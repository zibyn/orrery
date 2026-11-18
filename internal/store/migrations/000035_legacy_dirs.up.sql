-- #279, ADR-0031 "Migration": Workspaces go. Their Node-local directories
-- (<data-root>/workspaces/<id> on node_id) become what replaces them:
--   * an ephemeral one, its Sandbox's /workspace;
--   * a persistent one, a Volume with its ID, mounted at /workspace by the
--     Sandbox that mounted it read-write;
--   * a read-only mount, nothing.
-- legacy_dirs lists the directories still to be uploaded. Their Node merges
-- them at the next Create there, so the row pins the Sandbox (or the
-- Sandboxes mounting the Volume) to that Node until then. A directory on a
-- lost or deleted Node is lost, as it was before.

CREATE TABLE legacy_dirs (
    id         uuid PRIMARY KEY,
    node_id    uuid NOT NULL REFERENCES nodes (id) ON DELETE CASCADE,
    sandbox_id uuid REFERENCES sandboxes (id) ON DELETE CASCADE,
    volume_id  uuid REFERENCES volumes (id) ON DELETE CASCADE,
    -- Set when the Sandbox is admitted after the upgrade: its next commit
    -- then holds the directory and the row goes.
    started    boolean NOT NULL DEFAULT false,
    CONSTRAINT legacy_dirs_one_target CHECK ((sandbox_id IS NULL) <> (volume_id IS NULL))
);

CREATE UNIQUE INDEX legacy_dirs_sandbox ON legacy_dirs (sandbox_id) WHERE sandbox_id IS NOT NULL;
CREATE UNIQUE INDEX legacy_dirs_volume ON legacy_dirs (volume_id) WHERE volume_id IS NOT NULL;
CREATE INDEX legacy_dirs_node ON legacy_dirs (node_id);

-- Every persistent Workspace becomes a Volume, a tombstone if it was going
-- away, so versions published from it keep a source.
INSERT INTO volumes (id, owner_id, name, state, created_at, updated_at, deleted_at)
SELECT w.id, w.owner_id, 'workspace-' || left(replace(w.id::text, '-', ''), 8),
       CASE WHEN w.state IN ('deleting', 'deleted') THEN 'deleted' ELSE 'ready' END,
       w.created_at, now(),
       CASE WHEN w.state IN ('deleting', 'deleted') THEN coalesce(w.deleted_at, now()) END
FROM workspaces w
WHERE w.kind = 'persistent';

-- Only ready directories on a live Node have content to upload.
INSERT INTO legacy_dirs (id, node_id, sandbox_id, volume_id)
SELECT w.id, w.node_id,
       CASE WHEN w.kind = 'ephemeral' THEN w.sandbox_id END,
       CASE WHEN w.kind = 'persistent' THEN w.id END
FROM workspaces w
JOIN nodes n ON n.id = w.node_id AND n.lost_at IS NULL AND n.deleted_at IS NULL
LEFT JOIN sandboxes s ON s.id = w.sandbox_id
WHERE w.state = 'ready'
  AND (w.kind = 'persistent' OR (s.state IS NOT NULL AND s.state NOT IN ('deleting', 'deleted')));

-- A read-write mount of a persistent Workspace becomes the Volume at
-- /workspace, held as the old single-writer index held it. The hold trigger
-- fires only on state changes, so held is written here.
INSERT INTO sandbox_volumes (sandbox_id, volume_id, path, held)
SELECT s.id, s.workspace_id, '/workspace',
       s.state IN ('pending', 'provisioning', 'ready', 'stopping', 'deleting')
FROM sandboxes s
JOIN workspaces w ON w.id = s.workspace_id AND w.kind = 'persistent'
WHERE s.workspace_mode = 'rw' AND s.state <> 'deleted';

-- No Node walks a Workspace any more.
UPDATE resource_versions
SET state = 'failed', cause = 'publish_failed'
WHERE state = 'pending' AND publish_workspace_id IS NOT NULL;

UPDATE resource_versions rv
SET provenance = jsonb_build_object('source', 'volume',
        'volume', jsonb_build_object('id', w.id::text, 'path', rv.provenance -> 'workspace' -> 'path')),
    publish_volume_id = w.id
FROM workspaces w
WHERE rv.publish_workspace_id = w.id AND w.kind = 'persistent';

UPDATE resource_versions rv
SET provenance = jsonb_build_object('source', 'sandbox',
        'sandbox', jsonb_build_object('id', w.sandbox_id::text, 'path', rv.provenance -> 'workspace' -> 'path')),
    publish_sandbox_id = w.sandbox_id
FROM workspaces w
WHERE rv.publish_workspace_id = w.id AND w.kind = 'ephemeral';

DROP INDEX resource_versions_publish_idempotency;
DROP INDEX resource_versions_pending_publish;
ALTER TABLE resource_versions DROP COLUMN publish_workspace_id;

DROP INDEX sandboxes_workspace_rw;
DROP INDEX sandboxes_workspace_id;
ALTER TABLE sandboxes DROP COLUMN workspace_id, DROP COLUMN workspace_mode;

DROP TABLE workspaces;
