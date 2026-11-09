-- A Session records the Polaris Session (the ID token's sid) it was issued
-- for, so Back-Channel Logout can end it (ADR-0029). Only kind=session has one.
ALTER TABLE api_tokens ADD COLUMN sid text;
ALTER TABLE api_tokens ADD CONSTRAINT api_tokens_sid_only_for_session
    CHECK (sid IS NULL OR kind = 'session');
CREATE INDEX api_tokens_sid ON api_tokens (sid) WHERE sid IS NOT NULL;
