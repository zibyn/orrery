-- ADR-0031: a Sandbox's filesystem is committed as Generations. Volumes (#277)
-- add generations.volume_id, its exactly-one check, and volumes.head_generation_id.

CREATE TABLE generations (
    id           uuid PRIMARY KEY,
    owner_id     uuid NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    sandbox_id   uuid REFERENCES sandboxes (id) ON DELETE RESTRICT,
    seq          bigint NOT NULL CHECK (seq > 0),
    kind         text NOT NULL CHECK (kind IN ('diff', 'tree')),
    manifest_key text NOT NULL,
    size         bigint NOT NULL CHECK (size >= 0),
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX generations_sandbox_seq ON generations (sandbox_id, seq);

CREATE TABLE generation_objects (
    generation_id uuid NOT NULL REFERENCES generations (id) ON DELETE CASCADE,
    sha256        text NOT NULL,
    PRIMARY KEY (generation_id, sha256)
);

-- The CAS sweep looks references up by digest.
CREATE INDEX generation_objects_sha256 ON generation_objects (sha256);

ALTER TABLE sandboxes
    ADD COLUMN head_generation_id uuid REFERENCES generations (id) ON DELETE RESTRICT;

-- An open commit between SubmitGeneration and CommitGeneration. Its id becomes
-- the Generation's id; missing is what Presign may sign for PUT. base is not a
-- foreign key: a superseded head may be purged while an upload still names it,
-- and the commit then fails the head check anyway.
CREATE TABLE generation_uploads (
    id                 uuid PRIMARY KEY,
    owner_id           uuid NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    sandbox_id         uuid NOT NULL REFERENCES sandboxes (id) ON DELETE CASCADE,
    node_id            uuid NOT NULL REFERENCES nodes (id) ON DELETE CASCADE,
    generation         bigint NOT NULL,
    base_generation_id uuid,
    missing            text[] NOT NULL,
    expires_at         timestamptz NOT NULL,
    created_at         timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX generation_uploads_node ON generation_uploads (node_id);
