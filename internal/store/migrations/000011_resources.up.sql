-- M4 ticket #85: Resources, upload sessions, owner-scoped CAS and the
-- manifests that address ResourceVersions (ADR-0012, ADR-0021, schema.md).

CREATE TABLE resources (
    id                 uuid PRIMARY KEY,
    owner_id           uuid NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    name               text NOT NULL,
    current_version_id uuid,
    idempotency_key    text,
    deleted_at         timestamptz,
    created_at         timestamptz NOT NULL DEFAULT now()
);

-- A name is unique only while the Resource is alive (schema.md).
CREATE UNIQUE INDEX resources_owner_name
    ON resources (owner_id, name) WHERE deleted_at IS NULL;

-- Every creatable resource carries an Idempotency Key (schema.md). The
-- resources table has no owner-scoped natural key path to reuse, so the key
-- lives here alongside the name.
CREATE UNIQUE INDEX resources_owner_idempotency
    ON resources (owner_id, idempotency_key) WHERE idempotency_key IS NOT NULL;

CREATE TABLE resource_versions (
    id              uuid PRIMARY KEY,
    resource_id     uuid NOT NULL REFERENCES resources (id) ON DELETE RESTRICT,
    address         text,
    manifest_key    text,
    size            bigint NOT NULL DEFAULT 0 CHECK (size >= 0),
    provenance      jsonb NOT NULL,
    state           text NOT NULL CHECK (state IN ('pending', 'ready', 'failed', 'unavailable')),
    cause           text,
    idempotency_key text,
    expires_at      timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX resource_versions_resource ON resource_versions (resource_id, id);
-- api.md §1 keys idempotency by (User, endpoint, key) and the endpoint
-- includes its path parameters, so a version creation is unique per
-- (Resource, key). That is stricter in the right dimension than the generic
-- schema.md (owner_id, key) convention: it does not replay another
-- Resource's version for a reused key.
CREATE UNIQUE INDEX resource_versions_resource_idempotency
    ON resource_versions (resource_id, idempotency_key) WHERE idempotency_key IS NOT NULL;

-- The current pointer is added after both tables exist.
ALTER TABLE resources
    ADD CONSTRAINT resources_current_version
    FOREIGN KEY (current_version_id) REFERENCES resource_versions (id) ON DELETE RESTRICT;

CREATE TABLE resource_version_objects (
    version_id uuid NOT NULL REFERENCES resource_versions (id) ON DELETE RESTRICT,
    sha256     text NOT NULL,
    PRIMARY KEY (version_id, sha256)
);

CREATE TABLE cas_objects (
    owner_id   uuid NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    sha256     text NOT NULL,
    size       bigint NOT NULL CHECK (size >= 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (owner_id, sha256)
);

CREATE TABLE uploads (
    id          uuid PRIMARY KEY,
    resource_id uuid NOT NULL REFERENCES resources (id) ON DELETE RESTRICT,
    address     text NOT NULL,
    manifest    jsonb NOT NULL,
    expires_at  timestamptz NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX uploads_resource_address
    ON uploads (resource_id, address, expires_at);
