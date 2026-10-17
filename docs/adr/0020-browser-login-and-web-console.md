# ADR-0020: Password and OIDC login, cookie Sessions, and an embedded Web console

- Status: accepted; amends ADR-0010's caller identity ("No OIDC") and ADR-0017's Web console
- Date: 2026-10-17

## Context

`#45` had to say how a browser user authenticates when the platform had only API Tokens, and how the Web console is served while staying a client of the public REST API and the Gateway. A browser's `WebSocket` cannot set an `Authorization` header, so a console holding an API Token in memory cannot open a PTY or follow Exec output. `GET /git-providers/{id}/authorize` had the same gap: a browser following a link carries no Bearer token. Coder logs browsers in with password, GitHub or OIDC inside coderd and stores every login as a row in the same `api_keys` table as CLI tokens. Daytona sends an external IdP's JWT (facts in `docs/research/web-console-auth-vs-coder-daytona.md`).

## Decision

- **Two login methods: username and password, and one external OIDC issuer.** Each User has exactly one `login_type`: `password`, `oidc`, or `none`, which means the User authenticates with tokens only. An admin may change it, and accounts are never merged automatically.
- **OIDC follows Coder.**
  - A login matches a User by (`issuer`, `sub`) first, then by verified email.
  - An unmatched login creates a User only when `allow_signups` is on, or when no User exists yet. The first User created that way becomes `admin`.
  - `email_domains` restricts which emails may log in, and an email the IdP marks unverified is rejected.
  - New Users get the deployment's default User Quota, zero by default.
  - The IdP's refresh token is not kept.
- **Passwords** are stored as argon2id hashes, with a 12-character minimum and no composition rules.
  - Users change their own password, and an admin resets anyone's. Either change revokes every Session of that User.
  - Failed logins are rate-limited per (username, source IP) in process memory, which is enough because the Control Plane is one process (ADR-0014).
  - There is no email recovery; a forgotten password is reset by an admin.
- **A Session is an `api_tokens` row with `kind=session`.**
  - Its scope is `user`, or `admin` for an admin User.
  - It expires after 24 hours idle, and use slides that expiry.
  - It travels as a host-only cookie, `stars_session; HttpOnly; Secure; SameSite=Lax; Path=/`, with no `Domain` attribute, so port-forward subdomains never receive it.
  - The API treats cookie and Bearer identically.
  - Requests that authenticate by cookie face two checks: unsafe methods pass `http.CrossOriginProtection`, and WebSocket upgrades must carry `Origin` equal to `api`'s own origin. Bearer requests face neither.
  - Port-forward subdomains are same-site with `api`, which is why `SameSite` alone is not the CSRF defence.
- **The CLI gets an API Token by logging in.** `stars-sandbox login` asks a password User for their password and exchanges it for a named API Token. It sends an OIDC User to the console's `/cli-auth` page, which creates a token with the Session; the User pastes it back, as with `coder login`.
- **Git account linking requires a Session.** Both `authorize` and `callback` accept only the cookie, and `callback` requires the User bound to `state` to be the Session's User. Without that check, one User could send another a link that ends with the second User's Git account linked to the first User. `authorize_url` points at the console's Git link page.
- **The Web console is separate source, built as static files and embedded in `stars-sandbox-server`.** It is served at `/` on the `api` listener, on the same origin as `/api/v1`, with no CORS. It calls only public endpoints, and a fixed page list in `docs/spec/api.md` defines its reach rather than parity with every operation.

## Consequences

- A User disabled at the IdP keeps working Sessions until they go idle, and API Tokens until an admin revokes them.
- The console's login, `/cli-auth` and Git link pages are needed as soon as Git linking is, before the rest of the console.
- `bootstrap` sets the first admin's password interactively; with OIDC configured, the first OIDC login can create the first admin instead.

## Rejected alternatives

- Exchanging a pasted API Token for a Session with no passwords and no IdP: the owner chose real logins as Coder offers them.
- Keeping the API Token in the SPA and sending Bearer: browsers cannot set headers on a `WebSocket`.
- Nosurf double-submit CSRF tokens (Coder): the standard library's `Sec-Fetch-Site` check needs no token rendered into the page.
- Binding the Git OAuth flow to `state` alone: it permits the account-linking attack above.
- A separately deployed console behind CORS, or a console backend: a second deployment unit, or a second source of semantics.
- Mapping OIDC groups to `admin`, several issuers, email recovery, or a `/setup` page.
