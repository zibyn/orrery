-- #278: a Volume commits through the Sandbox that holds it. The upload keeps
-- that Sandbox in sandbox_id (its fence and Node authorize the PUTs) and
-- names the Volume whose head it advances.
ALTER TABLE generation_uploads
    ADD COLUMN volume_id uuid REFERENCES volumes (id) ON DELETE CASCADE;
