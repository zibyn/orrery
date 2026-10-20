# ADR-0016: Open egress, after-commit audit, and a Sandbox Token

- Status: accepted; Sandbox Token storage amended in place by #72
- Date: 2026-10-17

## Context

The recheck against Coder v2.38.0 and Daytona v0.190.0 (`docs/research/recheck-identity-network-channel-vs-coder-daytona.md`) kept three decisions only because they were stricter than both vendors: per-Sandbox egress rules in the netns, audit in the same transaction as the state, and a credential-free unix-socket Capability Channel. Stricter is not a reason. Coder ships no egress control by default, writes audit after the fact and only logs a failed write, and lets the workspace agent reach `coderd` over HTTPS with `CODER_AGENT_TOKEN` in its environment. The channel's "credential-free" property was already moot once ADR-0013 let Git tokens and Secrets into the Sandbox.

## Decision

**Egress.** The platform does not control or log Sandbox egress. There is no Egress Policy, no Egress Opening, no netns rule set and no subtracted address set. A Sandbox reaches whatever the Node's network reaches. Protecting PostgreSQL, MinIO, the registry and cloud metadata from Sandboxes belongs to the deployment's own network layout and credentials, and the deployment guide says so.

**Audit.** An Audit Record is inserted after the transaction that made the change commits. A failed insert is logged and never fails the request. The table stays in PostgreSQL. It records control-plane mutations, Node enroll and revoke, Secret changes, deletions and Git token issues; it does not record Gateway or per-request authorizations. Retention is a deployment-configured period; nothing pins an Audit Record to a live object.

**Sandbox Token.** In place of the Capability Channel, each Sandbox gets a Sandbox Token at creation, placed in the environment of each Exec. It authenticates to the `api` listener and can do exactly one thing: fetch a Git token for the Sandbox owner's linked account (ADR-0013). It is stored encrypted, delivered with every Exec, and becomes invalid when the Sandbox is deleted. (Amended in place by #72: it was stored hashed and delivered in `CreateSandbox`, so neither a restarted Agent nor a re-issued `CreateSandbox` could recover the plaintext ADR-0017 injects into each Exec.) The Git credential helper and `orrery token <provider>` call `api` with it over HTTPS.

## Consequences

- ADR-0011 is superseded; the Capability Channel, its socket mount and its `capability_channel` capability are gone.
- The Sandbox must reach `api` over the network; a deployment that hides `api` from Sandboxes loses Git authentication.
- A crash between commit and audit insert loses that Audit Record.
- Any code in a Sandbox can reach anything the Node can reach, unless the deployment network prevents it.

## Rejected alternatives

- A fixed Node-level deny list for PostgreSQL, MinIO, the registry and metadata: still platform-owned network rules, and the deployment is better placed to isolate its own services.
- Logging each Sandbox's outbound connections: a new mechanism with no consumer yet.
- Stdout-only audit: who deleted what must be queryable in the platform, as in Coder.
- Keeping the unix-socket channel: more moving parts than one narrowly scoped token, for a property ADR-0013 already gave up.
