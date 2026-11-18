-- ADR-0033: `orrery login` is a device authorization flow. A row lives from
-- POST /cli-logins until the CLI collects its token or it expires; only the
-- device code's hash is kept, and no token exists until the CLI collects it.
CREATE TABLE cli_logins (
    device_hash bytea PRIMARY KEY,
    user_code text NOT NULL UNIQUE,
    user_id uuid REFERENCES users (id) ON DELETE CASCADE,
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX cli_logins_expires_at ON cli_logins (expires_at);
