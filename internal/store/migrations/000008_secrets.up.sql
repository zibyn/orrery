-- M3 ticket #74: caller Secrets, stored encrypted (ADR-0013, ADR-0017).

CREATE TABLE secrets (
    id         uuid PRIMARY KEY,
    owner_id   uuid NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    name       text NOT NULL,
    ciphertext bytea NOT NULL,
    key_id     text NOT NULL,
    size       integer NOT NULL CHECK (size >= 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX secrets_owner_name ON secrets (owner_id, name);
