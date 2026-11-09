-- Polaris webhooks reach Users (ADR-0029). While disabled_at is set every
-- credential of the User is rejected. polaris_event_at is the creation time of
-- the last Polaris event applied, so a stale redelivery cannot undo a newer one.
ALTER TABLE users ADD COLUMN disabled_at timestamptz;
ALTER TABLE users ADD COLUMN polaris_event_at timestamptz;
