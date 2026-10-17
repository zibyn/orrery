-- M1 ticket #49: Node enrollment and the Control stream.
CREATE TABLE enrollment_packets (
    id              uuid PRIMARY KEY,
    secret_hash     bytea NOT NULL,
    expires_at      timestamptz NOT NULL,
    used_at         timestamptz,
    created_by      uuid NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    created_at      timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX enrollment_packets_created_at ON enrollment_packets (created_at);

-- Unreachable is derived from last_seen_at on read, never stored (ADR-0014).
CREATE TABLE nodes (
    id              uuid PRIMARY KEY,
    name            text NOT NULL UNIQUE,
    token_hash      bytea NOT NULL,
    paused          boolean NOT NULL DEFAULT false,
    lost_at         timestamptz,
    agent_version   text,
    proto_major     integer,
    proto_minor     integer,
    last_seen_at    timestamptz,
    runtime_health  text,
    capabilities    jsonb,
    hardware        jsonb,
    created_at      timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE gpus (
    uuid            text PRIMARY KEY,
    node_id         uuid NOT NULL REFERENCES nodes (id) ON DELETE RESTRICT,
    model           text NOT NULL
);

CREATE INDEX gpus_node_id ON gpus (node_id);
