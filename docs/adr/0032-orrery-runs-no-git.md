# ADR-0032: Orrery manages no Git credentials, and an Exec names its own Secrets

- Status: accepted; supersedes ADR-0013 except its Caller Secrets and their audit, and ADR-0016's Sandbox Token; amends in place ADR-0017's Git and Secrets, command lines and `api` reachability; `GLOSSARY.md` drops Git Provider, Git Account Link and Sandbox Token; `api.md`, `defaults.md`, `node-protocol.md`, `runtime.md` and `schema.md` follow, and those ADRs carry the amendment notes
- Date: 2026-11-18

## Context

ADR-0013 made Orrery a Git credential broker. Deployment configuration lists Git Providers, each User links an account through OAuth, and the tokens are stored in `git_links`. Every Exec carries a Sandbox Token whose one capability is `POST /git/credential` (ADR-0016), and the Node Agent installs a platform credential helper through `GIT_CONFIG_*` after clearing the image's helper list (ADR-0017).

Orrery's caller, Orqestra, already holds each User's GitHub App user token in its own MCP Connection and serializes its refresh with an advisory lock. A GitHub refresh returns a new refresh token and the old one stops working, so Orrery cannot refresh the same grant without breaking the caller's copy. Linking a second grant in Orrery would make the User authorize GitHub twice for one Agent.

The platform helper also stops a caller from configuring Git itself. Platform values overwrite the caller's Exec `env` on the same name (`node-protocol.md`), so a caller's own `GIT_CONFIG_COUNT` is replaced, and a Secret may not bind a `GIT_*` name.

A caller has no other safe way to pass a token. A Sandbox's Secrets are copied when it starts (`schema.md`), so a refreshed token reaches a running Sandbox only after a restart, and a GitHub App user token expires after eight hours. Exec `env` and `stdin` are stored in plaintext in `execs` (`internal/api/execs.go:219-236`).

## Decision

**Orrery manages no Git credentials.** Git Providers (`git_providers`), Git Account Links (`git_links`, `/git-providers`, `/external-auth/{id}/callback`, `/users/me/git-links`), `POST /git/credential`, the Sandbox Token (`ors_`, `sandboxes.token_ct`, `ORRERY_SANDBOX_TOKEN` in every Exec), `orrery git-credential`, `orrery token <provider>`, the console's Git page and the Agent's `GIT_CONFIG_*` entries are removed. Git in a Sandbox reads only the image's configuration and the caller's. The platform images keep `git` and `openssh-client`. The CLI still reads `ORRERY_SANDBOX_TOKEN` as a fallback API Token, so a caller can inject one.

**An Exec names its own Secrets.** `POST /sandboxes/{id}/execs` takes `secrets?: {ENV_NAME: secret_name}`.

- Names resolve against the Sandbox owner's Secrets, whoever creates the Exec, as a Sandbox's own Secrets do at start.
- They resolve once, when the Exec is created. The values are stored encrypted on the Exec row, never in `env` or `stdin`, so a repeated `StartExec` sends the same values. The ciphertext is cleared when the Exec finishes.
- An unknown Secret, or an invalid or reserved name, is `400 invalid_request`, as at Sandbox creation.
- On the same name, the Exec's Secret overrides the Sandbox's.
- A replay with the same `Idempotency-Key` returns the existing Exec and resolves nothing again.

A caller rotates a credential with `PUT /secrets/{name}`, and the next Exec gets the new value without a Sandbox restart.

**`GIT_*` is no longer reserved.** The prefix was reserved so that a Secret could not displace the platform helper. `LD_*`, `ORRERY_*`, `PATH` and `HOME` stay reserved.

**A caller authenticates Git the same way on every host.** The Exec's `env` sets one `credential.https://<host>.helper` per Git host through `GIT_CONFIG_COUNT`, `GIT_CONFIG_KEY_n` and `GIT_CONFIG_VALUE_n`. The helper prints the token from an environment variable that the Exec's `secrets` binds. GitHub, GitLab and Gitea differ only in the host and the username, and nothing in `env` is secret.

## Consequences

- A deployment needs `encryption_keys` only for Secrets, and creating a Sandbox no longer generates a token.
- Sandboxes no longer reach `api` for Git, so Egress Rules need an `allow` entry for `api` only when a caller runs the CLI in a Sandbox. `ORRERY_URL` is still set for that CLI.
- Clients of the removed routes and commands break at upgrade, with no alias period. Existing Git Account Links are dropped, and Users authorize again in their caller.
- Orrery's audit no longer records Git token use. The caller that holds the token records its own.
- A token still enters the Sandbox. A process that can read the Exec's environment sees it while the Exec runs, and the provider's scoping bounds what it reaches, as under ADR-0013.
- ADR-0010, ADR-0014, ADR-0020, ADR-0024, ADR-0026 and ADR-0029 still mention Git Providers, Git Account Links, Git tokens or the Sandbox Token. Those passages describe what this ADR removes.
- The research behind ADR-0013 (`docs/research/git-short-lived-credentials.md`, `sandbox-git-integration.md` and `recheck-git-secrets-vs-coder-daytona.md`) is deleted. Citations of it refer to Git history before this ADR.

## Rejected alternatives

- Keeping Git Account Links next to the caller's grant: the User authorizes GitHub in two places for one Agent, and Orrery keeps an OAuth client, refresh and a helper that the caller already has.
- Letting a caller hand its grant to Orrery: both would refresh it, and each refresh invalidates the other's refresh token.
- Passing the token in Exec `env`: `execs.env` stores it in plaintext.
- Sandbox Secrets with a restart on rotation: a GitHub App user token lasts eight hours, and a restart stops every running Exec and commits the filesystem (ADR-0031).
- Re-reading a Sandbox's Secrets at every Exec: it changes, for every caller, the rule that a running Sandbox keeps the values it started with (ADR-0013).
- A platform helper that picks a Secret by host: it keeps Git logic in Orrery to do what one caller-set `GIT_CONFIG` entry already does.
