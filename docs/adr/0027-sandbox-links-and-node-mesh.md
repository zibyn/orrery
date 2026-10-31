# ADR-0027: Sandbox Links and the Node Mesh

- Status: accepted
- Date: 2026-10-31

## Context

Issue #183. Code an Agent runs in a Sandbox often needs a dependency service such as PostgreSQL or Redis, and usually discovers the need mid-session. The service should take its compute from its own request rather than from the Sandbox's, so that it can live on another Node when the Sandbox's Node is full or is a GPU Node. ADR-0010 said Sandboxes are never connected into, ADR-0026 said the platform adds no implicit egress entries, and Nodes had no network between them, so none of this was possible. Nodes may sit behind NAT, and traffic between Sandboxes must not be relayed through the Control Plane.

The vendor survey (`docs/research/on-demand-dependency-services-vendor-practice.md`) found two shapes for "the sandbox is already running and now wants a Postgres": Docker inside a sandbox that has its own kernel, or a sibling container on the same host started by an outside SDK (Modal Sandbox Sidecars, Daytona Linked sandboxes). Cross-host overlays (Fly 6PN, Modal i6pn, Tailscale) carry long-lived service links. Spike #183 (`docs/research/sandbox-link-node-mesh-spike.md`) checked the mesh and the rules on runc and runsc.

## Decision

**A dependency service is a Sandbox.** There is no Service resource. A PostgreSQL is a Sandbox created from a `postgres` Snapshot, running the server as a detached Exec, with its own compute request, admitted and counted against the `sandboxes` User Quota like any other. A running Exec keeps Idle Stop away. Its state, logs, audit, and MCP tools are the Sandbox's. GPU work likewise needs nothing new: the Agent commits and pushes with its own `git` (ADR-0013), creates a GPU Sandbox, and clones and runs there. Datasets and baselines are Resources, separate from code.

**Sandbox Link.** A Sandbox declares at creation which of its owner's Sandboxes may connect to it: `links: {from: [sandbox_id], ports?: [port]}`. Omitting `ports` allows every TCP and UDP port. The declaration is fixed for the Sandbox's life, like the Egress Mode. Every `from` Sandbox must exist, be the caller's own and use `runtime: container`. A Link is a permission on the target, not a connection. Traffic between a linked pair is not subject to the source's Egress Mode or the operator's Egress Rules; everything else either Sandbox sends still is. The Sandbox's address is returned as `address` and changes on every start. There is no DNS and no `/etc/hosts` entry: the caller passes the address to its code.

**Enforcement.** The Agent's `bridge` table matches the target's host-side veth by `oifname` in two hooks: `forward` for traffic from the same bridge, and `output` for traffic routed in from the mesh. Only linked source addresses on linked ports get through; everything else to the target is dropped, the Node's own processes included. In `forward`, every target jump precedes every source `iifname` jump, because an `accept` in a source's chain ends the base chain and would skip the target's check. For a cross-Node pair, the source's chain accepts the linked destination before its Egress Policy. The Control Plane pushes each Node the full set of Links with an end on it, and pushes it again whenever an end's address changes; until then the target rejects the new address.

**Node Mesh.** Each containerd Node runs one kernel WireGuard interface. The Agent creates and configures it itself over netlink: neither `wireguard-tools` nor `ip` is required. Peers are the other Nodes, each with `AllowedIPs` set to that Node's Sandbox subnet; the interface has no address of its own. The Agent generates its WireGuard key in its data root and reports the public key, its Sandbox subnet and an optional endpoint (`--mesh-endpoint host:port`) in `Hello`. The Control Plane only distributes peers. It never forwards traffic and there is no relay. A Node without an endpoint initiates to peers that have one and keeps the NAT mapping alive with keepalives, so a pair is connected whenever at least one side is reachable. Agents report each peer's latest handshake, and the Control Plane keeps that connectivity matrix in memory. Two Nodes whose Sandbox subnets overlap are never peered. The mesh carries only Sandbox Link traffic: the Agent drops anything else entering or leaving the WireGuard interface.

**Masquerade moves to the Agent.** CNI `ipMasq` masquerades every destination outside the Node's subnet, so cross-Node traffic left the mesh with the Node's address and WireGuard dropped it. CNI runs with `ipMasq: false`, and the Agent's own nftables table masquerades what leaves its Sandbox subnet except through the WireGuard interface.

**Placement.** When `links.from` is set, the Scheduler prefers the Node of the source Sandboxes. Otherwise it considers only Nodes with a live mesh path to every placed source's Node. A source still `pending` constrains nothing. There is no placement field.

**Capability.** `sandbox_links` is `enforced` only after the registration self-check confirms that a linked probe connects and an unlinked one does not. Local Runtime has no netns and reports `unsupported`. A Sandbox with `links` is placed only where it is enforced. The mesh is not a Capability: whether two Nodes connect is the matrix, and a Node whose kernel lacks WireGuard simply has no paths.

## Consequences

- ADR-0010's Ingress statement ("Sandboxes listen on no ports and are never connected into") now holds for everyone except a target's linked sources.
- ADR-0026's "the platform adds no implicit entries" now excepts linked pairs, and its CNI `ipMasq` is replaced by the Agent's masquerade.
- Sandbox subnets must not overlap for two Nodes to be peered. Overlap was already unsupported for Egress Rules; it now also silently disables cross-Node Links between those Nodes.
- Each Node with an endpoint needs one UDP port open to the other Nodes. Two Nodes both behind NAT with no endpoint never host a cross-Node pair; the operator fixes it by opening a port on one of them.
- A dependency Sandbox is not deleted with the Sandbox that uses it. The Agent deletes it, and Idle Stop stops it once its server Exec ends.
- Addresses change on restart, so callers re-read `address` after a target restarts.
- WireGuard adds 60–80 bytes of overhead. The spike only sent small packets, so path MTU through the mesh, and runsc's handling of ICMP fragmentation-needed in particular, still has to be shown before release. Clamping MSS on the WireGuard interface is the planned answer.

## Rejected alternatives

- A Sidecar sharing the Sandbox's netns, as in a Pod: it fixes the dependency at creation time. Under runsc a second container cannot share the Sentry's loopback without an unverified multi-container sandbox.
- Same-Node sibling containers added at runtime, the Modal Sidecars shape: they are bound to the Sandbox's Node, so they cannot relieve a full Node or a GPU Node.
- A Service resource under `/sandboxes/{id}/services`, with its own quota, state machine and logs: every part of it already exists on Sandbox.
- Docker inside the Sandbox: privileged or Sysbox on runc, and on runsc it needs Node-wide `--net-raw` and a tmpfs `/var/lib/docker`.
- A Job concept: queueing, run-to-completion and retry semantics nobody needs yet; the Agent orchestrates the steps itself.
- Resources as the transport for code: code already has git.
- A Tailscale client or tsnet per container, and relaying through the Control Plane in DERP style.
- Shelling out to `wg` and `ip`: an extra host package for configuration the Agent can write over netlink.
- Userspace `wireguard-go` on a TUN device: slower, and every kernel a containerd Node runs (5.6 and later) has WireGuard built in.
- Exempting mesh traffic from CNI's masquerade with an earlier nat chain: an `accept` there does not stop CNI's chain, as the spike showed.
- Sharing one dependency Sandbox beyond its declared `links.from`, or links across owners.
