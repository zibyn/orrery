# runsc 根文件系统写入在宿主上可见吗：overlay2 spike

日期 **2026-11-17**。问题：要让「沙箱的整个文件系统停止后保留并能跨节点恢复」（参考 Daytona，见 ADR-0031 修订讨论），Agent 需要在宿主上按文件扫描容器根文件系统的改动（containerd overlayfs 快照的 upperdir），再按 `workspace-shared-storage-s3.md` §4.6 的 stat 增量提交。runsc 默认的 `overlay2` 设置可能把这些写入留在 gVisor 内部。本文回答两件事：

1. 在 runc、runsc 默认配置、runsc `overlay2 = "none"` 下，容器对根文件系统的写入能否在宿主 upperdir 里按文件看到；
2. 改成 `overlay2 = "none"` 的性能代价，和今天 runsc 下 bind mount 进来的 `/workspace` 比怎么样。

## 环境与方法

- 主机 `xzai-a800-8`：内核 5.15.0-119，`/` 是本地 SSD 上的 ext4；containerd 2.2.1（`orrery-containerd`，snapshotter `overlayfs`），runsc `release-20261026.0`。
- 镜像 `golang:1.26`（git 2.47.3），导入 containerd 命名空间 `spike`，不碰 `orrery` 命名空间。
- runsc 选项与 Agent 一致：`host-uds = "create"`（`internal/agent/containerd.go:93-94`），Agent 没有设置 `overlay2`，所以用的是 runsc 默认值。对照组只多加一行 `overlay2 = "none"`。
- 负载（全部在容器内，`ctr run`，`--net-host`）：写两个标记文件（`/root`、`/etc`）；`cp -a /usr/local/go/src`（约 15k 文件）；`git init && git add -A`；`git commit`；`git status` ×2；改 500 个跟踪文件；`git status`；`git commit -a`；`dd` 1 GiB 加 `fsync`。之后在宿主上用 `ctr snapshots mounts` 取 upperdir，检查标记文件、`.git` 是否可见，统计文件数。
- 五组配置：`runc`、`runsc-default`、`runsc-none` 写根文件系统；`runc-bind`、`runsc-bind` 写一个 bind mount 进来的宿主目录 `/work`（相当于今天的 `/workspace`）。`runsc-default`、`runsc-none` 各跑 3 次，bind 两组各 2 次，`runc` 1 次。`dd` 只在第一轮有数据（第二轮的输出过滤误删了这一行）。
- 脚本在 a800 的 `/root/rootfs-spike/run.sh`（`setup | run <cfg> | matrix | matrix2 | cleanup`），原始输出在同目录的 `matrix1.txt`、`matrix2.txt`。跑完已清掉容器和快照，`orrery-containerd` 已停；`spike` 命名空间里留着导入的 `golang:1.26` 镜像。

## 结果

### 1. 可见性

| 配置 | 宿主 upperdir 里能看到 `/root`、`/etc` 的标记文件和 `/root/src/.git` 吗 | upperdir 里的内容 |
|---|---|---|
| runc | **能** | 20,495 个文件，1.3G |
| runsc 默认 | **不能** | 只有 4 项：空的 `etc/` 和一个 2 GiB 的稀疏文件 `.gvisor.filestore.spike-runsc-default` |
| runsc `overlay2 = "none"` | **能** | 26,331 个文件，1.3G |

runsc 默认配置下，根文件系统的写入全部进了 gVisor 内部 overlay 的上层，存在 upperdir 里一个不透明的 filestore 文件中，宿主上看不到文件。按文件扫描和增量提交在这个配置下做不了。改成 `overlay2 = "none"` 之后，写入经过 gofer 落到 containerd 的 upperdir，和 runc 一样是普通文件。

### 2. 性能（毫秒，多次取平均）

| 操作 | runc 根 | runc bind | runsc 默认 根 | runsc `none` 根 | runsc bind（今天的 `/workspace`） |
|---|---|---|---|---|---|
| `cp -a` 15k 文件 | 428 | 460 | 10,982 | 9,914 | 11,975 |
| `git init` + `add -A` | 2,875 | 2,714 | 10,517 | 15,127 | 17,157 |
| `git commit` | 197 | 143 | 2,527 | 2,979 | 3,483 |
| `git status`（前两次） | 34 | 24 | 779 | 942 | 1,135 |
| 改 500 个文件 | 259 | 268 | 8,676 | 8,298 | 8,189 |
| `git status`（改后） | 29 | 22 | 908 | 993 | 1,270 |
| `git commit -a` | 138 | 124 | 1,856 | 2,057 | 2,278 |
| `dd` 1 GiB + fsync（1 次） | 2,674 | — | 1,623 | 4,207 | — |

- **`overlay2 = "none"` 的根文件系统和今天 runsc 下的 `/workspace` 一样快，甚至稍快**（`cp` 9.9 对 12.0 秒，`git add` 15.1 对 17.2 秒，`git status` 0.94 对 1.14 秒）。两者走的都是 gofer 访问宿主文件。
- **和 runsc 默认根文件系统比，`none` 慢的地方集中在元数据和写入密集的操作**：`git add` 慢 44%（15.1 对 10.5 秒），`git status` 慢约 10–20%，大文件顺序写慢约 2.6 倍（只有一次样本）。`cp` 和改文件持平。
- runsc 无论哪种配置，都比 runc 慢 20–40 倍，这是 gofer 本身的代价，不是 `overlay2` 带来的。

## 结论

1. **runsc 节点必须改成 `overlay2 = "none"`，整个根文件系统停止后保留并按文件增量提交的方案才能成立。** 默认配置下宿主只看到一个不透明的 filestore 文件，只能整体复制它，做不到文件级增量和去重，换节点恢复也绑定在 gVisor 的内部格式上。
2. **代价可以接受**：改完后根文件系统的性能和今天 runsc 的 `/workspace` 是同一个量级，代码和 Git 已经在这个速度下运行。用户在根文件系统上直接做的写入密集操作（装包、构建）会比现在慢一些，`git add` 这一类慢约 40%。
3. runc 节点不需要改动，upperdir 天然可见。

## 未确认、需要实测

- `overlay2 = "none"` 对 `apt-get install`、`pip install`、`npm install` 这类真实装包负载的影响：这次只测了复制和 Git。
- 大文件顺序写的差距只有一次样本，需要重复。
- `overlay2 = "none"` 下 runsc 的内存占用：默认配置的 filestore 是文件，不占内存，所以这里预计不会变好或变坏，但没有测。
- upperdir 里的 whiteout（删除基础镜像里的文件会生成字符设备 `0/0`）和 opaque 目录（`trusted.overlay.opaque` 扩展属性）要被扫描器正确记录和恢复，这次没测删除。
- 停止时 Agent 现在会删除快照（`internal/agent/containerd.go:302`）；要保留文件，提交必须在删除之前完成，或者改成停止后保留快照直到提交成功。
- 其他内核和文件系统（XFS、更新的内核）上的结果。
