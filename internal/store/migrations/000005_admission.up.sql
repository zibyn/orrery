-- M1 ticket #56: admission queue and User Quota (ADR-0015).

-- One row per Sandbox waiting for admission. An open row (ended_at IS NULL)
-- is the quota hold: the Sandbox it belongs to counts as active until it is
-- admitted, times out, or is canceled. The queue is ordered by
-- (priority, enqueued_at); `system` requests are the platform's own.
CREATE TABLE admission_requests (
    id          uuid PRIMARY KEY,
    sandbox_id  uuid NOT NULL REFERENCES sandboxes (id) ON DELETE RESTRICT,
    priority    text NOT NULL DEFAULT 'user' CHECK (priority IN ('user', 'system')),
    enqueued_at timestamptz NOT NULL DEFAULT now(),
    deadline_at timestamptz NOT NULL,
    ended_at    timestamptz,
    outcome     text CHECK (outcome IN ('admitted', 'admission_timeout', 'unsatisfiable', 'canceled'))
);

CREATE UNIQUE INDEX admission_requests_open ON admission_requests (sandbox_id)
    WHERE ended_at IS NULL;
CREATE INDEX admission_requests_queue ON admission_requests (priority, enqueued_at)
    WHERE ended_at IS NULL;

-- Sandboxes that were already queued when this table appeared get a request
-- with the deployment's default deadline; otherwise they would wait forever.
INSERT INTO admission_requests (id, sandbox_id, priority, enqueued_at, deadline_at)
SELECT gen_random_uuid(), id, 'user', now(), now() + interval '1 hour'
FROM sandboxes
WHERE state = 'pending' AND desired_state = 'running';
