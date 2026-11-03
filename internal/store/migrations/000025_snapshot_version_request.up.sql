-- The version keeps the request it was created from, build or import, in the
-- POST /snapshots/{id}/versions body shape, so a new version can start from it.
-- Rows written before held only the build's {dockerfile}.

ALTER TABLE snapshot_versions RENAME COLUMN build_request TO request;
UPDATE snapshot_versions SET request = jsonb_build_object('build', request) WHERE request IS NOT NULL;
