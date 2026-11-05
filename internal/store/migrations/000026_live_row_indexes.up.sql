-- Indexes that match the predicates the live-row queries actually use.
--
-- A Sandbox keeps its row after deletion (API idempotency), so most of the
-- table is state = 'deleted'. deleted_at is not the same fact: it marks the
-- delete request and releases the name at once, while state reaches deleted
-- only after teardown. The owner and sweep queries filter on state, and no
-- partial index carried that predicate, so each one scanned every tombstone.

-- ListSandboxes, CountActiveSandboxResources, GetSandboxByIdempotencyKey and,
-- with no leading column, the Reconciler's per-second sweep and
-- CountSandboxSnapshotReferences.
CREATE INDEX sandboxes_owner_live ON sandboxes (owner_id, id) WHERE state <> 'deleted';

-- ListWorkspaces, and the provisioning and deleting sweeps.
CREATE INDEX workspaces_owner_live ON workspaces (owner_id, id) WHERE state <> 'deleted';

-- FailSandboxesOnWorkspaceLost and FinishExecsOnWorkspaceLost.
CREATE INDEX sandboxes_workspace_id ON sandboxes (workspace_id) WHERE workspace_id IS NOT NULL;

-- The ON DELETE RESTRICT check when the purge deletes a version row; without
-- these it scans every Resource or Snapshot per deleted version.
CREATE INDEX resources_current_version ON resources (current_version_id)
    WHERE current_version_id IS NOT NULL;
CREATE INDEX snapshots_current_version ON snapshots (current_version_id)
    WHERE current_version_id IS NOT NULL;
