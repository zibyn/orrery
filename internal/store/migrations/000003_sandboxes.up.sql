-- M1 ticket #50: GPU-less Sandbox lifecycle (ADR-0017, ADR-0018).

ALTER TABLE nodes
    ADD COLUMN reserved_cpu double precision NOT NULL DEFAULT 0,
    ADD COLUMN reserved_memory bigint NOT NULL DEFAULT 0;

CREATE TABLE sandboxes (
    id                uuid PRIMARY KEY,
    owner_id          uuid NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    name              text NOT NULL,
    labels            jsonb NOT NULL DEFAULT '{}',
    desired_state     text NOT NULL DEFAULT 'running' CHECK (desired_state IN ('running', 'stopped', 'deleted')),
    state             text NOT NULL DEFAULT 'pending' CHECK (state IN (
                          'pending', 'provisioning', 'ready', 'stopping',
                          'stopped', 'deleting', 'deleted', 'failed')),
    cause             text,
    node_id           uuid REFERENCES nodes (id) ON DELETE RESTRICT,
    snapshot_digest   text NOT NULL,
    cpu               double precision NOT NULL CHECK (cpu > 0),
    memory            bigint NOT NULL CHECK (memory > 0),
    gpu_count         integer NOT NULL DEFAULT 0 CHECK (gpu_count >= 0),
    idle_stop_seconds integer NOT NULL DEFAULT 900 CHECK (idle_stop_seconds >= 0),
    last_activity_at  timestamptz,
    token_hash        bytea,
    secret_env_ct     bytea,
    generation        bigint NOT NULL DEFAULT 1,
    idempotency_key   text,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    deleted_at        timestamptz
);

CREATE UNIQUE INDEX sandboxes_owner_name ON sandboxes (owner_id, name) WHERE deleted_at IS NULL;
CREATE UNIQUE INDEX sandboxes_owner_idempotency ON sandboxes (owner_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;
CREATE INDEX sandboxes_node_id ON sandboxes (node_id);

CREATE TABLE workspaces (
    id             uuid PRIMARY KEY,
    owner_id       uuid NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    kind           text NOT NULL CHECK (kind IN ('ephemeral', 'persistent')),
    sandbox_id     uuid REFERENCES sandboxes (id) ON DELETE RESTRICT,
    node_id        uuid REFERENCES nodes (id) ON DELETE RESTRICT,
    state          text NOT NULL CHECK (state IN ('provisioning', 'ready', 'deleting', 'deleted', 'failed', 'lost')),
    cause          text,
    storage_handle text,
    conditions     jsonb,
    generation     bigint NOT NULL DEFAULT 1,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    deleted_at     timestamptz
);

CREATE UNIQUE INDEX workspaces_sandbox_id ON workspaces (sandbox_id) WHERE sandbox_id IS NOT NULL;
CREATE INDEX workspaces_node_id ON workspaces (node_id);

CREATE TABLE attachments (
    sandbox_id uuid PRIMARY KEY REFERENCES sandboxes (id) ON DELETE RESTRICT,
    workspace_id uuid NOT NULL REFERENCES workspaces (id) ON DELETE RESTRICT,
    mode       text NOT NULL CHECK (mode IN ('rw', 'ro')),
    generation bigint NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- Single writer: at most one rw Attachment per Workspace.
CREATE UNIQUE INDEX attachments_workspace_rw ON attachments (workspace_id) WHERE mode = 'rw';

CREATE TABLE allocations (
    sandbox_id uuid PRIMARY KEY REFERENCES sandboxes (id) ON DELETE RESTRICT,
    node_id    uuid NOT NULL REFERENCES nodes (id) ON DELETE RESTRICT,
    cpu        double precision NOT NULL,
    memory     bigint NOT NULL,
    disk       bigint NOT NULL DEFAULT 0,
    generation bigint NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX allocations_node_id ON allocations (node_id);
