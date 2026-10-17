-- M3 ticket #76: Sandbox Tokens are stored encrypted, like Secrets (ADR-0016,
-- schema.md). A rename keeps rows created before this ticket.
ALTER TABLE sandboxes RENAME COLUMN token_hash TO token_ct;
ALTER TABLE sandboxes ADD COLUMN token_key_id text;
