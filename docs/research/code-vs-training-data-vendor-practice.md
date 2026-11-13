# 代码与训练数据分层存储：沙箱 / 开发环境 / ML 训练平台的做法

调研日期 **2026-11-13**。问题：Workspace 怎么跨 Node，背景见 `workspace-shared-storage-s3.md` §4.6 和 `workspace-wsfs-fuse-spike.md`。P0 实测 FUSE 在热路径上会让 Git 慢 6–25 倍，于是当前方案是三层：`/workspace` 留在本地盘，按 Generation 提交到 S3；数据集放只读 Resource；检查点这类可变大文件是否用一个 JuiceFS 式的 FUSE `/data` 承载，还没定。本文核对各家平台怎样把**代码 / 环境 / 小文件热数据**和**数据集 / 检查点 / 大文件**分开存放。

只用一手来源：官方文档、API 参考、源码。厂商原文保留英文，每条结论后附来源。

已有调研里的事实直接引用，不重新取证：

- Daytona Volume（FUSE + S3、last write wins、不能当块设备、删除 409）、按 class 的停止语义：`daytona-sandbox-runtime-semantics.md` §1.5、§1.6
- Modal Volumes 的提交制、后台提交、last-write-wins、v2 的「不保证不丢数据」：`persistence-vendor-practice.md` §1.1
- E2B 暂停永不过期、Volume private beta 且无版本：`persistence-vendor-practice.md` §1.2
- Runloop Storage Object、磁盘快照：`persistence-vendor-practice.md` §1.3、`vendor-file-apis.md` §2.5
- Coder「名字派生自可变属性会重建卷」：`persistence-vendor-practice.md` §1.5
- Modal Sandbox 文件面、Filesystem Snapshot 30 天 TTL：`vendor-file-apis.md` §2.3
- JuiceFS 架构与缓存：`workspace-shared-storage-s3.md` §2

**来源说明**：本环境连不上 `docs.aws.amazon.com`（TLS 被断开，WebFetch 和 curl 都失败）。SageMaker 一节改用 AWS 自己的文档仓库 `awsdocs/amazon-sagemaker-developer-guide`。该仓库 **2023-06 已归档**，内容可能落后于现行文档，下文标为「归档文档」。

---

## 0. 先读这几条

1. **没有一家把 Git / 代码放在 FUSE-over-对象存储上，并且有文档写明这样推荐。** 写得最直白的是 E2B："Run builds, package installs, and Git operations on local disk, then copy the output to the volume." Daytona 说 FUSE 卷 "generally slower … compared to the local sandbox file system"，Modal CloudBucketMount 说 "renaming files, are not supported"，Vertex `/gcs` 说 "A renaming operation is not atomic"。只有 Lightning Studio 的 home 可能例外：它持久保存 git 下载的内容，但底层是什么，文档没写（未确认）。
2. **代码 / 环境层几乎都在本地盘或块卷上，靠「停止时快照 / 归档、启动时恢复」跨机。** 这一层与 orrery 的 Generation 方案最接近的是 **Anyscale workspace**：本地项目目录每 5 分钟快照一次，终止时再快照一次，建议小于 10 GB，大数据移到 shared storage 或对象存储。Daytona（容器类沙箱归档到对象存储）、Runloop（suspend 时快照磁盘）、Modal（Filesystem Snapshot）、Codespaces（`/workspaces` 是持久目录）也属于这一类。
3. **数据层分两派。** 一派是对象存储上的懒加载 FUSE：Daytona/E2B/Modal Volume、Modal CloudBucketMount（基于 Mountpoint）、Vertex `/gcs`、SageMaker FastFile、Anyscale shared storage、JuiceFS/Fluid CSI。另一派是网络文件系统或块卷：FSx Lustre、EFS、Filestore/NFS、RunPod 网络卷、Lightning 高性能卷、Kubeflow 的 PVC。每家都写了小文件的坑，建议也一致：打包成少量大文件。
4. **检查点也分两派。** 「写本地，再上传到对象存储」：SageMaker `/opt/ml/checkpoints` 持续同步 S3；Ray `train.report` 把本地临时目录上传到 `storage_path`；Determined `store_path()` 退出时上传到一个 UUID 目录。Vertex 的建议也是 "Use local storage for checkpointing … instead of Cloud Storage"。另一派「直接写共享挂载」：Modal 把 `output_dir` 设到 Volume 里，Anyscale 用 `/mnt/shared_storage`，RunPod 用 `/workspace` 网络卷，Lightning 用 Drive。Ray 和 Determined 每个检查点都是**新的不可变目录**，与 orrery 的 Resource / Artifact 模型同构。

---

## 1. 沙箱 / 开发环境平台

### 1.1 Daytona

| 类别 | 挂载路径 | 底层 | 跨节点 | 一致性 / 持久性 | 坑 |
|---|---|---|---|---|---|
| 沙箱文件系统（代码、环境） | 整个 rootfs，默认 home `/home/daytona` | runner 本地盘（container 类）；VM 类停止时 "offloads it to nearby storage" | container 类：停止后仍留在 runner 上，**archive** 时移到对象存储（默认停止 7 天后）；GPU 类停止即删 | 与单个沙箱绑定："deleting the sandbox deletes its state" | GPU 类 "filesystem is not preserved"（`daytona` §1.5、`vendor-file-apis.md` §2.2） |
| Volume（数据集、模型权重、构建缓存） | 自选绝对路径，示例 `/home/daytona/volume`，可带 `subpath`（S3 key 前缀） | **FUSE + S3 兼容对象存储** | 多个沙箱可同时挂载，"read from large files instantly - no need to upload files manually to each sandbox" | "Sandboxes that mount the same volume see writes immediately, but FUSE-backed volumes are not transactional … the last write wins." | "can not be used for applications that require block storage access (like database tables)"；"generally slower for both read and write operations compared to the local sandbox file system." |

来源：<https://www.daytona.io/docs/en/volumes/>，原文 "Use volumes to share datasets, model weights, build caches, or application state between sandboxes"。其余见 `daytona-sandbox-runtime-semantics.md` §1.5、§1.6。

**检查点**：没有专门的文档。按上面的分类，检查点只能写进 Volume。GPU 沙箱停止即删，所以 GPU 训练的检查点**必须**放 Volume（`daytona` §1.5，官方给的绕法就是 volumes）。

### 1.2 E2B

| 类别 | 挂载路径 | 底层 | 跨节点 | 一致性 / 持久性 | 坑 |
|---|---|---|---|---|---|
| 沙箱文件系统 | rootfs，home `/home/user` | 沙箱本地盘 | pause 保存文件系统和内存，resume 后恢复；"A paused sandbox is kept indefinitely" | 与沙箱同生命周期；`kill()` 后不可恢复 | 磁盘 Hobby 10 GiB / Pro 20 GiB（`vendor-file-apis.md` §2.1） |
| Volume（private beta） | 自选，示例 `/mnt/my-data` | 网络文件系统，具体实现文档没写（未确认） | "can be mounted across multiple sandboxes over time" | 文档没写一致性模型 | 见下 |
| 自挂 bucket | 示例 `/home/user/bucket` | 用户在模板里装 `gcsfuse` / `s3fs` / `blobfuse2`，运行时 `sudo` 挂载 | 由用户自理 | 取决于所用 FUSE 实现 | 平台不托管 |

E2B 的 Volume beta 限制页是本文**与 P0 结论最直接对应**的一手来源（<https://docs.e2b.dev/faq/volumes-beta-limitations.md>）：

- "**File locking can hang.** Keep SQLite databases and tools that use `flock()` or `fcntl()` locks on local disk."
- "**File metadata caching is disabled.** Repeated file lookups and metadata checks incur network overhead, so workloads with many small operations can be slow. **Run builds, package installs, and Git operations on local disk, then copy the output to the volume.**"
- "**Stale file handles can occur.** Heavy file creation, renaming, or deletion can cause `ESTALE` errors."
- "**Mounts are set at sandbox creation.**"；"The API and SDKs do not support snapshots, server-side copies, or read-only mounts."

来源：<https://docs.e2b.dev/volumes.md>、<https://docs.e2b.dev/sandbox/connect-bucket.md>、<https://docs.e2b.dev/sandbox/persistence.md>。

**检查点**：没有专门的文档（未确认）。

### 1.3 Modal（Sandboxes + Volumes + CloudBucketMount）

| 类别 | 挂载路径 | 底层 | 跨节点 | 一致性 / 持久性 | 坑 |
|---|---|---|---|---|---|
| Image + 容器 rootfs（代码、环境） | 示例 `workdir: "/repo"` | 镜像层 + 容器本地盘 | Filesystem Snapshot 保存为 Image，从它启动新沙箱；超过 24h 的工作建议 "using Filesystem Snapshots to preserve its state" | Snapshot 默认 30 天 TTL（`vendor-file-apis.md` §2.3） | 代码一般在 readiness probe 里 "pulling code with git pull" |
| Volume（权重、数据集、检查点） | 自选，示例 `/data`、`/vol` | Modal 自研分布式文件系统，"caching and chunking optimizations built-in" | "distributed by default … alongside Modal's global compute pool" | 提交制：后台提交加关闭时提交，last write wins，没有分布式锁（`persistence-vendor-practice.md` §1.1） | 见下 |
| CloudBucketMount（S3/R2/GCS） | 自选，示例 `/my-mount`，可 `key_prefix`、`read_only=True` | "built on top of AWS' mountpoint technology and inherits its limitations" | 直接读写桶 | 同 Mountpoint | "optimized for reading large files sequentially. Certain file operations, such as renaming files, are not supported." |

Volume 的定位和小文件坑（<https://modal.com/docs/guide/volumes>）：

- "They are optimized for **write-once, read-many** I/O workloads, like creating machine learning model weights and distributing them for inference."
- v1："Volumes work best when they contain less than 50,000 files and directories. The latency to attach or modify a Volume scales linearly with the number of files"；硬上限 "500,000 inodes"，超过后返回 `ENOSPC`。v2 没有文件数上限，"random accesses have improved performance"，可以在挂载点上执行 `sync` 触发提交。

**检查点**："To periodically checkpoint into a modal.Volume, just set the Trainer's `output_dir` to a directory in the Volume."，即**直接写共享卷**，靠后台提交持久化。模型权重的推荐做法同样是 "store them in a Modal Volume"（<https://modal.com/docs/guide/model-weights>）。

来源：<https://modal.com/docs/guide/sandbox>、<https://modal.com/docs/guide/cloud-bucket-mounts>。

### 1.4 Coder

| 类别 | 挂载路径 | 底层 | 跨节点 | 一致性 / 持久性 | 坑 |
|---|---|---|---|---|---|
| home 卷（代码、dotfiles、环境） | `/home/coder` | Kubernetes 模板用 `kubernetes_persistent_volume_claim_v1`，`access_modes = ["ReadWriteOnce"]`，大小由 `home_disk_size` 参数决定；Docker 模板用 `docker_volume` 加 `ignore_changes = all` | 取决于集群的 StorageClass：块卷 detach/attach，或者绑在一个节点上（由部署决定） | 块设备，本地 POSIX 语义 | 卷名派生自可变属性时，Terraform 会重建卷并清空数据（`persistence-vendor-practice.md` §1.5） |
| 数据集 / 检查点 | — | 平台不提供，由模板作者自己加卷 | — | — | — |

来源：<https://github.com/coder/coder/blob/main/examples/templates/kubernetes/main.tf>、<https://github.com/coder/coder/blob/main/examples/templates/docker/main.tf>。

### 1.5 Runloop

| 类别 | 挂载路径 | 底层 | 跨节点 | 一致性 / 持久性 | 坑 |
|---|---|---|---|---|---|
| Devbox 磁盘（代码、环境） | rootfs，home `/home/user` | VM 磁盘 | suspend 时 "The Devbox disk is being snapshotted"，resume 时 "The Devbox disk is being loaded" | "Only disk state, not in-memory state is preserved during suspend/resume" | — |
| Code Mount | home 下 | 创建时 **git clone**："This will clone the repo onto the Devbox" | 每个 devbox 各克隆一份 | 本地盘 | 只能在创建时声明 |
| Object Mount（数据集、权重、归档） | 自选 `object_path` | Storage Object，创建时放到磁盘上；"Archive objects are automatically extracted to the specified path." | 显式复制，不是 FUSE | 本地副本 | "ideal for large files, datasets, model weights, and archives that you want to reuse across multiple Devboxes" |

来源：<https://docs.runloop.ai/docs/devboxes/mounts/code-mounts.md>、<https://docs.runloop.ai/docs/devboxes/mounts/object-mounts.md>、<https://docs.runloop.ai/docs/devboxes/lifecycle.md>。

**检查点**：没有文档（未确认）。Runloop 的数据进入沙箱完全靠**创建时复制或解包**，没有共享挂载。

### 1.6 GitHub Codespaces

| 类别 | 挂载路径 | 底层 | 跨节点 | 一致性 / 持久性 | 坑 |
|---|---|---|---|---|---|
| 仓库 | `/workspaces` | 克隆到宿主 VM 上，"subsequently mounted into the dev container" | 文档没写换宿主的机制（未确认） | "preserved when you stop and start the codespace, and when you rebuild the container" | 先浅克隆，后补全量历史 |
| `/workspaces` 之外 | 容器 rootfs | 容器层 | — | stop/start 保留，**rebuild 时丢失**（`/tmp` 另算） | 想跨 rebuild 保留文件，要 symlink 到 `/workspaces` |
| 数据集 / 检查点 | — | 不提供 | — | — | — |

来源：<https://docs.github.com/en/codespaces/about-codespaces/deep-dive>，原文 "This is a persistent directory that is mounted into the container."

---

## 2. ML 训练平台

### 2.1 AWS SageMaker Training（归档文档）

| 类别 | 挂载路径 | 底层 | 跨节点 | 一致性 / 持久性 | 坑 |
|---|---|---|---|---|---|
| 代码 | 训练镜像 / 脚本 | ECR 镜像 | 每个任务拉镜像 | 不可变 | — |
| 数据集，File 模式（默认） | `/opt/ml/input/data/<channel>` | 启动前把 S3 **全量下载**到实例卷 | 显式复制 | 本地副本 | "Training starts after the full dataset has been downloaded"；文件多、文件小时下载开销会增加 |
| 数据集，FastFile 模式 | 同上 | S3 **懒加载**，"POSIX-compliant file system interface"，"streams S3 content on demand" | 懒加载 FUSE 式 | 只读输入 | "works best when data is read sequentially"；随机读 "throughput might be lower" |
| 数据集，Pipe 模式 | FIFO | 预取后写入命名管道 | 流 | — | "Each pipe may only be read by a single process"，"largely replaced by … fast file mode" |
| 数据集，FSx for Lustre / EFS | 挂载到 `/opt/ml/input/data/<channel>` | 网络文件系统；Lustre 可以链接到 S3 bucket | 挂载 | 文件系统语义 | 需要 VPC；Lustre 是单 AZ；"cold start issue due to lazy loading"；文件系统通道 "must use `File` mode" |
| 检查点 | `/opt/ml/checkpoints`（可改） | 本地目录 **持续同步** 到 `checkpoint_s3_uri` | 新任务启动时从 S3 恢复到本地路径 | 见下 | 见下 |
| 最终模型 / 输出 | `/opt/ml/model`、`/opt/ml/output` | 结束时打成 tar，作为单个对象上传 S3 | — | — | 多个容器写同一目录时文件名不能冲突 |

小文件建议：数据集小于 50–100 GB 但文件小于 50 MB 时，"consider serializing groups of such small files into fewer larger file containers (such as 150 MB per file)"。FSx for Lustre "is ideal when you have many small files"。

检查点原文（<https://github.com/awsdocs/amazon-sagemaker-developer-guide/blob/master/doc_source/model-checkpoints.md>）：

- "the checkpoint files are saved under a local directory of the containers (the default is `/opt/ml/checkpoints`). SageMaker provides the functionality to copy the checkpoints from the local path to Amazon S3 and automatically syncs the checkpoints in that directory with Amazon S3."
- "Existing checkpoints in S3 are written to the SageMaker container at the start of the job … Checkpoints added to the S3 folder after the job has started are not copied to the training container."
- "**If a checkpoint is deleted in the SageMaker container, it will also be deleted in the S3 folder.**"（镜像同步，不是追加）
- "To avoid overwrites in distributed training with multiple instances, you must manually configure the checkpoint file names and paths"

来源：<https://github.com/awsdocs/amazon-sagemaker-developer-guide/blob/master/doc_source/model-access-training-data.md>、`your-algorithms-training-algo-running-container.md`、`your-algorithms-training-algo-output.md`（同一仓库）。

### 2.2 Google Vertex AI 自定义训练（现名 Gemini Enterprise Agent Platform serverless training）

| 类别 | 挂载路径 | 底层 | 跨节点 | 一致性 / 持久性 | 坑 |
|---|---|---|---|---|---|
| 代码 | 训练容器 / Python 包 | Artifact Registry / GCS | 每个任务拉取 | 不可变 | — |
| 数据集 | `/gcs/<bucket>/…` | **Cloud Storage FUSE** | 每个节点都挂载 | rename 不是原子操作 | "store data in larger and fewer files"；"high throughput for large file sequential reads" |
| 数据集（NFS） | `/mnt/nfs/<mountPoint>` | Filestore 或自建 NFS，通过 `nfsMounts` 配置 | 网络文件系统 | NFS 语义 | 需要 VPC 对等连接 |
| 检查点 | `AIP_CHECKPOINT_DIR`（"a Cloud Storage URI of a directory intended for saving checkpoints"） | GCS | — | — | 同一页又建议 **"Use local storage for checkpointing and logs instead of Cloud Storage."** |

两条放在一起读：平台给检查点一个 GCS 目的地，但不建议通过 `/gcs` FUSE 直接写，应该先写本地再复制过去。具体由谁复制，文档没写（由框架或用户负责，未确认）。

来源：<https://docs.cloud.google.com/vertex-ai/docs/training/cloud-storage-file-system>（原文 "Avoid renaming directories. A renaming operation is not atomic in Cloud Storage FUSE."）、<https://docs.cloud.google.com/vertex-ai/docs/training/code-requirements>、<https://docs.cloud.google.com/vertex-ai/docs/training/train-nfs-share>。

### 2.3 Lightning AI Studios

| 类别 | 挂载路径 | 底层 | 跨节点 | 一致性 / 持久性 | 坑 |
|---|---|---|---|---|---|
| Studio home（代码、conda 环境、git 下载） | `~` = `/teamspace/studios/this_studio` | 文档没写（未确认） | 在 CPU 和 GPU 机器之间切换时保留（"Press a button and switch to a GPU"） | "Everything within the Studio home … is saved automatically"；包括 "downloaded files (via git, etc.)" | 每个 Studio 只能有一个 conda 环境 |
| Drive（团队共享） | `lit://<org>/<teamspace>/…`，Studio 内路径另见下 | "a shared filesystem across all Studios"，可以接 S3 / EFS | 共享 | 文档没写 | 示例："Train a model on Studio A, access the checkpoints from Studio B." |
| Data connections（S3/GCS/R2/EFS） | Studio 内的挂载路径文档没写（未确认） | 用户自己的桶 | 团队空间内共享 | — | S3/GCS "will be extremely slow if you have … millions of tiny files"，建议用 LitData 预处理 |
| 高性能卷 | `/teamspace/lightning_storage/<name>` | "network mounts rather than buckets"，固定大小 | **绑定在区域 / 计算位置上** | 读写 | Drive 浏览不了它 |

来源：<https://lightning.ai/docs/platform/build/ai-studio.md>、<https://lightning.ai/docs/platform/data/drive.md>、<https://lightning.ai/docs/platform/data/cloud-folders.md>、<https://lightning.ai/docs/platform/data/data-connections/s3-buckets.md>。

**检查点**：放在 Drive 或高性能卷上，即直接写共享存储。Studio home 用什么实现持久化（快照还是网络盘）未确认，所以这里不能当作「代码放在网络 FS 上」的证据。

### 2.4 Anyscale / Ray Train

**Anyscale 的存储分层**（<https://docs.anyscale.com/configuration/storage>、<https://docs.anyscale.com/storage/shared>、<https://docs.anyscale.com/workspaces/files>）：

| 类别 | 挂载路径 | 底层 | 跨节点 | 一致性 / 持久性 | 坑 |
|---|---|---|---|---|---|
| Workspace 项目文件（代码、配置） | `/home/ray/<project-name>` | 本地，**周期快照**：重启或终止时快照，另外 "every five minutes" | 用 Ray `working_dir` 注入集群各节点；从 workspace 提交 job 时也注入 | 崩溃可能丢掉上次快照之后的改动 | "keeping workspace files under 10 GB"；快照超过 25 分钟就取消；"large amounts of data impact workspace snapshotting, workspace cloning"；遵守 `.gitignore` / `.anyscaleignore` |
| 本地盘 | `/mnt/local_storage` | 节点块存储 | 不共享 | 集群终止即删 | 用于临时文件、缓存、object spilling |
| 共享存储 | `/mnt/cluster_storage`（集群）、`/mnt/user_storage`（用户）、`/mnt/shared_storage`（整个 cloud） | "a virtual file system mapped back to persistent storage"，VM 栈上映射到**默认系统对象存储桶**；K8s 上用 PVC + CSI（例如 blobfuse2）；老版本用 NFS | 所有节点 | 平台不删除，也不隔离 | 用途表里 `/mnt/shared_storage` 写的是 "Model checkpoints, shared datasets"；同时又说 "Anyscale doesn't recommend storing sensitive data, credentials, models, or code in shared storage" |
| 云对象存储 | `$ANYSCALE_ARTIFACT_STORAGE`（`s3://…/artifact_storage/`） | S3/GCS/Azure | 直接用 URI 访问 | 云厂商负责 | "Large datasets (TB+)"；"Stream large datasets directly from object storage rather than copying to local storage." |

代码怎么放，原文："For code, build your own custom images and manage your code assets through integration with a private Git repository."

**Ray Train 检查点**（<https://docs.ray.io/en/latest/train/user-guides/persistent-storage.html>、<https://docs.ray.io/en/latest/train/user-guides/checkpoints.html>）：

- "Ray Train expects all workers to be able to write files to the same persistent storage location."；推荐 "cloud object storage such as S3, GCS, or Azure Blob Storage"，也可以用 EFS / Filestore / NFS。
- 多节点时 "using the local filesystem of the head node as the persistent storage location is no longer supported."
- 流程："first dump the checkpoint to a local temporary directory. Then, the call to `train.report` uploads the checkpoint to its final persistent storage location. Then, the local temporary directory can be safely cleaned up"。分布式检查点："Each worker uploads its own checkpoint shard"。
- 目录布局：`{storage_path}/{name}/checkpoint_epoch=N/`，**每个检查点一个新目录**。
- working dir 产物同步（`SyncConfig(sync_artifacts=True)`）在 Ray 2.43 已经废弃，"will not be supported in Ray Train V2"。

### 2.5 Kubeflow Trainer（Kubernetes PVC 模式）

| 类别 | 挂载路径 | 底层 | 跨节点 | 一致性 / 持久性 | 坑 |
|---|---|---|---|---|---|
| 代码 / 运行时 | 训练镜像 + `TrainingRuntime` | 镜像 | — | 不可变 | — |
| 数据集 | `/workspace/dataset`（`DatasetMountPath`） | `dataset-initializer` 作业把数据**下载**到一个 PVC | 显式复制到 PVC | PVC 语义 | 内置 torchtune 运行时的 PVC 是 `accessModes: ["ReadWriteOnce"]`、`storage: 20Gi` |
| 预训练模型 | `/workspace/model`（`ModelMountPath`） | `model-initializer` 下载到同一个 PVC | 同上 | 同上 | — |
| 输出 / 检查点 | 示例 `output_dir=/workspace/output`，`checkpointer.checkpoint_dir=/workspace/model` | 同一个 PVC | 同上 | 同上 | 把结果导出到对象存储，这份清单里没有（未确认） |

来源：<https://github.com/kubeflow/trainer/blob/master/pkg/constants/constants.go>（`DatasetMountPath string = "/workspace/dataset"`、`ModelMountPath string = "/workspace/model"`）、<https://github.com/kubeflow/trainer/blob/master/manifests/base/runtimes/torchtune/llama3_2/llama3_2_1B.yaml>（`volumeClaimPolicies`）。

### 2.6 RunPod

| 类别 | 挂载路径 | 底层 | 跨节点 | 一致性 / 持久性 | 坑 |
|---|---|---|---|---|---|
| 容器盘 | 系统管理 | 宿主本地盘 | — | "Lost on stop/restart" | 编辑运行中的 Pod 会清空 `/workspace` 之外的所有数据 |
| 卷盘 | `/workspace`（默认） | 宿主本地盘 | **绑定在这台宿主上**，Pod 删除即删 | stop/restart 保留 | 官方用途："ideal for storing models, datasets, and checkpoints"；只能扩容，不能缩 |
| 网络卷 | `/workspace`（替换卷盘） | "backed by high-performance NVMe SSDs"，200–400 MB/s；同一数据中心内的网络存储 | 可以挂到多个 Pod，"transfer it between machines"；**绑定数据中心** | "Writing to the same volume from multiple workers simultaneously may cause data corruption." | 只能在创建 Pod 时挂载，之后不能卸；"Data does not sync automatically between volumes"，跨数据中心要用 S3 兼容 API 或 `runpodctl` 手动复制 |
| 全局卷 | `/workspace-global`（与网络卷同时挂载时） | 弹性容量，不绑区域 | 任意 Pod | 文档没写 | "designed for data that many Pods read, such as model weights. You can also write to them, including large files such as checkpoints, but writes can be slower … Use a network volume instead when your workload … depends on full POSIX file system behavior." |

来源：<https://docs.runpod.io/pods/storage/types>、<https://docs.runpod.io/storage/network-volumes>。

**检查点**：直接写 `/workspace`（卷盘或网络卷）。全局卷明确说**不是完整的 POSIX**、写入较慢。

### 2.7 Determined AI

| 类别 | 挂载路径 | 底层 | 跨节点 | 一致性 / 持久性 | 坑 |
|---|---|---|---|---|---|
| 代码 | 实验的 context 目录 + 镜像 | — | 每个 trial 下发一份 | — | — |
| 数据 | 用户自己负责，例如 S3 `base_url` | — | — | — | 平台不托管 |
| 检查点 `s3` / `gcs` / `azure` | — | 对象存储 | `store_path()` 先在本地准备目录，"When the context manager exits, the model is automatically uploaded"；`upload()` "chooses a random `storage_id`, then uploads the contents of `ckpt_dir` … into a directory by the name of the `storage_id`" | **每个检查点一个新的 UUID 目录**，元数据里有 `steps_completed`；完成后按 `save_*_best` 策略 GC | 不分片时只有 chief worker 能上传；`shard=True` 时所有 worker 都要调用，作为同步点，冲突文件会被检测出来 |
| 检查点 `shared_fs` | 容器内 `/determined_shared_fs` | 管理员在每台 agent 上挂载同一个 GlusterFS / NFS | 共享 FS | "the content of this directory to be the same on all agent hosts" | `det checkpoint download` 假定本机也挂载在同一个 `host_path` |

来源：<https://github.com/determined-ai/determined/blob/main/docs/reference/experiment-config-reference.rst>（Checkpoint Storage 一节）、<https://github.com/determined-ai/determined/blob/main/harness/determined/core/_checkpoint.py>。`docs.determined.ai` 在本环境访问失败，改读仓库里的文档源文件。

### 2.8 缓存层模式：JuiceFS CSI、Fluid / Alluxio

- **JuiceFS CSI**：PVC → PV；CSI Node 在每个节点上为每个 PVC 起一个 Mount Pod 运行 JuiceFS 客户端，挂载到宿主的 `/var/lib/juicefs/volume/[pv-name]`，再 bind 进应用 Pod。"When multiple Pods reference a same PV, Mount Pod will be reused." 一致性是 close-to-open，内核元数据缓存默认 1s；"A successful write does not mean data is persisted: that's actually flush's job."（<https://juicefs.com/docs/csi/introduction>、<https://juicefs.com/docs/community/guide/cache>）
- **FUSE 进程死亡**（与 P0 观察到的现象一致）："For CSI Driver versions prior to v0.25.0, if a Mount Pod crashes … the mount point inside the application Pod will not recover since it relies on an external binding from CSI Node (mount --bind) … any access will result in a `Transport endpoint is not connected` error." v0.25.0 起："CSI Node will hold all open file descriptors, making existing FUSE requests hang until Mount Pod recovers"。仍然建议配置 `mountPropagation: HostToContainer`，并且 "Application must be able to handle bad file descriptors"（<https://juicefs.com/docs/csi/guide/configurations/>）。
- **Fluid**："Kubernetes-native Distributed Dataset Orchestrator and Accelerator"，把 Dataset 抽象出来，配合可伸缩的缓存 runtime（Alluxio、JuiceFS 等）、DataLoad 预热和 "data affinity-scheduling"（<https://github.com/fluid-cloudnative/fluid>）。Dataset 有 `AccessModes` 字段（`api/v1alpha1/dataset_types.go`）。默认是否为 `ReadOnlyMany` 未确认。这一类方案面向的是**数据集读加速**，文档里没有把它用于代码或 Git 的说法。

---

## 3. 跨厂商对照

| 厂商 | 代码 / 环境放在哪 | 代码层怎么跨节点 | 数据集放在哪 | 检查点 | 有没有写明小文件 / Git 坑 |
|---|---|---|---|---|---|
| Daytona | 沙箱本地 FS | 停止时归档到对象存储（容器类）或卸载到附近存储（VM 类） | FUSE + S3 Volume | Volume（GPU 沙箱只能放这里） | FUSE 卷 "generally slower"、不能当块设备 |
| E2B | 沙箱本地 FS | pause / resume 快照 | Volume（beta）或用户自挂 FUSE | 未确认 | **"Run builds, package installs, and Git operations on local disk"** |
| Modal | Image + 容器 FS，`git pull` | Filesystem Snapshot → Image | Volume、CloudBucketMount | **直接写 Volume** | v1 超过 5 万文件变慢；CBM 不支持 rename |
| Coder | RWO PVC / docker volume `/home/coder` | 块卷（由 StorageClass 决定） | 模板作者自加 | 未提供 | — |
| Runloop | Devbox 磁盘，创建时 git clone | suspend 时快照磁盘 | Object Mount（创建时复制 / 解包） | 未确认 | — |
| Codespaces | `/workspaces`（宿主 VM 盘） | 未确认 | 不提供 | 不提供 | — |
| SageMaker | 镜像 | 每个任务重新拉取 | File（下载）/ FastFile（懒加载）/ FSx / EFS | **本地 `/opt/ml/checkpoints` 持续同步 S3** | 小文件要打包；FastFile 适合顺序读 |
| Vertex | 镜像 / 包 | 每个任务重新拉取 | `/gcs` FUSE、`/mnt/nfs` | GCS 目的地，但**建议先写本地** | rename 不是原子操作；用少量大文件 |
| Lightning | `/teamspace/studios/this_studio` | 自动持久化（机制未确认） | Drive、数据连接、高性能卷 | Drive 或高性能卷 | S3/GCS 上大量小文件 "extremely slow" |
| Anyscale | `/home/ray/<project>`，周期快照 | **每 5 分钟快照 + 终止时快照**，小于 10 GB | 对象存储（TB 级）或 shared storage | `/mnt/shared_storage` 或对象存储 | 大文件会拖慢快照，超过 25 分钟就失败 |
| Ray Train | （由 runtime 决定） | `working_dir` 注入 | Ray Data（不在本页） | **本地临时目录 → `train.report` 上传**，每个检查点一个新目录 | 多节点不能用本地 FS |
| Kubeflow Trainer | 镜像 | — | initializer 下载到 PVC 的 `/workspace/dataset` | 同一个 PVC 的 `/workspace/output` | — |
| RunPod | 容器盘（易失）+ `/workspace` | 网络卷（同一数据中心）；跨数据中心手动复制 | `/workspace` 卷或网络卷 | `/workspace` | 多写者可能损坏数据；全局卷不是完整 POSIX |
| Determined | context 目录 + 镜像 | 每个 trial 下发 | 用户自理 | **本地 `store_path()` → 上传到 UUID 目录**，或 shared_fs | — |
| JuiceFS CSI / Fluid | 不用于代码 | — | PVC 背后接缓存层 | 可以（可写 PV） | Mount Pod 死亡导致挂载丢失（老版本） |

---

## 4. 对 orrery 的含义

### 4.1 第一层：`/workspace` 本地盘 + Generation 提交到 S3

**支持的证据（强）**

- 没有一家把 Git 放在 FUSE-over-对象存储上并写明推荐。E2B 明说要在本地盘上做 Git；Daytona、Modal CBM、Vertex `/gcs` 的限制（较慢、不支持 rename、rename 非原子）对 Git 都是致命的。这与 P0 的数字一致。
- 「本地目录 + 周期 / 停止时快照到对象存储」在 **Anyscale workspace** 有几乎一模一样的先例：每 5 分钟快照一次，终止时再快照，建议小于 10 GB，遵守 `.gitignore`，崩溃丢失上次快照之后的改动。Daytona 的容器归档、Runloop 的 suspend 快照、Modal 的 Filesystem Snapshot、E2B 的 pause 也都属于「本地盘 + 停止时打包」。
- Anyscale 给出了**两个现成的数值护栏**：大小上限的建议值（10 GB），以及快照超时（25 分钟）。超时之后它会报 "Terminating the workspace might lead to loss of edits"，这正是 orrery 设计里「最后一次提交成功之前，不能淘汰本地目录」需要的用户提示。orrery 的 Generation 是增量的（只重新哈希变化的文件），比 Anyscale 的快照有优势，但 Anyscale 的经验说明：**体积护栏和失败提示是必须有的。**
- Anyscale 用 `.gitignore` / `.anyscaleignore` 排除文件，这说明「Workspace 里的大数据不该进快照」是厂商实际在做的约束，与 orrery 把数据集放到 Resource 的分层一致。

**反对或需要注意**

- Codespaces、Coder、RunPod 的代码层用的是**块卷 / 宿主盘**，不做对象存储快照；跨节点靠块卷重新挂载（Coder 的 PVC），或者干脆不跨（RunPod 卷盘、Codespaces 未确认）。这条路需要 orrery 没有的块存储集群（ADR-0014），不构成反证。
- Lightning Studio home 的持久化机制未确认。如果它其实是网络盘，那就是唯一一个「代码放在网络 FS 上」的例子，但也不是 FUSE-over-S3。

### 4.2 第二层：数据集放只读 Resource（内容寻址目录树，取到 Node 缓存）

**支持**

- 训练平台的数据集**默认就是只读输入**：SageMaker 的 channel、Vertex `/gcs` 的输入、Kubeflow `/workspace/dataset`、Runloop Object Mount、Fluid Dataset。
- 「启动前整份复制到本地」是 SageMaker 的**默认** File 模式，也是 Kubeflow initializer 和 Runloop Object Mount 的唯一方式，与 Resource 先取到 Node 缓存再挂只读的做法同形。SageMaker 给出的适用范围是 "less than 50-100 GB"。
- 各家的小文件建议（SageMaker "fewer larger file containers (such as 150 MB per file)"、Vertex "larger and fewer files"、Lightning LitData）都指向同一点：数据集应该打包。orrery 按内容寻址的 Resource 可以在 manifest 层做 pack，与这个建议相容。

**缺口**

- 大于 100 GB、又不能全量下载的数据集，各家都用**懒加载**解决：SageMaker FastFile、Vertex `/gcs`、Modal Volume、Daytona Volume、FSx Lustre（底下也是 "lazy loading"）。orrery 的 Resource 现在是整份取回。只读懒加载 FUSE 的写路径开销不重要，P0 的否决也不适用于它（`workspace-shared-storage-s3.md` §4.6 末条已经留了这条路）。是否需要它，取决于数据集规模，未确认。

### 4.3 第三层：可选的 FUSE `/data`，用来放检查点等可变大文件

**支持「有一个共享可写挂载」**

- Modal（`output_dir` 设到 Volume 里）、Anyscale（`/mnt/shared_storage`，VM 栈上底下就是对象存储）、Daytona（GPU 沙箱的唯一出路）、Lightning（Drive）、RunPod（网络卷 / 全局卷）都让用户**直接把检查点写到共享挂载上**。所以「对象存储上的 FUSE 可写卷用来放大文件」有大量先例。
- 这些平台写明的坑，正好可以作为 orrery `/data` 的契约清单：
  - 并发写只保证 last write wins：Daytona、Modal；RunPod 是 "may cause data corruption"。
  - rename 不是原子操作或不支持：Vertex、Modal CBM。
  - 锁会挂住、会出现 `ESTALE`：E2B。
  - 写入较慢，不是完整 POSIX：RunPod 全局卷。
  - FUSE 进程死亡时挂载丢失：JuiceFS CSI v0.25 之前；v0.25 起持有 fd 做平滑恢复。这与 P0 的观察一致，也说明 fd 交接是业界做法。

**反对「检查点必须靠 FUSE」**

- 训练平台的**主流**做法是「先写本地，再上传」，不依赖 FUSE：SageMaker（本地持续同步 S3）、Ray（`train.report` 上传）、Determined（`store_path()` 退出时上传）。Vertex 更是明说 "Use local storage for checkpointing … instead of Cloud Storage"，**反对**通过 `/gcs` FUSE 直接写检查点。
- Ray 和 Determined 的检查点是**每次一个新的不可变目录**（`checkpoint_epoch=N/`、UUID），写完以后不再修改。这正好对应 orrery 已有的 Resource / Artifact（内容寻址、不可变）：Sandbox 写到本地，比如 `/workspace` 之外的本地暂存目录，再由 Agent 把它发布成一个 Artifact 或 Resource 版本，跨节点恢复时当作只读 Resource 取回来。这条路**不需要 `/data` FUSE**，也不引入新的一致性模型。
- SageMaker 是「镜像同步」（本地删除，S3 也跟着删），这是可变目录的同步语义。如果 orrery 想支持「训练脚本只认一个固定目录」，可以在 Agent 侧做一个**单写者的本地目录 → S3 增量同步**。这与 Generation 提交是同一套机制（stat 扫描 + 只哈希变化的文件），同样不需要 FUSE。

**结论**：`/data` FUSE 有先例，但没有一家训练平台把它当作检查点的**唯一**方案。证据更支持先做「本地写 + 以不可变检查点目录发布」（Ray / Determined 形状），或者「本地目录增量同步」（SageMaker 形状）。等出现「多个 Sandbox 同时读写同一份大可变文件」的真实需求时，再评估 `/data`。到那时直接沿用 Modal / Daytona 的 last-write-wins 契约，以及 JuiceFS 的 fd 交接升级做法。

---

## 5. 未确认 / 未覆盖

- E2B Volume 的底层实现和一致性模型（文档没写）；E2B、Runloop 的检查点做法（没有文档）。
- Lightning Studio home（`/teamspace/studios/this_studio`）用什么实现持久化：快照、网络盘还是对象存储；数据连接在 Studio 内的挂载路径。
- Codespaces 换宿主 / 换机器类型时 `/workspaces` 怎么迁移。
- Vertex `AIP_CHECKPOINT_DIR` 由谁把本地检查点复制到 GCS（框架还是用户）。
- Fluid Dataset 的 `AccessModes` 默认值；Kubeflow Trainer 运行时之外，结果怎么导出到对象存储。
- SageMaker 现行文档与 2023-06 归档版之间的差异（本环境无法访问 `docs.aws.amazon.com`）。
- Modal Volume 的内部实现（只写了 "caching and chunking optimizations"）。

## 来源

- Daytona：<https://www.daytona.io/docs/en/volumes/>；`daytona-sandbox-runtime-semantics.md` §1.5–1.6
- E2B：<https://docs.e2b.dev/volumes.md>、<https://docs.e2b.dev/faq/volumes-beta-limitations.md>、<https://docs.e2b.dev/sandbox/connect-bucket.md>、<https://docs.e2b.dev/sandbox/persistence.md>
- Modal：<https://modal.com/docs/guide/volumes>、<https://modal.com/docs/guide/cloud-bucket-mounts>、<https://modal.com/docs/guide/sandbox>、<https://modal.com/docs/guide/model-weights>
- Coder：<https://github.com/coder/coder/blob/main/examples/templates/kubernetes/main.tf>、<https://github.com/coder/coder/blob/main/examples/templates/docker/main.tf>
- Runloop：<https://docs.runloop.ai/docs/devboxes/mounts/code-mounts.md>、<https://docs.runloop.ai/docs/devboxes/mounts/object-mounts.md>、<https://docs.runloop.ai/docs/devboxes/lifecycle.md>
- GitHub Codespaces：<https://docs.github.com/en/codespaces/about-codespaces/deep-dive>
- SageMaker（归档）：<https://github.com/awsdocs/amazon-sagemaker-developer-guide/tree/master/doc_source>（`model-access-training-data.md`、`model-checkpoints.md`、`your-algorithms-training-algo-running-container.md`、`your-algorithms-training-algo-output.md`）
- Vertex：<https://docs.cloud.google.com/vertex-ai/docs/training/cloud-storage-file-system>、<https://docs.cloud.google.com/vertex-ai/docs/training/code-requirements>、<https://docs.cloud.google.com/vertex-ai/docs/training/train-nfs-share>
- Lightning：<https://lightning.ai/docs/platform/build/ai-studio.md>、<https://lightning.ai/docs/platform/data/drive.md>、<https://lightning.ai/docs/platform/data/cloud-folders.md>、<https://lightning.ai/docs/platform/data/data-connections/s3-buckets.md>
- Anyscale：<https://docs.anyscale.com/configuration/storage>、<https://docs.anyscale.com/storage/shared>、<https://docs.anyscale.com/workspaces/files>
- Ray：<https://docs.ray.io/en/latest/train/user-guides/persistent-storage.html>、<https://docs.ray.io/en/latest/train/user-guides/checkpoints.html>
- Kubeflow Trainer：<https://github.com/kubeflow/trainer/blob/master/pkg/constants/constants.go>、<https://github.com/kubeflow/trainer/blob/master/manifests/base/runtimes/torchtune/llama3_2/llama3_2_1B.yaml>
- RunPod：<https://docs.runpod.io/pods/storage/types>、<https://docs.runpod.io/storage/network-volumes>
- Determined：<https://github.com/determined-ai/determined/blob/main/docs/reference/experiment-config-reference.rst>、<https://github.com/determined-ai/determined/blob/main/harness/determined/core/_checkpoint.py>
- JuiceFS CSI：<https://juicefs.com/docs/csi/introduction>、<https://juicefs.com/docs/csi/guide/configurations/>、<https://juicefs.com/docs/community/guide/cache>
- Fluid：<https://github.com/fluid-cloudnative/fluid>、<https://github.com/fluid-cloudnative/fluid/blob/master/api/v1alpha1/dataset_types.go>
