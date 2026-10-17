-- Users

-- name: CreateUser :one
INSERT INTO users (
    id, name, email, is_admin, login_type, password_hash,
    gpu_quota, sandbox_quota, idempotency_key, created_by
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING *;

-- name: GetUser :one
SELECT * FROM users WHERE id = $1 AND deleted_at IS NULL;

-- name: GetUserByName :one
SELECT * FROM users WHERE name = $1 AND deleted_at IS NULL;

-- LockUser serializes quota checks of one User: concurrent creates and starts
-- cannot both observe the same usage (ADR-0015).
-- name: LockUser :one
SELECT * FROM users WHERE id = $1 AND deleted_at IS NULL FOR UPDATE;

-- name: GetUserByIdempotencyKey :one
SELECT * FROM users
WHERE created_by = $1 AND idempotency_key = $2;

-- name: CountUsers :one
SELECT count(*) FROM users;

-- BootstrapAdmin creates the first admin only while the table is empty.
-- name: BootstrapAdmin :one
INSERT INTO users (
    id, name, email, is_admin, login_type, password_hash,
    gpu_quota, sandbox_quota
)
SELECT $1, $2, $3, true, 'password', $4, $5, $6
WHERE NOT EXISTS (SELECT 1 FROM users)
RETURNING *;

-- name: ListUsers :many
SELECT * FROM users
WHERE deleted_at IS NULL
  AND (sqlc.narg('after')::uuid IS NULL OR id > sqlc.narg('after'))
ORDER BY id
LIMIT sqlc.arg('limit');

-- name: UpdateUser :one
UPDATE users
SET name = $2,
    email = $3,
    is_admin = $4,
    login_type = $5,
    password_hash = $6,
    gpu_quota = $7,
    sandbox_quota = $8,
    updated_at = now()
WHERE id = $1 AND deleted_at IS NULL
RETURNING *;

-- name: SetUserQuota :one
UPDATE users
SET gpu_quota = $2, sandbox_quota = $3, updated_at = now()
WHERE id = $1 AND deleted_at IS NULL
RETURNING *;

-- name: UpdateUserPassword :one
UPDATE users
SET password_hash = $2, updated_at = now()
WHERE id = $1 AND deleted_at IS NULL
RETURNING *;

-- name: SoftDeleteUser :one
UPDATE users
SET deleted_at = now(), updated_at = now()
WHERE id = $1 AND deleted_at IS NULL
RETURNING *;

-- API tokens

-- name: CreateToken :one
INSERT INTO api_tokens (
    id, user_id, kind, name, secret_hash, scope, expires_at, idempotency_key
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: GetToken :one
SELECT * FROM api_tokens WHERE id = $1;

-- name: GetTokenWithUser :one
SELECT t.id, t.user_id, t.kind, t.name, t.secret_hash, t.scope,
       t.expires_at, t.last_used_at, t.idempotency_key, t.created_at,
       u.is_admin AS user_is_admin
FROM api_tokens t
JOIN users u ON u.id = t.user_id
WHERE t.id = $1 AND u.deleted_at IS NULL;

-- name: GetTokenByIdempotencyKey :one
SELECT * FROM api_tokens WHERE user_id = $1 AND idempotency_key = $2;

-- name: ListTokens :many
SELECT * FROM api_tokens
WHERE user_id = $1
  AND (sqlc.narg('after')::uuid IS NULL OR id > sqlc.narg('after'))
ORDER BY id
LIMIT sqlc.arg('limit');

-- name: DeleteToken :execrows
DELETE FROM api_tokens WHERE id = $1 AND user_id = $2;

-- A bounded delete keeps one purge tick from holding a long transaction.
-- name: PurgeExpiredTokens :exec
DELETE FROM api_tokens
WHERE id IN (
    SELECT id FROM api_tokens
    WHERE expires_at IS NOT NULL AND expires_at < now()
    LIMIT 10000
);

-- TouchToken records use and slides a Session's idle expiry (defaults.md
-- "身份"); API Tokens keep their fixed expiry. The caller supplies the new
-- expiry so the idle TTL has one source of truth.
-- name: TouchToken :exec
UPDATE api_tokens
SET last_used_at = now(),
    expires_at = CASE
        WHEN kind = 'session' THEN sqlc.arg('session_expires_at')::timestamptz
        ELSE expires_at
    END
WHERE id = sqlc.arg('id');

-- name: DeleteUserSessions :execrows
DELETE FROM api_tokens WHERE user_id = $1 AND kind = 'session';

-- Audit records

-- name: InsertAudit :exec
INSERT INTO audit_records (
    actor_user_id, actor_token_id, action, target_type, target_id, correlation_id, detail
) VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: ListAudit :many
SELECT a.*, u.name AS actor_name
FROM audit_records a
LEFT JOIN users u ON u.id = a.actor_user_id
WHERE (sqlc.narg('actor_id')::uuid IS NULL OR a.actor_user_id = sqlc.narg('actor_id'))
  AND (sqlc.narg('action')::text IS NULL OR a.action = sqlc.narg('action'))
  AND (sqlc.narg('target_type')::text IS NULL OR a.target_type = sqlc.narg('target_type'))
  AND (sqlc.narg('target_id')::text IS NULL OR a.target_id = sqlc.narg('target_id'))
  AND (sqlc.narg('from_at')::timestamptz IS NULL OR a.at >= sqlc.narg('from_at'))
  AND (sqlc.narg('to_at')::timestamptz IS NULL OR a.at < sqlc.narg('to_at'))
  AND (sqlc.narg('before_id')::bigint IS NULL OR a.id < sqlc.narg('before_id'))
ORDER BY a.id DESC
LIMIT sqlc.arg('limit');

-- Audit retention is deployment configuration; 0 means keep forever, so the
-- caller skips this query then (defaults.md).
-- name: PurgeAuditRecords :exec
DELETE FROM audit_records
WHERE id IN (
    SELECT id FROM audit_records
    WHERE at < now() - sqlc.arg('retention_seconds')::bigint * interval '1 second'
    LIMIT 10000
);

-- name: PurgeUserIdempotencyKeys :exec
UPDATE users SET idempotency_key = NULL
WHERE idempotency_key IS NOT NULL AND created_at < now() - interval '24 hours';

-- name: PurgeTokenIdempotencyKeys :exec
UPDATE api_tokens SET idempotency_key = NULL
WHERE idempotency_key IS NOT NULL AND created_at < now() - interval '24 hours';

-- name: PurgeSandboxIdempotencyKeys :exec
UPDATE sandboxes SET idempotency_key = NULL
WHERE idempotency_key IS NOT NULL AND created_at < now() - interval '24 hours';

-- name: PurgeWorkspaceIdempotencyKeys :exec
UPDATE workspaces SET idempotency_key = NULL
WHERE idempotency_key IS NOT NULL AND created_at < now() - interval '24 hours';

-- Enrollment packets and nodes

-- name: CreateEnrollmentPacket :one
INSERT INTO enrollment_packets (id, secret_hash, expires_at, created_by)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetEnrollmentPacket :one
SELECT * FROM enrollment_packets WHERE id = $1;

-- RedeemEnrollmentPacket is the single-use claim: it fails when the packet was
-- already used or has expired.
-- name: RedeemEnrollmentPacket :one
UPDATE enrollment_packets
SET used_at = now()
WHERE id = $1 AND used_at IS NULL AND expires_at > now()
RETURNING *;

-- name: CreateNode :one
INSERT INTO nodes (
    id, name, token_hash, agent_version, proto_major, proto_minor, last_seen_at
) VALUES ($1, $2, $3, $4, $5, $6, now())
RETURNING *;

-- name: GetNode :one
SELECT * FROM nodes WHERE id = $1;

-- name: GetNodeByName :one
SELECT * FROM nodes WHERE name = $1;

-- name: ListNodes :many
SELECT * FROM nodes
WHERE (sqlc.narg('after')::uuid IS NULL OR id > sqlc.narg('after'))
ORDER BY id
LIMIT sqlc.arg('limit');

-- name: NodeHello :exec
UPDATE nodes
SET last_seen_at = now(),
    agent_version = $2,
    proto_major = $3,
    proto_minor = $4
WHERE id = $1;

-- name: NodeHeartbeat :exec
UPDATE nodes
SET last_seen_at = now(),
    runtime_health = $2,
    hardware = $3,
    reserved_cpu = $4,
    reserved_memory = $5,
    cpu_per_gpu = $6,
    memory_per_gpu = $7
WHERE id = $1;

-- name: SetNodeCapabilities :exec
UPDATE nodes SET capabilities = $2 WHERE id = $1;

-- name: DeleteNodeGPUs :exec
DELETE FROM gpus WHERE node_id = $1;

-- name: InsertNodeGPU :exec
INSERT INTO gpus (uuid, node_id, model) VALUES ($1, $2, $3)
ON CONFLICT (uuid) DO UPDATE SET node_id = EXCLUDED.node_id, model = EXCLUDED.model;

-- name: ListNodeGPUs :many
SELECT * FROM gpus WHERE node_id = $1 ORDER BY uuid;

-- ListNodeGPUUsage is the capacity view: per model, how many cards the Node
-- reports and how many carry an Allocation (api.md §3).
-- name: ListNodeGPUUsage :many
SELECT g.model,
       count(*)::bigint AS total,
       count(ga.gpu_uuid)::bigint AS allocated
FROM gpus g
LEFT JOIN gpu_allocations ga ON ga.gpu_uuid = g.uuid
WHERE g.node_id = $1
GROUP BY g.model
ORDER BY g.model;

-- ListNodeFreeGPUs is the admission sweep's device list, in a stable order so
-- placements are reproducible.
-- name: ListNodeFreeGPUs :many
SELECT g.* FROM gpus g
LEFT JOIN gpu_allocations ga ON ga.gpu_uuid = g.uuid
WHERE g.node_id = $1 AND ga.gpu_uuid IS NULL
ORDER BY g.uuid;

-- name: PurgeEnrollmentPackets :exec
DELETE FROM enrollment_packets
WHERE used_at IS NOT NULL OR expires_at < now();

-- Sandboxes

-- name: CreateSandbox :one
INSERT INTO sandboxes (
    id, owner_id, name, labels, desired_state, state, snapshot_digest,
    cpu, memory, gpu_count, gpu_model, idle_stop_seconds, generation, idempotency_key
) VALUES ($1, $2, $3, $4, 'running', 'pending', $5, $6, $7, $8, $9, $10, 1, $11)
RETURNING *;

-- name: GetSandbox :one
SELECT * FROM sandboxes WHERE id = $1 AND state <> 'deleted';

-- name: GetSandboxIncludingDeleted :one
SELECT * FROM sandboxes WHERE id = $1;

-- name: GetSandboxByIdempotencyKey :one
SELECT * FROM sandboxes
WHERE owner_id = $1 AND idempotency_key = $2 AND state <> 'deleted';

-- name: ListSandboxes :many
SELECT * FROM sandboxes
WHERE owner_id = sqlc.arg('owner_id')
  AND state <> 'deleted'
  AND (sqlc.narg('state')::text IS NULL OR state = sqlc.narg('state'))
  AND (sqlc.narg('node_id')::uuid IS NULL OR node_id = sqlc.narg('node_id'))
  AND (sqlc.narg('has_gpu')::boolean IS NULL OR (gpu_count > 0) = sqlc.narg('has_gpu'))
  AND (sqlc.narg('label_key')::text IS NULL
       OR (jsonb_exists(labels, sqlc.narg('label_key')::text)
           AND labels ->> sqlc.narg('label_key')::text = sqlc.narg('label_value')::text))
  AND (sqlc.narg('after')::uuid IS NULL OR id > sqlc.narg('after'))
ORDER BY id
LIMIT sqlc.arg('limit');

-- An open admission request is part of active usage, so the pending state is
-- the quota hold (ADR-0015).
-- name: CountActiveSandboxResources :one
SELECT count(*)::bigint AS sandboxes,
       COALESCE(SUM(gpu_count), 0)::bigint AS gpus
FROM sandboxes
WHERE owner_id = $1 AND state IN ('pending', 'provisioning', 'ready', 'stopping');

-- name: StartSandbox :one
UPDATE sandboxes
SET desired_state = 'running', state = 'pending', cause = NULL,
    generation = generation + 1, updated_at = now()
WHERE id = $1 AND state = 'stopped'
RETURNING *;

-- name: StopSandbox :one
UPDATE sandboxes
SET desired_state = 'stopped', state = 'stopping',
    generation = generation + 1, updated_at = now()
WHERE id = $1 AND state IN ('pending', 'provisioning', 'ready')
RETURNING *;

-- name: TouchSandboxActivity :exec
UPDATE sandboxes SET last_activity_at = now() WHERE id = $1 AND state = 'ready';

-- The Idle Stop sweep's candidates: running Sandboxes with a finite idle
-- period (defaults.md, ADR-0015).
-- name: ListIdleSandboxes :many
SELECT * FROM sandboxes
WHERE state = 'ready' AND desired_state = 'running' AND idle_stop_seconds > 0
ORDER BY id;

-- name: DeleteSandbox :one
UPDATE sandboxes
SET desired_state = 'deleted', state = 'deleting', deleted_at = now(),
    generation = generation + 1, updated_at = now()
WHERE id = $1 AND state <> 'deleted'
RETURNING *;

-- Reconciler: these transitions are one-process writes and only guard the
-- state they expect, not the generation.

-- name: ListReconcilableSandboxes :many
SELECT * FROM sandboxes
WHERE state IN ('pending', 'provisioning', 'ready', 'stopping', 'deleting')
ORDER BY id;

-- name: SetSandboxNode :execrows
UPDATE sandboxes
SET node_id = $2, updated_at = now()
WHERE id = $1 AND node_id IS NULL AND state = 'pending' AND desired_state = 'running';

-- name: SetSandboxProvisioning :execrows
UPDATE sandboxes
SET state = 'provisioning', updated_at = now()
WHERE id = $1 AND state = 'pending' AND desired_state = 'running';

-- name: SetSandboxStopping :execrows
UPDATE sandboxes
SET state = 'stopping', updated_at = now()
WHERE id = $1 AND desired_state = 'stopped'
  AND state IN ('pending', 'provisioning', 'ready');

-- name: SetSandboxStopped :execrows
UPDATE sandboxes
SET state = 'stopped', updated_at = now()
WHERE id = $1 AND desired_state = 'stopped'
  AND state IN ('pending', 'provisioning', 'ready', 'stopping');

-- name: MarkSandboxDeleted :execrows
UPDATE sandboxes
SET state = 'deleted', updated_at = now()
WHERE id = $1 AND desired_state = 'deleted';

-- Observed transitions are fenced by the generation the Agent reports.

-- name: MarkSandboxReady :execrows
UPDATE sandboxes
SET state = 'ready', cause = NULL, last_activity_at = now(), updated_at = now()
WHERE id = $1 AND generation = $2 AND desired_state = 'running'
  AND state IN ('pending', 'provisioning');

-- name: MarkSandboxFailed :execrows
UPDATE sandboxes
SET state = 'failed', cause = $3, desired_state = 'stopped', updated_at = now()
WHERE id = $1 AND generation = $2 AND state <> 'deleted';

-- name: MarkSandboxRuntimeLost :execrows
UPDATE sandboxes
SET state = 'stopped', cause = $3, desired_state = 'stopped', updated_at = now()
WHERE id = $1 AND generation = $2 AND desired_state = 'running'
  AND state IN ('pending', 'provisioning', 'ready');

-- FailPendingSandbox ends a queued Sandbox without touching a Sandbox that a
-- concurrent stop or delete already moved on (admission_timeout,
-- unsatisfiable, quota_exceeded).
-- name: FailPendingSandbox :execrows
UPDATE sandboxes
SET state = 'failed', cause = $3, desired_state = 'stopped', updated_at = now()
WHERE id = $1 AND generation = $2 AND state = 'pending' AND desired_state = 'running';

-- TouchPendingSandbox locks the row and re-checks the state the admission
-- sweep read, so a concurrent stop or delete cannot be handed an Allocation.
-- name: TouchPendingSandbox :execrows
UPDATE sandboxes
SET updated_at = updated_at
WHERE id = $1 AND generation = $2 AND state = 'pending' AND desired_state = 'running';

-- name: MarkSandboxStopped :execrows
UPDATE sandboxes
SET state = 'stopped', updated_at = now()
WHERE id = $1 AND generation = $2 AND desired_state = 'stopped'
  AND state IN ('pending', 'provisioning', 'ready', 'stopping');

-- Admission requests

-- name: CreateAdmissionRequest :one
INSERT INTO admission_requests (id, sandbox_id, priority, deadline_at)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: EndAdmissionRequest :execrows
UPDATE admission_requests
SET ended_at = now(), outcome = $2
WHERE sandbox_id = $1 AND ended_at IS NULL;

-- The feasible-set FIFO queue: every open request in admission order. A
-- request that does not fit is skipped, not allowed to block later ones.
-- name: ListOpenAdmissionRequests :many
SELECT sqlc.embed(a), sqlc.embed(s)
FROM admission_requests a
JOIN sandboxes s ON s.id = a.sandbox_id
WHERE a.ended_at IS NULL
ORDER BY CASE a.priority WHEN 'system' THEN 0 ELSE 1 END, a.enqueued_at, a.id;

-- name: AdmissionQueuePosition :one
-- HAVING drops the aggregate row when the Sandbox has no open request, so
-- pgx.ErrNoRows means "not queued" rather than position zero.
SELECT count(*)::bigint AS position
FROM admission_requests other
JOIN admission_requests me ON me.sandbox_id = $1 AND me.ended_at IS NULL
WHERE other.ended_at IS NULL
  AND (CASE other.priority WHEN 'system' THEN 0 ELSE 1 END, other.enqueued_at, other.id)
   <= (CASE me.priority WHEN 'system' THEN 0 ELSE 1 END, me.enqueued_at, me.id)
HAVING count(*) > 0;

-- Ended requests no longer hold quota or a queue position.
-- name: PurgeTerminalAdmissionRequests :exec
DELETE FROM admission_requests
WHERE id IN (
    SELECT id FROM admission_requests
    WHERE ended_at IS NOT NULL
    LIMIT 10000
);

-- Workspaces

-- name: CreateWorkspace :one
INSERT INTO workspaces (id, owner_id, kind, sandbox_id, state, storage_handle, generation, idempotency_key)
VALUES ($1, $2, $3, $4, 'provisioning', $5, 1, $6)
RETURNING *;

-- name: GetWorkspace :one
SELECT * FROM workspaces WHERE id = $1;

-- name: GetWorkspaceByIdempotencyKey :one
SELECT * FROM workspaces
WHERE owner_id = $1 AND idempotency_key = $2 AND state <> 'deleted';

-- GetSandboxWorkspace is the Sandbox's Workspace through the durable
-- association; the Attachment row exists only while the Sandbox runs.
-- name: GetSandboxWorkspace :one
SELECT w.* FROM workspaces w
JOIN sandboxes s ON s.workspace_id = w.id
WHERE s.id = $1 AND w.state <> 'deleted';

-- name: ListWorkspaces :many
SELECT * FROM workspaces
WHERE owner_id = sqlc.arg('owner_id')
  AND state <> 'deleted'
  AND (sqlc.narg('after')::uuid IS NULL OR id > sqlc.narg('after'))
ORDER BY id
LIMIT sqlc.arg('limit');

-- name: ListProvisioningWorkspaces :many
SELECT * FROM workspaces WHERE state = 'provisioning' ORDER BY id;

-- name: SetWorkspaceConditions :exec
UPDATE workspaces SET conditions = $2, updated_at = now() WHERE id = $1;

-- name: MarkWorkspaceFailed :execrows
UPDATE workspaces
SET state = 'failed', cause = $3, updated_at = now()
WHERE id = $1 AND generation = $2 AND state = 'provisioning';

-- name: ListDeletingWorkspaces :many
SELECT * FROM workspaces WHERE state = 'deleting' ORDER BY id;

-- name: SetWorkspaceNode :execrows
UPDATE workspaces
SET node_id = $2, state = 'provisioning', generation = generation + 1, updated_at = now()
WHERE id = $1 AND node_id IS NULL;

-- name: MarkWorkspaceReady :execrows
UPDATE workspaces
SET state = 'ready', updated_at = now()
WHERE id = $1 AND generation = $2 AND state = 'provisioning';

-- name: SetWorkspaceDeleting :execrows
UPDATE workspaces
SET state = 'deleting', generation = generation + 1, updated_at = now()
WHERE id = $1 AND state <> 'deleted';

-- name: MarkWorkspaceDeleted :execrows
UPDATE workspaces
SET state = 'deleted', deleted_at = now(), updated_at = now()
WHERE id = $1 AND generation = $2;

-- Attachments

-- name: CreateAttachment :one
INSERT INTO attachments (sandbox_id, workspace_id, mode, generation)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetAttachment :one
SELECT * FROM attachments WHERE sandbox_id = $1;

-- name: DeleteAttachment :exec
DELETE FROM attachments WHERE sandbox_id = $1;

-- name: SetSandboxWorkspace :exec
UPDATE sandboxes
SET workspace_id = $2, workspace_mode = $3, updated_at = now()
WHERE id = $1;

-- Allocations

-- name: CreateAllocation :exec
INSERT INTO allocations (sandbox_id, node_id, cpu, memory, disk, generation)
VALUES ($1, $2, $3, $4, 0, $5)
ON CONFLICT (sandbox_id) DO NOTHING;

-- name: DeleteAllocation :exec
DELETE FROM allocations WHERE sandbox_id = $1;

-- name: DeleteGPUAllocations :exec
DELETE FROM gpu_allocations WHERE sandbox_id = $1;

-- NodePlacement is everything admission needs from one Node in a single round
-- trip: total usage, the GPU-less usage that shares the remainder, and the
-- reported card count that sizes the per-GPU share (ADR-0015).
-- name: NodePlacement :one
SELECT
    COALESCE(SUM(a.cpu), 0)::double precision AS used_cpu,
    COALESCE(SUM(a.memory), 0)::bigint AS used_memory,
    COALESCE(SUM(a.cpu) FILTER (WHERE s.gpu_count = 0), 0)::double precision AS used_cpu_no_gpu,
    COALESCE(SUM(a.memory) FILTER (WHERE s.gpu_count = 0), 0)::bigint AS used_memory_no_gpu,
    (SELECT count(*)::bigint FROM gpus g WHERE g.node_id = $1) AS gpu_total
FROM allocations a
JOIN sandboxes s ON s.id = a.sandbox_id
WHERE a.node_id = $1;

-- CreateGPUAllocation claims one card. The primary key is the exclusivity
-- guarantee; a conflicting insert must roll the admission back, not place a
-- Sandbox with fewer cards than it asked for.
-- name: CreateGPUAllocation :exec
INSERT INTO gpu_allocations (gpu_uuid, sandbox_id) VALUES ($1, $2);

-- name: ListSandboxGPUs :many
SELECT gpu_uuid FROM gpu_allocations WHERE sandbox_id = $1 ORDER BY gpu_uuid;

-- name: ListAllNodes :many
SELECT * FROM nodes ORDER BY id;

-- Execs

-- name: CreateExec :one
INSERT INTO execs (
    id, sandbox_id, argv, env, cwd, stdin, timeout_seconds, generation, idempotency_key, pty
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING *;

-- name: GetExec :one
SELECT * FROM execs WHERE id = $1;

-- name: GetExecByIdempotencyKey :one
SELECT * FROM execs WHERE sandbox_id = $1 AND idempotency_key = $2;

-- name: GetExecWithNode :one
SELECT sqlc.embed(e), s.node_id AS sandbox_node_id
FROM execs e
JOIN sandboxes s ON s.id = e.sandbox_id
WHERE e.id = $1;

-- name: ListExecs :many
SELECT * FROM execs
WHERE sandbox_id = $1
  AND (sqlc.narg('after')::uuid IS NULL OR id > sqlc.narg('after'))
ORDER BY id
LIMIT sqlc.arg('limit');

-- name: CancelExec :one
UPDATE execs
SET cancel_requested_at = COALESCE(cancel_requested_at, now())
WHERE id = $1 AND finished_at IS NULL
RETURNING *;

-- DeleteSandbox and stop both cancel first; the Reconciler waits for the
-- resulting exit before removing the Runtime (api.md §3).
-- name: CancelSandboxExecs :execrows
UPDATE execs
SET cancel_requested_at = now()
WHERE sandbox_id = $1 AND finished_at IS NULL AND cancel_requested_at IS NULL;

-- name: CountActiveExecs :one
SELECT count(*) FROM execs WHERE sandbox_id = $1 AND finished_at IS NULL;

-- CountRunningExecs is the Idle Stop view: a pending Exec has not started, so
-- it is not "a running Exec" (ADR-0015). Creating it already reset the clock.
-- name: CountRunningExecs :one
SELECT count(*) FROM execs
WHERE sandbox_id = $1 AND finished_at IS NULL AND started_at IS NOT NULL;

-- name: SetExecStarted :exec
UPDATE execs
SET started_at = COALESCE(started_at, $2),
    deadline_at = CASE
        WHEN timeout_seconds IS NULL THEN NULL
        ELSE COALESCE(started_at, $2) + make_interval(secs => timeout_seconds)
    END
WHERE id = $1;

-- name: AppendExecChunk :exec
INSERT INTO exec_log_chunks (exec_id, first_seq, last_seq, frames)
VALUES ($1, $2, $3, $4)
ON CONFLICT (exec_id, first_seq) DO NOTHING;

-- name: AdvanceExecStoredSeq :exec
UPDATE execs
SET stored_seq = GREATEST(stored_seq, $2),
    log_truncated = log_truncated OR $3
WHERE id = $1;

-- name: FinishExec :execrows
UPDATE execs
SET finished_at = now(), cause = $2, exit_code = $3, signal = $4
WHERE id = $1 AND finished_at IS NULL;

-- name: FinishSandboxExecs :execrows
UPDATE execs
SET finished_at = now(), cause = $2
WHERE sandbox_id = $1 AND finished_at IS NULL AND generation <= $3;

-- name: ListExecOutputChunks :many
SELECT * FROM exec_log_chunks
WHERE exec_id = $1 AND last_seq > sqlc.arg('after')
ORDER BY first_seq
LIMIT 32;

-- name: ListActiveExecs :many
SELECT sqlc.embed(e), s.node_id AS sandbox_node_id,
       s.state AS sandbox_state, s.desired_state AS sandbox_desired_state
FROM execs e
JOIN sandboxes s ON s.id = e.sandbox_id
WHERE e.finished_at IS NULL
ORDER BY e.id;

-- Execution Log retention is counted from the Exec's terminal state
-- (defaults.md); the Exec row itself is not an expiring row.
-- name: PurgeExecutionLogs :exec
DELETE FROM exec_log_chunks
WHERE ctid IN (
    SELECT c.ctid
    FROM exec_log_chunks c
    JOIN execs e ON e.id = c.exec_id
    WHERE e.finished_at IS NOT NULL
      AND e.finished_at < now() - sqlc.arg('retention_seconds')::bigint * interval '1 second'
    LIMIT 10000
);

-- Secrets

-- name: GetSecretByName :one
SELECT * FROM secrets WHERE owner_id = $1 AND name = $2;

-- name: CountSecrets :one
SELECT count(*)::bigint FROM secrets WHERE owner_id = $1;

-- UpsertSecret overwrites by (owner_id, name) while keeping the row's id
-- (api.md §3, "创建或覆盖").
-- name: UpsertSecret :one
INSERT INTO secrets (id, owner_id, name, ciphertext, key_id, size)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (owner_id, name) DO UPDATE
SET ciphertext = EXCLUDED.ciphertext,
    key_id = EXCLUDED.key_id,
    size = EXCLUDED.size,
    updated_at = now()
RETURNING *;

-- name: ListSecrets :many
SELECT * FROM secrets
WHERE owner_id = $1
  AND (sqlc.narg('after')::text IS NULL OR name > sqlc.narg('after'))
ORDER BY name
LIMIT sqlc.arg('limit');

-- name: DeleteSecret :one
DELETE FROM secrets WHERE owner_id = $1 AND name = $2 RETURNING *;
