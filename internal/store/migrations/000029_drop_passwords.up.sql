-- Polaris is the only identity provider (ADR-0029): no password, no login
-- type. A User is Polaris-linked (issuer and subject) or a machine User
-- (neither). Migrate refuses this file while a User who logs in is unlinked.
ALTER TABLE users DROP CONSTRAINT users_password_hash_only_for_password;
ALTER TABLE users DROP COLUMN password_hash;
ALTER TABLE users DROP COLUMN login_type;
ALTER TABLE users ADD CONSTRAINT users_oidc_identity_pair
    CHECK ((oidc_issuer IS NULL) = (oidc_subject IS NULL));
