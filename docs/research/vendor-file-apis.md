# 沙箱/容器服务商的文件进出：一手文档核对

面向数据面形状的决策。**这份取代 `sandbox-file-ingress-egress.md` 的 §1**——那份的可信度声明自认「结论来自搜索引擎摘要转述，未逐页打开原文核对」，本份逐页打开了原文，并对其中至少一条结论做了撤回（见 §6）。

> **取证强度**：每一家都打开了一手文档页（官方 docs / API reference / CLI reference），引文保留英文原文。文档没写、只能从上下文推出的结论，一律标「**推断**」。查不到就写「**未找到公开一手资料**」，不猜、不用二手博客充数（`glossary` 类第三方课程内容一律不采）。调研日期 2026-10-15。

## 0. 先读这五条

1. **形状收敛得比旧调研说的更彻底：就是「按路径的文件面」。** 九家沙箱厂商 + 四个容器工具，**没有一家**用资源句柄 / 对象 id 来寻址普通文件——`read(path)` / `write(path, bytes)` 是共同形态。对象 id 只出现在**挂载**那条平行线上（Blaxel drive id、E2B volume、Modal Volume、Vercel Drive），而挂载解决的是「让数据一直看得见」，不是「把文件搬进去」。
2. **字节路径只有三种，且第三种在沙箱厂商里永远是「额外出口」而不是主路径**：①平台 API 中转（Daytona、E2B、Modal、Vercel、Runloop、Northflank）；②**exec + 容器内 helper**（Cloudflare 的 `sandbox-shim`、`docker cp`/`podman cp`/`kubectl cp` 的 `tar | exec`）；③对象存储 + 预签名（E2B `uploadUrl()`/`downloadUrl()`、Runloop `get_download_url()`、Blaxel archive export）。**第②种对我们的 `#19`（hook 与 Worker 是同一个二进制的两个入口）是直接的形状先例，见 §5。**
3. **没有一家沙箱厂商有「声明的产物」概念。** 任务不声明产出、平台不在任务结束时代收；调用方按需拉。**唯一的例外是 Buildkite 的 `artifact_paths`**（§4）——任务在自己的配置里*声明 glob*，**agent 在步骤结束后代为收走**。它是 CI 不是沙箱，但它与 `#7` 要回答的问题**同构**，必须单独标出来。
4. **「沙箱必须活着」不是共识，是两个对立的阵营。** Cloudflare 明写 `Files` **不会**替你启动实例，调用方必须先起实例；Vercel 反着来——`writeFiles` 会自动 resume 一个已停止的常驻沙箱。Daytona/E2B 的文档只示范「先起再操作」；`docker cp`/`podman cp` 是唯一明确支持**已停止**容器的（停止 ≠ 销毁）。
5. **失败语义的文档密度差别巨大。** Cloudflare 一家把错误写到了 `SandboxFileError.code` 级别（`ENOSPC`/`EXDEV`/部分写入/与 helper 通信失败各是一类）；**Runloop、Fly、Northflank 三家的文件文档里没有任何一条失败语义**。而「忘了拿产物」的**明确警告，在 E2B 一手页面里找不到**（§6 撤回）。

---

## 1. 容器工具的经典形态（最广为人知的「list + cp」）

这一组是很多人的直觉来源，也是唯一一组「不需要平台 API、不需要网络服务」的文件面。

| 工具 | 对已停止的容器？ | 字节怎么走 | 明确写出的坑 |
|---|---|---|---|
| **`docker cp`** | ✅ "The `CONTAINER` can be a running or stopped container." | daemon 直接读写容器 rootfs；`-` 走 tar 流 | 见下 |
| **`podman cp`** | ✅ "The containers can be either running or stopped" | 同上 | 见下 |
| **`kubectl cp`** | 文档未明写（示例都是 `exec`，**推断**需要 pod Running） | `tar cf - \| kubectl exec -i -- tar xf -` | 镜像里**没有 tar 就失败** |
| **`docker exec`** | ❌ "runs a new command in a **running** container"；paused 容器回 `Error response from daemon: Container mycontainer is paused, unpause the container before exec` | 容器主进程的 stdin/stdout | 只能跑可执行文件："A chained or a quoted command doesn't work." |

**`docker cp` 的原文要点**（<https://docs.docker.com/reference/cli/docker/container/cp/>）：

- 语义："behaves like the Unix `cp -a` command"，"Ownership is set to the user and primary group at the destination."——**入容器时归 root**，出容器时归调用者；`-a` 才保留源 uid/gid。
- tar 通道："Using `-` as the `SRC_PATH` streams the contents of `STDIN` as a tar archive"，"In this case, `DEST_PATH` must specify a directory."；出方向 "streams the contents of the resource as a tar archive to `STDOUT`"。
- 目录语义靠尾斜杠区分：目标目录已存在时，`SRC_PATH` 以 `/.` 结尾则**只复制内容**，否则把源目录整个塞进去。
- ⚠️ "doesn't create parent directories for `DEST_PATH` if they don't exist."
- ⚠️ "It isn't possible to copy certain system files such as resources under `/proc`, `/sys`, `/dev`"，以及 tmpfs 与用户挂载点；官方给的绕法是自己 `tar` 走 `docker exec`。
- 符号链接：默认复制链接本身，`-L` 才跟随。

**`podman cp` 的原文要点**（<https://docs.podman.io/en/latest/markdown/podman-cp.1.html>）：

- ⚠️ **与 docker 相反**：`"If src_path is local and is a symbolic link, the symbolic target, is copied by default."`——podman 默认复制**目标**。
- `"podman cp does not support globbing (e.g., cp dir/*.txt)."`（绕法：`xargs`/`find`，或 `podman mount`）。
- `--archive` 默认 **true**（`"files copied to a container have changed ownership to the primary UID/GID of the container"`）；`--overwrite` 才允许「文件↔目录」互相覆盖，"otherwise, podman cp errors out"。
- rootless 下 `"podman cp ignores permission errors when copying from a running rootless container"`（TTY 设备属于宿主 root）。

**`kubectl cp` 的原文要点**（<https://kubernetes.io/docs/reference/kubectl/generated/kubectl_cp/>）：文档自己把内部机制写成了示例——

> ```
> # !!!Important Note!!!
> # Requires that the 'tar' binary is present in your container
> # image.  If 'tar' is not present, 'kubectl cp' will fail.
> #
> # For advanced use cases, such as symlinks, wildcard expansion or
> # file mode preservation, consider using 'kubectl exec'.
> tar cf - /tmp/foo | kubectl exec -i -n <some-namespace> <some-pod> -- tar xf - -C /tmp/bar
> ```

`--retries` 默认 `0`（不重试）。

**对我们的意义**：这一组说明「文件面」可以完全没有服务端 API——**发起方拿着一个能 exec 的通道，在两端各起一个 tar 就够了**。这是一种**形状**（不是实现建议）：我们的 Worker 已经有一条到控制层的出站流，文件面可以就是「流上的一个 tar 双向通道」。

---

## 2. 九家逐条

每家按同一组八个问题回答。引文为英文原文。

### 2.1 E2B

来源：<https://docs.e2b.dev/filesystem/upload>、<https://docs.e2b.dev/filesystem/download>、<https://docs.e2b.dev/filesystem/read-write>、<https://docs.e2b.dev/api-reference/filesystem/download-a-file.md>、<https://docs.e2b.dev/sandbox/persistence.md>、<https://docs.e2b.dev/quickstart/upload-download-files.md>

1. **动词与寻址**：`sandbox.files.write(path, data)`、`write_files(files)`、`files.read(path)`；REST 侧 `GET /files?path=`（"Path to the file, URL encoded."）。API reference 里还有 `listdir` / `makedir` / `move` / `remove` / `stat` / `createWatcher` / `watchDir`。**一律按路径**，相对路径相对用户 home。
2. **方向**：全双向 + 列 + 删改 + watch。但**整目录不支持**："We currently don't support an easy way to upload a whole directory."；下载同："You need to download each file separately."
3. **字节走哪条路**：**两条都有**。① REST 直传：响应是二进制的 `format: binary`，"the file extension's media type, or application/octet-stream as fallback"，且支持 `Range`/206、`If-Modified-Since`/304、`gzip`。② **签名 URL**：`sandbox.uploadUrl()` / `sbx.download_url(path, user, use_signature_expiration=...)`——"An upload URL will then be generated with a signature that allows only authorized users to upload files."，需要 `secure: true` 建沙箱，签名可设过期。
   ⚠️ **路由细节值得单独记**：请求头是 `E2b-Sandbox-Id` + `E2b-Sandbox-Port`（默认 `49983`），文档说请求被路由 "to that sandbox's **envd** over the shared sandbox host"——即**平台把 HTTP 请求代理进沙箱里一个监听端口上的 agent**。
4. **声明的产物**：❌ 无。调用方按需 `read()` / `download_url()` 拉。
5. **不收走会怎样**：`.kill()` → "the sandbox is permanently deleted and cannot be resumed"，"The sandbox is terminated and all resources are released. This is a terminal state."；`onTimeout`/`on_timeout` **默认 `"kill"`**（"Defaults to 'kill'; set to 'pause' to auto-pause on timeout"）。`pause` 则 "A paused sandbox is kept **indefinitely**"。**文档里没有「忘了下载」的警告**（见 §6）。
6. **文件面给谁**：给**沙箱外面的调用方**（SDK/API）。但实现上是「平台路由到沙箱内 envd 端口」——沙箱里必须有一个监听的 agent。沙箱里的 agent 要数据是靠自己的网络（`allowInternetAccess` 那条路），不是调这个面。
7. **沙箱是否必须活着**：**推断需要**。文件请求靠 `E2b-Sandbox-Id` 路由；文档里最接近「不可达」的错误是 **502 `SandboxRoutingError`**（"the proxy couldn't locate the sandbox"），且明确说 **没有**单独的 stopped/paused 状态码。
8. **已知坑**：体积上限**未文档化**；目录级上传/下载不支持；传目录路径回 400；404 = 文件不存在；磁盘 Hobby 10 GiB / Pro 20 GiB。

### 2.2 Daytona

来源：<https://www.daytona.io/docs/en/file-system-operations>、<https://www.daytona.io/docs/en/persistence/>

1. **动词与寻址**：全按路径，SDK `sandbox.fs.*`：`list_files`、`get_file_info`、`create_folder`、`upload_file`、`upload_files`、`upload_file_stream`、`download_file`、`download_files`、`download_file_stream`、`delete_file(recursive)`、`set_file_permissions`、`search_files`（glob 文件名）、`find_files`（文件内容）、`replace_in_files`（文本替换）、`move_files`。REST：`POST /files/upload-v2?path=`（multipart）、`GET /files/download?path=`、`POST /files/bulk-upload`、`POST /files/bulk-download`、`DELETE /files?path=&recursive=true`、`POST /files/move?source=&destination=`、`GET /files/search`、`GET /files/find`、`POST /files/replace`、`POST /files/permissions`、`GET /files/info`、`POST /files/folder`。
   > "File operations assume you are operating in the sandbox user's home directory"，"Use a leading `/` when providing absolute paths"，"operations run through the Daytona API … **without executing shell commands inside it**"。

   **最后半句很重要**：Daytona 明确说这条路不发 shell 进沙箱。
2. **方向**：最全的一家——双向 + 列 + 删 + 改（含内容替换）+ 权限 + 流式。
3. **字节走哪条路**：平台 API 中转，**有流式档**（"Uploads and downloads can stream to and from local paths, so large files transfer without being loaded into memory."）。REST 侧 streaming **没有对应的 endpoint 列出**。无预签名 URL 直出（volume 是 S3-backed 挂载，不是下载通道）。
4. **声明的产物**：❌ 无。
5. **不收走会怎样**：**默认持久**。"Daytona sandboxes are persistent by default. Stopping a sandbox does not destroy it"，"the sandbox keeps its identity, its filesystem, and its configuration, and can be started again"。但**分档，且 GPU 档会丢**：
   - Container：停后文件留在 runner 并**继续占磁盘配额**；"Archive moves it to object storage and frees the quota"（auto-archive 默认 **停 7 天后**，上限 30 天）。
   - VM：停时卸载到附近存储并释放配额，启动时恢复；auto-pause 默认 60 分钟且**保文件系统 + 内存**。
   - ⚠️ **GPU："Ephemeral by design: deleted on stop, filesystem is not preserved."** 官方给的绕法是 volumes。
   - 明写的警告："Filesystem and memory persistence are tied to a single sandbox: deleting the sandbox deletes its state."；"Ephemeral sandboxes are deleted as soon as they stop, discarding all state"。
6. **文件面给谁**：明确给沙箱外面的调用方（"without executing shell commands inside it"）。
7. **沙箱是否必须活着**：**文档没有正面回答**，也没有说文件 API 能在 stopped 上用。例子一律是 `stop()` → `start()` → 再操作（**推断**需要 running）。
8. **已知坑**：没说体积上限；**没有任何路径穿越 / 符号链接校验的说明**；删除目录要显式 `recursive: true`。

### 2.3 Modal（Sandboxes）

来源：<https://modal.com/docs/guide/sandbox-files>、<https://modal.com/docs/guide/sandbox>、<https://modal.com/docs/guide/sandbox-snapshots>、<https://modal.com/docs/reference/modal.Sandbox>

1. **动词与寻址**：`sb.filesystem.write_text / read_text / write_bytes / read_bytes / copy_from_local / copy_to_local / make_directory / list_files / stat / remove / watch`，另有 `snapshot_directory`、`mount_image`。全按路径。
2. **方向**：全双向 + 列 + 删 + watch。
3. **字节走哪条路**：SDK 流式——"It has convenience APIs for **streaming file copies in both directions**"。
   ⚠️ **唯一一家在文档里写死体积的一家**："These APIs may be used to read files of up to **5GB** and write files of any size."
4. **声明的产物**：❌ 无。
5. **不收走会怎样**：Filesystem Snapshot 是 "copies of the Sandbox's root filesystem at a given point in time"，"These Snapshots are Images and can be used to create new Sandboxes"，**默认 30 天 TTL**（"Previously, Filesystem Snapshots persisted indefinitely"），可 `ttl=None` 永久；"Deletion is irreversible. Deleted snapshots cannot be recovered."。触发时机："Snapshots are triggered from outside the Sandbox, typically **just before termination**."。**终止后未 snapshot 的文件能不能恢复：文档没有表述**。文档只说 "Once finished, no further commands can be executed inside the Sandbox."
6. **文件面给谁**：给沙箱外面的调用方（SDK 侧）。
7. **沙箱是否必须活着**：参考页**没有**对 `filesystem.*` 写 running 要求（只有 `mount_image` / `snapshot_directory` 明确写 "in a **running** Sandbox"）。**推断**需要 running，文档未明写。
8. **已知坑**：读 5GB 上限；watch 有 `filter`/`recursive`/`timeout`；Volumes 是另一条线——"changes are persisted by background commits that run every few seconds while the Sandbox executes" 加 "a final commit when the Sandbox terminates"，但 "You need to explicitly reload a Volume to see changes made since it was first mounted"（`.reload_volumes()`）。

### 2.4 Vercel Sandbox

来源：<https://vercel.com/docs/sandbox/sdk-reference>、<https://vercel.com/docs/sandbox/concepts/persistent-sandboxes>、<https://vercel.com/docs/sandbox/concepts/drives>

1. **动词与寻址**：三层。①`Sandbox`：`writeFiles([{path, content: Buffer, mode?}])`、`readFile({path, cwd?})` → `ReadableStream | null`、`readFileToBuffer()`、`downloadFile({src:{path,cwd}, dst:{path,cwd}, mkdirRecursive})`、`mkDir(path)`。②`SandboxUser`（多 agent 各自用户）：同名方法，但 `content: string | Uint8Array`。③`sandbox.fs`（`FileSystem`）：`readFile/writeFile/appendFile/mkdir/readdir/stat/lstat/realpath/mkdtemp/unlink/rm/rmdir/rename/copyFile/truncate/chmod/chown/symlink/readlink/access/exists`。
   > "`FileSystem` currently implements a focused **subset of `node:fs/promises`** for sandbox workflows. APIs such as file handles and watchers are not currently included."

   ⚠️ 读不存在的文件返回 `null`，不是抛错："The promise resolves to `null` when the file does not exist."
2. **方向**：全双向 + 列 + 删改 + 符号链接 + 权限。
3. **字节走哪条路**：SDK 流（`ReadableStream` / `Buffer`）。**没有预签名 URL 的文件面**；大数据走另外两条线——`mount-remote-storage`（FUSE 挂 S3）与 **Drives**。
4. **声明的产物**：❌ 无。**但有 Drives**：挂在路径上的持久目录，"Files written to a drive persist after the sandbox stops."，默认 1 TiB / 上限 16 TiB，一台最多挂 4 个，**同一 drive 同一时刻只能一个读写挂载**（共享读用 `drive.snapshot()`）。
5. **不收走会怎样**：⚠️ **这是唯一把「沙箱停止」从失败语义里彻底拿掉的一家。**
   > "Sandboxes are **persistent by default**: when a sandbox stops, the SDK snapshots its filesystem and restores it on the next resume."；"Pass `persistent: false` to opt out."

   快照 `snapshotExpiration` **默认 30 天**（`2,592,000,000` ms，从「上次使用」起算，每次使用重置），`0` = 永久；`keepLastSnapshots` 1–10。非持久沙箱 "discard their filesystem when the session stops"。另有兜底回收："Vercel removes sandboxes that can't resume from a snapshot after **14 days of inactivity**."
6. **文件面给谁**：给沙箱外面的调用方。`SandboxUser` 那一层是给「每个 agent 一个 Linux 用户、共享目录用 group」的多 agent 场景。
7. **沙箱是否必须活着**：**不需要。**
   > "If a persistent sandbox is stopped and you call `runCommand`, `writeFiles`, or other SDK methods on it, the SDK **automatically starts a new session and retries** the operation."

   两个例外不自动 resume：`sandbox.stop()`、`sandbox.update()`。`delete()` 之后 "the instance becomes inert: further API calls throw immediately."；`snapshot()` 之后 "the sandbox shuts down automatically and becomes unreachable. … any subsequent commands to the sandbox will fail."
8. **已知坑**：**没有体积上限的文档**；"Methods throw Node.js-style filesystem errors with fields such as `code`, `syscall`, and `path`."；没有 watcher/文件句柄。Drive 不能删正在被挂载的，"A drive cannot be deleted if it is currently attached to a running sandbox."

### 2.5 Runloop

来源：<https://docs.runloop.ai/docs/devboxes/files>、<https://docs.runloop.ai/docs/storage-objects/overview>、<https://docs.runloop.ai/docs/devboxes/overview>、<https://docs.runloop.ai/docs/devboxes/snapshots>

1. **动词与寻址**：只有四个：`devbox.file.write(file_path, contents)`、`read(file_path)`、`upload(path, file)`、`download(path)`。⚠️ 参数名不统一（read/write 用 `file_path`，upload/download 用 `path`）。相对路径相对 home。**没有 list / delete / move 的文档**。
2. **方向**：双向；小文本用 write/read，大或二进制用 upload/download（"files of any sizes"）。
3. **字节走哪条路**：SDK（TS upload 传 Node read stream，download 返 blob）。**另有对象存储线**：`storage_object.upload_from_file()`，取回用 `get_download_url(secondsToExpire)`——"Create secure, time-limited URLs for downloading object content."（示例 3600 秒；**没有 min/max 的文档**）。
4. **声明的产物**：❌ 无。但**输入侧有「创建时声明」**：mounts 只能在**建 devbox 时**声明，`code_mount` / `object_mount` / `file_mount`（"Mount Files Inline"——把文件内容在创建时注入）。
5. **不收走会怎样**：**未找到公开一手资料**。overview 只说 devbox 是 "Isolated, ephemeral virtual machines … created on demand, and deleted when they are no longer needed"，示例以 `await devbox.shutdown()` 收尾，但**没有一句讲 shutdown 之后文件如何**。Snapshot（"Currently only disk snapshots are supported"，"By default, snapshots persist indefinitely"）是**快照**，不等于沙箱持久。
6. **文件面给谁**：给外面的调用方（SDK）。
7. **沙箱是否必须活着**：**未找到公开一手资料**；object mount 是创建时声明。
8. **已知坑**：这一家的文件文档最薄——无 list/delete、无体积上限、无失败语义、"file not found"/"permission denied" 只在 best-practices 里作为「要写错误处理」提了一句。

### 2.6 Cloudflare（Sandbox SDK / Containers）

来源：<https://developers.cloudflare.com/sandbox/files/>、<https://developers.cloudflare.com/sandbox/files/manage-files/>、<https://developers.cloudflare.com/sandbox/reference/files/>、<https://developers.cloudflare.com/sandbox/concepts/lifetime/>

1. **动词与寻址**：`Files` 类——`readFile(path, opts)` → `Response`、`writeFile(path, content, opts)`、`stat`、`lstat`、`readDirectory`、`mkdir({recursive})`、`rename(source, destination)`、`remove({recursive, force})`。全部按路径，加 `cwd` / `user`（如 `"1000:1000"`）/ `signal` 三个选项。`writeFile` 收 `string | ArrayBuffer | ArrayBufferView | Blob | ReadableStream<Uint8Array>`，"A string is written as UTF-8."
2. **方向**：全双向 + 列 + 删改。
3. **字节走哪条路**：⚠️ **exec + 容器内 helper，不是平台 API 中转**。`new Files(container)` 只吃一个 `container.exec`；实现靠镜像里的 `/usr/local/bin/sandbox-shim`。文档的端到端例子就是「Worker 侧 `writeFile` 流式写入 → `container.exec(["gzip", ...])` → `readFile` 流式读回」。
   > "`writeFile()` streams the upload into the file, and `readFile()` returns a Response that streams the output file back."；"Neither method holds a whole file in the memory of the Durable Object."；"The body applies backpressure to the helper process."

   另外两条独立线：`S3Mount`（挂 R2，"Renames, locks, and permissions do not work as they do on a local disk"）、`DirectoryBackup`（把一个目录备份到 R2，"Saves one directory, and your Durable Object stores each backup record"）。
4. **声明的产物**：❌ 无（备份是调用方显式调的方法）。
5. **不收走会怎样**：⚠️ **最狠的一句**——"**Stopping the instance deletes the files on its disk.**"，概念页标题就是 "**Files and processes end with the instance**"。快照 `snapshotContainer()` "saves the writable root filesystem of a running instance"，"It does not save separately mounted filesystems, memory, or processes."，"**A snapshot expires 30 days after it is created or last restored.**"，"Files in `/run` are in memory, and a snapshot does not save them."。挂载的 R2 桶例外："They remain after every instance stops"。
6. **文件面给谁**：**明确是沙箱外面的 Worker**——"The `Files` class from `@cloudflare/sandbox` moves files between your **Worker** and a running sandbox"，`Files` 是从 Durable Object 里构造的。
7. **沙箱是否必须活着**：**必须**，而且声明得很直白："`Files` **does not start the instance**, so your Durable Object starts it before each file operation."
8. **已知坑**：⚠️ **唯一一家把「路径校验责任归调用方」写进文档的**：
   > "`Files` does not restrict which paths it opens"，示例自己拒绝绝对路径、空串、NUL 字节与任何 `..` 段；并警告沙箱里的代码可以 "create a symbolic link in `/workspace` that points elsewhere"；`user` 选项 "does not narrow access either"，只 "sets the owner of files that `Files` creates"。

   错误面写得最细：`SandboxFileError`（`code` 是 Linux 名如 `ENOENT`/`ENOSPC`/`ENOTDIR`/`EILSEQ`，"`SandboxFileError` is not a class. Use `SandboxFileError.is(error)` instead of `instanceof`"）、`SandboxProtocolError`（"`Files` could not complete its exchange with the helper binary."，镜像 tag 不匹配会触发）、以及**不包装运行时错误**（"A stopped container or an image lacking `/usr/local/bin/sandbox-shim` surfaces an `exec()` error"）。
   其他：`writeFile` 失败可能留半截——"A later failure can leave the file created, truncated, or partially written."；`rename` 跨文件系统 "rejects with `EXDEV`"，"The method does not fall back to copying and deleting."；`readDirectory` **不递归、不排序、不含元数据**，且 "Each call … starts one process in the container"，大目录建议改用 `container.exec(["find", directory])`；`remove` recursive "removes the directory tree without following symbolic links inside it"。

### 2.7 Fly.io（Machines）

来源：<https://docs.fly.io/machines/api/>、<https://docs.fly.io/flyctl/sftp>、<https://docs.fly.io/flyctl/ssh-console>

1. **动词与寻址**：⚠️ **Machines API 没有文件端点**——资源只有 machines / apps / certificates / tokens / volumes（volumes 是块存储供给，不是文件传输）。文件面在 **CLI**：`fly sftp get`（"retrieves a file from a remote VM"）、`fly sftp put`（"uploads a file to a remote VM"）、`fly sftp find`（"lists files (from an optional root directory) on a remote VM"）、`fly sftp shell`（"brings up an interactive SFTP session"）。按路径。
2. **方向**：SFTP 双向 + 列；删改靠 `shell` 或 `fly ssh console`。
3. **字节走哪条路**：**SFTP 直连 VM**（要 SSH）。平台 API 不经手字节。
4. **声明的产物**：❌ 无。
5. **不收走会怎样**：**未找到公开一手资料**。Fly 的持久化机制是 Volumes（挂载点），Machine 自身的 rootfs 不是持久面——但这句话在这几个页面里都没有写成原文。
6. **文件面给谁**：给外面的调用方，且要求机器上跑着 sshd（`fly ssh console` 的描述是 "Connect to a **running** instance of the current app."）。
7. **沙箱是否必须活着**：`fly ssh console` 写的是 running instance；`fly sftp` 页**没有写运行状态要求**——**推断**需要 running + SSH 启用，文档未明写。
8. **已知坑**：未找到（页面不含体积、校验、失败语义）。

### 2.8 Blaxel

来源：<https://docs.blaxel.ai/Sandboxes/Filesystem.md>、<https://docs.blaxel.ai/api-reference/archive/export-the-filesystem-changes-to-a-presigned-url.md>

1. **动词与寻址**：SDK `fs.write(path, content)` / `writeTree(files, baseDir)` / `writeBinary()` / `read()` / `readBinary()` / `download()` / `cp()` / `mkdir` / `ls` / `find` / `grep` / `rm` / `watch`（可带内容或 ignore list）。HTTP API 侧更多：`create-or-update-a-file-or-directory`、`get-file-or-directory-information`、`stat`、`delete`、`get/create/delete-directory-tree`、`find`/`fuzzy-search`/`search-for-text-content`、`stream-file-modification-events`，以及一整套 multipart 上传（initiate / part / complete / list-parts / list-uploads / abort）。
   ⚠️ **路径编码怪癖**：URL 里 "The API resolves a path without a leading slash relative to the filesystem service's working directory"，绝对路径要把前导斜杠编码成 `%2F`；CLI 用双斜杠。SDK 侧正常。
2. **方向**：全双向 + 列 + 删改 + 搜索。
3. **字节走哪条路**：HTTP 中转，**5 MB 单请求硬上限**："a single request is capped at 5 MB"，"Requests larger than this are rejected with an **HTTP 413 before they reach the sandbox**"；更大走 multipart（SDK 自动按 5 MB 分片）。
4. **声明的产物**：❌ 无「声明清单」，但 ⭐ **有平台侧主动收录这个形状**：`POST /archive/export` —— "Archives **everything the sandbox changed** on top of its base image and streams it, uncompressed, to a **presigned S3 PUT URL**."（详见 §4）
5. **不收走会怎样**：**未找到公开一手资料**（文档里有 TTL / 生命周期清理的 best-practices，但没有「销毁后文件如何」的原文）。
6. **文件面给谁**：给外面的调用方。
7. **沙箱是否必须活着**：**必须**。export 会先把沙箱 **quiesce**："The sandbox is quiesced first"（保存进程列表、停掉所有进程），之后**拒绝一切写文件系统的 API 调用**；"The freeze is not lifted afterwards"，要 `POST /archive/resume` 才解冻。
8. **已知坑**：5 MB 单请求 / 5 MB 分片；archive **不压缩**（所以 `size` 在开始上传前就已知），单 PUT 上限 5 GB，更大走 multipart，multipart 需要 `abortUrl`（预签名 DELETE），否则失败会留下分片。明写的三条警告：
   - "**Do not pipe base64-encoded file contents through process logs.**"——"Output can be truncated, and file contents are exposed in retained logs."
   - bulk read 与 archive 两种整目录法**都不保证一致快照**："Neither recipe guarantees a consistent snapshot if files change during the operation."
   - archive 路线要求镜像里有 `tar`，且 "Keep the temporary archive outside the directory you are exporting."
   - `find` **默认只返 20 条结果**（要传多一条来判断溢出）。

### 2.9 Northflank

来源：<https://northflank.com/docs/v1/api/copy-files.md>、<https://northflank.com/docs/v1/application/run/transfer-data-to-and-from-containers.md>

1. **动词与寻址**：**CLI/JS client 的 copy**，不是 HTTP 端点。`northflank [download|upload] service file <args>`（job 同理）；JS `apiClient.upload.service.files({ projectId, serviceId }, { localPath, remotePath })`。按路径。
2. **方向**：双向，"single files or entire directories"。`--localPath` 可省（用 CLI 当前目录），`--remotePath` 可省（用 "the default home directory of the container user"）；"**If the target directory does not exist, it will be created.**"
3. **字节走哪条路**：CLI/JS client 经平台 API（协议细节未文档化）。其余是「自己搭」：rsync over 转发端口、curl/wget、`python3 -m http.server`，都要求容器里跑着这些东西。
4. **声明的产物**：❌ 无。
5. **不收走会怎样**：**未找到公开一手资料**（有 volumes 的备份/克隆/恢复文档，但没有「沙箱销毁后文件如何」）。
6. **文件面给谁**：给外面的调用方（CLI/JS client）；容器内的搬运靠 shell 里的工具。
7. **沙箱是否必须活着**：**是**——文档把范围一致地限定在 "running workloads (services or jobs)"。
8. **已知坑**：**没有体积上限、没有错误语义**的文档（文档明说细节看 `--help`）。

---

## 3. 八问对照矩阵

| | 寻址 | 动词面 | 字节路径 | 声明产物 | 停/销毁后 | 面给谁 | 必须活着 |
|---|---|---|---|---|---|---|---|
| **E2B** | 路径 | 读/写/列/移/删/watch | API 中转 **或签名 URL** | ❌ | pause 永久保留；kill 终态 | 外部（但路由进沙箱内 envd） | 推断需要（502 路由不到） |
| **Daytona** | 路径 | 最全（含内容替换/权限/流式） | API 中转（含流式） | ❌ | 默认持久；**GPU 停即删**；7 天后归档 | 外部（明确"不发 shell 进沙箱"） | 推断需要 |
| **Modal** | 路径 | 读/写/列/删/watch | SDK 流式 | ❌ | 快照 30 天 TTL；否则未写 | 外部 | 未写（推断需要） |
| **Vercel** | 路径 | 读/写/列/删改/符号链接 | SDK 流 | ❌ | **默认持久，快照 30 天** | 外部 | ❌ **自动 resume** |
| **Runloop** | 路径 | 只有 read/write/upload/download | SDK + 对象存储预签名 URL | ❌ | **未找到** | 外部 | **未找到** |
| **Cloudflare** | 路径 | 读/写/列/删改 | **exec + 容器内 helper** | ❌ | **停止即删盘**；快照 30 天 | **Worker 侧**（沙箱外） | ✅ **必须，Files 不替你启动** |
| **Fly.io** | 路径 | SFTP get/put/find/shell | SFTP 直连 | ❌ | **未找到** | 外部（要 sshd） | 推断需要 |
| **Blaxel** | 路径 | 最全（含 grep/watch/multipart） | API 中转，**5 MB/请求** + **archive→预签名 S3** | ❌（但有 archive export） | **未找到** | 外部 | ✅ 是（export 会冻结） |
| **Northflank** | 路径 | CLI copy（文件/目录） | CLI→API | ❌ | **未找到** | 外部 | ✅ 是 |
| **docker cp** | 路径 | cp 一个/双向，`-` 走 tar | daemon 直读 rootfs | ❌ | ✅ **stopped 容器也行** | 宿主 | 不必 |
| **podman cp** | 路径 | 同上 | 同上 | ❌ | ✅ stopped 也行 | 宿主 | 不必 |
| **kubectl cp** | 路径 | cp 一个/双向 | `tar \| exec` | ❌ | 未写 | 外部 | 推断需要 |
| **docker exec** | — | 任意命令 | stdin/stdout | ❌ | ❌ | 宿主 | ✅ 必须 running |

**没有一家有「list + cp」之外的第四个形状。** 唯一的横向差异在**字节路径**与**「必须活着」**这两列。

---

## 4. ⭐ 「声明的产物清单」：唯一先例（不在沙箱厂商里）

**沙箱厂商：0 家。** 九家没有任何一家提供「任务声明它产出哪些文件、由平台在结束时收走」的机制。全部是调用方按需拉。

**但 CI 里有一家，而且形状正好命中 `#7` 的问题**：Buildkite 的 `artifact_paths`（<https://buildkite.com/docs/pipelines/artifacts>）：

> "Set the `artifact_paths` attribute" … "to upload artifacts **after the command step has finished its work**"；
> 该属性 "can contain an array of file paths or" "glob patterns"（示例 `"logs/**/*"`、`"coverage/**/*"`）；
> "**Buildkite agents** upload artifacts to a storage service during a build."。

三件值得记的事实：① **声明的是 glob，不是文件清单**——任务不需要知道实际产出哪些文件；② **收走的动作发生在「步骤结束之后」，由 agent 执行，不是由任务自己的命令执行**；③ 同一条命令里也能手动 `buildkite-agent artifact upload`。

> **为什么这条对我们比对别人更重要**：Buildkite 的 agent 跟我们的 Worker 是**同一类东西**（跑在机器上、任务之外、平台信任的那一半）。「任务声明 glob → agent 在任务结束后代收」是**已经被实现的形状**，而且它不要求任务自己能上网、也不要求任务持有凭据。

对照：GitHub Actions 的 `upload-artifact` 也在步骤里声明路径，但**上传是步骤里那个 action 自己发起的**（客户端直传签名 URL），平台不在任务结束时代收——严格说它不是「平台代收」形状。

另外 Blaxel 的 `POST /archive/export` 是**「平台侧一次性收走整个变更集」**的另一种写法（"Archives everything the sandbox changed on top of its base image and streams it, uncompressed, to a presigned S3 PUT URL."），但它是**调用方显式触发**、且会把沙箱冻结到需要显式解冻，不是「任务结束时自动收」。

---

## 5. 对我们这张票的结论

### 5.1 形状：按路径的文件面（哪几个动词），还是特殊目录 + 目录同步

**证据强度：强（九家一手文档逐一核对，收敛一致）。**

厂商收敛在**按路径的文件面**上，且动词面高度重合，可以压成四个：**写（文本/二进制）、读（文本/二进制/流）、列（可能带 find/grep）、删改（remove/rename/mkdir）**。语法糖分两档：Vercel/Cloudflare/Modal 直接给 `node:fs`-风味的一套；Daytona/E2B/Blaxel 给一个更窄的领域动词集。**没有任何一家用资源句柄寻址普通文件。**

「特殊目录 + 目录同步」（挂载）是**并行的第二条路**，不是文件面的替代，而且**每家都承认它有代价**：Cloudflare 说挂载的 R2 上 "Renames, locks, and permissions do not work as they do on a local disk"；Modal 说 Volume "You need to explicitly reload a Volume to see changes made since it was first mounted"；Vercel 说 Drive 同一时刻只能一个读写挂载。**挂载解决的是「数据一直看得见」，不是「把文件搬进去」——两者不互相取代。**

**对我们的一个硬约束（证据强度：中，属推断但约束来自我们自己的前提）**：先行形态里「平台 API 中转」全都隐含**平台能直接碰到沙箱**——E2B 最露骨，文件请求是**路由进沙箱里 envd 的监听端口**（`E2b-Sandbox-Port`，默认 49983）。我们的前提是**沙箱零监听端口、控制层碰不到宿主文件系统、Worker 主动出站拨号**，所以**API 中转这一形态对我们不直接成立**。剩下两条可用：

- **(a) 把文件面做成 Worker 侧的服务**，字节走 Worker 已经拨通的那条出站流（等价于把 Daytona 的「按路径文件 API」搬到 Worker 进程里实现，而不是搬进沙箱里）；
- **(b) 对象存储 + 预签名 URL**（与 `#5` 已定的路线一致），Worker 直传、控制层签发。

而且第 (a) 条**有形状先例，且与我们 `#19` 同构**：Cloudflare 的 `Files` 类**只吃一个 `container.exec`**，真正的读写由镜像里的 helper 二进制 `sandbox-shim` 完成，字节从 Worker 流进去、流回来，两边都不落地整个文件。这与 `#19`「hook 与 Worker 是同一个二进制的两个入口」是同一个构造：**同一个二进制，一处当命令、一处当 helper**。差别只在于 Cloudflare 的 exec 是平台内建的，而我们的 exec 必须走 Worker 自己的调度路径。**这是本次调研里对我们最有价值的一条先例。**

### 5.2 时机：产物由谁在什么时候触发搬运

**证据强度：强（「没有人做声明式产物」这件事在所有九家都能证伪式核对）；Buildkite 那条是单一先例。**

- **沙箱厂商：调用方按需拉。** 平台不知道什么是「产物」，也没有「任务结束」这个可挂钩的时刻（沙箱生命周期与任务生命周期在各家都是分开的）。
- **「忘了拿」的处理分三档，没有一家靠警告：**
  1. **默认持久 + 自动快照**（Vercel 默认持久、30 天快照 TTL；Daytona 默认持久、停止只释放配额）；Vercel 甚至把它从失败语义里拿掉了——停了的沙箱上 `writeFiles` 会**自动 resume**。
  2. **停止即毁，靠快照兜底**（Cloudflare："Stopping the instance deletes the files on its disk."，快照 30 天）。
  3. **默认就毁，什么都不兜**（E2B `onTimeout` 默认 `"kill"`；Daytona **GPU 档** "deleted on stop, filesystem is not preserved"）。
- ⚠️ **撤回一条旧结论**：旧调研说「E2B 反复强调文件随沙箱上下文销毁、必须在 `kill()` 之前下载，『忘了下载产物』被列为结果丢失的头号原因」。**逐页打开 E2B 一手页面后找不到这段话**——`persistence.md` 全文里 `download` / `lost` / `lose` / `retrieve` **各 0 次**。E2B 只写 `.kill()` 后 "permanently deleted and cannot be resumed"，**没有一句「记得先下载」**。这条应标为二手转述，不作为证据使用。
- **唯一「任务声明 + 平台代收」的实现是 Buildkite 的 `artifact_paths`**（§4）。**它不推翻「先行形态里没有声明产物清单」这句话对沙箱厂商的适用性，但推翻「这个形状不存在」**——它存在，只是长在 CI 上，且执行者（agent）跟我们 Worker 是同一种角色。

### 5.3 失败语义：文档里明写了什么

**证据强度：强（正反两面都核对了——写了什么、以及哪些家什么都没写）。**

明写了的：

- **Cloudflare**（最细）：`SandboxFileError` 带 Linux `code`（`ENOENT`/`ENOSPC`/`ENOTDIR`/`EILSEQ`…）与 `operation`/`path`/`destination`；`SandboxProtocolError` = "could not complete its exchange with the helper binary"（镜像 tag 不匹配即触发）；**运行时错误不被包装**——容器已停止或镜像缺 `sandbox-shim` 时直接冒 `exec()` 的错误；`writeFile` 失败**可能留下创建了/截断了/写了一半的文件**；`rename` 跨文件系统 `EXDEV` 且**不 fallback 到复制+删除**；递归失败**可能留下已建的父目录或半棵树**；abort 之后**已发生的副作用不回滚**。
- **Vercel**：停了的常驻沙箱 → **自动 resume 并重试**；`delete()` 后 "further API calls throw immediately"；`snapshot()` 后 "becomes unreachable … any subsequent commands to the sandbox will fail"；文件错误是 Node 风格（`code`/`syscall`/`path`）。
- **E2B**：**502 `SandboxRoutingError`** 是「路由不到沙箱」的唯一表达，且明确**没有** stopped/paused 的错误码；400/401/404/406/416/500 的语义逐条列出。
- **Daytona**：删除沙箱删状态；GPU 停即删；ephemeral 停即丢。
- **docker / podman / kubectl**：容器 stopped 仍可 `cp`（daemon 直接读 rootfs）；`docker exec` 对 paused 容器明确报错；`kubectl cp` **镜像没 tar 就失败**。

**什么都没写的**：**Runloop、Fly.io、Northflank 三家的文件文档里没有一条失败语义**（无超时、无重试、无沙箱不可达的行为）。Modal 也没有——它的文件指南里没有一句关于「沙箱已终止时调 filesystem 会怎样」。

**对我们的意义**：这是一个空位。我们撞上的失败模式（Worker 掉线、控制层换流、沙箱被拆除）在先行形态里**没有现成措辞可抄**——只有 Cloudflare 的「helper 通信失败」（`SandboxProtocolError`）与 E2B 的「路由不到沙箱」（`SandboxRoutingError`）这两条最接近，可以作为命名的参照。

---

## 附：未能证实 / 未覆盖

- **Runloop**：devbox shutdown 后文件是否保留（只有快照，无生命周期的原文）；文件 API 是否要求 devbox running；体积上限。**未找到公开一手资料**。
- **Fly.io**：Machine 停止后 rootfs 里的文件是否保留；`fly sftp` 是否要求机器 running、是否需要预先启用 SSH。**文档未写**。
- **Northflank**：文件拷贝的体积上限与错误语义。**文档未写**（明说看 `--help`）。文档里给出的备选（rsync/curl/wget/python http.server）都是「在容器里跑服务再转发端口」，与我们的零监听端口前提直接冲突，仅作反面记录。
- **Modal**：沙箱终止后未做 snapshot 的文件能否恢复；`filesystem.*` 是否要求 running。**文档未写**。
- **E2B**：单个文件的体积上限；`allowInternetAccess` 默认值。**本次未复核**（后者沿用旧调研）。
- **各家是否有路径穿越/符号链接防护**：**只有 Cloudflare 明确说「不做限制，责任在调用方」**；其余八家**一个字都没写**。这对我们是负面证据，不是正面证据。
- **Cloudflare 的 `mountBucket` / `s3fs-fuse` / `credentialProxy`**：本次**未复核**（不在本票的「文件进出」范围内），旧调研 §2 关于它的描述仍待独立核对。当前文档里对应的入口是 `S3Mount` 类与 `mount-an-r2-bucket` 页。
