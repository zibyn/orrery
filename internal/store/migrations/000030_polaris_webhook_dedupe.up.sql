-- Polaris event times have second precision and deliveries are unordered
-- (ADR-0029). polaris_disabled_at is the time of the last user.disabled
-- applied, so a user.enabled of the same second cannot undo it.
ALTER TABLE users ADD COLUMN polaris_disabled_at timestamptz;

-- The webhook-id of every Polaris delivery applied: a redelivery (Polaris
-- keeps the id across retries) changes nothing.
CREATE TABLE polaris_webhook_deliveries (
    id text PRIMARY KEY,
    received_at timestamptz NOT NULL DEFAULT now()
);
