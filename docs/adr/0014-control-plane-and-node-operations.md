# ADR-0014: Control Plane and Node operations

- Status: accepted
- Date: 2026-10-17

## Context

`#38` asked how the platform is run: replicas, background work, migrations, Node admission, liveness, draining, Agent upgrades, protocol compatibility, observability and listening surfaces. Vendors converged on one shape (`docs/research/control-plane-operations-vendor-practice.md`): stateless control-plane processes over one PostgreSQL, work claimed with `FOR UPDATE SKIP LOCKED`, single-instance periodic tasks guarded by an advisory lock (Coder) or a lease row (River), no consensus protocol. Coder's multi-replica HA is a paid feature because the Agent's long-lived connection lands on one replica and commands must be routed to it. No vendor signature-verifies its agent binary: GitHub checks a SHA-256 when one is provided, Coder serves the binary from the server so versions always match.

## Decision

**One process.** Phase 1 runs a single Control Plane process; the REST API, Agent Tool API and Gateway are modules of that binary, and the Node Agent is a second binary. The process holds no authoritative state: everything is in PostgreSQL and memory is cache, so availability is restart plus Reconciler convergence. Multiple replicas are an upgrade path that needs per-Node command routing (`NOTIFY` to the replica holding the stream); Phase 1 does not build it.

**No job queue.** Durable intent tables (Sandbox desired state, Exec, ExecAttempt) are the queue: a write sends `NOTIFY` to wake the Reconciler, with periodic polling as the fallback. Admission stays `#34`'s transactional feasible-set FIFO. Each periodic task (Reconciler sweep, purge, GC sweep) runs only after `pg_try_advisory_lock` on its own fixed id succeeds, so the code is already correct for several replicas. No leader election, no River.

**Migrations.** golang-migrate at startup under `pg_advisory_xact_lock`, forward-only with no down migrations. Rollback is restoring a backup. Upgrade is: stop the Control Plane, back up PostgreSQL, start the new version. Backup and availability of PostgreSQL and MinIO belong to the deployment; the recovery point is whatever its backups give.

**Node admission.** Only `admin` issues an Enrollment Packet, through REST or the CLI, and issuing is audited. A Node is schedulable as soon as its stream is open and it has reported Runtime Capabilities; there is no approval step. One Node Agent per host, and a Node is that Agent.

**Liveness.** The Node Agent heartbeats on its stream and the Control Plane records `last_seen_at`. Node Unreachable is derived when read (no heartbeat for twice the interval), never stored. Unreachable stops new placement only; running Execs follow `#33` and are not failed. A Node is never declared `lost` by a timer: `lost` is an explicit `admin` act (declare the Node lost, or delete it), because `#32` requires confirmation that it cannot recover.

**Paused Node.** A server-side `paused` flag, with `#34`'s draining semantics: no new allocations, unstarted allocations go back to the queue, running Sandboxes are not evicted. "Drained" is a queryable fact (paused and no running Sandbox), not a state. No eviction, no forced drain.

**Agent restart and upgrade.** An operator action: package manager or redeploy, then `SIGTERM`, replace the binary, start. No self-update and no signature verification. The first `SIGTERM` exits gracefully, a second forces. Exiting does not stop Docker or Podman containers; the restarted Agent adopts them through reconciliation. Local Runtime processes are the Agent's children and end with it; their ExecAttempts converge as `#33` prescribes, and the capability documentation says so.

**Protocol compatibility.** The stream handshake carries the Agent version and the Protobuf major. A different major is refused with `incompatible`: the Agent keeps its identity, retries slowly and fails loudly, matching ADR-0010's asymmetric failure rule. Within one major, any minor or patch skew is accepted and the Node is flagged `outdated` in the API. Upgrade the Control Plane first, then Agents. A major change is a stop-the-world window in which both sides change together.

**Execution Log storage.** Output Frames are stored in PostgreSQL in chunks. Each Exec has a durable log cap; reaching it writes an explicit truncation frame and marks the Exec `log_truncated` while the process keeps running. An explicit marker is not silent loss under `#33`. Retention counts from the Exec's terminal state; audit retention is at least log retention (ADR-0010). Spilling overflow to the object store is the upgrade path.

**Observability.** Structured JSON logs to stdout; the platform stores none of its own logs. Prometheus `/metrics` on the `ops` listener. No distributed tracing in Phase 1; correlation ids in requests and Audit Records tie events together. One purge task every 10 minutes deletes expired Execution Logs, expired Audit Records and ADR-0012's TTL-expired versions.

**Listening surfaces.** Deployment configuration has five separately addressable `addr:port` entries:

| Surface | Serves | Authentication |
|---|---|---|
| `api` | REST and Agent Tool API | API Token |
| `gateway` | Data Plane WebSocket | API Token |
| `node` | Node Agent stream and certificate renewal | mTLS, client certificate required |
| `enroll` | Enrollment Packet redemption only | TLS, no client certificate |
| `ops` | metrics | none; binds loopback by default |

`enroll` is apart from `node` because their TLS configurations differ. The Control Plane adds the `api`, `gateway`, `node` and `enroll` addresses, and the registry address, to the unconditionally subtracted set in every Sandbox's egress (ADR-0010), so unreachability is expressed by address and port. On Local Runtime there is no network namespace and egress is already `best_effort`; the single-use Packet is what remains.

**OCI registry.** A separate CNCF distribution process with the S3 driver on the same MinIO (`forcepathstyle: true`) and `redirect.disable: true`. Authentication is `http.tls.clientcas` set to the platform's private CA: enrolled Nodes pull and push with their Node certificate, so there is no token service. The Control Plane deletes manifests online; blob garbage collection is an offline maintenance operation (read-only mode, then `garbage-collect`), the one exception to ADR-0012's online retryable sweep.

## Consequences

- A Control Plane outage pauses scheduling, the Gateway and the Capability Channel until restart; running containers continue.
- A dead Node holds its Workspaces as `unavailable` until an `admin` acts.
- Every Protobuf major change costs a maintenance window.
- A long, noisy Exec loses its tail beyond the cap, visibly.
- Registry blob GC needs a maintenance window, so deleted Snapshot content lingers until one is run.

## Rejected alternatives

- Multiple active replicas in Phase 1: per-Node command routing for availability that restart already gives at single-team scale.
- A general job queue or River: the intent tables are already the queue, and advisory locks cover single-instance periodic work.
- Timer-driven `lost`: a timer guesses, and a wrong guess terminates Workspaces that would have come back.
- Self-updating, signature-verified Agents: no vendor does it, and an operator upgrade is enough for a small fleet.
- A version window spanning two Protobuf majors: every protocol change written twice, permanently.
- Execution Logs in the object store from day one: a second write path before any log has outgrown PostgreSQL.
- A registry token service: Node certificates already authenticate the only clients.
