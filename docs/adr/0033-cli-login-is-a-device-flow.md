# ADR-0033: `orrery login` is a device authorization flow

- Status: accepted; supersedes ADR-0029's "`orrery login` opens `/cli-auth` and takes the pasted API Token back" and its deferred loopback-redirect alternative; `api.md`, `schema.md` and `defaults.md` follow
- Date: 2026-11-18

## Context

ADR-0029 left `orrery login` printing the console's `/cli-auth` link. The page created an API Token as soon as it loaded and showed it, and the User pasted it into the terminal. That has three problems:

- The CLI usually runs on a remote machine (a dev box, a GPU Node, a Sandbox) while the browser is on the User's laptop. Copying a long token between them is awkward, and the loopback redirect ADR-0029 deferred cannot work there: the browser cannot reach the remote machine's `127.0.0.1`.
- Every load of `/cli-auth`, a refresh included, created another long-lived API Token, so unused `cli-…` tokens piled up.
- `/cli-auth` was a sidebar entry although it is only the CLI's landing page.

Polaris has no device authorization endpoint (ADR-0029), but the flow does not need the identity provider: the console already has a Session, and the Control Plane already issues API Tokens.

## Decision

**The Control Plane runs a device authorization flow shaped like RFC 8628 and `gh auth login`.**

1. `POST /cli-logins`, unauthenticated, returns a secret `device_code`, an eight-letter `user_code` (`XXXX-XXXX`, consonants only), the console's `verification_uri` (`/cli-auth`), `expires_in` and `interval`.
2. `orrery login` prints `verification_uri?code=<user_code>` and the code, and does not open a browser. The User opens the link in any browser, signs in through Polaris if needed, checks the code against the terminal and clicks Approve, which calls `POST /cli-logins/approve {user_code}` with the Session. Opening or refreshing the page creates nothing.
3. The CLI polls `POST /cli-logins/token {device_code}` every `interval`. Before approval the answer is `{status: "pending"}`. The first poll after approval creates a `user`-scoped named API Token for the approving User, deletes the login and returns `{status: "approved", token}`. An expired, unknown or already collected login is `404`.

The login stores only the device code's hash and, once approved, the User's id. The token is created at collection, so no plaintext token is ever stored or shown in the console. Expired rows are purged. Unexpired logins are capped across the deployment, and `POST /cli-logins` returns `429 quota_exceeded` past the cap, so the unauthenticated endpoint cannot grow the table without bound.

Pasting a token is gone. A caller that already holds an API Token passes it with `--token`, `ORRERY_SANDBOX_TOKEN` or the config file as before.

## Consequences

- `orrery login` works the same from a laptop, an SSH session and a Sandbox.
- The `/cli-auth` page is a form with the code prefilled and an Approve button; it leaves the sidebar.
- Phishing is the standard device-flow risk: someone can send a User a link to approve a login they started. The page tells the User to approve only a login they started and to check the code against their own terminal.
- A failure while creating the token after the login was claimed loses that login; the User runs `orrery login` again.

## Alternatives considered

- **Keep the paste, and only stop the page from creating a token on load.** It fixes the token pile-up but keeps the copy between machines.
- **Loopback redirect (RFC 8252).** It only works when the CLI and the browser are on the same machine.
- **Store the token at approval and hand it over on the next poll.** It keeps a plaintext token in the database between the two steps; creating it at collection needs nothing extra.
