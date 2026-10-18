package agent

import (
	"context"
	"log/slog"
	"sync"

	"github.com/google/uuid"

	nodev1 "github.com/zibyn/stars-sandbox/gen/node/v1"
	"github.com/zibyn/stars-sandbox/internal/nodeproto"
)

// noGenerationFence skips the stale-generation check. CancelExec uses it: the
// persistent cancel intent outlives the Sandbox generation it was created
// under (runtime.md §5).
const noGenerationFence int64 = 0

// commandRunner applies Control commands. Commands are idempotent and the
// generation is the fence: a command older than the last one seen for that
// resource is rejected (ADR-0015). Slow commands run off the receive loop and
// serialize per resource (runtime.md §9).
type commandRunner struct {
	ctx  context.Context
	rt   *Runtime
	send func(*nodev1.AgentMessage)

	mu    sync.Mutex
	gens  map[string]int64
	locks map[string]*sync.Mutex
}

func newCommandRunner(ctx context.Context, rt *Runtime, send func(*nodev1.AgentMessage)) *commandRunner {
	return &commandRunner{
		ctx: ctx, rt: rt, send: send,
		gens:  make(map[string]int64),
		locks: make(map[string]*sync.Mutex),
	}
}

func (c *commandRunner) handle(msg *nodev1.ControlMessage) {
	switch m := msg.Message.(type) {
	case *nodev1.ControlMessage_ProvisionWorkspace:
		cmd := m.ProvisionWorkspace
		wsID, err := uuid.Parse(cmd.WorkspaceId)
		if err != nil {
			c.result(cmd.CommandId, "invalid workspace id")
			return
		}
		go c.run("workspace:"+cmd.WorkspaceId, cmd.CommandId, cmd.Generation, func() error {
			if err := c.rt.provisionWorkspace(wsID); err != nil {
				return err
			}
			c.observeWorkspace(cmd.WorkspaceId, cmd.Generation, nodeproto.WorkspacePhaseReady, "")
			return nil
		})
	case *nodev1.ControlMessage_DeleteWorkspace:
		cmd := m.DeleteWorkspace
		wsID, err := uuid.Parse(cmd.WorkspaceId)
		if err != nil {
			c.result(cmd.CommandId, "invalid workspace id")
			return
		}
		go c.run("workspace:"+cmd.WorkspaceId, cmd.CommandId, cmd.Generation, func() error {
			if err := c.rt.deleteWorkspace(wsID); err != nil {
				return err
			}
			c.observeWorkspace(cmd.WorkspaceId, cmd.Generation, nodeproto.WorkspacePhaseGone, "")
			return nil
		})
	case *nodev1.ControlMessage_CreateSandbox:
		cmd := m.CreateSandbox
		sbID, err := uuid.Parse(cmd.SandboxId)
		if err != nil {
			c.result(cmd.CommandId, "invalid sandbox id")
			return
		}
		go c.run("sandbox:"+cmd.SandboxId, cmd.CommandId, cmd.Generation, func() error {
			if len(cmd.Resources) > 0 {
				// The Control Plane maps this to the fetching_resources
				// condition until running or failed (node-protocol.md).
				c.observeSandbox(cmd.SandboxId, cmd.Generation, nodeproto.SandboxPhaseFetchingResources, "")
			}
			cause, err := c.rt.createSandbox(c.ctx, &createSandboxSpec{
				sandboxID:     sbID.String(),
				generation:    cmd.Generation,
				imageDigest:   cmd.ImageDigest,
				cpu:           cmd.Limits.GetCpu(),
				memory:        cmd.Limits.GetMemory(),
				workspaceID:   cmd.Workspace.GetWorkspaceId(),
				workspaceMode: cmd.Workspace.GetMode(),
				gpuUUIDs:      cmd.GpuUuids,
				resources:     resourceMounts(cmd.Resources),
			})
			if err != nil {
				c.observeSandbox(cmd.SandboxId, cmd.Generation, nodeproto.SandboxPhaseCreateFailed, cause)
				return err
			}
			c.observeSandbox(cmd.SandboxId, cmd.Generation, nodeproto.SandboxPhaseRunning, "")
			return nil
		})
	case *nodev1.ControlMessage_StopSandbox:
		cmd := m.StopSandbox
		sbID, err := uuid.Parse(cmd.SandboxId)
		if err != nil {
			c.result(cmd.CommandId, "invalid sandbox id")
			return
		}
		go c.run("sandbox:"+cmd.SandboxId, cmd.CommandId, cmd.Generation, func() error {
			if err := c.rt.stopSandbox(c.ctx, sbID); err != nil {
				return err
			}
			c.observeSandbox(cmd.SandboxId, cmd.Generation, nodeproto.SandboxPhaseGone, "")
			return nil
		})
	case *nodev1.ControlMessage_StartExec:
		cmd := m.StartExec
		if _, err := uuid.Parse(cmd.SandboxId); err != nil {
			c.result(cmd.CommandId, "invalid sandbox id")
			return
		}
		if _, err := uuid.Parse(cmd.ExecId); err != nil {
			c.result(cmd.CommandId, "invalid exec id")
			return
		}
		// StartExec shares the Sandbox key: it must not overtake a Stop or run
		// against a newer generation.
		go c.run("sandbox:"+cmd.SandboxId, cmd.CommandId, cmd.Generation, func() error {
			return c.rt.startExec(c.ctx, cmd)
		})
	case *nodev1.ControlMessage_CancelExec:
		cmd := m.CancelExec
		execID, err := uuid.Parse(cmd.ExecId)
		if err != nil {
			c.result(cmd.CommandId, "invalid exec id")
			return
		}
		go c.run("exec:"+cmd.ExecId, cmd.CommandId, noGenerationFence, func() error {
			return c.rt.cancelExec(c.ctx, execID)
		})
	case *nodev1.ControlMessage_OpenTunnel:
		cmd := m.OpenTunnel
		var serve func(context.Context, string) error
		switch {
		case cmd.GetAgentHttp() != nil:
			serve = c.rt.openTunnel
		case cmd.GetSandboxPort() != nil:
			target := cmd.GetSandboxPort()
			serve = func(ctx context.Context, tunnelID string) error {
				return c.rt.forwardSandboxPort(ctx, tunnelID, target)
			}
		default:
			c.result(cmd.CommandId, "unsupported tunnel target")
			return
		}
		go c.run("tunnel:"+cmd.TunnelId, cmd.CommandId, noGenerationFence, func() error {
			return serve(c.ctx, cmd.TunnelId)
		})
	}
}

func (c *commandRunner) run(key, commandID string, generation int64, fn func() error) {
	c.mu.Lock()
	if generation < c.gens[key] {
		c.mu.Unlock()
		c.result(commandID, "stale generation")
		return
	}
	c.gens[key] = generation
	lock := c.locks[key]
	if lock == nil {
		lock = &sync.Mutex{}
		c.locks[key] = lock
	}
	c.mu.Unlock()

	lock.Lock()
	defer lock.Unlock()
	c.mu.Lock()
	stale := c.gens[key] != generation // a newer command arrived while waiting
	c.mu.Unlock()
	if stale {
		c.result(commandID, "superseded by a newer generation")
		return
	}
	err := fn()
	commandFailed(commandID, err)
	c.result(commandID, errString(err))
}

// commandFailed logs the error a command reports to the Control Plane; without
// it the Agent's log says nothing about a command that never applied.
func commandFailed(commandID string, err error) {
	if err != nil {
		slog.Warn("command failed", "command_id", commandID, "error", err)
	}
}

// resourceMounts copies the wire form into the adapter's mount table.
func resourceMounts(in []*nodev1.ResourceMount) []resourceMount {
	mounts := make([]resourceMount, 0, len(in))
	for _, mount := range in {
		mounts = append(mounts, resourceMount{address: mount.GetAddress(), mount: mount.GetMount()})
	}
	return mounts
}

func (c *commandRunner) observeSandbox(id string, generation int64, phase nodeproto.SandboxPhase, cause string) {
	c.send(&nodev1.AgentMessage{Message: &nodev1.AgentMessage_SandboxObserved{
		SandboxObserved: &nodev1.SandboxObserved{
			SandboxId: id, Generation: generation, Phase: string(phase), Cause: cause,
		},
	}})
}

func (c *commandRunner) observeWorkspace(id string, generation int64, phase nodeproto.WorkspacePhase, cause string) {
	c.send(&nodev1.AgentMessage{Message: &nodev1.AgentMessage_WorkspaceObserved{
		WorkspaceObserved: &nodev1.WorkspaceObserved{
			WorkspaceId: id, Generation: generation, Phase: string(phase), Cause: cause,
		},
	}})
}

func (c *commandRunner) result(commandID, detail string) {
	c.send(&nodev1.AgentMessage{Message: &nodev1.AgentMessage_CommandResult{
		CommandResult: &nodev1.CommandResult{CommandId: commandID, Error: detail},
	}})
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
