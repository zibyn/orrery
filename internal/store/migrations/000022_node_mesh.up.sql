-- #188: the Node Mesh facts from the Node's latest Hello (ADR-0027,
-- schema.md). All three are NULL when that Hello carried no mesh: a Node
-- without kernel WireGuard, a Local Runtime Node, or an older Agent.

ALTER TABLE nodes
    ADD COLUMN mesh_public_key text,
    ADD COLUMN mesh_endpoint text,
    ADD COLUMN sandbox_subnet text;
