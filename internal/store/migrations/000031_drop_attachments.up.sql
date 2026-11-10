-- #220: the single-writer rule moves from the attachments table to sandboxes.
-- A Sandbox holds the write while it is active and mounts its Workspace rw.

-- A deleting row with no Attachment came from stopped or failed (or already
-- released its Runtime): it holds no write, as DeleteSandbox now records.
UPDATE sandboxes s
SET workspace_mode = 'ro'
WHERE s.state = 'deleting' AND s.workspace_mode = 'rw'
  AND NOT EXISTS (SELECT 1 FROM attachments a WHERE a.sandbox_id = s.id);

DROP TABLE attachments;

CREATE UNIQUE INDEX sandboxes_workspace_rw ON sandboxes (workspace_id)
WHERE workspace_mode = 'rw'
  AND state IN ('pending', 'provisioning', 'ready', 'stopping', 'deleting');
