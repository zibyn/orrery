# ADR-0018: Ephemeral Workspaces follow their Sandbox, and the Gateway reaches Nodes through reverse tunnels

- Status: accepted
- Date: 2026-10-17

## Context

`#42` locked the public API, the Node protocol and the schema (`docs/spec/`). Two of its decisions change the model rather than only spelling it out. `#32` started an ephemeral Workspace's grace window whenever it had no Attachment, while ADR-0017 releases a Sandbox's read-write Attachment on stop. Together, an Idle Stop followed by the grace window would delete an ephemeral Workspace that its Sandbox was still going to start again. Separately, the Gateway has to reach files, PTYs and ports on Nodes that dial out and may sit behind NAT.

## Decision

**Ephemeral Workspace.** An ephemeral Workspace is created implicitly when a Sandbox is created without a Workspace, lives through that Sandbox's stops and starts, and is deleted with it. There is no grace window. `POST /workspaces` creates only persistent Workspaces. Daytona's ephemeral storage is likewise bound to its sandbox.

**Reverse tunnels.** The Node protocol has three RPCs besides enrollment: one `Control` stream, one `ExecOutput` stream per Exec, and `Tunnel`, a raw byte stream the Agent dials back when `Control` asks it to. Through a tunnel the Gateway either reverse-proxies to the Agent's internal HTTP surface (file API, PTY WebSocket) or reaches a Sandbox port. Coder and Daytona alike run an HTTP surface in their agent or daemon and proxy to it.

**Files go through a ready Sandbox.** The file API lives under `/sandboxes/{id}/files`, touches only the Workspace, needs the Sandbox `ready`, and writes only through a read-write Attachment, as in both vendors. `#32`'s rule that every file mutation goes through the write Lease stands.

## Consequences

- Stopping a Sandbox never loses an ephemeral Workspace; deleting it always does.
- Adding a data-plane operation adds an Agent HTTP handler, not a Protobuf message.
- Reading results out of a stopped GPU Sandbox means starting it, which goes through admission.

## Rejected alternatives

- Keeping the grace window: a timer whose only job, collecting orphan ephemeral Workspaces, has nothing left to collect.
- Host-side file access on a Workspace with no running Sandbox: neither vendor offers it, and it would bypass the single-writer rule.
- A Protobuf message per data-plane operation: every file verb and PTY control written twice, once in REST and once in Protobuf.
