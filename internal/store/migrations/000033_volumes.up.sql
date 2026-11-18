-- ADR-0031 (#277): Volumes are named directories in object storage, bound to
-- no Node, committed as Generations of kind tree, and held by one Sandbox at
-- a time.

CREATE TABLE volumes (
    id                 uuid PRIMARY KEY,
    owner_id           uuid NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    name               text NOT NULL,
    state              text NOT NULL DEFAULT 'ready' CHECK (state IN ('ready', 'deleted')),
    head_generation_id uuid,
    idempotency_key    text,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    deleted_at         timestamptz
);

CREATE UNIQUE INDEX volumes_owner_name ON volumes (owner_id, name) WHERE state <> 'deleted';
CREATE UNIQUE INDEX volumes_owner_idempotency ON volumes (owner_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL AND state <> 'deleted';
CREATE INDEX volumes_owner_live ON volumes (owner_id, id) WHERE state <> 'deleted';

ALTER TABLE generations
    ADD COLUMN volume_id uuid REFERENCES volumes (id) ON DELETE RESTRICT,
    ADD CONSTRAINT generations_one_holder CHECK ((sandbox_id IS NULL) <> (volume_id IS NULL));

CREATE UNIQUE INDEX generations_volume_seq ON generations (volume_id, seq);

ALTER TABLE volumes
    ADD CONSTRAINT volumes_head_generation_id_fkey
    FOREIGN KEY (head_generation_id) REFERENCES generations (id) ON DELETE RESTRICT;

-- Written at Sandbox creation and never changed, except held: the hold
-- follows the Sandbox's state (schema.md sandbox_volumes).
CREATE TABLE sandbox_volumes (
    sandbox_id uuid NOT NULL REFERENCES sandboxes (id) ON DELETE RESTRICT,
    volume_id  uuid NOT NULL REFERENCES volumes (id) ON DELETE RESTRICT,
    path       text NOT NULL,
    held       boolean NOT NULL,
    PRIMARY KEY (sandbox_id, path)
);

-- One Volume, one holder (#220's rule, moved to the association).
CREATE UNIQUE INDEX sandbox_volumes_held ON sandbox_volumes (volume_id) WHERE held;
CREATE INDEX sandbox_volumes_volume ON sandbox_volumes (volume_id);

-- The hold changes in the statement that changes the Sandbox's state, so
-- every transition (start, stop, failure, Node loss, deletion) keeps it, and a
-- start whose Volume another Sandbox holds fails sandbox_volumes_held. A
-- deleting Sandbox keeps what it held: deleting from stopped or failed does
-- not take a Volume back.
CREATE FUNCTION sandbox_volumes_follow_state() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.state IN ('pending', 'provisioning', 'ready', 'stopping') THEN
        UPDATE sandbox_volumes SET held = true WHERE sandbox_id = NEW.id AND NOT held;
    ELSIF NEW.state IN ('stopped', 'failed', 'deleted') THEN
        UPDATE sandbox_volumes SET held = false WHERE sandbox_id = NEW.id AND held;
    END IF;
    RETURN NULL;
END;
$$;

CREATE TRIGGER sandboxes_volume_hold
AFTER UPDATE OF state ON sandboxes
FOR EACH ROW WHEN (OLD.state IS DISTINCT FROM NEW.state)
EXECUTE FUNCTION sandbox_volumes_follow_state();

-- Sandbox and Volume publishes, deduplicated per (source, key) like
-- Workspace publishes (migration 000014).
ALTER TABLE resource_versions
    ADD COLUMN publish_sandbox_id uuid REFERENCES sandboxes (id) ON DELETE RESTRICT,
    ADD COLUMN publish_volume_id uuid REFERENCES volumes (id) ON DELETE RESTRICT;

CREATE UNIQUE INDEX resource_versions_publish_sandbox_idempotency
    ON resource_versions (publish_sandbox_id, publish_idempotency_key)
    WHERE publish_idempotency_key IS NOT NULL AND publish_sandbox_id IS NOT NULL;
CREATE UNIQUE INDEX resource_versions_publish_volume_idempotency
    ON resource_versions (publish_volume_id, publish_idempotency_key)
    WHERE publish_idempotency_key IS NOT NULL AND publish_volume_id IS NOT NULL;

CREATE INDEX resource_versions_pending_sandbox_publish
    ON resource_versions (publish_sandbox_id)
    WHERE state = 'pending' AND publish_sandbox_id IS NOT NULL;
CREATE INDEX resource_versions_pending_volume_publish
    ON resource_versions (publish_volume_id)
    WHERE state = 'pending' AND publish_volume_id IS NOT NULL;
