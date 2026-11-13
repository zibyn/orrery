# WSFS P0 spike：runc / runsc 下 bind 宿主 FUSE 挂载跑 Git

日期 **2026-11-13**。对应 `workspace-shared-storage-s3.md` §4.5 的 P0：WSFS 能不能以宿主 FUSE 挂载 + bind mount 的形态放进 Sandbox 的 `/workspace`。

## 环境与方法

- 主机 `xzai-a800-8`：内核 5.15.0-119，128 核，`/root` 在本地 SSD；containerd 2.2.1（`orrery-containerd`），runsc `release-20261026.0`；镜像 `golang:1.26`（git 2.47.3）。
- FUSE：`hanwen/go-fuse/v2` v2.12.1 的 `fs.NewLoopbackRoot`，`DirectMount`、`AllowOther`，宿主上以 `setsid` 独立进程运行，挂在 `<dir>/mnt`，再 `rbind` 进容器的 `/workspace`。loopback 每个操作都要再对宿主目录发一次 syscall；真实的 WSFS 元数据在内存里，元数据操作会便宜一些，但 FUSE 往返本身的开销一样不少。
- 测试仓库：Go 1.26 的 `src/` + `test/`，14,941 个跟踪文件，两次提交（第二次改了 3,000 个 `.go` 文件），repack 后 33 MiB；工作区 212 MiB。从只读 bind 的 bare repo `git clone --no-hardlinks`。
- 负载（`work.sh`）：clone、`git status` ×3、两次 checkout（各改写 3,000 个文件）、改 500 个文件再提交、`git gc`、`git fsck --strict`、新建 3,000 个未跟踪文件后 `git status`。之后跑 `fscheck`，检查 Git 和常见工具依赖的 13 项 POSIX 语义。
- runsc 用与 Agent 相同的默认配置（`host-uds=create`，bind mount 为 shared 访问）；另跑一组 `file-access-mounts=exclusive` 作对照。
- 脚本在 a800 的 `/root/wsfs-spike/`（`run.sh setup|matrix|kill|cleanup`），原始输出在其中的 `matrix1.txt`、`matrix-excl.txt`。

## 结果

### 正确性

两个 Runtime × 宿主目录/FUSE 的所有组合中，`git fsck --strict` 都通过，`fscheck` 下面这些项也都通过：`O_EXCL` 排他创建、rename 覆盖（锁文件模式）、硬链接（同 ino、nlink=2）、符号链接、可执行位、`MAP_SHARED` mmap 写回、跨进程 `fcntl` 锁和 `flock` 冲突、unix socket、`fsync`/`fdatasync`、unlink 后已打开的文件仍可读。

唯一的差异来自 gVisor 本身，与 FUSE 无关：runsc 把 1 GiB 的稀疏文件报告为已全部分配（`st_blocks` 等于文件大小），在宿主目录上也是这样。go-fuse 不开 `EnableLocks` 时，锁由内核按本机语义处理，单写者下够用。

### 性能（毫秒，单次运行）

| 步骤 | runc 宿主目录 | runc FUSE ttl=1s | runc FUSE ttl=60s | runc FUSE 60s+noxattr | runsc 宿主目录 | runsc FUSE ttl=1s | runsc FUSE 60s+noxattr | runsc excl 宿主目录 | runsc excl FUSE 60s |
|---|---|---|---|---|---|---|---|---|---|
| clone | 797 | 9433 | 9522 | 8598 | 6626 | 21918 | 19965 | 4287 | 18612 |
| status 冷 | 334 | 804 | 907 | 666 | 2242 | 3551 | 3369 | 1864 | 3360 |
| status 热 | 29–325 | 515–640 | 513–529 | 615–624 | 1270–1352 | 3253–3402 | 3229–3296 | 1012–1028 | 2587–2723 |
| checkout（3,000 文件） | 292 / 440 | 1707 / 2893 | 2561 / 2518 | 2075 / 2300 | 3780 / 3945 | 8829 / 8939 | 7607 / 7264 | 3086 / 3308 | 7049 / 6773 |
| 改 500 文件并提交 | 342 | 2317 | 2014 | 1575 | 4714 | 9668 | 8782 | 3278 | 7251 |
| gc | 593 | 1461 | 1448 | 1394 | 3196 | 4636 | 4396 | 2996 | 3934 |
| fsck | 1346 | 1377 | 1378 | 1364 | 3472 | 3594 | 3588 | 3386 | 3507 |
| 3,000 未跟踪文件 + status | 119 | 1894 | 1950 | 1621 | 2554 | 7621 | 6294 | 2218 | 6017 |

读法：

- **FUSE 在 runc 下让写密集的 Git 操作慢 2.5–16 倍**（clone 约 12 倍，checkout 约 6–9 倍，提交约 5–7 倍，gc 约 2.5 倍，3,000 个小文件约 16 倍）。读密集的 `fsck` 基本不受影响（读走内核页缓存）。
- **TTL 和关闭 xattr 几乎没用**：ttl 1s 和 60s 结果相同；关闭 xattr 只快 10–20%，代价是丢掉 `user.*` xattr。瓶颈是每次 create/write/rename 的 FUSE 往返，不在属性缓存。go-fuse 2.12.1 没有暴露内核 writeback cache 开关；FUSE passthrough 需要 6.9 以上内核，a800 是 5.15。这两项都没测。
- **runsc 本身就慢**：在宿主目录上，clone 比 runc 慢 8 倍，热 `status` 慢 4–45 倍（1.3s 对 29–325ms）。exclusive 模式只快 20–35%，而且不能用：Agent 的 Files API 会在 Sandbox 运行时直接写宿主 Workspace（`internal/agent/files.go:133`），exclusive 会让 Sandbox 看到过期内容。
- **runsc + FUSE 叠加**：clone 约 20s，是 runc 宿主目录的 25 倍；`status` 约 3.3s。

### FUSE 进程死亡

FUSE 进程被 `kill -9` 后，runc 和 runsc 容器里的 `/workspace` 都变成 `Transport endpoint is not connected`。在宿主上重新起一个 FUSE 进程，挂到同一路径（`fusermount` 报错），也救不回已运行的容器：容器 bind 的是那个已经死掉的挂载，只能重启 Sandbox。

## 结论

1. **语义上可行**：Git 需要的 POSIX 语义在两种 Runtime 下都能透过 FUSE。
2. **性能上不达标**：「FUSE 在每次读写的热路径上」这个形态，在 runc 下让 clone/checkout 慢一个数量级，在 runsc 下更是 20 秒级的 clone。按「Workspace 里跑 Git 要够快」的要求，P0 判定为**失败**，启用设计文档里的退路。
3. **退路的成本已量化**：同一工作区在宿主上做一遍 stat 扫描（size、mtime、ino、路径）要 41–44ms，16 并发对全部文件做 SHA-256 要 258ms（首次提交的最坏情况）。之后的增量提交只需对变化的文件重算哈希。
4. 如果以后仍要用 FUSE（例如大 Workspace 只读挂载的懒加载），FUSE 进程就是 Sandbox 的生命线：它不能跟 Agent 一起重启，升级要把 `/dev/fuse` 的 fd 交给新进程（JuiceFS 平滑升级的做法）。fd 交接本次没测。

## 未测

- 内核 writeback cache、FUSE passthrough（需更新的内核），以及非 loopback 的内存元数据实现。
- `/dev/fuse` fd 交接升级。
- 多次运行的方差（每组只跑了一次；runc 宿主目录 `status` 热 29–325ms 的离散度说明单次数据有噪声，但数量级差距远大于噪声）。
