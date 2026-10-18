-- #122: an Idempotency Key is unique only among live rows, as the replay
-- lookup requires. A tombstone keeps its key until the purge clears it, but it
-- must not block a create that reuses the key (otherwise the insert hits the
-- index and answers 500 instead of creating a new resource).

DROP INDEX sandboxes_owner_idempotency;
CREATE UNIQUE INDEX sandboxes_owner_idempotency
    ON sandboxes (owner_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL AND deleted_at IS NULL;

DROP INDEX workspaces_owner_idempotency;
CREATE UNIQUE INDEX workspaces_owner_idempotency
    ON workspaces (owner_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL AND state <> 'deleted';

DROP INDEX resources_owner_idempotency;
CREATE UNIQUE INDEX resources_owner_idempotency
    ON resources (owner_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL AND deleted_at IS NULL;
