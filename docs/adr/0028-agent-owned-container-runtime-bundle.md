# ADR-0028: Agent-owned container runtime bundle

- Status: proposed; accept after the spike in #206 is written up in `docs/research/`
- Date: 2026-11-02

## Context

Issue #206, a follow-up to #205, which removed `nft`, `ip` and the CNI plugins as host dependencies. A containerd Node still needs the operator to install rootful containerd 2.x with runc, a standalone buildkitd with a hand-written `buildkitd.toml` for a private CA, and optionally runsc with `containerd-shim-runsc-v1`. ADR-0026 put the Agent in its own namespace on the host's containerd, so `docs/deploy.md` says the Agent does not change containerd's configuration and can share one containerd with Docker on the same host. That leaves the runtime versions, and the CVE exposure that comes with them, outside the project's control, and every Node's preparation is manual.

## Decision

**The node release package bundles the runtimes.** `orrery-node_<v>_linux_<arch>.tar.gz` (amd64 and arm64) carries containerd, runc, buildkitd, runsc and `containerd-shim-runsc-v1` next to `orrery-agent` and `orrery`. It is one package per arch; there is no base package with an add-on. A Node then needs only root, systemd and a suitable kernel (cgroup v2; WireGuard for the Node Mesh, ADR-0027). The NVIDIA driver and the CDI spec stay on the host.

**containerd is private to the Agent.** It has its own socket (`/run/orrery/containerd/containerd.sock`), root (`/var/lib/orrery-agent/containerd`), state and `config.toml`, and never touches a host Docker or containerd. `--containerd` stays as an Agent flag for development and tests and defaults to the private socket. `install.sh` has no mode that uses the host's containerd, and that use is unsupported.

**`install.sh` writes three systemd units and all runtime configuration.** `orrery-containerd.service` runs with `KillMode=process` so shims and Execs outlive a containerd restart. `orrery-buildkitd.service` is ordered `After=` containerd only, since its OCI worker does not use containerd. `orrery-agent.service` has `Requires=` and `After=orrery-containerd.service`. The script generates containerd's `config.toml` (with the `runsc` runtime registered) and `buildkitd.toml` (`--oci-worker=true --containerd-worker=false`, and a `ca` for the registry and object-store hosts taken from `--ca`), and overwrites both on every run; operator additions are not supported. On a rerun it compares each bundled binary's sha256 and restarts a runtime unit only when its binary changed; the Agent restarts every time, as before.

**Security updates ship as Orrery releases and are deployed by hand.** Upstream versions and sha256 sums are pinned in CI; runsc is pinned to a release tag. Renovate opens bump PRs, and a runtime CVE fix ships in an Orrery patch release. An operator deploys it by rerunning `install.sh`; nothing updates a host on its own.

**Automatic updates are removed.** ADR-0023's opt-in `--auto-update` goes, on the Control Plane and on Nodes alike: `install.sh` drops `--auto-update` and `--no-auto-update`, and on its next run removes an updater left by an earlier version (the `orrery-updater` Compose project, or `orrery-update.service` and `/etc/orrery-agent/updater`). Every upgrade, Control Plane or Node, is an operator running the script. Once runtime CVEs are the project's to fix, an unattended path would let a compromised release reach every host that opted in; keeping the operator as the gate everywhere is simpler than keeping it for some hosts.

**Migration is a hard cut.** Drain the Node, rerun the new `install.sh`, undrain. The Node keeps its identity and does not re-register. A stopped Sandbox gets a new Runtime from `Create` in the private containerd on its next start (ADR-0009), its Workspace stays on the host, and images are pulled again. The script prints the commands to remove the old `orrery` namespace from the host containerd but does not run them, because that containerd may still serve Docker.

## Consequences

- ADR-0026's shared host containerd is superseded; its Provider, Isolation Runtime and Egress Rules decisions are unchanged.
- ADR-0023's "Automatic updates are opt-in" is superseded; its release artifacts and pull-based install are unchanged.
- The project owns runtime CVE fixes for every Node, but a fix protects a host only after its operator reruns `install.sh`.
- A host that also runs Docker stores images twice, once per containerd.
- The node package grows by roughly 150–200 MB per arch.
- `install.sh` drops `need ctr` and the updater; `docs/deploy.md`, `docs/spec/runtime.md` and `docs/spec/defaults.md` describe the bundled daemons and manual upgrades.

## Rejected alternatives

- Bundling the binaries but keeping the host's containerd: the versions would still be the operator's, so the CVE and preparation problems remain.
- Supporting a bring-your-own containerd alongside the bundle: version compatibility, the runsc shim path and CVE responsibility would each have two answers.
- A base package plus a runsc/buildkit add-on: doubles release artifacts, checksums and installer branches to save a one-time download.
- Importing images and stopped Sandboxes from the host containerd: Sandboxes already rebuild from `Create`, so the import saves only an image pull.
- Keeping `--auto-update` to deliver runtime CVE fixes faster: it removes the operator's gate on code execution as root on every opted-in host.
- Removing the updater only on Nodes: the Control Plane would keep a second upgrade path with the same exposure.
