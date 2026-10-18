-- M3 ticket #79: Git Account Links, stored encrypted (ADR-0013, schema.md).

CREATE TABLE git_links (
    user_id          uuid NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    provider_id      text NOT NULL,
    access_token_ct  bytea NOT NULL,
    refresh_token_ct bytea,
    key_id           text NOT NULL,
    expires_at       timestamptz,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, provider_id)
);
