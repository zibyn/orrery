-- #130: Snapshot creation accepts an Idempotency Key like every other create
-- endpoint (ADR-0017). The index ignores deleted rows so a reused key creates
-- a new Snapshot instead of colliding with a tombstone (#122).

ALTER TABLE snapshots ADD COLUMN idempotency_key text;

CREATE UNIQUE INDEX snapshots_owner_idempotency
    ON snapshots (owner_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL AND deleted_at IS NULL;
