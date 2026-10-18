# ADR-0012: Resource, Artifact, and Snapshot persistence

- Status: accepted; Artifact, Snapshot base, retention and object store amended by ADR-0017; content addressing, upload, Workspace publish, Snapshot pointer and import, deletion and the checksum consequence amended in place by #82 (ADR-0021); build mechanism amended in place by #92
- Date: 2026-10-17

## Context

`#36` locked how persisted content is versioned, collected, built and removed; this ADR records it as re-checked against vendor practice (`docs/research/persistence-vendor-practice.md`). Every surveyed sandbox vendor (Daytona, Modal, E2B, Blaxel, Vercel) ships mutable volumes for data, most with last-write-wins, and uses an immutable build plus a movable name pointer only for environment templates (E2B `name:tag → build_id`, Coder active version, OCI tag → digest). No sandbox vendor has an Artifact concept. The closest precedent for declared outputs collected into a content-addressed directory tree is Bazel REAPI. No vendor promises reproducible builds. Here the mutable, shared data store already exists: the node-bound, single-writer Workspace (`#32`).

## Decision

**Resource and ResourceVersion.** A Resource is a stable name with a current-version pointer; every content change publishes an immutable ResourceVersion. Data that must stay mutable lives in a Workspace, not a Resource. Updating the pointer accepts an optional `If-Match` expected version: with it the update is conditional and a mismatch is `conflict`; without it the latest publish wins.

**Content addressing.** SHA-256 over raw bytes for a file. Every ResourceVersion is a directory tree; publishing a single file yields a tree with that one entry. The tree is addressed by the SHA-256 of a manifest the Control Plane normalizes; the client only computes each file's SHA-256. Entries are `file`, `dir` or `symlink`, with sorted relative paths, sizes, content addresses and symlink targets; the only permission recorded is the executable bit (644 or 755); empty directories are kept. A manifest holds at most 1,000,000 entries. Timestamps, inodes and archive metadata are not content. tar/zip downloads are derived by the CLI and never create a version. CAS objects are deduplicated per owner, under `cas/<owner_id>/sha256/<hex>`, and may be shared across that owner's versions; ownership, provenance and retention stay per version. (Amended in place by #82: the address was left to a client-built manifest, and objects were shared across owners, which ADR-0021 rejects.)

**Upload.** The caller submits a manifest carrying each file's SHA-256. The Control Plane issues presigned PUTs only for objects not already stored, each carrying `x-amz-checksum-sha256` so the store verifies the digest on write. Submitting the same manifest again resumes the upload: PUTs are issued again only for objects still missing, until the upload session expires. Publishing checks every object with `HeadObject` in checksum mode, so each must exist with the declared SHA-256, then creates the ResourceVersion. Downloads go the same way in reverse: the caller reads the manifest and asks for presigned GETs. The Control Plane never reads content and never trusts a client-declared digest unverified. (Amended in place by #82: resume, the checksum-mode check and manifest-based downloads replace a derived archive endpoint that had the Control Plane read content.)

**Attachment to a Sandbox.** A Resource reference is resolved to a concrete ResourceVersion when the Sandbox is created. The Node Agent fetches it into a Node-local cache keyed by content address and mounts it read-only. A Sandbox never sees a version published after it was created; copying into the Workspace is the caller's choice.

**Publishing from a Workspace.** There is no Artifact concept (ADR-0017). `publish(path → Resource)` on a Workspace creates a `pending` ResourceVersion; the Node Agent reads the Workspace directory on the host, submits its manifest and uploads the missing objects, and the version moves the Resource's current pointer when it becomes `ready` (the last to become ready wins). A failed publish leaves the pointer alone. (Amended in place by #82: the publish was synchronous.) Paths must stay within `/workspace`; traversal, device and special files, and external mounts are rejected; symlinks are recorded, never followed. There is no freeze barrier: the manifest is what was read. If an Exec writes the same path while it is published, consistency is not guaranteed, and the capability documentation says so.

**Snapshot and SnapshotVersion.** Separate from Resource. A Snapshot has a current-version pointer that moves when a version becomes `ready` and can be set back explicitly; referring to a Snapshot by name means its current version. A SnapshotVersion is an OCI image digest in a platform-operated registry backed by the same object store; Nodes pull by digest, and only from that registry (ADR-0021). It is built as a `system`-priority job on a Node (a BuildKit build, then push; ADR-0021), with lifecycle `building → ready | failed | cancelled`; only `ready` is runnable. Any `ref@sha256:` may be imported: the Control Plane copies the whole index into the platform registry, keeping its digest. Any base may be built from; platform images are recommended defaults, imported from deployment configuration and readable by every User. (Amended in place by #82: the pointer, registry-only pulls and whole-index import are new.) Inputs are declared: the base digest, build inputs, and explicitly referenced ResourceVersions; the Workspace is never a build context. Builder version and base digest are recorded as provenance, not as part of the address, and reproducibility is not promised. Build Secrets never enter the output, manifest or logs.

**Retention.** A current version never expires. A non-current ResourceVersion that nothing references expires after a deployment-configured TTL. A SnapshotVersion is deleted only explicitly; a stopped Sandbox counts as a reference. There is no keep-last-N and no idle deactivation.

**Deletion and GC.** Deleting a Resource or a Snapshot (the latter added in place by #82) writes a tombstone; a current version cannot be deleted directly; active references block physical cleanup. PostgreSQL metadata and references are authoritative. CAS objects and unpublished uploads are removed only after database visibility checks, a configurable grace period, and an idempotent, retryable sweep. A published version whose blob temporarily cannot be read becomes unavailable and is repaired, not deleted.

**Quota.** No storage quota; retention TTL and capacity monitoring bound growth.

## Consequences

- The platform needs an OCI registry on the S3-compatible object store (ADR-0014); registry blob GC is an operations procedure.
- Tested on 2026-10-17 (#82): RustFS 1.0.2, Garage v2.4.1 and SeaweedFS 4.48 all reject a presigned PUT whose bytes do not match the signed `x-amz-checksum-sha256`, or that omits the header, with 400 and no leftover object, and return the SHA-256 from `HeadObject` in checksum mode. No server-side hashing step is needed.
- Agents running parallel Execs against one Workspace must not expect a publish to be point-in-time consistent.

## Rejected alternatives

- Mutable volumes for data (vendor norm): duplicates the Workspace with a second mutable store that, unlike the Workspace, has no single-writer rule.
- Mandatory conditional pointer updates: no vendor requires it, and simple callers gain nothing.
- A Workspace freeze barrier: no cheap freeze exists for an arbitrary Node-local directory on Docker or Local, and CI collects after exit without one.
- Policy version as a build input and a reproducibility promise: no vendor offers either, and nothing in the platform consumes them.
- Keep-last-N retention and idle Snapshot deactivation (Vercel, Daytona): unneeded at single-team scale.
- Following the Resource pointer live inside a running Sandbox: the Sandbox's inputs would change under it.
- A storage quota: added when one User fills the store.
