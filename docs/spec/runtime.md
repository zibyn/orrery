# Runtime 适配器与 Exec Supervisor

本文锁定 containerd 和 Local Runtime 如何实现 ADR-0009 的五个操作（Provider 见 ADR-0026），以及 Exec Supervisor 在 Node 上的记录格式和中继方式（ADR-0019）。厂商事实见 `docs/research/runtime-adapter-mechanics.md`。数字默认值以 `defaults.md` 为准。

## 1. 一个二进制，四种模式

Supervisor 是同一个平台二进制，有四种模式：

- `supervisor init`：Runtime 的 PID 1，只负责收尸，然后休眠。它覆盖镜像自带的 entrypoint 和 cmd，所以镜像里不需要 `sleep`，distroless 镜像也能用。Coder 同样把 PID 1 和干活的进程分开；Daytona 把两件事放在一个进程里，结果收尸和取退出码互相抢，只好另写 `childreap` 补救。
- `supervisor run <exec_id>`：每个 Exec 一个，负责启动命令、写记录。命令结束后它自己也退出。
- `supervisor probe`：同步执行，输出容器内的 `cpu.max`、`memory.max`、`pids.max` 和 `/dev/nvidia*` 列表，用来做回读。containerd 上的限额改在宿主上回读，只用它的设备列表（§9）。
- `supervisor forward <port> <socket>`：在 Sandbox 里监听运行目录上的 unix socket，把每个连接转到 `127.0.0.1:<port>`，用于端口转发（§14）。

## 2. 挂载

容器只有下面这些挂载，没有别的：

| 容器内 | 来源 | 模式 |
|---|---|---|
| `<path>` | 卷，经 `orrery-resourcefs` 的可写挂载（§17） | 读写 |
| `/.orrery/bin` | Supervisor 二进制和外部 CLI `orrery` | 只读 |
| `/.orrery/run` | 宿主 `/var/lib/orrery/sandboxes/<sandbox_id>/run` | 读写 |
| `/resources/<mount>` | Node 上该版本的 Resource 挂载（§11） | 只读 |
| `/etc/resolv.conf`、`/etc/hosts`、`/etc/hostname` | Agent 在 `<data-root>/sandboxes/<sandbox_id>/etc/` 里生成 | 只读 |

根文件系统不是挂载：它是 SnapshotVersion 之上的 overlay 快照，就是 Sandbox 自己的文件系统，停止后保留并提交为 Generation（§16，ADR-0031）。`/workspace` 是其中的普通目录，镜像里没有时 Agent 在首次 Create 时建一个空目录，Exec 的默认 cwd 是它。卷的 `<path>` 由请求给出（`api.md` §3），可以就是 `/workspace`。

`resolv.conf` 取宿主的上游 resolver（宿主用 systemd-resolved 时取 `/run/systemd/resolve/resolv.conf`），去掉回环地址的 nameserver，因为 Sandbox 的回环不是宿主的。这些 nameserver 能不能连通，由 Egress Rules 决定（§13），平台不额外放行。

Node 的部署包同时带外部 CLI。Agent 在自身可执行文件旁边找到 `orrery`，和 Supervisor 一起放进 `/.orrery/bin`；沙箱内的 CLI 对 CA 的信任见 `defaults.md` 的 `access.url`。每个 Exec 的 `PATH` 末尾追加 `/.orrery/bin`，环境变量的拼装见 `node-protocol.md`。Supervisor 把 spec 里的 `env` 和 `secret_env` 原样合进命令的环境，不再改动。

Supervisor 和命令都以镜像默认的 USER 运行，平台不另选 uid。`/.orrery/run` 在宿主上要让这个用户可写。沙箱内的进程能篡改本 Sandbox 的记录。记录不是防 Sandbox 自身进程的边界：Sandbox 本来就属于它的 owner，而记录按 Sandbox 分目录，影响不到别的 Sandbox。

## 3. 记录目录 `run/<exec_id>/`

| 文件 | 内容 |
|---|---|
| `output` | 只追加的帧文件。每帧为 `[1 字节 stream][4 字节长度，大端][data]`，stream 取 `stdout`、`stderr` 或 `truncated` |
| `started` | execve 成功后原子写入（先写 tmp 再 rename），内容是 `started_at` |
| `exit` | 原子写入的 JSON：`{cause, exit_code?, signal?, finished_at}` |
| `ctl.sock` | 控制 socket，见 §5 |
| `oom_kill` | 只在 runsc Node 上有：Agent 在发 spec 之前读到的宿主 cgroup `oom_kill` 计数，十进制整数，原子写入（§9） |

- **`seq` 就是这一帧在 `output` 里的字节偏移。** Agent 重启后直接 seek 到 `stored_seq` 接着读。
- **UTF-8 切分由 Supervisor 负责。** 它按流暂存末尾不完整的字节；遇到非法字节就单独成一帧，Agent 发出这一帧时标 `encoding: base64`。Agent 只逐帧转发，所以 seq 跨 Agent 重启保持稳定。
- 写到 `log_cap`（按 `output` 的字节数算）后，追加一条 `truncated` 帧，之后的输出丢弃。命令照常运行（ADR-0015）。
- PTY 的输出不写进 `output`，而是放在 Supervisor 内存中的环形缓冲里（ADR-0017）。

## 4. 启动与结束

1. Agent 用 detached exec 启动 `supervisor run <exec_id>`，exec 的 Cmd 和 Env 里不带任何规格。
2. Supervisor 执行原子的 `mkdir run/<exec_id>`。遇到 `EEXIST` 就立即退出，不跑命令。这样即使 StartExec 被重复下发，命令也不会跑两次。
3. Supervisor 监听 `ctl.sock`，等待 Agent 发来第一条 `spec` 消息：`{argv, env, secret_env, cwd, timeout, grace, log_cap, pty?, stdin?, pty_idle}`。规格不放进 exec 的 Env 或 argv：containerd 的 exec 进程 spec 要经过 containerd 和 shim，`systemd-run -E` 传的环境变量，`systemctl show` 就能看到。（Docker 和 Podman 时期的理由相同：Podman 会把 exec 的 Env 持久化，Docker 的 argv 会出现在 inspect 和事件流里。）走 socket 之后，两种 Runtime 都不会让 Secret 落到任何地方。如果等不到 spec，Supervisor 就写 `exit`，cause 为 `start_failed`。
4. fork 后用一条 CLOEXEC 管道判断 execve 是否成功。管道直接 EOF，说明 execve 成功，写 `started`；读到 errno，说明失败，写 `exit`，cause 为 `start_failed`。命令在新的进程组里运行，PTY 模式下用 `setsid` 并分配 pty。
5. 一次性的 `stdin` 写完后关闭。PTY 模式下忽略它（api.md §4）。
6. **主进程退出就算 Exec 结束。** Supervisor 先读完管道里已有的数据，再关掉读端，然后写 `exit`。后台子进程不杀，之后它们再写 stdout 会收到 EPIPE，并被重新挂到 PID 1 下，一直活到 Sandbox 停止。只有超时和取消才杀整个进程组。
7. `oom_killed` 的判定：命令被 SIGKILL，而且在它运行期间容器 cgroup 的 `memory.events` 里 `oom_kill` 计数上涨了。Docker 判 `OOMKilled` 用的是同一个依据。runc 上由 Supervisor 判定；runsc 的客户机里没有这个计数，改由 Agent 读宿主 cgroup 判定（§9）。

## 5. `ctl.sock` 中继

Agent 发给 Supervisor：

- `spec`：只能是第一条消息。
- `cancel`：先给进程组发 SIGTERM，过了 `grace` 再发 SIGKILL，cause 为 `canceled`。
- `input <bytes>`：只用于 PTY。
- `resize <rows> <cols>`：只用于 PTY，对 pty master 设置窗口大小。

Supervisor 发给 Agent：PTY 输出。每个新连接先收到环形缓冲的回放，再收实时流。多个 PTY Attachment 各占一个连接，都能写入。

API 层不提供任意信号，非 PTY 的 stdin 也是一次性的，所以要中继的只有上面这几种。取消是持久意图：Agent 不在时，等它回来再中继。

## 6. Agent 不在时由 Supervisor 计时

- **超时**：从 execve 起算，到点后先发 SIGTERM，过了 `grace` 再发 SIGKILL，cause 为 `timed_out`。`execs.deadline_at` 由 Control Plane 根据上报的 `started_at` 加上 timeout 写入，只用于展示。
- **PTY 空闲**：Supervisor 统计 `ctl.sock` 上的连接数。连接数为 0 并持续到 `pty_idle` 时长，就回收，cause 为 `pty_idle`。Agent 重启时所有连接都会断，Supervisor 的计数自然归零；客户端在空闲期内重连就不会被回收。

## 7. 结果交付与清理

Agent 把 `exit` 作为 `ExecOutput` 流的最后一帧（stream 为 `exit`）推送。Control Plane 写入结果后回 `OutputAck`，确认的 seq 覆盖到这一帧，Agent 就删掉 `run/<exec_id>/`。PTY Exec 也开一条 `ExecOutput` 流，只传这一帧。

## 8. Agent 重启后的对账

Agent 对它掌握的每个 Sandbox 扫描 `run/*`，对每个目录做如下判断：

| 观察到 | 动作 |
|---|---|
| 有 `exit` | 推送剩下的帧和 exit 帧 |
| 没有 `exit`，能连上 `ctl.sock` | Exec 还在跑，从 `stored_seq` 接着读 |
| 没有 `exit`，连不上 socket，Runtime 还活着 | Supervisor 死了。Agent 代写 `exit`，cause 为 `runtime_lost` |
| Runtime 已经没了 | 所有未结束的 Exec，由 Agent 代写 `exit`，cause 为 `runtime_lost`（ADR-0017） |

在 runsc Node 上，Agent 代写的 `runtime_lost` 先按 §9 的 `oom_killed` 规则判一次。

Agent 只接管它 containerd namespace 里带本 Node `orrery.node=<node_id>` 标签的 Runtime 实例；其余带 `orrery.managed` 的容器（例如宿主以新身份重新注册前、上一个身份留下的）直接删掉，连同它的网络（§13）。被接管的 Sandbox 的 netns、veth 和出网规则原样保留，Agent 不重写。不属于本 Node 的容器快照和缓存差异不动，只记一条日志：宣告 lost 可能是误判，那里也许是最后一份没提交的改动。

## 9. containerd

Agent 用 containerd 原生 gRPC 客户端（2.x），所有对象都放在 namespace `orrery` 里。只支持 rootful。containerd 是 Agent 私有的（ADR-0028）：node 包捆绑 containerd、runc 和 runsc，install.sh 以 `orrery-containerd` 服务运行它，socket `/run/orrery/containerd/containerd.sock`、root `/var/lib/orrery-agent/containerd`，配置不读宿主的 `conf.d`、关掉 NRI 和 CRI，不碰宿主的 Docker 或 containerd。`--containerd` 默认就是这个 socket，改它只用于开发和测试；用宿主 containerd 不受支持。每台 Node 只跑一种 Isolation Runtime，由 `--isolation runc|runsc` 选定（ADR-0026）：`runc` 用 containerd 默认的 `io.containerd.runc.v2`，`runsc`（gVisor 的 OCI runtime）用 `io.containerd.runsc.v1`。下面的步骤两者相同，runsc 的差别单列在后面。

- **Create**：先用 Node 的 registry 凭据按 digest 拉取镜像并解包（失败时为 `image_pull_failed`）。再准备容器快照：本地缓存的差异等于 `restore` 时直接复用这个快照，否则在镜像之上新建一个，把 head Generation 恢复进它的 upperdir（§16，失败时为 `runtime_create_failed`）。然后按 §13 建好 netns，再创建容器和 task，配置如下：
  - 标签 `orrery.managed` 和 `orrery.node=<node_id>`（§8）；
  - 进程为 `/.orrery/bin/supervisor init`，覆盖镜像的 entrypoint 和 cmd；init 进程的 stdio 用 `cio.NullIO`，输出本来就走 Supervisor 的记录；
  - 挂载按 §2；
  - 网络命名空间指向 §13 建好的 netns 路径，另加私有的 cgroup namespace；
  - 设置 CPU、内存、PID 限额；cgroup 路径取 containerd 默认的 `/<namespace>/<id>`；
  - GPU 只用 CDI：`pkg/cdi.WithCDIDevices("nvidia.com/gpu=GPU-<UUID>", …)`，不用 `pkg/oci` 里的设备选项。它会往 spec 里加 env、hook 和 mount，所以要排在会重置这些字段的 SpecOpts 之后；registry 用 `cdi.Configure(cdi.WithAutoRefresh(false))` 关掉自动刷新，与 `ctr` 一致。

  task 启动后回读：CPU、内存、PID 上限直接读宿主上的 `/sys/fs/cgroup/<cgroupsPath>/{cpu.max,memory.max,pids.max}`，runc 和 runsc 共用这一个读法（runsc 的客户机里看到的是 Sentry 模拟的 cgroupfs，不能拿来回读）；设备节点由同步执行的 `supervisor probe` 列出。与请求不一致就 `runtime_create_failed`。
- **Exec**：对 task 起一个 exec 进程，不接 stdio，进程为 `supervisor run <exec_id>`，然后按 §4 发送 spec。
- **Inspect**：读 task 状态。Agent 订阅 `/tasks/exit` 事件，再加上周期性扫描，及时发现 Runtime 死亡。
- **Stop**：kill 并删除 task，删除容器，再按 §13 拆掉网络，**保留容器快照**；然后按 §16 提交文件系统、按 §17 提交卷，都成功后才上报 `gone`。快照之后作为缓存留在 Node 上，直到 `DropSandboxDisk` 或磁盘水位淘汰。Runtime 死亡时走同一条提交路径。幂等；对象不存在视为成功。
- **Capabilities**：Node 注册时，用探针容器跑一遍 Create、回读，以及 §13 的出网自检和 Link 自检。哪一项不对，那一项就是 `unsupported`，不另列最低版本表。`kernel_isolation` 在 runsc 上是 `enforced`，在 runc 上是 `unsupported`。

**runsc 的差别**（`docs/research/containerd-cdi-runsc-cni-spike.md`）：

- **runsc 选项**：每个容器经 shim 的 runtime options 传入，不改 containerd 配置：`runtimeoptions.Options{TypeUrl: "io.containerd.runsc.v1.options", ConfigPath: <Agent 写的 TOML>}`，TOML 里 `[runsc_config]` 设 `host-uds = "create"`、`overlay2 = "none"` 和 `file-access = "shared"`。没有前者，Sandbox 里建的 unix socket 在宿主上看不到，PTY 的 `ctl.sock`（§5）和端口转发的 socket（§14）都会失效。代价是 Sandbox 能在它的宿主挂载（运行目录、容器快照）上建出宿主可见的 socket；反方向（连宿主上的 socket，`open`）仍然关闭。没有后者，根文件系统的写入全部封在 upperdir 里一个 `.gvisor.filestore.*` 文件中，§16 扫描不到；代价是根文件系统上写入密集的操作变慢，`git add` 一万多个文件慢约 40%，与今天 bind mount 进来的目录同一量级（`docs/research/runsc-rootfs-visibility-spike.md`）。文件 API 和 Sandbox publish（`api.md` §5）在 runsc 上不读 `/proc/<pid>/root`：那是 gVisor sandbox 进程的根，不是容器的；它们读 shim 挂在宿主上的 rootfs（按快照的 upperdir 在宿主挂载表里找到的 overlay），gofer 的写入就落在这里。`file-access = "shared"` 让 gVisor 不再假定独占根文件系统，宿主一侧（文件 API 上传）的改动 Sandbox 里也能看到；代价是根文件系统上的元数据操作要多一次重新校验。runc 仍读 `/proc/<pid>/root`。
- **GPU**：报 `unsupported`。spec 里一旦有 NVIDIA 设备，runsc 会自己开启 nvproxy，不会报错，所以带 GPU 的 Create 由 Agent 在建容器之前拒绝。
- **PID 上限**：`pids.max` 数的是宿主任务，包括 Sentry 的线程（空闲时约 20 到 51 个）和每个客户机线程对应的 systrap stub，不是客户机进程数 1:1。回读照样准确，上限照样是边界，但同一个数值能容纳的客户机进程更少；撞到上限时整个 Sandbox 退出（Runtime 死亡，Exec 以 `runtime_lost` 结束），而不是客户机里的 `fork()` 返回 `EAGAIN`。平台不为此调整请求的数值。
- **`oom_killed`**（`docs/research/runsc-memory-oom-spike.md`，#177）：客户机里没有 OOM 计数。Sentry 模拟的 cgroupfs 里没有 `memory.events`（v2 的是写死的全 0），Sentry 自己也没有 OOM killer，所以 Supervisor 在客户机里永远判不出 `oom_killed`。撞到内存上限时，宿主 OOM killer 在 Sandbox 的 cgroup 里杀 RSS 最大的那个进程：要么是这个 Exec 的 systrap stub，Sentry 随后以 SIGKILL 结束它，Supervisor 写下 `exited` 加信号 9，Sandbox 继续运行；要么是 Sentry 本身，整个 Sandbox 停止。是哪一种取决于负载，平台不保证。两种都由 Agent 读宿主的 `/sys/fs/cgroup/<cgroupsPath>/memory.events` 来判：
  - **基线**：Agent 连上 `ctl.sock` 之后、发 spec 之前，读一次宿主 `oom_kill`，写进记录目录的 `oom_kill` 文件（§3）。execve 在 spec 之后，所以基线一定早于命令。它和记录一起落在宿主上，Agent 重启后照样能读；没有这个文件就不做下面的改判。
  - **判定**：宿主 `oom_kill` 比基线大，下面两种 `exit` 改成 `oom_killed`：Supervisor 写的 `exited` 且信号为 SIGKILL（Agent 第一次读到这个 `exit` 时读计数并原子改写，之后的重发都用改写后的记录）；Agent 代写的 `runtime_lost`（§8，读计数后直接写成 `oom_killed`）。只看涨没涨，不看涨了多少：一次 OOM 可能杀好几个进程。`canceled`、`timed_out` 等其他 cause 不改。
  - **Sandbox 本身**：Sentry 被杀就是 Runtime 死亡，Sandbox 仍按 ADR-0017 变成 `stopped`，cause 为 `runtime_lost`，不新增 cause。只有其中的 Exec 报 `oom_killed`。
  - **局限**：计数是整个 Sandbox 的，读的时刻是 Agent 处理 `exit` 时而不是命令结束的那一刻，所以同一 Sandbox 里同时被 SIGKILL 的几个 Exec 分不开，与 ADR-0019 的结论相同。宿主 cgroup 读不到（例如 task 已被删）就当作没涨，结果保持 `exited` 或 `runtime_lost`。不设 `memory.oom.group`（它让每次 OOM 都杀掉 Sentry），也不调 Sentry 或 gofer 的 `oom_score_adj`。
- **数据目录**：runsc 执行不了宿主 tmpfs bind 挂载上的 Supervisor 二进制，所以 runsc Node 的 `--data-root` 不能放在 tmpfs 上。Agent 启动时检查，在 tmpfs 上就拒绝启动。

## 10. Local Runtime

Local 上所有 User 共用 Node 配置里的同一个非特权 uid，User 之间没有隔离（文件、信号、loopback 端口都互通），只给可信的内部用户用，与 Coder 的 External workspace 一致（ADR-0009）。以后要升级，就是改成每个 User 一个宿主 uid、工作目录 0700，再加 systemd 加固，接口不变。

- **Create**：先执行 `systemctl set-property --runtime orrery-sb-<id>.slice CPUQuota= MemoryMax= TasksMax=`，再执行 `systemd-run --unit=orrery-sb-<id> --slice=orrery-sb-<id>.slice supervisor init` 起占位进程。占位进程就是这个 Runtime 实例，只要它在，slice 就不会因为空闲被回收。回读直接读宿主上这个 slice 的 cgroup 文件。
- **Exec**：执行 `systemd-run --unit=orrery-exec-<exec_id> --slice=orrery-sb-<id>.slice --uid=<Node 配置的非特权用户> supervisor run <exec_id>`。这是一个瞬态 service，与 Agent 进程无关。Agent 拒绝以 root 运行 Local Exec。瞬态 service 成功退出后，systemd 会直接卸载这个单元，不留退出状态，所以退出状态以 `exit` 文件为准。
- **路径**：没有 §2 的挂载，也没有自己的根文件系统。Sandbox 的工作目录是 `<data-root>/sandboxes/<id>/workspace`，cwd 是它，同时设置 `ORRERY_WORKSPACE` 为这个路径；它就是 Local Sandbox 持久化的文件系统，按 §16 以完整树提交和恢复。卷挂在 `<data-root>/sandboxes/<id>/volumes/<卷名>`，`ORRERY_VOLUMES` 指向这个目录，请求里的 `path` 不生效（§17）；`PATH` 追加的是 Agent 自己的 bin 目录（`node-protocol.md`）；Supervisor 的记录目录是 `<data-root>/sandboxes/<id>/run`，`<data-root>` 即 Agent 的 `--data-root`。
- **Inspect**：看占位 unit 是否 active。
- **Stop**：`systemctl stop orrery-sb-<id>.slice`，停掉 slice 里的所有单元，然后按 §16 提交工作目录。
- **GPU**：每个 `orrery-exec-<exec_id>` 瞬态 service 带 `DevicePolicy=closed` 和 `DeviceAllow`（所分配卡的 `/dev/nvidia<N>`，以及 `/dev/nvidiactl`、`/dev/nvidia-uvm`），同时设置 `CUDA_VISIBLE_DEVICES`。注册时由能力自检验证：通过就报 `enforced`，否则报 `best_effort`。`runtime: local` 的请求接受 `best_effort`（ADR-0009）。
- **Resource 与构建**：没有 `/resources`。Agent 为每个 Sandbox 建目录 `<data-root>/sandboxes/<id>/resources/`，其中每个 `<mount>` 是指向该版本挂载 `<data-root>/resources/<address>` 的符号链接，并设置 `ORRERY_RESOURCES` 为这个目录（§11，ADR-0030）。版本挂载带 `allow_other`，Node 配置里的非特权 uid 能读。`resource_mount` 和 containerd Node 一样由对 `orrery-resourcefs` 的自检决定；`snapshot_build` 由 §12 的自检决定。
- **网络**：没有 netns，进程直接用宿主网络。Egress Rules 不适用于 Local Node，`egress_policy` 报 `unsupported`（ADR-0026）。

## 11. Node 的 Resource 挂载

ADR-0030。Resource 一律懒加载：创建时只读 manifest，对象在读到时才拉取。

- **守护进程**：`orrery-resourcefs`，Agent 部署包里的 systemd 服务（ADR-0028），不随 Agent 重启。每个 ResourceVersion 在 Node 上只挂一个 FUSE 实例，挂在 `<data-root>/resources/<address>`；Agent 再把它只读 bind mount 到容器的 `/resources/<mount>`。`<mount>` 是单段名字，默认取 Resource 名。按引用计数：最后一个引用该版本的 Sandbox 停止（`StopSandbox`，停止和删除共用它）后卸载，再启动时 `CreateSandbox` 重新挂载；守护进程把 manifest 缓存在 `<data-root>/cache/manifests/<address>`，重新挂载不必再读对象存储。Agent 不知道一个已停止的 Sandbox 何时被删除，所以不按删除计数。Agent 重启后，从接管的 Sandbox 的 `<data-root>/sandboxes/<id>/resources/` 符号链接重建引用计数，并卸载不再被引用的挂载。
- **目录树**：按 manifest 在内存里建。nodeid 取所有路径（文件和目录）排序后的位置，根为 1；目录项偏移也取排序位置；文件句柄没有状态。属性、目录项和页缓存都永不过期，因为版本不可变。
- **创建**：`CreateSandbox` 只带 `{address, mount}`。Agent 让守护进程挂载该版本，守护进程经 Agent 的 `Presign` 读 manifest。读不到时按 `defaults.md` 的 manifest 读取退避重试，用尽后以 `create_failed/resource_fetch_failed` 上报，Sandbox 以 `failed` 结束。
- **读取**：读取、缓存和校验的单位是块。不超过 4 MiB 的文件是一个块；更大的文件按 manifest 的 `blocks`（`api.md` §3）。ADR-0030 之前发布的版本里，大文件没有 `blocks`，按区间读整个对象，不做按块校验。
  - 首次读到一块时拉取它，核对 SHA-256，写进块缓存，然后返回数据；同一块的并发请求合并成一次下载。
  - 顺序读时预取后面若干块。
  - 预取块数、全 Node 并发下载上限、单次 GET 超时和重试次数见 `defaults.md`。重试用尽或摘要不符，读取返回 `EIO`。
  - 预签名 URL 由守护进程经本机 unix socket 向 Agent 要，Agent 再调 `Presign`（`node-protocol.md`）。首次访问时按批签这个文件或目录后续的块，缓存到快过期为止。
  - 对象在对象存储上 404 时，Control Plane 把引用它的版本标为 `unavailable`（ADR-0012）。
- **块缓存**：`<data-root>/cache/blocks/<sha256>`，卷（§17）和 Generation 恢复（§16）共用。剩余空间低于磁盘水位时，块、已等于 head 的停止 Sandbox 的缓存差异（§16）、Agent 的 containerd namespace 里未被本 Node 上任何 Sandbox（含 stopped）引用的镜像，以及 buildkitd 的构建缓存（prune），计入同一预算，一起按 LRU 淘汰。挂载中的版本，其块也可以淘汰，再读到时重新拉取。
- **升级**：新的守护进程用 SCM_RIGHTS 从旧进程接过每个挂载的 `/dev/fuse` 会话，连同 INIT 参数。旧进程的读取者处理完手上的请求就退出；挂载空闲时，旧进程查找不存在的随机文件名来唤醒阻塞的读取者（`docs/research/resource-fuse-handover-spike.md`）。整个过程挂载不断。守护进程崩溃时由 systemd 拉起，但已有挂载失效，直到用到它们的 Sandbox 重启。
- **能力**：Agent 自检时连上守护进程，做一次挂载和读取；不通就把 `resource_mount` 报为 `unsupported`。Local Runtime 也一样，它的 Sandbox 经符号链接目录访问挂载（§10）。

## 12. 构建

构建不起容器。Agent 用 BuildKit 的 Go 客户端对一个 BuildKit 端点执行 `Solve`（dockerfile frontend），端点由 Agent 配置 `--buildkit <addr>` 给出（ADR-0021），是独立的 rootful buildkitd，默认指向 install.sh 以 `orrery-buildkitd` 服务运行的捆绑 buildkitd：`unix:///run/orrery/buildkit/buildkitd.sock`（ADR-0028）。它用 OCI worker（`--containerd-worker=false --oci-worker-net host`），不经 containerd；私有 CA 由 install.sh 写进它的 `buildkitd.toml`。

- **能力**：Node 能否构建与它用哪种 Runtime 无关，containerd 和 Local Node 都跑捆绑的 buildkitd。`snapshot_build` 由注册时的自检决定：`--buildkit` 为空、连不上端点或自检失败就报 `unsupported`。
- **资源限制**：Agent 仿照 Local Runtime 建一个带 CPU/内存上限的 slice（`orrery-build-<version_id>.slice`，上限见 `defaults.md`），用 frontend 属性 `cgroup-parent` 指向它。实现时要验证 RUN 进程确实落在这个 slice 里；如果没有，改用 Node 级的构建上限。
- **输入**：没有默认构建上下文。`contexts` 的每一项是 §11 的版本挂载，经 session 的 `LocalMounts` 作为命名上下文 `context:<name>` 提供；`base` 以 `context:base=docker-image://<registry>/…@<digest>` 注入。Dockerfile 的 `FROM` 也可以写外部 `ref:tag` 或 `ref@digest`，由 BuildKit 直接从外部 registry 拉取。
- **凭据**：拉取 `base` 和推送用的 registry 凭据由 session 的 authprovider 提供。
- **Secret**：由 session 的 secretsprovider 提供，Dockerfile 用 `RUN --mount=type=secret,id=<id>` 读取。带 Secret 的构建一律 `no-cache`：secret 不进缓存键，而 buildkitd 的缓存由所有客户端共用，否则别的 User 用同一份 Dockerfile 就能命中缓存，拿到用 Secret 生成的层。
- **输出**：构建输出按帧写进构建 Exec 的 `run/<exec_id>/output`，和普通 Exec 一样上送。成功后推到 `push_repository`，上报 digest 和 BuildKit 实际解析出的 FROM digest。
- **结束**：超时以 `failed/timed_out` 结束；`CancelExec` 中止构建会话，以 `cancelled` 结束；Agent 重启时进行中的构建以 `failed/build_interrupted` 结束，不自动重试。Node 不可达时构建保持 `building`，Node 被宣告 lost 后为 `failed/node_lost`。

## 13. 网络与出网（containerd）

平台拥有每个 Sandbox 的网络（ADR-0026）。只有 IPv4。

- **Sandbox 子网**：每台 Node 一个，由 Agent 的 `--sandbox-subnet` 给出（默认值见 `defaults.md`）。多台 Node 的子网不能重叠，否则 Egress Rules 无法按子网区分 Sandbox；由运维规划，平台不检查。
- **建网**（Create 时，先于容器）：Agent 自己经 netlink 建网，不依赖 CNI 插件、`nft` 或 `ip`：建 netns `/run/netns/orrery-<sandbox_id>`（bind mount）；网桥 `orrery0` 持有子网的第一个地址作网关，并打开 IPv4 转发；一对 veth，宿主侧 `veth<sandbox_id 前 11 位十六进制>` 挂到网桥，另一端在 netns 里叫 `eth0`；netns 里 `lo` 和 `eth0` 启用，`eth0` 配上 Sandbox 地址，默认路由指向网关。地址由 Agent 从 Sandbox 子网分配，跳过网关、广播地址、任何 Sandbox 目录里记着的地址、宿主上任何 `orrery-*` netns 里正在用的地址（含升级前 CNI `host-local` 分出的和同一宿主上别的 Agent 的）和本进程已分出未释放的地址，从上次分出的地址往后找，所以同一地址不会分给两个 Sandbox。建 netns 前 Agent 在数据目录 `netns/<sandbox_id>` 留一个标记，拆网后删掉；启动时没有 `network.json` 的标记是崩溃留下的，Agent 删掉它的 veth 和 netns，地址随之释放。宿主侧 veth 的名字和 Sandbox 的地址记在 Sandbox 目录里，Stop 和对账时用；地址随 `SandboxObserved` 上报（`node-protocol.md`）。nftables 的表经 netlink 下发，每张表一个 batch，整表替换。
- **源地址转换**：不在网桥上做 masquerade，那样会把发往别的 Node 的 Sandbox 子网的流量也转换成 Node 地址，WireGuard 按 AllowedIPs 把它丢掉（spike #183）。Agent 独占一张 `ip` family 的表 `orrery_nat`，`postrouting` 钩子、优先级 `srcnat`，一条规则：`ip saddr <Sandbox 子网> ip daddr != <Sandbox 子网> oifname != "orrery-wg" masquerade`。在更早优先级的 nat 链里 `accept` 挡不住别的 nat 链，所以不能另加一条 masquerade 再去豁免。
- **拆网**（Stop 时，在删除容器之后）：删掉这个 Sandbox 的出网规则，删除宿主侧 veth，清掉以这个地址为原方向源或目的的 conntrack 条目（下一个分到该地址的 Sandbox 不继承它的连接；没有 conntrack 时跳过），释放地址，删除 netns。每一步幂等。runsc 启动后会把 netns 里的地址和路由搬进自己的网络栈，这个 netns 不能复用，所以每次 Create 都重新建。
- **规则表**：Agent 独占一张 `bridge` family 的 nftables 表 `orrery`，三条基础链分别挂 `forward` 钩子（同一网桥上 Sandbox 之间）、`input` 钩子（发往宿主，以及经宿主路由出去的一切）和 `output` 钩子（由网桥自己发给某个 Sandbox 的，包括从 Mesh 路由进来的），优先级 `filter`，`policy accept`，所以不经 Sandbox veth 的流量不受影响。`forward` 和 `input` 最前面先按每个 Sandbox 丢弃伪造的源地址：`iifname "<宿主侧 veth>" ip saddr != <Sandbox 地址> drop` 和 `iifname "<宿主侧 veth>" arp saddr ip != <Sandbox 地址> drop`；地址解析不出时该 veth 的一切都丢弃。Link 的 `ip saddr` 放行和 `ct state established` 都信任源地址，而 runc 的 Sandbox 有 `CAP_NET_RAW`，不先校验就能冒充别的 Sandbox（#249）。每个 Sandbox 一条出网链，`forward` 和 `input` 按 `iifname "<宿主侧 veth>"` 跳进去；每个带 `links` 的 Sandbox 另有一条入站链，`forward` 和 `output` 按 `oifname "<宿主侧 veth>"` 跳进去。`forward` 里所有入站跳转排在所有出网跳转之前：`accept` 会结束整条基础链，出网链在前时，`open` 的 Sandbox 能绕过目标的入站检查（spike #183 §3.3）。不用 `inet` 表：没有 `br_netfilter` 时它看到的入接口是网桥而不是 veth；Agent 也不加载 `br_netfilter`，否则会改变宿主上其他网桥的过滤行为。
- **每个 Sandbox 的链**，按下面的顺序：
  1. `ct state established,related accept`：别人发起的连接，回包照常放行；
  2. ARP 放行，其余非 IPv4 帧丢弃；
  3. 这个 Sandbox 作为来源的 Sandbox Link：`ip daddr <目标地址> [tcp|udp dport <ports>] accept`，排在 Egress Policy 之前（ADR-0027）；
  4. Egress Policy：按 Egress Rules `allow` → Egress Rules `deny` → Egress Mode 的顺序渲染（`open` 全部放行；`none` 全部丢弃；`allowlist` 先放行所列条目，再丢弃其余）。
- **每个 Link 目标的入站链**：`ct state established,related accept`；ARP 放行，其余非 IPv4 帧丢弃；每个来源一条 `ip saddr <来源地址> [tcp|udp dport <ports>] accept`；最后全部丢弃。来源没有地址（不在运行）时不渲染那一条。
- **Link 的生效**：Control Plane 用 `SandboxLinks` 把一端在这台 Node 上的全部 Link 连同两端当前地址整份下发（`node-protocol.md`），任一端地址变化或起停时重新下发；Agent 收到后整张 `orrery` 表重新渲染。重新下发之前，目标只认旧地址，即 fail-closed。目标的入站链在 Create 时按 `CreateSandbox.link_target` 先建好，第一份 `SandboxLinks` 到达之前谁都连不上它。

  Egress Policy 由一个纯函数从（Egress Rules，Egress Mode）算出有序条目，nftables 程序由它的输出渲染。条目是 `CIDR` 时匹配 `ip daddr`，任何协议；带 `:port` 时只匹配该端口的 TCP 和 UDP。
- **生效时机**：Rules 取 Agent 当前持有的那份（`node-protocol.md` 的 `HelloAck`），Mode 取 `CreateSandbox` 里的 `egress`，都在 Create 时渲染。运行中的 Sandbox 不因 Rules 变化而改写；Stop 再 start 后按新 Rules 重建。
- **自检 `sandbox_links`**：注册时用两个探针容器。一个作为目标监听 TCP 端口，只链接另一个；被链接的那个必须连得上，Agent 从宿主（网桥网关地址）连必须连不上；随后把这条 Link 的来源换成一个没人用的地址，并把来源探针的出网放开（`open`），它再连必须超时。都符合才报 `enforced`。
- **自检 `egress_policy`**：注册时在探针容器上做。Agent 在网桥网关地址上临时监听一个 TCP 端口；探针容器先按 Mode `none`（不带 Rules）建网，由 Supervisor 拨这个端口，必须连不上；再按 `allowlist`、只列这个地址和端口建网，必须连得上。两步都符合才报 `enforced`，否则 `unsupported`。自检不用运维的 Rules，结果与它们无关。

## 14. 端口转发（containerd）

Agent 不进入 Sandbox 的网络命名空间（ADR-0026）。`OpenTunnel` 的目标是 `sandbox_port` 时：

1. Agent 先试着连宿主上的 `run/ports/<port>.sock`；连得上就直接用。
2. 连不上就对 task 起一个 detached exec：`supervisor forward <port> /.orrery/run/ports/<port>.sock`。Supervisor 在 Sandbox 里监听这个 socket，每个连接转到 Sandbox 内的 `127.0.0.1:<port>`。同一端口已有转发进程时，新的直接退出。
3. Agent 连上 socket，与 Tunnel 流双向拷贝字节。

runc 和 runsc 走同一条路；runsc 上 socket 在宿主可见，靠的是 §9 的 `host-uds=create`。Local Runtime 没有 netns，Agent 直接拨 `127.0.0.1:<port>`。

## 15. Node Mesh（containerd）

Node 之间的 WireGuard 网络，只承载跨 Node 的 Sandbox Link 流量（ADR-0027）。只有 IPv4。

- **接口**：Agent 自己经 netlink 建内核 WireGuard 接口 `orrery-wg`、写私钥和 peer、加路由，不依赖 `wireguard-tools` 或 `ip`。接口没有地址。私钥在数据目录的 `mesh.key`（0600），第一次启动时生成，之后不变；Node 被删除或重新注册时随身份一起清掉。监听端口为 `--mesh-port`。内核没有 WireGuard 时 Agent 照常运行，`Hello` 不带 `mesh`，这台 Node 没有任何 Mesh 通路。
- **peer**：Control Plane 用 `MeshPeers` 下发其他每台 Node 的公钥、Sandbox 子网和端点（如果有），由 Reconciler 的 sweep 在一台 Node 的 peer 集合变化时发出，所以各份按顺序到达，丢了的一份下一轮重发（`node-protocol.md`）。Agent 把每个 peer 的 `AllowedIPs` 设成对方的 Sandbox 子网，并为它加一条 `<对方子网> dev orrery-wg` 路由；对带端点的 peer 设 keepalive（`defaults.md`）。新的一份逐个 peer 增量应用：不在其中的 peer 被移除，其余的就地更新，不整体替换，所以没变的 peer 保留握手会话和学到的端点，只有配置了端点的 peer 才会被设端点。两台都没有端点的 Node 之间没有通路。Sandbox 子网与本机重叠的 peer 不会下发。
- **过滤**：Agent 在 `orrery_nat` 之外再独占一张 `inet` 表 `orrery_mesh`，`forward` 钩子：从 `orrery-wg` 进来的，只放行 `ct state established,related` 和「来源地址 → 本机 Link 目标地址:端口」；发往 `orrery-wg` 的，只放行 established 和「本机 Link 来源地址 → 目标地址:端口」；其余丢弃。`output` 钩子丢弃宿主自己发往 `orrery-wg` 的一切。这样 Mesh 只承载 Link 流量，与运维的 Egress Rules 无关。条目来自同一份 `SandboxLinks`。
- **MSS**：`orrery_mesh` 在 `forward` 钩子上对经过 `orrery-wg` 的 TCP SYN 做 `tcp option maxseg size set rt mtu`。WireGuard 的封装开销是 60–80 字节，veth 是 1500；大包和 runsc 对 ICMP fragmentation-needed 的处理还没有实测（spike #183 §5）。
- **通路上报**：Agent 每个上报间隔读一次每个 peer 的最近握手时间，发 `MeshReport`。Control Plane 只在内存里保存这张连通矩阵：两台 Node 之间，任一方报告的最近握手在判定时限内（`defaults.md`），就算有通路。Control Plane 重启后矩阵为空，直到下一轮上报；这期间跨 Node 放置只能等待。
- **放置**：Link 的一端只放到与另一端每个已放置 Sandbox 的 Node 相同或有通路的 Node 上，目标和来源谁先放置都一样；候选之中优先另一端所在的 Node（ADR-0027）。Sandbox 不再绑定 Node（§16），这条约束对重新启动的 Sandbox 同样适用。已经运行的 Link 不因通路消失而被停止，只是连不上。
- Local Runtime 没有 netns，也不参加 Mesh。

## 16. Sandbox 文件系统的 Generation

ADR-0031。Sandbox 写进根文件系统的一切（装的包、`/workspace` 里的仓库）停止后保留，删除 Sandbox 时才删，可以在任意 Node 上重新启动。

- **提交的内容**：containerd Sandbox 是容器快照 upperdir 相对 SnapshotVersion 的差异（`kind=diff`）；Local Sandbox 是工作目录的完整树（`kind=tree`）。挂载点下的内容（卷、`/resources`、`/.orrery`、§2 生成的 `/etc` 文件）不在 upperdir 里，不提交。条目记录路径、类型（文件、目录、符号链接、硬链接）、完整 mode、uid/gid、mtime、扩展属性，以及 overlay 的 whiteout（字符设备 0/0，表示删除了镜像里的文件）和 opaque 目录（`trusted.overlay.opaque=y`，表示整个目录被替换）。内容按 Resource 的布局存：不超过 4 MiB 的文件一个 CAS 对象，更大的按 4 MiB 分块（§11，ADR-0030）。
- **扫描**：Agent 在本地保存上次提交的 stat 表（size、mtime、ctime、ino、mode），提交时对 upperdir 做一遍 stat 扫描，只对变化的文件算 SHA-256、上传，流程见 `node-protocol.md` 的「Generation 提交」。同时有 `HEAD` 和 `objects/` 的目录视为 Git 目录，其中的 `*.lock` 不提交。
- **何时提交**：Runtime 结束之后，`StopSandbox` 和 Runtime 死亡都一样。Agent 先报 `committing`，成功后才报 `gone`，所以 `stopping` 要等提交完成，卷的持有也到那时才释放。失败按 `defaults.md` 退避重试，期间 `SandboxObserved.commit_retrying` 为真；upperdir 读不出来时报 `commit_failed`，Sandbox 以 `failed/commit_failed` 结束，本地快照保留到 Sandbox 被删除。Control Plane 以 `INVALID_ARGUMENT` 拒绝条目（例如超过条目上限）同样报 `commit_failed`；`SubmitGeneration` 以 `FAILED_PRECONDITION` 拒绝持有者（Sandbox 已删除、不在本 Node、`generation` 过期）时没有可提交的对象，Agent 不再重试，直接报 `gone`。`CommitGeneration` 发出前 Agent 先把新的 stat 表写成待定，回复丢失或 Agent 重启后先重试同一个 `upload_id`，不在过期的 base 上重做。socket、FIFO 和 whiteout 以外的设备文件没有条目类型，不提交。运行中不提交。
- **恢复**：`CreateSandbox` 带 `restore` 时，Agent 先看本地有没有这个 Sandbox 的快照且 stat 表对应的 Generation 等于 `restore`，有就直接用；否则在镜像之上新建快照，读 `restore` 的 manifest，按条目把对象（从块缓存，缺的经 `Presign` 拉取）写进 upperdir，还原 mode、属主、xattr、whiteout 和 opaque，最后自底向上设 mtime（目录的 mtime 在子项写完之后设），再写下新的 stat 表。恢复后 ino 和 ctime 都会变，第一次 `git status` 会把跟踪文件重读一遍。
- **缓存**：停止 Sandbox 的快照和 stat 表在 Node 上保留，供下一次在这台 Node 启动时复用；等于 head 之后可以按磁盘水位淘汰（`defaults.md`），被淘汰的下次启动时从 Generation 恢复；淘汰后 Agent 报 `disk_dropped`（`node-protocol.md`），Control Plane 清掉 `node_id`，所以放置偏好的总是缓存差异等于 head 的 Node：别处启动过的 Sandbox `node_id` 已是那台 Node，旧 Node 上的差异不再被偏好。`DropSandboxDisk`（Sandbox 删除之后才发）立即删掉它们，连同整个 `<data-root>/sandboxes/<id>`。
- **Node 丢失**：宣告 lost 时，这台 Node 上运行中、`stopping` 和已绑定的 `pending` 的 Sandbox 变成 `stopped`，cause 为 `node_lost`，挂上 `changes_lost`，从各自的 head（没有就从 SnapshotVersion）在别处启动（`api.md` §3）。
- **升级迁移**（迁移 `000035`，ADR-0031「Migration」）：升级前的目录 `<data-root>/workspaces/<id>` 列在 `legacy_dirs` 里。升级时运行中的 Sandbox 保留原来的 bind mount（Local 继续在原目录里跑 Exec），直到停止。之后 Control Plane 把这个 Sandbox、以及挂着对应卷的 Sandbox 固定在目录所在的 Node 上，下一次在那里 `Create` 时 Agent 把目录合并进去：临时的拷进 upperdir 的 `/workspace`（Local 是工作目录），持久的经可写挂载拷进还没有 Generation 的同 ID 卷；只读挂载的记录直接删掉。这次启动之后的提交把它们上传，`legacy_dirs` 的行随之删除，Sandbox 不再固定。Node 被宣告 lost 或删除时它上面的行一并删除，目录按丢失处理。持久目录还没上传时，卷的 `pending_migration` 为真，文件接口看到的是空目录（`api.md` §3）。

## 17. 卷的挂载

ADR-0031。卷不绑 Node，内容是对象存储里的 Generation（`kind=tree`），由 `orrery-resourcefs`（§11）以可写方式挂载。卷用来放数据：FUSE 的写开销是本地盘的 2.5–16 倍（`docs/research/workspace-wsfs-fuse-spike.md`），Git 仓库和构建应放在 Sandbox 自己的文件系统里，平台不拦。

- **挂载**：`CreateSandbox` 的每个 `volumes` 项，Agent 让守护进程挂载该卷的 head Generation（没有时为空目录），挂在 `<data-root>/volumes/<volume_id>/mnt`，再读写 bind mount 到容器里的 `path`（排在 `/workspace` 之后，所以挂在 `/workspace` 的卷盖住它）。同一个卷同一时间只被一个 Sandbox 持有（`schema.md` `sandbox_volumes`），所以一台 Node 上也只有一个挂载。卷已经挂着时（Agent 重启、Create 重发），守护进程保留它的本地状态，除非它既没有未提交的写入、base 也不等于请求的 head（上次 Create 失败留下的，head 已在别处前进），那就重新挂。Agent 把 Sandbox 的卷记在 `<data-root>/sandboxes/<id>/volumes.json`，重启后据此恢复定期提交，停止时据此提交和卸载。
- **读取**：与 §11 相同，按块懒加载、校验、缓存。
- **POSIX 范围**（#278 定）：创建、写、截断、`O_APPEND` 和 `MAP_SHARED` 的 mmap 写（都经内核的写路径），rename（含覆盖已有名字、目录换到别处、`RENAME_NOREPLACE`），硬链接，符号链接，unlink 后仍打开的文件可读写，mode、属主、mtime 和扩展属性。权限由内核按 mode 检查（`default_permissions`）。`flock` 和 `fcntl` 锁由内核处理，只在本 Node 有效（单写者够用）。`RENAME_EXCHANGE` 和设备节点拒绝（`EINVAL`、`EPERM`）；FIFO 和 socket 可以建、可以用，但同 §16 一样不提交。卷根目录 mode 0777、属主 root，不随提交保存。
- **写入**：目录树和每个 inode 的元数据在守护进程内存里，文件内容第一次写（或截断、`fallocate`）时整个复制到本地写缓存 `<data-root>/volumes/<volume_id>/data/<inode>`，之后的读写都走它；没写过的文件仍从块缓存懒读。不直接写对象存储。`fsync` 只把写缓存刷到本地盘，不等提交；元数据只在内存里。目录树一把锁，只保护元数据，持有期间不做磁盘 IO；文件内容的读、写、`fsync` 只和同一个 inode 的截断、复制串行，seal 处理一个文件时只让对这个文件的修改等待，不挡其他文件（`docs/research/volume-vs-juicefs-bench.md` §3）。
- **提交**：有未提交的写入时（守护进程的变更计数大于上次提交的）每 60 秒提交一次 Generation，Sandbox 停止时再提交一次（先于 §16 的文件系统提交），流程同 `node-protocol.md`「Generation 提交」，首批带 `volume_id`，`generation` 取持有它的 Sandbox 的。提交只扫脏集合，取的是一个时间点：守护进程在树锁下复制目录树的元数据并冻结写缓存里的文件，随即放锁；对冻结文件的写等到这个文件处理完，其他文件照常读写。锁外把冻结的文件按 §11 的布局切成对象、算 SHA-256，放到 `<data-root>/volumes/<volume_id>/objects/`，多个文件并行处理，处理完一个解冻一个；最后由快照生成完整的 `kind=tree` manifest。处理完的写缓存文件留作干净副本，inode 同时指向这些对象：再写它不用复制，只是重新变脏；下一次提交时仍没被写过的干净副本删掉，之后再写才再复制一次。`SubmitGeneration` 只送引用这些对象的条目；Agent 从 `objects/` 并发上传缺失的对象（`defaults.md`「Node 并发对象上传」）。`CommitGeneration` 发出前守护进程记下 `upload_id`，回复丢失或 Agent 重启后先重试它；被拒（对象缺失、head 已动）就丢掉它重新提交，只有 `SubmitGeneration` 的 `FAILED_PRECONDITION`（不再持有）才放弃。提交成功后 `objects/` 里的对象移进块缓存，按水位淘汰。提交期间的新写入不丢，留到下一次。停止时卷提交以 `INVALID_ARGUMENT` 被拒，Sandbox 以 `failed/commit_failed` 结束，卷保持挂载、写入留在本地；这个 `failed` 的 Sandbox 继续持有它的卷直到被删除（`schema.md` `sandbox_volumes`），所以没有别的 Sandbox 会悄悄挂上较旧的 head。删除这个 Sandbox 才释放卷；之后挂上它的 Sandbox 若在同一 Node 上，守护进程保留这些写入（见「挂载」），在别的 Node 上则从 head 开始。
- **卸载**：提交成功（或不再持有）后 Agent 让守护进程卸载并删掉 `<data-root>/volumes/<volume_id>/`，之后才报 `gone`。
- **丢失**：Node 丢失或守护进程崩溃，丢掉的是最近一次提交之后的写入；守护进程崩溃后挂载失效，直到 Sandbox 重启，重新挂载时丢弃残留的写缓存。
- **交接**：沿用 §11 的 `/dev/fuse` 交接，未提交的本地状态随会话一起交出（#278 实现，可行）。可写会话不能两个进程同时应答，所以和只读挂载不同：只读挂载发完之后，旧进程逐个卷停止读取、等手上的请求答完（排空），把内存状态（inode、nodeid、内核 lookup 计数、目录项 cookie、base、待确认的 `upload_id`、未提交对象）写到 `<data-root>/volumes/<volume_id>/handover.json`，再连同复制的 fd 交出；写缓存文件原地不动。新进程读这个文件重建，说 ready 之后才开始读。这期间对卷的请求在内核队列里等，不报错；文件句柄不带状态，打开的文件照常可用。新进程在 ready 之前失败时，旧进程用留下的 fd 和自己的状态继续服务。
- **Local Runtime**：挂在 `<data-root>/sandboxes/<id>/volumes/<卷名>`（指向守护进程挂载的符号链接），`ORRERY_VOLUMES` 指向这个目录；挂载带 `allow_other`，Node 配置里的非特权 uid 能读写。
