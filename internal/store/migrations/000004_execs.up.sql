-- M1 ticket #51: non-PTY Execs with durable logs (ADR-0015, ADR-0019).

CREATE TABLE execs (
    id                  uuid PRIMARY KEY,
    sandbox_id          uuid NOT NULL REFERENCES sandboxes (id) ON DELETE RESTRICT,
    argv                jsonb NOT NULL,
    env                 jsonb NOT NULL DEFAULT '{}',
    cwd                 text NOT NULL DEFAULT '',
    stdin               bytea,
    pty                 jsonb,
    timeout_seconds     integer CHECK (timeout_seconds > 0),
    accepted_at         timestamptz NOT NULL DEFAULT now(),
    started_at          timestamptz,
    cancel_requested_at timestamptz,
    deadline_at         timestamptz,
    finished_at         timestamptz,
    cause               text,
    exit_code           integer,
    signal              integer,
    log_truncated       boolean NOT NULL DEFAULT false,
    stored_seq          bigint NOT NULL DEFAULT 0,
    generation          bigint NOT NULL,
    idempotency_key     text,
    -- Derivable state, never written (ADR-0015).
    state               text GENERATED ALWAYS AS (
        CASE
            WHEN finished_at IS NOT NULL THEN 'finished'
            WHEN cancel_requested_at IS NOT NULL THEN 'canceling'
            WHEN started_at IS NOT NULL THEN 'running'
            ELSE 'pending'
        END
    ) STORED
);

CREATE INDEX execs_sandbox_id ON execs (sandbox_id);
CREATE INDEX execs_active ON execs (sandbox_id) WHERE finished_at IS NULL;
CREATE UNIQUE INDEX execs_sandbox_idempotency ON execs (sandbox_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;

-- One row per OutputBatch. first_seq is the frame's byte offset on the Node
-- record, so an Agent resume never duplicates a frame.
CREATE TABLE exec_log_chunks (
    exec_id   uuid NOT NULL REFERENCES execs (id) ON DELETE RESTRICT,
    first_seq bigint NOT NULL,
    last_seq  bigint NOT NULL,
    frames    jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (exec_id, first_seq)
);
