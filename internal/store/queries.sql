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

-- name: PurgeUserIdempotencyKeys :exec
UPDATE users SET idempotency_key = NULL
WHERE idempotency_key IS NOT NULL AND created_at < now() - interval '24 hours';

-- name: PurgeTokenIdempotencyKeys :exec
UPDATE api_tokens SET idempotency_key = NULL
WHERE idempotency_key IS NOT NULL AND created_at < now() - interval '24 hours';

-- name: PurgeSandboxIdempotencyKeys :exec
UPDATE sandboxes SET idempotency_key = NULL
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
    reserved_memory = $5
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

-- name: PurgeEnrollmentPackets :exec
DELETE FROM enrollment_packets
WHERE used_at IS NOT NULL OR expires_at < now();

-- Sandboxes

-- name: CreateSandbox :one
INSERT INTO sandboxes (
    id, owner_id, name, labels, desired_state, state, snapshot_digest,
    cpu, memory, gpu_count, idle_stop_seconds, generation, idempotency_key
) VALUES ($1, $2, $3, $4, 'running', 'pending', $5, $6, $7, $8, $9, 1, $10)
RETURNING *;

-- name: GetSandbox :one
SELECT * FROM sandboxes WHERE id = $1 AND state <> 'deleted';

-- name: GetSandboxByIdempotencyKey :one
SELECT * FROM sandboxes
WHERE owner_id = $1 AND idempotency_key = $2 AND state <> 'deleted';

-- name: ListSandboxes :many
SELECT * FROM sandboxes
WHERE owner_id = sqlc.arg('owner_id')
  AND state <> 'deleted'
  AND (sqlc.narg('state')::text IS NULL OR state = sqlc.narg('state'))
  AND (sqlc.narg('node_id')::uuid IS NULL OR node_id = sqlc.narg('node_id'))
  AND (sqlc.narg('label_key')::text IS NULL
       OR (jsonb_exists(labels, sqlc.narg('label_key')::text)
           AND labels ->> sqlc.narg('label_key')::text = sqlc.narg('label_value')::text))
  AND (sqlc.narg('after')::uuid IS NULL OR id > sqlc.narg('after'))
ORDER BY id
LIMIT sqlc.arg('limit');

-- name: CountActiveSandboxes :one
SELECT count(*) FROM sandboxes
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
SET state = 'ready', cause = NULL, updated_at = now()
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

-- name: MarkSandboxStopped :execrows
UPDATE sandboxes
SET state = 'stopped', updated_at = now()
WHERE id = $1 AND generation = $2 AND desired_state = 'stopped'
  AND state IN ('pending', 'provisioning', 'ready', 'stopping');

-- Workspaces

-- name: CreateWorkspace :one
INSERT INTO workspaces (id, owner_id, kind, sandbox_id, state, storage_handle, generation)
VALUES ($1, $2, $3, $4, 'provisioning', $5, 1)
RETURNING *;

-- name: GetWorkspace :one
SELECT * FROM workspaces WHERE id = $1;

-- name: GetSandboxWorkspace :one
SELECT * FROM workspaces
WHERE sandbox_id = $1 AND state <> 'deleted';

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

-- Allocations

-- name: CreateAllocation :exec
INSERT INTO allocations (sandbox_id, node_id, cpu, memory, disk, generation)
VALUES ($1, $2, $3, $4, 0, $5)
ON CONFLICT (sandbox_id) DO NOTHING;

-- name: DeleteAllocation :exec
DELETE FROM allocations WHERE sandbox_id = $1;

-- name: NodeAllocated :one
SELECT COALESCE(SUM(cpu), 0)::double precision AS cpu,
       COALESCE(SUM(memory), 0)::bigint AS memory
FROM allocations WHERE node_id = $1;

-- name: ListAllNodes :many
SELECT * FROM nodes ORDER BY id;
