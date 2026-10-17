# ADR-0009: Freeze the Runtime Provider contract

- Status: accepted
- Date: 2026-10-17

## Context

The platform supports Docker, Podman, and a trusted Local Runtime. They do not provide the same isolation or resource guarantees. The contract must remain small without allowing a backend to silently ignore a requested security or resource property.

## Decision

`Runtime Provider` is a Node Agent-internal adapter with six operations:

- `Create`
- `Exec`
- `Inspect`
- `Stop`
- `Destroy`
- `Capabilities`

`Create` prepares a `ready` Runtime and does not execute a user command. `Exec` is the only command operation. `Exec` receives argv (not an implicit shell), environment, a Workspace-relative cwd, timeout, and optional stdin/output hooks. PTY lifecycle, durable logs, cancellation, retry, and reconciliation remain above this contract.

`Stop` is idempotent and preserves the Workspace. `Destroy` is idempotent only for stopped or absent Runtime instances; destroying a running Runtime returns `conflict`. Neither operation deletes the Workspace. `Exec` never implicitly starts or recreates a stopped or missing Runtime.

The common request surface is one platform-managed, read-write Workspace at `/workspace`. No arbitrary host mounts, host network, privileged mode, or caller-selected uid/gid are exposed. Docker and Podman accept immutable platform image digests. Local Runtime uses the Node host environment and returns `unsupported` when a concrete image digest is requested.

The common hard resource limits are CPU, memory, and PID count. IO bandwidth, disk capacity, and GPU memory limits are not common Provider guarantees. GPU requests use resolved vendor/device/kind records; the Scheduler allocates concrete devices and the Provider only attaches them or rejects the request.

`Capabilities` reports self-checked facts with three values:

- `enforced`: the Provider can guarantee the capability now;
- `best_effort`: it may attempt the capability but it is not a boundary;
- `unsupported`: it does not provide the capability.

A required capability must be `enforced`; otherwise creation fails. Node Agent reports a capability snapshot at registration and rechecks critical capabilities before `Create`. Dynamic resource availability remains Scheduler state, not capability state.

The initial posture is:

| Capability | Docker rootful | Podman rootless | Local Runtime |
|---|---|---|---|
| process/mount namespace | enforced | enforced | unsupported |
| CPU/memory/PID limits | enforced | enforced only with valid cgroup delegation | enforced via systemd scope |
| Workspace read-write mount | enforced | enforced | enforced as host cwd |
| GPU device visibility | enforced via CDI | enforced via CDI | best_effort |
| GPU memory limit | unsupported | unsupported | unsupported |
| disk capacity quota | unsupported | unsupported | unsupported |
| egress policy | enforced | enforced | best_effort |
| arbitrary extra mounts / privileged mode | unsupported | unsupported | unsupported |
| `capability_channel` (the named mount exception, ADR-0011) | enforced | enforced | best_effort |

## Consequences

- The Scheduler must place requests using current Runtime Capabilities and live resource accounting separately.
- A Local Runtime is a real, low-isolation deployment option, not a fake test backend; production policy must restrict it to trusted Nodes.
- Adding pause, resize, arbitrary mounts, hot GPU replacement, or image fallback requires a new contract decision rather than backend-specific extensions.
- Runtime Provider errors are normalized into `invalid_request`, `unsupported`, `not_found`, `conflict`, `resource_exhausted`, and `runtime_failed`.

## Rejected alternatives

- A broad Provider interface containing files, Git, scheduling, or egress rule authoring.
- Silent best-effort degradation when a required capability is unavailable.
- Implicit start/recreate during `Exec` or implicit stop during `Destroy`.
- Treating Local Runtime as a fake or pretending it has container isolation.
