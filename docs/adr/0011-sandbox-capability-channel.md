# ADR-0011: The Sandbox Capability Channel

- Status: superseded by ADR-0016
- Date: 2026-10-17

## Context

ADR-0010 settled that a Sandbox reaches platform capabilities through a credential-free Capability Channel, the one named exception to ADR-0009's "no arbitrary host mounts", and that source-address authentication is not a mechanism. It left open who listens, how a request is authenticated and authorized, what the transport is scoped to, and what it carries. The closest vendor precedent is Fly.io's `/.fly/api` unix socket inside every Machine, which is authenticated by being reachable at all and which mints Machine-scoped OIDC tokens ([OpenID Connect](https://fly.io/docs/security/openid-connect/)). Argo artifact plugins and the SPIFFE Workload API use the same shape (a fixed-path unix socket). The previous design round used a bind-mounted unix socket served by the owning worker, with exactly `{fetch, push}` as verbs.

## Decision

**Verbs.** The channel accepts a closed enumeration of verbs. Each verb's semantics belong to the decision that defines it; this ADR defines only transport, identity and the authorization frame. The list is exactly `git.credential`, defined by ADR-0013; this ADR adds none. Adding a verb is a new decision. File operations stay on the Data Plane, Artifacts are declared on the Exec and collected by the Node Agent, and Resources are attached at creation, so none of them is a channel verb.

**Host side.** The owning Node Agent listens and forwards over its existing outbound mTLS connection to the Control Plane. A socket on the Node's local filesystem is not a connection into the Node.

**Transport and identity.** One unix socket per Sandbox, bind-mounted at a fixed path whose location is also given in an environment variable. The Node Agent maps socket to Sandbox; the identity of a request is the Sandbox whose socket it arrived on. That is mount-namespace identity, not source address: every process in a netns dials the same address, but only that container sees that socket. Requests are not attributed to an Exec (no peer-credential attestation): the Sandbox is the trust unit.

**Wire protocol.** HTTP/1.1 with JSON bodies, reusing the REST error model of `#31`, so `curl --unix-socket` is a sufficient client.

**Authorization.** The Control Plane decides every request: the Sandbox is `ready`, the verb is on the list, and the target lies in the Sandbox's scope (such as a Git account linked by the Sandbox's owner). The decision is written as an Audit Record in the same transaction, as for Gateway authorizations. A verb that mutates the Workspace additionally requires the Sandbox to hold the current read-write Attachment, whose fencing token the Node Agent checks locally at execution (`#32`). An unreachable Control Plane fails the request closed with `unavailable`; there is no cached authorization.

**Fencing and teardown.** When the read-write Attachment is fenced, Workspace-mutating verbs are refused with `conflict` and in-flight ones are terminated before the next writer is admitted, as `#32` does for write-dependent processes; read-only verbs continue. When the Sandbox enters `stopping` or `deleting`, new requests are refused, in-flight ones are cancelled, and the socket goes with the Runtime. A Node Agent restart rebuilds the socket mapping and fails in-flight requests. Channel requests are not durable intents: no Reconciler, no automatic retry.

**Egress.** The channel does not use the network stack, so it is reachable under every Egress Policy including `none`. Egress Policy governs the Sandbox's network only; channel verbs are governed by their own authorization and reach only platform-registered targets.

**Capability.** `capability_channel`: `enforced` on Docker and Podman, `best_effort` on Local Runtime, where the socket is a host file any host process can open and so proves nothing. It is always mounted where enforced; there is no opt-out, since an empty verb list is already the narrowest surface.

**Secrets.** The only credential on the channel is the Git account token returned by `git.credential` (ADR-0013). Other caller Secrets reach a Sandbox as environment variables set at creation.

## Consequences

- An audit fact for a channel request names the Sandbox and its owner, never the Exec that issued it.
- Every channel call costs a Control Plane round trip and fails while the Control Plane is unreachable; Agent-facing documentation must say so, so it is not mistaken for network flakiness.
- ADR-0009's capability table gains a `capability_channel` row.

## Rejected alternatives

- Mirroring a subset of the public REST API on the socket, Fly-style: the Sandbox would act with its owner's authority, so prompt-injected code could delete Workspaces or create Sandboxes.
- The Gateway or a separate process as host side: the Gateway is caller ingress and the channel never crosses the network; a separate process adds a deployable without adding a boundary.
- Per-Exec sockets: container mounts are fixed at creation.
- Peer-credential attribution to an Exec: precision the trust model does not use.
- Cached authorization on the Node Agent: it would outlive revocation.
- gRPC on the socket: needs a generated client inside every image.
- Delivering or refreshing general Secrets as a channel verb: environment variables at creation suffice (ADR-0013).
