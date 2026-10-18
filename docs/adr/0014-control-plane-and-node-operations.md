# ADR-0014: Control Plane and Node operations

- Status: accepted; amended by ADR-0016 and ADR-0017; one Runtime per Node, lost, deletion and paused builds amended in place by #92
- Date: 2026-10-17

## Context

`#38` asked how the platform is run: replicas, background work, migrations, Node admission, liveness, draining, Agent upgrades, protocol compatibility, observability and listening surfaces. Vendors converged on one shape (`docs/research/control-plane-operations-vendor-practice.md`): stateless control-plane processes over one PostgreSQL, work claimed with `FOR UPDATE SKIP LOCKED`, single-instance periodic tasks guarded by an advisory lock (Coder) or a lease row (River), no consensus protocol. Coder's multi-replica HA is a paid feature because the Agent's long-lived connection lands on one replica and commands must be routed to it. No vendor signature-verifies its agent binary: GitHub checks a SHA-256 when one is provided, Coder serves the binary from the server so versions always match.

## Decision

**One process.** The Control Plane is a single process; the REST API, Agent Tool API and Gateway are modules of that binary, and the Node Agent is a second binary. The process holds no authoritative state: everything is in PostgreSQL and memory is cache, so availability is restart plus Reconciler convergence. Multiple replicas are an upgrade path that needs per-Node command routing (`NOTIFY` to the replica holding the stream), not part of this design.

**No job queue.** Durable intent tables (Sandbox desired state, Exec) are the queue: a write sends `NOTIFY` to wake the Reconciler, with periodic polling as the fallback. Admission stays `#34`'s transactional feasible-set FIFO. Each periodic task (Reconciler sweep, purge, GC sweep) runs only after `pg_try_advisory_xact_lock` on its own fixed id succeeds, so the code is already correct for several replicas. No leader election, no River.

**Migrations.** golang-migrate at startup under `pg_advisory_xact_lock`, forward-only with no down migrations. Rollback is restoring a backup. Upgrade is: stop the Control Plane, back up PostgreSQL, start the new version. Backup and availability of PostgreSQL and the object store belong to the deployment; the recovery point is whatever its backups give.

**Node admission.** Only `admin` issues an Enrollment Packet, through REST or the CLI, and issuing is audited. Redeeming it yields a per-Node token stored hashed (ADR-0017). A Node is schedulable as soon as its stream is open and it has reported Runtime Capabilities; there is no approval step. One Node Agent per host, and a Node is that Agent. A Node has exactly one Runtime Provider, chosen by Agent configuration (`--runtime docker|podman|local` with its socket).

**Liveness.** The Node Agent heartbeats on its stream and the Control Plane records `last_seen_at`. Node Unreachable is derived when read (no heartbeat for twice the interval), never stored. The heartbeat carries Runtime health, and an unhealthy Runtime receives no new placement. Unreachable stops new placement only; running Execs follow `#33` and are not failed. A Node is never declared `lost` by a timer: `lost` is an explicit `admin` act, because `#32` requires confirmation that it cannot recover.

Declaring a reachable Node lost is refused with `conflict`; a live Node whose storage failed uses the per-Workspace declaration instead. The declaration is irreversible: it clears the Node's token and pauses it, so a reconnecting Agent receives `UNAUTHENTICATED` and drops its identity, and the host can only come back by enrolling as a new Node. In one transaction it fails every non-deleted Sandbox on the Node (`stopped` ones and `pending` ones bound there included) with `node_lost`, releasing their Allocations and GPUs and ending their read-write Attachments; ends unfinished Execs with `node_lost`; moves `deleting` Workspaces to `deleted` and every other non-terminal Workspace to `lost`; fails running builds with `node_lost`; and fails the ResourceVersion of every running publish with cause `node_lost`. Deleting a `lost` Workspace makes it `deleted` without a command to the Node.

Deleting a Node is a separate act. It requires the Node to be paused with no non-terminal Workspace and no non-terminal Sandbox, build rows included. The row stays as a tombstone with `deleted_at` and no token, its GPU rows are removed, and it is hidden from the Node list and the capacity view; tombstones are never purged. A still-connected Agent receives `UNAUTHENTICATED` and its stream is closed.

**Paused Node.** A server-side `paused` flag: no new Workspaces and no builds are placed there, while Workspaces already bound to it can still start Sandboxes, so their owners can move data out. Running Sandboxes are not evicted. "Drained" is a queryable fact (paused and no running Sandbox), not a state. No eviction, no forced drain.

**Agent restart and upgrade.** An operator action: package manager or redeploy, then `SIGTERM`, replace the binary, start. No self-update and no signature verification. The first `SIGTERM` exits gracefully, a second forces. Exiting stops neither Runtimes nor Execs: each Exec runs under its Exec Supervisor (ADR-0009), which keeps writing output and exit status to the Node, and the restarted Agent adopts the Runtimes through reconciliation and resumes each Exec's output from its last stored sequence. On Local Runtime the supervisor is a detached host process and behaves the same.

**Node retirement.** There is no migration and no eviction. A persistent Workspace leaves a Node the way a caller moves any data: an Exec archives it, the archive is published to a Resource, and the caller restores it into a Workspace on another Node. Then `admin` deletes the Node.

**Protocol compatibility.** The stream handshake carries the Agent version and the Protobuf major and minor; the minor tells the Control Plane which messages the Agent understands. A different major is refused with `incompatible`: the Agent keeps its identity, retries slowly and fails loudly, matching ADR-0010's asymmetric failure rule. Within one major, an Agent minor newer than the Control Plane's is refused; an older one is accepted and the Node is flagged `outdated` in the API. Upgrade the Control Plane first, then Agents. A major change is a stop-the-world window in which both sides change together.

**Streams.** One control stream carries heartbeats, commands and status. Each Exec's output and each PTY attachment rides its own gRPC stream on the same HTTP/2 connection, so bulk output never delays a heartbeat or a cancellation.

**Execution Log storage.** Output Frames of non-PTY Execs are stored in PostgreSQL in chunks, in tables that are never `UNLOGGED`. Each Exec has a durable log cap; reaching it writes an explicit truncation frame and marks the Exec `log_truncated` while the process keeps running. An explicit marker is not silent loss under `#33`. Logs never terminate an Exec: while the Control Plane is unreachable the Exec Supervisor's Node-local output is the Log Spool, bounded by the same cap and ending in the same truncation frame. Retention counts from the Exec's terminal state and defaults to 30 days. Spilling overflow to the object store is the upgrade path.

**Observability.** Structured JSON logs to stdout; the platform stores none of its own logs. Prometheus `/metrics` on `api`, requiring an `admin` token. No distributed tracing; correlation ids in requests and Audit Records tie events together. One purge task every 10 minutes deletes, in batches, the expired rows of every table that has them (ADR-0017).

**Listening surfaces.** Deployment configuration has two `addr:port` entries:

| Surface | Serves | Authentication |
|---|---|---|
| `api` | REST, the Gateway (Data Plane and port forwarding), `/metrics` | API Token; Sandbox Token for Git; `admin` for metrics |
| `node` | Node Agent stream and Enrollment Packet redemption | per-Node token over TLS; the Packet for redemption |

Sandboxes reach `api` with their Sandbox Token (ADR-0016); keeping the other surfaces away from Sandboxes is the deployment's network layout.

**OCI registry.** A separate CNCF distribution process with the S3 driver on the same object store, with redirects enabled so Nodes fetch blobs from the store directly. The Control Plane hands each Node a registry credential over its stream; rotating it is an operator action, so there is no token service. The Control Plane deletes manifests when a SnapshotVersion is deleted; blob garbage collection is an offline operations procedure (read-only mode, then `garbage-collect`).

## Consequences

- A Control Plane outage pauses scheduling, the Gateway and Git authentication until restart; running containers continue.
- A dead Node holds its Workspaces as `unavailable` until an `admin` acts.
- Every Protobuf major change costs a maintenance window.
- A long, noisy Exec loses its tail beyond the cap, visibly.
- Registry blob GC needs a maintenance window, so deleted Snapshot content lingers until one is run.

## Rejected alternatives

- Multiple active replicas: per-Node command routing for availability that restart already gives at single-team scale.
- A general job queue or River: the intent tables are already the queue, and advisory locks cover single-instance periodic work.
- Timer-driven `lost`: a timer guesses, and a wrong guess terminates Workspaces that would have come back.
- Self-updating, signature-verified Agents: no vendor does it, and an operator upgrade is enough for a small fleet.
- A version window spanning two Protobuf majors: every protocol change written twice, permanently.
- Execution Logs in the object store from day one: a second write path before any log has outgrown PostgreSQL.
- A registry token service: one rotated credential covers the only clients, the Nodes.
- `redirect.disable`: Nodes already reach the object store, and multi-gigabyte GPU images should not stream through the registry process.
