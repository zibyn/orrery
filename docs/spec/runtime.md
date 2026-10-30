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
| `/workspace` | Workspace | 按 Attachment 的模式，读写或只读 |
| `/.orrery/bin` | Supervisor 二进制和外部 CLI `orrery` | 只读 |
| `/.orrery/run` | 宿主 `/var/lib/orrery/sandboxes/<sandbox_id>/run` | 读写 |
| `/resources/<mount>` | Node 的 Resource 缓存（§11） | 只读 |
| `/etc/resolv.conf`、`/etc/hosts`、`/etc/hostname` | Agent 在 `<data-root>/sandboxes/<sandbox_id>/etc/` 里生成 | 只读 |

`resolv.conf` 取宿主的上游 resolver（宿主用 systemd-resolved 时取 `/run/systemd/resolve/resolv.conf`），去掉回环地址的 nameserver，因为 Sandbox 的回环不是宿主的。这些 nameserver 能不能连通，由 Egress Rules 决定（§13），平台不额外放行。

Node 的部署包同时带外部 CLI。Agent 在自身可执行文件旁边找到 `orrery`，和 Supervisor 一起放进 `/.orrery/bin`。沙箱内的 Git credential helper 和 `orrery token <provider>` 就是它；CA 的信任见 `defaults.md` 的 `access.url`。每个 Exec 的 `PATH` 末尾追加 `/.orrery/bin`，环境变量的拼装见 `node-protocol.md`。Supervisor 把 spec 里的 `env` 和 `secret_env` 原样合进命令的环境，不再改动。

Supervisor 和命令都以镜像默认的 USER 运行，平台不另选 uid。`/.orrery/run` 在宿主上要让这个用户可写。沙箱内的进程能篡改本 Sandbox 的记录。记录不是防 Sandbox 自身进程的边界：Sandbox 本来就属于它的 owner，而记录按 Sandbox 分目录，影响不到别的 Sandbox。

## 3. 记录目录 `run/<exec_id>/`

| 文件 | 内容 |
|---|---|
| `output` | 只追加的帧文件。每帧为 `[1 字节 stream][4 字节长度，大端][data]`，stream 取 `stdout`、`stderr` 或 `truncated` |
| `started` | execve 成功后原子写入（先写 tmp 再 rename），内容是 `started_at` |
| `exit` | 原子写入的 JSON：`{cause, exit_code?, signal?, finished_at}` |
| `ctl.sock` | 控制 socket，见 §5 |

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
7. `oom_killed` 的判定：命令被 SIGKILL，而且在它运行期间容器 cgroup 的 `memory.events` 里 `oom_kill` 计数上涨了。Docker 判 `OOMKilled` 用的是同一个依据。

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

Agent 只接管它 containerd namespace 里带本 Node `orrery.node=<node_id>` 标签的 Runtime 实例；其余带 `orrery.managed` 的容器（例如宿主以新身份重新注册前、上一个身份留下的）直接删掉，连同它的网络（§13）。被接管的 Sandbox 的 netns、veth 和出网规则原样保留，Agent 不重写。Workspace 目录不动，只记一条日志：宣告 lost 可能是误判，那里也许是最后一份数据。

## 9. containerd

Agent 用 containerd 原生 gRPC 客户端（2.x），连 `--containerd` 给出的 socket，所有对象都放在 Agent 自己的 namespace `orrery` 里。只支持 rootful。每台 Node 只跑一种 Isolation Runtime，由 `--isolation runc|gvisor` 选定（ADR-0026）：`runc` 用 containerd 默认的 `io.containerd.runc.v2`，`gvisor` 用 `io.containerd.runsc.v1`。下面的步骤两者相同，gvisor 的差别单列在后面。

- **Create**：先用 Node 的 registry 凭据按 digest 拉取镜像并解包（失败时为 `image_pull_failed`）。然后按 §13 建好 netns，再创建容器和 task，配置如下：
  - 标签 `orrery.managed` 和 `orrery.node=<node_id>`（§8）；
  - 进程为 `/.orrery/bin/supervisor init`，覆盖镜像的 entrypoint 和 cmd；init 进程的 stdio 用 `cio.NullIO`，输出本来就走 Supervisor 的记录；
  - 挂载按 §2；
  - 网络命名空间指向 §13 建好的 netns 路径，另加私有的 cgroup namespace；
  - 设置 CPU、内存、PID 限额；cgroup 路径取 containerd 默认的 `/<namespace>/<id>`；
  - GPU 只用 CDI：`pkg/cdi.WithCDIDevices("nvidia.com/gpu=GPU-<UUID>", …)`，不用 `pkg/oci` 里的设备选项。它会往 spec 里加 env、hook 和 mount，所以要排在会重置这些字段的 SpecOpts 之后；registry 用 `cdi.Configure(cdi.WithAutoRefresh(false))` 关掉自动刷新，与 `ctr` 一致。

  task 启动后回读：CPU、内存、PID 上限直接读宿主上的 `/sys/fs/cgroup/<cgroupsPath>/{cpu.max,memory.max,pids.max}`，runc 和 gvisor 共用这一个读法（gvisor 的客户机里看到的是 Sentry 模拟的 cgroupfs，不能拿来回读）；设备节点由同步执行的 `supervisor probe` 列出。与请求不一致就 `runtime_create_failed`。
- **Exec**：对 task 起一个 exec 进程，不接 stdio，进程为 `supervisor run <exec_id>`，然后按 §4 发送 spec。
- **Inspect**：读 task 状态。Agent 订阅 `/tasks/exit` 事件，再加上周期性扫描，及时发现 Runtime 死亡。
- **Stop**：kill 并删除 task，删除容器，再按 §13 拆掉网络。幂等；对象不存在视为成功。
- **Capabilities**：Node 注册时，用一个探针容器跑一遍 Create、回读和 §13 的出网自检。哪一项不对，那一项就是 `unsupported`，不另列最低版本表。`kernel_isolation` 在 gvisor 上是 `enforced`，在 runc 上是 `unsupported`。

**gvisor 的差别**（`docs/research/containerd-cdi-runsc-cni-spike.md`）：

- **runsc 选项**：每个容器经 shim 的 runtime options 传入，不改 containerd 配置：`runtimeoptions.Options{TypeUrl: "io.containerd.runsc.v1.options", ConfigPath: <Agent 写的 TOML>}`，TOML 里 `[runsc_config]` 设 `host-uds = "create"`。没有它，Sandbox 里建的 unix socket 在宿主上看不到，PTY 的 `ctl.sock`（§5）和端口转发的 socket（§14）都会失效。代价是 Sandbox 能在它的宿主挂载（Workspace、运行目录）上建出宿主可见的 socket；反方向（连宿主上的 socket，`open`）仍然关闭。
- **GPU**：报 `unsupported`。spec 里一旦有 NVIDIA 设备，runsc 会自己开启 nvproxy，不会报错，所以带 GPU 的 Create 由 Agent 在建容器之前拒绝。
- **PID 上限**：`pids.max` 数的是宿主任务，包括 Sentry 的线程（空闲时约 20 到 51 个）和每个客户机线程对应的 systrap stub，不是客户机进程数 1:1。回读照样准确，上限照样是边界，但同一个数值能容纳的客户机进程更少；撞到上限时整个 Sandbox 退出（Runtime 死亡，Exec 以 `runtime_lost` 结束），而不是客户机里的 `fork()` 返回 `EAGAIN`。平台不为此调整请求的数值。
- **`oom_killed`**：Supervisor 在客户机里读到的 `memory.events` 是 Sentry 模拟的。实现时要验证 OOM 时 `oom_kill` 计数会上涨；如果不会，改读宿主 cgroup 的 `memory.events`（§4 第 7 步的依据不变）。
- **数据目录**：runsc 执行不了宿主 tmpfs bind 挂载上的 Supervisor 二进制，所以 gvisor Node 的 `--data-root` 不能放在 tmpfs 上。Agent 启动时检查，在 tmpfs 上就拒绝启动。

## 10. Local Runtime

Local 上所有 User 共用 Node 配置里的同一个非特权 uid，User 之间没有隔离（文件、信号、loopback 端口都互通），只给可信的内部用户用，与 Coder 的 External workspace 一致（ADR-0009）。以后要升级，就是改成每个 User 一个宿主 uid、Workspace 目录 0700，再加 systemd 加固，接口不变。

- **Create**：先执行 `systemctl set-property --runtime orrery-sb-<id>.slice CPUQuota= MemoryMax= TasksMax=`，再执行 `systemd-run --unit=orrery-sb-<id> --slice=orrery-sb-<id>.slice supervisor init` 起占位进程。占位进程就是这个 Runtime 实例，只要它在，slice 就不会因为空闲被回收。回读直接读宿主上这个 slice 的 cgroup 文件。
- **Exec**：执行 `systemd-run --unit=orrery-exec-<exec_id> --slice=orrery-sb-<id>.slice --uid=<Node 配置的非特权用户> supervisor run <exec_id>`。这是一个瞬态 service，与 Agent 进程无关。Agent 拒绝以 root 运行 Local Exec。瞬态 service 成功退出后，systemd 会直接卸载这个单元，不留退出状态，所以退出状态以 `exit` 文件为准。
- **路径**：没有 §2 的挂载。cwd 是 Workspace 在宿主上的路径，同时设置 `ORRERY_WORKSPACE` 为这个路径；`PATH` 追加和 credential helper 用 Agent 自己的 bin 目录（`node-protocol.md`）；Supervisor 的记录目录是 `<data-root>/sandboxes/<id>/run`，`<data-root>` 即 Agent 的 `--data-root`。
- **Inspect**：看占位 unit 是否 active。
- **Stop**：`systemctl stop orrery-sb-<id>.slice`，停掉 slice 里的所有单元。
- **GPU**：每个 `orrery-exec-<exec_id>` 瞬态 service 带 `DevicePolicy=closed` 和 `DeviceAllow`（所分配卡的 `/dev/nvidia<N>`，以及 `/dev/nvidiactl`、`/dev/nvidia-uvm`），同时设置 `CUDA_VISIBLE_DEVICES`。注册时由能力自检验证：通过就报 `enforced`，否则报 `best_effort`。`runtime: local` 的请求接受 `best_effort`（ADR-0009）。
- **Resource 与构建**：`resource_mount` 报 `unsupported`；`snapshot_build` 由 §12 的自检决定。
- **网络**：没有 netns，进程直接用宿主网络。Egress Rules 不适用于 Local Node，`egress_policy` 报 `unsupported`（ADR-0026）。

## 11. Node 的 Resource 缓存

- **布局**：对象按 (sha256, 可执行位) 缓存为只读文件（0444/0555）。版本树用硬链接从对象拼出，再只读 bind mount 到容器的 `/resources/<mount>`。`<mount>` 是单段名字，默认取 Resource 名。
- **拉取**：`CreateSandbox` 只带 `{address, mount}`。Agent 先读 manifest，对缓存里没有的对象调 `Presign` 取 GET（`node-protocol.md`），下载后核对 sha256。拉取期间上报 `fetching_resources`，Sandbox 挂上同名 condition。
- **失败**：重试沿用 Workspace provisioning 那组退避（`defaults.md`）；用尽后以 `create_failed/resource_fetch_failed` 上报，Sandbox 以 `failed` 结束。对象在对象存储上 404 时，Control Plane 把引用它的版本标为 `unavailable`（ADR-0012）。
- **淘汰**：剩余空间低于磁盘水位时，按 LRU 淘汰没有 Sandbox 引用的版本树；版本树都淘汰后，没有链接的对象一并回收。Agent 的 containerd namespace 里未被本 Node 上任何 Sandbox（含 stopped）引用的镜像，以及 buildkitd 的构建缓存（prune），计入同一预算，一起按 LRU 淘汰。

## 12. 构建

构建不起容器。Agent 用 BuildKit 的 Go 客户端对一个 BuildKit 端点执行 `Solve`（dockerfile frontend），端点由 Agent 配置 `--buildkit <addr>` 给出（ADR-0021），是宿主上独立的 rootful buildkitd；没有默认值（ADR-0026）。

- **能力**：Node 能否构建与它用哪种 Runtime 无关，containerd 和 Local Node 配了 buildkitd 就能构建。`snapshot_build` 由注册时的自检决定：没配 `--buildkit`、连不上端点或自检失败就报 `unsupported`。
- **资源限制**：Agent 仿照 Local Runtime 建一个带 CPU/内存上限的 slice（`orrery-build-<version_id>.slice`，上限见 `defaults.md`），用 frontend 属性 `cgroup-parent` 指向它。实现时要验证 RUN 进程确实落在这个 slice 里；如果没有，改用 Node 级的构建上限。
- **输入**：没有默认构建上下文。`contexts` 的每一项经 §11 的缓存拼出版本树，经 session 的 `LocalMounts` 作为命名上下文 `context:<name>` 提供；`base` 以 `context:base=docker-image://<registry>/…@<digest>` 注入。Dockerfile 的 `FROM` 也可以写外部 `ref@digest`。
- **凭据**：拉取 `base` 和推送用的 registry 凭据由 session 的 authprovider 提供。
- **Secret**：由 session 的 secretsprovider 提供，Dockerfile 用 `RUN --mount=type=secret,id=<id>` 读取。带 Secret 的构建一律 `no-cache`：secret 不进缓存键，而 buildkitd 的缓存由所有客户端共用，否则别的 User 用同一份 Dockerfile 就能命中缓存，拿到用 Secret 生成的层。
- **输出**：构建输出按帧写进构建 Exec 的 `run/<exec_id>/output`，和普通 Exec 一样上送。成功后推到 `push_repository`，上报 digest 和 BuildKit 实际解析出的 FROM digest。
- **结束**：超时以 `failed/timed_out` 结束；`CancelExec` 中止构建会话，以 `cancelled` 结束；Agent 重启时进行中的构建以 `failed/build_interrupted` 结束，不自动重试。Node 不可达时构建保持 `building`，Node 被宣告 lost 后为 `failed/node_lost`。

## 13. 网络与出网（containerd）

平台拥有每个 Sandbox 的网络（ADR-0026）。只有 IPv4。

- **Sandbox 子网**：每台 Node 一个，由 Agent 的 `--sandbox-subnet` 给出（默认值见 `defaults.md`）。多台 Node 的子网不能重叠，否则 Egress Rules 无法按子网区分 Sandbox；由运维规划，平台不检查。
- **建网**（Create 时，先于容器）：Agent 建 netns `/run/netns/orrery-<sandbox_id>`，直接调 CNI 插件：`loopback` 配 `lo`；`bridge` 配 `eth0`，网桥 `orrery0`，`isGateway: true`，`ipMasq: true`（出宿主时做源地址转换），IPAM 用 `host-local`，地址取自 Sandbox 子网。宿主侧 veth 的名字由 CNI 结果给出，Agent 记在 Sandbox 目录里，Stop 和对账时用。
- **拆网**（Stop 时，在删除容器之后）：删掉这个 Sandbox 的出网规则，CNI DEL，删除 netns。每一步幂等。runsc 启动后会把 netns 里的地址和路由搬进自己的网络栈，这个 netns 不能复用，所以每次 Create 都重新建。
- **规则表**：Agent 独占一张 `bridge` family 的 nftables 表 `orrery`，两条基础链分别挂 `forward` 钩子（发往同一网桥上别的 Sandbox）和 `input` 钩子（发往宿主，以及经宿主路由出去的一切），优先级 `filter`，`policy accept`，所以不经 Sandbox veth 的流量不受影响。每个 Sandbox 一条普通链，基础链按 `iifname "<宿主侧 veth>"` 跳进去。不用 `inet` 表：没有 `br_netfilter` 时它看到的入接口是网桥而不是 veth；Agent 也不加载 `br_netfilter`，否则会改变宿主上其他网桥的过滤行为。
- **每个 Sandbox 的链**，按下面的顺序：
  1. `ct state established,related accept`：别人发起的连接，回包照常放行；
  2. ARP 放行，其余非 IPv4 帧丢弃；
  3. Egress Policy：按 Egress Rules `allow` → Egress Rules `deny` → Egress Mode 的顺序渲染（`open` 全部放行；`none` 全部丢弃；`allowlist` 先放行所列条目，再丢弃其余）。

  Egress Policy 由一个纯函数从（Egress Rules，Egress Mode）算出有序条目，nftables 程序由它的输出渲染。条目是 `CIDR` 时匹配 `ip daddr`，任何协议；带 `:port` 时只匹配该端口的 TCP 和 UDP。
- **生效时机**：Rules 取 Agent 当前持有的那份（`node-protocol.md` 的 `HelloAck`），Mode 取 `CreateSandbox` 里的 `egress`，都在 Create 时渲染。运行中的 Sandbox 不因 Rules 变化而改写；Stop 再 start 后按新 Rules 重建。
- **自检 `egress_policy`**：注册时在探针容器上做。Agent 在网桥网关地址上临时监听一个 TCP 端口；探针容器先按 Mode `none`（不带 Rules）建网，由 Supervisor 拨这个端口，必须连不上；再按 `allowlist`、只列这个地址和端口建网，必须连得上。两步都符合才报 `enforced`，否则 `unsupported`。自检不用运维的 Rules，结果与它们无关。

## 14. 端口转发（containerd）

Agent 不进入 Sandbox 的网络命名空间（ADR-0026）。`OpenTunnel` 的目标是 `sandbox_port` 时：

1. Agent 先试着连宿主上的 `run/ports/<port>.sock`；连得上就直接用。
2. 连不上就对 task 起一个 detached exec：`supervisor forward <port> /.orrery/run/ports/<port>.sock`。Supervisor 在 Sandbox 里监听这个 socket，每个连接转到 Sandbox 内的 `127.0.0.1:<port>`。同一端口已有转发进程时，新的直接退出。
3. Agent 连上 socket，与 Tunnel 流双向拷贝字节。

runc 和 gvisor 走同一条路；gvisor 上 socket 在宿主可见，靠的是 §9 的 `host-uds=create`。Local Runtime 没有 netns，Agent 直接拨 `127.0.0.1:<port>`。
