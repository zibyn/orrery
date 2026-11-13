# 用户本机访问平台数据：沙箱 / ML 平台 / 数据版本工具的做法

调研日期 **2026-11-13**。问题：用户想在**自己的笔记本或工作站**上访问存在平台里的数据（Volume、数据集、模型检查点、Artifact）时，各家给的是显式下载 / 上传命令、同步，还是本地挂载目录？有用户认为 orrery 至少应该给一个本地挂载目录，本文为这个判断找正反证据，并记录确实提供挂载的厂商是怎么实现的。

只用一手来源：官方文档、CLI 参考、源码、厂商工程博客。厂商原文保留英文，每条结论后附来源。

范围说明：

- 数据在**沙箱 / 训练作业内部**怎么存放，见 `code-vs-training-data-vendor-practice.md`，本文不重复取证。
- macOS / Linux 的挂载机制本身（macFUSE、FUSE-T、NFS loopback、FSKit）由另一份调研负责。本文只记录厂商**文档写明**用了哪种机制。
- orrery 侧的背景是 ADR-0030：不可变的 Resource 在 Node 上经 `orrery-resourcefs` 懒加载挂载，大文件按 4 MiB 分块存储。

**来源说明**：`docs.aws.amazon.com` 在本环境不可达（见 `code-vs-training-data-vendor-practice.md` 的来源说明），所以 Mountpoint for Amazon S3 用的是它的 GitHub 仓库 `awslabs/mountpoint-s3`。Lightning AI 的文档站返回 403，所以 Lightning 一节读的是 PyPI 上 `lightning-sdk` 2026.11.10.post0 的源码。

---

## 0. 先读这几条

1. **本地挂载不是常态，显式的下载 / 上传命令才是。** 在本文查的 8 家沙箱 / 开发环境平台和 8 家 ML 平台里，**只有 Hugging Face** 提供了官方的本地挂载（`hf-mount`）。其余各家给的是 `get/put/cp/download/upload`、SDK 的 read/write，或者 rsync / Mutagen 式的同步。Modal、Daytona、E2B、Runloop、RunPod、Lightning、W&B、Kaggle 的文档和 CLI 参考里都**没有**本地挂载命令。
2. **提供本地挂载的，主要是「存储层」产品，而不是「计算平台」。** lakeFS（Everest，收费版）、Pachyderm、JuiceFS、rclone、Mountpoint for S3、gcsfuse、BlobFuse2 都能挂载。但其中 Mountpoint、gcsfuse、BlobFuse2 三家云厂商官方的挂载工具**只支持 Linux**，在 Mac 上用不了。
3. **会挂载的厂商，每一家都同时保留显式下载。** Hugging Face 有 `hf download` 和本地缓存，lakeFS 有 `lakectl local`，DVC 有 `dvc pull`（自己不挂载，只提供 fsspec 流式读），Pachyderm 有 `pachctl get file`（未确认），JuiceFS 有 Gateway、WebDAV 和 Python SDK。挂载是**附加**的访问方式，不替代下载。
4. **在 macOS 上真正可用的挂载，分两种实现：**
   - **本地 NFS 服务器**：`hf-mount` 默认用 NFS（"recommended -- works everywhere, no root, no kernel extension"），lakeFS Everest 在 macOS 上用 "NFS V3"，rclone 推荐 `rclone nfsmount`。
   - **macFUSE（kext）**：`hf-mount --fuse`、JuiceFS、rclone mount、Pachyderm（文档还写着已停更的 osxfuse，并警告 Catalina 之后 "no longer stable"）。
   rclone 文档里另外列了 FUSE-T。较新的产品都把 NFS 放在默认或推荐的位置。
5. **只读、不可变的数据最适合本地挂载。** `hf-mount` 挂载 repo 时 "always mounted read-only"，lakeFS Everest 默认只读并 "pins the commit"。可写挂载都附带警告：hf-mount 是 "last writer wins"，Pachyderm 要求 "**ONLY** when you have sole ownership"，Mountpoint 不能修改已有文件。orrery 的 Resource 不可变、按版本寻址，正好落在「只读挂载」这个最容易做对的区间。

---

## 1. 沙箱 / 开发环境平台

### 1.1 Modal

- **命令**：`modal volume get`（"Download files from a modal.Volume object."，目录会递归下载）、`put`（"Upload a file or directory to a modal.Volume."）、`ls`、`cp`（"Copy within a modal.Volume."）、`rm`。完整列表里**没有 mount 子命令**。<https://modal.com/docs/reference/cli/volume>
- **SDK**："Volumes can also be used outside Modal via the Python SDK or our CLI."，上传用 `vol.batch_upload()` 的 `put_file` / `put_directory`。网页端 "only supports downloading files up to 16 MB. For larger files, please use the CLI"。<https://modal.com/docs/guide/volumes>
- **本地挂载**：无。文档里「挂载」只指挂进容器（包括 `modal shell` 挂在 `/mnt`）。<https://modal.com/docs/guide/volumes>

### 1.2 Daytona

- **本地访问**：Volume 只能挂进沙箱（`daytona create --volume my-awesome-volume:/home/daytona/volume`），读写都通过沙箱文件系统完成："Read from and write to a volume just like any other directory in the sandbox file system." 文档没有不经过沙箱、从本机直接读写 Volume 的 API。<https://www.daytona.io/docs/en/volumes/>
- 「Mount External Storage」页讲的是在**沙箱内**用 FUSE 挂外部存储（"External storage is mounted using FUSE."，工具有 `mount-s3`、`gcsfuse`、`blobfuse2`），不是挂到用户本机。<https://www.daytona.io/docs/en/mount-external-storage/>
- **本地挂载**：无。

### 1.3 E2B

- **SDK**：Volume 有独立于沙箱的读写 API，原文 "The SDK methods are meant to be used when the volume is not mounted to any sandbox."。<https://docs.e2b.dev/volumes.md>
  - 上传："You can upload data to a volume using the `writeFile()` / `write_file()` method."，官方示例是遍历本地目录、逐个文件写入。<https://docs.e2b.dev/volumes/upload.md>
  - 下载："You can download data from a volume using the `readFile()` / `read_file()` method."。<https://docs.e2b.dev/volumes/download.md>
  - 另有 `makeDir`、`list` 等。<https://docs.e2b.dev/volumes/read-write.md>
- **CLI**：文档索引里没有 Volume 的 CLI 命令（未确认）。<https://docs.e2b.dev/llms.txt>
- **本地挂载**：无。

### 1.4 Runloop

- **CLI**：`rli object upload <path>`、`rli object download <id> <path>`（带 `--extract`，可在下载后解开归档）。来源是 `@runloop/rl-cli` 的 README 和命令列表，我没有逐条核对 flag（未确认）。<https://docs.runloop.ai/docs/tools/rl-cli>
- **SDK**：上传用 "`upload_from_file()` / `uploadFromFile()`"；下载用 "Get a time-limited signed download URL via `get_download_url(seconds_to_expire)`"。<https://docs.runloop.ai/llms.txt>（Storage Objects 条目）
- **Devbox 文件**："file transfer via `rli devbox scp` / rsync"。<https://docs.runloop.ai/llms.txt>（SSH Access 条目）
- **本地挂载**：无。`object_mount` 是在创建时挂进 devbox 的。

### 1.5 Coder

- **Coder Desktop 文件同步**："provides bidirectional synchronization between a local directory and your workspace"。首次 "clones your workspace directory to your local directory, then watches for changes"。<https://coder.com/docs/user-guides/desktop/desktop-connect-sync>
  - 冲突：状态栏会显示 "Conflicts"，处理方法是 "delete the conflicting file that contains changes you don't want to keep"。
  - 限制："excludes version control system directories like `.git/`"。
- **实现**：底层是 **Mutagen**。文档没写，但 macOS 客户端源码的 `Makefile` 打包了 `mutagen-darwin-arm64`、`mutagen-darwin-amd64` 和 `mutagen-agents.tar.gz`。<https://github.com/coder/coder-desktop-macos/blob/main/Makefile>
- **OS**：Coder Desktop 只有 macOS（`brew install --cask coder/coder/coder-desktop`）和 Windows（`winget install Coder.CoderDesktop`）版本。<https://coder.com/docs/user-guides/desktop>
- **本地挂载**：无。这是双向同步，不是挂载。

### 1.6 GitHub Codespaces

- **命令**：`gh codespace cp`："copies files between the local and remote file systems"。`--expand` 让远端路径按 "in the manner of `scp`" 展开，默认会 "creates an SSH key pair in `~/.ssh`"。<https://cli.github.com/manual/gh_codespace_cp>
- **本地挂载**：官方文档里没有（未确认是否有官方的 sshfs 指南）。社区的做法是自行转发 SSH 后用 sshfs，这不是一手来源，本文不采用。

### 1.7 Gitpod / Ona

- **命令**：Ona CLI 的 SSH 配置写好之后，"For file transfers, use the `-O` flag: `scp -O <environment-id>.ona.environment:/workspaces/project/file.txt ./local-file.txt`"。<https://ona.com/docs/ona/integrations/cli.md>
- **本地挂载**：无。

### 1.8 DevPod

- **方向是从本地到远端**：`devpod up ./path/to/my-folder` 时，"DevPod will sync the folder into the remote machine"。之后要 "re-uploading your local folder" 就得 Recreate 或 Reset。<https://github.com/loft-sh/devpod/blob/main/docs/pages/developing-in-workspaces/create-a-workspace.mdx>
- 文档没写把远端数据拉回本地的专门命令，用户只能走 SSH（`WORKSPACE_NAME.devpod`）。同步的实现在文档里也没写（未确认）。
- **本地挂载**：无。

---

## 2. ML 平台

### 2.1 RunPod

- **S3 兼容 API**："Runpod provides an S3-protocol compatible API for direct access to your network volumes." <https://docs.runpod.io/storage/s3-api>
  - 只覆盖 15 个数据中心。
  - 不支持预签名 URL（"Pre-signed URLs are not supported."）。
  - 单个 PutObject 小于 500MB。
  - 超过 10,000 个文件时 list / sync "can be slow or error-prone"，"Full sync support is in development"。
  - 官方示例只用 AWS CLI 和 Boto3，没有挂载示例。
- **runpodctl**：`runpodctl send` / `receive` 用一次性码传文件，"pre-installed on all Pods"。另外列了 SCP、rsync 和 cloud sync。<https://docs.runpod.io/runpodctl/transfer-files>
- **本地挂载**：无官方方案。理论上可以用第三方 S3 FUSE 工具挂这个 S3 API，但 RunPod 没有文档。

### 2.2 Lightning AI

- **CLI**：`lightning cp`。源码里的路由逻辑要求 "At least one path must be a lit://"、"One path must be local"，也就是本地 ↔ `lit://`（Drive / Studio）之间的复制。另有 `lightning ls`、`rm`，以及 `lightning dataset download/upload`、`lightning model`、`lightning container download/upload`。<https://pypi.org/project/lightning-sdk/>（`lightning_sdk/cli/cp/__init__.py`、`lightning_sdk/cli/dataset/`，版本 2026.11.10.post0）
- **本地挂载**：CLI 没有 mount 子命令。我在源码里搜了 `mount`，只在日志 TUI 里出现。
- 文档站本环境访问被拒（403），文档里有没有其他本地挂载方式「未确认」。

### 2.3 Anyscale

- **命令**：`anyscale workspace_v2 pull`："Pull the working directory of a workspace. New files will be created, existing files will be overwritten." `push` 方向相反。<https://docs.anyscale.com/reference/cli/workspaces>
  - 两个命令都写明 "This command depends on rsync, please make sure it is installed on your system."
  - `--delete` 会删除对端多出来的文件。
  - "Excluded files (like .git unless --pull-git-state is used) are preserved and not deleted."
- 按旧版 IDE 指南，`anyscale workspace run` "does an implicit push unless you pass `--no-push`"。<https://docs.anyscale.com/1.0.0/workspaces/ides>
- **本地挂载**：无。shared storage（`/mnt/shared_storage`）只在集群内挂载，见 `code-vs-training-data-vendor-practice.md`。

### 2.4 SageMaker

- 训练数据、检查点和模型产物都在 S3（见 `code-vs-training-data-vendor-practice.md` 的 SageMaker 一节）。从本机访问就是普通的 S3 客户端（AWS CLI / SDK），SageMaker 自己没有本地挂载工具（未确认，因为 `docs.aws.amazon.com` 不可达）。
- AWS 官方的 S3 挂载工具 Mountpoint "is only available for Linux operating systems"，见 §3.7。所以在 Mac 上，AWS 一方没有可用的挂载。

### 2.5 Vertex AI

- 数据和产物在 Cloud Storage（作业内的 `/gcs`，见 `code-vs-training-data-vendor-practice.md`）。本机访问用 `gcloud storage` 一类的客户端，或者 gcsfuse。
- gcsfuse 的安装要求是 "a compatible operating system such as Linux or Windows Subsystem for Linux"，不支持 macOS，见 §3.8。Vertex 自己没有本地挂载工具（未确认）。

### 2.6 Weights & Biases Artifacts

- **命令**：SDK `Artifact.download()`；CLI "Use the `wandb artifact get` command to download an artifact from the W&B server."，例如 `wandb artifact get project/artifact:alias --root mnist/`。<https://docs.coreweave.com/guides/artifacts/download-and-use-an-artifact>（`docs.wandb.ai` 已 301 跳到这里）
- **语义（源码）**：<https://github.com/wandb/wandb/blob/main/wandb/sdk/artifacts/artifact.py>、<https://github.com/wandb/wandb/blob/main/wandb/env.py>
  - `download()` 的说明是 "Existing files located within `root` are not modified. Explicitly delete `root` before you call `download` if you want the contents of `root` to exactly match the artifact."
  - 支持 `path_prefix` 部分下载。单个文件超过 2GB 时用 multipart 并行下载。
  - 默认下载目录是 `./artifacts`（`WANDB_ARTIFACT_DIR`）。
  - 缓存目录是 `WANDB_CACHE_DIR`，默认取 `platformdirs.user_cache_dir("wandb")`。`skip_cache=True` 跳过缓存。
- **本地挂载**：无。

### 2.7 Hugging Face Hub

**显式下载 + 本地缓存。** <https://huggingface.co/docs/huggingface_hub/guides/manage-cache>

- 用 `hf download` / `snapshot_download()` 下载，默认缓存在 `~/.cache/huggingface/hub`。
- 缓存结构：`blobs/` 按哈希存放文件内容，`snapshots/<commit>/` 里是指向 blob 的符号链接，"if the same file was fetched in revision `bbbbbb`, it would have the same hash and the file would not need to be re-downloaded"。
- `trees/<commit>.json` 缓存每个 commit 的文件列表，理由是 "A commit is immutable, so its file list never changes"。
- Windows 不支持符号链接时退化成复制，需要开启 Developer Mode。

**本地挂载：`hf-mount`**，官方开源，Rust 实现。<https://github.com/huggingface/hf-mount>

- 定位："Mount Hugging Face Buckets and repos as local filesystems. No download, no copy, no waiting."
- **懒加载**："Files are fetched lazily on first read, so only the bytes your code actually touches ever hit the network."
- **两种后端**："**NFS** (recommended) -- works everywhere, no root, no kernel extension"；"**FUSE** -- tighter kernel integration, requires root or macFUSE on macOS"。
- **macOS 安装**：`brew install hf-mount` "installs the NFS backend only"，因为 "macFUSE is closed-source and not distributable through homebrew-core"。装 macFUSE "requires reboot on first install"。README 另给了 launchd 开机自动挂载的示例。
- **读写**：repo "always mounted read-only"（可指定 `--revision`，也可只挂子目录）；Bucket 默认可读写，可加 `--read-only`。
  - 默认写模式是 "append-only, in-memory, synchronous upload on close"。
  - `--advanced-writes` 才支持随机写。
- **缓存**：
  - 磁盘块缓存在 `--cache-dir`（默认 `/tmp/hf-mount-cache`），大小上限 `--cache-size` 默认约 10 GB。
  - 元数据 TTL 默认 10 s，远端变更靠轮询（默认 30 s）或 Bucket 的 SSE 事件流感知。
- **不适合的场景**："no multi-writer support, no cross-node file locking"；"Latency-sensitive random I/O (first reads require network round-trips)"；"files can be stale for up to 10 s"；"last writer wins, no conflict detection"。
- **Hub 文档里的定位**：hf-mount 和 CLI sync、S3 API、fsspec `hf://`、DuckDB 并列，是访问方式之一，原文 "Mount as local filesystem — any tool works"；同步用 "CLI sync | Batch transfers, backups"。在 HF Jobs / Spaces 里，同样的东西由平台托管成 Volume。<https://huggingface.co/docs/hub/storage-buckets-access>

### 2.8 Kaggle

- **CLI**：`kaggle datasets download <DATASET>`，`-f` 下载单个文件，`--unzip` 解压，`-p` 指定目录。大文件 "are served inside a zip archive"。上传用 `kaggle datasets create -p <FOLDER_PATH>` / `version`。<https://github.com/Kaggle/kaggle-api/blob/main/docs/datasets.md>
- **kagglehub**：在 Kaggle notebook 之外，"The resource files are downloaded to a local cache folder"，默认 `~/.cache/kagglehub/`，可用 `KAGGLEHUB_CACHE` 改。在 notebook 里则 "served from the shared Kaggle resources cache"。<https://github.com/Kaggle/kagglehub/blob/main/README.md>
- **本地挂载**：无。

---

## 3. 数据版本 / 存储工具

### 3.1 DVC

- `dvc pull`："It has the same effect as running dvc fetch and dvc checkout."，可以按 target 只拉一部分。<https://doc.dvc.org/command-reference/pull>
- 文件从缓存放进工作区的方式："By default, DVC tries to use reflinks for the cache"，不行就退回 copy；也可以配置成 hardlink 或 symlink。<https://doc.dvc.org/user-guide/data-management/large-dataset-optimization>
- **流式读**：`DVCFileSystem` "is a read-only filesystem"，"can otherwise stream from supported remote storage"。它是 fsspec 接口，不是 OS 层面的挂载。<https://doc.dvc.org/api-reference/dvcfilesystem>
- **本地挂载**：无。

### 3.2 lakeFS

- **`lakectl local`**（显式同步）：`clone` / `pull` / `commit` / `status`，工作方式是 "copying the data onto the host machine"，建议数据 "typically no larger than 15 GB"。<https://docs.lakefs.io/guides/local-checkouts/>
- **lakeFS Mount（Everest）**：<https://docs.lakefs.io/mount/>
  - 定位："lets you virtually mount a remote lakeFS repository onto a local directory"。
  - **收费**："Available in lakeFS Team and lakeFS Enterprise"，二进制要找厂商要。
  - **机制**：macOS 写的是 "(with NFS V3)"，`--protocol` 默认是 `nfs`，`fuse` 也可选。
  - **懒加载**："Files are only downloaded when their content is accessed."
  - **只读为默认**，并 pin 住挂载时分支 HEAD 的 commit。`--write-mode` 允许在本地修改，再用 `everest commit` 提交回分支。
  - **缓存**按 commit ID 做键，默认在卸载时清掉；`--cache-dir` 可以跨会话复用。
  - **写模式限制**：不支持 rename、临时文件、硬链接 / 软链接、POSIX 锁；"Writes to a mount directory during a commit may be lost"。Windows（CFAPI）只读。
- 两种方式并存，文档只说它们 "different performance characteristics"，没有推荐其中一种。<https://docs.lakefs.io/guides/local-checkouts/>

### 3.3 Pachyderm

- `pachctl mount`："Pachyderm uses FUSE to mount repositories as local filesystems."，默认只读，加 `--write` 或 `+w` 后可写。<https://go-mod-viewer.appspot.com/github.com/pachyderm/pachyderm@v1.13.4/doc/docs/1.11.x/how-tos/export-data-out-pachyderm/mount-repo-to-local-computer.md>
- **写回时机**："Upon exiting the `pachctl mount` command, Pachyderm uploads all the changes"。
- **并发写的警告**："their changes will likely be overwritten when you exit"；"Use writable mount **ONLY** when you have sole ownership over the mounted data."
- **macOS**：文档让用户安装 `osxfuse`，同时警告 "Because Apple has announced phasing out support for macOS kernel extensions, including FUSE, this functionality is no longer stable on macOS Catalina (10.15) or later." 2.6.x 版文档仍保留这段警告。<https://docs.pachyderm.com/2.6.x/export-data/mount-repo-to-local-computer/>（最新版页面本环境连接被断开，未确认）

### 3.4 Quilt

- `quilt3.Package.install(...)` 把包和数据全部下载到本地 registry，或用 `dest=` 指定目录。
- `browse` "downloads a package manifest without also downloading the data in the package"，之后可以按需取单个文件。
- 来源：<https://github.com/quiltdata/quilt/blob/master/docs/walkthrough/installing-a-package.md>
- **本地挂载**：文档没有（未确认）。

### 3.5 JuiceFS

- 客户端 "supports various operating systems ... including ... Linux, macOS, and Windows"。
- macOS："you need to install macFUSE to enable FUSE mounting. If FUSE mounting is not your primary use case, installing macFUSE is not required. You can also conveniently read and write data using JuiceFS through WebDAV, Gateway, or the Python SDK."
- Windows 需要 WinFsp。
- 来源：<https://github.com/juicedata/juicefs/blob/main/docs/en/getting-started/installation.md>
- 可读写，POSIX 语义（缓存见 `workspace-shared-storage-s3.md` §2）。

### 3.6 rclone mount

- **macOS 有三条路**：<https://rclone.org/commands/rclone_mount/>
  - macFUSE："a traditional FUSE driver utilizing a macOS kernel extension (kext)"。
  - FUSE-T："mounts" via an NFSv4 local server"。
  - `rclone nfsmount`："starts the NFS server and performs the mount for you"，文档在支持的 macOS 上推荐用它。
- Windows 需要 WinFsp。
- **按块读取**："When rclone reads files from a remote it reads them in chunks"，默认块大小 128M（`--vfs-read-chunk-size`）。
- **缓存模式** `--vfs-cache-mode off|minimal|writes|full`：
  - `full` 模式下 "the files in the cache will be sparse files and rclone will keep track of which bits of the files it has downloaded"。
  - 默认 `off`，直接读写远端。

### 3.7 Mountpoint for Amazon S3

- "Mountpoint for Amazon S3 is only available for Linux operating systems." <https://github.com/awslabs/mountpoint-s3/blob/main/doc/INSTALL.md>
- 写入只适合 "write new objects sequentially from a single client at a time"，不适合 "make edits to existing files"，也不支持目录 rename 和 symlink。<https://github.com/awslabs/mountpoint-s3>

### 3.8 gcsfuse

- 安装在 "a compatible operating system such as Linux or Windows Subsystem for Linux"，不支持 macOS。
- 语义限制："Cloud Storage FUSE is not POSIX compliant"；多个挂载同时写同一个对象时 "the first mount to complete the write operation and successfully flush its changes to the object is saved"。
- 缓存分 file、stat、list 三种。
- 来源：<https://docs.cloud.google.com/storage/docs/cloud-storage-fuse/overview>

### 3.9 Azure BlobFuse2

- "uses the libfuse open source library (fuse3) to communicate with the Linux FUSE kernel module"，"BlobFuse lets Linux workloads use Azure Blob Storage like a local file system"。只支持 Linux。<https://github.com/Azure/azure-storage-fuse/blob/main/README.md>

---

## 4. 对比表

「下载」指显式的 get / download / cp 命令或 SDK 调用；「同步」指 rsync / Mutagen 式的目录同步；「挂载」指在用户本机出现一个 OS 级目录。

| 厂商 | 下载 / 上传 | 同步 | 本地挂载 | 挂载读写 | 懒加载 | 挂载支持的 OS（含 macOS） | 挂载的依赖 |
|---|---|---|---|---|---|---|---|
| Modal | `modal volume get/put/cp`、SDK `batch_upload` | — | 无 | — | — | — | — |
| Daytona | 只能经沙箱的文件 API | — | 无 | — | — | — | — |
| E2B | SDK `Volume.readFile/writeFile` | — | 无 | — | — | — | — |
| Runloop | `rli object upload/download`、签名 URL、`rli devbox scp` | rsync over SSH | 无 | — | — | — | — |
| Coder | `coder` SSH / scp | Coder Desktop 双向同步（Mutagen） | 无 | — | — | 同步：macOS、Windows | Coder Desktop |
| Codespaces | `gh codespace cp`（scp） | — | 无（官方） | — | — | — | — |
| Gitpod / Ona | `scp -O` | — | 无 | — | — | — | — |
| DevPod | 本地目录上传到远端 | `devpod up ./dir` 时同步过去 | 无 | — | — | — | — |
| RunPod | S3 兼容 API（AWS CLI / boto3）、`runpodctl send/receive`、scp | rsync、cloud sync | 无（官方） | — | — | — | — |
| Lightning AI | `lightning cp lit://…`、`dataset/model download/upload` | — | 无（CLI） | — | — | — | — |
| Anyscale | — | `workspace_v2 pull/push`（rsync） | 无 | — | — | — | — |
| SageMaker | S3 客户端 | `aws s3 sync` | 无（AWS 一方 Mountpoint 只支持 Linux） | — | — | — | — |
| Vertex AI | GCS 客户端 | — | 无（gcsfuse 只支持 Linux / WSL） | — | — | — | — |
| W&B Artifacts | `wandb artifact get`、`Artifact.download()` + 缓存 | — | 无 | — | — | — | — |
| **Hugging Face** | `hf download` + 内容寻址缓存、`hf upload` | Bucket 的 CLI sync（命令名未确认） | **hf-mount** | repo 只读；Bucket 读写 | 是 | Linux、**macOS**（默认 NFS；FUSE 需要 macFUSE） | NFS：无依赖、不需要 root；FUSE：fuse3 / macFUSE |
| Kaggle | `kaggle datasets download/create/version`、kagglehub 缓存 | — | 无 | — | — | — | — |
| DVC | `dvc pull/push` | — | 无（只有 `DVCFileSystem` fsspec 流式读） | — | 流式读 | — | — |
| **lakeFS** | `lakectl fs`（未确认） | `lakectl local clone/pull/commit` | **Everest**（收费） | 默认只读；`--write-mode` | 是 | Linux、**macOS（NFS v3）**、Windows（CFAPI，只读） | 收费版二进制 |
| **Pachyderm** | `pachctl get/put file`（未确认） | — | **`pachctl mount`** | 默认只读；`--write` 在退出时上传 | 未确认 | Linux、macOS（osxfuse，Catalina 后 "no longer stable"） | FUSE / osxfuse |
| Quilt | `Package.install`；`browse` 只下 manifest | — | 无（未确认） | — | 按文件 | — | — |
| **JuiceFS** | Gateway、WebDAV、Python SDK | — | **`juicefs mount`** | 读写 | 是 | Linux、**macOS（macFUSE）**、Windows（WinFsp） | macFUSE / WinFsp |
| **rclone** | `rclone copy` | `rclone sync` | **`rclone mount` / `nfsmount`** | 读写 | 是（按块读） | Linux、**macOS（macFUSE / FUSE-T / NFS）**、Windows（WinFsp） | 按机制而定 |
| Mountpoint for S3 | — | — | 有 | 只能顺序写新文件 | 是 | **只有 Linux** | FUSE |
| gcsfuse | — | — | 有 | 读写（非 POSIX） | 是 | **只有 Linux / WSL** | FUSE |
| BlobFuse2 | — | — | 有 | 读写 | 是 | **只有 Linux** | fuse3 |

---

## 5. 对 orrery 的含义

**本地挂载是可选项，不是常态。在计算平台里很少见。**

- 沙箱 / 开发环境平台（Modal、Daytona、E2B、Runloop、Coder、Codespaces、Ona、DevPod）**没有一家**提供把平台数据挂到用户本机的功能。它们给的是 get/put、SDK 读写、scp，以及（Coder、Anyscale）rsync / Mutagen 同步。
- ML 平台里只有 Hugging Face 有，而且是一个独立的开源工具 `hf-mount`。对「至少要有一个本地挂载目录」这个判断，行业的证据**不支持**把它当成基本功能。但证据支持把它当成可选的增值功能，尤其是面向只读、不可变的数据集和模型。

**挂载总是和显式下载搭配，而且下载先于挂载存在。**

- Hugging Face：`hf download` 加内容寻址缓存是主路径，hf-mount 是附加的。
- lakeFS：`lakectl local` 和 Everest 并存。
- DVC：只有 `dvc pull` 和 fsspec 流式读。
- JuiceFS：Mac 上不装 macFUSE 时，可以改用 WebDAV、Gateway 或 SDK。

对 orrery 来说，CLI 先要有 `resource download`（按版本、可只下部分路径、带本地内容寻址缓存，可以对照 HF 的 `blobs/` + `snapshots/` 设计），挂载放在后面。

**挂载在 macOS 上怎么实现。**

- 较新的实现（hf-mount、lakeFS Everest、rclone `nfsmount`）**默认或推荐本地 NFS 服务器**，理由是不需要 kext、不需要 root。hf-mount 的原话是 "works everywhere, no root, no kernel extension"，并且因为 macFUSE 闭源，Homebrew 只能分发 NFS 后端。
- 较老的或 FUSE 优先的实现（JuiceFS、rclone mount、Pachyderm、hf-mount `--fuse`）依赖 macFUSE。装 macFUSE 首次需要重启；Pachyderm 还写明 Catalina 之后不稳定。
- 三大云厂商官方的 FUSE 工具（Mountpoint、gcsfuse、BlobFuse2）都不支持 macOS。

**orrery 的数据形态正适合只读挂载。**

- Resource 不可变、按版本（地址）寻址，对应 hf-mount 的 repo 只读挂载（`--revision` pin 住版本）和 lakeFS Everest 的默认只读（pin commit）。这两家都把只读当作最稳的模式：HF 的 `trees/<commit>.json` 原文写着 "A commit is immutable, so its file list never changes"，ADR-0030 的属性和页缓存也永不过期。
- 可写挂载各家都附带强烈警告（last writer wins、退出时才上传、独占使用、不支持 rename 和锁），所以 orrery 如果做本地挂载，应该只挂 Resource 的只读视图。上传仍走显式的 `resource push`（大文件按 4 MiB 分块）。
- `orrery-resourcefs` 的读路径（manifest 驱动的 inode、分块校验、预签名 GET、LRU 缓存）原则上可以复用到客户端。但在 macOS 上，它要换成或附加一个 NFS 前端，才能做到 hf-mount 那样不需要 kext。这一点交给挂载机制那份调研评估。

---

## 6. 未确认清单

- E2B 是否有 Volume 的 CLI 命令。
- Runloop `rli object upload/download` 的确切 flag。
- Codespaces 是否有官方的 sshfs / 挂载指南。
- DevPod 同步的实现，以及有没有从远端拉回本地的命令。
- Lightning AI 文档（403）里是否有 CLI 之外的本地挂载方式。
- SageMaker、Vertex 是否有平台自己的本地挂载工具（目前只查到云厂商通用的 Mountpoint 和 gcsfuse，二者都只支持 Linux）。
- Pachyderm 最新版文档的 macOS 要求（是否改为 macFUSE 或 FUSE-T），以及 `pachctl get/put file` 的现行语法和挂载是否懒加载。
- Quilt 是否有本地挂载。
