# 文件搬运的实现机制与路径安全：一手来源核对

面向「文件在宿主/容器/对象存储之间怎么搬」这一层。**本票不重复 `vendor-file-apis.md` 的结论**——那份写清了九家厂商的文件面**形状**（动词、时机、失败语义）；本票只写增量：**实现机制 + 每条路径上的路径安全与攻击面**，以及这些先例对我们落地侧三条死规矩的支持或威胁。

> **取证强度**：每条断言标来源。凡标「**文档**」= 官方文档原文（我打开了页面）；「**源码**」= 官方仓库源码（我打开了文件，逐行读过）；「**推断**」= 从以上两者推出、原文没说；「**[未证实]**」= 查了但没查到。**本票零实测**——没有跑过任何 docker cp / kubectl cp / openat2 实验。调研日期 **2026-10-16**；版本敏感处标版本（moby master、containerd main、Go 1.24/1.22/1.20、kubectl 1.34–1.36）。

## 0. 先读这五条

1. **搬字节只有四种机制，路径安全的责任永远在「落地那一侧」。** 四种是：宿主侧二进制自己打 tar（`docker cp`、`containerd`）；容器内 `tar` + exec（`kubectl cp`）；SFTP 直连（Fly.io）；对象存储中转（S3 预签名）。**四种都出过路径穿越**，而**四次的根因是同一个**（§8）。
2. **共同根因是「用户态字符串规范化」**：把安全实现成**改写**输入（剥掉一层 path root、剥掉前导斜杠、折叠 `..`、去尾斜杠）而不是**拒绝**输入。GNU tar `safer_name_suffix`、node-tar 的 single-root-strip、`kubectl cp` 的 `stripPathShortcuts` 是三个独立实现的同一个错误。`kubectl cp` 从 2018 到 **2026** 修了六轮，仍在同一处（最新一条 CVE-2026-19444，2026-08）。
3. **成熟实现的收敛解法是「把路径判定交给内核」**：moby 的 `go-archive` 已经把整条解包路径改成 `os.Root`（openat2 语义），containerd 用 `continuity/fs.RootPath`（securejoin 路线）。同一件事的两种世代，都在**拒绝**而不是**改写**。**这是本票对我们落地侧最有价值的先例。**
4. **`os.Root` 不是万能，官方文档自己列了它不管什么**：不禁止跨 bind mount、不禁止 `/proc` 特殊文件、不禁止打开 Unix 设备文件；Unix 上 `Chmod/Chown/Chtimes` 若文件中途变成符号链接仍有竞态。这**正好**说明我们第三条死规矩（只处理普通文件）不是多余的。
5. **没有一家做「临时文件 + 原子 rename」**：moby、containerd、kubectl cp 全是就地 `O_TRUNC` 写（半截文件是它们的已知失败态）。我们的原子落地**比全部先行形态都严**，代价是临时名、同挂载点、崩溃残留三件事要自己认领（§9 缺的两条）。

---

## 1. `docker cp`：宿主侧二进制打 tar，daemon 直接读写 rootfs

**怎么做的**（源码：`moby/moby` `daemon/archive.go`、`daemon/archive_unix.go`；`docker/cli` `cli/command/container/cp.go`）

- 三个 daemon 方法：`ContainerStatPath` / `ContainerArchivePath`（出方向，返 `io.ReadCloser` + 路径是文件还是目录）/ `ContainerExtractToDir`（入方向）。tar 由 **daemon 侧**（宿主二进制）生成与消费，**因此不要求容器里有 `tar`**——这是它与 `kubectl cp` 最大的机制差别。
- 出方向：`containerArchivePath` 只做 `filepath.Join("/", path)` 把路径变成容器内绝对路径，再 `cfs.Stat`；**没有 `EvalSymlinks`，没有 followLink 参数**。tar 流由 `pkg/archive`（现 `github.com/moby/go-archive`）打。
- 入方向：`containerExtractToDir` 里 `absPath, err := filepath.EvalSymlinks(filepath.Join("/", path))`，注释写着「the destination path needs to be resolved with all symbolic links followed」「we need to also evaluate the last path element if it is a symlink」；`os.Lstat` 确认是目录（否则 `extraction point is not a directory`）；`checkWritablePath` 遍历 `ctr.MountPoints`，落在只读 volume 或 `ReadonlyRootfs` 上就拒绝；最后 `archive.UntarUncompressed`。
- CLI flags（源码）：`flags.BoolVarP(&opts.followLink, "follow-link", "L", false, "Always follow symlinks in SRC_PATH")`、`flags.BoolVarP(&opts.copyUIDGID, "archive", "a", false, "Archive mode (copy all uid/gid information)")`、`-q/--quiet`（无 TTY 时自动静默）。`-L` 的语义是「总是跟随 SRC_PATH 里的符号链接」，实现在 CLI 侧解析源、把结果写进请求（源码）；**它不是安全开关**。
- `-` 约定（文档）：`"Use '-' as the source to read a tar archive from stdin"`，且「此时 `DEST_PATH` 必须指定一个目录」；出方向 `"streams the contents of the resource as a tar archive to STDOUT"`。

**路径安全**（源码：`github.com/moby/go-archive` `archive.go`，1408 行，master）

- 解包**整条链在 `os.Root` 里**：`func createTarFile(root *os.Root, dstPath string, hdr *tar.Header, reader io.Reader, opts *TarOptions) error`——`os.OpenRoot(dest)` 拿到 root，普通文件走 `root.OpenFile(dstPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC|…)`，注释写明 `"path resolution is bounded within root using openat(2) semantics"`；目录 `root.Mkdir`；硬链接 `root.Link`。
- 三条**拒绝式**校验：条目名 `if !filepath.IsLocal(name) { return breakoutError(...) }`；硬链接目标 `if cleaned == "." || !filepath.IsLocal(cleaned) { return …, breakoutError(...) }`（绝对目标按 chroot 语义转成 root 相对后再判）；解析后的父目录 `if relParent != "." && !filepath.IsLocal(relParent) { return …, breakoutError(...) }`。`type breakoutErr struct{ error }` 让它可被单独识别。
- 符号链接**按原样创建**：`root.Symlink(linkTarget, dstPath)`，注释说 `"containment applies when the symlink is followed, not at creation"`——即**允许树内链接、拒绝出树**。
- 一条**自承认的 TOCTOU**（源码注释，原文）：`resolveArchivePath` 是「a compatibility workaround rather than the preferred long-term implementation」，它「resolves the path separately before the actual operation, so a concurrent filesystem change may cause the operation to affect a different path within root」，但「The subsequent os.Root operation still confines the operation to root and prevents such a change from escaping it」。并写明将来要用 handle-relative + resolve-in-root 取代它。
- **历史**（文档：Docker Engine 19.03 release notes）：CVE-2018-15664 的修法原文是 `"Root is now passed to chroot for chroot Tar/Untar (CVE-2018-15664)"`（moby#39292，19.03.0）；19.03.0–19.03.4 的 Known Issues 里一直挂着 `"CVE-2018-15664 symlink-exchange attack with directory traversal"` 与 workaround `"docker pause container before doing file operations"`；同一处的已知问题还记着修法带来的回归——`"docker cp regression due to CVE mitigation. An error is produced when the source of docker cp is set to /"`。另一条：CVE-2019-14271 在 19.03.1 修（`"Fixed loading of nsswitch based config inside chroot under Glibc."`），19.03.8 再 `"Improve mitigation … for some nscd configuration."`。

**对我们的推论**

- **「tar 由可信侧打」有先例**：`docker cp` 证明了文件搬运**不需要**目标侧有 `tar`。我们的 `download` 是 Worker 在宿主侧打包/直传，因此天然绕开 `kubectl cp` 的全部前提（容器里得有 tar）与全部 CVE。
- **我们的落地规格可以比它严一档**，因为有 `os.Root` 这个现成的内核语义封装（Go 1.24+）。moby 是把它**当补丁**后来加的（还留着兼容垫片和 TOCTOU 注脚），我们是**从第一天就按它写**。
- 它承认不做 temp+rename（`O_TRUNC` 直接开写），所以「半截文件」在它的失败态里存在。我们的第三条规矩加原子 rename 是**净加密**，不是抄来的。

> 来源：<https://raw.githubusercontent.com/moby/moby/master/daemon/archive_unix.go>、<https://raw.githubusercontent.com/moby/moby/master/daemon/archive.go>、<https://raw.githubusercontent.com/docker/cli/master/cli/command/container/cp.go>、<https://raw.githubusercontent.com/moby/go-archive/main/archive.go>、<https://docs.docker.com/engine/release-notes/19.03/>。`docker cp` 的 CLI 文档（含 tar 流、符号链接、不建父目录、`/proc` 等系统文件不可复制）见 `vendor-file-apis.md` §1（本票未重复核对）。

---

## 2. `kubectl cp`：容器内 `tar` + exec，六轮 CVE 的活标本

**怎么做的**（文档：kubectl cp 参考页；源码：`kubernetes/kubectl` `pkg/cmd/cp/cp.go` 594 行 + `filespec.go`，master）

- 官方文档自己把机制写成了示例（原文）：
  ```
  # !!!Important Note!!!
  # Requires that the 'tar' binary is present in your container
  # image.  If 'tar' is not present, 'kubectl cp' will fail.
  #
  # For advanced use cases, such as symlinks, wildcard expansion or
  # file mode preservation, consider using 'kubectl exec'.
  tar cf - /tmp/foo | kubectl exec -i -n <some-namespace> <some-pod> -- tar xf - -C /tmp/bar
  ```
  `--retries` 默认 `0`（`"Specify 0 to disable or any negative value for infinite retrying."`）。
- 出容器（源码）：`copyFromPod` → `newTarPipe`；重试时命令变成 `{"sh", "-c", "tar cf - '<escaped>' | tail -c+<n>"}`（单引号已转义），靠 `tail -c+` 从上次读到的字节继续。
- 入容器（源码）：`copyToPod` 先 `checkDestinationIsDir`（在容器里跑 `test -d`，30 秒超时），不是目录则 `tar -xmf -`，`--no-preserve` 时改成 `tar --no-same-permissions --no-same-owner -xmf -`。
- 本地→容器方向由客户端自己打 tar（`makeTar`/`recursiveTar`），符号链接按 `os.Readlink` 写成 `Symlink` 条目。
- 收方向落地（源码 `untarAll`）：**符号链接一律跳过**（`"warning: skipping symlink"`，并提示改用 `exec … tar cf - | tar xf -`），普通文件 `os.Create` + `io.Copy` 就地落地，`os.MkdirAll(…, 0755)` 建父目录。

**路径安全**（源码）

- 出方向先算 prefix：`prefix := stripPathShortcuts(srcFile.StripSlashes().Clean().String())`，注释说这是为了处理「path 里多余的 `../` 试图越过远端文件系统的 `/`」。`untarAll` 里 `if !strings.HasPrefix(header.Name, prefix) { return fmt.Errorf("tar contents corrupted") }`。
- 落地前再判一次：`if !isRelative(dest, destFileName) { … "warning: file %q is outside target destination, skipping" }`。`isRelative`（`filespec.go`）的实现是 **`strip-then-compare`**：`relative, err := filepath.Rel(base, target)`，然后 `return relative == "." || relative == stripPathShortcuts(relative)`——**把 `../` 剥掉再比，剥完变了就说明越界**。
- 这条防线被反复打穿，正是 `strip-then-compare` 的代价（六轮，见 §8）。

**对我们的推论**

- **`kubectl cp` 的最终答案是「移除符号链接支持」**（CVE-2019-11251 的修法，见 §8）——不是加第十层校验。这直接支持我们第二条死规矩：**「不跟随符号链接」不是保守，是先行形态交了四次学费之后的结论**。
- 它揭示了一个我们必须正视的事实：**下行 tar 的内容对我们也是不可信的**。我们的 `download` 是 Worker 从工作区树里打 tar，而那棵树是**沙箱里的代码能写的**——沙箱能造出 `src -> /etc` 这样的链接。所以「被搬运的 archive 由不受信方生成」这条对我们是成立的，落地侧的三条规矩必须照样管住**我们自己生成的** tar。
- 反面教训：「容器里必须有 tar」这一前提本身就是攻击面（容器可以给你一个恶意 `tar`，CVE-2026-19444 的官方描述原文就是 `"If the tar binary in the container is malicious, it could run any code and output unexpected, malicious results."`）。**我们的镜像不需要 tar，这条风险不存在。**

> 来源：<https://kubernetes.io/docs/reference/kubectl/generated/kubectl_cp/>、<https://raw.githubusercontent.com/kubernetes/kubectl/master/pkg/cmd/cp/cp.go>、<https://raw.githubusercontent.com/kubernetes/kubectl/master/pkg/cmd/cp/filespec.go>、<https://github.com/kubernetes/kubernetes/issues/141294>、<https://kubernetes.io/docs/reference/issues-security/official-cve-feed/>（官方 CVE feed，本票由它取得 2018–2026 各条 kubectl cp 的官方叙述与修复版本）。

---

## 3. VS Code Remote：URI 寻址的 `FileSystemProvider`

**怎么做的**（文档：`code.visualstudio.com/api/references/vscode-api` 的 `FileSystemProvider` 一节）

- 接口成员（全部列出）：`stat(uri) → FileStat`、`readDirectory(uri) → [string, FileType][]`、`createDirectory(uri)`、`readFile(uri) → Uint8Array`、`writeFile(uri, content, {create, overwrite})`、`delete(uri, {recursive})`、`rename(oldUri, newUri, {overwrite})`、`copy(source, destination, {overwrite})`、`watch(uri, {excludes, recursive})`、`onDidChangeFile`。
- **寻址是 URI，不是宿主路径**：原文 `"work is done with uris and assumes hierarchical paths"`（`foo:/my/path` 是 `foo:/my/` 的子项）；有 `onFileSystem:<scheme>` 激活事件。
- 符号链接的表达是**两个维度**：`FileType.SymbolicLink: 64` 与 `File: 1` / `Directory: 2` **可叠加**，`stat` 对符号链接返回**referent 的元数据**。
- 错误面是一组**必须使用**的码：`FileSystemError.FileNotFound` / `.FileExists` / `.NoPermissions`。`createDirectory` 明确 **"no mkdirp-logic required"**——父目录不存在就抛 `FileNotFound`。`copy` 是唯一明说 optional 的成员（`"Implementing this function is optional but it will speedup the copy operation."`）。
- 全 `writeFile` 语义是**整文件替换**（不是追加），并有 `create`/`overwrite` 两个显式开关。

**路径安全**

- 文档里**没有任何**路径穿越、符号链接逃逸、路径规范化的说明——校验责任落在 provider 实现者身上（与 Cloudflare 的「Files does not restrict which paths it opens」同构，见 `vendor-file-apis.md` §2.6）。
- VS Code **remote extension host 的线上协议没有公开规范**，只能核对扩展 API 这一层。**[未证实]**

**对我们的推论**

- **可抄的是错误面，不是寻址**：我们 `ls`/`download`/`upload` 的对外错误应当把「路径不存在」「父目录不存在」「已存在」分成三个，而不是合成一个 `invalid_argument`（我们的 ADR-0008 现在只把签发阶段失败合成 `invalid_argument`，逐路径结果里有 `code` 却没定死取值——这是 `#17` 该收的口）。
- `createDirectory` 拒绝 mkdirp 的取向与我们**相反**，且是有意的：我们 `upload` 落地是自动 `mkdir -p`（ADR-0008），因为树里没有 `mkdir` 这个动词，调用方不该被迫先提一个 Task 去建目录。差别要记明，别被 VS Code 的取向带偏。
- 符号链接「类型」与「本体」分两个维度表达、`stat` 返回 referent 元数据——对我们 `ls` 输出是个提示（树里可能有链接，调用方需要能看出来）。

> 来源：<https://code.visualstudio.com/api/references/vscode-api>（`FileSystemProvider` 一节）。

---

## 4. containerd / CRI：运行时**没有**文件面

**怎么做的**（源码：`kubernetes/cri-api` `pkg/apis/runtime/v1/api.proto` 2398 行；`containerd/containerd` `pkg/archive/tar.go` 830 行）

- **CRI 一个文件 RPC 都没有。** 逐条读完 `service RuntimeService` 的 37 个 RPC（Version / RunPodSandbox / … / ExecSync / Exec / Attach / PortForward / ContainerStats / UpdateRuntimeConfig / Status / CheckpointContainer / GetContainerEvents / ListMetricDescriptors / …）与 `service ImageService` 的 6 个（ListImages / StreamImages / ImageStatus / PullImage / RemoveImage / ImageFsInfo）：**没有任何读写列删复制容器文件的 RPC**。最接近的是 `ExecSync` / `Exec` / `Attach` / `PortForward`，以及只管日志的 `ReopenContainerLog`、管检查点归档导出的 `CheckpointContainer`（`Location of the checkpoint archive used for export`）。
- 结论：**容器文件搬运在 CRI 世界里不是运行时接口**，`kubectl cp` 是上层用 `exec` 自己造的。containerd 提供的是 **toolkit**：`pkg/archive` 的 `Apply`（把 OCI 风格 diff tar 落盘）、`Diff` / `WriteDiff` / `ChangeWriter`（反向生成 tar，删除用 `.wh.` 白障，`"This style is based off AUFS whiteouts"`），加上 `WithNoSameOwner`、`WithFilter`、`WithParents`、`WithSourceDateEpoch` 这类选项。
- `applyNaive`（源码）的落地顺序：`hdr.Name = filepath.Clean(hdr.Name)`（注释 `"// Normalize name, for safety and for a simple is-root check"`）→ `accept, err := options.Filter(hdr)` → `skipFile(hdr)` → `ppath, base := filepath.Split(hdr.Name)` → `ppath, err = fs.RootPath(root, ppath)`（来自 `github.com/containerd/continuity/fs`）→ `path := filepath.Join(ppath, filepath.Join("/", base))`，`if path == root` 就忽略该条目 → `mkparent` 建父目录。

**路径安全**（源码）

- 走的是**字符串前缀 + securejoin** 路线：`fs.RootPath` 把父目录解析回 root 内，`validateWhiteout` 用 `if !strings.HasPrefix(originalPath, dir)` 兜白障（返回 `invalid archive`）。
- `hardlinkRootPath` 注释写着 `"returns target linkname, evaluating and bounding any symlink to the parent directory"`；但**越界时它悄悄退化成 root**：`if !strings.HasPrefix(targetPath, root) { targetPath = root }`——**失败被吞掉，不是报错**。
- 符号链接直接 `os.Symlink(hdr.Linkname, path)`；设备/FIFO 交给 OS-specific 的 `handleTarTypeBlockCharFifo`；普通文件 `openFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, hdrInfo.Mode())` 后 `copyBuffered`。
- **没有 temp+rename**（源码确认）。

**对我们的推论**

- 「CRI 没有文件面」= **我们的文件面没有可对齐的运行时接口**，必须自己在 Worker 侧定义——与 ADR-0006 已定的形状一致，此处得到实证。
- 两个**反例**要避开：① `hardlinkRootPath` 越界时**静默 clamp**，这是「绝不静默降级」的反面（我们的规矩是拒绝 + 大声失败）；② `strings.HasPrefix` 前缀比较是字符串判断，正是 §8 共同根因的家族。**我们不要这一代的做法，要 moby 那一代（`os.Root`）。**
- 代价记一笔：`os.Root` 的方法集里**没有 `CreateTemp`**（逐条列出的方法只有 Chmod/Chown/Chtimes/Close/Create/FS/Lchown/Link/Lstat/Mkdir/MkdirAll/Name/Open/OpenFile/OpenRoot/ReadFile/Readlink/Remove/RemoveAll/Rename/Stat/Symlink/WriteFile）。原子落地要自己编临时名（见 §9）。

> 来源：<https://raw.githubusercontent.com/kubernetes/cri-api/master/pkg/apis/runtime/v1/api.proto>、<https://pkg.go.dev/k8s.io/cri-api/pkg/apis/runtime/v1>、<https://raw.githubusercontent.com/containerd/containerd/main/pkg/archive/tar.go>、<https://pkg.go.dev/github.com/containerd/containerd/v2/pkg/archive>、<https://pkg.go.dev/os>。

---

## 5. `rsync` 与 `tar`：路径语义与「拒绝 vs 剥离」

**`rsync` 怎么做的**（文档：`man7.org/linux/man-pages/man1/rsync.1.html`）

- `--safe-links`：**接收端**忽略「指向被复制树之外」的所有符号链接，`"All absolute symlinks are also ignored."`；必须与 `--links`/`--archive` 同用；因为过滤发生在接收端，发送端 munge 过也照样生效；且「被跳过的文件仍会阻止 receiver 的对应删除」。
- `--munge-links`：只作用于一侧，把每个链接值前缀成 `"/rsyncd-munged/"`，使链接「磁盘上不可用但原值可复原」；副作用：munge 过的接收端再配 `--safe-links`，**所有链接都会被丢掉**（因为看起来都是绝对路径）。
- `--copy-unsafe-links`：反过来——把指向树外的链接**复制其引用目标**。
- `--keep-dirlinks`：把「指向目录的链接」当目录用；文档自带警告：`"you must trust all the symlinks in the copy or enable the --munge-links option on the receiving side"`，否则「能自己造链接的用户」可以置换目录来影响被指向目录的内容；**备份场景建议用 bind mount 代替**。
- 多主机安全（原文）：`"From 3.1.2 onward it has aborted when a file list contains an absolute or relative path that tries to escape out of the top of the transfer."`（**中止**，不是剥离）；3.2.5 再加两条（源参数不得超出客户端请求、文件表必须服从 exclude）。对旧客户端，`"safest to do a copy into a dedicated destination directory for the remote files when you don't trust the remote host"`。
- 增量与断点：delta-transfer（只发差异）；`--checksum` 用 128 位校验代替 size/mtime 快判（`"this can slow things down significantly"`）；**任何情况下**`"rsync always verifies that each transferred file was correctly reconstructed"`（传输时算的全文件校验和）。`--partial` 保留半截文件；`--append`「已知变大」才用，文档自己说 `"can be dangerous"`（**不检查已有内容**），`--append-verify` 把已有数据一并纳入校验和。

**`tar` 怎么做的**（文档：`man7.org/linux/man-pages/man1/tar.1.html`；一手咨询：CVE-2016-6321）

- `-P/--absolute-names` 的原文是 `"Don't strip leading slashes from file names when creating archives."`——**注意它说的是创建**，不是提取时的保护开关。
- 提取侧相关：`--keep-old-files`（`"Don't replace existing files when extracting."`）、`--overwrite`、`--no-same-owner`（`"Extract files as yourself (default for ordinary users)."`，root 默认 `--same-owner`）、`--no-same-permissions`（`"Apply the user's umask when extracting permissions"`，普通用户默认）。
- **CVE-2016-6321 的一手咨询**（sintonen.fi，[`tar-extract-pathname-bypass.txt`](https://sintonen.fi/advisories/tar-extract-pathname-bypass.txt)）：受影响 `"GNU tar 1.14 to 1.29 (inclusive)"`；根因是 `safer_name_suffix()`——`"Making the name safe involves stripping all offending path components"`，于是**它把恶意条目改写而不是拒绝**，`"the path name effectively becomes relative to the target directory"`，攻击者因此能覆盖命令行指定路径之外的文件（最严重可达 `"a full system compromise (remote code execution as root)"`）。修法原文：`"Skip entries with member names containing a '..', or fail the whole tar extract operation."`

**对我们的推论**

- **「剥离/规范化」是本票要根除的模式**：GNU tar 的 `safer_name_suffix`、node-tar 的 single-root-strip、`kubectl cp` 的 `stripPathShortcuts`——三个独立实现的同一个错。**我们的规矩「路径规范化且落在树内」如果实现成「Clean 之后看前缀」，就是同一个错误。**
- 应改成：**`filepath.IsLocal` 拒绝 + 内核侧兜底**。`filepath.IsLocal`（Go 1.20）文档原文列了它判的四件事（在子树内 / 非绝对 / 非空 / Windows 非保留名），并明说 `"IsLocal is a purely lexical operation."`、`"In particular, it does not account for the effect of any symbolic links that may exist in the filesystem."`——**所以词法判定必须再叠一层内核判定**（`os.Root` / `openat2(RESOLVE_BENEATH)`）。
- rsync 的两条可直接照抄：①**跳过不可信的链接**（而不是把它解开成目标），并且**让它的存在阻止删除**——等价到我们：`download` 打包时遇到非普通文件要么失败要么明确跳过，且绝不为它造出任何东西；②**`--append` 与 `--append-verify` 的差别就是「断点续传必须把已有数据纳入校验」**——我们的 upload 完成调用要求调用方报 `size` 并逐条核对，正是这个思想（ADR-0008），此处得到独立印证。

> 来源：<https://man7.org/linux/man-pages/man1/rsync.1.html>、<https://man7.org/linux/man-pages/man1/tar.1.html>、<https://sintonen.fi/advisories/tar-extract-pathname-bypass.txt>。另：`docker cp` 与 `podman cp` 的符号链接默认行为**相反**（docker 默认复制链接本身、`-L` 才跟随；podman 默认复制**目标**），见 <https://docs.podman.io/en/latest/markdown/podman-cp.1.html> 与 `vendor-file-apis.md` §1。

---

## 6. 对象存储方向：S3 预签名的硬语义与「整目录」的几种形状

**预签名 URL 的官方语义**（文档：AWS S3 User Guide，`using-presigned-url.html`）

- **可重复使用**，原文 `"You can use the presigned URL multiple times, up to the expiration date and time."`
- **无法撤销**：没有撤销 API，只有靠凭据失效。原文 `"In general, a presigned URL expires when the credential you used to create it is revoked, deleted, or deactivated. This is true even if the URL was created with a later expiration time."`，且 `"If you created a presigned URL by using a temporary token, then the URL expires when the token expires."`
- 寿命取值：控制台 1 分钟–12 小时，CLI/SDK 最高 7 天；**临时凭据签的活不过会话**（AssumeRole 默认 1 小时，EC2 实例配置约 6 小时）。
- 检查时机：S3 在**收到请求那一刻**判过期——`"if a client begins to download a large file immediately before the expiration time, the download continues even if the expiration time passes during the download."` 但断线重连就失败。
- 它是 **bearer token**：`"In essence, presigned URLs are bearer tokens that grant access to those who possess them."`；可用 bucket policy 条件进一步收紧，例如 `s3:signatureAge`（文档给了 10 分钟的示例）。
- **上传完整性**：预签名 PUT 可带 checksum；`"presigned URLs created with AWS Signature Version 2 only support MD5 checksums"`，SigV4 支持 `CRC-64/NVME, CRC32, CRC32C, SHA-1, SHA-256, MD5, XXHash64, XXHash3, XXHash128, SHA-512`，做法是「把相应的 checksum 头放进上传请求」。`Content-MD5` 是 legacy：`"The legacy Content-MD5 header remains available for single part uploads using SSE-S3 encryption."`（`checking-object-integrity.html`）；S3 会在服务端独立算一遍再比，不匹配则拒绝。

**体积与 multipart**（文档：`upload-objects.html`、`mpuoverview.html`、`mpu-abort-incomplete-mpu-lifecycle-config.html`）

- **单次 PUT 上限 5 GB**（`"With a single PUT operation, you can upload a single object up to 5 GB in size."`）；multipart 用于 5 MB–50 TB；控制台单文件上限 160 GB。
- **对象直到 `CompleteMultipartUpload` 才存在**：`"Upon receiving the complete multipart upload request, Amazon S3 constructs the object from the uploaded parts"`。分片可以乱序上传（`"in any order, and in parallel"`），且**同名并发**不是先完成者胜——versioning 桶里 `"the current version of the object is determined by which upload started most recently (createdDate)"`。
- **必须显式 complete 或 abort**：`"After you initiate a multipart upload and upload one or more parts, you must either complete or stop the multipart upload to stop incurring charges"`；未完成的分片**一直计费**；`"After stopping a multipart upload, you can't upload any part using that upload ID again"`，且要等所有 part 上传结束才能安全 abort。**唯一的内建清扫是桶生命周期的 `AbortIncompleteMultipartUpload`，粒度是「天」**（示例 7 天）。

**「大目录搬运」的几种形状**（文档：OCI distribution spec；AWS CLI `s3 sync`；厂商面见 `vendor-file-apis.md`）

- **逐对象 + 客户端并发**：Buildkite `artifact_paths`、GitHub Actions v4 直传签名 URL（`vendor-file-apis.md` §4）。
- **整树打成一个 tar 再塞一个对象**：Blaxel `archive/export`（不压缩、直接流到预签名 S3 PUT）、`docker save`、OCI layer。
- **注册表式 blob 上传**（OCI distribution spec，`raw.githubusercontent.com/opencontainers/distribution-spec/main/spec.md`）：`POST` 开 session 拿 `Location`（`202 Accepted`），`PATCH` 分块（`"The first chunk's range MUST begin with 0."`，`"Chunks MUST be uploaded in order"`，乱序回 `416`），`PUT ?digest=<整块 digest>` 关闭（`201 Created`）；`GET` session 返 `204 No Content` + `Range` 可用于**断点续传**；取消是客户端 `DELETE`（`"Clients SHOULD send this request when aborting a blob upload, releasing server resources."`，失败可忽略）。**服务端只有一句回收承诺**：`"If this request fails or is not called, the server SHOULD eventually timeout unfinished uploads."`
- **增量同步**（AWS CLI `s3 sync`）：判据是三条之一——大小不同 / 源比目标新 / 目标不存在（原文三处分别给出）；`--size-only` 只比大小，`--exact-timestamps` 要求时间戳精确相同，`--delete` 删目标多余项。**文档没有一句「不比较内容」**，但其判据（size + mtime）**推断**即不保证内容一致。

**对我们的推论**

- ADR-0007 / ADR-0008 的两条地基被一手原文撑住：**预签名 URL 无法撤销**（⇒「按搬运完成回收」机制上不可能，只能时间驱动）；**下载已开始的传输不受过期影响**（⇒ URL 寿命不是下载方向的传输时间上界，只有 upload 那次 PUT 是）。
- 一条新增的具名代价：**multipart 一上，清理责任全归我们**——对象不是原子出现、未完成分片不会被普通过期规则清掉，且桶生命周期的粒度是**天**（与 ADR-0007 否决 lifecycle 的理由是同一个）。ADR-0007 §七 已写这条，此处补一手原文作依据。
- `s3 sync` 的判据是**反面教材**：size+mtime 明确不保证内容一致。我们的判据不能退化成 mtime——`size` 断言 + 逐条结果（ADR-0008）才是「半截文件不能当成功」的兑现。

> 来源：<https://docs.aws.amazon.com/AmazonS3/latest/userguide/using-presigned-url.html>、<https://docs.aws.amazon.com/AmazonS3/latest/userguide/upload-objects.html>、<https://docs.aws.amazon.com/AmazonS3/latest/userguide/mpuoverview.html>、<https://docs.aws.amazon.com/AmazonS3/latest/userguide/mpu-abort-incomplete-mpu-lifecycle-config.html>、<https://docs.aws.amazon.com/AmazonS3/latest/userguide/checking-object-integrity.html>、<https://docs.aws.amazon.com/cli/latest/reference/s3/sync.html>、<https://raw.githubusercontent.com/opencontainers/distribution-spec/main/spec.md>。

---

## 7. 机制 × 安全属性对照表

| 机制 | 谁打 tar / 谁搬字节 | 路径校验在哪一侧、怎么判 | 符号链接 | 硬链接 / 设备 / FIFO | 原子落地 | 需要容器里有 tar |
|---|---|---|---|---|---|---|
| **docker cp** | **宿主 daemon** 打 tar | daemon，`os.Root` + `filepath.IsLocal`（**拒绝**） | 对**源**默认不跟随（`-L` 才跟随）；解包时按原样创建、仅在跟随出树时拒绝 | 硬链接目标 `IsLocal` 校验；设备/FIFO 由 `handleTarTypeBlockCharFifo` 创建 | ❌ `O_TRUNC` 就地写 | ❌ 不需要 |
| **kubectl cp** | **容器内 tar** + exec | 客户端，`stripPathShortcuts` + `isRelative`（**剥离后比较**，六轮 CVE） | **一律跳过**（v1.16 起连支持都移除） | 无特殊处理（普通文件走 `os.Create`） | ❌ `os.Create` 就地写 | ✅ **必须** |
| **containerd `pkg/archive`** | 调用方给 tar 流 | `filepath.Clean` + `fs.RootPath`（securejoin）+ `strings.HasPrefix`（**前缀比较**，越界时静默 clamp 到 root） | `os.Symlink(hdr.Linkname, path)` 原样创建 | 设备/FIFO OS-specific 创建 | ❌ `O_TRUNC` 就地写 | — （toolkit，不是接口） |
| **CRI** | — | **没有文件面**（37+6 个 RPC 中零个文件操作） | — | — | — | — |
| **VS Code `FileSystemProvider`** | 扩展实现者 | 文档未涉及；责任在 provider | `FileType.SymbolicLink` 与 `File`/`Directory` 可叠加，`stat` 返回 referent 元数据 | 未涉及 | 未涉及（`writeFile` 是整文件替换） | — |
| **rsync** | rsync 自己 | 接收端；3.1.2 起**遇逃逸即中止** | `--safe-links` 丢弃出树链接；`--munge-links` 使其不可用；`--keep-dirlinks` 文档自带「你必须信任所有链接」警告 | 未涉及 | `--partial` 保留半截；`--append-verify` 把已有数据纳入校验 | — |
| **GNU tar** | tar | 提取时**剥离** `..`（CVE-2016-6321） | 未内建保护 | 未内建保护 | ❌ 就地写 | — |
| **S3 预签名** | 调用方 HTTP | 不适用（键由我们铸，不透明） | 不适用 | 不适用 | ✅ **单键 PUT 原子**；multipart 不是原子 | — |
| **OCI blob 上传** | 客户端 | 不适用 | 不适用 | 不适用 | ✅ 只有 complete 才成 blob；`PUT ?digest=` 时校验整块 digest | — |

---

## 8. CVE 清单与共同根因

| CVE | 机制 | 一句话根因 |
|---|---|---|
| [CVE-2018-1002100](https://github.com/kubernetes/kubernetes/issues/61297) | kubectl cp | 容器返回的 tar 里写 `/some/remote/dir/../../../../tmp/foo`，被原样落到 `/tmp/foo`（目标目录之外） |
| CVE-2019-1002101 | kubectl cp | 同族；接下去三次修复都不完整 |
| CVE-2019-11246 | kubectl cp | 又一次不完整修复 |
| [CVE-2019-11249](https://github.com/kubernetes/kubernetes/issues/80984) | kubectl cp | `"a third issue … that could enable a directory traversal such that a malicious container could replace or create files on a user's workstation"`；客户端侧缺陷、需用户交互 |
| [CVE-2019-11251](https://github.com/kubernetes/kubernetes/issues/87773) | kubectl cp | `"a combination of two symlinks"` 把文件复制到目标目录之外；**修法：v1.16.0 起移除 `kubectl cp` 的符号链接支持**，建议改用 `exec`+`tar`；1.15.4 的兼容修法是「把符号链接推迟到所有普通文件解包之后再解包」 |
| [CVE-2026-19444](https://github.com/kubernetes/kubernetes/issues/141294) | kubectl cp（Windows 客户端） | `"A path traversal vulnerability exists in the kubectl cp command when it is run on Windows."`；影响 v1.34.0–1.34.11 / 1.35.0–1.35.8 / 1.36.0–1.36.4，修于 ≥1.34.12 / 1.35.9 / 1.36.5 |
| CVE-2018-15664 | docker cp | **TOCTOU**：`symlink-exchange attack with directory traversal`——解包不在冻结的文件系统/chroot 内做，用户态解析出的路径在使用前被换掉；修法 `"Root is now passed to chroot for chroot Tar/Untar"`，过渡期 workaround 是 `docker pause` |
| CVE-2019-14271 | docker cp | chroot 内加载 `nsswitch` 配置（特权进程因此加载宿主侧库）；19.03.8 又 `"Improve mitigation … for some nscd configuration."` |
| [CVE-2016-6321](https://sintonen.fi/advisories/tar-extract-pathname-bypass.txt) | GNU tar 1.14–1.29 | `safer_name_suffix()` **剥离**越界分量而不是拒绝，`..` 因此逃出命令行指定的路径；修法是「跳过含 `..` 的成员或让整个解包失败」 |
| [CVE-2021-32804](https://github.com/advisories/GHSA-3jfq-g458-7qm9)（同族 CVE-2021-32803） | node-tar | 只**剥一层** path root：`////home/user/.bashrc` 剥完仍是绝对路径，于是 `"allowing arbitrary file creation and overwrite"` |

**共同根因（三条，全部有上表支撑）**

1. **用户态字符串规范化**：把安全实现成「改写输入」。改写规则永远不全——剥一层 vs 剥全部、前导斜杠、`..` 折叠、Windows 分隔符与保留名、大小写。tar / node-tar / kubectl cp 三处是同一个错误的三个化身。
2. **check-then-use（TOCTOU）**：判定与使用之间路径可以被改掉。CVE-2018-15664 是教科书案例；moby 今天的 `resolveArchivePath` 注释里还留着这条自承认（只是它用 `os.Root` 把后果限制在 root 内）。
3. **信任被搬运的 archive**：tar 条目名与链接目标是**被搬运方写的数据**，不是路径。`kubectl cp` 从 2018 到 2026 的全部 CVE 都源于此，而它的最终答案是**放弃符号链接**。

---

## 9. 我们的三条死规矩够不够

| 死规矩 | 够不够 | 依据与要补的话 |
|---|---|---|
| ① **路径规范化且落在树内** | ⚠️ **不够，取决于实现** | 若写成「`filepath.Clean` 之后看前缀」= 复刻 CVE-2016-6321 / CVE-2021-32804 / kubectl cp 六轮。**必须写成「`filepath.IsLocal` 拒绝 + 内核侧兜底」**：`filepath.IsLocal`（Go 1.20）是纯词法、明说不处理符号链接；`os.Root`（Go 1.24）或 `openat2(RESOLVE_BENEATH)` 用 `openat(2)` 语义兜住词法判不到的部分。规格里应写明「本系统在任何一处都不使用 strip-then-compare」 |
| ② **不跟随符号链接** | ✅ **够，且被两个先例加固** | kubectl cp 的终局修法是**移除符号链接支持**（CVE-2019-11251）；rsync `--safe-links` 在接收端**丢弃**出树链接。要补：`os.Root` **允许**树内符号链接、只拒绝出树（moby 注释：`"containment applies when the symlink is followed, not at creation"`），所以「不跟随」必须显式落实（逐段 `O_NOFOLLOW` / `RESOLVE_NO_SYMLINKS` / `os.Root` 的 openat 语义），并且**写方向**不能在树里造出链接 |
| ③ **只处理普通文件 + 体积上界** | ✅ **够，而且比库给的保证强** | `os.Root` 文档明说它**不禁止**「traversal of filesystem boundaries, Linux bind mounts, /proc special files, or access to Unix device files」；`open(2)` 的 `O_NOFOLLOW` **只作用于最后一段**（前导分量照样跟随）。所以「普通文件」必须自己判：`Lstat` 确认类型 → `open` → **`fstat` 复查 inode 一致**（否则检查与打开的可能是两个 inode，回到 TOCTOU） |
| ④ **（本票新增）同目录原子 rename** | ❌ **现在没人在 ADR 里认领** | `rename` 的原子性**只在同一挂载点内**成立；`os.Root` 方法集里**没有 `CreateTemp`**（已逐条核对），临时名要自己编；跨挂载点会 `EXDEV`，而先行形态明确**不 fallback 到复制+删除**（Cloudflare：`"The method does not fall back to copying and deleting."`）。这条要写进规格：临时文件必须与目标同目录（同 bind mount） |
| ⑤ **（本票新增）临时物与残留的清理责任** | ❌ **同上** | 所有被读到的实现都是就地 `O_TRUNC`，所以「半截文件」是它们的已知失败态（Cloudflare 原文：`"A later failure can leave the file created, truncated, or partially written."`）。我们做原子落地是**加密**，但换来一条新责任：**进程崩溃留下的临时名由谁收**。中转区有保留期扫帚（ADR-0007），树里的临时文件没有——这是本票挖出的一个缺口 |

**额外一条加固建议（成本几乎为零）**：`archive/tar` 的 `Reader.Next` 在 `GODEBUG=tarinsecurepath=0` 时会对非本地的条目名返回 `ErrInsecurePath`，且**附带 `"Only file names are validated, not link targets."`**（文档）。它的默认值将来可能翻转（Go 1.20 release notes 原文：`"A future version of Go may disable insecure paths by default."`）。这条**不能当防线**（它默认关着，且不管链接目标），但**可以当第二道探针**：Worker 解包时显式开这个 GODEBUG，任何非本地条目名都会在 `Next` 处先冒出来，日志里留下证据。同理 `archive/zip` 有 `zipinsecurepath`。

---

## 10. 未能证实 / 未覆盖

- **GNU tar manual 的对应节点**（absolute names / 提取成员过滤的正文）本次被 `gnu.org` 连续 **429 rate-limited**，未能打开。§5 里 tar 的结论全部来自 `man7.org` 的 `tar(1)` man page（已打开）与 CVE-2016-6321 的一手咨询（已打开）。**「GNU tar 手册里关于不可信档案的专门章节」[未证实]。**
- **VS Code remote extension host 的线上协议没有公开规范**，只能核对扩展 API 那一层（`FileSystemProvider`，已打开）。**协议层面的路径处理、目录表达 [未证实]。**
- **`os.Root` 在 pkg.go.dev 上的完整页被截断**：`ErrClosed` 的原话、「建议用于不可信路径分量」的原话**未取到**。本票引用的 os.Root 版本（Go 1.24）与「不允许引用目录外位置（含跟随符号链接出目录）」取自 `go.dev/doc/go1.24`（已打开）；方法集与平台限制取自 `pkg.go.dev/os`（已打开，部分截断）。
- **containerd 自己的 CRI 插件是否有私有文件扩展未查**；本票核对的是 CRI proto 的服务定义与 containerd `pkg/archive` 的公开 API。「CRI 没有文件面」这句限定在 **CRI v1 API** 上。
- **Blaxel 的 `abortUrl` / GitLab Workhorse 的 `AbortURL`·`DeleteURL`**：引自仓库内 `vendor-file-apis.md`，本票**未重新打开**一手页面。本票新增的同类一手证据是 AWS 的 `AbortIncompleteMultipartUpload` 与 OCI distribution spec 的 session `DELETE`。
- **本票零实测**：没有运行过 `docker cp`、`kubectl cp`、`openat2`、`os.Root` 的任何实验；所有「源码读出的」结论来自阅读官方仓库源码，未经运行验证。
- **大目录搬运的性能**（tar 包 vs 逐对象 vs 挂载的吞吐、小文件代价、并发度）本票未调研。
- **`docker cp` 的 `-L` 究竟在 CLI 还是 daemon 落实**：源码读出 CLI 侧解析源（daemon 的 `containerArchivePath` 签名里没有 `followLink`），但**未追完整调用链**，标为「源码读出的、未逐层验证」。
