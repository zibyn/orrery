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

-- name: GetUserByOIDCIdentity :one
SELECT * FROM users
WHERE oidc_issuer = $1 AND oidc_subject = $2 AND deleted_at IS NULL;

-- name: GetUserByEmail :one
SELECT * FROM users WHERE email = $1 AND deleted_at IS NULL;

-- name: BindUserOIDCIdentity :one
UPDATE users
SET oidc_issuer = $2, oidc_subject = $3, updated_at = now()
WHERE id = $1 AND deleted_at IS NULL
RETURNING *;

-- CreateOIDCUser creates an OIDC User; the first User in the table is the
-- admin, even when allow_signups is off (ADR-0020).
-- name: CreateOIDCUser :one
INSERT INTO users (
    id, name, email, is_admin, login_type, oidc_issuer, oidc_subject,
    gpu_quota, sandbox_quota
) VALUES (
    $1, $2, $3,
    NOT EXISTS (SELECT 1 FROM users),
    'oidc', $4, $5, $6, $7
)
RETURNING *;

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
SELECT * FROM nodes WHERE id = $1 AND deleted_at IS NULL;

-- name: GetNodeByName :one
SELECT * FROM nodes WHERE name = $1;

-- name: ListNodes :many
SELECT * FROM nodes
WHERE deleted_at IS NULL
  AND (sqlc.narg('after')::uuid IS NULL OR id > sqlc.narg('after'))
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
    id, owner_id, name, labels, runtime, desired_state, state, snapshot_digest,
    cpu, memory, gpu_count, gpu_model, idle_stop_seconds, generation,
    idempotency_key, secret_env_ct, token_ct, token_key_id
) VALUES ($1, $2, $3, $4, $5, 'running', 'pending', $6, $7, $8, $9, $10, $11, 1, $12, $13, $14, $15)
RETURNING *;

-- CreateBuildSandbox is the internal row that carries a Snapshot build's
-- Allocation and Exec (schema.md, ADR-0021). It has no Sandbox Token, no
-- Workspace and no Runtime; secret_env_ct holds the build's Secret snapshot.
-- name: CreateBuildSandbox :one
INSERT INTO sandboxes (
    id, owner_id, kind, name, labels, desired_state, state, snapshot_digest,
    cpu, memory, gpu_count, idle_stop_seconds, generation, secret_env_ct
) VALUES ($1, $2, 'build', $3, '{}', 'running', 'pending', $4, $5, $6, 0, 0, 1, $7)
RETURNING *;

-- MarkBuildSandboxDone takes a build row out of the admission and reconcile
-- sweeps once its version is terminal. It has no Runtime to stop.
-- name: MarkBuildSandboxDone :execrows
UPDATE sandboxes
SET state = 'stopped', desired_state = 'stopped', updated_at = now()
WHERE id = $1 AND kind = 'build' AND state <> 'stopped';

-- name: GetSandbox :one
SELECT * FROM sandboxes WHERE id = $1 AND kind = 'user' AND state <> 'deleted';

-- GetSandboxIncludingDeleted does not filter kind: a build Exec's log is
-- readable through its owner, and the build row carries that ownership.
-- name: GetSandboxIncludingDeleted :one
SELECT * FROM sandboxes WHERE id = $1;

-- name: GetSandboxByIdempotencyKey :one
SELECT * FROM sandboxes
WHERE owner_id = $1 AND kind = 'user' AND idempotency_key = $2 AND state <> 'deleted';

-- name: ListSandboxes :many
SELECT * FROM sandboxes
WHERE owner_id = sqlc.arg('owner_id')
  AND kind = 'user'
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
-- the quota hold (ADR-0015). Builds are system work and never count (ADR-0021).
-- name: CountActiveSandboxResources :one
SELECT count(*)::bigint AS sandboxes,
       COALESCE(SUM(gpu_count), 0)::bigint AS gpus
FROM sandboxes
WHERE owner_id = $1 AND kind = 'user'
  AND state IN ('pending', 'provisioning', 'ready', 'stopping');

-- name: StartSandbox :one
UPDATE sandboxes
SET desired_state = 'running', state = 'pending', cause = NULL,
    generation = generation + 1, updated_at = now()
WHERE id = $1 AND state = 'stopped'
RETURNING *;

-- SetSandboxSecretEnv replaces the start-time Secret snapshot (schema.md).
-- name: SetSandboxSecretEnv :exec
UPDATE sandboxes SET secret_env_ct = $2 WHERE id = $1;

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
WHERE kind = 'user' AND state = 'ready' AND desired_state = 'running' AND idle_stop_seconds > 0
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
WHERE kind = 'user'
  AND (state IN ('pending', 'provisioning', 'ready', 'stopping', 'deleting')
       -- A failed Sandbox can still hold a Runtime (workspace_lost); the stop
       -- command runs until the Node observes it gone and the compute is
       -- released (ADR-0017).
       OR (state = 'failed'
           AND EXISTS (SELECT 1 FROM allocations a WHERE a.sandbox_id = sandboxes.id)))
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
SET state = 'ready', cause = NULL, conditions = NULL, last_activity_at = now(), updated_at = now()
WHERE id = $1 AND generation = $2 AND desired_state = 'running'
  AND state IN ('pending', 'provisioning');

-- name: MarkSandboxFailed :execrows
UPDATE sandboxes
SET state = 'failed', cause = $3, conditions = NULL, desired_state = 'stopped', updated_at = now()
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
SET state = 'stopped', conditions = NULL, updated_at = now()
WHERE id = $1 AND generation = $2 AND desired_state = 'stopped'
  AND state IN ('pending', 'provisioning', 'ready', 'stopping');

-- Sandbox Resource mounts: versions pinned at create time. The Reconciler
-- sends the address on every create, so a restarted Sandbox mounts what it was
-- created with (runtime.md §11).
-- name: CreateSandboxResource :exec
INSERT INTO sandbox_resources (sandbox_id, resource_version_id, mount)
VALUES ($1, $2, $3);

-- name: ListSandboxResourceMounts :many
SELECT sr.mount, rv.address
FROM sandbox_resources sr
JOIN resource_versions rv ON rv.id = sr.resource_version_id
WHERE sr.sandbox_id = $1
ORDER BY sr.mount;

-- name: CountSandboxResources :one
SELECT count(*)::bigint FROM sandbox_resources WHERE sandbox_id = $1;

-- SetSandboxConditions writes the Agent-reported condition set under the
-- generation fence.
-- name: SetSandboxConditions :execrows
UPDATE sandboxes
SET conditions = $3, updated_at = now()
WHERE id = $1 AND generation = $2 AND state <> 'deleted';

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
SELECT * FROM nodes WHERE deleted_at IS NULL ORDER BY id;

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
SELECT sqlc.embed(e), s.node_id AS sandbox_node_id, s.owner_id AS sandbox_owner_id,
       s.secret_env_ct AS sandbox_secret_env_ct,
       s.token_ct AS sandbox_token_ct, s.token_key_id AS sandbox_token_key_id,
       s.state AS sandbox_state, s.desired_state AS sandbox_desired_state
FROM execs e
JOIN sandboxes s ON s.id = e.sandbox_id
WHERE e.finished_at IS NULL AND s.kind = 'user'
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

-- Git account links

-- name: UpsertGitLink :one
INSERT INTO git_links (user_id, provider_id, access_token_ct, refresh_token_ct, key_id, expires_at)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (user_id, provider_id) DO UPDATE
SET access_token_ct = EXCLUDED.access_token_ct,
    refresh_token_ct = EXCLUDED.refresh_token_ct,
    key_id = EXCLUDED.key_id,
    expires_at = EXCLUDED.expires_at,
    updated_at = now()
RETURNING *;

-- name: ListGitLinks :many
SELECT * FROM git_links WHERE user_id = $1 ORDER BY provider_id;

-- name: GetGitLink :one
SELECT * FROM git_links WHERE user_id = $1 AND provider_id = $2;

-- name: GetGitLinkForUpdate :one
SELECT * FROM git_links WHERE user_id = $1 AND provider_id = $2 FOR UPDATE;

-- name: DeleteGitLink :one
DELETE FROM git_links WHERE user_id = $1 AND provider_id = $2 RETURNING *;

-- Resources

-- name: CreateResource :one
INSERT INTO resources (id, owner_id, name, idempotency_key)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- GetResource includes tombstones so DELETE can be idempotent; reads hide
-- them in the handler (api.md §1).
-- name: GetResource :one
SELECT * FROM resources WHERE id = $1;

-- name: GetResourceByIdempotencyKey :one
SELECT * FROM resources
WHERE owner_id = $1 AND idempotency_key = $2 AND deleted_at IS NULL;

-- name: ListResources :many
SELECT * FROM resources
WHERE owner_id = sqlc.arg('owner_id')
  AND deleted_at IS NULL
  AND (sqlc.narg('after')::uuid IS NULL OR id > sqlc.narg('after'))
ORDER BY id
LIMIT sqlc.arg('limit');

-- LockResource serializes current-pointer moves of one Resource (ADR-0012).
-- name: LockResource :one
SELECT * FROM resources WHERE id = $1 AND deleted_at IS NULL FOR UPDATE;

-- name: SoftDeleteResource :one
UPDATE resources
SET deleted_at = now()
WHERE id = $1 AND deleted_at IS NULL
RETURNING *;

-- name: SetResourceCurrent :one
UPDATE resources
SET current_version_id = $2
WHERE id = $1
RETURNING *;

-- PurgeResourceIdempotencyKeys expires Idempotency Keys after 24 hours
-- (defaults.md).
-- name: PurgeResourceIdempotencyKeys :exec
UPDATE resources SET idempotency_key = NULL
WHERE idempotency_key IS NOT NULL AND created_at < now() - interval '24 hours';

-- Resource versions

-- name: CreateResourceVersion :one
INSERT INTO resource_versions (
    id, resource_id, address, manifest_key, size, provenance,
    state, idempotency_key
) VALUES ($1, $2, $3, $4, $5, $6, 'ready', $7)
RETURNING *;

-- CreatePendingResourceVersion is the Workspace publish path: the version
-- starts pending and the Agent fills in the manifest (node-protocol.md
-- "Workspace publish").
-- name: CreatePendingResourceVersion :one
INSERT INTO resource_versions (
    id, resource_id, provenance, state, publish_workspace_id,
    publish_idempotency_key
) VALUES ($1, $2, $3, 'pending', $4, $5)
RETURNING *;

-- name: GetResourceVersion :one
SELECT * FROM resource_versions WHERE id = $1;

-- name: GetResourceVersionByPublishIdempotencyKey :one
SELECT * FROM resource_versions
WHERE publish_workspace_id = $1 AND publish_idempotency_key = $2;

-- SetResourceVersionManifest records what SubmitManifest normalized. Only a
-- pending version can accept a manifest: a terminal one must not be revived.
-- name: SetResourceVersionManifest :one
UPDATE resource_versions
SET address = $2, manifest_key = $3, size = $4
WHERE id = $1 AND state = 'pending'
RETURNING *;

-- name: DeleteResourceVersionObjects :exec
DELETE FROM resource_version_objects WHERE version_id = $1;

-- MarkResourceVersionReady and MarkResourceVersionFailed are conditional on
-- pending so a redelivered command after a restart cannot overwrite a
-- terminal state.
-- name: MarkResourceVersionReady :execrows
UPDATE resource_versions
SET state = 'ready', cause = NULL
WHERE id = $1 AND state = 'pending';

-- name: MarkResourceVersionFailed :execrows
UPDATE resource_versions
SET state = 'failed', cause = $2
WHERE id = $1 AND state = 'pending';

-- ListPendingWorkspacePublishes is the Reconciler's desired-state query. The
-- path lives in provenance (schema.md).
-- name: ListPendingWorkspacePublishes :many
SELECT rv.id, rv.resource_id, rv.provenance, rv.publish_workspace_id, w.node_id
FROM resource_versions rv
JOIN workspaces w ON w.id = rv.publish_workspace_id
WHERE rv.state = 'pending' AND rv.publish_workspace_id IS NOT NULL
ORDER BY rv.id;

-- GetPendingWorkspacePublish authorizes SubmitManifest and CommandResult: the
-- version must still be pending and its Workspace bound to the calling Node.
-- name: GetPendingWorkspacePublish :one
SELECT rv.*, w.node_id, r.owner_id
FROM resource_versions rv
JOIN workspaces w ON w.id = rv.publish_workspace_id
JOIN resources r ON r.id = rv.resource_id
WHERE rv.id = $1
  AND rv.state = 'pending'
  AND rv.publish_workspace_id IS NOT NULL;

-- FindPublishObjectOwner answers the Presign PUT question: is sha256 one of
-- the objects of a pending publish assigned to this Node, and which owner's
-- CAS key addresses it (ADR-0021)?
-- name: FindPublishObjectOwner :one
SELECT r.owner_id
FROM resource_version_objects rvo
JOIN resource_versions rv ON rv.id = rvo.version_id
JOIN resources r ON r.id = rv.resource_id
JOIN workspaces w ON w.id = rv.publish_workspace_id
WHERE rv.state = 'pending'
  AND rv.publish_workspace_id IS NOT NULL
  AND w.node_id = $1
  AND rvo.sha256 = $2
LIMIT 1;

-- GetLiveResourceByName resolves a publish's Resource name without creating
-- it (api.md §3: a name is unique among live Resources of one owner).
-- name: GetLiveResourceByName :one
SELECT * FROM resources
WHERE owner_id = $1 AND name = $2 AND deleted_at IS NULL;

-- name: GetResourceVersionByIdempotencyKey :one
SELECT * FROM resource_versions WHERE resource_id = $1 AND idempotency_key = $2;

-- name: ListResourceVersions :many
SELECT * FROM resource_versions
WHERE resource_id = $1
  AND (sqlc.narg('after')::uuid IS NULL OR id < sqlc.narg('after'))
ORDER BY id DESC
LIMIT sqlc.arg('limit');

-- name: PurgeResourceVersionIdempotencyKeys :exec
UPDATE resource_versions
SET idempotency_key = NULL, publish_idempotency_key = NULL
WHERE (idempotency_key IS NOT NULL OR publish_idempotency_key IS NOT NULL)
  AND created_at < now() - interval '24 hours';

-- name: InsertResourceVersionObject :exec
INSERT INTO resource_version_objects (version_id, sha256)
VALUES ($1, $2)
ON CONFLICT (version_id, sha256) DO NOTHING;

-- name: ListResourceVersionObjects :many
SELECT sha256 FROM resource_version_objects WHERE version_id = $1 ORDER BY sha256;

-- A successful publish validation proves the uploaded objects are present, so
-- the owner's unavailable versions that reference them are re-checked and may
-- return to ready (api.md §3).
-- name: ListUnavailableVersionsReferencing :many
SELECT DISTINCT rv.id
FROM resource_versions rv
JOIN resources r ON r.id = rv.resource_id
JOIN resource_version_objects rvo ON rvo.version_id = rv.id
WHERE r.owner_id = sqlc.arg('owner_id')
  AND rv.state = 'unavailable'
  AND rvo.sha256 = ANY(sqlc.arg('sha256')::text[]);

-- MarkResourceVersionAvailable is conditional on unavailable so a concurrent
-- state change is not overwritten.
-- name: MarkResourceVersionAvailable :execrows
UPDATE resource_versions
SET state = 'ready'
WHERE id = $1 AND state = 'unavailable';

-- CAS objects (owner-scoped, ADR-0021)

-- name: UpsertCASObject :exec
INSERT INTO cas_objects (owner_id, sha256, size)
VALUES ($1, $2, $3)
ON CONFLICT (owner_id, sha256) DO NOTHING;

-- name: GetCASObject :one
SELECT * FROM cas_objects WHERE owner_id = $1 AND sha256 = $2;

-- name: DeleteCASObject :exec
DELETE FROM cas_objects WHERE owner_id = $1 AND sha256 = $2;

-- Upload sessions

-- name: CreateUpload :one
INSERT INTO uploads (id, resource_id, address, manifest, expires_at)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetUpload :one
SELECT * FROM uploads WHERE id = $1;

-- ReusableUpload is the resume rule: an unexpired session for the same
-- Resource and manifest address is reused (api.md §3).
-- name: ReusableUpload :one
SELECT * FROM uploads
WHERE resource_id = $1 AND address = $2 AND expires_at > now()
ORDER BY created_at DESC, id
LIMIT 1;

-- name: DeleteUpload :exec
DELETE FROM uploads WHERE id = $1;

-- Presign authorization (node-protocol.md "Presign")
--
-- Only objects and manifests of versions mounted by a Sandbox (including a
-- build row later) allocated to this Node may be signed for GET. CAS keys are
-- owner-scoped, so the caller needs the Resource's owner (ADR-0021).

-- name: ListNodeSandboxObjectRefs :many
SELECT DISTINCT r.owner_id, rv.id AS version_id
FROM sandbox_resources sr
JOIN sandboxes sb ON sb.id = sr.sandbox_id
JOIN resource_versions rv ON rv.id = sr.resource_version_id
JOIN resources r ON r.id = rv.resource_id
JOIN resource_version_objects rvo ON rvo.version_id = rv.id
WHERE sb.node_id = $1 AND sb.state <> 'deleted' AND rvo.sha256 = $2;

-- name: ListNodeSandboxManifestOwner :one
SELECT r.owner_id
FROM sandbox_resources sr
JOIN sandboxes sb ON sb.id = sr.sandbox_id
JOIN resource_versions rv ON rv.id = sr.resource_version_id
JOIN resources r ON r.id = rv.resource_id
WHERE sb.node_id = $1 AND sb.state <> 'deleted' AND rv.address = $2
LIMIT 1;

-- name: MarkResourceVersionUnavailable :execrows
UPDATE resource_versions
SET state = 'unavailable'
WHERE id = $1 AND state = 'ready';

-- M4 ticket #88: Snapshots and SnapshotVersions (schema.md, ADR-0012).

-- name: CreateSnapshot :one
INSERT INTO snapshots (id, owner_id, name)
VALUES (sqlc.arg('id'), sqlc.narg('owner_id'), sqlc.arg('name'))
RETURNING *;

-- name: GetSnapshot :one
SELECT * FROM snapshots WHERE id = $1 AND deleted_at IS NULL;

-- name: GetSnapshotByName :one
SELECT * FROM snapshots
WHERE owner_id = $1 AND name = $2 AND deleted_at IS NULL;

-- name: GetPlatformSnapshotByName :one
SELECT * FROM snapshots
WHERE owner_id IS NULL AND name = $1 AND deleted_at IS NULL;

-- name: ListSnapshots :many
SELECT * FROM snapshots
WHERE deleted_at IS NULL
  AND (owner_id = sqlc.arg('owner_id') OR owner_id IS NULL)
  AND (sqlc.narg('after')::uuid IS NULL OR id > sqlc.narg('after'))
ORDER BY id
LIMIT sqlc.arg('limit');

-- name: DeleteSnapshot :one
UPDATE snapshots SET deleted_at = now()
WHERE id = $1 AND owner_id = $2 AND deleted_at IS NULL
RETURNING *;

-- name: SetSnapshotCurrentVersion :one
UPDATE snapshots SET current_version_id = sqlc.arg('version_id')
WHERE id = sqlc.arg('id') AND deleted_at IS NULL
RETURNING *;

-- name: CreateSnapshotVersion :one
INSERT INTO snapshot_versions (
    id, snapshot_id, digest, state, cause, source,
    build_sandbox_id, build_exec_id, provenance, idempotency_key, build_request
) VALUES (
    sqlc.arg('id'), sqlc.arg('snapshot_id'), sqlc.narg('digest'), sqlc.arg('state'),
    sqlc.narg('cause'), sqlc.arg('source'), sqlc.narg('build_sandbox_id'),
    sqlc.narg('build_exec_id'), sqlc.narg('provenance'), sqlc.narg('idempotency_key'),
    sqlc.narg('build_request')
)
RETURNING *;

-- name: GetSnapshotVersion :one
SELECT * FROM snapshot_versions WHERE id = $1 AND snapshot_id = $2;

-- name: GetSnapshotVersionByIdempotencyKey :one
SELECT * FROM snapshot_versions
WHERE snapshot_id = $1 AND idempotency_key = $2;

-- name: ListSnapshotVersions :many
SELECT * FROM snapshot_versions
WHERE snapshot_id = sqlc.arg('snapshot_id')
  AND (sqlc.narg('after')::uuid IS NULL OR id > sqlc.narg('after'))
ORDER BY id
LIMIT sqlc.arg('limit');

-- name: MarkSnapshotVersionReady :one
UPDATE snapshot_versions
SET state = 'ready', digest = sqlc.arg('digest'), provenance = sqlc.narg('provenance')
WHERE id = sqlc.arg('id') AND state = 'building'
RETURNING *;

-- name: MarkSnapshotVersionFailed :one
UPDATE snapshot_versions SET state = 'failed', cause = sqlc.arg('cause')
WHERE id = sqlc.arg('id') AND state = 'building'
RETURNING *;

-- name: CancelSnapshotVersion :one
UPDATE snapshot_versions SET state = 'cancelled'
WHERE id = sqlc.arg('id') AND state = 'building'
RETURNING *;

-- name: DeleteSnapshotVersion :exec
DELETE FROM snapshot_versions WHERE id = $1 AND snapshot_id = $2;

-- An import runs in the Control Plane process, so a restart leaves only a
-- building row behind; mark it failed rather than letting it hang.
-- name: FailBuildingSnapshotImports :exec
UPDATE snapshot_versions SET state = 'failed', cause = 'import_failed'
WHERE state = 'building' AND source = 'import';

-- The suffix is "<repo>@<digest>"; matching the suffix avoids caring which
-- registry address a Sandbox was created against.
-- name: CountSandboxSnapshotReferences :one
SELECT count(*)::bigint FROM sandboxes
WHERE kind = 'user' AND state <> 'deleted'
  AND right(snapshot_digest, length(sqlc.arg('ref')::text)) = sqlc.arg('ref')::text;

-- name: GetSnapshotVersionByID :one
SELECT * FROM snapshot_versions WHERE id = $1;

-- ListBuildRuns is the build sweep's queue: every build version still tied to
-- a live build sandbox or Exec, terminal versions included, so cleanup is
-- retried until it succeeds (ADR-0014: desired state is the queue).
-- name: ListBuildRuns :many
SELECT sqlc.embed(v), sqlc.embed(s),
       e.id AS exec_id, e.accepted_at AS exec_accepted_at,
       e.started_at AS exec_started_at, e.deadline_at AS exec_deadline_at,
       e.cancel_requested_at AS exec_cancel_requested_at,
       e.finished_at AS exec_finished_at, e.timeout_seconds AS exec_timeout_seconds,
       e.generation AS exec_generation
FROM snapshot_versions v
JOIN sandboxes s ON s.id = v.build_sandbox_id
JOIN execs e ON e.id = v.build_exec_id
WHERE v.source = 'build' AND s.kind = 'build'
  -- A terminal version whose sandbox and Exec are already settled needs no
  -- more work.
  AND NOT (s.state = 'stopped' AND e.finished_at IS NOT NULL)
ORDER BY v.id;
-- M4 ticket #90: Retention, CAS sweep and tombstone cleanup (ADR-0012).

-- ListExpiredResourceVersions is the ResourceVersion TTL sweep. The handler
-- re-applies the eligibility predicate; the filters here keep the batch small.
-- name: ListExpiredResourceVersions :many
SELECT rv.id, rv.address, rv.manifest_key, rv.state, rv.created_at,
       (r.current_version_id IS NOT DISTINCT FROM rv.id) AS current, r.owner_id
FROM resource_versions rv
JOIN resources r ON r.id = rv.resource_id
WHERE r.deleted_at IS NULL
  AND rv.created_at < sqlc.arg('cutoff')
  AND rv.state <> 'pending'
  AND (r.current_version_id IS NULL OR r.current_version_id <> rv.id)
  AND NOT EXISTS (
    SELECT 1 FROM sandbox_resources sr
    JOIN sandboxes sb ON sb.id = sr.sandbox_id
    WHERE sr.resource_version_id = rv.id AND sb.state <> 'deleted'
  )
ORDER BY rv.created_at
LIMIT sqlc.arg('limit');

-- name: DeleteResourceVersion :exec
DELETE FROM resource_versions WHERE id = $1;

-- name: DeleteExpiredUploads :execrows
DELETE FROM uploads WHERE expires_at <= now();

-- ListSweepableCASObjects finds objects past the grace window that no version
-- of the same owner references and no unexpired upload will still write. The
-- owner join is what makes the sweep owner-scoped (ADR-0021).
-- name: ListSweepableCASObjects :many
SELECT c.owner_id, c.sha256, c.created_at
FROM cas_objects c
WHERE c.created_at < sqlc.arg('cutoff')
  AND NOT EXISTS (
    SELECT 1
    FROM resource_version_objects rvo
    JOIN resource_versions rv ON rv.id = rvo.version_id
    JOIN resources r ON r.id = rv.resource_id
    WHERE r.owner_id = c.owner_id AND rvo.sha256 = c.sha256
  )
  AND NOT EXISTS (
    SELECT 1
    FROM uploads u
    JOIN resources r ON r.id = u.resource_id
    WHERE r.owner_id = c.owner_id
      AND u.expires_at > now()
      AND EXISTS (
        SELECT 1 FROM jsonb_array_elements(u.manifest->'entries') e
        WHERE e->>'sha256' = c.sha256
      )
  )
ORDER BY c.created_at
LIMIT sqlc.arg('limit');

-- Tombstoned Resources keep their versions until no Sandbox references them,
-- then the version, its objects and the manifest go, and finally the row.
-- name: ListDeletedResources :many
SELECT * FROM resources
WHERE deleted_at IS NOT NULL
ORDER BY deleted_at
LIMIT $1;

-- name: ClearResourceCurrent :exec
UPDATE resources SET current_version_id = NULL
WHERE id = $1 AND deleted_at IS NOT NULL;

-- name: ListResourceVersionsForCleanup :many
SELECT rv.id, rv.address, rv.manifest_key, r.owner_id
FROM resource_versions rv
JOIN resources r ON r.id = rv.resource_id
WHERE rv.resource_id = $1
  AND rv.state <> 'pending'
  AND NOT EXISTS (
    SELECT 1 FROM sandbox_resources sr
    JOIN sandboxes sb ON sb.id = sr.sandbox_id
    WHERE sr.resource_version_id = rv.id AND sb.state <> 'deleted'
  )
ORDER BY rv.id
LIMIT $2;

-- name: CountResourceVersions :one
SELECT count(*)::bigint FROM resource_versions WHERE resource_id = $1;

-- name: CountResourceUploads :one
SELECT count(*)::bigint FROM uploads WHERE resource_id = $1;

-- name: DeleteDeletedResource :exec
DELETE FROM resources
WHERE id = $1 AND deleted_at IS NOT NULL AND current_version_id IS NULL;

-- Tombstoned Snapshots drop each version once the last Sandbox reference is
-- gone (registry manifest first), then the row.
-- name: ListDeletedSnapshots :many
SELECT * FROM snapshots
WHERE deleted_at IS NOT NULL
ORDER BY deleted_at
LIMIT $1;

-- name: ClearSnapshotCurrent :exec
UPDATE snapshots SET current_version_id = NULL
WHERE id = $1 AND deleted_at IS NOT NULL;

-- A Snapshot deleted mid-build must keep its building version: the build row
-- still owns the Sandbox and Exec that release its allocation. Only terminal
-- versions are tombstone garbage.
-- name: ListSnapshotVersionsForCleanup :many
SELECT * FROM snapshot_versions
WHERE snapshot_id = $1
  AND state <> 'building'
ORDER BY id
LIMIT $2;

-- name: CountSnapshotVersions :one
SELECT count(*)::bigint FROM snapshot_versions WHERE snapshot_id = $1;

-- name: DeleteDeletedSnapshot :exec
DELETE FROM snapshots
WHERE id = $1 AND deleted_at IS NOT NULL AND current_version_id IS NULL;

-- Manifest objects are content-addressed, so two versions (or a current and a
-- non-current one) can share one manifest. The sweep only deletes it when no
-- other version of the owner still names the address.
-- name: CountResourceManifestReferences :one
SELECT count(*)::bigint
FROM resource_versions rv
JOIN resources r ON r.id = rv.resource_id
WHERE r.owner_id = $1 AND rv.address = $2 AND rv.id <> $3;

-- Snapshot digests are unique content too; deleting a version must not remove
-- a manifest another version of the same Snapshot still uses.
-- name: CountSnapshotVersionDigestReferences :one
SELECT count(*)::bigint FROM snapshot_versions
WHERE snapshot_id = $1 AND digest = $2 AND id <> $3;

-- A deleted Sandbox no longer retains versions, but its sandbox_resources
-- rows outlive it (the row is kept for API idempotency). The sweep releases
-- those references before the version/FK checks run.
-- name: DeleteDeletedSandboxResources :execrows
DELETE FROM sandbox_resources sr
USING sandboxes sb
WHERE sb.id = sr.sandbox_id AND sb.state = 'deleted';

-- Node pause/resume/delete (#94)

-- A tombstoned Node is hidden everywhere GetNode is used, so a second delete
-- or an Agent reconnect after deletion is not found.
-- name: PauseNode :one
UPDATE nodes SET paused = true WHERE id = $1 AND deleted_at IS NULL RETURNING *;

-- name: ResumeNode :one
UPDATE nodes SET paused = false WHERE id = $1 AND deleted_at IS NULL RETURNING *;

-- Deleting a Node clears its token (bytea NOT NULL, so an empty value, which
-- no token hash matches) and keeps the row as a tombstone (ADR-0014).
-- name: TombstoneNode :execrows
UPDATE nodes SET deleted_at = now(), token_hash = ''
WHERE id = $1 AND deleted_at IS NULL;

-- Deletion requires no non-terminal Workspace (deleted, failed and lost are
-- terminal) and no non-terminal Sandbox, build rows included (only deleted and
-- failed are terminal; a stopped Sandbox still blocks) (api.md §3).
-- name: CountNodeNonTerminalWorkspaces :one
SELECT count(*)::bigint FROM workspaces
WHERE node_id = $1 AND state NOT IN ('deleted', 'failed', 'lost');

-- name: CountNodeNonTerminalSandboxes :one
SELECT count(*)::bigint FROM sandboxes
WHERE node_id = $1 AND state NOT IN ('deleted', 'failed');

-- Lost cascades (#95)

-- Declaring a Node lost is irreversible: the Token is cleared, the Node is
-- paused, and lost_at is what deriveNodeStatus reads (ADR-0014).
-- name: MarkNodeLost :execrows
UPDATE nodes
SET token_hash = '', paused = true, lost_at = now()
WHERE id = $1;

-- Every non-deleted Sandbox on the Node, including stopped and bound pending
-- ones and build rows, becomes failed/node_lost (api.md §3).
-- name: FailSandboxesOnNodeLost :execrows
UPDATE sandboxes
SET state = 'failed', cause = 'node_lost', desired_state = 'stopped',
    conditions = NULL, updated_at = now()
WHERE node_id = $1 AND state <> 'deleted';

-- name: DeleteSandboxAttachmentsOnNodeLost :execrows
DELETE FROM attachments
WHERE sandbox_id IN (
    SELECT s.id FROM sandboxes s WHERE s.node_id = $1 AND s.state <> 'deleted'
);

-- name: DeleteGPUAllocationsOnNodeLost :execrows
DELETE FROM gpu_allocations
WHERE sandbox_id IN (
    SELECT s.id FROM sandboxes s WHERE s.node_id = $1 AND s.state <> 'deleted'
);

-- name: DeleteAllocationsOnNodeLost :execrows
DELETE FROM allocations
WHERE sandbox_id IN (
    SELECT s.id FROM sandboxes s WHERE s.node_id = $1 AND s.state <> 'deleted'
);

-- name: FinishExecsOnNodeLost :execrows
UPDATE execs
SET finished_at = now(), cause = 'node_lost'
WHERE finished_at IS NULL
  AND sandbox_id IN (
      SELECT s.id FROM sandboxes s WHERE s.node_id = $1 AND s.state <> 'deleted'
  );

-- A Workspace being deleted on a lost Node reaches deleted without a command;
-- every other non-terminal one becomes lost (ADR-0014).
-- name: MarkDeletingWorkspacesDeletedOnNodeLost :execrows
UPDATE workspaces
SET state = 'deleted', deleted_at = now(), updated_at = now()
WHERE node_id = $1 AND state = 'deleting';

-- name: MarkWorkspacesLostOnNodeLost :execrows
UPDATE workspaces
SET state = 'lost', updated_at = now()
WHERE node_id = $1 AND state IN ('provisioning', 'ready');

-- name: FailBuildsOnNodeLost :execrows
UPDATE snapshot_versions
SET state = 'failed', cause = 'node_lost'
WHERE state = 'building'
  AND build_sandbox_id IN (SELECT id FROM sandboxes WHERE node_id = $1);

-- name: FailPublishesOnNodeLost :execrows
UPDATE resource_versions
SET state = 'failed', cause = 'node_lost'
WHERE state = 'pending'
  AND publish_workspace_id IN (SELECT id FROM workspaces WHERE node_id = $1);

-- A Workspace declared lost on a live Node: its attached Sandboxes become
-- failed/workspace_lost and their Execs end runtime_lost (ADR-0017). A row
-- already deleting keeps its own teardown.
-- name: MarkWorkspaceLost :execrows
UPDATE workspaces
SET state = 'lost', updated_at = now()
WHERE id = $1 AND state IN ('provisioning', 'ready');

-- name: FailSandboxesOnWorkspaceLost :execrows
UPDATE sandboxes
SET state = 'failed', cause = 'workspace_lost', desired_state = 'stopped',
    conditions = NULL, updated_at = now()
WHERE workspace_id = $1 AND state NOT IN ('deleted', 'deleting');

-- name: FinishExecsOnWorkspaceLost :execrows
UPDATE execs
SET finished_at = now(), cause = 'runtime_lost'
WHERE finished_at IS NULL
  AND sandbox_id IN (
      SELECT id FROM sandboxes
      WHERE workspace_id = $1 AND state = 'failed' AND cause = 'workspace_lost'
  );

-- Abandoning a Workspace stuck in deleting reaches deleted with no Node
-- command (api.md §3).
-- name: MarkWorkspaceAbandoned :execrows
UPDATE workspaces
SET state = 'deleted', deleted_at = now(), updated_at = now()
WHERE id = $1 AND state = 'deleting';
