# ADR-0010: Identity, secrets, network, and audit boundaries

- Status: accepted; Capability Channel, Egress and Audit superseded by ADR-0016; Node identity, Ingress, Deletion and caller tokens amended by ADR-0017; caller identity amended by ADR-0020 (password and OIDC login)
- Date: 2026-10-17

## Context

The platform has to answer who a caller is, how a Node becomes trusted, where the platform's own long-lived secrets live, whether a caller's secrets may enter a Sandbox, what the egress default is, and what is recorded. Vendor practice is uniform and points the other way on several of these: egress is open by default (12 of 14 surveyed products), credentials are delivered into the execution side (DevPod, Tekton, OpenHands, `sandboxd`), and Node agents authenticate with a bearer token over TLS (GitHub, GitLab, Coder). This is a single-team, single-control-plane, NAT-deployable deployment, which changes which of those answers are affordable.

## Decision

**Caller identity.** Per-User static API tokens over TLS, stored hashed in PostgreSQL, scoped `user` or `admin`. No OIDC. There is no role model beyond ownership plus `admin`; `admin` must not be the credential an Agent holds, because enrolling a Node mints a new Node identity.

**Node identity.** Private-CA-issued client certificates. The Node generates its own keypair (the Control Plane never sees a private key) and redeems a single-use, short-lived Enrollment Packet for one CSR-signed certificate; renewal happens at 50% of remaining life through an RPC separate from enrollment. Revocation is `delete the Node row and refuse renewal`, with no CRL or OCSP. The failure semantics are asymmetric on purpose: `not_found`/`unauthenticated` clears local identity, while a transport failure preserves it and backs off, so a Control Plane restart cannot make a fleet clear its own identity.

**Platform secrets.** Object-store keys, Git provider minting credentials, the CA private key and database credentials are deployment-time file path references. They do not live in PostgreSQL and are not reachable through the API, so they stay out of backups, the query surface, and the question of who may read a table.

**Caller secrets.** A caller's secrets and Git account tokens may enter a Sandbox, as Coder and Daytona allow; how is ADR-0013. The platform never writes them inside the Workspace. Egress-proxy placeholder substitution is the documented upgrade path, not the mechanism.

**Capability Channel.** The Sandbox reaches platform capabilities through a platform-managed channel, which is a named exception to the Runtime Provider contract's "no arbitrary host mounts". It carries declared verbs and targets and is never a general command executor; its only credential-carrying verb is ADR-0013's `git.credential`. Source-address authentication is not a mechanism: nothing inside a Sandbox can be authenticated by where it connects from.

**Egress.** Default `public`: public addresses reachable, while private ranges, CGNAT, loopback, link-local, cloud metadata and host addresses are unconditionally denied, and Control Plane, Gateway and session addresses are unconditionally subtracted from whatever is reachable. The policy is a property of the Sandbox, declared when it is created, not a property of the Workspace. A caller's `allow: [CIDR]` is a request: a platform-maintained Egress Opening list decides whether it may land on a denied destination, and a request that cannot land fails loudly at creation.

**Ingress.** Callers enter only through the Control Plane and the Gateway. Sandboxes listen on no ports and are never connected into. The cost is honest and documented: a service running inside a Sandbox is not reachable from outside.

**Deletion.** Ownership is the authorization: a User may delete what it owns, and `admin` is the only path that bypasses that. A deletion that would collide with a live Attachment or an unfinished Exec returns a conflict instead of implicitly cancelling. Deletion is logical before physical and leaves an audit fact.

**Audit.** An append-only table in PostgreSQL, in the same transaction as the state it describes, so audit and state cannot diverge in a crash. It records control-plane mutations, Lease mint and revoke, Node enroll/renew/revoke, egress policy and Egress Opening changes, deletions, and every Gateway authorization decision. It does not record per-request data-plane reads and writes, nor Sandbox output, which is the Execution Log with its own retention.

## Consequences

- The previous design round's "no credentials inside the Sandbox" property is deliberately given up for *caller* secrets. What replaces it is provider scoping and audit (ADR-0013). The counter-precedent still governs *platform* credentials, which never enter a Sandbox at all.
- Because the Capability Channel is a host-side mount, the Runtime Provider contract needs one named exception and a Capability that reports it honestly as `enforced` or `unsupported`.
- Ownership-only deletion means there is no delegation between Users; every shared-resource question is answered by `admin` or not at all.
- Because audit shares the control-plane database, there is no second source of truth to reconcile, and no separate audit store to keep alive.

## Rejected alternatives

- OIDC or an external IdP: an issuer and a token-exchange flow for zero benefit at one team, with no user model to federate.
- Shared registration tokens for Nodes: GitLab's own architecture blueprint calls a shared token untraceable ("no possibility of knowing the source of a leaked token").
- Source-address authentication for the Capability Channel or the Gateway.
- Egress-proxy placeholder substitution with a per-Sandbox CA in v1: TLS interception, a CA delivered into every Sandbox, and response scrubbing, for the benefit of vendors who must serve arbitrary tooling.
- KMS/Vault, or platform secrets stored in the database.
- A separate audit store on the Tekton Results model.
