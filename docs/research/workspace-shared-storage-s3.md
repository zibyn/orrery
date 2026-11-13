# Workspace 跨 Node 共享存储：借鉴 JuiceFS 的 S3 方案

调研日期 **2026-11-13**。问题：Workspace 现在是 Node 本地目录（`<data-root>/…/workspace` 绑定挂载到 `/workspace`），磁盘空间和数据都绑死在一台 Node 上（GLOSSARY「Workspace」：*permanently bound to one compute Node*，Node 丢了 Workspace 就丢了）。要设计一套**以 S3 为后端的 Workspace 存储**，让磁盘空间跨 Node 共享、Workspace 能换 Node 启动、Node 丢失不丢已提交数据，并且对 Workspace 里的 Git 仓库读写正确且够快。不直接部署 JuiceFS，只借鉴它的设计。

一手来源：JuiceFS 社区版文档 [Architecture](https://juicefs.com/docs/community/architecture)、[Cache](https://juicefs.com/docs/community/guide/cache)、[Internals](https://juicefs.com/docs/community/internals)。仓库内事实来自 ADR-0012、ADR-0015、ADR-0021、ADR-0026、`docs/spec/runtime.md`、`docs/spec/schema.md` 和 `internal/{manifest,castransfer,objectstore,agent}`。

---

## 1. 现状里决定方案形状的约束

| 约束 | 出处 | 对方案的影响 |
|---|---|---|
| **单写者**：同一 Workspace 同时至多一个 Sandbox 读写挂载，由部分唯一索引 `sandboxes_workspace_rw` 保证 | GLOSSARY、ADR-0015（#220） | **最关键**。JuiceFS 复杂度的大头（多客户端共享元数据引擎、session 心跳、分布式锁、close-to-open 失效）在这里大部分不需要 |
| 租约以动作结束，不靠过期 | ADR-0015 | 不能照搬 JuiceFS 的 session 超时清理；写权的「围栏」用现有 Sandbox 状态 |
| Exec 在 Agent 重启后存活 | ADR-0015、ADR-0019 | 挂载进程不能是 Agent 本身，否则 Agent 升级会把 `/workspace` 拔掉 |
| Node 访问对象存储只走 Control Plane 签发的预签名 URL，带 `x-amz-checksum-sha256` | ADR-0012、ADR-0021、`castransfer` | Node 不持有长期 S3 密钥；对象按 SHA-256 寻址最顺手 |
| CAS 按 owner 去重，键 `cas/<owner_id>/sha256/<hex>` | ADR-0021、`objectstore.CASKey` | Workspace 的数据块可以直接放进同一命名空间，与 Resource 共享去重和 GC |
| PostgreSQL 是元数据权威，GC 先查库再宽限期再删 | ADR-0012 | Workspace 的「当前版本指针」放 PG，对象删除复用现有 sweep |
| Isolation Runtime 有 runc 和 runsc；Workspace 以宿主路径 bind mount 进容器 | ADR-0026、`agent/containerd.go` | 宿主上挂一个文件系统再 bind 进去，容器侧代码基本不变；runsc 经 gofer 访问宿主 FUSE 需实测 |

## 2. JuiceFS 怎么做（只取和本题相关的）

- **元数据与数据分离**。客户端（FUSE 等）同时连元数据引擎（Redis/TiKV/MySQL/PostgreSQL/SQLite）和对象存储。元数据存名字、属性、目录树、符号链接、锁、文件到数据的映射、引用计数、客户端 session。
- **Chunk / Slice / Block**。文件按偏移切 64 MiB 的 Chunk；一次连续写是一个 Slice（不跨 Chunk）；Slice 落盘时再切成 ≤4 MiB 的 Block，各自一个对象。重复写同一区域产生重叠 Slice，读取取最新；客户端异步 **compaction** 把同一 Chunk 内的 Slice 合并。
- **对象永不覆盖**。键为 `${fsname}/chunks/${hash}/${sliceId}_${index}_${size}`，sliceId 全局递增。修改只写新对象，旧对象经回收站或 compaction 之后延迟删除（`delfile`、`delslices`，回收站默认 1 天）。
- **一致性是 close-to-open**。`write` 只进客户端缓冲，`flush`/`fsync`/`close` 才持久化并对其他客户端可见。内核 attr/entry 缓存默认 1 秒；`--open-cache` 会削弱 close-to-open，默认关闭。
- **本地缓存**。从对象存储读来的 Block 缓存在本地盘（默认上限 100 GiB、保留 10% 空闲，近似 LRU）。
- **`--writeback`**。写入落到本地缓存就提交元数据并返回，异步上传；本地盘在上传前丢失则**数据永久丢失**；官方定位是「短时大量小文件写入，如解压」。
- **锁**。flock 和 POSIX 记录锁按 inode + session 存在元数据引擎里（SQL 下是 `jfs_flock`、`jfs_plock` 表）。
- **小文件没有合并机制**：小文件就是一个小 Block 对象，官方为避免读放大不做合并。

## 3. 借鉴什么、不借鉴什么

| JuiceFS 设计 | 取舍 | 理由 |
|---|---|---|
| 元数据 / 数据分离，数据切块进 S3 | **借鉴** | 这是空间跨 Node 共享的根本 |
| 对象不可变、只写新对象、延迟删除 | **借鉴**，并改为按 SHA-256 内容寻址 | 与 ADR-0021 的 CAS、预签名校验和、现有 GC sweep 一致；同 owner 的 Workspace 之间（例如同一仓库 clone 多份）天然去重 |
| 本地盘缓存 + writeback | **借鉴，且作为默认** | Git 的大量小写入和 `fsync` 如果每次都等 S3，`git checkout`/`git status` 不可用。单写者让 writeback 的风险只剩「本 Node 丢失 → 丢最后一次提交之后的改动」，比现在「Node 丢失 → 全丢」严格更好 |
| 每个元数据操作都打远端元数据引擎 | **不借鉴** | `git status` 对每个跟踪文件 `lstat`，十万级文件 × 一次 PG 往返不可接受。单写者下，写入 Node 本地持有完整元数据即可，远端只存**已提交的不可变版本** |
| 多客户端 session、心跳超时、远端 flock/plock 表 | **不借鉴** | 违反「不靠过期」；单写者下锁只在一台机器的内核/FUSE 进程里，本地处理即可 |
| Chunk/Slice 重叠写 + compaction | **简化为固定 4 MiB Block** | Slice 是为「没有本地完整副本的多写者」设计的。我们写者有本地副本，提交时只对脏 Block 重算哈希即可，不需要 Slice 层和它的 compaction |
| 小文件一对象 | **改进：小文件打包** | Git 的 loose object、工作区源码都是大量 KB 级文件；一文件一 PUT 的请求数和成本都太高。提交时把小文件拼成 pack 对象（restic/git packfile 的做法），元数据记 `(pack, offset, length, sha256)`，读用 Range GET |
| close-to-open 跨客户端可见 | **改为「只读挂载看某个已提交版本」** | 只读 Sandbox 看到的是一致的时间点快照，比 close-to-open 更强也更简单；顺带解决 ADR-0012 里「从 Workspace publish 没有冻结屏障」的问题——从一个已提交版本 publish 就是时间点一致的 |

## 4. 推荐方案：WSFS（单写者、版本化、S3 为底）

### 4.1 数据模型

- **Workspace Generation（工作区版本）**：一个 Workspace 的一次提交，不可变。内容是一份元数据（目录树 + 每个 inode 的属性 + 数据映射），数据全部是 CAS 对象。PG 里 `workspaces.head_generation` 指向最新的已提交版本。
- **元数据记录**（比 Resource manifest 多存，因为 Workspace 要能原样复原）：路径/inode、类型（file/dir/symlink，**硬链接用 inode 共享表达**）、完整 mode（不只可执行位）、uid/gid、mtime、size，以及数据映射：
  - 小文件（阈值暂定 ≤ 1 MiB，待测）：`{pack: sha256, offset, length, sha256}`；
  - 大文件：按偏移固定 4 MiB 的 Block 列表 `[sha256…]`，稀疏空洞记为空；
- **对象**：Block 和 pack 都进 `cas/<owner_id>/sha256/<hex>`，用现有 `castransfer.Put/Get` 和预签名带校验和。元数据本身也是对象：基线（全量）+ 增量段（每次提交只传变化的条目，像 LSM/Litestream），每 N 个增量合成一次新基线，避免百万文件的 Workspace 每次提交都传全量。
- **提交（commit）**，由写者 Node 执行：
  1. 冻结一个提交点：把当前脏集合（脏 inode、脏 Block）切出来，之后的写入进下一批；
  2. 对脏小文件打包、对脏 Block 重算 SHA-256，向 Control Plane 提交缺失对象列表（同 ADR-0012 的 manifest 提交流程），拿预签名 PUT 上传；
  3. 上传元数据增量段；
  4. Control Plane 在**一个事务里**校验提交者仍是写权持有者（该 Sandbox 的 `workspace_mode = 'rw'` 且处于持写状态——这就是现成的围栏，无需新租约），`HeadObject` 校验对象，写 `workspace_generations` 行并 CAS 推进 `head_generation`。

  提交时机：Sandbox stop 前（**stop 必须等最后一次提交成功才释放写权**）、周期性（默认如 60 秒且有脏数据）、脏数据量阈值、显式 API `checkpoint`。

### 4.2 Node 侧挂载（P0 已否决此形态，见 §4.6）

- 每个挂载一个 **`orrery wsfs` 进程**（与 Exec Supervisor 同样独立于 Agent，Agent 重启不影响挂载），用 FUSE（`hanwen/go-fuse/v2`，需新增依赖）挂在 `<data-root>/workspaces/<id>/mnt`，`containerd.go` 里 `bindMount(workspace, …)` 的源路径换成这个挂载点，其余不变；Local Runtime 直接用这个路径做 cwd。
- **写者**：元数据在本地（SQLite WAL 或内存 + 本地日志，均在 Node 本地盘），全部 `lookup/getattr/readdir/rename/create` 本地完成；数据写到本地暂存区（按 inode 的稀疏文件），`fsync` = 本地落盘后返回（writeback）。读未缓存的 Block 时按需从 S3 拉（**懒加载**，换 Node 启动无需先全量下载）。
- **只读者**：挂载某个固定 Generation，元数据从 S3 拉，数据懒加载 + 本地缓存；不跟随 head（要新内容就重建 Sandbox 或显式 remount）。
- **本地缓存**：Block 缓存按内容寻址，同一 Node 上所有 Workspace 共享，LRU + 水位线，水位线并入现有「Node 磁盘低于水位不接新分配」（ADR-0015）。脏数据和未提交元数据**不可淘汰**（JuiceFS writeback 的 `rawstaging` 教训）。
- **预签名吞吐**：懒加载一次可能要几百个 GET，每个都向 Control Plane 要 URL 会成为瓶颈。批量签名（一次请求签一个 Generation 的一批对象，带过期）是最小改动；若仍不够再按 `s3-short-lived-authorization.md` 给 Node 发限定前缀的短时凭据。

### 4.3 Git 专项

| Git 行为 | WSFS 要保证的 |
|---|---|
| `*.lock` 用 `O_CREAT\|O_EXCL` 创建再 `rename` 覆盖（index、refs、config） | 本地元数据上的原子 `rename`、排他创建；提交点切在两个元数据操作之间，不会出现半个 rename |
| `git status` 对所有跟踪文件 `lstat`，用 mtime/ctime/ino/size 判断是否变化 | 元数据本地化后 `lstat` 是本地操作；**换 Node 后 inode 号、ctime 都变**，第一次 `git status` 会重读全部文件刷新 index——要么在 Generation 里持久化 inode 号和 ctime 并按原值回报（推荐，FUSE 可自定这两个值），要么文档建议 `core.checkStat=minimal` |
| packfile、`.idx`、`index` 用 `mmap` 读 | FUSE 支持 mmap 读；懒加载 Block 在缺页时阻塞拉取 |
| `git gc`/`repack` 写新 pack、删旧 pack、loose object 大量增删 | 写入在本地，提交时才上传；被删对象由 Generation 不再引用 → 走现有宽限期 GC。频繁 gc 只放大对象存储写量，不影响正确性 |
| `git clone --local`、部分工具用硬链接 | inode 与路径分离，`nlink` 正确 |
| 符号链接、可执行位、空目录 | 元数据如实保存（比 Resource manifest 多存完整 mode） |
| `fsync`（取决于 git 版本与 `core.fsync` 配置） | writeback 下只等本地盘，故不拖慢 Git；持久化语义是「Node 存活即不丢，Node 丢失回退到上次提交」，要在能力文档里写清 |
| 其他工具的 `flock`/`fcntl` 锁（npm、SQLite 等） | 单写者 → 锁只需在本机生效；FUSE 未实现锁时内核按本地语义处理，需实测 runsc 下行为 |

FIFO、unix socket 可以在写者本地存在但不进入 Generation；设备文件拒绝（与 publish 的规则一致）。

### 4.4 Control Plane 变更

- `workspaces`：`node_id` 从「永久绑定」降级为「缓存亲和 Node」（可空）；新增 `head_generation`、`storage`（`local` 旧模式 / `wsfs`）。
- 新表 `workspace_generations`：`id, workspace_id, seq, meta_key, base_seq, size, sandbox_id, created_at`。
- 调度：`wsfs` Workspace 在任意可行 Node 上都能启动，优先有暖缓存的 `node_id`（并入 ADR-0015 的放置次序，最后一条平局规则前）。
- Node 宣告 lost：其上 `wsfs` Workspace 从 `head_generation` 恢复，记一个 condition 说明「最近一次提交之后的改动已丢失」。
- 保留与 GC：head 和被只读 Sandbox、pending publish 引用的 Generation 保留；其余在宽限期后删除行，对象由现有 CAS sweep 按引用回收。没有引用计数表，sweep 需要能枚举 Generation 元数据引用的对象（这是 GC 的主要新代码）。
- `publish(path → Resource)`：从一个 Generation 发布，只生成 manifest，**不重传数据**（对象已在同一 owner 的 CAS 里）。
- GLOSSARY「Workspace」与 ADR 需要改：它不再 *permanently bound to one compute Node*，「Node 丢失」从「永久丢失」变为「丢失未提交部分」。建议新开一条 ADR。

### 4.5 分期

1. ~~**P0 spike**~~ **已完成（2026-11-13，`workspace-wsfs-fuse-spike.md`）**：语义全部正确，但 FUSE 在热路径上让 runc 的 clone 慢约 12 倍、checkout 慢 6–9 倍，runsc 下 clone 约 20s（runc 宿主目录 0.8s）。判定失败，改走 §4.6。
2. **P1 提交与恢复**（按 §4.6）：Generation 格式、扫描提交、stop 前提交、换 Node 先恢复再启动、Node lost 恢复。
3. **P2 运维面**：GC 枚举、本地副本淘汰与水位、批量预签名、从 Generation publish、只读挂载（直接把某个 Generation 恢复成只读目录）。

### 4.6 修订后的推荐：本地目录为活副本，Generation 为 S3 上的持久版本

§4.1 的数据模型、§4.4 的 Control Plane 变更不变，只换 Node 侧：

- **运行时**：Workspace 仍是 Node 本地盘目录，原样 bind mount，Git 和 Files API 的性能、语义与现在完全相同，不引入 FUSE 和 go-fuse 依赖。
- **提交**：Agent 对目录做 stat 扫描（size、mtime、ctime、ino、mode），与上一个 Generation 的记录比较，只对变化的文件重算 SHA-256、打包、上传（P0 实测：15k 文件、212 MiB 的工作区，扫描 41–44ms，全量哈希 258ms）。Agent 本地保存上次提交的 stat 表，以免每次都从 S3 读元数据。
- **一致性**：stop 时没有写者，提交天然时间点一致。运行中的周期提交需要冻结点：用 Runtime 的 pause（cgroup freezer，runc/runsc 都支持）冻住 Sandbox，同时阻塞 Agent 自己的 Files API 写入，期间扫描并把变化的文件 reflink/复制到暂存区，然后解冻，再从暂存区慢慢上传。冻结时长 ≈ 扫描 + 复制变化文件。**未测**：pause 对正在运行的 Exec/PTY 的影响、reflink 依赖 Node 文件系统（XFS/btrfs，否则退化为复制）。
- **换 Node / 空间共享**：Workspace 停止且最后一次提交成功后，本地目录变成可淘汰的缓存，按水位线回收，空间也就释放了。在别的 Node 启动时，先把 head Generation 恢复到本地（对象按内容寻址，Node 上已有的块不重复下载），再启动 Sandbox。代价是大 Workspace 的启动时间与其大小成正比，没有懒加载。
- **Git 的 ino/ctime**：恢复到新目录后 ino、ctime 必然改变，第一次 `git status` 会重读跟踪文件刷新 index（耗时约等于对工作区做一遍哈希，P0 规模下是亚秒级），之后恢复正常。也可以由平台在 Snapshot 里默认 `core.checkStat=minimal`，这一项不做。
- **只读挂载**：把指定 Generation 恢复成一个只读目录再 bind 进去，同一 Node 上的多个只读者共享这份恢复结果。
- **懒加载留给以后**：只有当「大 Workspace 换 Node 的启动时间」成为真实问题时，再评估只读 FUSE 懒加载层（读多写少，FUSE 的写开销不再关键），或者等 Node 内核支持 FUSE passthrough（6.9+）后重测。

## 5. 被否决的方案

- **直接部署 JuiceFS**：题目排除；另外它的 session 超时、需要单独的元数据引擎、Node 持有 S3 长期密钥，都与现有 ADR 冲突。
- **照搬 JuiceFS 的「每操作访问 PG 元数据」**：Git 工作负载元数据密集，延迟不可接受；单写者已让远端元数据没有必要。
- **允许多写者**：要引入分布式锁、缓存失效和 session，整个复杂度回到 JuiceFS；现在没有需求打破单写者规则。
- **S3 上的块设备镜像（ext4 镜像切块，NBD/ublk 挂载）**：Git 性能最好、runsc 兼容最省心，但失去文件级去重、无法从 Generation 直接 publish、只读者无法便宜地挂载时间点版本、镜像大小需预分配。若 P0 显示 FUSE 在 runsc 下完全不可用、而又必须要懒加载，它是第二候选。
- **NFS/CephFS 等共享文件系统**：要运维新存储集群，与「对象存储是唯一外部存储」相悖（ADR-0014）。

## 6. 未确认、需实测

- runsc gofer 访问宿主 FUSE 挂载的语义与性能；FUSE 下 runsc 的 `flock`/`fcntl`。
- 小文件打包阈值、pack 大小、提交周期的默认值。
- 换 Node 后由 FUSE 按 Generation 里保存的原 inode 号和 ctime 回报，是否足以让 `git status` 不全量重读（需在 `core.trustctime`、`core.checkStat` 默认值下验证）。
- 目标 Git 版本下 `core.fsync` 的默认行为。
- 批量预签名在大仓库冷启动时的吞吐。
