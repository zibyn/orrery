# 重查 #36（ADR-0012 Resource / Artifact / Snapshot 持久化）：对照 Coder 与 Daytona

调研日期 **2026-10-17**。只用一手来源：官方文档、钉死版本源码、厂商自己仓库的 issue；厂商原文保留英文。没有做任何实测。

| 项目 | 钉死 | 简写 |
|---|---|---|
| coder/coder | tag v2.38.0，commit `c3c6a672821e8ac2472c45f3be7a3c600aa8dae1` | `C/` = `https://github.com/coder/coder/blob/c3c6a672821e8ac2472c45f3be7a3c600aa8dae1/` |
| daytonaio/daytona | tag v0.190.0 | `D/` = `https://github.com/daytonaio/daytona/blob/v0.190.0/` |

复用：`persistence-vendor-practice.md`（下称 **PVP**，泛厂商对照）、`daytona-sandbox-runtime-semantics.md`（**DSR**）、`recheck-runtime-and-workspace-vs-coder-daytona.md`（**RRW**）。这些文件已有的事实只给出处。被审对象：ADR-0012 全文、ADR-0014 的 OCI registry 段、ADR-0015 的磁盘条款、GLOSSARY。

---

## 先读这几条

1. **Artifact 的保留规则自相矛盾，可能永不过期。** ADR-0012 写 Artifact *"published as a ResourceVersion"*，又写 *"A current version never expires … Artifacts follow the same rule"*，但没说 Artifact 发布到哪个 Resource、会不会移动指针。如果每个 Artifact 自成一个 Resource，那它就是当前版本，永远不过期。GPU checkpoint 每份几十 GB，这会无界增长。Daytona 的 Volume 没有任何自动清理，有人专门提了 issue 要 `autoDeleteInterval`（<https://github.com/daytonaio/daytona/issues/4669>）。**应修改**，顺带**应简化**：把 Artifact 从「Exec 的声明式输出」改成一个「把 Workspace 路径发布为某个具名 Resource 的新版本」的操作（A1）。
2. **大于 5 GiB 的文件，现行上传设计验不了 SHA-256。** 单次 PUT 的上限是 5 GB（*"You can send a `PUT` request to upload an object of up to 5 GB in a single operation."*）。multipart 上传时，SHA-256 只支持 composite，不支持 full object（*"Full object checksums in multipart uploads are only available for CRC-based checksums … This type of validation isn't available for other algorithms, such as SHA and MD5."*，<https://docs.aws.amazon.com/AmazonS3/latest/userguide/checking-object-integrity-upload.html>）。单个模型权重或 checkpoint 超过 5 GiB 很常见，此时「存储写入时按文件 SHA-256 校验」做不到。两家都没碰到这个问题：Coder 上传上限 100 MiB，Daytona 的构建上下文不校验。**应修改**（U1）。
3. **平台基础镜像白名单比两家都重，挡不住的东西也说不清。** Daytona 接受 *"any publicly accessible image or container registry"*，只禁浮动 tag，另外提供平台预置的 `general` 快照（如 `daytona-gpu`）当默认起点。Coder 的模板里写什么镜像都行，官方 Docker 模板用的就是浮动 tag `codercom/example-base:ubuntu`。我们 Runtime 只接受 digest、容器非特权（RRW R6/R7），镜像内容本身并不会抬高权限。**应简化**：去掉白名单，接受任意 digest 作为基础；平台镜像降级为推荐默认；增加「按 digest 导入、不构建」的路径（S2）。
4. **SnapshotVersion 按 TTL 物理删除，比两家都激进。** Coder 从不删模板版本，只做可逆的 `archived`，而且 `files` 表不在 dbpurge 的清理范围内。Daytona 闲置 14 天的快照只转 `inactive`，把 runner 上的缓存清掉；快照删除时，源码里也找不到从内部 registry 删镜像的路径。我们的「引用」也没有定义。Coder 的定义是「任一未删除工作区的**最新** build 所用的版本」，停着的工作区也算在内。**应修改**：SnapshotVersion 不做 TTL 删除，只做显式删除；「引用」按 Coder 的口径写明，停止的 Sandbox 也算（R1）。
5. **Node 本地缓存没有淘汰规则，也不计入磁盘账。** ADR-0012 让 Node Agent 把 ResourceVersion 拉进 Node 本地缓存，然后只读挂载，但没写什么时候删。ADR-0015 的磁盘 watermark 也没把缓存算进去。Daytona 专门为 runner 上的快照缓存跑了三个定时任务（scale-down、BuildInfo 过期、inactive 清理）。GPU 数据集动辄数百 GB，这一条躲不掉。**应修改**（M1）。
6. **不可变只读 Resource 保留，不要改成 Daytona 式的可变 FUSE 卷。** Daytona 自己的 issue 把这条路的坑都列出来了：只能读写挂载，Agent 场景要求只读（#3340）；不是 POSIX，追加写、rename、随机写都会坏（#4844、#5006）；速度慢，用户要求本地卷（#2584）。我们的可变数据由 Node 本地 POSIX 的 Workspace 承担，Resource 只读且不可变，这些坑一个都碰不到。GPU 训练要的「把数据集和权重分发到 Node」，两家都没有像样的方案，这正是 Resource 存在的理由。

---

## 逐条对照表

编号：R* 对应 Resource，U* 对应上传，M* 对应挂载，A* 对应 Artifact，S* 对应 Snapshot，G* 对应保留与 GC。

| # | 裁决 | Coder | Daytona | 判定 |
|---|---|---|---|---|
| R0 | 数据用不可变 ResourceVersion；可变数据只放 Workspace | 不设数据对象，持久数据就是 Terraform 里的可变 `docker_volume`（`C/examples/templates/docker/main.tf#L147-L151`） | Volume 可变，FUSE + S3，*"the last write wins"*（`D/apps/docs/src/content/docs/en/volumes.mdx#L620`），**没有只读挂载**：runner 的 `VolumeDTO` 只有 `volumeId/mountPath/subpath` 三个字段（`D/apps/runner/pkg/api/dto/volume.go`） | **保留**：见下文 R0 |
| R0' | Resource = 稳定名 + 当前指针 | template 下的 `active_version_id` 指向一个不可变的 template version；`promote` 移动这个指针（`C/coderd/templateversions.go#L1435-L1450`） | Snapshot 是「名字 → 内容地址 ref」，多个名字可以共享同一个 ref（`D/apps/api/src/sandbox/managers/snapshot.manager.ts#L1510-L1512`）；同一个名字下没有版本历史 | **保留**：形状和 Coder 的 template → active version 一样 |
| R0'' | 指针更新可选 `If-Match` | `promote` 是无条件的，只检查版本 job 已成功、版本未归档（`C/coderd/templateversions.go#L1435-L1446`） | 没有指针 | **保留**：可选，不带时与 Coder 等价 |
| R2 | 单文件按原始字节做 SHA-256；目录用规范化 manifest；tar/zip 只是派生物；CAS 对象跨版本共享 | 对**整个 tar** 做 SHA-256，按 (hash, creator) 去重；zip 先转成 tar；内容存进 PostgreSQL 的 `files.data`；上限 100 MiB（`C/coderd/files.go#L32`、`#L83-L125`；`C/coderd/database/queries/files.sql`） | 构建上下文由 SDK 在客户端算 **MD5**，输入只有相对路径和内容，然后整个打成 tar 上传，key 为 `{org}/{md5}/context.tar`（`D/libs/sdk-python/src/daytona/_sync/object_storage.py#L62-L72`、`#L92-L130`）；镜像 ref 用的是镜像自身的 sha256 | **保留**：见下文 R2 |
| U1 | 调用方提交带 sha256 的 manifest；只为缺失的对象签发带 `x-amz-checksum-sha256` 的预签名 PUT；发布时检查对象是否齐全 | 内容经 API 上传，由服务端计算哈希（`C/coderd/files.go#L117-L118`） | SDK 拿 **STS 临时凭证**（1 小时，范围限定在本组织前缀）直接 PUT，已存在就跳过；服务端不校验（`D/apps/api/src/object-storage/services/object-storage.service.ts#L33-L93`；`object_storage.py#L66`） | **应修改**：见下文 U1 |
| M1 | 创建 Sandbox 时把引用解析为具体版本；Node 按内容地址缓存；只读挂载；不实时跟随指针 | 工作区 build 钉在某个 template version 上，*"Workspaces that are already running stay on their current version until you update them."*（`C/docs/admin/templates/managing-templates/index.md#L106-L108`） | Volume 是实时 FUSE 挂载，*"read from large files instantly"*（`volumes.mdx#L9`），写入立刻可见；runner 上的快照缓存有专门的清理任务（`snapshot.manager.ts#L104-L108`、`#L1315-L1340`、`#L1439-L1443`） | **应修改**：钉版本保留；补上缓存淘汰、磁盘记账、拉取阻塞 provisioning 三条 |
| A1 | Artifact = Exec 的显式声明输出，Exec 结束后收集，发布为 ResourceVersion | **没有**这个概念 | **没有**这个概念。最接近的是整机 archive：*"the entire filesystem state is moved to a cost-effective object storage"*（`D/apps/docs/src/content/docs/en/sandboxes.mdx#L1364`），以及沙箱备份（RRW W2） | **应简化 + 应修改**：见下文 A1 |
| A2 | 路径限定在 `/workspace`；拒绝越界、设备文件和外部挂载；符号链接不跟随 | 无 | Volume 的 subpath 和挂载路径会拒绝 `..`、`//` 与系统目录（`volumes.mdx#L134-L140`；`D/apps/runner/pkg/docker/volumes_mountpaths.go#L106-L112`） | **保留**：便宜，而且是安全边界（PVP §6） |
| A3 | 没有冻结屏障，manifest 以实际读到为准 | 无 | 没有冻结；整目录读取时 *"Neither recipe guarantees a consistent snapshot"*（PVP §2.5） | **保留** |
| S1 | Snapshot 与 Resource 分开；SnapshotVersion = 平台 registry（MinIO 后端）里的 OCI digest；在 Node 上以 `system` 作业 `docker build` 后 push | 不建镜像、不运营 registry；镜像由模板写死（`main.tf#L176`）；envbuilder 可以选配 registry 层缓存（`C/docs/admin/integrations/devcontainers/envbuilder/envbuilder-security-caching.md`） | **几乎同构**：`snapshot-manager` 就是 CNCF distribution v3，用 `s3-aws` driver（`D/apps/snapshot-manager/internal/server/server.go#L16-L20`、`config.go#L28-L44`）；runner 从对象存储取上下文再 `docker build`（`D/apps/runner/pkg/docker/image_build.go#L60-L69`），按镜像 hash 打 tag `daytona-{hash}:daytona` | **保留**：与 Daytona 一致 |
| S2 | 只能从平台维护的基础镜像 digest 名单构建 | 任意镜像，官方模板用浮动 tag | 任意公开或私有 registry 的镜像（`D/apps/docs/src/content/docs/en/snapshots.mdx#L298`），只要求 *"must include either a tag or a digest … `latest`/`lts`/`stable` tags are not supported"*（`#L34`）；平台另有 `general` 快照，*"available to all organizations"*（`D/apps/api/src/sandbox/entities/snapshot.entity.ts#L39-L44`） | **应简化**：见下文 S2 |
| S3 | 输入要声明：基础 digest、构建输入、引用的 ResourceVersion；Workspace 绝不作为构建上下文；provenance 不进地址；不承诺可复现 | 无 | 上下文只来自 SDK 显式上传的路径；构建以 `sha256(dockerfile + 排序后的 context hashes)` 去重（`D/apps/api/src/sandbox/entities/build-info.entity.ts#L11-L15`）；声明式镜像 *"cached for 24 hours"*（`D/apps/docs/src/content/docs/en/declarative-builder.mdx#L19`）；没有可复现承诺 | **保留** |
| S4 | 构建 Secret 不进产物、manifest 和日志 | 无 | 只看到拉私有基础镜像所用的 registry 凭证；**未找到**构建期 RUN secret 的 API | **保留**：BuildKit secret mount 本身就具备，成本接近零（PVP §4.6） |
| S5 | 状态 `building → ready \| failed \| cancelled` | 状态来自 provisioner job，有 `canceled`（PVP §4） | `building/pending/pulling/active/inactive/error/build_failed/removing`（`D/apps/api/src/sandbox/enums/snapshot-state.enum.ts`） | **保留**：S2 若加导入路径，可以只多一个 `pulling` |
| S6 | 不从运行中的 Sandbox 捕获 Snapshot；Workspace 不进 Snapshot | 无 | 有「从沙箱生成快照」和备份，做法是 `docker commit` 后 push（`D/apps/runner/pkg/docker/backup.go#L78-L110`） | **保留**：我们的 Workspace 是 bind mount，commit 拿不到它（RRW W2） |
| G1 | 当前版本永不过期；非当前且无引用的 ResourceVersion / SnapshotVersion 过 TTL 后过期 | 模板版本**从不删除**。路由里只有 `archive`/`unarchive`（`C/coderd/coderd.go#L1598-L1599`），归档注释写的是 *"a soft delete action, so is reversible"*；「在用」的定义是任一工作区最新的那次非 delete build（`C/coderd/database/queries/templateversions.sql#L151-L226`）。`dbpurge` 不清 `files` 表（`C/coderd/database/dbpurge/dbpurge.go`） | 快照**不会自动删除**。闲置 14 天转 `inactive`（`snapshot.manager.ts#L64`、`#L1361-L1410`），只清 runner 缓存；Volume 没有任何 TTL（issue #4669） | **应修改**：见下文 G1 |
| G2 | 墓碑；当前版本不能直接删；有活跃引用就不做物理清理；PG 为准；宽限期 + 幂等清扫；读不到 blob 时标 unavailable | 只有软删除（archive） | Volume：挂着时删返回 409（`D/apps/api/src/sandbox/services/volume.service.ts#L161-L162`）；`pending_delete → deleting → deleted`，行被改名为 `-deleted` 后保留（`D/apps/api/src/sandbox/managers/volume.manager.ts#L242-L285`）；bucket 非空时报错，让用户重试（`#L262-L263`）。Snapshot：删行时，只有同一 ref 下不再有 ACTIVE 快照，才清 runner 缓存（`snapshot.manager.ts#L812-L829`） | **保留**（Resource CAS 部分）：形状与 Daytona 一致，见下文 G2 |
| G3 | registry：Control Plane 在线删 manifest，blob GC 离线做（ADR-0014） | 无 | `removeImage` 只用于 transient registry（`snapshot.manager.ts#L1104-L1116`）；沙箱销毁时删它的备份仓库（`D/apps/api/src/sandbox/managers/backup.manager.ts#L951-L953`）；**未找到**删除内部 registry 中快照镜像的路径，也没看到 registry GC 配置 | **应简化**：随 G1，SnapshotVersion 只在显式删除时删 manifest；blob GC 写成运维手册，不进设计 |
| G4 | 不设存储配额，靠 TTL 和容量监控 | 单次上传上限 100 MiB（`files.go#L32`） | 每组织 100 个卷，卷数据 *"does not count against your storage quota"*（`volumes.mdx#L1123`）；快照有单个大小上限 `maxSnapshotSize`（`snapshot.manager.ts#L1520-L1530`） | **保留**：单团队；超限时报错的那一条见 U1 |
| G5 | registry = distribution + S3 driver；Node 证书做 mTLS 认证；`redirect.disable` | 无 | distribution + s3 driver + htpasswd（`D/apps/snapshot-manager/internal/server/config.go#L59-L66`） | **保留**：与 Daytona 同形，用 mTLS 替掉 htpasswd，少一份共享密码 |

---

## 需要展开的条目

### R0 不可变只读 Resource：保留。Daytona 的可变卷是一份反面清单

- Daytona 自己的文档就承认了限制：*"can not be used for applications that require block storage access … generally slower"*（`volumes.mdx#L1119`）。
- 用户在 Daytona 仓库里报告的问题，几乎条条都是我们这套设计绕开的：
  - **只读挂载**（<https://github.com/daytonaio/daytona/issues/3340>，open）：*"When an AI agent consumes ad-hoc scripts generated in a "Producer" sandbox, there is no infrastructure-level lock to prevent the agent from accidentally modifying or deleting those source artifacts."*，*"The suggested workaround is to bake assets into a Snapshot … too slow"*。我们的 Resource 天生只读，发布一个新版本也不用重建镜像。
  - **POSIX 语义**（#4844 已关，#5006 open）：*"appending to an existing file may be unsafe or unsupported"*；*"Atomic rename fails … Random-write access fails … No file locking"*；*"Agents writing model checkpoints, datasets, or intermediate results to a Volume will hit these errors today."*。维护者关掉 #4844 时给的理由是：*"it's up to the Daytona user to manage this since they're in control of the volume it self."*。我们这边，checkpoint 写在 Node 本地 POSIX 的 Workspace 里，写完再发布，所以不会碰到。
  - **慢**（#2584，open）：*"Daytona's current Volumes are S3-backed and slow"*，用户要的是 *"Local Volumes - NVMe-backed persistent storage on the same host"*。这正是我们 Workspace 的形态。
- Coder 不设数据对象，持久性全部交给 Terraform 卷。它最大的坑是「名字派生自可变属性，卷会被重建」（PVP §1.5）。我们的 Resource 名字和内容地址是分开的，不会踩到这一条。
- **两家为什么不做不可变数据版本**：没找到一手理由。【推断】Daytona 的定位是「通用共享目录」，有 last-write-wins 就够用；Coder 的数据都在工作区卷里，从来不跨工作区分发。我们多了 GPU 数据集和权重的分发需求，这一点两家都没有覆盖到。

### R2 规范化 manifest 加逐文件 CAS：保留，但要知道两家都只哈希整包

两家都对**整个归档**取哈希。Coder 是 tar 的 SHA-256，Daytona 是自己拼的 MD5，后者不含权限和符号链接，又由客户端计算、服务端信任。他们这么做站得住，因为对象小：Coder 有 100 MiB 上限，Daytona 只是构建上下文。我们的 Resource 要装数据集和权重。v2 只比 v1 多几个文件的情况下，只有逐文件 CAS 才能免掉重传，Node 缓存也才能按文件复用。这一层的复杂度是 GPU 场景要求的，保留。规范化规则本身有先例（REAPI、Nix，见 PVP §3）。

### U1 上传：应修改，补上大文件的边界

- 现行设计只描述了单次 PUT 加 `x-amz-checksum-sha256`。S3 的 PUT 单次上限是 5 GB；multipart 下 SHA-256 只有 composite，*"checksum of checksums"*，跟文件本身的 SHA-256 不是同一个值（AWS 文档，见「先读」第 2 条）。所以 >5 GiB 的文件没法让存储按 ResourceVersion 的内容地址在写入时校验。
- 两家没碰到这个问题：Coder 上限 100 MiB；Daytona 的 SDK 直接 PUT 一个 tar，而且不校验。
- **最小修改**：Phase 1 规定单文件 ≤ 5 GiB，超过就在 manifest 阶段返回显式错误；升级路径是 multipart（每个 part 带 SHA-256 composite）加上发布前由服务端或 Node 流式计算全文件 SHA-256。ADR-0012 的 Consequences 已经给 MinIO 留了「server-side hashing step」，大文件可以和它共用同一条后路。
- 另外，Daytona 用的是 **STS 临时凭证**，前缀限定、1 小时有效，没有走逐对象预签名（`object-storage.service.ts#L50-L93`）。我们逐对象签发，粒度更细，与 `s3-short-lived-authorization.md` 的结论一致，不用改。

### M1 挂载：钉版本保留，缓存要补三条规则

- 钉版本与 Coder 的「运行中的工作区留在原版本上」一致。Daytona 的实时 FUSE 恰好是 #3340 抱怨的那种情况。
- 两家都踩过、我们没防的有三处：
  1. **缓存淘汰**。Daytona 为 runner 上的快照镜像跑了三个定时任务：按需缩容（`snapshot.manager.ts#L104-L108`）、BuildInfo 过期（`#L1315-L1340`，配置项 `buildInfoSnapshotRunnerStalenessDays`）、inactive 快照清理（`#L1439-L1443`）；另外还会清孤儿 FUSE 挂载目录（`D/apps/runner/pkg/docker/volumes_cleanup.go#L25-L28`）。建议写一句：没有被任何非删除 Sandbox 引用的缓存条目，在磁盘低于 watermark 时按 LRU 淘汰。
  2. **磁盘记账**。ADR-0015 的 watermark 要把缓存算进去，否则一个 500 GB 的数据集可能把同一 Node 上 Workspace 和 Log Spool 的空间吃掉（RRW 第 3 条讲的就是这种连带）。
  3. **拉取时间**。Daytona 的 FUSE 是懒加载，我们是先拉完再挂载。所以 Sandbox 的 provisioning 要等拉取完成，这件事要写进状态说明（可见的 condition），免得被当成卡死。懒加载留作升级路径。

### A1 Artifact：应简化成一个「发布」操作，并补上归属与保留

- 两家都没有 Artifact。Daytona 维护者的立场是，卷里放什么、怎么用由用户自己管（#4844 的关闭理由）。【推断】两家不需要它：Daytona 的数据本来就写在 S3 卷里，Coder 的工作区卷本身就是持久存储。
- 我们确实需要「把数据从绑定在 Node 上的 Workspace 搬进对象存储」：GPU checkpoint 要在 Node 丢失后还在，ADR-0014 的 Node 退役路径也靠它。所以这个**能力**要保留。
- 但把它做成「Exec 的声明式输出」，是照搬 CI 的形态。CI 这样做，是因为 runner 的工作区在 job 结束后就被销毁（PVP §2）。我们的 Workspace 是持久的，Agent 又是交互式工作的，Exec 绑定反而带来几个悬而未决的问题：Exec 失败或超时还收不收？PTY Exec 何时算「结束」？要不要在创建 Exec 时就预知输出路径？
- **建议**：Artifact 改成 Workspace 上的一个操作，`publish(path → Resource 名)`，REST 和 Capability Channel 都能调用。由 Node Agent 读取（A2 的路径规则不变），发布为**指定 Resource** 的新版本，并移动指针（可带 `If-Match`）。这样 Artifact 只是「来源是 Workspace 的 ResourceVersion」，GLOSSARY 少一条独立生命周期，保留规则也自然明确：每个 Resource 只有最新的 checkpoint 是当前版本，旧版本按 TTL 过期，整个 Resource 要显式删除。
- 如果坚持保留 Exec 声明式，至少要补两句：Artifact 发布到哪个 Resource；它在 G1 规则下算不算当前版本。

### S2 基础镜像白名单：应简化

- 两家都接受任意镜像。Daytona 只禁浮动 tag，平台预置的 `general` 快照（如 GPU 用的 `daytona-gpu`，`D/apps/docs/src/content/docs/en/sandboxes.mdx#L123`）只是默认值，不是限制。
- 白名单要防的是什么，ADR-0012 没写。镜像内容不会抬高权限：容器非特权，挂载受限（RRW R6）。Exec Supervisor 是平台以只读方式挂进去的（ADR-0015），也不依赖镜像里装了什么。白名单的代价却很具体：每换一个 CUDA/PyTorch 版本都要 `admin` 介入。对 GPU 团队来说，这是最常换的东西。
- **两家为什么不限制**：没找到一手理由。【推断】用户自带环境是两家的核心卖点。
- **建议**：基础镜像接受任意 digest（比 Daytona 的「tag 或 digest」更严，与 ADR-0009 的「只认 digest」一致）。平台维护的镜像改成推荐默认。另加一条「按 digest 导入、不构建」的路径，对应 Daytona 的 `pulling`，省掉一个空 Dockerfile。拉取外部 registry 需要的出网，按 Egress Policy 处理，不另设白名单。GLOSSARY 的 Platform Base Image 随之改成「推荐起点」，或者删掉。

### G1 保留：SnapshotVersion 不做 TTL 删除；「引用」要写明

- 两家都**不自动删除环境版本**。Coder 只做可逆归档；Daytona 只把闲置快照转成 `inactive`，并清掉 Node 上的缓存。我们对 SnapshotVersion 做 TTL 物理删除，比两家都激进，换来的只是少几份镜像。单团队的镜像数量少，体积也远小于数据。
- 引用的口径照 Coder：版本被**任一未删除 Sandbox 的创建记录**引用即「在用」，**停止的 Sandbox 也算**，因为重启是一次新的 Create，要用同一个 digest（RRW R4）。正在构建、引用了某些输入的 SnapshotVersion 也算。Coder 的 SQL 把这一条写得很具体，「restart … would use the version」（`templateversions.sql#L192-L196`）。
- **建议**：SnapshotVersion 只在显式删除时删除，Node 缓存按 M1 淘汰（这就是 Daytona `inactive` 的实际效果）。ResourceVersion 保留 TTL，因为数据体积大，checkpoint 会持续产生。

### G2 GC：Resource 侧保留

Daytona 的 Volume 删除也是异步状态机加挂着时 409，墓碑行改名后保留；Snapshot 删除前也先确认同一 ref 没有别人在用，这就是 CAS 共享的引用检查。我们多出来的宽限期和幂等清扫，是因为对象被多个版本共享。Daytona 一卷一 bucket，不共享，所以不需要这两样。这一层是 R2 带来的必然成本，保留。

---

## 站在完整系统看：三件套是否过重？

| 我们的概念 | 两家对应 | 结论 |
|---|---|---|
| Snapshot / SnapshotVersion | Daytona Snapshot（同构）；Coder template version + 镜像 | 保留。去掉白名单、加导入、不做 TTL 删除以后，与 Daytona 基本对齐 |
| Resource / ResourceVersion | 两家都没有。Daytona Volume 是可变 FUSE 卷，Coder 用 Terraform 卷 | 保留：GPU 数据集和权重的只读分发是两家都没覆盖、而我们确实需要的；Daytona 的 issue 证明可变卷不适合这个用途 |
| Artifact | 两家都没有 | **不再作为独立概念**，并入 Resource：变成「从 Workspace 发布到 Resource」这一个操作 |
| Content Address、Platform Base Image | Coder 整 tar 的 SHA-256；Daytona 的镜像 hash ref 加 `general` 快照 | Content Address 保留为内部术语；Platform Base Image 改成推荐默认 |

改完以后，对外的持久化概念是两个：**Snapshot**（环境）和 **Resource**（只读数据，来源可以是上传，也可以是从 Workspace 发布）。再加一个 **Workspace**（可变、Node 本地）。Daytona 对外是 Snapshot + Volume + archive/backup 三个概念，数量与我们相当，但分工不同：可变数据在 Daytona 是 Volume，在我们是 Workspace；只读分发在我们是 Resource，Daytona 没有。

**两家都没有、确有必要的**：

1. GPU 数据集和权重的只读分发与 Node 本地缓存（R0、M1）。
2. 把 checkpoint 从绑定在 Node 上的 Workspace 搬进对象存储（A1）。Daytona 用 S3 卷和整机备份解决，Coder 不解决。
3. 大文件上传与完整性校验（U1）。两家的对象都小，不需要。

---

## 未查 / 未确认

- MinIO 是否在预签名 PUT 上校验 `x-amz-checksum-sha256`、是否支持 multipart 的 SHA-256 composite：未实测（ADR-0012 已列）。
- Daytona 构建期 RUN secret：在 v0.190.0 的 API 与 runner 中只看到 registry 凭证，没逐个文件确认不存在。
- Daytona 内部 registry 是否有部署层（Helm、cron）做 GC：只查了 `apps/` 源码，没查部署仓库。
- Coder 是否在别处（企业版、cron）清理 `files` 表：只查了 `dbpurge.go` 和 `files.sql`。

## 来源

- Coder（`C/` 前缀）：`coderd/files.go`、`coderd/database/queries/files.sql`、`coderd/database/queries/templateversions.sql`、`coderd/templateversions.go`、`coderd/coderd.go`、`coderd/database/dbpurge/dbpurge.go`、`examples/templates/docker/main.tf`、`docs/admin/templates/managing-templates/index.md`、`docs/admin/templates/extending-templates/resource-persistence.md`、`docs/admin/integrations/devcontainers/envbuilder/envbuilder-security-caching.md`；issue <https://github.com/coder/coder/issues/10192>
- Daytona（`D/` 前缀）：`apps/docs/src/content/docs/en/{volumes,snapshots,declarative-builder,sandboxes,mount-external-storage}.mdx`、`apps/api/src/sandbox/managers/{snapshot,volume,backup}.manager.ts`、`apps/api/src/sandbox/services/volume.service.ts`、`apps/api/src/sandbox/entities/{snapshot,volume,build-info}.entity.ts`、`apps/api/src/sandbox/enums/snapshot-state.enum.ts`、`apps/api/src/object-storage/services/object-storage.service.ts`、`apps/api/src/docker-registry/services/docker-registry.service.ts`、`apps/runner/pkg/docker/{volumes_mountpaths,volumes_cleanup,image_build,backup}.go`、`apps/runner/pkg/api/dto/{volume,snapshot}.go`、`apps/snapshot-manager/internal/server/{server,config}.go`、`libs/sdk-python/src/daytona/_sync/object_storage.py`；issues <https://github.com/daytonaio/daytona/issues/3340>、<https://github.com/daytonaio/daytona/issues/4844>、<https://github.com/daytonaio/daytona/issues/5006>、<https://github.com/daytonaio/daytona/issues/2584>、<https://github.com/daytonaio/daytona/issues/4669>、<https://github.com/daytonaio/daytona/issues/4413>
- AWS S3：<https://docs.aws.amazon.com/AmazonS3/latest/userguide/checking-object-integrity-upload.html>
