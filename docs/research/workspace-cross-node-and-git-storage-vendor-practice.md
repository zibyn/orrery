# Workspace 跨 Node 与 Git 存储：沙箱 / 云开发环境平台的做法

调研日期 **2026-11-14**。问题：Workspace 现在是 Node 本地目录，bind mount 到 Sandbox 的 `/workspace`，并且永久绑定在一个 Node 上（GLOSSARY「Workspace」）。我们倾向的方案是 `workspace-shared-storage-s3.md` §4.6：本地目录作为活副本，S3 上存不可变的 Workspace Generation，提交时用 stat 扫描做增量。FUSE 承载 Git 写操作已被否决（`workspace-wsfs-fuse-spike.md`）。本文回答两件事：

1. 其他沙箱 / 云开发环境平台怎样让 Workspace（尤其是里面的 Git 仓库）跨节点存活、迁移或同步；
2. 把 `.git` 和工作区文件（working tree）分开存储是否可行、有没有人这样做，以及在 orrery 里值不值得做。

只用一手来源：官方文档、钉死版本的源码、规格和厂商自己的工程博客；二手文章只用来找线索，不作引用。厂商原文保留英文。没有做任何实测。

| 项目 | 钉死 | 简写 |
|---|---|---|
| gitpod-io/gitpod（Gitpod Classic，README 写明已不推荐） | commit `85ece5100b7e5c30d49680651026eb500eb58fd6`（2026-11-11） | `G/` = `https://github.com/gitpod-io/gitpod/blob/85ece5100b7e5c30d49680651026eb500eb58fd6/` |
| e2b-dev/infra | commit `87310f0c0f01fd88c048ac08c371fca516a47fe9`（2026-11-13） | `E/` = `https://github.com/e2b-dev/infra/blob/87310f0c0f01fd88c048ac08c371fca516a47fe9/` |
| git/git | tag `v2.53.0`（commit `67ad42147a7acc2af6074753ebd03d904476118f`） | `GIT/` = `https://github.com/git/git/blob/v2.53.0/` |

复用的既有调研（这些文件里已有的事实只给出处，不重新取证）：`workspace-shared-storage-s3.md`（**WSS**）、`workspace-wsfs-fuse-spike.md`（**WSFS**）、`persistence-vendor-practice.md`（**PVP**）、`recheck-persistence-vs-coder-daytona.md`（**RPC**）、`recheck-runtime-and-workspace-vs-coder-daytona.md`（**RRW**）、`daytona-sandbox-runtime-semantics.md`（**DSR**）、`sandbox-git-integration.md`（**SGI**）、`code-vs-training-data-vendor-practice.md`（**CVT**）。

**来源说明**：本环境连不上 `docs.aws.amazon.com` 和 `aws.amazon.com`（TLS 被断开），`awsdocs/amazon-sagemaker-developer-guide` 仓库已清空，所以 AWS 一节没有一手来源。`replit.com` 和 `codesandbox.io` 对脚本返回 Cloudflare 403，这两家的博客改读 Wayback Machine 存档的原文（URL 见来源），内容就是厂商原文。

---

## 先读这几条

1. **没有一家沙箱或云开发环境平台把 `.git` 和工作区分开存储。** 每一家都把整个目录或整块盘当作一个单位来持久化：Codespaces 是 `/workspaces`；Gitpod Classic 把整个 workspace 目录打成一个 `full.tar`（`G/components/ws-daemon/pkg/controller/workspace_operations.go#L534`，`G/components/content-service/pkg/storage/storage.go#L26-L27`）；Ona 用 EBS 数据盘；Cloud Workstations 用挂在 `/home` 的 PD；Coder 用 PVC 或 docker volume；Daytona 用 `docker commit`；E2B、Modal、CodeSandbox 用块设备或文件系统快照。真正把 Git 对象和工作区分开的，只有带专用服务端的大型 monorepo 系统：Google CitC、Meta Sapling/EdenFS、Microsoft VFS for Git。它们的代价正是 P0 已经否决的东西：虚拟文件系统要在线，要专用协议，还要服务端（§3）。
2. **跨节点有三种做法，没有人在运行中跨节点同步一个可写目录。** (a) 网络块设备重新挂载：Ona 的 spot 回收是 "The EBS volume is reattached to the new instance"，Cloud Workstations 每次会话新建 VM 再挂上原 PD。(b) 从快照恢复：Gitpod Classic 用 tar 到对象存储，Daytona 用镜像推到 registry，Ona 和 Cloud Workstations 用云盘快照，Modal 和 E2B 用自家快照。(c) 块级懒加载：E2B 用 NBD 按 4 MB 块从对象存储取，Replit 用 NBD 按 16 MiB 块从 GCS 取，Fly 用 dm-clone 加 iSCSI 边跑边 hydrate。orrery §4.6 属于 (b)，粒度是文件级加 4 MiB 块。在做 (b) 的平台里，它是唯一按内容寻址、跨版本去重的。
3. **一致性点几乎都选在「停止之后」。** Gitpod Classic 会等容器停掉再备份，等 5 分钟还没停就强杀再备份（`G/components/ws-daemon/pkg/controller/workspace_controller.go#L403-L441`）。源码里也找不到运行中的周期备份。运行中做持久化的有三家，都要先找一个冻结点或一致点：E2B 的 filesystem-only 快照 "needs `envd` to freeze it"；Replit 在服务端识别 btrfs 事务提交写 superblock 的那组写操作，只在那个点持久化；Daytona 每小时 `docker commit` 一次（RRW #5）。Modal 的 Exit Snapshot 在退出时自动拍，并写明不保证硬件故障时的持久性。这和 §4.6「stop 时提交天然一致，运行中要 pause 冻结」的判断一致。
4. **对 Git 做专门处理的只有三家，而且都不是存储布局。** Gitpod Classic 在 stop 备份后记录一次 `git status`（`G/.../workspace_operations.go#L266-L276`）。它的 prebuild 恢复后会做 `git stash push`、fetch 到目标提交、`stash pop`，冲突就 `reset --hard`（`G/components/content-service/pkg/initializer/prebuild.go#L147-L191`）。Codespaces 先浅克隆，再在后台补全历史，还能在不启动 codespace 的情况下把当前分支导出成一个远端分支。Replit 在快照之外 "store a copy of the git history on a separate disk volume. Every Replit application has an immutable, append-only git remote"。Replit 是唯一给 `.git` 单独存一份的，但它存的是**冗余副本**，用来防 Agent 弄坏 Git，主存储仍然是整盘。
5. **单写者让这件事变得简单，Replit 把这句话写在了明处。** 原文："Since Repls are only running in one place at a time, that isn't an issue for us. If you remove the multi-writer requirement from a network file system, then it's really no different from a local filesystem." 这正是 orrery 单写者规则加 §4.6 能成立的前提。
6. **Git 自身机制里，能做到「`.git` 和工作区分开放」的有 `--separate-git-dir`/gitfile、`GIT_DIR`/`core.worktree`、`GIT_OBJECT_DIRECTORY`、alternates 和 `git worktree`。但它们解决的是路径，不是持久性。** 跨 Node 迁移真正要处理的 Git 问题只有一个：换机器后 index 里缓存的 `st_ino`/`st_ctime`/`st_dev` 失效，第一次 `git status` 要把跟踪文件重新读一遍（GIT/Documentation/technical/racy-git.adoc#L24-L45）。这是性能问题，不影响正确性。§4.6 已经估算过它的代价。
7. **结论：在 orrery 里不做存储层面的 `.git`/工作区分离。** 继续用 §4.6 的整树 Generation，加三条 Git 感知规则（§4.3）。将来如果跨 Node 恢复时间成了真问题，可以考虑一种只在**恢复路径**上的切法：先恢复 `.git`，干净的跟踪文件从本地对象库物化，只有改过的文件和未跟踪文件才从 CAS 下载。这需要先实测（§4.4）。

---

## 1. 平台对照

### 1.1 总表

| 平台 | 数据存在哪里 | 跨节点怎么做 | 停止时 / 运行中怎么持久化，一致性点 | 对 Git / `.git` 的专门处理 |
|---|---|---|---|---|
| GitHub Codespaces | 宿主 VM 上的 `/workspaces`，"a persistent directory that is mounted into the container"；底层盘的类型文档没写 | 停止后只收存储费；换宿主的机制**未找到一手来源**。prebuild 是 "A snapshot of the generated container"，创建时 "downloads the existing container snapshot from storage and deploys it on a fresh virtual machine" | stop 时 "any saved changes in your codespace will still be available"；rebuild 只清 `/workspaces` 之外；运行中的持久化机制未写 | 先浅克隆、后补全量历史（SGI B.1）；可以不启动 codespace，把当前检出的分支导出为 `codespace-` 前缀的分支，"only the Git branch that is currently checked out in the codespace is exported" |
| Gitpod Classic | Node 本地 XFS 工作目录（`G/components/ws-daemon/pkg/daemon/daemon.go#L198`）；官方博客说 gitpod.io 用 SSD RAID 0 | 每次启动都从对象存储恢复：先尝试下载 `full.tar`，没有就跑 initializer（git clone 或 prebuild）（`G/components/content-service/pkg/initializer/initializer.go#L471-L503`）；存储后端有 GCS、S3、MinIO（`G/components/content-service/pkg/storage/`） | **只在停止后**整目录打一个 tar 上传（`G/.../workspace_operations.go#L456-L561`）；等容器停掉，超过 5 分钟就强杀（`G/.../workspace_controller.go#L403-L441`）；源码里没有运行中的周期备份 | 备份后更新 `LastGitStatus`（`G/.../workspace_operations.go#L266-L276`）；prebuild：恢复快照后 stash、fetch 目标提交、pop，失败就回退到纯 Git 初始化（`G/.../prebuild.go#L63-L107`、`#L147-L191`）；fetch 默认 `--depth=1`（`G/components/content-service/pkg/initializer/git.go#L253`） |
| Ona（原 Gitpod Flex） | 用户自己 AWS/GCP 账号里的 VM 磁盘；Enterprise 的 "dual disk" 把系统盘和数据盘分开，数据盘放 `/workspaces`、容器镜像层和 Dev Container 产物 | spot 回收时 "The EBS volume is reattached to the new instance"；停得久了，数据盘 "can be detached and later restored from a provider backup"；GCP 可以跨 zone 重启；prebuild 是 "volume snapshots in your cloud provider (backed by S3)"，首次启动时 "lazy-load data from the prebuild snapshot" | 盘本身持久，"Everything on disk persists across environment stops and starts"；停止后归档，可以取消归档 | prebuild 发布前清掉 SCM 凭据和 Git 身份（SGI B.2）；rebuild 时 `/workspaces/<repo>` 靠 bind mount 保留；没有专门针对 `.git` 的存储处理 |
| Coder | 由模板决定：Kubernetes 用 RWO PVC，Docker 用 `docker_volume`（CVT §1.4） | 交给基础设施和 Terraform：块卷 detach/attach，或者绑在一个节点上（RRW W2） | 块设备，本地 POSIX 语义；平台不做快照 | 没有 |
| Daytona | runner 本地盘（container 类）；VM 类停止时 "offloads it to nearby storage"（CVT §1.1） | stop 时和运行中每小时做一次 `docker commit` 并推到 registry；runner 不可用时在别的 runner 上从备份恢复（RRW #5） | 镜像层快照，运行中就做 commit | Git 动词是平台 API（SGI B.5）；存储上不区分 `.git` |
| Modal | 镜像层加容器本地盘；Filesystem Snapshot "calculated as the difference from your base image, so only modified files are stored"；Directory Snapshot 只快照一个目录，可以挂进另一个运行中的沙箱 | 快照是 Image，从它在任意机器上起新沙箱；Memory Snapshot "can only be restored on the same exact instance type"；挂载的 Image 里的文件 "are prioritized when containers load files"（加载机制没有展开，可能是懒加载，未确认） | 手动快照；Exit Snapshot 在 terminate、空闲超时、OOM 时自动拍，但 "do not guarantee that a Sandbox's filesystem will be persisted in cases of rare hardware failures"；Memory Snapshot 期间不能有 exec 在跑 | 文档建议把系统依赖和应用代码分开：新基础镜像起沙箱，再挂之前快照的应用目录。这是代码目录和 rootfs 分开，不是 `.git` 和工作区分开 |
| E2B | Firecracker VM 的 rootfs 是 NBD 块设备，本地有写缓存（COW）；快照是对象存储（GCS/S3）上按 build 分层的块差异 | 恢复时按 4 MB 块懒取（`E/packages/orchestrator/pkg/sandbox/block/streaming_chunk.go#L23`）；块设备偏移到某个 build 存储区域的映射（`E/packages/shared/pkg/storage/header/mapping.go#L12-L40`） | pause 时把写缓存弹出并冻结，导出差异（`E/packages/orchestrator/pkg/sandbox/rootfs/nbd.go#L96-L100`、`#L170-L173`）；fork 时 "The pause duration scales with the amount of disk changes since the last snapshot"；filesystem-only 快照 "needs `envd` to freeze it" | 没有；Volume beta 文档明说 Git 要在本地盘上做（CVT §1.2） |
| Fly.io Machines | Volume "is a slice of an NVMe drive on the same physical server"，"It is not network storage" | 每天做快照，恢复成新卷；`fly volumes fork` 默认放到同 region 的另一台物理机；平台内部迁移用 dm-clone 加 iSCSI，"kill, clone, boot"，未 hydrate 的块读时从原卷取 | 每天快照，默认保留 5 天；文档提醒两次快照之间宿主机坏了，这段时间的数据就丢了 | 没有 |
| Replit | 虚拟块设备，"backed by Google Cloud Storage and are lazily loaded and cached by our storage servers"；每 16 MiB 一个块，不可变地存在 GCS，manifest 指向所有块；文件系统是 btrfs | 懒加载，所以哪台机器都能挂；fork 和 checkpoint 只是复制 manifest | 持久点选在 btrfs 事务提交：服务端识别 `write_all_supers` 那组写操作，"we use it to know which are the safest points to fully persist" | Agent 在每次 checkpoint 时做 `git commit`；能 "recover just the git object graph from a previous version of the filesystem"；另外 "store a copy of the git history on a separate disk volume"，即每个应用有一个 "immutable, append-only git remote" |
| CodeSandbox | Firecracker VM；磁盘和内存都用 COW 快照；内存快照放在 NVMe 上 | 跨主机迁移**未找到一手来源**；2024 年的博客说 live migration 还只是 "I still want to experiment with" | 休眠时做内存快照（免费用户 5 分钟） | 没有 |
| Google Cloud Workstations | 持久盘（zonal 或 regional PD）挂在 `/home`；会话结束时删掉 VM，"detaches and retains the persistent disk" | 每次启动都是新 VM 挂原盘；regional PD 可以 "replicate the VM and disk resources" 到两个 zone；`archiveTimeout` 到期把盘转成快照，下次启动时再从快照建盘 | 盘本身持久；可以给所有工作站的 PD 配定时快照策略 | 没有；只有 `/home` 持久 |
| AWS SageMaker Studio / Cloud9 | **未取得一手来源**（见来源说明） | — | — | — |

### 1.2 逐家要点

**GitHub Codespaces**。clone 在宿主 VM 上做，"subsequently mounted into the dev container"，是浅克隆，"the entire history of the repository is copied down with a full clone" 发生在之后（<https://docs.github.com/en/codespaces/about-codespaces/deep-dive>）。停止时 "A stopped codespace incurs only storage costs"，空闲超时后 "your data is preserved from the last time your changes were saved"；删除之后 "your work will be deleted too"（<https://docs.github.com/en/codespaces/about-codespaces/understanding-the-codespace-lifecycle>）。prebuild 是整容器快照，按 region 存放（<https://docs.github.com/en/codespaces/prebuilding-your-codespaces/about-github-codespaces-prebuilds>）。导出分支：codespace 不运行也能 "export your changes to a branch"，范围只有当前检出的分支；是否包含未提交的改动，文档没写（<https://docs.github.com/en/codespaces/troubleshooting/exporting-changes-to-a-branch>）。由此可以推出平台能在不启动 codespace 的情况下读取它的持久存储，并在服务端执行 Git 操作。具体怎么实现，未找到一手来源。

**Gitpod Classic**。数据路径完全是「本地盘 + 停止后整包上传」：

- 备份：`uploadWorkspaceContent` 先删掉 ready 文件，用 `content.BuildTarbal` 把整个 workspace 目录（含 `.git`）打成一个 tar，做 UID 映射后上传（`G/components/ws-daemon/pkg/controller/workspace_operations.go#L456-L561`）。普通 workspace 的对象名固定是 `full.tar`，每次覆盖；prebuild 和快照用 `snapshot-<ns>.tar`（`G/.../workspace_controller.go#L451-L453`，`G/.../workspace_operations.go#L353`）。没有增量，也没有去重。
- 并发：备份用一个信号量限流，等 15 分钟拿不到也照样上传（`G/.../workspace_operations.go#L457-L469`）。官方博客解释了原因："Backing up and restoring local disks proved to be an expensive operation. We implemented a solution using a daemonSet that uploads and downloads uncompressed tar archives to/from S3 … (de)compressing archives consumes most available CPU on a node"；"Particularly the content backup/restore produced this problem. We implemented cgroup-based IO limiter"（<https://ona.com/stories/we-are-leaving-kubernetes>）。
- 恢复：`InitializeWorkspace` 先尝试下载 `full.tar`，有就算 `WorkspaceInitFromBackup`，没有才跑 Git 或 prebuild 初始化（`G/components/content-service/pkg/initializer/initializer.go#L471-L503`）。
- 存储取舍（同一篇博客）："SSD RAID 0: This offered high IOPS and bandwidth but tied the data to a specific node. The failure of any single disk would result in complete data loss. This is how gitpod.io operates today"；PVC 的问题是 "Unpredictable attachment and detachment timing, leading to unpredictable workspace startup times"、"Limited number of disks that could be attached to an instance"、"AZ locality constraints"；最后的结论是 "we, once again, identified storage as the crucial element for providing all three: reliable startup performance, reliable workspaces (don't lose my data) and optimal machine utilization."

**Ona（原 Gitpod Flex）**。离开 Kubernetes 之后改成每个环境一台 VM，数据在云盘上。

- dual disk："Dual disk environments separate the VM system disk from the environment data disk … The data disk contains the workspace and development state that should survive VM replacement"；"If the VM is later replaced, Ona can reattach the preserved disk or recreate it from a provider backup, such as an image or snapshot"（<https://ona.com/docs/ona/runners/dual-disk.md>）。注意它分开的是**系统和数据**，不是 `.git` 和工作区。
- prebuild 和快照："Snapshots are stored as volume snapshots in your cloud provider (backed by S3)"（<https://ona.com/docs/ona/projects/prebuilds.md>）；warm pool 页说 "Instances lazy-load data from the prebuild snapshot on first boot, and larger volumes take longer to fully hydrate"，大 monorepo 就用 warm pool 把启动从 "minutes to around 10 seconds"（<https://ona.com/docs/ona/projects/warm-pools.md>）。
- base snapshot："A base snapshot stores a prepared copy of the organization's default image … The workspace, repository, user configuration, and project setup still start fresh"（<https://ona.com/docs/ona/runners/aws/base-snapshots.md>）。
- 对 Agent 的建议："For agents, the recommended workflow is ephemeral environments - one fresh environment per task"（<https://ona.com/docs/ona/environments/persistent-storage.md>）。

**Modal**。三种快照，见 <https://modal.com/docs/guide/sandbox-snapshots.md>：

- Filesystem Snapshot 只覆盖 rootfs，"Any mounted Volumes are not covered by this Snapshot"；默认 30 天 TTL。
- Directory Snapshot 的用途之一是 "Updating system dependencies separately from application code"。
- Memory Snapshot 的限制："Snapshotting a Sandbox will currently cause it to terminate"、"It is not possible to snapshot a Sandbox while an exec command is still running"。
- Exit Snapshot（<https://modal.com/docs/guide/sandbox-exit-snapshots.md>）在退出时自动拍，用途写的是 "Resuming an agent or coding session after an idle timeout"。

Volume 的提交制见 PVP §1.1。

**E2B**。文档见 <https://docs.e2b.dev/sandbox/persistence.md>、<https://docs.e2b.dev/sandbox/fork.md>、<https://docs.e2b.dev/sandbox/filesystem-only-snapshots.md>。pause 默认同时保存文件系统和内存；filesystem-only 快照 "reboots on resume"，用途包括 "persisting a workspace's files between sessions"。源码层面：

- `SwapForBackgroundSeal` 先 flush NBD 设备，再换上一个新的空缓存，旧缓存冻结后在后台导出，"Only the device flush stays on the critical path"（`E/packages/orchestrator/pkg/sandbox/rootfs/nbd.go#L204-L209`）。这和 §4.6「冻结 → 复制到暂存区 → 解冻 → 慢慢上传」是同一个形状，只是做在块层。
- 差异块用 roaring bitmap 记脏块，映射到本 build 的存储偏移（`E/packages/shared/pkg/storage/header/mapping.go#L22-L40`）；读取时按块从对象存储取，本地有缓存（`E/packages/orchestrator/pkg/sandbox/build/storage_diff.go#L105-L120`）。

**Fly.io**。卷是本地 NVMe（<https://fly.io/docs/volumes/overview/>）。官方提醒 "Fly.io takes daily snapshots … but the snapshots shouldn't be your primary backup method"，以及 "any data stored between the time when the snapshot was taken and the time when the failure occurred will be lost"（<https://fly.io/docs/volumes/snapshots/>）。fork 会把新卷放到另一台物理机，"The new volume and the source volume do not continue to sync"（<https://fly.io/docs/volumes/volume-manage/>）。平台的排空迁移写在工程博客里（<https://fly.io/blog/machine-migrations/>）：

- "No matter the backup interval, a 'restore from backup migration' will lose data, and a 'backup and restore' migration incurs untenable downtime."
- "Kill, copy, boot is slow. But kill, clone, boot is fast"。dm-clone 的语义是 "reads of uninitialized blocks will pull from the original"，写入不走网络。
- 只 hydrate 用到的块：DISCARD 会让 dm-clone 把对应区域直接标记为已 hydrate。

对 orrery 的含义：块级懒加载要求源 Node **在线**，所以它能用于计划内排空，Node 丢失时用不上。orrery 的 Generation 放在 S3 上，不依赖源 Node。

**Replit**。两篇工程博客：*Replit Storage: The Next Generation*（2023-07-19，更新于 2023-10-17）和 *Inside Replit's Snapshot Engine*（2025-12-17）。

- 旧方案就是「本地 btrfs + 启动时整包流入」："Once the Repl's filesystem is mounted, we streamed a serialized version of the last snapshot of the filesystem … All this process was slower and more brittle the larger the Repl was. And that was the reason why Repls had been historically capped to 1 GiB"。这是 §4.6「大 Workspace 启动时间与大小成正比」的真实先例。
- 新方案 margarine："slices up the entire virtual size of the disk into 16MiB blocks … downloading that entire block from GCS and writing it to a local scratch disk"。manifest 加 COW：每个块在由干净变脏时分配一个新 UUID，"When there is a btrfs commit, we create a copy of this map, update it with pointers to the new blocks, and persist the blocks with their associated UUID, plus the manifest"。
- 为什么不用 NFS："we would need to write to a GCS file every time anyone touched a file in a repl, which would be cost prohibitive and high latency"。
- Git：见先读第 4 条。原文还有 "If a user wants to undo code changes made by the Agent, they can rollback to any checkpoint, which uses git to revert the code to its earlier state"，以及 "But what happens if the Agent accidentally corrupts the git state itself? That is where the design of Bottomless Storage Infrastructure comes in"。

**CodeSandbox**。*How we clone a running VM in 2 seconds*（2022-09-01）："We use this technique both for our disks (by creating disk CoW snapshots) and for our memory snapshots"。*How we scale our microVM infrastructure using low-latency memory decompression*（2024-05-29）："store those snapshots on NVMe drives for low-latency access"，并说跨服务器 live migration 还停留在设想阶段。磁盘快照存放在哪里、跨主机怎么搬，**未找到一手来源**。

**Google Cloud Workstations**。见 <https://docs.cloud.google.com/workstations/docs/architecture>：每次启动 "Creates a VM … Attaches the persistent disk to the VM"，结束时 "deletes the VM but detaches and retains the persistent disk"。API 参考（<https://docs.cloud.google.com/workstations/docs/reference/rest/v1/projects.locations.workstationClusters.workstationConfigs>）里有三个相关字段：

- `archiveTimeout`："converting its disk into a snapshot. This generally saves costs at the expense of greater startup time on next workstation start"。
- `sourceSnapshot`：从快照建盘。
- `replicaZones`：regional PD 的两个 zone。

它的做法是「热数据在块盘上，冷了转快照」。orrery §4.6 的「停止后本地目录变成可淘汰缓存」是同一个思路，区别在于我们的「快照」是按内容寻址的 Generation。

---

## 2. Git 自身的机制：对「`.git` 与工作区分离」和「跨节点迁移」各有什么用

| 机制 | 一手语义 | 对「分离」的用处 | 对「跨节点」的用处 | 对 orrery 的判断 |
|---|---|---|---|---|
| `--separate-git-dir` / gitfile | "place the cloned repository at the specified directory, then make a filesystem-agnostic Git symbolic link to there. The result is Git repository can be separated from working tree."（GIT/Documentation/git-clone.adoc#L329-L334）；工作区根下一个写着 `gitdir: <path>` 的纯文本 `.git` 文件（GIT/Documentation/gitrepository-layout.adoc#L24-L32） | 正是「`.git` 放别处」的官方做法 | 不提供持久性，只是一个路径指针。`gitdir:` 指向 `/workspace` 之外时，Generation 只带走指针，到新 Node 上仓库就断了 | 不作为平台机制。要写进能力文档：仓库目录必须在 `/workspace` 之内 |
| `GIT_DIR` / `GIT_WORK_TREE` / `core.worktree` / `GIT_COMMON_DIR` / `GIT_INDEX_FILE` / `GIT_OBJECT_DIRECTORY` | 分别指定仓库目录、工作区根、共享的非 worktree 文件、index 文件、对象目录（GIT/Documentation/git.adoc#L496-L535、#L566-L572；GIT/Documentation/config/core.adoc#L327-L340） | 能把 index、对象库、refs 分别放到不同路径 | 同上，只改路径 | 不需要 |
| alternates / `clone --shared` / `--reference` | 对象库可以借用别的对象库（GIT/Documentation/gitrepository-layout.adoc#L80-L89）；`GIT_ALTERNATE_OBJECT_DIRECTORIES` "New objects will not be written to these directories"（GIT/Documentation/git.adoc#L513-L519）；`--shared` 的 NOTE 写着源仓库 gc 之后 "the cloned repository will become corrupt"，`git repack -a` 可以解除依赖（GIT/Documentation/git-clone.adoc#L80-L105；SGI A.4） | 能做到「共享只读对象库 + 每个 Workspace 一份私有可写部分」，是 Git 原生最接近「对象和工作区分开」的形态 | 被借用的库在 `/workspace` 之外时，Generation 带不走它，换 Node 就损坏 | **不做**：要么自己维护一个永不 gc 的 per-owner 对象库，要么接受损坏风险（SGI D.3）。CAS 按 owner 去重已经覆盖了「同 owner 多份 clone」最常见的重复，但只覆盖工作区文件，pack 不在内（见 §4.2） |
| `git worktree` | "sharing everything except per-worktree files such as `HEAD`, `index`, etc."（GIT/Documentation/git-worktree.adoc#L69）；链接默认用绝对路径，`worktree.useRelativePaths` 默认 false，设为 true 会启用 `extensions.relativeWorktrees`（GIT/Documentation/config/worktree.adoc#L11-L19）；手工移动后要 `repair`（GIT/Documentation/git-worktree.adoc#L144-L163）；BUGS 一节写明多检出 "still experimental"（#L534-L536） | 一个对象库对应多个工作区 | Sandbox 里的路径恒为 `/workspace/...`，换 Node 后绝对路径不变，所以不受影响。但 Agent 在宿主上看到的是 `<data-root>/…`，如果在宿主侧对链接工作区跑 Git，路径会断 | Workspace 内部的 worktree 能被 Generation 正常带走。平台不在宿主侧跑 Git，所以没有影响 |
| partial clone（`--filter=blob:none`） | "filter out all blobs (file contents) until needed by Git"（GIT/Documentation/git-clone.adoc#L186-L194）；"Use of partial clone requires that the user be online and the origin remote or other promisor remotes be available for on-demand fetching"（GIT/Documentation/technical/partial-clone.adoc#L40-L46） | 让 `.git` 只存历史骨架，blob 按需取 | Generation 变小，但恢复后缺的 blob 要去远端取，需要网络和凭据（SGI A.4） | 用户自己选。平台不默认，也不依赖它 |
| sparse-checkout | cone 模式是默认（GIT/Documentation/git-sparse-checkout.adoc#L25-L26）；`clone --sparse` 起初只检出顶层（git-clone.adoc#L180-L184） | 缩小工作区，`.git` 仍然完整 | Generation 和恢复量都随工作区变小 | 用户自己选 |
| `git bundle` | 能 clone 和 fetch，"There's no corresponding 'write' support"（SGI A.4） | 能把 refs 和对象打成一个文件 | 只能带走 refs 能到达的对象；index（暂存区）、未跟踪文件、ignored 文件都带不走 | 不适合当 Workspace 持久化手段，见 §4.2 方案 C |
| `scalar` | "only commit and tree objects are cloned"，默认开 sparse-checkout，工作区放在 `<enlistment>/src`，"This encourages the separation between tracked files (inside `src/`) and untracked files, such as build artifacts (outside `src/`)"（GIT/Documentation/scalar.adoc#L24-L34、#L56-L68） | 它分开的是**跟踪文件和构建产物**，不是 `.git` 和工作区 | 与 partial clone 一样需要在线 | 不需要 |
| `core.fsmonitor` | v2.53.0 内置 daemon "currently … Windows and MacOS"（GIT/Documentation/config/core.adoc#L64-L79）；源码只有 darwin 和 win32 后端（GIT/compat/fsmonitor/）；默认拒绝网络挂载（GIT/Documentation/git-fsmonitor--daemon.adoc#L73-L77） | 无 | 无。Linux 上只能接 hook（如 Watchman） | 不适用：Node 是 Linux |
| `index.skipHash`（以及 `feature.manyFiles`） | 不计算 index 末尾的哈希，写 index 更快；2.13 以前的 Git 拒绝解析，2.40 以前的 `git fsck` 会报错（GIT/Documentation/config/index.adoc#L34-L43）；`feature.manyFiles` 同时打开 `index.version=4` 和 untracked cache（GIT/Documentation/config/feature.adoc#L34-L47） | 无 | 无 | 和迁移无关。untracked cache 依赖**目录的 mtime**（GIT/Documentation/git-update-index.adoc#L477-L488），所以 Generation 恢复时要连目录 mtime 一起还原 |
| `core.checkStat` / `core.trustctime` | Git 比较 `st_mode`、`st_mtime`、`st_ctime`、`st_uid`、`st_gid`、`st_ino`、`st_size`；`st_dev` 默认不比，"because this member is not stable on network filesystems"（GIT/Documentation/technical/racy-git.adoc#L24-L45）；`checkStat=minimal` 只比整秒 mtime（加上 ctime，前提是 trustCtime）和大小（core.adoc#L136-L150）；`trustctime=false` 忽略 ctime（core.adoc#L114-L119）；`update-index --refresh` 会 "re-match" stat（git-update-index.adoc#L247-L255） | 无 | **这是换机器后 index 失效的根因。** 恢复后 ino 和 ctime 必然变化；只要 mtime 和大小一致，Git 读一遍内容确认没变后就刷新 index，结果是对的，只是第一次慢 | 默认不改用户配置。§4.6 已估算一次全量重读的代价（P0 规模是亚秒）；可选的优化见 §4.3 |
| `core.fsync` | 大多数平台的默认值相当于 `committed,-loose-object`，"risks losing recent work in the event of an unclean system shutdown"（GIT/Documentation/config/core.adoc#L652-L655） | 无 | 关系到「Node 突然掉电时本地活副本里的 `.git` 是否完整」 | Generation 读的是页缓存（stop 后或冻结时），Generation 的一致性不受它影响。只有「Node 掉电后在同一台 Node 上原地恢复」才受它影响，那属于本地盘的语义 |
| `git maintenance` | 定时任务：`prefetch` 每小时，`loose-objects` 和 `incremental-repack` 每天（GIT/Documentation/git-maintenance.adoc#L70-L72）；`loose-objects` 把松散对象打进 pack（#L133-L150）；`incremental-repack` 增量重打包（#L152） | 无 | 影响 Generation 的增量大小：pack 文件一旦写出就不会再改，stat 扫描会自然跳过；但每次全量 `gc`/`repack -a` 都会写出一个和仓库一样大的新 pack，要整个重新上传 | 见 §4.3 规则 3 |
| `safe.directory` | "By default, Git will refuse to even parse a Git config of a repository owned by someone else"（GIT/Documentation/config/safe.adoc#L20-L24） | 如果把 `.git` 放到另一个属主的存储上，Git 会拒绝 | 恢复时要保证 uid/gid 还原（WSS §4.1 已经要求记录 uid/gid） | 恢复的正确性要求，不是新机制 |

---

## 3. 谁把 Git 对象存储和工作区分开了：大型 monorepo 系统

| 系统 | 怎么分 | 代价 | 来源 |
|---|---|---|---|
| Google CitC（Piper） | "a cloud-based storage backend and a Linux-only FUSE file system"；工作区是整个 Piper 仓库上叠加的改动，"only modified files are stored in their workspace … an average workspace has fewer than 10 files"；"All writes to files are stored as snapshots in CitC"；"CitC workspaces are available on any machine that can connect to the cloud-based storage system, making it easy to switch machines" | 每次读都依赖云端存储和 FUSE；前提是单一的中心化仓库（Piper）和配套的构建、浏览工具 | Potvin & Levenberg, *Why Google Stores Billions of Lines of Code in a Single Repository*, CACM 2016-07（<https://cacm.acm.org/research/why-google-stores-billions-of-lines-of-code-in-a-single-repository/>，读的是 Wayback 存档） |
| Meta Sapling + 虚拟文件系统（EdenFS） | 克隆时 "downloads almost no history"；Segmented Changelog 只下载提交图的形状，提交内容 "lazily filling in"；虚拟文件系统让工作区看起来是完整的，内容按需取 | "accessing a file for the first time requires a network request"；"Many of our scale features require using a Sapling-specific server"；"When using Sapling with a Git repository, some of these optimizations will not apply"；发布时虚拟文件系统 "not yet publicly available" | <https://engineering.fb.com/2022/11/15/open-source/sapling-source-control-scalable/> |
| Microsoft VFS for Git | "virtualizes the file system beneath your Git repository"，"only downloads objects as they are needed" | "requires a Git service that supports the GVFS protocol"；只支持 Windows；README 说 "for new deployments, we strongly recommend you consider Scalar instead of VFS for Git" | <https://github.com/microsoft/VFSForGit> |
| Scalar（并入 Git） | 不做虚拟文件系统，只用 partial clone 加 sparse-checkout 加后台维护 | 要在线，部分对象按需取 | §2 `scalar` 行 |
| Codespaces / Gitpod / Ona 的 prebuild | 不分 `.git`，把「clone 加依赖安装之后的整个容器或数据盘」做成快照，新环境从快照起 | 快照按 region 或 runner 存放；Gitpod 恢复后还要 fetch、checkout 到新提交（§1.2） | §1.2 |

共同点：**分开的一侧总是「远端的、只读的、按内容寻址的对象库」，另一侧是「本地的、只存改动的覆盖层」**，靠虚拟文件系统把两者拼成完整视图，所以都要求在线，都要专用服务端。WSFS 已经证明 FUSE 在 Git 写操作的热路径上慢一个数量级。这一类形态的「工作区只存改动」部分，orrery 已经用另一种方式做到了：Generation 增量加 CAS 去重。

---

## 4. 对 orrery 的建议

### 4.1 前提（都是仓库里已有的事实）

- 单写者：同一时刻最多一个读写 Sandbox（GLOSSARY「Workspace」），所以活副本只有一份，在一台 Node 上。
- §4.6：运行时就是本地目录，Generation 是按 stat 扫描得到的增量，数据进 `cas/<owner_id>/sha256/<hex>`（ADR-0021），≤4 MiB 的文件一个对象，更大的文件按 4 MiB 分块（ADR-0030，WSS §4.1 补记）。
- Workspace 是任意文件树，含 0..N 个 Git 仓库；平台**没有** Repository 资源，也不代为 clone 或 push（ADR-0013）。平台并不知道仓库在哪里。
- Agent 的 Files API 在 Sandbox 运行时直接写宿主上的 Workspace 目录（`internal/agent/files.go:133`，WSFS「性能」一节）。

### 4.2 四种切法逐一评估

| 切法 | 做法 | 在 orrery 里的问题 | 判断 |
|---|---|---|---|
| A. `.git` 放持久或共享存储，工作区放本地盘 | 例如 `.git` 放 FUSE/NFS/对象存储，工作区留在本地 | Git 的写操作几乎都落在 `.git` 里：clone 写 pack，checkout 和 status 写 index，commit 写松散对象，refs 用锁文件加 rename。WSFS 测得这些操作在 FUSE 上慢 2.5–16 倍，等于把最慢的部分放到了最慢的存储上。另外平台不知道 `.git` 在哪里，得在 Sandbox 里改 `GIT_DIR` 或用 gitfile，违背 ADR-0013「平台不管仓库」 | **否决** |
| B. 工作区放共享存储，`.git` 放本地 | — | `git status` 要对每个跟踪文件 `lstat`，热路径还是落在共享存储上；`.git` 留在本地又带不走 | **否决** |
| C. 只持久化 `.git` 加未提交的改动，工作区随时从 Git 重建 | 提交时存 `.git`（或 bundle）加 diff，恢复时 checkout 再打补丁 | (1) Workspace 不只有仓库：未跟踪文件、ignored 文件（`node_modules`、构建产物、`.env`）、仓库之外的文件都要另外保存，最后还是一套整树机制。(2) 三方状态（HEAD、index、工作区）、子模块、LFS、sparse、链接 worktree、hooks、可执行位都要精确重建，任何一处出错都是静默的数据错误。(3) 省下的存储很少：CAS 已经在 Generation 之间和同 owner 的 Workspace 之间按内容去重，干净的跟踪文件每份内容只存一次 | **否决** |
| D. 存储不分，只在**恢复路径**上利用 `.git` | Generation 仍然是整树（§4.6 不变）；提交时给每个普通文件多记一个 Git blob id（可选字段）；跨 Node 恢复时先恢复所有 `.git` 目录，对 blob id 能在本地对象库找到的文件，用 `git cat-file` 在本地物化，其余文件从 CAS 下载；任何一步失败都回退到从 CAS 下载 | 只是优化，不影响正确性，CAS 永远是权威来源。收益取决于「pack 大小 / 工作区大小」：P0 样本是 33 MiB 对 212 MiB，但**只有一个样本**。代价是在提交时多算一个 SHA-1（或 SHA-256）blob id，并在恢复时解压和解 delta | **暂不做，留作 P3 候选**（见 §4.4 的触发条件） |

### 4.3 推荐：不做存储层面的分离，在 §4.6 上加三条 Git 感知规则

1. **运行中的 Generation 不收 Git 的锁文件。** §4.6 运行中提交是在 cgroup 冻结时扫描，冻结点可能正好落在某个 Git 命令中间，留下 `index.lock`、`*.lock`。Git 的设计是「写临时文件 → rename」，所以冻结点上的状态相当于一次进程崩溃，是一致的。只有锁文件会残留，恢复后 Git 会拒绝运行（"Another git process seems to be running"）。规则：如果一个目录里同时有 `HEAD` 和 `objects/`，就把它当作 Git 目录，提交时跳过其中的 `*.lock`，效果等于那次 Git 操作在 rename 之前崩溃了。stop 时的提交没有活着的进程，不需要这条规则，但统一处理更简单。这是平台唯一需要「知道哪里是 `.git`」的地方，靠扫描判断，不靠 Repository 元数据。
2. **恢复要还原 mtime（包括目录的 mtime）、mode、uid/gid，接受 ino 和 ctime 会变。** 这样第一次 `git status` 的代价就是「把跟踪文件重读一遍」（racy-git.adoc#L24-L45）。untracked cache 依赖目录 mtime（git-update-index.adoc#L477-L488），目录 mtime 必须在子项写完之后再设。`safe.directory` 要求属主一致（safe.adoc#L20-L24）。不在 Snapshot 里默认 `core.checkStat=minimal`、`core.trustctime=false`。理由：§4.6 已经判断代价是亚秒级；改用户的 Git 语义是隐式行为，而且 `minimal` 会放过同一秒内、同样大小的改动。只有当实测大仓库的第一次 status 不可接受时，再考虑由 Agent 在恢复后、启动 Sandbox 之前对发现的仓库跑一次 `git update-index --refresh`。这样代价挪到了启动前，语义不变。
3. **pack 文件天然适合增量提交，`gc` 和全量 repack 是 Generation 体积的主要放大源。** `objects/pack/pack-<hash>.pack` 写出后就不再改，stat 扫描会跳过。但每次全量 repack 都会写一个和仓库一样大的新 pack，旧 pack 被删除，于是 Generation 要上传一个整仓大小的新对象，旧对象等 GC 回收。松散对象是 KB 级小文件，每个都是一个 CAS 对象和一次 PUT；ADR-0030 暂缓了小文件打包，Git 松散对象正是小文件的主要来源。先不改用户配置，只在 Generation 指标里分别统计 `.git/objects` 和其他路径的上传字节数与 PUT 次数，用数据决定要不要做小文件打包，或者在 Snapshot 的系统 gitconfig 里建议 `maintenance.strategy=incremental`。

能力文档里要写明的限制（不是新机制）：仓库的 `gitdir:`、alternates、`--shared`/`--reference` 借用的对象库、链接 worktree 的主仓库，都必须在 `/workspace` 之内，否则换 Node 后仓库会损坏或断开（§2 的 `--separate-git-dir`、alternates、worktree 三行）。partial clone 恢复后补取 blob 需要网络和 Git 凭据（SGI A.4）。

和 §4.6 的组合：§4.1 的数据模型、§4.4 的 Control Plane 变更、§4.6 的 Node 侧流程都不变；以上三条是提交器和恢复器的实现细节，不新增表，也不新增 API。方案 D 如果将来做，只在 Generation 元数据里给文件条目加一个可选的 `git_oid` 字段，并加一条恢复路径；不改 CAS 布局，不改 publish，也不改只读挂载。

### 4.4 风险

- **运行中冻结提交的 Git 一致性**：上面的论证依赖「Git 的每次写都是临时文件加 rename」。v2.53.0 的 refs、index、对象都是这样，但 reftable 后端、`commit-graph`、`multi-pack-index` 在冻结点上的行为没有逐一核对。用户工具（IDE、LSP）在 `.git` 里写的私有文件也一样没核对。
- **Files API 和 Git 并发**：Agent 的 Files API 直接写宿主目录。如果它在冻结窗口之外写进 `.git`（例如上传一个 `.git/config`），就绕过了 Sandbox 里的 Git 锁。§4.6 已要求冻结期间阻塞 Files API，这里只是再强调一次，冻结窗口之外的情况不归平台管。
- **方案 D 的触发条件**：只有同时满足三条才值得做：跨 Node 恢复时间成了真问题（用户等待或 SLA）；实测大仓库的 `.git` 明显小于工作区；本地物化比从 S3 下载快。另外它只对 Node 上**没有**块缓存的情形有用。
- **外部先例的边界**：Replit 的「独立的 append-only git remote」是对抗 Agent 弄坏 `.git` 的冗余手段。orrery 的 Generation 历史已经能回到任意时间点，只要 Generation 保留策略覆盖到所需的时间窗口，这一项就不需要另做。

---

## 5. 未确认、需要实测

- Codespaces 停止后存储在哪里、怎么换宿主：未找到一手来源。导出分支是否包含未提交的改动：文档没写。
- CodeSandbox 磁盘快照存放的位置和跨主机机制：未找到一手来源。
- AWS SageMaker Studio（space 的 EBS/EFS）和 Cloud9（EBS）：本环境无法访问 AWS 文档，**未取得一手来源**，下文也没有据此下结论。
- Modal Image 和 Directory Snapshot 的加载是不是懒加载：文档只说 "prioritized when containers load files"，未确认。
- Meta 博客发布于 2022-11；EdenFS 后来是否开源、现状如何，没有核对。
- 冻结点上 reftable、commit-graph、multi-pack-index 以及第三方工具在 `.git` 里写的文件是否都是「临时文件加 rename」（§4.4）：需要读源码或做崩溃注入测试。
- 跳过 `.git/**/*.lock` 之后，恢复出的仓库能否通过 `git fsck --strict`，第一条 Git 命令能否正常运行：需要在 runc 和 runsc 下各做一次冻结注入实测。
- 大仓库（GB 级、十万级文件）跨 Node 恢复后第一次 `git status` 的耗时，以及 `update-index --refresh` 前移到启动前的收益：需要实测。
- `.git/objects` 在 Generation 上传字节数和 PUT 次数中的占比，以及 `gc`/`repack` 的放大倍数：需要在真实 Agent 负载上统计。
- 方案 D：多个真实仓库的 pack 与工作区大小比例，以及本地 `git cat-file` 物化和 S3 下载的速度对比：需要实测。

---

## 来源

**平台文档**
- GitHub Codespaces：<https://docs.github.com/en/codespaces/about-codespaces/deep-dive>、<https://docs.github.com/en/codespaces/about-codespaces/understanding-the-codespace-lifecycle>、<https://docs.github.com/en/codespaces/prebuilding-your-codespaces/about-github-codespaces-prebuilds>、<https://docs.github.com/en/codespaces/troubleshooting/exporting-changes-to-a-branch>
- Ona：<https://ona.com/docs/ona/environments/persistent-storage.md>、<https://ona.com/docs/ona/runners/dual-disk.md>、<https://ona.com/docs/ona/projects/prebuilds.md>、<https://ona.com/docs/ona/projects/warm-pools.md>、<https://ona.com/docs/ona/runners/aws/base-snapshots.md>、<https://ona.com/docs/ona/runners/aws/environment-classes.md>、<https://ona.com/docs/ona/environments/archive-auto-delete.md>
- Modal：<https://modal.com/docs/guide/sandbox-snapshots.md>、<https://modal.com/docs/guide/sandbox-exit-snapshots.md>
- E2B：<https://docs.e2b.dev/sandbox/persistence.md>、<https://docs.e2b.dev/sandbox/filesystem-only-snapshots.md>、<https://docs.e2b.dev/sandbox/snapshots.md>、<https://docs.e2b.dev/sandbox/fork.md>
- Fly.io：<https://fly.io/docs/volumes/overview/>、<https://fly.io/docs/volumes/snapshots/>、<https://fly.io/docs/volumes/volume-manage/>
- Google Cloud Workstations：<https://docs.cloud.google.com/workstations/docs/architecture>、<https://docs.cloud.google.com/workstations/docs/customize-container-images>、<https://docs.cloud.google.com/workstations/docs/reference/rest/v1/projects.locations.workstationClusters.workstationConfigs>
- Microsoft VFS for Git README：<https://github.com/microsoft/VFSForGit>

**一手工程博客**
- Gitpod / Ona，*We're leaving Kubernetes*（2024-10-31）：<https://ona.com/stories/we-are-leaving-kubernetes>
- Fly.io，*Machine migrations*：<https://fly.io/blog/machine-migrations/>
- Replit，*Replit Storage: The Next Generation*（2023-07-19）：<https://replit.com/blog/replit-storage-the-next-generation>，存档 <http://web.archive.org/web/20260720102157/https://replit.com/blog/replit-storage-the-next-generation>
- Replit，*Inside Replit's Snapshot Engine*（2025-12-17）：<https://replit.com/blog/inside-replits-snapshot-engine>，读自 Wayback 存档
- CodeSandbox，*How we clone a running VM in 2 seconds*（2022-09-01）：<https://codesandbox.io/blog/how-we-clone-a-running-vm-in-2-seconds>，存档 <http://web.archive.org/web/20260313051627/https://codesandbox.io/blog/how-we-clone-a-running-vm-in-2-seconds>
- CodeSandbox，*How we scale our microVM infrastructure using low-latency memory decompression*（2024-05-29）：<https://codesandbox.io/blog/how-we-scale-our-microvm-infrastructure-using-low-latency-memory-decompression>，读自 Wayback 存档
- Meta，*Sapling: Source control that's user-friendly and scalable*（2022-11-15）：<https://engineering.fb.com/2022/11/15/open-source/sapling-source-control-scalable/>
- Google，Potvin & Levenberg，*Why Google Stores Billions of Lines of Code in a Single Repository*，CACM 2016-07：<https://cacm.acm.org/research/why-google-stores-billions-of-lines-of-code-in-a-single-repository/>，存档 <http://web.archive.org/web/20251112010052/https://cacm.acm.org/research/why-google-stores-billions-of-lines-of-code-in-a-single-repository/>

**源码（钉死 commit / tag）**
- Gitpod Classic：`G/components/ws-daemon/pkg/controller/workspace_operations.go#L236-L280`、`#L347-L362`、`#L456-L561`；`G/components/ws-daemon/pkg/controller/workspace_controller.go#L377-L453`；`G/components/content-service/pkg/storage/storage.go#L26-L27`；`G/components/content-service/pkg/initializer/initializer.go#L420-L505`；`G/components/content-service/pkg/initializer/prebuild.go#L27-L191`；`G/components/content-service/pkg/initializer/git.go#L248-L258`；`G/components/ws-daemon/pkg/daemon/daemon.go#L198`
- E2B infra：`E/packages/orchestrator/pkg/sandbox/rootfs/nbd.go#L96-L100`、`#L170-L173`、`#L204-L209`；`E/packages/orchestrator/pkg/sandbox/block/streaming_chunk.go#L23`；`E/packages/orchestrator/pkg/sandbox/build/storage_diff.go#L105-L120`；`E/packages/shared/pkg/storage/header/mapping.go#L12-L40`
- Git v2.53.0：`GIT/Documentation/git-clone.adoc#L80-L105`、`#L180-L194`、`#L329-L334`；`GIT/Documentation/git.adoc#L496-L535`、`#L566-L572`；`GIT/Documentation/gitrepository-layout.adoc#L24-L32`、`#L80-L89`；`GIT/Documentation/config/core.adoc#L64-L79`、`#L114-L119`、`#L136-L150`、`#L327-L340`、`#L652-L655`；`GIT/Documentation/config/index.adoc#L34-L43`；`GIT/Documentation/config/feature.adoc#L34-L47`；`GIT/Documentation/config/worktree.adoc#L11-L19`；`GIT/Documentation/config/safe.adoc#L20-L24`；`GIT/Documentation/git-worktree.adoc#L69`、`#L144-L163`、`#L534-L536`；`GIT/Documentation/technical/racy-git.adoc#L24-L45`；`GIT/Documentation/git-update-index.adoc#L247-L255`、`#L477-L488`；`GIT/Documentation/technical/partial-clone.adoc#L40-L46`；`GIT/Documentation/git-sparse-checkout.adoc#L25-L26`；`GIT/Documentation/scalar.adoc#L24-L34`、`#L56-L68`；`GIT/Documentation/git-maintenance.adoc#L70-L72`、`#L133-L152`；`GIT/Documentation/git-fsmonitor--daemon.adoc#L73-L77`；`GIT/compat/fsmonitor/`
