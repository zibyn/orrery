package agent

import (
	"context"
	"sync"

	"github.com/google/uuid"

	nodev1 "github.com/zibyn/stars-sandbox/gen/node/v1"
)

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
			c.observeWorkspace(cmd.WorkspaceId, cmd.Generation, "ready", "")
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
			c.observeWorkspace(cmd.WorkspaceId, cmd.Generation, "gone", "")
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
			cause, err := c.rt.createSandbox(c.ctx, &createSandboxSpec{
				sandboxID:     sbID.String(),
				generation:    cmd.Generation,
				imageDigest:   cmd.ImageDigest,
				cpu:           cmd.Limits.GetCpu(),
				memory:        cmd.Limits.GetMemory(),
				workspaceID:   cmd.Workspace.GetWorkspaceId(),
				workspaceMode: cmd.Workspace.GetMode(),
			})
			if err != nil {
				c.observeSandbox(cmd.SandboxId, cmd.Generation, "create_failed", cause)
				return err
			}
			c.observeSandbox(cmd.SandboxId, cmd.Generation, "running", "")
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
			c.observeSandbox(cmd.SandboxId, cmd.Generation, "gone", "")
			return nil
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
	c.result(commandID, errString(fn()))
}

func (c *commandRunner) observeSandbox(id string, generation int64, phase, cause string) {
	c.send(&nodev1.AgentMessage{Message: &nodev1.AgentMessage_SandboxObserved{
		SandboxObserved: &nodev1.SandboxObserved{
			SandboxId: id, Generation: generation, Phase: phase, Cause: cause,
		},
	}})
}

func (c *commandRunner) observeWorkspace(id string, generation int64, phase, cause string) {
	c.send(&nodev1.AgentMessage{Message: &nodev1.AgentMessage_WorkspaceObserved{
		WorkspaceObserved: &nodev1.WorkspaceObserved{
			WorkspaceId: id, Generation: generation, Phase: phase, Cause: cause,
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
