# 运行中 Sandbox 的文件系统怎么持久化：Daytona、E2B 与可比系统

调研日期 **2026-11-19**。问题：`docs/spec/runtime.md` §16 写着「运行中不提交」，Sandbox 根文件系统（containerd upperdir 相对 SnapshotVersion 的差异，ADR-0031）只在 Runtime 结束后提交成 Generation，Node 一丢整段会话就没了。卷已经每 60 秒做一次时间点 seal（§17），但根文件系统不在我们的 FUSE 后面，冻结它要换一种机制。本文回答 Daytona 和 E2B 在沙箱**运行中**怎样持久化文件系统：何时触发、用什么冻结、停顿多久、丢多少、怎么恢复，再看几家可比系统只取冻结机制，最后列出 orrery 可选的做法。

只收一手来源：官方文档（含 `.md` 原文）、钉死版本的官方仓库源码、厂商工程师的官方博客。厂商原文保留英文。每条断言标 **[文档写了]**、**[源码读出]** 或 **[未证实]**。

| 项目 | 钉死 | 简写 |
|---|---|---|
| daytonaio/daytona（2026-06 起转私有，这是冻结前最后一版） | tag `v0.190.0`（commit `01c502bb1f1f`） | `D/` = `https://github.com/daytonaio/daytona/blob/v0.190.0/` |
| e2b-dev/infra | commit `ec06ca431328dd1a713d6e40fbe29ef7e4c4931a`（2026-11-18） | `E/` = `https://github.com/e2b-dev/infra/blob/ec06ca431328dd1a713d6e40fbe29ef7e4c4931a/` |
| google/gvisor | tag `release-20261026.0`（与 a800 上的 runsc 同版） | `G/` = `https://github.com/google/gvisor/blob/release-20261026.0/` |
| moby/moby | tag `v28.5.2` | `M/` = `https://github.com/moby/moby/blob/v28.5.2/` |

复用的既有调研，只给出处、不重复：`workspace-cross-node-and-git-storage-vendor-practice.md`（**WCN**，已写 E2B `SwapForBackgroundSeal`、块级懒加载、Modal Exit Snapshot、Fly 每日快照、Replit 的 btrfs 提交点、CodeSandbox CoW 快照）；`recheck-runtime-and-workspace-vs-coder-daytona.md`（**RRW** #5，Daytona 备份迁移）；`daytona-sandbox-runtime-semantics.md`（**DSR** §1.5，stop/pause/archive 各保住什么）；`persistence-vendor-practice.md`（**PVP**，E2B 暂停沙箱永不过期）；`runsc-rootfs-visibility-spike.md`（**RRV**，runsc 要 `overlay2 = "none"` 才能在宿主上按文件看到写入）。

> **取证强度**：没有做任何实测，两家都是托管服务，本机无凭据。Daytona 的源码是 2026-06 冻结的快照，线上代码已私有，**源码读出的行为不等于线上行为**；本文有一处源码与文档对不上（§1.3）。E2B 的源码是活仓库，但大量路径受 feature flag 控制（如 `DeferRootfsExportFlag`），**线上哪些分支打开了看不到**。

---

## 0. 先读这几条

1. **两家都没有「运行中按固定周期提交且保证一致」的机制。** Daytona 有运行中备份，但用 `docker commit` 且显式 `Pause: false`，不冻结（`D/apps/runner/pkg/docker/container_commit.go#L22-L25`）。按 `v0.190.0` 的状态机，一次启动里最多做一次（§1.2）。E2B 只在 pause（手动或超时自动）、createSnapshot、fork 时持久化，源码里没有周期 checkpoint（§2.1）。
2. **要一致，两家都靠先停住整个沙箱。** Daytona 的「从运行中沙箱建快照」先 `ContainerPause`（cgroup freezer）再 commit，commit 完就解冻，push 不在冻结窗口里（`D/apps/runner/pkg/docker/snapshot_sandbox.go#L42-L102`）。E2B 暂停 Firecracker VM。只存文件系统的快照还要在 guest 里先 `FIFREEZE`，因为 "FC never flushes the guest page cache"（`E/packages/orchestrator/pkg/sandbox/sandbox.go#L3065-L3071`）。
3. **停顿和什么成正比，两家差得很远。** Daytona 冻结期间要把整个读写层打成 tar（`M/daemon/images/image_commit.go#L22-L43`），停顿随全部差异增长，每次都是全量。E2B 只把 flush 加换一块空 COW 缓存留在关键路径上，其余都放到解冻之后：rootfs 在后台 reflink，内存走 CoW 窗口（§2.3）。文档给的数字只有一个：pause 约 **4 s / GiB 内存**，恢复约 1 s。
4. **没有一家给运行中沙箱写出 RPO。** Daytona runner 丢了，丢的是上次完成的备份之后的所有改动，备份本身还可能是撕裂的。E2B 丢的是上次 pause 或 snapshot 之后的所有改动，快照在上传完成前只存在节点上。Modal 和 Fly 都写明快照在硬件故障时不保证存在（§3.3、§3.4）。
5. **runsc 的宿主侧扫描有一个 runc 没有的坑。** 根挂载默认 `--file-access=exclusive`，sentry 可以缓存元数据和数据并异步写回（`G/pkg/sentry/fsimpl/gofer/gofer.go#L391-L406`）。`runsc pause` 只停任务，不刷缓存。`runsc checkpoint` 的 `PrepareSave` 会先 `fs.Sync`，但 `overlay2 = "none"` 时根文件系统的内容不进 checkpoint 镜像，仍然留在宿主 upperdir 里（§3.2）。

---

## 1. Daytona

### 1.1 三条写进 registry 的路径

| 路径 | 触发 | 冻结 | 产物 | 来源 |
|---|---|---|---|---|
| 运行中备份 | API cron 每 5 分钟挑 `STARTED`、上次备份超过 1 小时的沙箱 | **不冻结**（`Pause: false`） | `backup-<id>:<时间戳>` 镜像推到备份 registry | [源码读出] `D/apps/api/src/sandbox/managers/backup.manager.ts#L131-L164`、`#L632` |
| 停止后备份（含 archive） | cron 每 10 秒挑 `STOPPED`/`ARCHIVING` 且未备份的 | 容器已停，天然一致 | 同上 | [源码读出] `backup.manager.ts#L498-L520`；[文档写了] archive 的转移是 "The backup completes and the filesystem is moved to object storage"（<https://www.daytona.io/docs/en/sandboxes.md>） |
| 从沙箱建快照 | 用户 `POST /sandbox/{id}/snapshot` | 运行中的容器 `ContainerPause` 到 commit 结束 | `daytona-<hash>:daytona` 快照镜像 | [源码读出] `D/apps/runner/pkg/docker/snapshot_sandbox.go#L42-L102` |

三条都是 `docker commit` 加 `docker push`，区别只在冻不冻结。commit 失败、报 "failed to get digest" 时退到 `ContainerExport` + `ImageImport`（`D/apps/runner/pkg/docker/container_commit.go#L31-L42`），export 导出的是整个容器文件系统，连镜像层一起拍平 [源码读出]。

### 1.2 运行中备份：条件和实际频率

- **条件** [源码读出]：`adHocBackupCheck` 用 `@Cron(EVERY_5_MINUTES)`，每台 READY runner 每轮最多取 25 个沙箱，条件是 `state = STARTED`、`backupState IN (NONE, COMPLETED)`、`lastBackupAt` 为空或早于 1 小时、`autoDeleteInterval != 0`（排除 ephemeral），按 `lastBackupAt` 升序（`backup.manager.ts#L131-L164`）。手动的 `createBackup` 同样拒绝 ephemeral（`D/apps/api/src/sandbox/services/sandbox.service.ts#L1069-L1080`）。
- **一次启动里最多一次** [源码读出，未实测]：`setBackupPending` 第一行就是 `if (sandbox.backupState === BackupState.COMPLETED) return`（`backup.manager.ts#L598-L601`）。`COMPLETED` 只在启动时（`D/apps/api/src/sandbox/managers/sandbox-actions/sandbox-start.action.ts#L624-L636`）、停止时和错误重试时回到 `NONE`。所以第一次运行中备份完成后，后面每轮 cron 选中它也只会空转。手动 `createBackup` 走的是同一个函数，同样空转（`backup.manager.ts#L955-L959`）。RRW #5 写的「运行中每小时一次」是从 cron 条件读出的意图，按这份源码实际是「启动后、距上次备份满 1 小时时做一次」。线上代码已私有，可能已经修了。
- **并发和超时** [源码读出]：每台 runner 同时最多 6 个备份（`MAX_CONCURRENT_BACKUPS_PER_RUNNER` 默认 6，`D/apps/api/src/config/configuration.ts#L160`；检查在 `backup.manager.ts#L806-L819`）；runner 侧整个 commit+push 的超时默认 60 分钟（`D/apps/runner/cmd/runner/config/config.go#L59`）；API 把进行超过 2 小时的备份标为错误（`backup.manager.ts#L546-L586`）。
- **文档**：沙箱、持久化、快照三页都没有写运行中备份，也没有给沙箱数据的 RPO [文档写了：未提及]。`security-exhibit` 里的 RPO/RTO 说的是 "Critical Daytona platform data (control plane, configuration, metadata)"（`D/apps/docs/src/content/docs/en/security-exhibit.mdx#L56-L58`），不是沙箱文件系统。

### 1.3 冻结：备份不冻结，快照只冻结 commit 那一段

- **备份** [源码读出]：`ContainerCommit(..., container.CommitOptions{Reference: imageName, Pause: false})`（`container_commit.go#L22-L25`）。Docker 的默认是冻结，文档给的理由是 "By default, the container being committed and its processes will be paused while the image is committed. This reduces the likelihood of encountering data corruption during the process of creating the commit."（<https://docs.docker.com/reference/cli/docker/container/commit/>）[文档写了]。Daytona 关掉了这一项，所以运行中备份是一次**不原子的在线拷贝**：tar 走读写层的过程中容器继续写，跨文件不一致，单个文件也可能被读到一半 [推断，未证实]。
- **快照** [源码读出]：先 `ContainerPause`，commit 到本地临时 tag，**马上** `ContainerUnpause`，之后才打 tag、`PushImage`。注释写的是 "Running containers are briefly paused during commit to produce a consistent on-disk snapshot."（`snapshot_sandbox.go#L42-L102`）。`docker pause` 用的是 freezer cgroup，进程感知不到（<https://docs.docker.com/reference/cli/docker/container/pause/>）[文档写了]。
- **文档与源码不一致**：线上快照页第一句是 "Create a snapshot from a running or stopped sandbox."，可容器类的表格里写着 "Cold … Filesystem only … Required sandbox state: **Stopped**"（<https://www.daytona.io/docs/en/snapshots.md> 「Create snapshot from sandbox」）[文档写了]。`v0.190.0` 的 API 对容器类接受 `STARTED` 或 `STOPPED`（`sandbox.service.ts#L1308-L1331`）[源码读出]。冻结之后线上是不是收紧成「只能停止后快照」，看不到 [未证实]。

### 1.4 时长与增量

- **commit 停顿和全部差异成正比** [源码读出]：moby 的 `CommitImage` 对读写层调 `rwlayer.TarStream()`，再 `layerStore.Register` 成新层（`M/daemon/images/image_commit.go#L22-L43`、`#L100`）。冻结发生在 `CreateImageFromContainer` 开头，持续整个 commit（`M/daemon/commit.go#L147-L149`）。tar 的对象是这个容器创建以来的**整个**读写层，不是上次备份以来的增量，所以同一次运行里的第二次 commit 会把第一次的内容再打一遍。
- **push 只传顶层**：底下的镜像层（以及上一次备份恢复时作为基础的那一层）在 registry 里已经有了，按 registry 的常规行为不重传 [未证实，没读 push 代码]。
- **层会越叠越多**：从备份恢复的容器以备份镜像为基础，下一次备份在它上面再加一层。源码里没找到拍平或压层的逻辑（在 `apps/runner`、`apps/api/src` 里搜 `squash`/`flatten` 无结果）[源码读出：未找到]。

### 1.5 丢失与恢复

- **只有备份完成才能换 runner** [源码读出]：runner 不可调度、不是 READY 或得分过低，并且 `backupState === COMPLETED`，下次 start 就把 `runnerId` 清空，到新 runner 上从备份恢复；没有完成的备份就报 `ERROR`，"Sandbox has no runner and backup is not completed"（`sandbox-start.action.ts#L473-L541`）。每次完成的备份都追加进 `existingBackupSnapshots`，恢复时扫这个列表（`D/apps/api/src/sandbox/entities/sandbox.entity.ts#L271-L291`）。
- **恢复是整镜像拉取** [源码读出]：`ImagePull`（`D/apps/runner/pkg/docker/image_pull.go#L22-L45`），不是懒加载。`estargz` 只是 `go.mod` 里的间接依赖（`D/apps/runner/go.mod#L57`）。
- **丢多少**：上次 `COMPLETED` 备份之后的一切。按 §1.2 的源码，一次长时间运行只在大约 1 小时那个点留下一份备份，而且这份可能是撕裂的 [源码读出 + 推断]。

### 1.6 VM 类（Linux VM / Windows）

- [文档写了] pause "freezes the VM with filesystem and memory intact"，状态 "offloaded to nearby storage"；热快照 `includeMemory: true` 要求沙箱在 `STARTED` 状态（<https://www.daytona.io/docs/en/persistence.md>、<https://www.daytona.io/docs/en/snapshots.md>）。没有暂停时长、增量或故障语义的描述。
- [源码读出] 公开仓库里没有 VM runner：v2 executor 的 `pauseSandbox` 直接返回 "pause is not supported for sandbox type"（`D/apps/runner/pkg/runner/v2/executor/sandbox.go#L105-L107`），`v0.190.0` 只对 Windows 接受 `includeMemory`（`sandbox.service.ts#L1323-L1331`）。VM 类的冻结机制 **[未证实]**。

---

## 2. E2B

### 2.1 何时持久化

- [文档写了] 三个入口：`pause()`（也可以由超时触发 auto-pause）、`createSnapshot()`、fork。pause 默认保存文件系统和内存；`mode: 'filesystem'` 只存磁盘（<https://docs.e2b.dev/sandbox/persistence.md>、<https://docs.e2b.dev/sandbox/filesystem-only-snapshots.md>）。文档没有任何周期 checkpoint。
- [源码读出] 编排器的 `Checkpoint` RPC（在原地拍快照、沙箱继续跑）只有两个调用方：fork（`E/packages/api/internal/handlers/sandbox_fork.go#L139` → `E/packages/api/internal/orchestrator/checkpoint_instance.go#L21-L26`）和快照模板（`E/packages/api/internal/orchestrator/snapshot_template.go#L108`）。在 `packages/` 里搜周期性 checkpoint 或 snapshot interval，没有结果 [源码读出：未找到]。
- [文档写了] 运行时间上限：Pro 24 小时、Hobby 1 小时，"After a sandbox is paused and resumed, the continuous runtime limit is reset"（persistence.md「Limitations」）。也就是说，E2B 的运行中会话本来就有上限，长会话靠用户自己 pause/resume 来切段。

### 2.2 冻结机制

- **VM 暂停是一致点** [源码读出]：`Pause` 的注释列了步骤："1. We pause FC VM 2. We call FC snapshot endpoint … this only creates the snapfile and drains and flushes the disk … 5. … we copy the pages directly from the FC process to a local cache. 6. We then can either close the sandbox or resume it."（`E/packages/orchestrator/pkg/sandbox/sandbox.go#L2900-L2913`）。vCPU 停住以后，内存和虚拟盘是同一时刻的，**含内存**。
- **只存文件系统时必须在 guest 里冻结** [源码读出]："FC never flushes the guest page cache and no memory snapshot will preserve it, so the rootfs must be quiesced before pause or it would persist missing acknowledged writes. This is mandatory"（`sandbox.go#L3065-L3071`）。做法是调 envd 的 `/fsfreeze`，也就是对根文件系统做 `FIFREEZE` ioctl（`E/packages/orchestrator/pkg/sandbox/reclaim.go#L198-L241`；`E/packages/envd/internal/services/fsfreeze/fsfreeze_linux.go#L13-L43`）。envd 低于 0.6.6 时退回到 guest 里一次 `sync`（`E/packages/shared/pkg/utils/version.go#L26-L29`），而 sync 和 pause 之间有竞态，这正是 `FIFREEZE` 要关上的窗口（`reclaim.go#L235-L237`）。guest 不提供冻结的，拒绝做 fs-only pause（`reclaim.go#L199-L204`）。文档对应的说法是 "capturing the filesystem needs `envd` to freeze it"，此时 pause 回 503（persistence.md）[文档写了]。
- **cgroup 冻结只是辅助** [源码读出]：pause 前有一步尽力而为的 guest 回收（fstrim、sync、drop_caches、compact），其中会冻结用户 cgroup，失败就解冻（`sandbox.go#L3037-L3046`；`version.go#L12-L16`）。一致性靠的是 VM 暂停，不是这一步。
- **结果是时间点一致的** [文档写了]：快照是 "a persistent point-in-time capture of a running sandbox, including both its filesystem and memory state"（<https://docs.e2b.dev/sandbox/snapshots.md>）。

### 2.3 停顿时长与增量

- [文档写了] "Pausing a sandbox takes approximately **4 seconds per 1 GiB of RAM**"，"Resuming a sandbox takes approximately **1 second**"（persistence.md「Limitations」）。fork 的停顿 "scales with the amount of disk changes since the last snapshot"（WCN §1.1 已引）。snapshot "briefly pauses, then continues running"，并且 "causes all active connections (e.g. WebSocket, PTY, command streams) to be dropped"（snapshots.md）。fs-only 快照 "smaller and faster to take"，但没有给数字（filesystem-only-snapshots.md）。
- [源码读出] **原地 checkpoint 把复制移出了冻结窗口**：`WithMaintainSandbox` 让沙箱进程在快照期间保持存活，之后原地恢复（`sandbox.go#L2835-L2842`）。指标 `guest_freeze.duration` 的定义是 "from the FC pause call to the in-place resume"（`sandbox.go#L3355-L3357`）。rootfs 侧用 `SwapForBackgroundSeal`：flush NBD 设备，换上一块空 COW 缓存，旧缓存冻结后在后台 reflink，"Only the device flush stays on the critical path"（`E/packages/orchestrator/pkg/sandbox/rootfs/nbd.go#L203-L241`，WCN 已引）。内存侧，脏页集合 "is fixed at pause, exactly like rootfs"，这些页先被写保护，VM 恢复后，guest 写到哪一页，就先把那一页的旧内容复制出来，其余页由后台 sweep 搬走（`sandbox.go#L3740-L3749`、`#L3401-L3403`）。这两条都受 feature flag 控制（`E/packages/orchestrator/pkg/server/sandboxes.go#L2404-L2408`），线上的开启比例看不到 [未证实]。
- [源码读出] **增量**：rootfs 按 4 KiB 块记脏块，内存按 4 KiB 页或 2 MiB 大页（`E/packages/shared/pkg/storage/header/diff.go#L10-L12`）。差异挂在父 build 的链上，只存本次的脏块（映射见 WCN §1.2）。

### 2.4 持久性与恢复

- [源码读出] **上传是异步的**：pause 返回之后，快照文件在后台上传，重试预算 2 小时（`uploadTotalBudget = 2 * time.Hour`，`E/packages/orchestrator/pkg/server/sandboxes.go#L80`、`#L3064-L3072`）。上传期间，其他节点可以从本节点的缓存拉这份快照（`advertised`，`sandboxes.go#L2866-L2867`）。这段时间节点坏了，快照会不会丢，源码里看不到兜底 [推断，未证实]。
- **运行中节点丢失**：文档没有写 [文档写了：未提及]。源码里没有周期 checkpoint，rootfs 的写缓存也在节点本地（`rootfs/nbd.go`），所以丢的是上次 pause 或 snapshot 之后的全部改动 [推断，未证实]。
- [文档写了] fs-only 快照恢复时冷启动；对 envd 无响应时拍下的快照，文档提醒 "files written and flushed before the pause are there … and writes not flushed before the pause may be lost"（persistence.md）。
- 恢复是懒加载：rootfs 经 NBD 按块取，内存经 UFFD 缺页时取（WCN §1.1、§1.2）。

### 2.5 用户面 API 一览

| API | 内容 | 原沙箱 | 文档写的限制 |
|---|---|---|---|
| `pause()` / auto-pause（`onTimeout: {action: 'pause'}`） | 文件系统 + 内存，或 `mode: 'filesystem'` | 停止 | 4 s/GiB；连接断开；节点还在完成上一次快照时回 503，沙箱继续运行 |
| `createSnapshot()` | 同上，一对多 | 短暂暂停后继续 | 需要 envd ≥ 0.5.0；连接断开 |
| fork | 原地 checkpoint + 新沙箱 | 短暂暂停后继续 | 停顿随上次快照以来的磁盘改动增长 |

来源：persistence.md、snapshots.md、filesystem-only-snapshots.md、fork.md（URL 见文末）。

---

## 3. 可比系统：只看冻结机制

### 3.1 runc / Docker：cgroup freezer

- [文档写了] `docker pause` "uses the freezer cgroup … With the freezer cgroup the process is unaware, and unable to capture, that it is being suspended"（<https://docs.docker.com/reference/cli/docker/container/pause/>）。
- [文档写了] cgroup v2：向 `cgroup.freeze` 写 `1`，"all belonging processes will be stopped … Freezing of the cgroup may take some time; when this action is completed, the "frozen" value in the cgroup.events control file will be updated to "1""（<https://github.com/torvalds/linux/blob/v6.12/Documentation/admin-guide/cgroup-v2.rst#L994-L1004>）。所以「冻结完成」要等 `cgroup.events` 报告，不能写完就当成已冻结。
- runc 容器和宿主共用内核页缓存，冻结那一刻容器里已经返回的 `write(2)` 和 `MAP_SHARED` 脏页，宿主读 upperdir 都能看到 [推断，未证实]。

### 3.2 gVisor runsc：pause、checkpoint 和 sentry 缓存

- [源码读出] `runsc pause` 的说明是 "pause suspends all processes in a container"（`G/runsc/cmd/pause.go`），没有刷文件系统缓存这一步。
- [文档写了] `runsc checkpoint --image-path=<path> [--leave-running]`：默认 checkpoint 之后停止容器；"`--leave-running` functions by causing an immediate restore so the container … may have a different process id"（`G/g3doc/user_guide/checkpoint_restore.md#L28-L41`）。用 `--network=host` 时，已连接的 socket 恢复后返回 `ECONNRESET`（同文件 `#L158-L169`）。文档没有给 checkpoint 的耗时。
- [源码读出] **gofer 挂载的文件不进 checkpoint 镜像**：`PrepareSave` 只把管道数据、以及已删除但仍被打开的文件的数据存进镜像，然后 "Flush local state to the remote filesystem"（`fs.Sync`）（`G/pkg/sentry/fsimpl/gofer/save_restore.go#L47-L88`）。所以在 `overlay2 = "none"`（RRV 的结论）下，根文件系统的内容仍然只在宿主 upperdir 里：checkpoint 保证宿主文件和保存下来的内存状态一致，但不替你冻结或复制 upperdir，恢复时也要求宿主上还是同一份文件。默认 `overlay2` 的上层放在 sentry 的 filestore 里，那时它会随内存一起进 checkpoint [未证实]。
- [源码读出] **宿主看到的可能落后于沙箱看到的**：根挂载默认 `--file-access=exclusive`（`G/runsc/config/flags.go#L146`）。exclusive 模式下 "The client may cache arbitrary filesystem state (file data, metadata …)"，"Client changes to filesystem state may be sent to the remote filesystem asynchronously"，"File timestamps are based on client clocks"（`G/pkg/sentry/fsimpl/gofer/gofer.go#L391-L406`）；源码里也承认 "a client write was serviced by the page cache, and only written back to the remote file later"（`gofer.go#L1998-L2001`）。有宿主 FD 时，普通写走宿主 FD（`gofer.go#L960-L975`），所以落后主要出在元数据（mtime 等）和没有宿主 FD 的页缓存写上。实际差多少 [未证实]。

### 3.3 Modal（基于 gVisor）

- [文档写了] Modal 的内存快照建在 runsc 上："for security reasons Modal uses the gVisor container runtime `runsc`"，"We're freezing containers mid-execution and serializing them to disk"，"We capture the container's filesystem mutations and its entire process tree"（Jonathon Belotti，*Memory Snapshots: Checkpoint/Restore for Sub-second Startup*，2025-01-28，<https://modal.com/blog/mem-snapshots>）。文中没有给 checkpoint 时长，只给了恢复 p50 约 1.05 s。
- [文档写了] 沙箱内存快照的限制：快照会让沙箱终止，"Snapshotting a Sandbox will currently cause it to terminate"；有 exec 在跑时不能拍（<https://modal.com/docs/guide/sandbox-snapshots.md>「Limitations」）。也就是说，Modal 没有用 `--leave-running` 做在线持久化。
- [文档写了] Filesystem Snapshot 是 "copies of the Sandbox's root filesystem at a given point in time"，"calculated as the difference from your base image"（同页）。拍的时候沙箱停不停、停多久，文档没写 [未证实]。
- [文档写了] Exit Snapshot 的补充（WCN 已引「不保证硬件故障」）："Snapshot failure loses that Sandbox's filesystem state"；GPU 沙箱被抢占时 "a preempted host generally does not give the Sandbox enough time to take an exit snapshot"，建议 "snapshot the filesystem periodically"（<https://modal.com/docs/guide/sandbox-exit-snapshots.md>）。也就是说，周期快照这件事 Modal 交给了用户自己做。

### 3.4 Fly Machines suspend

- [文档写了] suspend 用 Firecracker 快照 "capture the entire VM state: CPU registers, memory contents, open file handles"，存到持久存储；但 "**Snapshots aren't guaranteed to persist.**"，丢的情形包括 "The machine is migrated to a different host"，以及 "Hardware failures, space reclamation, or corruption can cause them to be deleted"；卷不受影响（<https://fly.io/docs/reference/suspend-resume.md>）。Fly 卷的每日快照和 RPO 见 WCN §1.2。

---

## 4. 对照

| | 运行中触发 | 冻结方式 | 含内存 | 冻结时长随什么增长 | 增量 | 节点丢失时丢掉 | 恢复 |
|---|---|---|---|---|---|---|---|
| Daytona 运行中备份 | cron，满 1 小时，每次启动至多一次（`v0.190.0`） | **无** | 否 | 不冻结；commit 时长随整个读写层增长 | 否，每次整层 | 上次完成的备份之后的一切，备份可能撕裂 | 整镜像 pull |
| Daytona 快照 | 用户 | `docker pause`，只覆盖 commit | 否（容器类） | 整个读写层的 tar | 否 | — | 整镜像 pull |
| E2B pause / snapshot / fork | 用户或超时 | Firecracker VM pause（fs-only 时加 guest `FIFREEZE`） | 默认是 | 文档：4 s/GiB 内存；原地 checkpoint：flush 加换缓存 | 是，4 KiB 块 / 页 | 上次快照之后的一切；上传完成前快照只在节点上 | 懒加载 |
| Modal 内存快照 | 用户 | runsc checkpoint，拍完沙箱终止 | 是 | 未公开 | — | — | 恢复 p50 约 1 s |
| Fly suspend | 用户或自动停止 | Firecracker 快照 | 是 | 未公开 | — | 快照不保证存在 | 恢复用时几百毫秒 |

---

## 5. 对 orrery 的含义

前提，都是仓库里已有的事实：根文件系统的差异就是 containerd 快照的 upperdir，Agent 有 stat 表（§16）；runsc 节点要设 `overlay2 = "none"` 才能按文件扫描（RRV）；a800 的数据盘是 ext4，没有 reflink（RRV「环境」）；P0 实测，15k 文件、212 MiB 的目录，stat 扫描 41–44 ms，16 并发全量 SHA-256 258 ms（`workspace-shared-storage-s3.md` §4.6）；a800 上 runc 写 1 GiB 加 fsync 2.7 s（RRV §2）。卷不在 upperdir 里，由守护进程自己 seal（§17）。下面只列选项，不做决定。

### 选项 A：冻结容器，扫描并暂存，再解冻

- **做法**：containerd `Task.Pause`（runc 写 `cgroup.freeze`；runsc 调 `runsc pause`），等 `cgroup.events` 报 `frozen 1`；对 upperdir 做 stat 扫描，和 stat 表比；把变化的文件复制到 `<data-root>/sandboxes/<id>/` 下的暂存区；解冻。锁外算哈希、上传，形状和 §17 的 seal 一样。变体是**预复制**：先不冻结扫一遍、复制一遍，再短暂冻结，只重扫并复制这期间又变了的文件，停顿就只剩扫描加上这一小段差异。
- **一致性**：runc 下得到整个根文件系统的时间点视图，相当于所有进程同时断电，只是页缓存还在（§3.1），所以算崩溃一致，应用层是否一致取决于应用自己的落盘格式（Git、带 WAL 的 SQLite 能扛）[推断]。runsc 下 `runsc pause` 不刷 sentry 缓存（§3.2），元数据可能落后：要么冻结前在容器里 `sync`（`sync` 和冻结之间仍有竞态），要么根挂载改成 shared 或 writethrough 模式，代价没测过 [未证实]。
- **预计停顿**：冻结延迟，加 stat 扫描（按 P0 数字是几十毫秒一档），再加复制变化的字节。ext4 上只能真复制，量级参考 a800 写 1 GiB 约 2.7 s（带 fsync）。也就是说，平时增量几 MB 时在一百毫秒上下；刚 `pip install` 完 1 GiB 时要几秒，预复制可以把这几秒挪到冻结之外 [估算，未实测]。
- **厂商证据**：Daytona 的快照就是这个形状，但冻结期间 tar 整个读写层，停顿随全部差异增长，比 A 粗（§1.3、§1.4）；Docker 默认 commit 要冻结（§1.3）；E2B 证明了「只把 flush 和换缓存留在冻结窗口里」是能做到的（§2.3）。和 VM 暂停不同，冻结 cgroup 不会让 TCP 连接断开，只是这段时间不响应 [推断，未证实]。Exec 和 Supervisor 计时（§6）也会被冻结，要能扛住这段停顿 [未证实]。

### 选项 B：冻结后对 upperdir 打文件系统级 CoW 快照

- **做法**：把 `<data-root>` 或 containerd snapshotter 的目录放在能 CoW 的文件系统上：btrfs 子卷快照、XFS 对变化的文件逐个 `reflink`，或者 LVM thin / dm 块快照。冻结、打快照、解冻，然后从快照里慢慢扫，扫完删掉。
- **一致性**：和 A 一样，是冻结那一刻的时间点。块级快照要求宿主文件系统也是一致的，等于要对宿主文件系统做 `FIFREEZE`，会把同一文件系统上的所有 Sandbox 一起冻住；不这样做就只是崩溃一致 [推断]。
- **预计停顿**：btrfs 快照只写元数据，按理和差异大小无关；XFS reflink 随变化文件的数量和 extent 增长，不随字节数增长。都比 A 的真复制小 [未证实，未实测]。
- **厂商证据**：E2B 在块层就是这么做的，换 COW 缓存、后台 reflink（§2.3）；Replit 用 btrfs 提交点，CodeSandbox 用 CoW 盘快照（WCN §1.2）。
- **代价**：Node 的数据盘文件系统成了部署要求，a800 现在是 ext4；overlayfs 跑在 btrfs 或 XFS 上的行为，还有 RRV 的可见性结论在这些文件系统上是否成立，都要重测（RRV「未确认」已列出其他文件系统）。

### 选项 C：`runsc checkpoint --leave-running`

- **做法**：只对 runsc 适用。checkpoint 会先停住整个 sentry，刷 gofer 缓存（`fs.Sync`），再写内存镜像，然后立即恢复（§3.2）。
- **一致性**：这是 runsc 下唯一一个能原子地做到「任务全停、sentry 已刷盘」的原语，而且含内存。但在 `overlay2 = "none"` 时镜像里没有 upperdir 的内容，复制 upperdir 仍要另外做（A 或 B），还得发生在 checkpoint 停住的那段时间里，可 `runsc` 没有暴露这个窗口 [源码读出 + 推断]。
- **预计停顿**：随内存大小增长。gVisor 没有公布数字；可以参考 E2B 的 Firecracker 是 4 s/GiB（§2.3）。`--leave-running` 是一次立即恢复，进程 ID 可能变，host 网络下的连接会被重置 [文档写了]。
- **厂商证据**：Modal 的内存快照就建在 runsc checkpoint 上，但拍完沙箱就终止，没有拿它做在线持久化（§3.3）。E2B 推出只存文件系统的 pause，理由正是 "smaller and faster"（§2.3）。runc 下对应的是 CRIU（`ctr task checkpoint`），本文没有调研。
- **适用面**：只为了文件系统的 RPO 而去存内存，成本过高；它更接近「pause/resume」这类产品功能。

### 选项 D：不冻结，在线扫描，逐文件校验

- **做法**：运行中周期性地做 stat 扫描；每个变化的文件，打开、`fstat`、复制或算哈希，再 `fstat` 一次，size、mtime、ctime 有变就重试或留到下一轮。这样提交出来的 Generation 标记为「运行中、非原子」，停止时的提交仍然是原子的，并且会取代它。
- **一致性**：只有单个文件是一致的，跨文件不一致。比如 Git 的 ref 可能已经指向一个这次没扫到的对象，SQLite 的主库和 `-wal` 可能不配对。Node 丢失后恢复出来的就是这样一个可能撕裂的状态 [推断]。
- **预计停顿**：零，代价是 IO，随变化的字节增长。
- **厂商证据**：Daytona 的运行中备份就是这一档，而且连逐文件校验都没有（`Pause: false`，§1.3）；Daytona 的整目录读接口自己写着 "Neither recipe guarantees a consistent snapshot"（PVP §2 引）；Gitpod Classic 干脆不做运行中备份（WCN 先读第 3 条）。
- **runsc**：§3.2 说的宿主落后问题这里同样存在。

---

## 6. 未证实、需要实测

- Daytona 线上的运行中备份是否仍然「每次启动至多一次」，冻结后是否改了 `setBackupPending`；容器类快照是否已收紧为只能在停止后拍（§1.2、§1.3）。
- Daytona VM 类 pause 和热快照的机制（源码不公开）。
- E2B 线上 `DeferRootfsExportFlag` 和 CoW 内存窗口的开启范围，以及 `guest_freeze.duration` 的实际分布（只有指标定义，没有公布的数字）。
- E2B 快照在上传完成前节点丢失时的处理；运行中节点丢失后 API 怎样处理这个沙箱。
- runsc 在 exclusive 模式下，宿主 upperdir 相对沙箱视图到底落后多少（mtime、目录项、页缓存写），以及冻结前 `sync` 或改成 shared/writethrough 模式的代价。
- containerd 对 runc 和 runsc 做 `Task.Pause` 的耗时，冻结期间 Exec、`ctl.sock` 中继和 Supervisor 计时器的行为，以及 TCP 连接能否撑过几秒的冻结。
- btrfs 或 XFS 上 overlayfs upperdir 的快照/reflink 耗时，以及 RRV 的可见性结论在这些文件系统上是否成立。
- `runsc checkpoint --leave-running` 在 orrery 的 Sandbox 规模下的耗时。

---

## 来源

**官方文档**

- Daytona：<https://www.daytona.io/docs/en/persistence.md>、<https://www.daytona.io/docs/en/snapshots.md>、<https://www.daytona.io/docs/en/sandboxes.md>；`D/apps/docs/src/content/docs/en/security-exhibit.mdx`
- E2B：<https://docs.e2b.dev/sandbox/persistence.md>、<https://docs.e2b.dev/sandbox/snapshots.md>、<https://docs.e2b.dev/sandbox/filesystem-only-snapshots.md>、<https://docs.e2b.dev/sandbox/fork.md>、<https://docs.e2b.dev/faq/sandbox-lifetime.md>
- Docker：<https://docs.docker.com/reference/cli/docker/container/commit/>、<https://docs.docker.com/reference/cli/docker/container/pause/>
- Linux：<https://github.com/torvalds/linux/blob/v6.12/Documentation/admin-guide/cgroup-v2.rst#L994-L1004>
- gVisor：`G/g3doc/user_guide/checkpoint_restore.md`
- Modal：<https://modal.com/docs/guide/sandbox-snapshots.md>、<https://modal.com/docs/guide/sandbox-exit-snapshots.md>
- Fly.io：<https://fly.io/docs/reference/suspend-resume.md>

**厂商博客**

- Modal，Jonathon Belotti，*Memory Snapshots: Checkpoint/Restore for Sub-second Startup*（2025-01-28）：<https://modal.com/blog/mem-snapshots>

**源码**

- Daytona（`D/`）：`apps/runner/pkg/docker/{backup.go,container_commit.go,snapshot_sandbox.go,image_pull.go}`、`apps/runner/cmd/runner/config/config.go`、`apps/runner/pkg/runner/v2/executor/sandbox.go`、`apps/api/src/sandbox/managers/backup.manager.ts`、`apps/api/src/sandbox/managers/sandbox-actions/{sandbox-start,sandbox-stop}.action.ts`、`apps/api/src/sandbox/services/sandbox.service.ts`、`apps/api/src/sandbox/entities/sandbox.entity.ts`、`apps/api/src/config/configuration.ts`
- E2B（`E/`）：`packages/orchestrator/pkg/sandbox/{sandbox.go,reclaim.go}`、`packages/orchestrator/pkg/sandbox/rootfs/nbd.go`、`packages/orchestrator/pkg/server/sandboxes.go`、`packages/shared/pkg/utils/version.go`、`packages/shared/pkg/storage/header/diff.go`、`packages/envd/internal/services/fsfreeze/fsfreeze_linux.go`、`packages/api/internal/orchestrator/{checkpoint_instance.go,snapshot_template.go}`、`packages/api/internal/handlers/sandbox_fork.go`
- gVisor（`G/`）：`pkg/sentry/fsimpl/gofer/{gofer.go,save_restore.go}`、`runsc/config/flags.go`、`runsc/cmd/{pause.go,checkpoint.go}`
- moby（`M/`）：`daemon/commit.go`、`daemon/images/image_commit.go`
