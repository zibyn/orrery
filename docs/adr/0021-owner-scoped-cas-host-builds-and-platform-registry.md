# ADR-0021: Owner-scoped CAS, builds on a BuildKit endpoint, and a platform-only registry

- Status: accepted; amends ADR-0012's content addressing and Snapshot import and ADR-0017's Snapshot build in place; build endpoint amended in place by #92
- Date: 2026-10-17

## Context

Grilling M4 (`#82`) found gaps and two contradictions in the locked persistence spec. Two were tested on this machine on 2026-10-17. Object store: RustFS 1.0.2, Garage v2.4.1 and SeaweedFS 4.48 all enforce a signed `x-amz-checksum-sha256` on presigned PUTs and return it from `HeadObject`; `registry:3` (distribution 3.1.2, s3 driver) pushes, and pulls through a 307 to a presigned GET. Builds: in the unprivileged container of `runtime.md` §9 (default seccomp and AppArmor), neither BuildKit-rootless nor buildah starts, because the default seccomp profile forbids creating a user namespace; both run only unconfined, and then still need a host AppArmor profile for their binary path (`apparmor_restrict_unprivileged_userns=1`). kaniko runs, but has no named contexts and silently ignores `RUN --mount=type=secret`. The host's Docker build API (buildx) accepts `--secret`, `--build-context` and `--cgroup-parent`. Three of the resulting choices are hard to reverse and likely to surprise a reader.

Grilling M5 (`#92`, primary sources read on 2026-10-18) found that binding builds to the host Runtime's build API means a second build path on Podman: Podman v6.1.4's compat `/build` and libpod build share one handler that supports named contexts, secrets, `cgroupparent` and `nocache`, but implements no BuildKit `/session`. BuildKit v0.34.0's Go client `Solve` takes named contexts as `context:<name>` and gets local directories, secrets and push credentials from the client over its session; `cgroup-parent` is a frontend attribute. buildkitd in a container needs seccomp and AppArmor unconfined; as a rootful host service it does not. Daytona v0.190.0 calls Docker's `ImageBuild` (BuildKit with a session) on its runner.

## Decision

**CAS objects are deduplicated per owner.** An object's key is `cas/<owner_id>/sha256/<hex>`; the upload skips only objects the same owner already stores.

**A Snapshot build runs on one BuildKit endpoint.** The Node Agent calls `Solve` with the dockerfile frontend through BuildKit's Go client, against the endpoint given by Agent configuration (`--buildkit <addr>`). On a Docker Node it defaults to dockerd's built-in BuildKit and may point at a rootful buildkitd on the host instead. Whether a Node can build is independent of its Runtime: a Podman or Local Node with a buildkitd builds too, and `snapshot_build` is decided by the capability self-check. Named contexts come from session `LocalMounts` over directories assembled from the Node cache, `base` is injected as `context:base=docker-image://…@digest`, Secrets come from the session's secrets provider and push credentials from its auth provider. Resources are limited by a CPU- and memory-limited slice the Agent creates as the Local Runtime does, passed as `cgroup-parent`. The build is still admitted by the Scheduler and holds an Allocation through an internal Sandbox row with no Runtime container. A build with Secrets runs with `no-cache`, because Secrets are not part of the cache key and buildkitd's cache is shared by all its clients. Builds do not count toward User Quota.

**Nodes pull images only from the platform registry.** Every image a Sandbox runs is a SnapshotVersion in the platform registry. External images enter by import, which the Control Plane performs. Platform Snapshots are listed in deployment configuration and imported by the Control Plane at startup.

## Consequences

- The same file uploaded by two Users is stored twice.
- A Node needs a reachable BuildKit endpoint to build; without one it reports `snapshot_build` as `unsupported`.
- Build Secrets defeat the build cache, so builds with them are slower.
- Whether `cgroup-parent` actually confines `RUN` processes to the slice must be verified during implementation; if it does not, builds fall back to a Node-wide build limit.
- Whether the Go client can drive dockerd's built-in BuildKit through dockerd's `/grpc` must be verified during implementation; if it cannot, Docker Nodes use `ImageBuild` with a session instead, as Daytona does.
- A Node that builds also runs Sandboxes; a build-only Node is a later switch.
- A Node needs no egress to public registries, and an upstream image deleted after import keeps running.

## Rejected alternatives

- Global CAS deduplication: declaring someone else's SHA-256 would skip the upload and allow the publish, so knowing a hash would be enough to read the content.
- Building inside a Sandbox (BuildKit-rootless or buildah): neither starts under the default seccomp profile, and running them unconfined weakens the Sandbox boundary for every build.
- kaniko: no named contexts, and it silently drops secret mounts.
- Binding builds to the host Runtime's build API: Podman implements no BuildKit `/session`, so Podman would need a second, differently shaped build request, and a Local Node could never build.
- Letting Nodes pull external images directly: Node egress and upstream availability would decide whether a Sandbox starts, and the platform registry would no longer be the single source of runnable images.
