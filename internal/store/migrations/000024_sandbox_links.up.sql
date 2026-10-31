-- #189: the Sandbox's Links, declared on the target (ADR-0027, schema.md).
-- Fixed at creation. links_from holds the source Sandbox ids; links_ports
-- empty means every port.

ALTER TABLE sandboxes
    ADD COLUMN links_from uuid[] NOT NULL DEFAULT '{}',
    ADD COLUMN links_ports integer[] NOT NULL DEFAULT '{}';
