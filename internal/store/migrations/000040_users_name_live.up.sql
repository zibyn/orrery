-- A deleted User releases its name, as a deleted Sandbox, Resource or Volume
-- does: GetUserByName already skips tombstones, so a full unique constraint
-- made user-token --name of a deleted machine User neither find nor create
-- one. The index keeps the constraint's name, which pipeline.go and oidc.go
-- match on.
ALTER TABLE users DROP CONSTRAINT users_name_key;
CREATE UNIQUE INDEX users_name_key ON users (name) WHERE deleted_at IS NULL;
