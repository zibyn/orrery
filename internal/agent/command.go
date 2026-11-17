package agent

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	nodev1 "github.com/zibyn/orrery/gen/node/v1"
	"github.com/zibyn/orrery/internal/nodeproto"
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
	// checked is closed once the startup self-check has reported Capabilities.
	checked chan struct{}
	// The Control Plane sends each set once, so a failed apply is retried here.
	peers, links latestApply
}

// networkRetryMin and networkRetryMax bound the backoff between re-applies of
// a failed MeshPeers or SandboxLinks set.
var networkRetryMin, networkRetryMax = time.Second, 30 * time.Second

// latestApply applies full sets in order and re-applies the latest one with
// backoff until it succeeds, a newer set supersedes it, or ctx ends (#252).
type latestApply struct {
	mu    sync.Mutex
	seq   int
	timer *time.Timer
}

func (l *latestApply) apply(ctx context.Context, what string, fn func() error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq++
	if l.timer != nil {
		l.timer.Stop()
	}
	l.try(ctx, what, l.seq, networkRetryMin, fn)
}

// try runs fn under l.mu, so a retry cannot overtake a newer set.
func (l *latestApply) try(ctx context.Context, what string, seq int, delay time.Duration, fn func() error) {
	if seq != l.seq || ctx.Err() != nil {
		return
	}
	err := fn()
	if err == nil {
		return
	}
	slog.Warn("apply "+what, "error", err, "retry_in", delay)
	l.timer = time.AfterFunc(delay, func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.try(ctx, what, seq, min(delay*2, networkRetryMax), fn)
	})
}

func newCommandRunner(ctx context.Context, rt *Runtime, send func(*nodev1.AgentMessage)) *commandRunner {
	return &commandRunner{
		ctx: ctx, rt: rt, send: send,
		gens:    make(map[string]int64),
		locks:   make(map[string]*sync.Mutex),
		checked: make(chan struct{}),
	}
}

// selfChecked releases the commands that wait for the Capabilities.
func (c *commandRunner) selfChecked() { close(c.checked) }

// afterSelfCheck orders a command that places new work after the self-check:
// Create and Build depend on the capability state (egress_policy,
// snapshot_build), while commands for running Sandboxes are served at once.
// The Reconciler re-sends a command lost to a dropped stream.
func (c *commandRunner) afterSelfCheck() error {
	select {
	case <-c.checked:
		// A stream that ended after its self-check releases nothing.
		return c.ctx.Err()
	case <-c.ctx.Done():
		return c.ctx.Err()
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
			if err := c.afterSelfCheck(); err != nil {
				return err
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
				volumes:       volumeMounts(cmd.Volumes),
				egress:        egressModeOf(cmd.Egress),
				linkTarget:    cmd.LinkTarget,
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
			return c.stopSandbox(sbID, cmd.Generation)
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
	case *nodev1.ControlMessage_PublishWorkspace:
		cmd := m.PublishWorkspace
		if _, err := uuid.Parse(cmd.VersionId); err != nil {
			c.result(cmd.CommandId, "invalid version id")
			return
		}
		if _, err := uuid.Parse(cmd.WorkspaceId); err != nil {
			c.result(cmd.CommandId, "invalid workspace id")
			return
		}
		// ResourceVersions have no generation; the Control Plane sends 0. The
		// version id is part of the key, so only the same publish collapses
		// onto one lock.
		go c.run("publish:"+cmd.VersionId, cmd.CommandId, noGenerationFence, func() error {
			return c.rt.publishWorkspace(c.ctx, cmd)
		})
	case *nodev1.ControlMessage_PublishSandbox:
		cmd := m.PublishSandbox
		if _, err := uuid.Parse(cmd.VersionId); err != nil {
			c.result(cmd.CommandId, "invalid version id")
			return
		}
		sandboxID, err := uuid.Parse(cmd.SandboxId)
		if err != nil {
			c.result(cmd.CommandId, "invalid sandbox id")
			return
		}
		go c.run("publish:"+cmd.VersionId, cmd.CommandId, noGenerationFence, func() error {
			return c.rt.publishSandbox(c.ctx, sandboxID, cmd)
		})
	case *nodev1.ControlMessage_BuildSnapshot:
		cmd := m.BuildSnapshot
		if _, err := uuid.Parse(cmd.VersionId); err != nil {
			c.result(cmd.CommandId, "invalid version id")
			return
		}
		if _, err := uuid.Parse(cmd.SandboxId); err != nil {
			c.result(cmd.CommandId, "invalid sandbox id")
			return
		}
		if _, err := uuid.Parse(cmd.ExecId); err != nil {
			c.result(cmd.CommandId, "invalid exec id")
			return
		}
		// SnapshotVersions have no generation; the version id keys the lock.
		// Buildx is canceled through the Exec id, not a Supervisor socket.
		go c.run("build:"+cmd.VersionId, cmd.CommandId, noGenerationFence, func() error {
			if err := c.afterSelfCheck(); err != nil {
				return err
			}
			return c.rt.buildSnapshot(c.ctx, cmd)
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
	case *nodev1.ControlMessage_MeshPeers:
		// Applied inline so successive full sets land in order; a Node
		// without Mesh ignores it (runtime.md §15).
		if c.rt.mesh != nil {
			c.peers.apply(c.ctx, "mesh peers", func() error { return c.rt.mesh.apply(m.MeshPeers.Peers) })
		}
	case *nodev1.ControlMessage_SandboxLinks:
		// Inline for the same reason: each set replaces the last whole.
		c.links.apply(c.ctx, "sandbox links", func() error { return c.rt.driver.setLinks(c.ctx, m.SandboxLinks.Links) })
	}
}

func (c *commandRunner) run(key, commandID string, generation int64, fn func() error) {
	err := c.serialize(key, generation, fn)
	commandFailed(commandID, err)
	c.result(commandID, errString(err))
}

// stopSandbox ends a Sandbox and commits its filesystem, reporting under the
// stop's generation.
func (c *commandRunner) stopSandbox(id uuid.UUID, generation int64) error {
	return c.rt.stopSandbox(c.ctx, id, generation, func(phase nodeproto.SandboxPhase, cause string, retrying bool) {
		c.reportSandbox(id.String(), generation, phase, cause, retrying)
	})
}

// runtimeDied takes a dead Runtime through the stop's commit path, in line
// with the Sandbox's commands (runtime.md §16).
func (c *commandRunner) runtimeDied(sandboxID string, generation int64) {
	id, err := uuid.Parse(sandboxID)
	if err != nil {
		return
	}
	if err := c.serialize("sandbox:"+sandboxID, generation, func() error {
		return c.stopSandbox(id, generation)
	}); err != nil {
		slog.Warn("commit a dead Runtime", "sandbox_id", sandboxID, "error", err)
	}
}

var (
	errStaleGeneration = errors.New("stale generation")
	errSuperseded      = errors.New("superseded by a newer generation")
)

// serialize runs fn under key's lock unless a newer generation was seen.
func (c *commandRunner) serialize(key string, generation int64, fn func() error) error {
	c.mu.Lock()
	if generation < c.gens[key] {
		c.mu.Unlock()
		return errStaleGeneration
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
		return errSuperseded
	}
	return fn()
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

// observeSandbox reports a phase; `running` carries the address recorded at
// Create (node-protocol.md).
func (c *commandRunner) observeSandbox(id string, generation int64, phase nodeproto.SandboxPhase, cause string) {
	c.reportSandbox(id, generation, phase, cause, false)
}

// reportSandbox is observeSandbox with commit_retrying for `committing`.
func (c *commandRunner) reportSandbox(id string, generation int64, phase nodeproto.SandboxPhase, cause string, retrying bool) {
	var address string
	if phase == nodeproto.SandboxPhaseRunning {
		if sbID, err := uuid.Parse(id); err == nil {
			address = recordedAddress(c.rt.dataRoot, sbID)
		}
	}
	c.send(&nodev1.AgentMessage{Message: &nodev1.AgentMessage_SandboxObserved{
		SandboxObserved: &nodev1.SandboxObserved{
			SandboxId: id, Generation: generation, Phase: string(phase), Cause: cause, Address: address,
			CommitRetrying: retrying,
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
