-- M4 ticket #88: Snapshot import and platform images (ADR-0012, ADR-0021,
-- schema.md). A Snapshot with owner_id NULL is a platform Snapshot; the
-- current-version FK is added after both tables exist.

CREATE TABLE snapshots (
    id                 uuid PRIMARY KEY,
    owner_id           uuid REFERENCES users (id) ON DELETE RESTRICT,
    name               text NOT NULL,
    current_version_id uuid,
    deleted_at         timestamptz,
    created_at         timestamptz NOT NULL DEFAULT now()
);

-- NULLS NOT DISTINCT keeps two platform Snapshots (owner_id NULL) from
-- sharing a name. A tombstoned Snapshot releases its name.
CREATE UNIQUE INDEX snapshots_owner_name
    ON snapshots (owner_id, name) NULLS NOT DISTINCT
    WHERE deleted_at IS NULL;

CREATE TABLE snapshot_versions (
    id               uuid PRIMARY KEY,
    snapshot_id      uuid NOT NULL REFERENCES snapshots (id) ON DELETE RESTRICT,
    -- "sha256:<hex>" only; the repository is derived from the Snapshot.
    digest           text CHECK (digest IS NULL OR digest ~ '^sha256:[0-9a-f]{64}$'),
    state            text NOT NULL CHECK (state IN ('building', 'ready', 'failed', 'cancelled')),
    cause            text,
    source           text NOT NULL CHECK (source IN ('import', 'build')),
    -- Reserved for the M4 build ticket (#89).
    build_sandbox_id uuid,
    build_exec_id    uuid,
    provenance       jsonb,
    idempotency_key  text,
    created_at       timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX snapshot_versions_snapshot ON snapshot_versions (snapshot_id, id);

CREATE UNIQUE INDEX snapshot_versions_idempotency
    ON snapshot_versions (snapshot_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;

ALTER TABLE snapshots
    ADD CONSTRAINT snapshots_current_version_id_fkey
    FOREIGN KEY (current_version_id) REFERENCES snapshot_versions (id) ON DELETE RESTRICT;
