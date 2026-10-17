# Runtime 适配器与 Exec Supervisor

本文锁定 Docker、Podman 和 Local Runtime 如何实现 ADR-0009 的五个操作，以及 Exec Supervisor 在 Node 上的记录格式和中继方式（ADR-0019）。厂商事实见 `docs/research/runtime-adapter-mechanics.md`。数字默认值以 #44 为准。

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
| `/.stars/bin` | Supervisor 二进制 | 只读 |
| `/.stars/run` | 宿主 `/var/lib/stars-sandbox/sandboxes/<sandbox_id>/run` | 读写 |
| `CreateSandbox.resources[].mount` | Node 的 Resource 缓存 | 只读 |

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
- 写到上限后，追加一条 `truncated` 帧，之后的输出丢弃。命令照常运行（ADR-0015）。
- PTY 的输出不写进 `output`，而是放在 Supervisor 内存中的环形缓冲里（ADR-0017）。

## 4. 启动与结束

1. Agent 用 detached exec 启动 `supervisor run <exec_id>`，exec 的 Cmd 和 Env 里不带任何规格。
2. Supervisor 执行原子的 `mkdir run/<exec_id>`。遇到 `EEXIST` 就立即退出，不跑命令。这样即使 StartExec 被重复下发，命令也不会跑两次。
3. Supervisor 监听 `ctl.sock`，等待 Agent 发来第一条 `spec` 消息：`{argv, env, secret_env, cwd, timeout, grace, pty?, stdin?, pty_idle}`。规格不放进 exec 的 Env 或 argv，原因有三：Podman 会把 exec 的 Env 持久化到 libpod 状态库；Docker 的 argv 会出现在 inspect 结果和事件流里；`systemd-run -E` 传的环境变量，`systemctl show` 就能看到。走 socket 之后，三种 Runtime 都不会让 Secret 落到任何地方。如果等不到 spec，Supervisor 就写 `exit`，cause 为 `start_failed`。
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

## 9. Docker 与 Podman

两者共用同一个 Docker Engine API 客户端，区别只是 socket 不同。Podman 的兼容 API 支持 detach exec 和 CDI。

- **Create**：先用 Node 的 registry 凭据按 digest 拉取镜像（失败时为 `image_pull_failed`）。然后创建容器，配置如下：
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

- **Create**：先执行 `systemctl set-property --runtime stars-sb-<id>.slice CPUQuota= MemoryMax= TasksMax=`，再执行 `systemd-run --unit=stars-sb-<id> --slice=stars-sb-<id>.slice supervisor init` 起占位进程。占位进程就是这个 Runtime 实例，只要它在，slice 就不会因为空闲被回收。回读直接读宿主上这个 slice 的 cgroup 文件。
- **Exec**：执行 `systemd-run --unit=stars-exec-<exec_id> --slice=stars-sb-<id>.slice --uid=<Node 配置的非特权用户> supervisor run <exec_id>`。这是一个瞬态 service，与 Agent 进程无关；cwd 是 Workspace 在宿主上的路径。Agent 拒绝以 root 运行 Local Exec。瞬态 service 成功退出后，systemd 会直接卸载这个单元，不留退出状态，所以退出状态以 `exit` 文件为准。
- **Inspect**：看占位 unit 是否 active。
- **Stop**：`systemctl stop stars-sb-<id>.slice`，停掉 slice 里的所有单元。
- **GPU**：只靠 `CUDA_VISIBLE_DEVICES`，属于 `best_effort`（ADR-0009）。
