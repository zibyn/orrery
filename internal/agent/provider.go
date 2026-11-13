package agent

import (
	"context"
	"log/slog"

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
// registration self-check, and Runtime liveness. Host-path work (Workspace
// provisioning, Supervisor records, files, publish, resources, tunnels) is
// identical for every Provider and stays on Runtime, so the seam is these
// operations and nothing more.
type runtimeDriver interface {
	createSandbox(ctx context.Context, cmd *createSandboxSpec) (cause string, err error)
	stopSandbox(ctx context.Context, sandboxID uuid.UUID) error
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
	// Versions are mounted before the Runtime starts, so a restart that adopts
	// a running Runtime also re-takes its hold (runtime.md §11).
	if cause, err := r.mountResources(ctx, id, cmd.resources); err != nil {
		return cause, err
	}
	cause, err := r.driver.createSandbox(ctx, cmd)
	if err != nil {
		r.releaseResources(context.WithoutCancel(ctx), id)
	}
	return cause, err
}

// stopSandbox also releases the Sandbox's Resource versions: StopSandbox is
// the last command a Node sees for a stopped or deleted Sandbox, and a
// restart mounts them again.
func (r *Runtime) stopSandbox(ctx context.Context, sandboxID uuid.UUID) error {
	if err := r.driver.stopSandbox(ctx, sandboxID); err != nil {
		return err
	}
	r.releaseResources(ctx, sandboxID)
	return nil
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
