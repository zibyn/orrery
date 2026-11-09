# ADR-0029: Polaris is the only identity provider; machine Users come from a local command

- Status: proposed; supersedes ADR-0020's login methods, OIDC matching rules and password handling, and amends ADR-0010's caller identity
- Date: 2026-11-09

## Context

ADR-0020 gave the Control Plane two login methods, username and password and one external OIDC issuer, plus admin-created Users, admin password resets and a password `bootstrap`. That is a user system of its own: password storage, rate limiting, recovery by an admin, signup gating with `allow_signups` and `email_domains`, and account matching by email. The owner wants every application to share one identity system, Polaris (`~/Projects/polaris`), so Orrery stops carrying that complexity.

What Polaris offers decides where the line falls:

- It is an OIDC Provider with authorization code and PKCE, Back-Channel Logout, Webhooks (`user.disabled`, `user.enabled`, `user.deleted`, `user.updated` with `changed`) and RBAC on a registered API. Access tokens are RFC 9068 JWTs whose `entitlements` are limited to their `aud` (`docs/spec/protocol.md`, `docs/spec/rbac.md`).
- It has no device authorization flow and no personal access tokens, and an access token lives 10 minutes. Long-lived credentials for the CLI, MCP, the SDKs and CI therefore cannot be Polaris tokens.
- It holds no application business data (Polaris ADR 0032). User Quota and resource ownership stay in Orrery.
- It imports argon2id PHC hashes through `POST /users` (Polaris ADR 0033), and Orrery already stores passwords in that format.

Orrery's `oidc.go` is already a standard RP, with authorization code, PKCE and nonce, issuing a cookie Session. Pointing it at Polaris is most of the work.

## Decision

**Polaris answers who a person is; Orrery decides what they may do and which credentials they hold.** Sessions, API Tokens, Sandbox Tokens and Node certificates remain Orrery's. An API Token becomes a credential delegated after a Polaris login, not a login credential.

**The only browser login is OIDC against Polaris.**

- `oidc.issuer_url`, `oidc.client_id` and `oidc.client_secret` are required, and `oidc.audience` (the Orrery API's identifier, `api://orrery`) is new.
- `allow_signups` and `email_domains` are removed. Who may use Orrery is decided in Polaris, by turning on `requireRole` for the Orrery Application.
- A login matches a User by (`oidc_issuer`, `oidc_subject`) only, and creates one when nothing matches. The new User gets the deployment's default User Quota. There is no email matching, since Polaris is the only user pool.
- On every login the callback verifies the access token with `github.com/zibyn/polaris/sdk/go`. It sets `is_admin` from whether `entitlements` contains `orrery:admin`, and copies name and email from the claims.
- The Session row records the ID token's `sid`.

**Polaris notifies Orrery about changes.**

- `POST /api/v1/polaris/backchannel-logout` verifies the logout token (`VerifyLogoutToken`) and revokes the Sessions carrying that `sid`.
- `POST /api/v1/polaris/webhook` verifies the signature with the shared secret (`oidc.webhook_secret`), drops events older than the last one applied for that `sub`, and ignores unknown types:
  - `user.disabled` sets `users.disabled_at`, and `user.enabled` clears it. `authenticate` rejects every credential of a disabled User: Session, API Token and the User's Sandbox Tokens.
  - `user.deleted` runs the existing logical deletion of the User.
  - `user.updated` with `roles` in `changed` clears `is_admin`, and the next login sets it again. This fails closed: a newly granted admin must log in once. Pulling the current Roles through the Management API with an M2M Application is the upgrade path.

**Machine Users are created only by a local command.** A machine User has no Polaris identity (`oidc_issuer` and `oidc_subject` are both null), cannot log in through a browser, and acts only through API Tokens.

```
orrery-server user-token --name <name> [--admin] [--scope read_only|user|admin] [--token-name <name>]
orrery-server user-token --sub <polaris sub> [--scope ...] [--token-name <name>]
```

- It reads the deployment configuration file, connects to PostgreSQL directly, and prints one new API Token. It never goes over the network API, so possession of the configuration file and database credentials is the authority.
- `--name` finds or creates a machine User; `--admin` makes it an admin. Polaris events never touch a machine User.
- `--sub` mints a token for an existing Polaris-linked User. It is the break-glass path while Polaris is unreachable and how the first admin gets a token before any Role is assigned. It does not change that User's `is_admin`.
- e2e creates its Users (`admin`, `alice` and the rest) this way and never runs Polaris. The Dex-based `e2e/oidc_test.go` and the Dex service are removed. Login, Back-Channel Logout and Webhooks are tested at the API handler against PostgreSQL and an in-process fake Polaris.

**Removed.**

- `POST /sessions`, `PUT /users/me/password`, `PUT /users/{id}/password` and `POST /users`.
- The `is_admin` and `login_type` fields of `PATCH /users/{id}`, which now only edits what Orrery owns.
- Password `bootstrap`, `password.go`, and the login rate limiter.
- The console's password form, password change page, and create-user and reset-password actions. Profile edits link to the Polaris account center.
- `orrery login` prompts for no password. It always opens the console's `/cli-auth` page and takes the pasted API Token back.

**Kept.** `GET /users`, `GET /users/{id}`, `DELETE /users/{id}` (any User; the only removal for a machine User), quota endpoints, `/users/me`, `/users/me/tokens`, Git account linking, the Session cookie with its CSRF and Origin checks (ADR-0020), Sandbox Tokens and Node identity.

**Schema (migration 000027).**

- Drop `users.password_hash`, `users.login_type` and `users_password_hash_only_for_password`.
- Add `CHECK ((oidc_issuer IS NULL) = (oidc_subject IS NULL))`; null means a machine User.
- Add `users.disabled_at` and a nullable `api_tokens.sid`.

**Migration is a one-off script run before the configuration switches to Polaris.**

1. For every `password` User, call Polaris `POST /users` with the username, the email and the existing argon2id hash, so nobody resets a password. A `409` names an existing Polaris User's `sub`, which is linked instead.
2. Link existing `oidc` Users from another issuer to their Polaris User by verified email, once, in the script.
3. Write the returned `sub` and Polaris's issuer into `oidc_subject` and `oidc_issuer`.
4. `login_type=none` Users become machine Users as they are.
5. Assign the Orrery admin Role in Polaris to each current admin.

Existing API Tokens keep working.

## Consequences

- A User disabled or deleted in Polaris loses every Orrery credential within one Webhook delivery. That closes ADR-0020's gap, where idle Sessions and API Tokens outlived an IdP disable.
- Login depends on Polaris being up. Existing Sessions and API Tokens do not, because they are verified locally, and `user-token --sub` covers the rest.
- A Role change in Polaris reaches Orrery only at the next login (grant) or as soon as the Webhook arrives (revoke).
- `users.name` and `users.email` stay unique. A name collision on login, machine Users included, is disambiguated with the subject as the current OIDC path does. An email copied from the claims that is already taken leaves the User's stored email unchanged rather than failing the login.
- If the configuration switches before the migration script has run, existing people log in as new Users and lose sight of their resources. The runbook puts the script first.
- GLOSSARY: **Session** becomes "obtained by a User logging in through Polaris", and a **Machine User** entry is added. `docs/spec/api.md` loses the removed endpoints and gains the two Polaris endpoints.

## Rejected alternatives

- **Accepting Polaris access tokens directly as the API credential.** They live 10 minutes and Polaris has no device flow, so every CLI, MCP and CI caller would need a refresh-token store, and Sandbox-side tooling could not hold one.
- **Polaris M2M Applications as machine Users.** A `client_credentials` token speaks for the Application, not a User, and gets no refresh token. Orrery would map client IDs to Users and teach every client the grant, and e2e would need a running Polaris.
- **Machine Users created through the API by an admin.** That keeps a network path that creates identities outside Polaris, which is the complexity this ADR removes.
- **Quota as a Polaris Role or profile claim.** Polaris holds no application business data (Polaris ADR 0032), and quota changes would then need a Polaris admin.
- **Keeping password login as a fallback.** Two identity sources for the same person is what this ADR exists to end.
- **A loopback-redirect CLI login (RFC 8252), now.** Polaris supports it and it would remove the paste step, but `/cli-auth` already works; it can come later.
