-- #151: an explicit SnapshotVersion delete writes a tombstone; purge removes
-- its registry manifest before the row. As with #122, a tombstone must not
-- block a create that reuses its Idempotency Key.

ALTER TABLE snapshot_versions ADD COLUMN deleted_at timestamptz;

DROP INDEX snapshot_versions_idempotency;
CREATE UNIQUE INDEX snapshot_versions_idempotency
    ON snapshot_versions (snapshot_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL AND deleted_at IS NULL;
