-- #165: the Sandbox's Egress Mode (ADR-0026, schema.md). Fixed at creation and
-- sent unchanged in every CreateSandbox. egress_allow holds the allowlist
-- entries as written and is empty for the other modes. Build rows keep the
-- default: builds do not run in a Sandbox Runtime (ADR-0021).

ALTER TABLE sandboxes
    ADD COLUMN egress_mode text NOT NULL DEFAULT 'open'
        CHECK (egress_mode IN ('open', 'none', 'allowlist')),
    ADD COLUMN egress_allow text[] NOT NULL DEFAULT '{}',
    ADD CHECK ((egress_mode = 'allowlist') = (cardinality(egress_allow) > 0));
