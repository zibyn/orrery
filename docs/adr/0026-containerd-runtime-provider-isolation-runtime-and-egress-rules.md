# ADR-0026: containerd Runtime Provider, Isolation Runtime and platform Egress Rules

- Status: accepted; Isolation Runtime value `gvisor` renamed to `runsc` in place by #176 (2026-10-31); runsc `oom_killed` consequence added by #173 (2026-10-31); "no implicit entries" excepts Sandbox Links, and CNI masquerade replaced by the Agent's, by ADR-0027
- Date: 2026-10-30

## Context

Issue #160. Docker and Podman Nodes reach their engine through the Engine API, so the engine owns the Sandbox netns and the platform cannot filter egress. ADR-0016 left egress to the deployment because the project was early, not because egress control was unwanted. Untrusted code also needs a stronger boundary than a shared host kernel. Docker and Podman can both run `runsc`, so gVisor alone would not justify a migration; owning the network does. Three container backends also made every Runtime Capability self-check exist several times over.

Spike #161 checked the premises on containerd 2.2.1, runsc `release-20261026.0` and CNI plugins v1.9.1 (`docs/research/containerd-cdi-runsc-cni-spike.md`). CDI injection, host cgroup readback under runsc, runsc on a caller-created CNI netns and `bridge`-family nftables on the host-side veth all work. Entering the recorded netns to reach a Sandbox port does not work under runsc, and that changed this decision before it was accepted.

## Decision

**One container Provider on containerd.** The Docker and Podman Runtime Providers are removed. The container Provider uses the containerd native gRPC API in an Agent-owned namespace. Request `runtime: container | local` is unchanged. Only rootful containerd is supported. The ADR-0009 contract (five operations, Exec Supervisor, mounts, readback) is unchanged. Capabilities report the Provider as `containerd` or `local`.

**Isolation Runtime is a Node choice.** Each containerd Node runs exactly one Isolation Runtime, `runc` or `runsc` (gVisor's OCI runtime, through the `io.containerd.runsc.v1` shim), set by Agent configuration. It is reported as Runtime Capability `kernel_isolation` (`enforced` under runsc, `unsupported` on runc). A request cannot select it; a deployment that wants gVisor everywhere enrolls only runsc Nodes.

**The platform owns the Sandbox network.** Each Sandbox gets its own netns and veth on a CNI bridge with host-local IPAM from the Node's Sandbox subnet. Egress is filtered by an Agent-owned `bridge`-family nftables table that matches the host-side veth by `iifname`, outside the reach of code in the Sandbox and identical for runc and runsc: runsc moves the netns's addresses into its own network stack, but its packets still leave through the same veth. An `inet` table is not used, because without `br_netfilter` it sees the bridge rather than the veth, and the Agent does not load `br_netfilter`, which would change filtering for everything else on the host bridge. `ct state established,related` is accepted first, so replies to connections a Sandbox did not initiate still flow. The netns, veth and rules are torn down on `Stop` and built again on `Create`; under runsc a netns cannot be reused after its Sandbox started.

**Tunnels keep the Supervisor forwarder.** A Tunnel to a Sandbox port is served by `supervisor forward PORT <sock>` inside the Sandbox, on a unix socket in the shared run mount, and the Agent dials that socket, as today. The Agent never enters the Sandbox netns. On runsc Nodes the Agent passes runsc `host-uds=create` for each container through the shim's runtime options, so a socket created inside the Sandbox is visible on the host; the PTY control socket needs the same flag.

**Egress Rules and Egress Mode.** Egress Rules are written by the operator in the deployment configuration as `egress.allow` and `egress.deny` (IPv4 CIDR with optional port) and pushed by the Control Plane to every Node over the Control channel. Egress Mode is chosen by the caller at creation: `open` (default), `none`, or `allowlist` with a non-empty `allow`; `allow` with any other mode is `invalid_request`. A Sandbox's Egress Policy is evaluated in order: Rules `allow` → Rules `deny` → Mode. The Mode is fixed for the Sandbox's life and reapplied on restart; a Rules change applies to Sandboxes created or started afterwards. The platform adds no implicit entries: reachability of `api`, DNS, metadata and other Sandboxes is entirely the operator's configuration, and `defaults.md` ships a recommended set. Capability `egress_policy` is `enforced` only after the registration probe confirms a denied destination is unreachable and an allowed one reachable. A request with a Mode other than `open` is placed only on Nodes where it is enforced.

**Other surfaces.** GPU attaches via CDI (`pkg/cdi.WithCDIDevices` in the containerd 2.x client) on runc Nodes and is `unsupported` on runsc Nodes. runsc turns on nvproxy by itself when the OCI spec carries NVIDIA devices, so the Agent on a runsc Node refuses a GPU request before `Create` rather than relying on runsc to fail. Limits are read back from the host cgroup at `/sys/fs/cgroup/<cgroupsPath>` under both Isolation Runtimes. Snapshot builds need a standalone buildkitd (`--buildkit`); without one `snapshot_build` is `unsupported`. Image eviction applies to the Agent's containerd namespace. A runsc Node's data root must not be on tmpfs, because runsc cannot execute the Supervisor from a tmpfs bind mount. Local Runtime has no netns: `egress_policy` is `unsupported` and Egress Rules do not apply to it.

**Migration is a hard cut.** Drain the Node, upgrade the Agent, install containerd and buildkitd, re-register. A stopped Sandbox gets a new Runtime from `Create` on its next start (ADR-0009), and its Workspace stays on the host. `--runtime docker` and `--runtime podman` fail at Agent startup.

## Consequences

- ADR-0016's Egress section is superseded; its Audit and Sandbox Token sections and its rejection of per-connection egress logging stand.
- ADR-0009's Provider list and capability table change: Docker and Podman columns become containerd runc and containerd runsc; rows `kernel_isolation` and `egress_policy` are added.
- ADR-0017's port-forward sentence ("the Node Agent enters the Sandbox's network namespace") is corrected in place: the Supervisor forwarder is the mechanism. ADR-0019's Engine API notes (Podman exec Env, one Engine API client) no longer describe a running Provider.
- A fresh deployment with empty Egress Rules is fully open, cloud metadata included; safety depends on the operator adopting the recommended Rules.
- A misconfigured Rules set can cut Sandboxes off from `api`, breaking Git authentication; the platform does not detect this.
- Sandbox subnets must be planned non-overlapping across Nodes for Rules to name them.
- Under runsc, `pids.max` counts host tasks (Sentry threads and one systrap stub per guest thread), not guest processes one to one: the same limit admits fewer guest processes, and reaching it kills the whole Sandbox instead of failing one `fork()`. The limit is still read back exactly and still a boundary.
- Under runsc the guest has no OOM counter and the Sentry has no OOM killer (spike #177, `docs/research/runsc-memory-oom-spike.md`). A memory-limit breach is resolved by the host OOM killer, which kills either the Exec's systrap stub (the Exec ends by SIGKILL, the Sandbox lives) or the Sentry (the Sandbox stops with `runtime_lost`); the workload decides which, and the platform does not promise either. The Agent attributes both from the host cgroup's `oom_kill` against a per-Exec baseline kept in the Exec record, so the affected Execs still end `oom_killed` (runtime.md §9).
- With `host-uds=create`, code in a runsc Sandbox can create host-visible unix sockets on its host-backed mounts (Workspace and run directory). Connecting to host sockets (`open`) stays closed.
- Snapshot builds run in buildkitd, on its own network, so Egress Rules do not apply to a build's `RUN` steps.
- Rootless Podman Nodes must be reinstalled with rootful containerd.

## Rejected alternatives

- Adding `HostConfig.Runtime: runsc` to the existing Engine API adapter: gives gVisor but not network ownership.
- Keeping Docker and Podman alongside containerd: every capability self-check and the egress mechanism would exist three times.
- CRI: built for kubelet, adds a PodSandbox layer the platform does not use.
- Tunnels entering the Sandbox netns from a recorded path: under runsc the netns keeps only `lo` and an address-less `eth0` after start, so a host process in it cannot reach the guest's loopback or address (spike #161 §3.3). Dialing the Sandbox's bridge address instead misses listeners bound to `127.0.0.1`. `runsc port-forward --stream` works but would be a runsc-only branch.
- An `inet` table with `br_netfilter` loaded: changes filtering for every bridge on the host.
- Rules inside the Sandbox netns: under runc, a process with enough privilege in the netns could reach them.
- Domain-name egress rules: needs a DNS or SNI proxy; can arrive later as a new capability.
- Platform-implied `api`/DNS allowances or Node-appended entries: the operator decides all reachability.
- Expressing "no egress" as an empty `allow` list: indistinguishable from an omitted field in common clients, and it fails open.
- Caller-selectable gVisor in v1.
- GPU under gVisor (nvproxy): not verified, and outside #160.
- Rootless containerd: conflicts with host-side nftables on the Sandbox veth.
