# ADR-0009: Freeze the Runtime Provider contract

- Status: accepted; mounts amended by ADR-0019; request `runtime`, Local Runtime and the Local GPU exception amended in place by #92
- Date: 2026-10-17

## Context

The platform supports Docker, Podman, and a trusted Local Runtime. They do not provide the same isolation or resource guarantees. The contract must remain small without allowing a backend to silently ignore a requested security or resource property.

## Decision

`Runtime Provider` is a Node Agent-internal adapter with five operations:

- `Create`
- `Exec`
- `Inspect`
- `Stop`
- `Capabilities`

`Create` prepares a `ready` Runtime and does not execute a user command. `Exec` is the only command operation. `Exec` receives argv (not an implicit shell), environment, a Workspace-relative cwd, timeout, and optional stdin. It does not run the command through the runtime's own exec stream, which cannot be reattached once its client goes away. It starts the command under the Exec Supervisor: a platform binary mounted read-only into the Runtime that records Output Frames and the exit status in a Node-local directory outside the Workspace. The Node Agent reads that record from its last offset, so an Exec outlives a Node Agent restart. Durable logs, cancellation and reconciliation remain above this contract.

`Stop` terminates and removes the Runtime instance; it is idempotent and never deletes the Workspace. There is no `Start`, so a stopped Runtime is never reused and a restarted Sandbox gets a new one from `Create`. `Exec` never implicitly creates a missing Runtime.

The common request surface is one platform-managed, read-write Workspace at `/workspace`. No arbitrary host mounts, host network, privileged mode, or caller-selected uid/gid are exposed; the platform mounts are the Workspace, read-only Resources, and under `/.stars` the read-only Exec Supervisor binary and the Sandbox's read-write Exec record directory (ADR-0019). Docker and Podman accept immutable platform image digests. Local Runtime uses the Node host environment and returns `unsupported` when a concrete image digest is requested.

A Sandbox request selects `runtime: container | local`, defaulting to `container`; a caller cannot tell Docker from Podman. A `container` request is placed only on Docker or Podman Nodes, a `local` request only on Local Nodes, and the choice stays an admission constraint on restart. A `local` request has no Snapshot, so the default platform Snapshot does not apply to it; one that names a `snapshot` or `resources` is rejected `unsupported`.

On Local Runtime there is no `/workspace` and no `/.stars`: the cwd is the Workspace's host path, also given as `STARS_WORKSPACE`, and the Exec record lives in the Node's data directory. Every User's commands run as the one unprivileged uid of the Node configuration, so Users on a Local Node are not isolated from each other (files, signals and loopback ports are shared). Local Runtime is for trusted insiders only, as with Coder's external workspaces. The upgrade path is one host uid per User, Workspace directories at `0700` and systemd hardening, with this contract unchanged.

The common hard resource limits are CPU, memory, and PID count. IO bandwidth and GPU memory limits are not common Provider guarantees. Disk capacity is enforced only where the Node's Workspace filesystem is XFS with project quotas; elsewhere it is admission accounting plus a Node disk watermark, not isolation. GPU requests use resolved vendor/device/kind records; the Scheduler allocates concrete devices and the Provider only attaches them or rejects the request.

`Capabilities` reports self-checked facts with three values:

- `enforced`: the Provider can guarantee the capability now;
- `best_effort`: it may attempt the capability but it is not a boundary;
- `unsupported`: it does not provide the capability.

A required capability must be `enforced`; otherwise creation fails. The one exception is GPU device visibility on Local Runtime: requesting `runtime: local` is the caller accepting low isolation, so `best_effort` is accepted there. Node Agent reports a capability snapshot at registration. After `Create` it reads back the effective cgroup limits and device nodes and fails the creation if they differ from the request, so a runtime that silently ignores a limit is caught. Dynamic resource availability remains Scheduler state, not capability state.

The initial posture is:

| Capability | Docker rootful | Podman rootless | Local Runtime |
|---|---|---|---|
| process/mount namespace | enforced | enforced | unsupported |
| CPU/memory/PID limits | enforced | enforced only with valid cgroup delegation | enforced via systemd scope |
| Workspace read-write mount | enforced | enforced | enforced as host cwd |
| GPU device visibility | enforced via CDI | enforced via CDI | enforced via systemd `DevicePolicy=closed` when the self-check passes, else best_effort |
| GPU memory limit | unsupported | unsupported | unsupported |
| disk capacity quota | enforced on XFS project quota, else unsupported | enforced on XFS project quota, else unsupported | enforced on XFS project quota, else unsupported |
| Exec Supervisor (survives Node Agent restart) | enforced | enforced | enforced as a detached host process |
| arbitrary extra mounts / privileged mode | unsupported | unsupported | unsupported |

## Consequences

- The Scheduler must place requests using current Runtime Capabilities and live resource accounting separately.
- A Local Runtime is a real, low-isolation deployment option, not a fake test backend; production policy must restrict it to trusted Nodes.
- Adding pause, resize, arbitrary mounts, hot GPU replacement, or image fallback requires a new contract decision rather than backend-specific extensions.
- Runtime Provider errors are normalized into `invalid_request`, `unsupported`, `not_found`, `conflict`, `resource_exhausted`, and `runtime_failed`.

## Rejected alternatives

- A broad Provider interface containing files, Git, scheduling, or egress rule authoring.
- Silent best-effort degradation when a required capability is unavailable.
- Implicit start/recreate during `Exec`.
- A separate `Destroy` after `Stop`: with no `Start`, a stopped Runtime has no further use, as with Coder's stop (container removed, volume kept).
- Running commands through `docker exec`/`podman exec` directly: the stream cannot be reattached (moby#9527), so a Node Agent restart would lose every running Exec. Coder and Daytona both keep a supervising process inside the sandbox for this reason.
- Platform-authored egress rules: egress is the deployment's network (ADR-0016).
- Treating Local Runtime as a fake or pretending it has container isolation.
