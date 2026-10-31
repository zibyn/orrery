-- #187: the Sandbox's current IPv4 address as the Agent reports it with
-- `running` (ADR-0027, schema.md). NULL whenever the Sandbox is not running.

ALTER TABLE sandboxes ADD COLUMN address text;
