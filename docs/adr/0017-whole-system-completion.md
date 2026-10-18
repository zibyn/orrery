# ADR-0017: The whole system, completed against Coder and Daytona

- Status: accepted; Sandbox causes, Node Resource cache, Snapshot import and build, default Snapshot and object store access amended in place by #82 (ADR-0021); build endpoint, paused and deleted Nodes, `node_lost` cascade and `workspace_lost` amended in place by #92
- Date: 2026-10-17

## Context

After the per-ticket rechecks (ADR-0015, ADR-0016), the whole system was audited at once: an internal consistency audit (`docs/research/internal-consistency-audit.md`), a feature-surface comparison (`system-feature-gap-vs-coder-daytona.md`) and the remaining rechecks of Git and Secrets, persistence and operations (`recheck-git-secrets-…`, `recheck-persistence-…`, `recheck-operations-…`). They found lifecycle edges with no exit, a Runtime death that would hold a GPU forever, features both vendors ship and we never mentioned, and several mechanisms heavier than either vendor for no gain. This ADR closes them. Where it contradicts an earlier ADR, this one wins; ADR-0010, ADR-0012 and ADR-0014 are amended in place where noted.

## Decision

### Lifecycle

**Sandbox.** States are `pending` (queued for admission) → `provisioning` → `ready` → `stopping` → `stopped`, plus `deleting` → `deleted`, plus terminal `failed` with a cause: `admission_timeout`, `unsatisfiable`, `quota_exceeded`, `runtime_create_failed`, `image_pull_failed`, `resource_fetch_failed` (added in place by #82), `workspace_lost` (added in place by #92), `node_lost`. `start` on a `stopped` Sandbox re-enters admission at `pending`. Stopping or deleting releases the Sandbox's compute and its read-write Attachment; a start that finds another holder of the read-write Attachment fails with `conflict`. Deleting a Sandbox with running Execs cancels them and then deletes, as both vendors force-delete; ADR-0010's `conflict` on unfinished Execs is withdrawn.

**Runtime death on a live Node.** When the Node Agent reports that a Runtime is gone (OOM-killed container, daemon restart, host reboot), its running Execs end with `runtime_lost`, and the Sandbox becomes `stopped` with that cause and releases its resources. An unreachable Node releases nothing and is only marked unavailable; once `admin` declares it lost, its Sandboxes become `failed` with `node_lost`, and the rest of the cascade is ADR-0014's Liveness.

**Workspace.** Provisioning that exhausts its retries ends `failed`. `admin` may declare one Workspace lost on a live Node whose storage failed; the Sandboxes attached to it, running or stopped, become `failed` with `workspace_lost`, and the Reconciler stops their Runtimes, so their Execs end with `runtime_lost`.

### Exec and PTY

PTY output is not an Execution Log: it lives only in the Node's ring buffer, and the durable log is for non-PTY Execs. A PTY runs under the Exec Supervisor too, so it survives a Node Agent restart; an idle reap ends it with `pty_idle`. The Supervisor enforces the Exec timeout even while the Agent is absent and receives signals, stdin and window resizes relayed by the Agent. Its Node-local record is removed once the Exec is terminal and its output stored. `oom_killed` is an Execution Result cause, read from cgroup `memory.events`. Every create endpoint accepts an Idempotency Key, unique per (User, endpoint, key); a repeat returns the existing resource and the body is not compared.

### Nodes and surfaces

**Node identity.** Redeeming an Enrollment Packet yields a per-Node token, stored hashed; deleting the Node or declaring it lost clears it. There is no private CA, no mTLS and no certificate renewal; ADR-0010's asymmetric failure rule still governs the Agent. Both vendors use a bearer key per runner or provisioner.

**Listeners.** Two: `api` serves REST, the Gateway (Data Plane and port forwarding) and `/metrics`, which requires an `admin` token; `node` serves the Agent stream and Enrollment Packet redemption, both over TLS.

**Registry.** The Control Plane hands each Node a registry credential over its stream; rotating it is an operator action. Redirects are enabled, so Nodes fetch blobs from the object store directly.

**Health and versions.** The heartbeat carries Runtime health; a Node whose Runtime is unhealthy receives no new placement. An Agent whose Protobuf minor is newer than the Control Plane's is refused, which enforces "Control Plane first".

**Paused and deleted Nodes.** Paused blocks new Workspaces and builds only; Workspaces already bound there can still start Sandboxes, so their owners can archive them out. Deleting a Node requires it to be paused with no non-terminal Workspace and no non-terminal Sandbox, build rows included, leaves a tombstone (ADR-0014), and is a separate act from declaring it lost.

**Periodic work.** Periodic tasks take `pg_try_advisory_xact_lock`. Purge covers every table with expiring rows (Execution Logs, Audit Records, expired versions, used or expired Enrollment Packets, expired API Tokens and Idempotency Keys, terminal admission requests) in batches. Execution Log tables are never `UNLOGGED`; log retention defaults to 30 days.

### Ports

The Gateway forwards to a Sandbox's ports for its owner or an admin: a per-port subdomain, through which the Node Agent enters the Sandbox's network namespace and dials `127.0.0.1:PORT`, plus a TCP tunnel in the CLI (`stars-sandbox port-forward`). Nothing in a Sandbox is exposed on a network address, and there is no public sharing. SSH and IDE access is not a platform feature; a User runs `sshd` in the Sandbox and tunnels to it.

### Users, tokens and audit

`admin` creates and deletes Users and sets User Quota; the first `admin` is created by the server-side CLI on the Control Plane host. A User holds any number of named API Tokens, each with optional expiry, `last_used`, individual revocation, and a scope of `user`, `read_only` or `admin`. `admin` reads Audit Records through `GET /audit`. The Gateway authorizes once when a connection opens; plain HTTP requests carry a signed ticket valid for at most one minute; the caller's API Token is stripped before anything reaches the Sandbox.

### Persistence

**Artifact is withdrawn as a concept.** Publishing is a REST operation on a Workspace, `publish(path → Resource)`, which creates a ResourceVersion and moves that Resource's current pointer; older versions expire by TTL. The persistent concepts are Resource and Snapshot, besides the Workspace.

**Uploads** are limited to 5 GiB per file and larger files fail explicitly, because S3 verifies a full-object SHA-256 only on single PUTs.

**The Node's Resource cache** counts toward its disk budget, and unreferenced entries are evicted LRU below the disk watermark. Provisioning completes only after inputs are fetched, with a visible condition meanwhile. (Amended in place by #82:) Objects are cached by SHA-256 and executable bit as read-only files; a version tree is assembled from hard links and bind-mounted read-only at `/resources/<mount>`. Eviction is by version tree, and an object goes once no tree links it. Docker images no Sandbox on the Node references, stopped ones included, and the BuildKit cache count toward the same budget and are evicted with it. Fetching retries with the Workspace provisioning backoff; when it is exhausted the Sandbox ends `failed` with `resource_fetch_failed`. Local Runtime reports `resource_mount` as `unsupported`.

**Snapshot.** Any `ref@sha256:` may be imported into the platform registry (amended in place by #82), and a build may start from any base; platform images are recommended defaults, not a whitelist. A SnapshotVersion is deleted only explicitly, never by TTL, and a stopped Sandbox counts as a reference. Registry blob GC is an operations procedure, not part of the design. A build is a `system`-priority job on a Node: the Node Agent drives a BuildKit endpoint (ADR-0021, amended in place by #92). It still passes Scheduler admission and holds an Allocation; an internal Sandbox row with no Runtime container carries the Allocation and an Exec whose Execution Log is the build log, and a failed build ends the SnapshotVersion `failed`. (Amended in place by #82: a build was a platform-internal `system`-priority Sandbox, but BuildKit-rootless and buildah cannot start in an unprivileged container under the default seccomp profile; ADR-0021.) Build-time secrets are the caller's Secrets, and a build that uses any runs without the build cache. Whether a Node can build is its self-checked `snapshot_build`, independent of its Runtime. A `container` Sandbox without a Snapshot uses the deployment's default platform Snapshot; a `local` one has none (ADR-0009).

**On restart** Secrets are re-resolved by name; ResourceVersions and the Snapshot digest stay as pinned at creation.

**Object store.** Nodes read and write it only through presigned URLs issued by the Control Plane, which they request with the `Presign` RPC (added in place by #82). The specification requires an S3-compatible object store, not MinIO, which is archived.

### Git and Secrets

- **Refresh** of one Git Account Link is serialized; the new token is stored before anything else; only an explicit permanent error such as `invalid_grant` invalidates the link, and everything else is transient; tokens refresh ahead of expiry.
- **Issue** validates the token first and returns the authorization URL if it fails. A host matching no Git Provider passes through silently to git's next helper. The username is fixed per provider type (`oauth2` for GitLab).
- **Providers** default to PKCE S256, which can be disabled per provider, and configure a single `base_url` from which endpoints are derived by type. Upstream revocation on unlink is best effort and never blocks the unlink.
- **The helper** is installed through `GIT_CONFIG_COUNT` in each Exec's environment, after clearing the helper list; no file is written.
- **Secrets** reject reserved names (`GIT_*`, `LD_*`, `PATH`, `HOME`, the platform prefix). A User may hold at most 50, each at most 24 KiB. The Node Agent injects them into each Exec's environment, never into the container configuration.
- **Encryption keys** are deployment-time file references, and each ciphertext carries its `key_id` for rotation.

### API surface

- Sandboxes have an optional name, unique per User, and labels. The list filters by owner, state, label, Node and has-GPU, with cursor pagination.
- `?wait_for=<state>` long-polls a Sandbox state.
- `GET /users/me/quota` reports quota usage. A read-only capacity view lists each Node's GPU model, total, allocated and status. A Sandbox shows `queue_position` and `blocked_reason` while `pending`.
- `GET /sandboxes/{id}/execs` lists a Sandbox's Execs.
- Files: list, stat, read with range, write, mkdir, move, delete, and tar upload and download.
- The Agent Tool API is MCP, served by `stars-sandbox mcp` over stdio. Its tools are generated from the OpenAPI document, plus two composite tools: run an Exec and wait for its result, and a PTY send-and-read.
- Per-Sandbox CPU, memory and GPU utilization are exported to Prometheus with Sandbox labels.
- SDKs are Python and Go. Docker inside a Sandbox is unsupported, and the capability documentation says so.

### Command lines

The CLI a caller uses is separate from the server-side commands. `stars-sandbox` is the external CLI: it speaks only the public REST API and the Gateway, with an API Token or, inside a Sandbox, the Sandbox Token, and holds every caller-facing command, `admin` operations through REST included. The server-side binaries are `stars-sandbox-server` (run the Control Plane, migrate, bootstrap the first `admin`) and `stars-sandbox-agent` (enroll and run a Node Agent); they are installed only on platform hosts and are never needed by a caller.

### Web console

A Web console is part of the system as a separate module, implemented later. It is a client of the public REST API and the Gateway only, with no private endpoints, so it can never be the only way to do something. Its login and serving are ADR-0020.

## Consequences

- Every failure has a terminal state and every hold is released by an event; nothing waits on a timer that the design forbids.
- A dev server, TensorBoard or Jupyter in a Sandbox is reachable by its owner without opening a port on the Sandbox.
- Deleting a Node is the whole revocation story; a leaked registry credential needs an operator rotation.
- Sandbox Tokens and port-forward tickets mean `api` must be reachable from Sandboxes and browsers alike.

## Rejected alternatives

- Keeping mTLS for Nodes: neither vendor issues per-agent certificates, and a hashed per-Node token gives the same per-Node revocation.
- Five or three listeners: every surface but `node` already authenticates with an API Token.
- Artifact as a declared Exec output: no vendor has one, it had no target Resource, and an explicit publish covers it.
- A base-image whitelist and TTL-expired SnapshotVersions: both vendors accept any image and neither expires environment versions.
- Platform SSH: a tunnel to a User's own `sshd` gives the same reach with no platform key management.
- A Web console with its own backend: it would become a second source of semantics.
