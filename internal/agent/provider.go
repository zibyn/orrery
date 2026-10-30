package agent

import (
	"context"

	"github.com/google/uuid"

	nodev1 "github.com/zibyn/orrery/gen/node/v1"
	"github.com/zibyn/orrery/internal/nodeproto"
)

// Runtime Provider names reported in Capabilities (node-protocol.md). One Node
// has exactly one Provider, chosen by Agent configuration (ADR-0014).
const (
	ProviderDocker     = "docker"
	ProviderPodman     = "podman"
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
}

// containerDriver is the Docker/Podman half: the Engine API adapter already
// implemented on Runtime (runtime.md §9).
type containerDriver struct{ r *Runtime }

func (d containerDriver) createSandbox(ctx context.Context, cmd *createSandboxSpec) (string, error) {
	return d.r.createContainerSandbox(ctx, cmd)
}

func (d containerDriver) stopSandbox(ctx context.Context, sandboxID uuid.UUID) error {
	return d.r.stopContainerSandbox(ctx, sandboxID)
}

func (d containerDriver) startExec(ctx context.Context, cmd *nodev1.StartExec) error {
	return d.r.startContainerExec(ctx, cmd)
}

func (d containerDriver) probe(ctx context.Context) (nodeproto.CapabilityReport, error) {
	return dockerProbe(ctx, d.r.config())
}

func (d containerDriver) healthy(ctx context.Context) bool {
	return dockerHealthy(ctx, d.r.socket)
}

func (d containerDriver) adopt(ctx context.Context, report func(string, int64)) {
	d.r.adoptContainers(ctx, report)
}

func (d containerDriver) scan(ctx context.Context, report func(string, int64)) {
	d.r.scanContainers(ctx, report)
}

func (d containerDriver) watchEvents(ctx context.Context, report func(string, int64)) {
	d.r.watchContainerEvents(ctx, report)
}

func (d containerDriver) runtimeAlive(ctx context.Context, sandboxID uuid.UUID) (bool, bool) {
	return d.r.containerAlive(ctx, sandboxID)
}

// The Runtime methods below are the Control command surface. Each one is
// Provider-dispatched so command.go and the reconciliation loops never branch
// on the Provider.

func (r *Runtime) createSandbox(ctx context.Context, cmd *createSandboxSpec) (string, error) {
	if err := r.egressRefused(cmd.egress); err != nil {
		return "runtime_create_failed", err
	}
	return r.driver.createSandbox(ctx, cmd)
}

func (r *Runtime) stopSandbox(ctx context.Context, sandboxID uuid.UUID) error {
	return r.driver.stopSandbox(ctx, sandboxID)
}

func (r *Runtime) startExec(ctx context.Context, cmd *nodev1.StartExec) error {
	return r.driver.startExec(ctx, cmd)
}

func (r *Runtime) probe(ctx context.Context) (nodeproto.CapabilityReport, error) {
	return r.driver.probe(ctx)
}

func (r *Runtime) healthy(ctx context.Context) bool {
	return r.driver.healthy(ctx)
}

func (r *Runtime) adopt(ctx context.Context, report func(string, int64)) {
	r.driver.adopt(ctx, report)
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
