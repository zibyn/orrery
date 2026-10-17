# 持久化（Resource / Artifact / Snapshot）vs 厂商做法：重新确认 #36

调研日期 **2026-10-17**。对象是已关票据 **#36「Lock Resource, Artifact, and Snapshot persistence」** 的裁决。只用一手来源（官方文档、API 参考、规范、源码）；厂商原文保留英文。

已有调研里能复用的事实直接引用，不重新取证：

- Daytona Volume / Snapshot 状态机、两周转 Inactive、卷删除 409：`daytona-sandbox-runtime-semantics.md` §1.6
- 各家沙箱文件面、Modal 快照 30 天 TTL、Vercel `keepLastSnapshots`、Cloudflare 快照 30 天、Runloop 快照「persist indefinitely」：`vendor-file-apis.md` §2.2–2.6
- 「没有一家沙箱厂商有声明的产物」、Buildkite `artifact_paths`：`vendor-file-apis.md` §0 第 3 条、§4
- GitHub Actions v4 直传签名 URL、90 天：`sandbox-file-ingress-egress.md` §3

本文件新增的一手取证：Modal Volumes、E2B（持久化 / 卷 / 模板 / 标签 / 缓存）、Runloop（Storage Object / Blueprint）、Vercel（Snapshots / Drives / Images）、Blaxel Volumes、Coder、GitHub `upload-artifact` 源码、GitLab CI artifacts 与 runner 源码、Argo Workflows、Bazel REAPI proto、OCI image-spec 与 distribution GC、BuildKit、Nix 文件系统对象模型。

---

## 0. 一句话结论

**#36 的形状不是自创的，但它把「模板/镜像那一族」的做法搬到了「数据那一族」。** 厂商的数据持久化一律是**可变卷**（last-write-wins、无版本）；「不可变构建 + 可移动名字指针」只出现在**环境模板**（E2B tag、Coder active version、OCI tag→digest）。「声明输出路径 → 执行者收走 → 目录树哈希」的完整先例在 **Bazel REAPI**（`output_paths` + `Tree` digest + canonical `Directory`），不在沙箱厂商。

---

## 1. 持久命名存储：可变卷，还是带当前指针的不可变版本？

| 厂商 | 持久数据对象 | 可变？ | 版本 / 时间点 | 并发 | 删除 |
|---|---|---|---|---|---|
| Daytona | Volume（FUSE + S3） | 可变 | 无 | 多挂，**last write wins** | 异步 `pending_delete`→`deleting`，挂着就 409，不可恢复（`daytona` §1.6） |
| Modal | Volume v1/v2 | 可变，**提交制** | 无版本历史 | 多写，**last write wins** | 立即且保证丢失，计费最长 4 天 |
| E2B | Volume（private beta）；暂停的沙箱 | 可变 | 无（快照是沙箱级） | 文档未写 | `Volume.destroy()`；暂停沙箱**永不过期** |
| Runloop | Storage Object；磁盘快照 | 文档未写对象能否改 | 快照不可变 | — | 「permanent and cannot be undone」 |
| Vercel | Drive；沙箱快照 | Drive 可变 | **Drive 有只读时间点快照**、可 fork | **单一读写挂载** | 挂着不能删；快照有 `deleted` 状态 |
| Cloudflare | R2 挂载；`DirectoryBackup` | 可变 | 备份记录由调用方的 DO 存 | 挂载不可并发写 | — |
| Blaxel | Volume | 可变（可 `readOnly`） | 文档未写 | **一卷只挂一个沙箱** | — |
| Coder | Terraform 资源（卷） | 可变 | 无 | — | 靠 `ignore_changes = all` 防误重建 |

### 1.1 Modal Volumes：最接近「提交 + 指针」的一家，但仍是 last-write-wins

来源：<https://modal.com/docs/guide/volumes>

- 提交制："any Volume changes made within a container need to be committed."；挂载时取最新态："At container creation time the latest state of an attached Volume is mounted."；看别人的改动要显式 reload："you need to explicitly reload the Volume to see changes made since it was first mounted."
- 后台提交："Modal Volumes run background commits" … "every few seconds while your Function or Sandbox executes"；收尾："A final snapshot and commit is also automatically performed on container shutdown."
- **冲突语义**："Last write wins in case of concurrent modification of the same file"，"any data the last writer didn't have when committing changes will be lost."；不提供锁（"nor is distributed file locking supported"）。v1 建议 "avoid doing more than 5 concurrent commits"。
- v2："We cannot yet guarantee that no data will be lost, so we don't recommend using Volumes v2 for mission-critical data."；v2 "only changes are written"、无文件数上限、支持硬链接。
- 删除："If you delete the volume, the data is guaranteed to be lost"；"you may still be billed for that storage for up to four days"；引用断掉："any deployed Apps or running Functions utilizing that Volume will cease to function"。

**对 #36**：Modal 的「commit」是**无条件**的——它把冲突交给 last-write-wins。#36 的「指针更新带 expected version、冲突返回 conflict」比它多一步，而这一步正是 Modal 文档里「数据会丢」那句话的反面。

### 1.2 E2B：数据侧可变；模板侧是本文最贴近 #36 的「名字指针」

来源：<https://docs.e2b.dev/sandbox/persistence>、<https://docs.e2b.dev/volumes>、<https://docs.e2b.dev/volumes/manage>

- 暂停沙箱："A paused sandbox is kept indefinitely"，"no time-to-live and no automatic deletion"，"There is currently no configurable "auto-kill after N days" option"；`kill()` 后 "permanently deleted and cannot be resumed"。
- Volume："Persistent storage that exists independently of sandboxes and can be mounted across multiple sandboxes."，"Volumes are currently in private beta."；管理面只有 create / connect / list / info / destroy，**没有版本**。

### 1.3 Runloop：Storage Object 没写不可变；Blueprint 同名多版本但没有指针

来源：<https://docs.runloop.ai/docs/storage-objects/overview>、<https://docs.runloop.ai/llms.txt>

- Storage Object："Storage Objects provide a way to store and manage files, data, and other resources"；删除 "permanent and cannot be undone"，"will no longer be able to access it"；取回走 "secure, time-limited URLs"。**是否可改、有无状态机、TTL：文档未写。**
- llms.txt 的摘要："objects persist and accrue charges otherwise"；磁盘快照 "snapshots persist and accrue storage charges until explicitly deleted"。

### 1.4 Vercel：唯一给数据卷做了「只读时间点」的一家

来源：<https://vercel.com/docs/sandbox/concepts/drives>

- "Only one sandbox at a time can mount a given drive as read-write: use snapshots for shared access."
- 快照挂载："Each snapshot mount reflects the drive as it was at the moment you mounted it, and does not include any writes made to the drive thereafter."——**只读时间点视图，但没有 id 可以长期引用**（每次 `drive.snapshot()` 现取）。
- Fork："Writes to a forked drive do not change the original drive"；血缘字段 `parentDriveId` / `rootDriveId`。
- 保留："Persists until manually deleted"；"A drive cannot be deleted if it is currently attached to a running sandbox."

### 1.5 Blaxel、Coder

- Blaxel（<https://docs.blaxel.ai/Sandboxes/Volumes>）："persistent storage that survives sandbox destruction and recreation"；"can only be attached to one sandbox at a time"；"you cannot detach a volume from a sandbox"。版本 / 保留：文档未写。
- Coder（<https://coder.com/docs/admin/templates/extending-templates/resource-persistence>）："By default, all Coder resources are persistent"；最大的坑是**名字派生自可变属性会被重建**："Terraform will recreate the volume (wiping its data!)"，建议用 `owner_id` / `id` 这种不可变标识，再加 `ignore_changes = all`。

**这条坑直接支持 #36 的一个细节**：Resource 是「稳定逻辑名」，版本有不可变地址——Coder 文档在告诉你，把持久数据绑到可变名字上会丢数据。

---

## 2. 输出 / 产物收集

### 2.1 GitHub Actions `upload-artifact@v4`

来源：<https://github.com/actions/upload-artifact>（README、`src/shared/search.ts`）、`actions/toolkit` 的 `path-and-artifact-name-validation.ts`

- **不可变**："Artifacts created by upload-artifact@v4 are immutable."；"Artifact names must be unique since each created artifact is idempotent so multiple jobs cannot modify the same artifact."
- `overwrite: true` 不是改写：同名旧产物先删，新产物拿 "a new ID, the previous one will no longer exist."
- **SHA-256 摘要输出**：`artifact-digest` = "SHA-256 digest of an Artifact"。
- 保留："Artifacts are retained for 90 days by default."；"Minimum 1 day."，"Maximum 90 days unless changed from the repository settings page."
- 路径："Relative and absolute file paths are both allowed."；多路径取 "the least common ancestor"；`..` 的处理 README 未写；文件名禁 `" : < > | * ? \r \n`。
- 隐藏文件默认不收："hidden files are ignored by this action to avoid unintentionally uploading sensitive information."
- ⚠️ **符号链接是跟随的**：`search.ts` 里 glob 选项是 `followSymbolicLinks: true`、`omitBrokenSymbolicLinks: true`。**与 #36「保留不跟随」相反。**
- 上传是步骤里的 action 自己发起的，不是平台代收（`vendor-file-apis.md` §4 已记）。

### 2.2 GitLab CI artifacts

来源：<https://docs.gitlab.com/ci/yaml/>、<https://docs.gitlab.com/ci/jobs/job_artifacts/>、`doc/administration/settings/continuous_integration.md`、`gitlab-runner/commands/helpers/file_archiver.go`

- 路径以 `$CI_PROJECT_DIR` 为根，"can't directly link outside it"；runner 源码：`artifact path is not a subpath of project directory` 时**跳过并警告，不让 job 失败**（"Do not fail job when a file is invalid or not found."）。
- glob 用 `doublestar.WithNoFollow()`（注释："to prevent symlink cycles during the initial glob"），随后 `filepath.Walk`（lstat，不进入链接目录）。**推断**：链接作为条目归档、不跟随；文档面只写了缓存一侧 "symlinks are no longer followed"（Runner 18.1）。
- `artifacts:untracked`：收所有 git 未跟踪文件，且 "ignores configuration in the repository's `.gitignore`"。
- 过期：未设 `expire_in` 用实例默认，"The default expiration time is 30 days."；`never` 永不过期；"artifacts are deleted hourly by default (using a cron job), and are not accessible anymore."
- **保留最新**：每个 ref 的最新 pipeline 和最近一次成功 pipeline 的产物不受 `expire_in` 约束。
- 手动删除："a destructive action that cannot be reverted"。

### 2.3 Argo Workflows

来源：<https://argo-workflows.readthedocs.io/en/latest/walk-through/artifacts/>

- 声明：`outputs.artifacts[].path`，"the artifact's `path` may be a directory rather than just a file."
- 打包："Artifacts are packaged as Tarballs and gzipped by default."；`archive: none` 原样上传。
- GC："Artifacts can be deleted `OnWorkflowCompletion` or `OnWorkflowDeletion`"，单个产物可覆盖为 `Never`；失败产生 "Artifact GC Failure" 条件，在 `ARGO_ARTIFACT_GC_RETRY_WINDOW`（默认 60 分钟）内重试；靠 finalizer `workflows.argoproj.io/artifact-gc` 挡住 Workflow 删除，`forceFinalizerRemoval` 放行。已不存在不算失败（幂等）。
- ⚠️ "Argo-workflows explicitly allows path traversal where a key containing "../" may allow users to traverse up the directory structure"。

### 2.4 Bazel REAPI：最贴近 #36 Artifact 的一家

来源：`bazelbuild/remote-apis` 的 `build/bazel/remote/execution/v2/remote_execution.proto`

- `Command.output_paths`："Only the listed paths will be returned to the client as output." … "Other files or directories that may be created during command execution are discarded."；目录输出 "will be returned as a Tree message digest"。路径必须相对、无首尾斜杠，且 "MUST be deduplicated and sorted lexicographically"。
- 输出里的符号链接单独成 `output_symlinks`（"if the file was a symbolic link to another file"）——**作为链接返回，不解引用**。

### 2.5 沙箱厂商有没有一等「产物」？

**没有。** `vendor-file-apis.md` §0 第 3 条已核过九家沙箱：都是按需拉的文件面；本轮新查的 E2B、Vercel、Blaxel 也只是卷 / 快照 / 文件 API。「声明 → 执行者收走」只存在于 CI / 构建系统：Buildkite `artifact_paths`、GitLab `artifacts:paths`、Argo `outputs.artifacts`、REAPI `output_paths`。

**一致性栅栏**：四家都在**命令结束后**收，天然没有并发写者；没有一家描述「冻结工作区」。`vendor-file-apis.md` 记过的 Daytona 甚至明说整目录读 "Neither recipe guarantees a consistent snapshot if files change during the operation."

---

## 3. 内容寻址：谁公开 CAS / 目录清单？

| 系统 | 单文件 | 目录 | 权限 | 时间戳 | 符号链接 | 排序 |
|---|---|---|---|---|---|---|
| Bazel REAPI | `Digest{hash,size}` | `Directory` proto 的 digest，递归；`Tree` 打平 | `unix_mode` **可选** NodeProperty | `mtime` **可选** | `SymlinkNode{name,target}` | **MUST** 按 UTF-8 字节序 |
| Nix | flat（raw bytes 哈希） | NAR 序列化 | **只有可执行位** | 不编码 | 有，target 是任意串 | NAR 规范内定 |
| OCI | blob digest（SHA-256 MUST 实现） | manifest 引用 layer（tar），**tar 本身不规范化** | tar 内 | tar 内（BuildKit 可改写） | tar 内 | 不保证 |
| GitHub v4 | — | 整个 zip 的 SHA-256 | zip 内 | zip 内 | 跟随 | — |

### 3.1 Bazel REAPI

- canonical form："In order to ensure that two equivalent directory trees hash to the same value, the following restrictions MUST be obeyed" … "The files, directories and symlinks in the directory must each be sorted in lexicographical order by path. The path strings must be sorted by code point, equivalently, by UTF-8 bytes." … "A `Directory` that obeys the restrictions is said to be in canonical form."
- `NodeProperties` 里 `mtime`、`unix_mode` 都是可选字段；`FileNode.is_executable` 是独立布尔。
- 符号链接 target："`..` components are allowed anywhere in the target path as logical canonicalization may lead to different behavior in the presence of directory symlinks" … "canonicalization is still recommended where this is possible without impacting correctness."
- **GC 语义**："The lifetime of entries in the CAS is implementation specific, but it SHOULD be long enough to allow for newly-added and recently looked-up entries to be used in subsequent calls"；`FindMissingBlobs`："Servers SHOULD increase the lifetimes of the referenced blobs"；ActionCache 返回结果时引用的 blob "will be for some period of time afterwards"。即**协议只规定「被摸过就续命」，回收是实现自由的 TTL/LRU，不是引用计数**。

### 3.2 Nix

来源：<https://nix.dev/manual/nix/stable/store/file-system-object>、<https://nix.dev/manual/nix/stable/store/file-system-object/content-address>

- 三种对象：Regular File（"A single boolean representing the executable permission"）、Directory、Symbolic link（"An arbitrary string, known as the target"）；"Nix does not encode any other file system notions such as hard links, permissions, timestamps, or other metadata."
- 为什么不用 tar："They do not have a canonical serialisation"，"TAR files can have variable amounts of padding"，"They store more information than we have in our notion of FSOs, such as time stamps."；单文件用 flat 哈希，"matching tools such as `sha256sum`"。

**这与 #36 的「单文件按原始字节、目录按规范清单、tar/zip 是派生物」逐条同构。**

### 3.3 OCI 镜像与 distribution GC

来源：<https://github.com/opencontainers/image-spec/blob/main/descriptor.md>、<https://distribution.github.io/distribution/about/garbage-collection/>、`distribution/docs/content/about/configuration.md`

- "Implementations MUST implement SHA-256 digest verification"；"Retrieved content SHOULD be verified against this digest when consumed via untrusted sources."
- GC 是 mark-and-sweep，"stop-the-world garbage collection"；⚠️ "You should ensure that the registry is in read-only mode or not running at all." 否则并发上传会让 "the image's layers are mistakenly deleted leading to a corrupted image"。
- 两段式删除：API 删 manifest 只是去引用并使其不可读，blob 在下次 GC 才被移除；被任一 manifest 引用的 layer 保留。`--delete-untagged` 删无 tag 的 manifest。
- **未完成上传的清扫**：`uploadpurging` 默认开启，`age: 168h`（"Upload directories which are older than this age will be deleted. Defaults to `168h` (1 week)."），`interval: 24h`。

**对 #36**：「数据库可见性检查 + 宽限期 + 幂等清扫」正是为了**不需要** distribution 那种只读窗口。distribution 自己的上传清扫用的就是「按年龄 + 宽限」——#36 的 unpublished upload 清扫与它同形。

### 3.4 BuildKit

来源：`moby/buildkit` 的 `docs/buildkitd.toml.md`、`docs/build-repro.md`

- 缓存 GC 是**空间驱动**的：`reservedSpace` / `maxUsedSpace` / `minFreeSpace`，每条策略可带 `keepDuration = "48h"`。
- 可复现性：`SOURCE_DATE_EPOCH` 固定 image config 的 `created` 与 history 时间；镜像内文件时间要另开 `rewrite-timestamp=true`；文档自认缺口（`compatibility-version=10` 下拒绝 zstd）。

### 3.5 自定义规范化目录清单：先例还是新发明？

**先例。** REAPI `Directory`（排序 + 可选 mode + symlink target）与 Nix NAR（只保留可执行位 + 无时间戳）两套成熟设计都在做同一件事。#36 的「固定权限表示」更接近 Nix（REAPI 把 mode 留为可选）。**沙箱厂商没有一家对外暴露目录内容地址**；唯一给用户看摘要的是 GitHub v4 的整包 SHA-256——那是归档的哈希，不是规范化目录的哈希（同内容、不同压缩级别就会变）。

---

## 4. 环境模板

| 厂商 | 构建入口 | 基础镜像限制 | 状态机 | 名字 → 构建 | 缓存键 | 闲置/保留 |
|---|---|---|---|---|---|---|
| Daytona | 镜像 / Dockerfile / 声明式构建器 / 从沙箱捕获 | 必须带 tag 或 digest，**禁 `latest`/`lts`/`stable`**；任意公开或已配私有仓库 | Pending / Building / Pulling / Snapshotting / Active / Inactive / Error / Build Failed / Removing | 文档未写名字能否复用 | 声明式镜像「cached for 24 hours」 | **2 周不用转 Inactive** |
| E2B | SDK 构建器 / CLI | 任意 registry；**拒绝无 `/etc/os-release` 的镜像**（scratch、distroless） | `building` / `ready` / `error` | **`name:tag` → `build_id`，tag 可移动** | 层按命令+输入；文件按内容 | 文档未写 |
| Modal | `Image` 方法链 / Dockerfile / registry | 无限制 | 文档未写 | 不适用 | 按层定义 | 不适用 |
| Runloop | Dockerfile / `system_setup_commands` / 以 blueprint 为基 | 任意公开 registry；runloop 基础镜像只是推荐 | 文档页未列 | 同名多版本，**无自动指针**，CLI `prune` 只留最新 | 文档未写 | 「persist indefinitely」 |
| Vercel | 推到 VCR 的 OCI 镜像；或沙箱快照 | **只从 Vercel 自家 registry（VCR）拉** | 镜像：Ready / Preparing / Unoptimized；快照：created / deleted / failed | 镜像 tag / digest | — | 快照默认上次使用后 30 天；`keepLastSnapshots` 1–10 |
| Coder | Terraform 模板版本 | 不适用 | provisioner job：pending / running / succeeded / canceling / canceled / failed / unknown | **template → active version** | data source 导入时读一次 | 版本可 `archived` |

### 4.1 Daytona

来源：<https://www.daytona.io/docs/en/snapshots>、<https://www.daytona.io/docs/en/declarative-builder>

- "include either a tag or a digest"；"the latest/lts/stable tags are not supported"。
- "Snapshots automatically become inactive after 2 weeks of not being used."；"Deactivated snapshots are not available for new sandboxes"；重新激活 Inactive → Pending。
- "Deleted snapshots cannot be recovered."；"Deleting a snapshot also deletes its warm pools"。
- 声明式构建器："Declarative images are cached for 24 hours, and are automatically reused when running the same script."；**没有任何可复现性承诺**，也没写缓存键怎么算。

### 4.2 E2B

来源：<https://docs.e2b.dev/template/tags>、`/template/how-it-works`、`/template/build`、`/template/caching`、`/template/base-image`、`/sandbox/snapshots`

- 标签：`my-template` 等价 `my-template:default`；"you can start a sandbox from a specific build by passing its `build_id` directly"；`assign_tags` "Assign new tag(s) to an existing build" 用于晋升；"Removing a tag does not delete the build artifact."
- 构建结果是一次快照："Snapshot the sandbox and make it ready for you to spawn it"；内核 "fixed at template build time"。
- 状态：`{ status: 'building' | 'ready' | 'error', logEntries: [...] }`；构建 "up to 1 hour. If a build exceeds this, it will be terminated."
- 缓存："Each layer is cached based on the command and its inputs"；"we cache the files based on their content"；`.dockerignore` 中的文件 "excluded from uploads and hash calculations"。
- 基础镜像："Use any Docker image from Docker Hub or other registries"；"Unsupported images fail the build with a clear error naming the reason."
- 模板 vs 快照：E2B 自己推荐声明式模板优于捕获快照（"templates are faster and more resource-efficient than snapshots"）。

**对 #36**：E2B 的 `name:tag → build_id` 就是「稳定名 + 指向不可变构建的当前指针」，而且**移动指针不删构建**。但 `assign_tags` 是无条件的，没有 expected-version。

### 4.3 Runloop

来源：<https://docs.runloop.ai/docs/devboxes/blueprints/lifecycle>、`/blueprints/dockerfile-customization`

- 同名多版本："you create multiple versions of a Blueprint with the same name"；清理办法是自己列出同名的、留最新、删其余；CLI "Prune old blueprint versions, keeping only the latest for each name"。
- "By default, Blueprints persist indefinitely and continue to incur storage costs."；"this action cannot be undone"。
- 基础镜像："This process works with any image that is available on Docker Hub."
- 构建密钥：`secrets={"NPM_TOKEN": "npm-token"}` 注入 `dockerfile` 与 `system_setup_commands`；**是否进镜像：文档未写**。

### 4.4 Modal Images

来源：<https://modal.com/docs/guide/images>

- "Modal uses the definition of an Image to determine whether it needs to be rebuilt."；"Images are cached per layer"。
- 可复现性靠用户 pin："recommend pinning dependencies tightly"；平台侧有 **Image Builder Version**——"a workspace level-configuration that will be used for every Image built in your workspace"，升级后镜像会重建。

**对 #36**：#36 把 `builder version` 列为 SnapshotVersion 的声明输入，**Modal 的 Image Builder Version 是这条的直接先例**。

### 4.5 Vercel

来源：<https://vercel.com/docs/sandbox/concepts/images>、<https://vercel.com/docs/sandbox/concepts/snapshots>

- "Sandbox pulls images from Vercel Container Registry (VCR)"；"VCR only serves an image to Sandbox once it has prepared an optimized `linux/amd64` build."
- 托管镜像每晚重发，滚动 tag 自动带安全补丁；支持 `@sha256:` 固定。
- **快照状态里有 `deleted`**："The snapshot was removed, either because you called `delete()` or because it reached its expiration."；"each time you create a sandbox from a snapshot, its expiration timer resets"。
- `keepLastSnapshots`：`count` 1–10，`deleteEvicted` 默认 `true`（"evicted snapshots are deleted immediately"）。
- 快照跨 sandbox 存活："deleting a single sandbox can't remove a snapshot that other sandboxes still start from."

### 4.6 构建期密钥

Docker 一手文档："Build arguments and environment variables are inappropriate for passing secrets to your build, because they persist in the final image."；secret mount "makes it temporarily available inside the build container, for the duration of the build instruction."（<https://docs.docker.com/build/building/secrets/>）。沙箱厂商里只有 Runloop 文档写了构建密钥，且**没说是否进镜像**。

### 4.7 三个问题的回答

- **有没有人把用户限制在平台基础镜像上？** 没有严格的白名单。最接近的三家：Vercel 只从**自家 registry** 拉（可以推任意镜像进去）；Daytona 禁浮动 tag；E2B 拒绝无发行版标识的镜像。
- **构建状态机？** 都有，形状一致：进行中 → 就绪 / 失败（Daytona 9 态、E2B 3 态、Coder 7 态）。显式 `cancelled` 只有 Coder 有。
- **可复现性承诺？** 没有一家承诺。Modal 和 BuildKit 把可复现性交给用户 pin / `SOURCE_DATE_EPOCH`，Daytona 只承诺 24 小时缓存。

---

## 5. 保留与 GC

| 对象 | 默认 | 机制 |
|---|---|---|
| GitHub artifact | 90 天（1–90） | 到期删除 |
| GitLab artifact | 实例默认 30 天；`never` 可选 | 每小时 cron；每个 ref 保留最新 |
| Argo artifact | 按策略 | 跟随 Workflow 完成/删除，finalizer 挡删除，60 分钟重试 |
| Modal 沙箱快照 | 30 天 | `vendor-file-apis.md` §2.3 |
| Cloudflare 快照 | 创建或最近恢复后 30 天 | `vendor-file-apis.md` §2.6 |
| Vercel 快照 | 最近使用后 30 天，可设 0 | 数量上限 + TTL；`deleted` 状态 |
| Daytona Snapshot | 2 周不用转 Inactive（不删） | 显式 activate |
| E2B 暂停沙箱 / Runloop 快照与 Blueprint / Vercel Drive / Modal Volume | **永久** | 只有显式删除 |
| REAPI CAS | 实现自定 | 被摸过即续命 |
| distribution | 不过期 | 离线 mark-and-sweep；未完成上传 168h |
| BuildKit cache | 空间阈值 + `keepDuration` | 后台 GC |

**软删除 / 墓碑**：

- **可见墓碑**：Vercel 快照的 `deleted` 状态；Daytona 的 `Removing`、卷的 `pending_delete`；Coder 模板版本 `archived`。
- **两段式**：distribution（API 删 → 不可读，GC 才删 blob）；GitLab（到期 → "not accessible anymore"，cron 删）；Argo（finalizer + GC Pod）。
- **引用阻止删除**：Daytona 卷挂着时 409、父沙箱有 fork 子时不能删；Vercel Drive 挂着不能删；Coder 模板有运行中的 workspace 时不能删；distribution 被任一 manifest 引用的 layer 保留。
- **可恢复的软删除：没有一家。** 所有用户可见的删除都写着 irreversible / cannot be undone / cannot be recovered。

---

## 6. #36 逐条对照

判定四档：**先例相符** / **我们比厂商重** / **没有厂商这么做** / **厂商有、我们缺**。

| #36 要素 | 证据 | 判定 |
|---|---|---|
| Resource = 稳定名 + 当前版本指针，每次变更发布不可变 ResourceVersion | 模板侧普遍：E2B `name:tag → build_id`（§4.2）、Coder active version（§4 表）、OCI tag→digest。**数据侧没有一家**：Daytona / Modal / E2B / Blaxel / Vercel Drive 都是可变卷（§1） | **先例相符**（形状借自模板/registry）；**我们比厂商重**（数据侧厂商一律可变卷） |
| 指针更新为比较并交换，冲突返回 conflict | Modal：无条件 commit + last-write-wins + "data … will be lost"（§1.1）；Daytona 同；E2B `assign_tags` 无条件 | **没有厂商这么做**（成本低：PostgreSQL 一个条件 UPDATE）。Vercel 的另一种做法是「单一读写挂载」（§1.4） |
| 单文件 SHA-256 按原始字节 | OCI MUST SHA-256；Nix flat = `sha256sum`；GitHub v4 输出 SHA-256 | **先例相符** |
| 目录用规范化清单（排序路径、类型、固定权限、大小、地址、链接目标；不含时间戳等） | REAPI canonical `Directory`（MUST 排序，mode/mtime 可选）；Nix（只有可执行位，无时间戳）（§3.1–3.2） | **先例相符**（非新发明）；**没有沙箱厂商对外暴露**。固定权限的取舍更接近 Nix |
| tar/zip 是派生的下载物 | Nix 明确拒绝 tar 作为哈希输入；Argo 默认 tgz、GitHub 哈希的是 zip | **先例相符**（Nix/REAPI）；CI 系统把归档当本体，比我们粗 |
| CAS 对象共享，权属/保留按版本 | REAPI CAS 去重 + 实现自定生命周期；distribution 跨仓库共享 blob | **先例相符** |
| Artifact 只能是 Exec 的显式输出 | REAPI `output_paths`（未列出的 "are discarded"）、Buildkite、GitLab、Argo；沙箱厂商都没有（§2.5） | **先例相符**（CI/构建系统）；沙箱厂商**没有这个概念** |
| 路径限于 /workspace，拒绝穿越 | GitLab runner：非项目子路径即拒绝（但只警告不失败）；REAPI 只允许相对路径；**Argo 明确允许 `../`**；GitHub 允许绝对路径 | **先例相符**；我们「失败而非跳过」比 GitLab 严 |
| 拒绝设备/特殊文件与外部挂载 | 本轮查到的一手资料里没有一家写这条（REAPI 的类型只有 file/dir/symlink，等于隐式拒绝） | **我们比厂商重**（但便宜、是安全边界，不建议删） |
| 符号链接保留、不跟随 | REAPI `output_symlinks`、Nix symlink 节点、GitLab runner 推断不跟随；**GitHub v4 `followSymbolicLinks: true`** | **先例相符**（REAPI/Nix）；GitHub 是反例 |
| 短暂冻结 Workspace 栅栏，不一致就失败 | 四家 CI 都在命令结束后收，没有一家冻结；Daytona 明说不保证一致 | **没有厂商这么做**——别人靠「命令结束后再收」得到一致性。若 #36 的 Artifact 也在 Exec 结束后收，栅栏可能只需「这个 Workspace 没有别的 Exec 在跑」 |
| Snapshot 与 Resource 分离 | E2B 模板 vs 卷、Daytona Snapshot vs Volume、Vercel 快照 vs Drive，都是两个对象 | **先例相符** |
| SnapshotVersion 生命周期 building → ready/failed/cancelled | E2B `building/ready/error`、Daytona 9 态、Coder 含 `canceled` | **先例相符** |
| 只从平台批准的基础镜像 digest 构建 | 无白名单：Vercel 只限自家 registry、Daytona 禁浮动 tag、E2B 拒绝无发行版镜像；Runloop / Modal / E2B 允许任意镜像 | **我们比厂商重**（Daytona 的「禁 latest」是最近的一档） |
| 声明的输入（含 builder 版本、策略版本），Workspace 不作为隐式构建上下文 | Modal Image Builder Version；E2B 缓存按命令+输入、文件按内容；**没有一家承诺可复现** | **先例相符**（builder version 有先例）；「策略版本」作为输入**没有厂商这么做** |
| 密钥可作短时构建输入，绝不进输出 | Docker：build arg/env "persist in the final image"，secret mount 只活一条指令；Runloop 构建密钥未说明是否进镜像 | **先例相符**（BuildKit secret mount） |
| 只有 ready 且已持久化的才可运行，输入过期不影响输出 | Vercel 镜像 `image_not_ready`；E2B 删 tag 不删构建 | **先例相符** |
| 删除先逻辑后物理；当前版本不能直接删；删 Resource 产生墓碑 | 可见墓碑：Vercel `deleted`、Daytona `Removing`/`pending_delete`、Coder `archived`；两段式：distribution、GitLab | **先例相符** |
| 活跃引用阻止物理清理 | Daytona 409、Vercel Drive、Coder 模板、distribution mark 阶段 | **先例相符** |
| 宽限期 + 幂等可重试 GC / 孤儿清扫 | distribution `uploadpurging` 168h；Argo 60 分钟重试 + 已不存在不算失败；GitLab 每小时 cron | **先例相符**；且比 distribution 的只读窗口更轻 |
| 版本读不到 blob 时标为不可用、修复/重试，不删 | 本轮没找到任何厂商写这条 | **没有厂商这么做**（代价是一个状态值） |
| 默认保留策略 | 厂商都有默认值：GitHub 90 天、GitLab 30 天 + 保留最新、Vercel 30 天 + 最多 10 份、Daytona 2 周闲置；#36 只写「可配置宽限期」，没写版本保留 | **厂商有、我们缺**：非当前 ResourceVersion / 未引用 SnapshotVersion / Artifact 的**默认保留规则**（TTL 或「保留最近 N 份」） |
| 闲置模板降级 | Daytona 2 周转 Inactive、可重新激活 | **厂商有、我们缺**（YAGNI 候选：单团队规模用不上） |
| 只读共享时间点 / fork | Vercel Drive 快照与 fork | 不缺：不可变 ResourceVersion 本身就是可共享的时间点 |

### 6.1 归纳

- **先例相符**：内容寻址、规范化目录清单（REAPI/Nix）、声明式输出收集（REAPI/CI）、保留不跟随的符号链接、Snapshot 分离与构建状态机、构建密钥不进输出、逻辑删除 + 引用阻止 + 宽限期清扫。
- **我们比厂商重**：数据侧用不可变版本而非可变卷；只允许平台批准的基础镜像；显式拒绝特殊文件。前两条是有意的取舍，第三条便宜且属安全边界。
- **没有厂商这么做**：指针 CAS（厂商都是 last-write-wins）、Workspace 冻结栅栏（厂商都在命令结束后收）、把策略版本作为构建输入、blob 丢失时标为不可用而不删。
- **厂商有、我们缺**：默认版本/产物保留规则（TTL 或保留最近 N 份）。闲置模板降级是可选项。

---

## 7. 未查 / 未确认

- E2B Volume 的并发、删除、保留：页面指向 "Volumes beta limitations"，未读。
- Runloop Storage Object 能否改写、Blueprint 构建状态枚举、构建密钥是否进镜像：文档未写。
- Blaxel Volume 的版本与保留：页面未写。
- GitLab artifacts 符号链接：runner 源码推断不跟随，归档器写法未逐行读。
- OCI distribution spec 有没有条件 tag 更新：未查。
- Modal Images 构建失败状态：文档未写。

---

## 来源

- Modal Volumes：<https://modal.com/docs/guide/volumes>
- Modal Images：<https://modal.com/docs/guide/images>
- E2B：<https://docs.e2b.dev/sandbox/persistence>、<https://docs.e2b.dev/volumes>、<https://docs.e2b.dev/volumes/manage>、<https://docs.e2b.dev/template/quickstart>、<https://docs.e2b.dev/template/tags>、<https://docs.e2b.dev/template/how-it-works>、<https://docs.e2b.dev/template/build>、<https://docs.e2b.dev/template/caching>、<https://docs.e2b.dev/template/base-image>、<https://docs.e2b.dev/sandbox/snapshots>
- Runloop：<https://docs.runloop.ai/docs/storage-objects/overview>、<https://docs.runloop.ai/docs/devboxes/blueprints/lifecycle>、<https://docs.runloop.ai/docs/devboxes/blueprints/dockerfile-customization>、<https://docs.runloop.ai/llms.txt>
- Daytona：<https://www.daytona.io/docs/en/snapshots>、<https://www.daytona.io/docs/en/declarative-builder>、<https://www.daytona.io/docs/en/volumes>（经 `daytona-sandbox-runtime-semantics.md`）
- Vercel：<https://vercel.com/docs/sandbox/concepts>、<https://vercel.com/docs/sandbox/concepts/snapshots>、<https://vercel.com/docs/sandbox/concepts/drives>、<https://vercel.com/docs/sandbox/concepts/images>
- Blaxel：<https://docs.blaxel.ai/Sandboxes/Volumes>
- Coder：<https://coder.com/docs/admin/templates/extending-templates/resource-persistence>、<https://coder.com/docs/admin/templates/managing-templates>、`coder/coder` 的 `codersdk/provisionerdaemons.go`、`codersdk/templateversions.go`
- GitHub：<https://github.com/actions/upload-artifact>（README、`src/shared/search.ts`）、`actions/toolkit` 的 `packages/artifact/src/internal/upload/path-and-artifact-name-validation.ts`
- GitLab：<https://docs.gitlab.com/ci/yaml/>、<https://docs.gitlab.com/ci/jobs/job_artifacts/>、`gitlab-org/gitlab` 的 `doc/administration/settings/continuous_integration.md`、`gitlab-org/gitlab-runner` 的 `commands/helpers/file_archiver.go`
- Argo：<https://argo-workflows.readthedocs.io/en/latest/walk-through/artifacts/>
- Bazel REAPI：`bazelbuild/remote-apis` 的 `build/bazel/remote/execution/v2/remote_execution.proto`
- OCI / distribution：<https://github.com/opencontainers/image-spec/blob/main/descriptor.md>、<https://distribution.github.io/distribution/about/garbage-collection/>、`distribution/distribution` 的 `docs/content/about/configuration.md`
- BuildKit / Docker：`moby/buildkit` 的 `docs/buildkitd.toml.md`、`docs/build-repro.md`；<https://docs.docker.com/build/building/secrets/>
- Nix：<https://nix.dev/manual/nix/stable/store/file-system-object>、<https://nix.dev/manual/nix/stable/store/file-system-object/content-address>
