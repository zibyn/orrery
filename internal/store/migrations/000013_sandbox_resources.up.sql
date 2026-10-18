-- M4 ticket #86: Resource versions mounted into Sandboxes, and the Agent's
-- condition while it fetches them (schema.md, runtime.md §2 and §11).

-- conditions is the Sandbox's public condition set, e.g.
-- {"fetching_resources":true} while the Node pulls the version trees.
ALTER TABLE sandboxes ADD COLUMN conditions jsonb;

-- A reference retains the ResourceVersion: it cannot be deleted while any
-- Sandbox (stopped included) still names it (schema.md).
CREATE TABLE sandbox_resources (
    sandbox_id          uuid NOT NULL REFERENCES sandboxes (id) ON DELETE RESTRICT,
    resource_version_id uuid NOT NULL REFERENCES resource_versions (id) ON DELETE RESTRICT,
    mount               text NOT NULL,
    PRIMARY KEY (sandbox_id, mount)
);

CREATE INDEX sandbox_resources_version ON sandbox_resources (resource_version_id);
