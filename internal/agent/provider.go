package agent

import (
	"context"
	"errors"
	"log/slog"
	"os"

	"github.com/google/uuid"

	nodev1 "github.com/zibyn/orrery/gen/node/v1"
	"github.com/zibyn/orrery/internal/nodeproto"
)

// Runtime Provider names reported in Capabilities (node-protocol.md). One Node
// has exactly one Provider, chosen by Agent configuration (ADR-0014).
const (
	ProviderContainerd = "containerd"
	ProviderLocal      = "local"
)

// runtimeDriver is the part of the Runtime Provider contract that genuinely
// differs between Providers (ADR-0009): lifecycle, Exec launch, the
// registration self-check, and Runtime liveness. Host-path work (Supervisor
// records, files, publish, resources, tunnels) is
// identical for every Provider and stays on Runtime, so the seam is these
// operations and nothing more.
type runtimeDriver interface {
	createSandbox(ctx context.Context, cmd *createSandboxSpec) (cause string, err error)
	// stopSandbox ends the Runtime and keeps its filesystem for the commit.
	stopSandbox(ctx context.Context, sandboxID uuid.UUID) error
	// commitTarget is the stopped Sandbox's filesystem; ok is false when
	// there is none left to commit (runtime.md §16).
	commitTarget(ctx context.Context, sandboxID uuid.UUID) (target commitTarget, ok bool, err error)
	// dropSandboxDisk removes the filesystem once committed.
	dropSandboxDisk(ctx context.Context, sandboxID uuid.UUID) error
	// filesystem is a running Sandbox's own filesystem for the file API and
	// publish (api.md §5); errNotRunning without a Runtime here.
	filesystem(ctx context.Context, sandboxID uuid.UUID) (sandboxFS, error)
	startExec(ctx context.Context, cmd *nodev1.StartExec) error
	probe(ctx context.Context) (nodeproto.CapabilityReport, error)
	healthy(ctx context.Context) bool
	adopt(ctx context.Context, report func(sandboxID string, generation int64))
	scan(ctx context.Context, report func(sandboxID string, generation int64))
	watchEvents(ctx context.Context, report func(sandboxID string, generation int64))
	runtimeAlive(ctx context.Context, sandboxID uuid.UUID) (alive, known bool)
	// startForward runs `supervisor forward` in the Sandbox for a Tunnel
	// (runtime.md §14).
	startForward(ctx context.Context, sandboxID uuid.UUID, args ...string) error
	// setLinks replaces the SandboxLinks set and re-renders (runtime.md §13).
	setLinks(ctx context.Context, links []*nodev1.SandboxLink) error
	close()
}

// The Runtime methods below are the Control command surface. Each one is
// Provider-dispatched so command.go and the reconciliation loops never branch
// on the Provider.

func (r *Runtime) createSandbox(ctx context.Context, cmd *createSandboxSpec) (string, error) {
	if err := r.egressRefused(cmd.egress); err != nil {
		return "runtime_create_failed", err
	}
	id, err := uuid.Parse(cmd.sandboxID)
	if err != nil {
		return "runtime_create_failed", err
	}
	// The Runtime is about to write the filesystem: it is no longer what was
	// committed, and eviction must leave it alone (runtime.md §16).
	r.diskMu.Lock()
	err = os.Remove(cleanMarkPath(r.dataRoot, id))
	r.diskMu.Unlock()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "runtime_create_failed", err
	}
	// Versions are mounted before the Runtime starts, so a restart that adopts
	// a running Runtime also re-takes its hold (runtime.md §11).
	if cause, err := r.mountResources(ctx, id, cmd.resources); err != nil {
		return cause, err
	}
	// shortcut: a Volume mounted for a Create that then fails stays mounted
	// until a later mount of it here finds it stale (runtime.md §17), unmount
	// it on failure if stale mounts pile up on Nodes.
	if cause, err := r.mountVolumes(ctx, id, cmd.volumes); err != nil {
		r.releaseResources(context.WithoutCancel(ctx), id)
		return cause, err
	}
	cause, err := r.driver.createSandbox(ctx, cmd)
	if err != nil {
		r.releaseResources(context.WithoutCancel(ctx), id)
		return cause, err
	}
	r.startVolumeCommits(id, cmd.generation)
	return "", nil
}

// sandboxReport sends one SandboxObserved for the Sandbox being stopped.
type sandboxReport func(phase nodeproto.SandboxPhase, cause string, retrying bool)

// stopSandbox ends the Runtime, commits its filesystem and reports gone only
// after the commit (runtime.md §16); StopSandbox and a Runtime death both
// end here. It also releases the Sandbox's Resource versions: StopSandbox is
// the last command a Node sees for a stopped or deleted Sandbox, and a
// restart mounts them again. Callers serialize it per Sandbox.
func (r *Runtime) stopSandbox(ctx context.Context, sandboxID uuid.UUID, generation int64, report sandboxReport) error {
	if err := r.driver.stopSandbox(ctx, sandboxID); err != nil {
		return err
	}
	r.stopVolumeCommits(sandboxID)
	r.releaseResources(ctx, sandboxID)
	// Volumes first: their holds end with this stop (runtime.md §17).
	if len(readVolumeRecord(r.dataRoot, sandboxID)) > 0 {
		report(nodeproto.SandboxPhaseCommitting, "", false)
		err := r.commitVolumes(ctx, sandboxID, generation, func() {
			report(nodeproto.SandboxPhaseCommitting, "", true)
		})
		if isCommitFailed(err) {
			slog.Error("Sandbox Volume cannot be committed", "sandbox_id", sandboxID, "error", err)
			report(nodeproto.SandboxPhaseCommitFailed, string(nodeproto.SandboxPhaseCommitFailed), false)
			return nil
		}
		if err != nil {
			return err
		}
	}
	target, ok, err := r.driver.commitTarget(ctx, sandboxID)
	if err != nil {
		return err
	}
	cleanMark := cleanMarkPath(r.dataRoot, sandboxID)
	if _, err := os.Stat(cleanMark); err == nil {
		ok = false // a cached diff already committed: nothing new to commit
	}
	if ok {
		report(nodeproto.SandboxPhaseCommitting, "", false)
		err := r.commitWithRetry(ctx, sandboxID, generation, target, func() {
			report(nodeproto.SandboxPhaseCommitting, "", true)
		})
		if isCommitFailed(err) {
			// The disk stays until the Sandbox is deleted (runtime.md §16).
			slog.Error("Sandbox filesystem cannot be committed", "sandbox_id", sandboxID, "error", err)
			report(nodeproto.SandboxPhaseCommitFailed, string(nodeproto.SandboxPhaseCommitFailed), false)
			return nil
		}
		if err != nil {
			return err
		}
		// The disk stays as this Node's cache of the head (runtime.md §16).
		if err := os.WriteFile(cleanMark, nil, 0o600); err != nil {
			return err
		}
	}
	report(nodeproto.SandboxPhaseGone, "", false)
	return nil
}

// dropSandboxDisk serves DropSandboxDisk, sent once the Sandbox is deleted:
// its cached diff goes, and so does its whole directory (records, network
// state, stat tables), which nothing reads again.
func (r *Runtime) dropSandboxDisk(ctx context.Context, id uuid.UUID) error {
	r.diskMu.Lock()
	defer r.diskMu.Unlock()
	if err := r.dropSandboxDiskLocked(ctx, id); err != nil {
		return err
	}
	return os.RemoveAll(sandboxPath(r.dataRoot, id))
}

func (r *Runtime) dropSandboxDiskLocked(ctx context.Context, id uuid.UUID) error {
	if err := r.driver.dropSandboxDisk(ctx, id); err != nil {
		return err
	}
	return r.forgetCachedDiff(id)
}

func (r *Runtime) startExec(ctx context.Context, cmd *nodev1.StartExec) error {
	return r.driver.startExec(ctx, cmd)
}

// probe adds resource_mount, decided on every Provider by a mount and read
// through orrery-resourcefs (runtime.md §11).
func (r *Runtime) probe(ctx context.Context) (nodeproto.CapabilityReport, error) {
	report, err := r.driver.probe(ctx)
	if err != nil {
		return report, err
	}
	ok := false
	if r.resourcefs != nil {
		if err := r.resourcefs.SelfTest(ctx); err != nil {
			slog.Warn("resourcefs self-check failed", "error", err)
		} else {
			ok = true
		}
	}
	return withSelfCheck(report, nodeproto.CapResourceMount, ok), nil
}

func (r *Runtime) healthy(ctx context.Context) bool {
	return r.driver.healthy(ctx)
}

func (r *Runtime) adopt(ctx context.Context, report func(string, int64)) {
	r.driver.adopt(ctx, report)
	r.rebuildResourceRefs(ctx)
	r.resumeVolumeCommits()
}

func (r *Runtime) scan(ctx context.Context, report func(string, int64)) {
	r.driver.scan(ctx, report)
}

func (r *Runtime) watchEvents(ctx context.Context, report func(string, int64)) {
	r.driver.watchEvents(ctx, report)
}

func (r *Runtime) runtimeAlive(ctx context.Context, sandboxID uuid.UUID) (bool, bool) {
	return r.driver.runtimeAlive(ctx, sandboxID)
}
