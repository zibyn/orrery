-- ADR-0032: an Exec's own `secrets` are copied at creation, like a Sandbox's
-- at start, and cleared when the Exec finishes (schema.md).
ALTER TABLE execs ADD COLUMN secret_env_ct bytea;
