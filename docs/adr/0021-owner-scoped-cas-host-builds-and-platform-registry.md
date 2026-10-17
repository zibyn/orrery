# ADR-0021: Owner-scoped CAS, builds on the host build API, and a platform-only registry

- Status: accepted; amends ADR-0012's content addressing and Snapshot import and ADR-0017's Snapshot build in place
- Date: 2026-10-17

## Context

Grilling M4 (`#82`) found gaps and two contradictions in the locked persistence spec. Two were tested on this machine on 2026-10-17. Object store: RustFS 1.0.2, Garage v2.4.1 and SeaweedFS 4.48 all enforce a signed `x-amz-checksum-sha256` on presigned PUTs and return it from `HeadObject`; `registry:3` (distribution 3.1.2, s3 driver) pushes, and pulls through a 307 to a presigned GET. Builds: in the unprivileged container of `runtime.md` §9 (default seccomp and AppArmor), neither BuildKit-rootless nor buildah starts, because the default seccomp profile forbids creating a user namespace; both run only unconfined, and then still need a host AppArmor profile for their binary path (`apparmor_restrict_unprivileged_userns=1`). kaniko runs, but has no named contexts and silently ignores `RUN --mount=type=secret`. The host's Docker build API (buildx) accepts `--secret`, `--build-context` and `--cgroup-parent`. Three of the resulting choices are hard to reverse and likely to surprise a reader.

## Decision

**CAS objects are deduplicated per owner.** An object's key is `cas/<owner_id>/sha256/<hex>`; the upload skips only objects the same owner already stores.

**A Snapshot build runs on the host Runtime's build API.** The Node Agent calls Docker's or Podman's build API inside a CPU- and memory-limited slice it creates as the Local Runtime does, passing it as `cgroup-parent`. The build is still admitted by the Scheduler and holds an Allocation through an internal Sandbox row with no Runtime container. A build with Secrets runs with `--no-cache`. Builds do not count toward User Quota.

**Nodes pull images only from the platform registry.** Every image a Sandbox runs is a SnapshotVersion in the platform registry. External images enter by import, which the Control Plane performs. Platform Snapshots are listed in deployment configuration and imported by the Control Plane at startup.

## Consequences

- The same file uploaded by two Users is stored twice.
- A Node needs a Docker or Podman build API on the host; a Local Node cannot build.
- Build Secrets defeat the build cache, so builds with them are slower.
- Whether `cgroup-parent` actually confines `RUN` processes to the slice must be verified during implementation; if it does not, builds fall back to a Node-wide build limit.
- A Node needs no egress to public registries, and an upstream image deleted after import keeps running.

## Rejected alternatives

- Global CAS deduplication: declaring someone else's SHA-256 would skip the upload and allow the publish, so knowing a hash would be enough to read the content.
- Building inside a Sandbox (BuildKit-rootless or buildah): neither starts under the default seccomp profile, and running them unconfined weakens the Sandbox boundary for every build.
- kaniko: no named contexts, and it silently drops secret mounts.
- Letting Nodes pull external images directly: Node egress and upstream availability would decide whether a Sandbox starts, and the platform registry would no longer be the single source of runnable images.
