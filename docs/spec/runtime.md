# Runtime 适配器与 Exec Supervisor

本文锁定 Docker、Podman 和 Local Runtime 如何实现 ADR-0009 的五个操作，以及 Exec Supervisor 在 Node 上的记录格式和中继方式（ADR-0019）。厂商事实见 `docs/research/runtime-adapter-mechanics.md`。数字默认值以 `defaults.md` 为准。

## 1. 一个二进制，三种模式

Supervisor 是同一个平台二进制，有三种模式：

- `supervisor init`：Runtime 的 PID 1，只负责收尸，然后休眠。它覆盖镜像自带的 entrypoint 和 cmd，所以镜像里不需要 `sleep`，distroless 镜像也能用。Coder 同样把 PID 1 和干活的进程分开；Daytona 把两件事放在一个进程里，结果收尸和取退出码互相抢，只好另写 `childreap` 补救。
- `supervisor run <exec_id>`：每个 Exec 一个，负责启动命令、写记录。命令结束后它自己也退出。
- `supervisor probe`：同步执行，输出容器内的 `cpu.max`、`memory.max`、`pids.max` 和 `/dev/nvidia*` 列表，用来做回读。

## 2. 挂载

容器只有下面这些挂载，没有别的：

| 容器内 | 来源 | 模式 |
|---|---|---|
| `/workspace` | Workspace | 按 Attachment 的模式，读写或只读 |
| `/.stars/bin` | Supervisor 二进制和外部 CLI `stars-sandbox` | 只读 |
| `/.stars/run` | 宿主 `/var/lib/stars-sandbox/sandboxes/<sandbox_id>/run` | 读写 |
| `/resources/<mount>` | Node 的 Resource 缓存（§11） | 只读 |

Node 的部署包同时带外部 CLI。Agent 在自身可执行文件旁边找到 `stars-sandbox`，和 Supervisor 一起放进 `/.stars/bin`。沙箱内的 Git credential helper 和 `stars-sandbox token <provider>` 就是它；CA 的信任见 `defaults.md` 的 `STARS_ACCESS_URL`。每个 Exec 的 `PATH` 末尾追加 `/.stars/bin`，环境变量的拼装见 `node-protocol.md`。Supervisor 把 spec 里的 `env` 和 `secret_env` 原样合进命令的环境，不再改动。

Supervisor 和命令都以镜像默认的 USER 运行，平台不另选 uid。`/.stars/run` 的宿主属主要按 rootful 或 rootless 的映射来设，保证这个用户可写。沙箱内的进程能篡改本 Sandbox 的记录。记录不是防 Sandbox 自身进程的边界：Sandbox 本来就属于它的 owner，而记录按 Sandbox 分目录，影响不到别的 Sandbox。

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
3. Supervisor 监听 `ctl.sock`，等待 Agent 发来第一条 `spec` 消息：`{argv, env, secret_env, cwd, timeout, grace, log_cap, pty?, stdin?, pty_idle}`。规格不放进 exec 的 Env 或 argv，原因有三：Podman 会把 exec 的 Env 持久化到 libpod 状态库；Docker 的 argv 会出现在 inspect 结果和事件流里；`systemd-run -E` 传的环境变量，`systemctl show` 就能看到。走 socket 之后，三种 Runtime 都不会让 Secret 落到任何地方。如果等不到 spec，Supervisor 就写 `exit`，cause 为 `start_failed`。
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

Agent 只接管带本 Node `stars.node=<node_id>` 标签的 Runtime 实例；其余带 `stars.managed` 的容器（例如宿主以新身份重新注册前、上一个身份留下的）直接 `rm -f`。Workspace 目录不动，只记一条日志：宣告 lost 可能是误判，那里也许是最后一份数据。

## 9. Docker 与 Podman

两者共用同一个 Docker Engine API 客户端，区别只是 socket 不同。Podman 的兼容 API 支持 detach exec 和 CDI。

- **Podman 只支持 rootless**。Agent 仍以 root 运行，连接 `/run/user/<uid>/podman/podman.sock`。`/.stars/run` 和 Workspace 在宿主上的属主按 rootless 的 uid 映射设置。端口转发进入网络命名空间时用 inspect 里的 `NetworkSettings.SandboxKey`，不用 PID。
- **Create**：先用 Node 的 registry 凭据按 digest 拉取镜像（失败时为 `image_pull_failed`）。然后创建容器，配置如下：
  - 标签 `stars.managed` 和 `stars.node=<node_id>`（§8）；
  - Entrypoint 设为 `/.stars/bin/supervisor init`，Cmd 留空，`Init: false`；
  - 挂载按 §2；
  - `CgroupnsMode: private` 必须显式传，因为默认值随版本和配置而变；
  - 设置 CPU、内存、PID 限额；
  - GPU 只用 `DeviceRequests[{Driver: "cdi", DeviceIDs: ["nvidia.com/gpu=GPU-<UUID>"]}]`，不用 `HostConfig.Devices`，也不用 `nvidia` driver（Podman 会静默丢弃 `nvidia` driver 的请求）。

  启动容器后同步执行 `supervisor probe`，回读结果与请求不一致就 `runtime_create_failed`。
- **Exec**：exec create 加 start，`Detach: true`，Cmd 为 `supervisor run <exec_id>`，然后按 §4 发送 spec。
- **Inspect**：读 `State.Running`。Agent 订阅 `die` 事件，再加上周期性扫描，及时发现 Runtime 死亡。
- **Stop**：`rm -f`。幂等；容器不存在视为成功。
- **Capabilities**：Node 注册时，用一个探针容器跑一遍 Create 和 `probe` 来自检。哪一项回读不对，那一项就是 `unsupported`，不另列最低版本表。例如 Docker 低于 28.2 时 CDI 默认不开启，自检就会失败；rootless Podman 没有委派 `cpu memory pids` 时，crun 会直接报错。

## 10. Local Runtime

Local 上所有 User 共用 Node 配置里的同一个非特权 uid，User 之间没有隔离（文件、信号、loopback 端口都互通），只给可信的内部用户用，与 Coder 的 External workspace 一致（ADR-0009）。以后要升级，就是改成每个 User 一个宿主 uid、Workspace 目录 0700，再加 systemd 加固，接口不变。

- **Create**：先执行 `systemctl set-property --runtime stars-sb-<id>.slice CPUQuota= MemoryMax= TasksMax=`，再执行 `systemd-run --unit=stars-sb-<id> --slice=stars-sb-<id>.slice supervisor init` 起占位进程。占位进程就是这个 Runtime 实例，只要它在，slice 就不会因为空闲被回收。回读直接读宿主上这个 slice 的 cgroup 文件。
- **Exec**：执行 `systemd-run --unit=stars-exec-<exec_id> --slice=stars-sb-<id>.slice --uid=<Node 配置的非特权用户> supervisor run <exec_id>`。这是一个瞬态 service，与 Agent 进程无关。Agent 拒绝以 root 运行 Local Exec。瞬态 service 成功退出后，systemd 会直接卸载这个单元，不留退出状态，所以退出状态以 `exit` 文件为准。
- **路径**：没有 §2 的挂载。cwd 是 Workspace 在宿主上的路径，同时设置 `STARS_WORKSPACE` 为这个路径；`PATH` 追加和 credential helper 用 Agent 自己的 bin 目录（`node-protocol.md`）；Supervisor 的记录目录是 `<data-root>/sandboxes/<id>/run`，`<data-root>` 即 Agent 的 `--data-root`。
- **Inspect**：看占位 unit 是否 active。
- **Stop**：`systemctl stop stars-sb-<id>.slice`，停掉 slice 里的所有单元。
- **GPU**：瞬态单元带 `DevicePolicy=closed` 和 `DeviceAllow`（所分配卡的 `/dev/nvidia<N>`，以及 `/dev/nvidiactl`、`/dev/nvidia-uvm`），同时设置 `CUDA_VISIBLE_DEVICES`。注册时由能力自检验证：通过就报 `enforced`，否则报 `best_effort`。`runtime: local` 的请求接受 `best_effort`（ADR-0009）。
- **Resource 与构建**：`resource_mount` 报 `unsupported`；`snapshot_build` 由 §12 的自检决定。

## 11. Node 的 Resource 缓存

- **布局**：对象按 (sha256, 可执行位) 缓存为只读文件（0444/0555）。版本树用硬链接从对象拼出，再只读 bind mount 到容器的 `/resources/<mount>`。`<mount>` 是单段名字，默认取 Resource 名。
- **拉取**：`CreateSandbox` 只带 `{address, mount}`。Agent 先读 manifest，对缓存里没有的对象调 `Presign` 取 GET（`node-protocol.md`），下载后核对 sha256。拉取期间上报 `fetching_resources`，Sandbox 挂上同名 condition。
- **失败**：重试沿用 Workspace provisioning 那组退避（`defaults.md`）；用尽后以 `create_failed/resource_fetch_failed` 上报，Sandbox 以 `failed` 结束。对象在对象存储上 404 时，Control Plane 把引用它的版本标为 `unavailable`（ADR-0012）。
- **淘汰**：剩余空间低于磁盘水位时，按 LRU 淘汰没有 Sandbox 引用的版本树；版本树都淘汰后，没有链接的对象一并回收。未被本 Node 上任何 Sandbox（含 stopped）引用的 Docker 本地镜像和 BuildKit 缓存（`builder prune`）计入同一预算，一起按 LRU 淘汰。

## 12. 构建

构建不起容器。Agent 用 BuildKit 的 Go 客户端对一个 BuildKit 端点执行 `Solve`（dockerfile frontend），端点由 Agent 配置 `--buildkit <addr>` 给出（ADR-0021）。`runtime=docker` 时默认用 dockerd 内置的 BuildKit，也可以指向宿主上的 rootful buildkitd。

- **能力**：Node 能否构建与它用哪种 Runtime 无关，Podman 和 Local Node 配了 buildkitd 也能构建。`snapshot_build` 由注册时的自检决定：连不上端点或自检失败就报 `unsupported`。
- **待验证**：实现时先验证 Go 客户端能否经 dockerd 的 `/grpc` 驱动它内置的 BuildKit。如果不行，Docker Node 改走 `ImageBuild` 加 session（Daytona 的做法），输入、凭据和资源的传法不变。
- **资源限制**：Agent 仿照 Local Runtime 建一个带 CPU/内存上限的 slice（`stars-build-<version_id>.slice`，上限见 `defaults.md`），用 frontend 属性 `cgroup-parent` 指向它。实现时要验证 RUN 进程确实落在这个 slice 里；如果没有，改用 Node 级的构建上限。
- **输入**：没有默认构建上下文。`contexts` 的每一项经 §11 的缓存拼出版本树，经 session 的 `LocalMounts` 作为命名上下文 `context:<name>` 提供；`base` 以 `context:base=docker-image://<registry>/…@<digest>` 注入。Dockerfile 的 `FROM` 也可以写外部 `ref@digest`。
- **凭据**：拉取 `base` 和推送用的 registry 凭据由 session 的 authprovider 提供。
- **Secret**：由 session 的 secretsprovider 提供，Dockerfile 用 `RUN --mount=type=secret,id=<id>` 读取。带 Secret 的构建一律 `no-cache`：secret 不进缓存键，而 buildkitd 的缓存由所有客户端共用，否则别的 User 用同一份 Dockerfile 就能命中缓存，拿到用 Secret 生成的层。
- **输出**：构建输出按帧写进构建 Exec 的 `run/<exec_id>/output`，和普通 Exec 一样上送。成功后推到 `push_repository`，上报 digest 和 BuildKit 实际解析出的 FROM digest。
- **结束**：超时以 `failed/timed_out` 结束；`CancelExec` 中止构建会话，以 `cancelled` 结束；Agent 重启时进行中的构建以 `failed/build_interrupted` 结束，不自动重试。Node 不可达时构建保持 `building`，Node 被宣告 lost 后为 `failed/node_lost`。
