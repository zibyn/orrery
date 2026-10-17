# ADR-0013: Git accounts and caller Secrets

- Status: accepted
- Date: 2026-10-17

## Context

This is a coding sandbox: an Agent must be able to clone and push with plain `git`, and open pull requests, against GitHub, GitLab and self-hosted instances, on behalf of the User who owns the Sandbox. ADR-0010 had required every caller credential entering a Sandbox to be short-lived, Sandbox-scoped and revocable, and ADR-0011 kept credentials off the Capability Channel entirely. Only GitHub can issue such a credential (`docs/research/git-short-lived-credentials.md`); GitLab and Gitea cannot. Vendors do not hold themselves to that rule. Coder's External Auth registers one OAuth application per provider, lets each user authorize once ("Login with GitHub / GitLab"), stores the token in its database, and hands it to `git` inside the workspace through `GIT_ASKPASS`, matched by host ([external-auth](https://coder.com/docs/admin/external-auth)). Daytona passes caller values into a sandbox as environment variables at creation. An earlier draft of this decision had the platform manage Repositories, clone on the host and execute every push host-side so that no token ever entered the Sandbox; it was dropped as too heavy for what it bought.

## Decision

**Git Providers.** Deployment configuration lists Git Providers, each with an id, a type (`github`, `gitlab`, `gitea`, `generic`), an OAuth client id, a client secret held as a deployment-time file reference (ADR-0010), endpoint URLs (required for self-hosted instances), a host pattern and scopes. Several may coexist, for example github.com, gitlab.com and an internal GitLab. For GitHub both OAuth Apps and GitHub Apps are accepted; the documentation recommends a GitHub App, whose user tokens expire with a refresh token and reach only the repositories it is installed on.

**Git Account Links.** A User links an account by running the provider's OAuth authorization-code flow with PKCE against `/external-auth/<id>/callback`. The access and refresh tokens are stored encrypted in PostgreSQL, one link per User and provider. Unlinking deletes them and calls the provider's revoke endpoint where one exists. There is no personal-access-token path for Git; a PAT is an ordinary Secret.

**In the Sandbox.** The Node Agent sets `credential.helper` (after resetting the image's helper list) to a platform helper mounted next to the Capability Channel socket. On `get`, the helper calls the channel verb `git.credential {host}`; the Control Plane matches the host against the provider patterns, refreshes the owner's token if it has expired, writes an Audit Record and returns it. `store` and `erase` do nothing, so git never persists the token. With no linked account the request fails with the authorization URL, which the Agent can pass to its User; with more than one matching provider it fails rather than guessing. The same verb backs `stars-sandbox token <provider>`, so `GH_TOKEN=$(stars-sandbox token github) gh pr create` works. Git itself runs in the Sandbox and reaches the remote under the Sandbox's Egress Policy; a self-hosted instance on a private address needs an Egress Opening.

**Caller Secrets.** A Secret is a User-owned Control Plane resource with a name and a write-only value, stored encrypted in PostgreSQL. At Sandbox creation the caller maps environment variable names to Secret names and the values are injected then; later changes to a Secret reach only new Sandboxes. Phase 1 has no file delivery and no in-place refresh. The platform never writes a Secret into the Workspace.

**Audit.** Secret create, update and delete; the Secret names a Sandbox was created with; every `git.credential` issue, with host and Sandbox. Never a value.

## Consequences

- Credentials do enter the Sandbox: any process in it can call the helper and read the owner's token, which reaches every repository the token reaches. What bounds this is the provider's own scoping (a GitHub App installation, OAuth scopes), the Egress Policy, and the audit trail, not the Sandbox boundary.
- Revoking a link stops future issues; a token already read in a running Sandbox lives until it expires upstream.
- The Capability Channel's Phase 1 verb list is exactly `git.credential`, and it is the channel's one credential-carrying verb.
- There is no platform Repository resource, no platform clone and no platform push; cloning, pushing and pull requests are the Agent's own `git`/`gh` commands.
- Git over SSH is not supported; HTTPS only.

## Rejected alternatives

- Host-side execution through a `git-remote-stars` remote helper, keeping tokens out of the Sandbox: no vendor precedent for the combination, breaks submodules and LFS, confines repositories to `/workspace`, and costs a platform-managed Repository model.
- Egress-proxy placeholder substitution: TLS interception and a per-Sandbox CA, for the same transparency.
- Requiring short-lived Git credentials: GitLab and Gitea cannot issue them.
- Personal access tokens as the Git path: users would paste long-lived tokens where an OAuth link with refresh exists.
