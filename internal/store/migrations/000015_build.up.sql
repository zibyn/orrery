-- M4 ticket #89: Snapshot builds (ADR-0017, ADR-0021, schema.md).
--
-- A build is a SnapshotVersion whose work runs on a Node: an internal
-- `kind=build` Sandbox row carries the Allocation and the build Exec, but has
-- no Runtime container, never appears in the API and does not count toward
-- User Quota.

ALTER TABLE sandboxes
    ADD COLUMN kind text NOT NULL DEFAULT 'user'
    CHECK (kind IN ('user', 'build'));

-- The build request is the desired state the Reconciler turns into a
-- BuildSnapshot command; it is not part of a version's result provenance.
ALTER TABLE snapshot_versions ADD COLUMN build_request jsonb;

-- The build row and its Exec are integral to the version, so the references
-- are real foreign keys (schema.md: ON DELETE RESTRICT).
ALTER TABLE snapshot_versions
    ADD CONSTRAINT snapshot_versions_build_sandbox
        FOREIGN KEY (build_sandbox_id) REFERENCES sandboxes (id) ON DELETE RESTRICT,
    ADD CONSTRAINT snapshot_versions_build_exec
        FOREIGN KEY (build_exec_id) REFERENCES execs (id) ON DELETE RESTRICT;

-- The Reconciler sweeps a build version and its sandbox together.
CREATE INDEX snapshot_versions_build ON snapshot_versions (build_sandbox_id)
    WHERE source = 'build';
CREATE INDEX snapshot_versions_build_exec ON snapshot_versions (build_exec_id)
    WHERE build_exec_id IS NOT NULL;
