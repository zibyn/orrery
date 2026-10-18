# ADR-0015: Execution, Workspace and scheduling rechecked against Coder and Daytona

- Status: accepted; Runtime death, PTY logging and Paused Node amended by ADR-0017; per-GPU share default amended by `docs/spec/defaults.md`; placement among feasible Nodes amended in place by #92
- Date: 2026-10-17

## Context

`#30`–`#34` were rechecked against Coder v2.38.0 and Daytona v0.190.0 (`docs/research/recheck-runtime-and-workspace-vs-coder-daytona.md`, `recheck-api-and-exec-vs-coder-daytona.md`, `recheck-scheduling-vs-coder-daytona.md`). The spine matches both: REST as the one authority, an event-plus-sweep Reconciler over durable desired state, quota holds, feasible-set FIFO, whole-GPU exclusivity, no preemption or migration. A durable Exec is heavier than either vendor, but Daytona's own issues show what it prevents (#4520 a 202 for a command that never ran, #5140 a sandbox still `started` after its daemon was OOM-killed, #5144 output dropped on timeout), so it stays.

Three kinds of gap did not survive the recheck: TTL leases that contradict our own "no timer decides" rule, retry and attempt machinery neither vendor has, and resource leaks both vendors had to plug (idle sandboxes, endless queues, orphan PTYs, unbounded disk). ADR-0009 and ADR-0014 are amended in place; this ADR records what changes in `#31`–`#34`, whose decisions live only in their tickets.

## Decision

**Leases end by an act, never by expiry.** The read-write Workspace Attachment and the Compute Lease lose their TTL and renewal. `#34` already keeps `unknown` allocations and ADR-0014 never declares a Node lost by timer, so an expiring lease could only either double-allocate a GPU or do nothing. Each of them is its row's monotonic generation, used as the fencing token. At most one read-write Workspace Attachment stays; revoking it terminates every Exec in the Sandbox that holds it. Daytona's GPU reservation likewise *"stays reserved ... until it is fully destroyed"*.

**Exec is one row.** ExecAttempt, Attempt Identity, Execution Retry and Exec Result Aggregation are removed. A Sandbox never leaves its Node, so a new Attempt and a redelivery of the same Exec are indistinguishable. The Exec stores timestamps and its Execution Result, and its state is derived from them, as Coder derives a build's. There is no automatic retry: a failure before start ends the Exec with a retryable error code and the caller decides whether to create another. The Idempotency Key is a unique constraint on the caller-supplied key that returns the existing Exec, not a general idempotency table.

**Execs survive a Node Agent restart.** The Exec Supervisor (ADR-0009) keeps a running Exec writing output and exit status on the Node; the Agent resumes from the last stored sequence. A disconnected but living Agent likewise keeps running and replays on reconnect. A Runtime that dies on a live Node ends its Execs with `runtime_lost`, and an `admin`-declared lost Node ends them with `node_lost` (ADR-0017).

**Logs never kill an Exec.** `#33`'s `log_storage_failed` termination is withdrawn; a full spool or reached cap writes the truncation frame and marks `log_truncated` (ADR-0014).

**PTY.** Any number of attachments may send input, as in both vendors. Replay is a bounded ring buffer on the Node, not PostgreSQL. A PTY with no attachment for the idle period (deployment configuration, default 5 minutes) is terminated, as Coder does. The exit status is the last data frame, never only a WebSocket close reason (Daytona PR #4926).

**Node Agent streams.** Exec output and PTY traffic leave the single control stream for per-Exec gRPC streams on the same connection (ADR-0014).

**OpenAPI.** REST/OpenAPI stays the sole semantic authority; whether the document is hand-written or generated from code is an implementation choice. Both vendors generate it.

**Scheduling.**
- Priorities are only `user` and the platform-internal `system`; `high` is removed. Neither vendor has a caller-selectable priority.
- Among feasible Nodes, a GPU request is packed onto the Node left with the fewest free GPUs of the requested model; a GPU-less request prefers Nodes without GPUs, then the first feasible one. Ties break by the Node's `created_at`. The policy is not configurable.
- User Quota bounds GPU count and concurrent Sandboxes only. A stopped Workspace's disk counts toward the Node's disk budget, not User Quota.
- Quota is checked only at admission. Lowering a quota never cancels queued holds or running Sandboxes.
- Every admission request has a deadline, defaulting to a deployment value; reaching it ends the request `admission_timeout` and releases its hold. `#34`'s "no implicit TTL on queueing" is withdrawn: a request bound to a paused or unreachable Node would otherwise wait and hold quota forever.
- Every Sandbox has an Idle Stop: no running Exec and no live PTY or Data Plane connection for its idle period stops it. The default is 15 minutes, set per Sandbox at creation, and 0 disables it. Hard reservations without preemption make a forgotten GPU Sandbox block the queue indefinitely otherwise.
- A GPU Node reserves a CPU and memory share per GPU (Node configuration, default three quarters of what is left after a host reservation, split evenly by card count); GPU-less Sandboxes, Snapshot builds among them, use only the remainder, so they cannot strand idle GPUs.
- Disk is admission accounting, not isolation, except under XFS project quotas (ADR-0009). A Node below its disk watermark accepts no new allocations.

**Workspace.**
- Provisioning retries with exponential backoff up to a limit, exposes a visible `provisioning_stalled` condition, and can be deleted at any time.
- `admin` may abandon cleanup of a Workspace stuck in `deleting` on a live Node; the abandonment is an Audit Record. This is Coder's orphan delete.

## Consequences

- An Agent upgrade no longer ends long GPU runs, at the cost of a second read-only mount into every Runtime.
- Callers own retries before start; the platform never runs a command twice on its own.
- Idle Sandboxes and unattended PTYs release their resources without operator action.
- Disk isolation depends on the Node's filesystem, and the capability report says which.

## Rejected alternatives

- Keeping TTL leases: incompatible with `unknown` being held and with `lost` being an `admin` act.
- Coder's answer to Agent restart (end every running Exec): an Agent upgrade would kill every training run.
- A single PTY controller with a control-transfer protocol: neither vendor needs one.
- GPU Nodes exclusive to GPU Sandboxes (Daytona): a single-team deployment may have no CPU-only Node.
- Multi-dimensional User Quota: Coder uses one scalar; GPUs and Sandbox count are what is scarce here.
