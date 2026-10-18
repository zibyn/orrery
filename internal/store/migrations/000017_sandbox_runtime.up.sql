-- M5 ticket #97: the Sandbox's requested Runtime (ADR-0009, schema.md).
--
-- A `container` Sandbox is placed on a Docker or Podman Node; a `local`
-- Sandbox only on a Local Node. The choice is stored because it stays an
-- admission constraint on restart. Build rows keep the default: builds do not
-- run in a Sandbox Runtime (ADR-0021).

ALTER TABLE sandboxes
    ADD COLUMN runtime text NOT NULL DEFAULT 'container'
    CHECK (runtime IN ('container', 'local'));
