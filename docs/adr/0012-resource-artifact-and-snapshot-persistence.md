# ADR-0012: Resource, Artifact, and Snapshot persistence

- Status: accepted
- Date: 2026-10-17

## Context

`#36` locked how persisted content is versioned, collected, built and removed; this ADR records it as re-checked against vendor practice (`docs/research/persistence-vendor-practice.md`). Every surveyed sandbox vendor (Daytona, Modal, E2B, Blaxel, Vercel) ships mutable volumes for data, most with last-write-wins, and uses an immutable build plus a movable name pointer only for environment templates (E2B `name:tag → build_id`, Coder active version, OCI tag → digest). No sandbox vendor has an Artifact concept. The closest precedent for declared outputs collected into a content-addressed directory tree is Bazel REAPI. No vendor promises reproducible builds. Here the mutable, shared data store already exists: the node-bound, single-writer Workspace (`#32`).

## Decision

**Resource and ResourceVersion.** A Resource is a stable name with a current-version pointer; every content change publishes an immutable ResourceVersion. Data that must stay mutable lives in a Workspace, not a Resource. Updating the pointer accepts an optional `If-Match` expected version: with it the update is conditional and a mismatch is `conflict`; without it the latest publish wins.

**Content addressing.** SHA-256 over raw bytes for a file; a directory is addressed by a normalized manifest (sorted relative paths, file type, a fixed permission representation, sizes, content addresses, symlink targets). Timestamps, inodes and archive metadata are not content. tar/zip downloads are derived and never create a version. CAS objects may be shared across versions; ownership, provenance and retention stay per version.

**Upload.** The caller submits a manifest carrying each file's SHA-256. The Control Plane issues presigned PUTs only for objects not already stored, each carrying `x-amz-checksum-sha256` so the store verifies the digest on write. Publishing checks every object exists, then creates the ResourceVersion. The Control Plane never reads content and never trusts a client-declared digest unverified.

**Attachment to a Sandbox.** A Resource reference is resolved to a concrete ResourceVersion when the Sandbox is created. The Node Agent fetches it into a Node-local cache keyed by content address and mounts it read-only. A Sandbox never sees a version published after it was created; copying into the Workspace is the caller's choice.

**Artifact.** An Artifact is an explicit, caller-declared Exec output, collected by the Node Agent after the Exec reaches a terminal state and published as a ResourceVersion. Paths must stay within `/workspace`; traversal, device and special files, and external mounts are rejected; symlinks are recorded, never followed. There is no freeze barrier: the manifest is what was read. If another Exec writes the same Workspace during collection, consistency is not guaranteed, and the capability documentation says so.

**Snapshot and SnapshotVersion.** Separate from Resource. A SnapshotVersion is an OCI image digest in a platform-operated registry backed by the same object store; Nodes pull by digest. It is built as a `system`-priority job on a Node (`docker build`, then push), with lifecycle `building → ready | failed | cancelled`; only `ready` is runnable. The base must be on a platform-maintained list of base image digests, changed only by the platform and audited, like an Egress Opening. Inputs are declared: the base digest, build inputs, and explicitly referenced ResourceVersions; the Workspace is never a build context. Builder version and base digest are recorded as provenance, not as part of the address, and reproducibility is not promised. Build Secrets never enter the output, manifest or logs.

**Retention.** A current version never expires. A non-current ResourceVersion or SnapshotVersion that nothing references expires after a deployment-configured TTL. There is no keep-last-N and no idle deactivation. Artifacts follow the same rule.

**Deletion and GC.** Deleting a Resource writes a tombstone; a current version cannot be deleted directly; active references block physical cleanup. PostgreSQL metadata and references are authoritative. CAS objects, registry blobs and unpublished uploads are removed only after database visibility checks, a configurable grace period, and an idempotent, retryable sweep. A published version whose blob temporarily cannot be read becomes unavailable and is repaired, not deleted.

**Quota.** No storage quota in Phase 1; retention TTL and capacity monitoring bound growth.

## Consequences

- Phase 1 needs an OCI registry deployment on top of MinIO; its listening surface belongs with `#38`.
- Whether MinIO verifies `x-amz-checksum-sha256` on presigned PUTs must be tested before implementation; if not, the Upload paragraph needs a server-side hashing step.
- Agents running parallel Execs against one Workspace must not expect Artifact snapshots to be point-in-time consistent.
- Which phase ships Resource and Snapshot is `#39`'s call; this ADR fixes only their semantics.

## Rejected alternatives

- Mutable volumes for data (vendor norm): duplicates the Workspace with a second mutable store that, unlike the Workspace, has no single-writer rule.
- Mandatory conditional pointer updates: no vendor requires it, and simple callers gain nothing.
- A Workspace freeze barrier: no cheap freeze exists for an arbitrary Node-local directory on Docker or Local, and CI collects after exit without one.
- Policy version as a build input and a reproducibility promise: no vendor offers either, and nothing in the platform consumes them.
- Keep-last-N retention and idle Snapshot deactivation (Vercel, Daytona): unneeded at single-team scale.
- Following the Resource pointer live inside a running Sandbox: the Sandbox's inputs would change under it.
- A storage quota in Phase 1: added when one User fills the store.
