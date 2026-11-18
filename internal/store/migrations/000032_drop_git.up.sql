-- ADR-0032: Orrery manages no Git credentials, so Git Account Links and the
-- Sandbox Token go.
DROP TABLE git_links;
ALTER TABLE sandboxes DROP COLUMN token_ct, DROP COLUMN token_key_id;
