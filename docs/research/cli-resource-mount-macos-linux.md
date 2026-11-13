# `orrery resource mount`：在 Linux 和 macOS 上用同一套实现挂载 Resource

调研日期 **2026-11-13**。问题：CLI 要提供 `orrery resource mount <name>[@version] <dir>`，在开发者机器上把一个不可变、只读的 ResourceVersion 挂成本地目录，读时才经预签名 URL 从 S3 取 4 MiB 内容寻址块（布局见 ADR-0030）。用户要求 Linux 和 macOS 的行为尽量一致。CLI 是静态 Go 二进制（`CGO_ENABLED=0`），发布 linux/darwin × amd64/arm64 四个包（ADR-0023，`.github/workflows/ci.yml` 第 230 行）。服务端的 `orrery-resourcefs` 在 Linux 上用 `github.com/hanwen/go-fuse/v2`（ADR-0030，`resource-lazy-read-spike.md`），这里只作背景。

要回答的是：哪种挂载机制能在两个系统上只写一份（或几乎一份）实现，以及每种机制让用户付出什么。

只用一手来源：官方文档、源码、release notes、许可证。英文原文照引，每条结论附链接。没能核实的标「未确认」。本次读的版本：macFUSE 5.5.0（2026-10-17）、FUSE-T 1.2.9（2026-10-06）、go-fuse `master@6018575`（2026-11-11，最新 tag v2.12.1 是 2026-10-21）、cgofuse `master`（最新 release v1.6.0）、go-nfs v0.0.4、rclone v1.76.0 的 `master`、Apple `NFS-343.100.5` 与 `xnu-12377.121.6`、Linux `v7.4-rc2`。**本文没有在 Mac 上实测任何一条**，凡是「能不能挂上、要不要 sudo」都是读文档和源码得出的。

---

## 0. 先读这几条

1. **Linux 上非 root 挂 NFS 不行，FUSE 可以。** mount(2)："Appropriate privilege (Linux: the CAP_SYS_ADMIN capability) is required to mount filesystems"（[mount(2)](https://man7.org/linux/man-pages/man2/mount.2.html)）；mount(8) 的绕法是 root 预先在 fstab 写 `user` 行（"Normally, only the superuser can mount filesystems. However, when fstab contains the user option on a line, anybody can mount the corresponding filesystem"，[mount(8)](https://man7.org/linux/man-pages/man8/mount.8.html)）；内核里 `nfs`/`nfs4` 也没有 `FS_USERNS_MOUNT`（[fs/nfs/fs_context.c](https://github.com/torvalds/linux/blob/v7.4-rc2/fs/nfs/fs_context.c)），而 `fuse` 有（[fs/fuse/inode.c](https://github.com/torvalds/linux/blob/v7.4-rc2/fs/fuse/inode.c)）。FUSE 的设计目标之一就是 "secure, non-privileged mounts"，经 setuid 的 `fusermount`（[fuse.rst](https://github.com/torvalds/linux/blob/v7.4-rc2/Documentation/filesystems/fuse/fuse.rst)）。所以 **NFS 回环做不了两边通用的唯一机制**。
2. **go-fuse 已经能在 macOS 上走 macFUSE 的 FSKit 后端，而且不需要 cgo。** go-fuse `master` 新增了 `fskit` 包（2026-10-31 提交，**还没进任何 tag**），用 `purego.Dlopen` 加载 macFUSE 5.3.0+ 的 `MFMount.framework`，接收的仍是 `fuse.RawFileSystem`（[fskit/fskit_darwin.go](https://github.com/hanwen/go-fuse/blob/6018575d027b3cedc12d34e8e1ad7e5a50c0a6f5/fskit/fskit_darwin.go#L214)）。purego 在 macOS amd64/arm64 上是 Tier 1，不需要 `CGO_ENABLED=1`（[purego README](https://github.com/ebitengine/purego#supported-platforms)）。go-fuse 走 kext 的老路径也只是 exec `mount_macfuse`，同样没有 cgo（[fuse/mount_darwin.go](https://github.com/hanwen/go-fuse/blob/6018575d027b3cedc12d34e8e1ad7e5a50c0a6f5/fuse/mount_darwin.go#L101)）。也就是说，**一份 `RawFileSystem`、`CGO_ENABLED=0`、Linux 和 macOS 都能挂**，差别只在挂载调用那一行。
3. **macOS 上没有免安装的 FUSE。** 要么装 macFUSE，要么装 FUSE-T，两者都是闭源二进制，商用都要另谈授权。macFUSE 许可证第 4 条："Redistributions in binary form, bundled with commercial software, are not allowed without specific prior written permission. This includes the automated download or installation or both of the binary form in the context of commercial software"（[LICENSE.txt](https://github.com/macfuse/macfuse/blob/release/macfuse/LICENSE.txt)）。FUSE-T 只提供 libfuse 层（`libfuse-t.dylib`），go-fuse 用不上；cgofuse 能用，但要 cgo。macOS 上**唯一免安装**的是系统自带的 NFS 客户端，配一个纯 Go 的 NFSv3 服务（rclone `nfsmount` 就是这么做的）。

**推荐**：**两个系统都用 go-fuse，同一个 `RawFileSystem`**（与 `orrery-resourcefs` 共用那份按 manifest 生成节点 ID 的实现）。Linux 经 `fusermount3` 非 root 挂载；macOS 要求用户自己装 macFUSE，优先 FSKit 后端（不用重启、不用降低安全级别），kext 后端兜底。macOS 的 NFS 回环先不做，列为「用户不肯装 macFUSE」时的第二前端。详见 §4。

---

## 1. 逐个机制

### 1.1 macFUSE

- **版本**：最新 5.5.0，2026-10-17（[release](https://github.com/macfuse/macfuse/releases/tag/macfuse-5.5.0)）。macFUSE 5 "supports macOS 11 and later versions of macOS"（[5.0.0 release](https://github.com/macfuse/macfuse/releases/tag/macfuse-5.0.0)）。
- **两个后端**："By default, macFUSE uses the VFS API to mount file systems. When specifying the mount-time option `-o backend=fskit`, macFUSE will use FSKit to mount the file system." VFS 后端 "requires shipping a kernel extension"；FSKit "is a user-space API and has been introduced in macOS 15.4"（[FUSE Backends](https://github.com/macfuse/macfuse/wiki/FUSE-Backends)）。macFUSE 说 FSKit 后端 "will eventually supersede the kernel backend"（[MFMount.framework](https://github.com/macfuse/macfuse/wiki/Getting-Started-(Developer)-%E2%80%90-MFMount.framework)）。
- **FSKit 后端的限制**（[FUSE Backends](https://github.com/macfuse/macfuse/wiki/FUSE-Backends)）：
  - "The FUSE notification API is not supported, yet."
  - "Most mount options previously handled by the kernel are not implemented, yet."
  - "I/O performance of FSKit volumes is not on par with volumes using the kernel extension backend."
  - **只读挂载要 macOS 26.4**："Add support for read-only mounts on macOS 26.4 and later"（[5.5.0 release](https://github.com/macfuse/macfuse/releases/tag/macfuse-5.5.0)）。在 15.4–26.3 上只能挂成读写、由文件系统对写返回错误；这样的行为在 Finder 和各种工具里是什么样子，**未确认**。
  - 5.5.0 还修了一个 FSKit 的数据损坏问题："Mitigate a data corruption issue in `FSKit` on macOS 26 and 27 by rejecting empty `FUSE_READ` replies with `EIO`"（同上）。对不可变的 Resource，文件大小不会变，这条 workaround 的副作用（"can cause read errors when a file's size changes remotely"）碰不到。
  - 5.3.0 引入新的 channel API 后，"significantly improves I/O performance for volumes mounted using the `FSKit` backend"（[5.3.0 release](https://github.com/macfuse/macfuse/releases/tag/macfuse-5.3.0)）。两个后端之间现在差多少，**未确认**（没有官方数字）。
  - FSKit 挂载必须在登录用户的上下文里跑，不能 `sudo`："Running the file system as `root` through `sudo` or a system LaunchDaemon can make the macFUSE extensions unavailable"（[Troubleshooting (Developer)](https://github.com/macfuse/macfuse/wiki/Troubleshooting-(Developer))）。这对 CLI 正好合适。
- **安装与授权**（[Getting Started](https://github.com/macfuse/macfuse/wiki/Getting-Started)）：
  - FSKit 后端："It does not require a kernel extension or any security configuration changes in Recovery Mode." 但扩展要用户在 "System Settings > General > Login Items & Extensions > File System Extensions" 里启用（[Troubleshooting](https://github.com/macfuse/macfuse/wiki/Troubleshooting)；go-fuse 的错误信息也是这句，[fskit_darwin.go](https://github.com/hanwen/go-fuse/blob/6018575d027b3cedc12d34e8e1ad7e5a50c0a6f5/fskit/fskit_darwin.go#L55)）。
  - kext 后端在 Apple Silicon 上：先在系统设置里 "Enable System Extensions…"，关机，进 Recovery 的 Startup Security Utility，"Select "Reduced Security" and enable "Allow user management of kernel extensions from identified developers". Then click on "OK", enter your login password when prompted and restart your Mac." 之后还要再 "Allow" 一次并重启；每次升级 macFUSE 都要重新 Allow。"Recent versions of macOS do not support loading third-party kernel extensions when being running in a virtual machine"（同上页）。
  - 安装需要管理员密码（pkg 安装器，同上页）。
- **许可证**：自 3.9（2019 年 5 月）起 "some components of the macFUSE software are no longer open source, e.g. the kernel extension"；"Bundling or integrating macFUSE with commercial software requires a paid-for license"（[Open Source Status](https://github.com/macfuse/macfuse/wiki/Open-Source-Status)）。条款原文见 §0 第 3 条。

### 1.2 FUSE-T

- **原理**："FUSE-T is a kext-less implementation of FUSE for macOS that uses NFS v4 local server instead of a kernel extension"；"libfuse launches a FUSE-T NFS/SMB/FSKit server that exposes a local TCP port to the macOS mount proccess … The server converts RPC calls into FUSE requests that emulate original kernel-libfuse protocol"（[README](https://github.com/macos-fuse-t/fuse-t)）。后端可选 `-o backend=[smb|nfs|fskit]`，默认 NFS；FSKit 后端 "for macOS 26+"（[wiki](https://github.com/macos-fuse-t/fuse-t/wiki)，README）。
- **API 兼容**："API headers in libfuse are unchanged … The APIs are compatible with 'macfuse' but differ from the Linux version of libfuse"（README）。它交付的是 `libfuse-t.dylib` / `libfuse-t.a`（[wiki](https://github.com/macos-fuse-t/fuse-t/wiki)），**不提供** `/dev/fuse` 式的设备描述符，也不提供 `mount_macfuse`。
- **限制**（[wiki](https://github.com/macos-fuse-t/fuse-t/wiki)）：
  - "Caching of attributes is done by the client. Currently the caching attributes returned by the filesystem implementation are ignored."
  - 不支持：IOCTL、BMAP、FALLOCATE；"Notifications: Works for SMB backend, unsupported for NFS and FSKit"。
  - "File access and modification times cannot be set separately"。rclone 记录了后果："viewing files with various tools, notably macOS Finder, will cause rlcone to update the modification time of the file"，以及 "When mounting with `--read-only`, attempts to write to files will fail _silently_"（[rclone mount](https://rclone.org/commands/rclone_mount/)）。
  - 权限：终端要在 "Privacy & Security" / "Files and Folders" 里打开 "Network Volumes"，"This is a consequence of fuse-t leveraging the nfs system under the covers"（README）。
  - mmap 行为与性能数字：官方没写，**未确认**。README 只说 "Good performance"。
- **许可证**："Free for non-commercial use"；"For commercial use or/and bundling with commercial software … The software vendor has to obtain a commercial license from the FUSE-T authors"；内含的 libfuse 是 LGPL（[License.txt](https://github.com/macos-fuse-t/fuse-t/blob/main/License.txt)）。
- **Go 库能不能用**：
  - go-fuse：**不能**。darwin 上它只找 `/Library/Filesystems/macfuse.fs/.../mount_macfuse` 和 `osxfuse` 的同名工具，自己说 FUSE 内核协议（[mount_darwin.go](https://github.com/hanwen/go-fuse/blob/6018575d027b3cedc12d34e8e1ad7e5a50c0a6f5/fuse/mount_darwin.go#L101)）；FSKit 路径只加载 macFUSE 的 `MFMount.framework`（[fskit_darwin.go#L31](https://github.com/hanwen/go-fuse/blob/6018575d027b3cedc12d34e8e1ad7e5a50c0a6f5/fskit/fskit_darwin.go#L31)）。仓库里没有任何 FUSE-T 相关代码（全文搜索 `fuse-t`/`FUSE-T` 无结果）。这是读源码得出的推断。
  - cgofuse：**能**，在 cgo 构建里依次 `dlopen` macFUSE 的 `libfuse.2.dylib`、`libosxfuse.2.dylib`，最后 `/usr/local/lib/libfuse-t.dylib`（[host_cgo.go#L172-L183](https://github.com/winfsp/cgofuse/blob/master/fuse/host_cgo.go#L172-L183)）。

### 1.3 go-fuse v2 在 darwin 上

- **维护者态度**："The main developer (hanwen@) does not own a Mac to test, but accepts patches to make Go-FUSE work on Mac"（[README#macos-support](https://github.com/hanwen/go-fuse#macos-support)）。
- **已知限制**（同上）："All of the limitations of OSXFUSE, including lack of support for NOTIFY"；"OSX issues STATFS calls continuously (leading to performance concerns)"；"OSX has trouble with concurrent reads from the FUSE device, leading to performance concerns"。
- **kext 路径**：`fuse.NewServer` 在 darwin 上 exec `mount_macfuse`（或 `mount_osxfuse`），经 socketpair 拿到 FUSE 设备描述符（[mount_darwin.go#L33](https://github.com/hanwen/go-fuse/blob/6018575d027b3cedc12d34e8e1ad7e5a50c0a6f5/fuse/mount_darwin.go#L33)）。整个仓库没有 `import "C"`，不需要 cgo。
- **FSKit 路径**：`fskit.Mount(mountPoint string, rawFS fuse.RawFileSystem, opts *fuse.MountOptions)` 和 `fskit.MountFS(dir, root fs.InodeEmbedder, options)`（[fskit_darwin.go#L214](https://github.com/hanwen/go-fuse/blob/6018575d027b3cedc12d34e8e1ad7e5a50c0a6f5/fskit/fskit_darwin.go#L214)），`//go:build darwin`；非 darwin 上有返回 `ErrUnsupported` 的桩（[fskit_other.go](https://github.com/hanwen/go-fuse/blob/6018575d027b3cedc12d34e8e1ad7e5a50c0a6f5/fskit/fskit_other.go)），所以 CLI 可以无条件 import。依赖 `github.com/ebitengine/purego v0.10.0`（go.mod），用 `purego.Dlopen` 加载 MFMount（[#L110](https://github.com/hanwen/go-fuse/blob/6018575d027b3cedc12d34e8e1ad7e5a50c0a6f5/fskit/fskit_darwin.go#L110)）。它把 MFMount 的错误码翻成可读提示，例如 "macFUSE FSKit extension is not approved; enable it in System Settings → General → Login Items & Extensions → File System Extensions"。
  - **未发布**：`fskit` 的两个提交是 2026-10-31 和 2026-11-04（[commits](https://github.com/hanwen/go-fuse/commits/master/fskit)），v2.12.1 是 2026-10-21，`master` 比它多 101 个提交。用它就要钉 `master` 的伪版本，或等下一个 tag。ADR-0030 本来就说要给 go-fuse 打补丁（上游或 fork），这里多一个钉版本的理由。
  - macFUSE 也注明 "parts of the `MFMount.framework` API may still change in future releases"（[5.3.0 release](https://github.com/macfuse/macfuse/releases/tag/macfuse-5.3.0)）。
- **Linux 对照**：没有 root 时经 `fusermount3`（找不到再找 `fusermount`）挂载，root 时可直接 mount（[mount_linux.go](https://github.com/hanwen/go-fuse/blob/6018575d027b3cedc12d34e8e1ad7e5a50c0a6f5/fuse/mount_linux.go)）。开发机上要有 fuse3 包提供的 `fusermount3`；rclone 也写明 systemd 下挂载 "requires the `fusermount` or `fusermount3` program"，新版 Ubuntu 上可能被 AppArmor 拦（[rclone mount](https://rclone.org/commands/rclone_mount/)）。

### 1.4 cgofuse（winfsp/cgofuse）

- README 的平台表：macOS 只有 **cgo** 列打勾，**!cgo** 只在 Windows 上有；macOS 的构建前提是 "macFUSE, command line tools"；Linux 是 "libfuse-dev, libfuse3-dev, gcc"（[README](https://github.com/winfsp/cgofuse)）。`host_cgo.go` 带 `//go:build cgo`（[#L1](https://github.com/winfsp/cgofuse/blob/master/fuse/host_cgo.go#L1)）。许可证 MIT。
- 运行时在 Linux 上 `dlopen("libfuse.so.2")` 或 `libfuse3.so.3`，在 macOS 上按 §1.2 的顺序找 macFUSE 或 FUSE-T（[host_cgo.go#L165-L200](https://github.com/winfsp/cgofuse/blob/master/fuse/host_cgo.go#L165-L200)）。
- **对 orrery 的含义**：要么放弃 `CGO_ENABLED=0`，要么 CLI 拆成两种构建；darwin 的 cgo 构建要在 macOS runner 上做（或配交叉编译工具链），Linux 用户机器上还要有 libfuse 的 .so。rclone 就是这样：cgofuse 版的挂载 `//go:build cmount && (... (darwin && cgo) ...)`（[cmd/cmount/mount.go](https://github.com/rclone/rclone/blob/master/cmd/cmount/mount.go)），Homebrew 的 rclone "will not support `mount` any more"（[install.md](https://github.com/rclone/rclone/blob/master/docs/content/install.md)）。它的接口是 libfuse 的高层 path API，不是 `RawFileSystem`，和 `orrery-resourcefs` 不能共用代码。

### 1.5 纯 Go 的 NFSv3 回环服务

- **go-nfs**："NFSv3 protocol implementation in pure Golang"；"Minimally tested"；最新 v0.0.4（2026-04-13），Apache-2.0。挂载示例：macOS `mount -o port=<n>,mountport=<n> -t nfs localhost:/mount <mountpoint>`，Linux `mount -o port=<n>,mountport=<n>,nfsvers=3,noacl,tcp -t nfs localhost:/mount <mountpoint>`；"The need for running portmap (which is the only part that needs a privileged listening port) can be avoided through specific mount options"。基于 billy 抽象，"NFS expects access to an `inode` or equivalent unique identifier … These are considered opaque identifiers here"（[README](https://github.com/willscott/go-nfs)）。
- **rclone 的做法**：`nfsmount` "spins up an NFS server using serve nfs command and mounts it to the specified mountpoint"；"For macOS … prefer the dedicated rclone nfsmount command"（[rclone nfsmount](https://rclone.org/commands/rclone_nfsmount/)）。源码：服务监听随机端口，再执行 `mount -o port=P -o mountport=P -o tcp ...`；默认**不加** sudo，`--sudo` 是可选项 "Use sudo to run the mount/umount commands as root"；macOS 上卸载用 `diskutil umount force`（[cmd/nfsmount/nfsmount.go](https://github.com/rclone/rclone/blob/master/cmd/nfsmount/nfsmount.go)）。rclone 依赖 `github.com/willscott/go-nfs v0.0.4`（[go.mod](https://github.com/rclone/rclone/blob/master/go.mod)）。`serve nfs` 是 NFSv3，"If the server is restarted the handle cache will be lost and connected NFS clients will get stale handle errors"，"This server does not implement any authentication"（[rclone serve nfs](https://rclone.org/commands/rclone_serve_nfs/)）。
- **macOS：非 root、免安装，按源码看是可以的。**
  - xnu 对非 root 挂载的要求只是挂载点归自己："If the user is not root, ensure that they own the directory onto which we are attempting to mount"，并 "silently enforce MNT_NOSUID and MNT_NODEV"（[vfs_syscalls.c#L2075-L2086，#L1271-L1279](https://github.com/apple-oss-distributions/xnu/blob/xnu-12377.121.6/bsd/vfs/vfs_syscalls.c#L2075-L2086)）。
  - NFS 客户端：只有 `resvport` 要特权，"root permission is required to mount using resvport mount option"（[mount_nfs.8#L183-L192](https://github.com/apple-oss-distributions/NFS/blob/NFS-343.100.5/mount_nfs/mount_nfs.8#L183-L192)；内核 `priv_check_cred(..., PRIV_NETINET_RESERVEDPORT, 0)`，[nfs_vfsops.c#L3310](https://github.com/apple-oss-distributions/NFS/blob/NFS-343.100.5/kext/nfs_vfsops.c#L3310)），`mount_nfs.c` 只在显式 `-P` 或 `-o resvport` 时设这个标志（[mount_nfs.c](https://github.com/apple-oss-distributions/NFS/blob/NFS-343.100.5/mount_nfs/mount_nfs.c)）。内核注释："ok for non-root can set owner to self"（[nfs_vfsops.c#L3228](https://github.com/apple-oss-distributions/NFS/blob/NFS-343.100.5/kext/nfs_vfsops.c#L3228)）。
  - 推荐的参数：`mount -t nfs -o vers=3,tcp,port=P,mountport=P,nolocks,rdonly localhost:/ <dir>`。`nolocks`："do not support NFS file locking operations. Any attempt … will return the error ENOTSUP"；也可用 `locallocks` 在客户端本地加锁（[mount_nfs.8#L387-L409](https://github.com/apple-oss-distributions/NFS/blob/NFS-343.100.5/mount_nfs/mount_nfs.8#L387-L409)）。这一组参数在真机上能否不经 sudo 挂上，**未确认**（rclone 默认不 sudo 是旁证）。
  - 与 FUSE-T 同理，终端可能要被授予 "Network Volumes" 权限（[FUSE-T README](https://github.com/macos-fuse-t/fuse-t)），**未确认**是否同样适用于直接挂 NFS。
- **Linux：要 root，且要装 NFS 客户端工具。** 见 §0 第 1 条。`mount -t nfs` 走 nfs-utils 的 `mount.nfs` 帮助程序（nfs(5) "is part of the nfs-utils (NFS utilities) project"，[nfs(5)](https://man7.org/linux/man-pages/man5/nfs.5.html)）；开发机默认是否装了，**未确认**（随发行版）。Linux 上默认 `resvport`："If this option is not specified, or the resvport option is specified, the NFS client uses a privileged source port"（同上），回环上用 root 挂没问题。
- **缓存与语义**：
  - macOS 属性缓存默认 "The default minimum is 5 seconds and the default maximum is 60 seconds"，可用 `actimeo=` 统一设置；`nonegnamecache` 可关负缓存（[mount_nfs.8#L345-L379](https://github.com/apple-oss-distributions/NFS/blob/NFS-343.100.5/mount_nfs/mount_nfs.8#L345-L379)）。版本不可变，可以把 `actimeo` 设得很大。Linux 默认 `acregmax` 60 秒（[nfs(5)](https://man7.org/linux/man-pages/man5/nfs.5.html)）。
  - macOS 读：`rsize` "The default is 8192 for UDP mounts and 32768 for TCP mounts"；`readahead` "The default is 16. This may be in the range of 0 - 128"（[mount_nfs.8#L279-L322](https://github.com/apple-oss-distributions/NFS/blob/NFS-343.100.5/mount_nfs/mount_nfs.8#L279-L322)）。macOS 客户端允许的 `rsize` 上限和回环 NFS 的实际吞吐，**未确认**。
  - 文件句柄：NFS 客户端拿着句柄跨服务重启；服务端须让句柄是无状态的（例如按 manifest 排序路径编号，正是 ADR-0030 spike 2 的做法），否则会像 rclone 那样 "stale handle errors"。go-nfs 自带的 `CachingHandler` 是有状态的 LRU（README 示例 `nfshelper.NewCachingHandler(handler, 1)`），要换成自己的实现。
  - 这条路和 Linux 上的 FUSE 完全是两套代码：一个是 billy/go-nfs 的 handler，一个是 `fuse.RawFileSystem`。能共用的只有 manifest、块获取、校验与缓存。

### 1.6 Apple FSKit（第三方直接用）

- "Implement a file system that runs in user space"，从 macOS 15.4 起有；"You do this by developing an FSKit module (FSModule), which you deliver as an app extension that runs in user space, and is compatible with Mac App Store distribution"；"The current version of FSKit supports only FSUnaryFileSystem"（[FSKit](https://developer.apple.com/documentation/fskit)）。
- 不以块设备为资源的 `FSPathURLResource`（"A resource that represents a path in the system file space"）和 `FSGenericURLResource`（"A resource that represents an abstract URL"）从 **macOS 26.0** 才有；15.4 只有 `FSBlockDeviceResource`（[FSPathURLResource](https://developer.apple.com/documentation/fskit/fspathurlresource)，[FSGenericURLResource](https://developer.apple.com/documentation/fskit/fsgenericurlresource)，[FSBlockDeviceResource](https://developer.apple.com/documentation/fskit/fsblockdeviceresource)）。
- **对 orrery 的含义**：要交付一个装在 `.app` 里的 app extension（Swift/Objective-C，签名、公证），一个单独的 Go 二进制做不到。这正是 macFUSE 5 的 FSKit 后端替人做掉的部分。自己写不划算。

### 1.7 同类工具怎么做

| 工具 | Linux | macOS | 依据 |
|---|---|---|---|
| rclone `mount` | FUSE（`cmd/mount` 是 `//go:build linux`，bazil；或 cgofuse 的 `cmount`） | cgofuse + macFUSE 或 FUSE-T，需 cgo 构建 | [cmd/mount/mount.go](https://github.com/rclone/rclone/blob/master/cmd/mount/mount.go)，[cmd/cmount/mount.go](https://github.com/rclone/rclone/blob/master/cmd/cmount/mount.go)，[rclone mount](https://rclone.org/commands/rclone_mount/) |
| rclone `nfsmount` | NFSv3 回环（实验性，v1.65 起） | 同左，官方推荐的 macOS 路径；不指定 `--vfs-cache-mode` 时 "the mount point will be read-only" | [rclone nfsmount](https://rclone.org/commands/rclone_nfsmount/) |
| JuiceFS | FUSE | "you need to install macFUSE to enable FUSE mounting"；否则用 WebDAV、Gateway 或 Python SDK | [JuiceFS 安装](https://juicefs.com/docs/community/getting-started/installation/) |
| s3fs-fuse | FUSE | `brew install --cask macfuse` + `brew install gromgit/fuse/s3fs-mac`；GPLv2 | [s3fs README](https://github.com/s3fs-fuse/s3fs-fuse) |
| gcsfuse | 只列 Linux 发行版（"Rocky Linux 8.9 or later"、"Ubuntu 18.04 or later" 等）和 WSL | 不支持（文档未提） | [Cloud Storage FUSE overview](https://docs.cloud.google.com/storage/docs/cloud-storage-fuse/overview) |
| Mountpoint for Amazon S3 | "Mountpoint for Amazon S3 is only available for Linux operating systems." | 不支持 | [INSTALL.md](https://github.com/awslabs/mountpoint-s3/blob/main/doc/INSTALL.md) |

没有一家在 macOS 上做到了「免安装 FUSE」；做到免安装的只有走 NFS 的 rclone `nfsmount`。

---

## 2. 对比表

「root」指开发者每次挂载时是否需要 root/sudo；「额外安装」指 orrery 二进制之外用户要装的东西。

| 机制 | Linux | macOS | root | 额外安装 | cgo | 许可证 | 性能与语义注意点 |
|---|---|---|---|---|---|---|---|
| **go-fuse + Linux 内核 FUSE** | 是 | — | 否（经 setuid `fusermount3`） | fuse3（提供 `fusermount3`），多数发行版已装，**未确认** | 否 | go-fuse BSD 风格（"BSD-style license"） | 与 `orrery-resourcefs` 同一套代码和参数；spike 1 已测 |
| **go-fuse + macFUSE kext** | — | macOS 11+ | 否（安装 pkg 要管理员密码） | macFUSE；Apple Silicon 要进 Recovery 设 "Reduced Security" 并重启；虚拟机里不能加载 kext | 否 | macFUSE 闭源二进制，商业捆绑/自动安装需书面许可 | 性能最好；go-fuse 自述 macOS 上 STATFS 频繁、FUSE 设备并发读有问题 |
| **go-fuse `fskit` + macFUSE FSKit 后端** | — | 15.4+；只读挂载 26.4+ | 否，且**不能** sudo | macFUSE 5.3+；在系统设置里启用 File System Extensions，不重启 | 否（purego） | 同上；go-fuse 的 `fskit` 未发布 | macFUSE 自述 I/O "not on par" kext；不支持 notification 和多数挂载参数；MFMount API 可能变 |
| **cgofuse + macFUSE / FUSE-T** | 是（libfuse） | 是 | 否 | macFUSE 或 FUSE-T；Linux 要 libfuse .so | **是** | MIT（库）+ 上述后端许可 | 破坏 `CGO_ENABLED=0`；高层 path API，不能与 `orrery-resourcefs` 共用 |
| **FUSE-T（NFS/SMB/FSKit 后端）** | — | macOS 13+（1.2.6 起），FSKit 后端 26+ | 否 | FUSE-T pkg/brew；终端要 "Network Volumes" 权限 | 只能经 cgofuse，故**是** | 闭源，非商用免费，商用需授权；libfuse LGPL | 忽略文件系统返回的属性缓存时间；只读时写入"静默失败"；mmap、性能**未确认** |
| **纯 Go NFSv3 回环（go-nfs）** | 是，但要 root | 是 | Linux **是**；macOS 否（挂载点归自己、不加 `resvport`，**未实测**） | Linux 要 nfs-utils 的 `mount.nfs`；macOS 无 | 否 | go-nfs Apache-2.0 | 两套前端代码；默认属性缓存 5–60 s（可调大）；TCP `rsize` 默认 32 KiB、预读 16 个 RPC；服务重启后句柄需无状态才不 stale；无认证，只能听 127.0.0.1 |
| **自写 FSKit 扩展** | — | 15.4+（URL/路径资源 26.0+） | 否 | 要随附签名的 `.app` | 要 Swift/ObjC | — | 不是单个 Go 二进制能交付的 |

---

## 3. 能做到「一份实现」的程度

把挂载拆成两层：下面是 manifest → 节点树、块获取（预签名、重试、SHA-256 校验）、本地块缓存，两边完全相同；上面是暴露给内核的协议层。

- **go-fuse 两边通用**：协议层也是同一份 `fuse.RawFileSystem`。差别只剩挂载调用：Linux `fuse.NewServer(...)`；macOS `fskit.Mount(...)`（或 kext 时仍是 `fuse.NewServer`）。语义上的差别来自 macOS 的 FUSE 实现本身：FSKit 不支持多数挂载参数（例如 Linux 上的 `MaxReadAhead`、`FOPEN_KEEP_CACHE` 在 FSKit 上是否生效，**未确认**），STATFS 频繁，FSKit 15.4–26.3 不能只读挂载。
- **Linux FUSE + macOS NFS**：两套协议层，缓存语义各自不同（FUSE 的 entry/attr 超时与 NFS 客户端的 `actimeo`），句柄生命周期也不同。rclone 走的是这条路，也因此 macOS 和 Linux 的行为说明要分开写。
- **NFS 两边通用**：Linux 上每次都要 sudo，还要装 nfs-utils，违背「CLI 一个二进制、普通用户能用」。不取。
- **cgofuse 两边通用**：代码一份，但要 cgo，且与服务端 go-fuse 的实现分家。不取。

---

## 4. 推荐

**用 go-fuse 做两边的唯一实现，macOS 上依赖用户自装的 macFUSE，默认 FSKit 后端，kext 兜底。**

决定性的三条事实：

1. **Linux 上 NFS 需要 `CAP_SYS_ADMIN`，FUSE 不需要**（§0 第 1 条）。所以 Linux 只能是 FUSE，而两边一致只能靠 macOS 也用 FUSE。
2. **go-fuse 的 `fskit` 包经 purego 接 macFUSE 的 FSKit 后端，入参就是 `fuse.RawFileSystem`，没有 cgo**（§1.3）。`CGO_ENABLED=0` 和四平台交叉编译都不用动，CLI 能直接复用 `orrery-resourcefs` 的只读文件系统（节点 ID 按 manifest 排序路径、无状态句柄）。
3. **macOS 上没有免安装的 FUSE，macFUSE 又不许商业软件捆绑或自动安装**（§0 第 3 条）。所以 CLI 不能替用户装，只能检测、报错并给出安装指引。FSKit 后端免去了 Apple Silicon 上 Recovery 模式 + 重启这一步，只需在系统设置里点一次启用（§1.1）。

落地要点（不在本文范围，只列相关的）：

- 依赖要钉 go-fuse `master` 的伪版本，直到它发 tag；与 ADR-0030 要求的 go-fuse 补丁合并考虑。
- `fskit.Available()` 为假时（macOS < 15.4，或未装 macFUSE 5.3+），退回 kext 路径；`mount_macfuse` 也找不到时，报错给出 macFUSE 下载地址和两条授权路径。
- macOS 26.4 以下用 FSKit 时挂不成只读，文件系统层要对所有写操作返回 `EROFS`。
- 在 Mac 上要实测一遍 spike 1 的读负载（小文件 DataLoader 形态 + 4 GiB 顺序读），分别跑 FSKit 和 kext；macFUSE 说 FSKit 慢，但没给数字。
- 是否把 macFUSE 的商业授权买下来，取决于 orrery 算不算 "commercial software"，这是许可问题，不是技术问题。

**什么情况下改推荐**：

- 用户（或公司 IT）不肯装 macFUSE，或实测 FSKit 太慢、kext 又因为 Recovery 模式而装不了：在 macOS 上加一个 go-nfs 前端（rclone `nfsmount` 的做法），共用下面那层。代价是第二套协议层代码和另一份缓存语义说明。
- go-fuse 的 `fskit` 包被回退或长期不发布：自己 fork，或者 macOS 改用 NFS 前端。cgofuse 不在候选里，因为它要 cgo。

---

## 5. 未确认项

1. go-fuse `fskit` 在 Apple Silicon、macOS 15.4/26.x 上能否以非 root 挂上，`RawFileSystem` 的只读实现在其上是否完全可用；维护者没有 Mac，本文也没实测。
2. macFUSE FSKit 与 kext 后端的实际读性能（小文件 IOPS、单流顺序读），以及 go-fuse 的 `MaxReadAhead`、`FOPEN_KEEP_CACHE`、entry/attr 超时在 FSKit 上是否生效。
3. macOS 15.4–26.3 上 FSKit 只能读写挂载时，对写操作一律返回 `EROFS` 的效果（Finder 会不会写 `._` 文件、报什么错）。
4. macOS 上 `mount -t nfs -o vers=3,tcp,port=P,mountport=P,nolocks,rdonly localhost:/ <dir>` 在不加 sudo 的情况下是否真能成功；是否触发 "Network Volumes" 隐私授权；macOS NFS 客户端 `rsize` 的上限和回环吞吐。
5. Linux 开发机默认是否装了 `fusermount3`、`mount.nfs`（随发行版）。
6. FUSE-T 的 mmap 行为和性能，官方没有数字。
7. orrery 的发布方式是否落入 macFUSE / FUSE-T 许可证所说的 "commercial software"。
