-- M1 ticket #47: only the tables this ticket uses. Later tickets add theirs.
CREATE TABLE users (
    id              uuid PRIMARY KEY,
    name            text NOT NULL UNIQUE,
    email           text,
    is_admin        boolean NOT NULL DEFAULT false,
    login_type      text NOT NULL CHECK (login_type IN ('password', 'oidc', 'none')),
    password_hash   text,
    oidc_issuer     text,
    oidc_subject    text,
    gpu_quota       integer NOT NULL DEFAULT 0 CHECK (gpu_quota >= 0),
    sandbox_quota   integer NOT NULL DEFAULT 0 CHECK (sandbox_quota >= 0),
    idempotency_key text,
    created_by      uuid REFERENCES users (id) ON DELETE RESTRICT,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    deleted_at      timestamptz,
    CONSTRAINT users_password_hash_only_for_password
        CHECK (password_hash IS NULL OR login_type = 'password'),
    CONSTRAINT users_oidc_unique UNIQUE (oidc_issuer, oidc_subject)
);

CREATE UNIQUE INDEX users_email_unique ON users (email) WHERE email IS NOT NULL;
-- Idempotency is keyed by (actor, key); bootstrap has no actor.
CREATE UNIQUE INDEX users_idempotency_key_unique
    ON users (created_by, idempotency_key) WHERE idempotency_key IS NOT NULL;

CREATE TABLE api_tokens (
    id              uuid PRIMARY KEY,
    user_id         uuid NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    kind            text NOT NULL CHECK (kind IN ('token', 'session')),
    name            text NOT NULL,
    secret_hash     bytea NOT NULL,
    scope           text NOT NULL CHECK (scope IN ('read_only', 'user', 'admin')),
    expires_at      timestamptz,
    last_used_at    timestamptz,
    idempotency_key text,
    created_at      timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX api_tokens_name_unique
    ON api_tokens (user_id, name) WHERE kind = 'token';
CREATE UNIQUE INDEX api_tokens_idempotency_key_unique
    ON api_tokens (user_id, idempotency_key) WHERE idempotency_key IS NOT NULL;
CREATE INDEX api_tokens_user_id ON api_tokens (user_id);

CREATE TABLE audit_records (
    id              bigserial PRIMARY KEY,
    at              timestamptz NOT NULL DEFAULT now(),
    -- Deliberately not foreign keys: revoking a token deletes its row, and an
    -- Audit Record must survive the thing it names (ADR-0016).
    actor_user_id   uuid,
    actor_token_id  uuid,
    action          text NOT NULL,
    target_type     text NOT NULL,
    target_id       text NOT NULL,
    correlation_id  text,
    detail          jsonb NOT NULL DEFAULT '{}'::jsonb
);

CREATE INDEX audit_records_at_id ON audit_records (at DESC, id DESC);
CREATE INDEX audit_records_actor ON audit_records (actor_user_id);
CREATE INDEX audit_records_action ON audit_records (action);
CREATE INDEX audit_records_target ON audit_records (target_type, target_id);
