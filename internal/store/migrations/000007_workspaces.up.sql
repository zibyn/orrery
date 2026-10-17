-- M1 ticket #59: persistent Workspaces, Workspace Attachments and Idle Stop.

-- The Attachment row is released on stop, so the Sandbox remembers which
-- Workspace and mode it mounts across stops (ADR-0018).
ALTER TABLE sandboxes
    ADD COLUMN workspace_id uuid REFERENCES workspaces (id) ON DELETE RESTRICT,
    ADD COLUMN workspace_mode text NOT NULL DEFAULT 'rw'
        CHECK (workspace_mode IN ('rw', 'ro'));

UPDATE sandboxes s
SET workspace_id = w.id
FROM workspaces w
WHERE w.sandbox_id = s.id;

-- Every creatable resource carries its Idempotency Key (schema.md).
ALTER TABLE workspaces ADD COLUMN idempotency_key text;

CREATE UNIQUE INDEX workspaces_owner_idempotency
    ON workspaces (owner_id, idempotency_key) WHERE idempotency_key IS NOT NULL;
