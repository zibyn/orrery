# ADR-0030: Resources are mounted lazily through a Node FUSE daemon, and large files are stored as 4 MiB blocks

- Status: accepted (2026-11-13, after spike 1 `docs/research/resource-lazy-read-spike.md` and spike 2 `docs/research/resource-fuse-handover-spike.md`); amends ADR-0012's "Attachment to a Sandbox" and "Content addressing" and ADR-0017's single-file limit in place; `runtime.md` §11, `node-protocol.md`, `api.md` and `defaults.md` are to follow
- Date: 2026-11-13

## Context

A Sandbox today waits until every object of every Resource it mounts is on the Node: the Agent reads the manifest, presigns all missing objects in one gRPC `Presign`, downloads them one by one, hard-links a version tree and bind-mounts it read-only (`runtime.md` §11, `internal/agent/resources.go`). For a training dataset of 300 GB in about 1,000,000 files that is a long `fetching_resources`, and the code has three limits besides: one `Presign` reply over gRPC's default 4 MiB receive limit fails beyond about 9,400 objects (a presigned GET is 443 bytes), sequential downloads outlive the one-hour URL lifetime, and `resourceTreeMu` is held for the whole download, blocking other fetches and eviction on the Node.

Research (`docs/research/code-vs-training-data-vendor-practice.md`) found that training platforms lazily load large datasets from object storage (SageMaker FastFile, Vertex `/gcs`, Modal Volumes, JuiceFS CSI, Fluid) and that none puts code or Git on such a mount; the WSFS spike (`docs/research/workspace-wsfs-fuse-spike.md`) measured FUSE writes 2.5–16x slower than local disk but read-only `git fsck` at parity. `docs/research/resource-object-layout.md` found that today's one-object-per-file layout cannot hold files over 5 GB under the rule that the Control Plane never trusts an unverified client digest (a single PUT is capped at 5 GB, and SHA-256 under multipart is composite only), and that a ranged read of a whole-file object cannot be verified on Garage v2.4.1 or SeaweedFS. A RustFS 1.0.2 benchmark on 2026-11-13 found ranged reads inside a large object no faster per request than small objects (9,469 vs 11,605 GET/s at 64-way concurrency, loopback), so packing small files buys request count, not read speed.

The decisions were grilled on 2026-11-13; Q-numbers refer to that session.

## Decision

**Every Resource is mounted lazily (Q1, Q4).** The eager path, its version-tree assembly and the `fetching_resources` phase and condition are removed (Q6). `CreateSandbox` still fetches the manifest, so an unreadable manifest still ends the create with `resource_fetch_failed`; objects are fetched when read. This holds on runc, runsc and Local Nodes alike; runsc reads through its gofer and is slower.

**Large files are stored as 4 MiB blocks (Q2, Q13, Q14).** A file of at most 4 MiB is one object whose key and digest are unchanged from today. A larger file is a sequence of 4 MiB blocks, each a CAS object `cas/<owner_id>/sha256/<block digest>` uploaded by presigned PUT with `x-amz-checksum-sha256`; its manifest entry lists the block digests in order, and its address is computed by the Control Plane from block digests it verified with `HeadObject` in checksum mode. Owner-scoped deduplication (ADR-0021) applies per block. The single-file limit becomes 1 TiB, bounded by the 256 MiB manifest. Existing versions are not migrated; their files over 4 MiB are read by ranged GET of the whole-file object without per-block verification. Small-file packing is deferred until measured.

**One FUSE daemon per Node, apart from the Agent (Q3, Q15, Q19).** `orrery-resourcefs` is a systemd service in the Agent's bundle (ADR-0028), installed by `install.sh`. It mounts each ResourceVersion once at `<data-root>/resources/<address>`; the Agent bind-mounts that read-only at `/resources/<mount>` in every Sandbox that uses it and unmounts when the last referencing Sandbox, stopped ones included, is deleted (Q17). An upgrade hands the `/dev/fuse` descriptors from the old process to the new one, as JuiceFS v1.2's smooth upgrade does. Spike 2 made this work only because the daemon's state is a function of the manifest: it implements `fuse.RawFileSystem` with node IDs given by sorted manifest path and stateless handles, and needs a go-fuse patch (upstreamed or forked) to serve an already initialized session and to stop readers after their current request; a crashed daemon is restarted by systemd and its mounts stay broken until their Sandboxes restart. The Agent reports `resource_mount` from a self-check against the daemon.

**Reads (Q7, Q9, Q11).** The unit of fetch, cache and verification is a block; a block is checked against its SHA-256 before it is served. A GET has a 60-second timeout and is retried 10 times before the read fails with `EIO`, as JuiceFS's `--get-timeout` and `--io-retries` defaults; a digest mismatch counts as a failed attempt. Attribute, entry and page caches never expire, because a version never changes. A file read sequentially prefetches its next four blocks (16 MiB): spike 1 measured a single cold stream at 228 MiB/s with one block ahead and 585 MiB/s with four, against 739 MiB/s from local disk (`docs/research/resource-lazy-read-spike.md`); JuiceFS prefetches one block but backs it with a 300 MiB read buffer. Downloads are capped at 200 per Node. Both are constants. There is no warm-up API (Q12).

**Credentials stay presigned (Q5, Q16, Q18).** The daemon asks the Agent for presigned GETs over a local unix socket, on first access, in batches of a file's or directory's following blocks, and caches them until they expire. The Node still never holds object-store credentials.

**Cache (Q10).** Blocks are cached on the Node keyed by digest and evicted by LRU under the Node's existing disk watermark, together with images and the BuildKit cache. A block can be evicted while its version is mounted, since it can be fetched again.

**Builds (Q8).** A Snapshot build's named contexts are read from the same mounts.

## Consequences

- A Sandbox with a large Resource starts as soon as its manifest is read; a training run's first epoch pulls the data, and later epochs do too when the Node's cache cannot hold the dataset.
- Object-store failures surface during a run as `EIO` after about ten minutes of retries at worst, not as a failed create.
- `fetching_resources` disappears from the API's Sandbox conditions and the Node protocol's `SandboxObserved` phases; a client that waited on it sees `running` directly.
- The daemon is a new process whose death breaks every Resource mount on its Node until the Sandboxes restart.
- Upload changes for the API, the CLI and the Agent's Workspace publish: a large file is uploaded as blocks. Workspace Generations (`docs/research/workspace-shared-storage-s3.md`) will use the same layout (Q23).
- A version mounted lazily depends on the object store for as long as it is read, not only at create.

## Rejected alternatives

- Keeping eager fetch for some mounts or Nodes: both paths would stay, and the caller would choose for no gain the spike could show.
- Ranged reads of whole-file objects for new versions: unverifiable on two of the three stores, and files over 5 GB cannot be stored.
- Multipart objects with 16 MiB parts and a composite checksum: fewer objects, but a 16 MiB verification unit and composite formulas not yet confirmed equal across RustFS and SeaweedFS (`resource-object-layout.md` B2). Revisit if the cache unit grows or object count becomes a problem.
- Content-defined chunking or small-file packs: offsets in a pack are client-declared and deduplication would rest on them, and GC needs repacking.
- Short-lived object-store credentials on the Node: unnecessary while batched presigning suffices.
- The FUSE server in the Agent process: an Agent upgrade would break every mount.
