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
