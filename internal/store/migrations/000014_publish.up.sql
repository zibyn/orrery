-- M4 ticket #87: Workspace publish. A publish creates a pending
-- ResourceVersion whose provenance is workspace{id, path}; the identity used
-- for Idempotency-Key dedupe is (Workspace, key) (api.md §1: the key is
-- scoped by the endpoint, and this endpoint's path parameter is the
-- Workspace). The existing resource_versions_resource_idempotency index
-- belongs to POST /resources/{id}/versions, so publish uses its own nullable
-- pair rather than colliding with an upload keyed by the same string.
ALTER TABLE resource_versions
    ADD COLUMN publish_workspace_id uuid REFERENCES workspaces (id) ON DELETE RESTRICT,
    ADD COLUMN publish_idempotency_key text;

CREATE UNIQUE INDEX resource_versions_publish_idempotency
    ON resource_versions (publish_workspace_id, publish_idempotency_key)
    WHERE publish_idempotency_key IS NOT NULL;

-- The Reconciler derives a command for every pending publish on each tick.
CREATE INDEX resource_versions_pending_publish
    ON resource_versions (publish_workspace_id)
    WHERE state = 'pending' AND publish_workspace_id IS NOT NULL;
