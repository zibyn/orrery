# Runtime Adapter 机制核对：Docker / Podman / systemd 的一手事实

背景：Node Agent 驱动 Docker（rootful）、Podman（rootless）与受信任的 Local runtime（systemd）。容器 entrypoint 改写为只读 bind-mount 的平台二进制 Exec Supervisor；命令通过 **detached exec 一个 Supervisor** 启动，输出/退出文件写在 bind-mount 的宿主目录里，所以命令能跨 Node Agent 重启存活。本文只核对这个形状所依赖的运行时机制。

> **取证**：只用官方文档与钉版本的源码，调研日期 2026-10-17。原文保留英文。行号对应下表的钉版本。未实测的写 **[未实测]**。

| 简写 | 版本 | 前缀 |
|---|---|---|
| `M/` | moby `docker-v29.9.1`（API 1.56） | `https://github.com/moby/moby/blob/docker-v29.9.1/` |
| `CLI/` | docker/cli `v29.9.1` | `https://github.com/docker/cli/blob/v29.9.1/` |
| `CD/` | containerd `v2.4.2` | `https://github.com/containerd/containerd/blob/v2.4.2/` |
| `P/` | podman `v6.1.4` | `https://github.com/containers/podman/blob/v6.1.4/` |
| `CM/` | conmon `v2.2.1` | `https://github.com/containers/conmon/blob/v2.2.1/` |
| `NV/` | nvidia-container-toolkit `v1.20.1` | `https://github.com/NVIDIA/nvidia-container-toolkit/blob/v1.20.1/` |
| `K/` | Linux `v6.17` `cgroup-v2.rst` | `https://github.com/torvalds/linux/blob/v6.17/Documentation/admin-guide/cgroup-v2.rst` |
| `SD/` | systemd `v262.1` | `https://github.com/systemd/systemd/blob/v262.1/` |
| `C/` | coder `v2.38.0`（`c3c6a672…`） | `https://github.com/coder/coder/blob/c3c6a672821e8ac2472c45f3be7a3c600aa8dae1/` |
| `DT/` | daytona `v0.190.0` | `https://github.com/daytonaio/daytona/blob/v0.190.0/` |

## 结论表

| # | 问题 | 结论 |
|---|---|---|
| 1a | Docker `Detach: true` 的 exec 在客户端断开/退出后是否还活着 | **活着。** 路由层用 `context.Background()` 调 `ContainerExecStart`，与请求 ctx 无关；进程由 containerd-shim 经 `runc exec` 拉起 |
| 1b | exec `Env` 能否从 `GET /exec/{id}/json` / `docker inspect` 读回 | **读不回。** `ExecInspect` 只返回 `entrypoint/arguments/tty/privileged/user`；容器 inspect 只有 `ExecIDs`。**但 `arguments` 会暴露，而且会写进 `exec_create` 事件**，所以秘密只能放 Env，不能放 argv |
| 2 | Docker CDI 请求字段、版本、默认开启 | `HostConfig.DeviceRequests[{Driver:"cdi", DeviceIDs:["nvidia.com/gpu=GPU-<uuid>"]}]`（CLI 的 `--device <CDI 名>` 就是这样翻译的）。25.0.0 加入；**28.2.0 起默认开启**（release notes；`docker run` 参考页写的是 28.3.0，两页不一致）。`nvidia-ctk cdi generate` 默认**同时**生成 index 名和 UUID 名 |
| 3a | Podman compat API 支持 Detach exec 与 CDI 吗 | **都支持，不必改用 libpod API。** compat exec start 的 `Detach` 走 libpod 的分离启动；compat create 把 `DeviceRequests` 里 `Driver=="cdi"` 的 `DeviceIDs` 转成 CDI 设备。⚠️ 其他 driver 的 DeviceRequest（如 `nvidia`/`--gpus`）**会被静默丢弃** |
| 3b | detached `podman exec` 在客户端退出后是否还活着 | **活着。** conmon 非 sync 模式下 double-fork + `setsid()`，并把自己设为 subreaper |
| 3c | rootless cgroup v2 委派 | 要委派 `cpu`、`memory`、`pids`（需要 cpuset 时再加 `cpuset`）。systemd v262 的 `user@.service` 默认就是 `Delegate=pids memory cpu`。**控制器没委派时会大声失败**（crun：`the requested cgroup controller ... is not available`），不会静默忽略。Podman 6.0 已移除 cgroup v1（v1 上「警告并忽略」的老行为随之消失） |
| 4a | cgroupns=private 下在容器内读 `cpu.max/memory.max/pids.max` | **读得到，且就是该容器 cgroup 自己的值**（cgroupns root 就是容器 cgroup，挂出来就是文件系统根）。⚠️ **祖先 cgroup（比如我们的 Node slice）上的限制看不到**；有效上限是沿祖先链取最小值 |
| 4b | 容器内 `memory.events` 的 `oom_kill`；Docker 怎么定 `OOMKilled` | 读得到，而且是**分层累计**的（本层计数看 `memory.events.local`）。Docker 的 `State.OOMKilled` = shim 监视容器 cgroup 的 `memory.events`，**`oom_kill` 一涨**就发 `TaskOOM` 事件，然后置 true。所以是「容器里任何进程被 OOM 杀过」，不一定是主进程 |
| 5 | systemd：transient slice 限额、`systemd-run --unit` 存活、停 slice | slice 能带 `CPUQuota/MemoryMax/TasksMax`：用 D-Bus `StartTransientUnit("X.slice")`，或者对隐式加载的 slice 用 `systemctl set-property --runtime`（slice 没有单元文件也能加载）。`systemd-run` **本身不能建 slice**，它的 `-p` 作用在 service 上。transient service 的父进程是 PID 1，**调用方退出后照常运行**。**停 slice 会停掉其中所有单元**（单元对 slice 有隐式 `Requires=`） |
| 6 | Coder / Daytona 的 PTY resize、信号、PID 1 | 两家都是「平台二进制当 PID 1 并负责收尸」。**Coder**：检测到自己是 PID 1 时，fork 一个 `--no-reap` 的自己去干活，PID 1 只做 reap 和信号转发。**Daytona**：daemon 直接当 PID 1，在同一进程里跑 reaper，结果和 `cmd.Wait()` 抢子进程退出码，只好专门写了 `childreap` 来补救。resize 两家都是对 pty master 调 `TIOCSWINSZ`。信号：Coder 经 SSH `signal` 请求转发给进程，Daytona 没有信号 API，只有 SIGKILL 整棵进程树 |

---

## 1. Docker exec：detach 存活与 Env 可见性

- **API 契约**：`POST /exec/{id}/start` 原文 *"If detach is true, this endpoint returns immediately after starting the command."*（`M/api/swagger.yaml#L11296-L11330`）。
- **为什么断开不杀**：`ContainerExecStart` 的注释写 *"If ctx is cancelled, the process is terminated."*，而且确实会在 `<-ctx.Done()` 时发 KILL（`M/daemon/exec.go#L161`、`#L319-L330`）。但 HTTP 路由传进去的 ctx 是 `context.Background()`，旁边还留着一句 *"TODO: Maybe we should we pass ctx here if we're not detaching?"*（`M/daemon/server/router/container/exec.go#L139-L147`）。所以**不论 detach 与否**，客户端断开都不会触发 KILL；detach 只决定是否 hijack 连接（`#L105`）。
- **谁是父进程**：dockerd 调 `tsk.Exec(...)` 交给 containerd（`M/daemon/exec.go#L308`）。shim 用 `runc exec --process <tmpfile>` 起进程（`https://github.com/containerd/go-runc/blob/v1.2.1/runc.go#L291-L308`）。containerd 文档：*"The shim process takes responsibility as a sub-reaper to cleanup exited containers or setns(2) processes."*（`CD/core/runtime/v2/README.md`，"containerd-shim-runc-v2 as sub-reaper" 节）。
- ⚠️ **同一节对我们的 PID 1 有直接要求**：*"If the PID namespace is different from shim process, the container init process should cleanup any orphaned reparented processes created by setns process (exec operation)."* 也就是说，exec 出来的进程一旦变成孤儿，会被重挂到**容器的 PID 1（Exec Supervisor）**，由它收尸。
- **dockerd 自己重启**：默认 *"when the Docker daemon terminates, it shuts down running containers"*，要靠 live-restore：*"containers remain running if the daemon becomes unavailable"*（https://docs.docker.com/engine/daemon/live-restore/）。exec 记录只在内存里，`ExecCommands *ExecStore \`json:"-"\``（`M/daemon/container/container.go#L106`）。所以 dockerd 重启后 `GET /exec/{id}/json` 就查不到了，**退出状态只能靠我们自己写的文件**。
- **Env 可见性**：`ContainerExecInspect` 返回的 `ProcessConfig` 只有 `Tty/Entrypoint/Arguments/Privileged/User`（`M/daemon/inspect.go#L199-L240`），没有 Env。Env 只在启动时合并进 OCI process（`M/daemon/exec.go#L257`）。shim 写给 runc 的临时文件里有它，但用完即删：`defer os.Remove(f.Name())`（go-runc `runc.go#L302`）。⚠️ argv 会进事件：`exec_create: <entrypoint> <args...>`（`M/daemon/exec.go#L152`）。**[未实测]** 容器内同 uid 进程、宿主 root 都能读 `/proc/<pid>/environ`，这一条是内核语义，不归 Docker 管。

**结论**：「detached exec Supervisor + 文件落盘」在 Docker 上成立。秘密走 exec `Env` 是 Docker API 面上唯一不回显的通道；argv 里不能放秘密。

## 2. Docker CDI

- **字段**：`DeviceRequest{Driver, Count, DeviceIDs, Capabilities, Options}`（`M/api/swagger.yaml#L337-L370`）。CLI 把合格的 CDI 名翻译成 `DeviceRequest{Driver: "cdi", DeviceIDs: cdiDeviceNames}`（`CLI/cli/command/container/opts.go#L495`、`#L608-L614`）。daemon 端注册的驱动名就是 `"cdi"`（`M/daemon/cdi.go#L35`），CDI 请求不能带 `Count` 或 `Options`（`#L99-L107`）。
- **版本**：25.0.0 *"Add support for CDI devices under Linux."*（https://docs.docker.com/engine/release-notes/25.0/）。28.2.0 *"CDI is now enabled by default."*（https://docs.docker.com/engine/release-notes/28/）。`docker run` 参考页写的是 *"enabled by default since Docker Engine 28.3.0"*（https://docs.docker.com/reference/cli/docker/container/run/），两页不一致。源码以 `features.cdi` 为准，**没设置时视为开启**（`M/daemon/command/daemon.go#L1217-L1226`）。⚠️ swagger 里 `CDISpecDirs` 的说明仍写着要 experimental（`M/api/swagger.yaml#L7358-L7372`），这段已经过时。
- **NVIDIA**：`sudo nvidia-ctk cdi generate --output=/var/run/cdi/nvidia.yaml`。从 v1.18.0 起由 `nvidia-cdi-refresh` 自动生成，但 *"The reconfiguration of MIG devices"* 之后必须手动重新生成（https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/latest/cdi-support.html）。`--device-name-strategy` 的默认值是 `[index, uuid]`：*"If this is specified multiple times, the devices will be duplicated for each strategy"*（`NV/cmd/nvidia-ctk/cdi/generate/generate.go#L148-L154`）。UUID 策略直接用 NVML UUID 作名字（`NV/pkg/nvcdi/namer.go#L82-L97`），所以名字形如 `nvidia.com/gpu=GPU-fef8…`（MIG 是 `MIG-…`）。
- 另外：装了 `nvidia-cdi-hook` 时，`Driver:"nvidia"` 的请求也会内部走 CDI（`M/daemon/devices_nvidia_linux.go#L42-L50`）。我们直接用 `cdi` 驱动，不需要这条路。

**结论**：用 `DeviceRequests[{Driver:"cdi", DeviceIDs:["nvidia.com/gpu=GPU-<UUID>"]}]`，要求 Engine 不低于 28.2。按 UUID 命名不需要额外参数，默认就会生成。

## 3. Podman

- **compat exec detach**：*"If we are detaching, we do NOT want to hijack. Instead, we perform a detached start"* → `sessionCtr.ExecStart(sessionID)`（`P/pkg/api/handlers/compat/exec.go#L177-L188`）。`ExecStart` 原文 *"starts an exec session in the container, but does not attach to it. Returns immediately"*，签名里没有 ctx（`P/libpod/container_exec.go#L271-L274`），往下走到 `ExecContainerDetached`（`P/libpod/oci_conmon_exec_common.go#L155`）。
- **conmon 存活**：*"In the non-sync case, we double-fork in order to disconnect from the parent, as we want to continue in a daemon-like way"*，然后 `setsid()`，以及 *"Set self as subreaper so we can wait for container process"*（`CM/src/conmon.c#L87-L140`）。**[未实测]** 用 `podman system service` 时，service 进程退出不影响 conmon。
- **compat CDI**：`for _, r := range cc.HostConfig.Resources.DeviceRequests { if r.Driver == "cdi" { devices = append(devices, r.DeviceIDs...) } }`（`P/pkg/api/handlers/compat/containers_create.go#L169-L173`）。其他 driver 没有 else 分支，**静默丢弃**。CDI 注入发生在 libpod 生成 spec 的时候：*"GPUs are also handled via CDI."*（`P/libpod/container_internal_common.go#L715-L735`）。`HostConfig.Devices` 会被拼成 `host:container:perms` 字符串（`#L164-L168`），所以 **CDI 名应该走 DeviceRequests，不要塞进 Devices**。
- **Env**：compat exec create 把 `Env` 放进 `ExecConfig.Environment`（`P/pkg/api/handlers/compat/exec.go#L59-L67`）。inspect 结构 `InspectExecProcess` 里没有 env（`P/libpod/define/container_inspect.go#L853-L866`）。⚠️ 但 `ExecSession.Config *ExecConfig \`json:"config"\`` 会**持久化进 libpod 状态库**（`P/libpod/container_exec.go#L54-L56`、`#L93-L110`），也就是 rootless 用户自己的存储目录。这一点和 Docker（只在内存里）不同。
- **rootless 委派**：troubleshooting #26 原文 *"non-root users do not have resource limit delegation permissions. This causes setting resource limits to fail."*，报错 `crun: the requested cgroup controller \`cpu\` is not available`。检查方法是 `cat /sys/fs/cgroup/user.slice/user-$(id -u).slice/user@$(id -u).service/cgroup.controllers`，修法是 `Delegate=memory pids cpu cpuset`（`P/troubleshooting.md#L693-L735`）。systemd v262 自带 `Delegate=pids memory cpu`（`SD/units/user@.service.in#L30`）。6.0.0：*"Support for running on cgroups v1 systems has been removed."*（`P/RELEASE_NOTES.md#L112`）。

**结论**：Podman 用 compat API 就够了（detach exec 和 CDI 都支持）。启动前读 `cgroup.controllers` 预检 `cpu memory pids`；缺了 Podman 会大声失败，不会静默放过。

## 4. 容器内回读有效限额与 OOM

- cgroupns：*"The process running inside the cgroup namespace will have its "/proc/$PID/cgroup" output restricted to cgroupns root. The cgroupns root is the cgroup of the process at the time of creation"*（`K#L2940-L2946`）。在 ns 内挂 cgroup2：*"This will mount the unified cgroup hierarchy with cgroupns root as the filesystem root."*（`K#L3066-L3073`）。所以容器内 `/sys/fs/cgroup/memory.max` 就是容器 cgroup 那一层的文件。Docker 的 `CgroupnsMode` 默认值 *"depending on daemon version, kernel support and configuration"*（`M/api/swagger.yaml#L1127-L1139`），**要显式传 `private`**。
- ⚠️ **只看得到这一层**：*"A child can only consume up to the configured amount of the resource. Limits can be over-committed"*（`K#L679-L690`）。挂在祖先 slice 上的限制在容器内看不见，那里读到的是 `max`。
- `memory.events`：*"all fields in this file are hierarchical ... For the local events at the cgroup level see memory.events.local."*；`oom_kill`：*"The number of processes belonging to this cgroup killed by any kind of OOM killer."*（`K#L1450-L1499`）。
- Docker `OOMKilled`：shim 的 v2 OOM watcher 在 `i.ev.OOMKill > lastOOM` 时发布 `TaskOOMEventTopic`（`CD/pkg/oom/v2/v2.go#L60-L85`），dockerd 收到 `EventOOM` 就置 `c.State.OOMKilled = true`（`M/daemon/monitor.go#L191-L206`）。API 原文：*"Whether a process within this container has been killed because it ran out of memory since the container was last started."*（`M/api/swagger.yaml#L6120-L6123`）。

**结论**：Supervisor 可以在容器内读 `cpu.max/memory.max/pids.max` 来回读本层限额，读 `memory.events` 的 `oom_kill` 判断 OOM，判据和 Docker 自己一样。但如果平台在 Node slice 上另加了上限，这个值得由 Node Agent 在宿主侧读。**[未实测]** rootless Podman 下容器 cgroup 是否就是 cgroupns root，与 crun 和 cgroup manager 有关，上线前应探针确认。

## 5. systemd（Local runtime）

- **transient service 存活**：*"It will run in a clean and detached execution environment, with the service manager as its parent process. In this mode, systemd-run will start the service asynchronously in the background and return after the command has begun execution"*（`SD/man/systemd-run.xml#L69-L74`）。⚠️ *"units that ran successfully are unloaded immediately"*（`#L545-L552`），所以成功退出后 systemd 侧不留退出状态，要么用 `RemainAfterExit`（`SD/docs/CONTROL_GROUP_INTERFACE.md#L222`），要么靠我们自己的退出文件。
- **slice 限额**：*"To create and start a transient (scope, service or slice) unit ... use the StartTransientUnit() method"*；*"Resource limits may be set on services, scopes and slices the same way."*（`SD/docs/CONTROL_GROUP_INTERFACE.md#L178`、`#L200`）。`systemd-run` 只建 service/scope/path/socket/timer（`SD/man/systemd-run.xml#L64-L67`），`--slice=` 只负责把 service 放进那个 slice（`#L161-L170`）。`systemctl set-property` 原文 *"many resource control settings ... may. The changes are applied immediately, and stored on disk for future boots, unless --runtime is passed, in which case the settings only apply until the next reboot."*（`SD/man/systemctl.xml#L687-L712`）。没有单元文件的 slice 也能加载：`unit_load_fragment_and_dropin(u, false)`（`SD/src/core/slice.c#L167`）。⚠️ transient 单元 *"will be released as soon as it is not running or referenced anymore"*（`SD/man/org.freedesktop.systemd1.xml#L1731-L1733`），**[未实测]** 空 slice 是否会被回收、限额是否随之丢失；用 `set-property --runtime` 写 drop-in 可以绕开这个问题。
- **停 slice**：*"Units with the Slice= setting set automatically acquire Requires= and After= dependencies on the specified slice unit."*（`SD/man/systemd.resource-control.xml#L169-L171`）。`Requires=` 的语义是 *"this unit will be stopped (or restarted) if one of the other units is explicitly stopped"*（`SD/man/systemd.unit.xml#L683-L692`）。

**结论**：每个 Workspace 一个 slice（用 `set-property --runtime` 或 `StartTransientUnit` 带上 `CPUQuota/MemoryMax/TasksMax`），每条命令一个 `systemd-run --unit=… --slice=…` 的 transient service。调用方退出不影响命令；`systemctl stop X.slice` 会停掉整个 Workspace。

## 6. Coder v2.38.0 与 Daytona v0.190.0

**Daytona daemon**
- 形状和我们一样：runner 把 daemon 只读挂进容器，`binds = append(binds, fmt.Sprintf("%s:%s:ro", d.daemonPath, common.DAEMON_PATH))`，把 entrypoint 改成 daemon，原 entrypoint 降为 cmd（`DT/apps/runner/pkg/docker/container_configs.go#L143-L163`、`#L189`）。
- PID 1：*"The daemon runs as PID 1 inside containers, so orphaned processes (e.g. from process.exec) get reparented here."* → `childreap.Start()`（`DT/apps/daemon/cmd/daemon/main.go#L225-L230`）。⚠️ 踩过的坑写在包注释里：*"Those two calls race: whichever syscall the kernel dispatches first wins, and the other returns ECHILD. When the reaper wins, cmd.Wait() reports a non-ExitError error and the daemon loses the real exit code"*（`DT/apps/daemon/pkg/childreap/childreap.go#L5-L17`）。
- 收到 SIGINT/SIGTERM 只关掉 toolbox server，不向子进程转发（`main.go#L232-L245`）。
- PTY：`pty.StartWithSize`，resize 用 `pty.Setsize`，cols/rows 上限 1000（`DT/apps/daemon/pkg/toolbox/process/pty/session.go#L64`、`#L245-L265`）。没有信号 API，kill = 深度优先 SIGKILL 整棵子树，并且要在 shell 退出之前做，否则子孙进程会被重挂到 PID 1 而漏杀（`#L125-L194`）。

**Coder agent**
- PID 1：`if reaper.IsInitProcess() && !noReap && isLinux` 时 fork 一个 `--no-reap` 的自己，PID 1 只负责 reap 并转发 `StopSignals`（Interrupt/SIGTERM/SIGHUP），然后传递子进程退出码（`C/cli/agent.go#L131-L164`、`C/cli/signal_unix.go#L10-L16`）。reaper 用 `reap.ReapChildren(... opts.ReapLock)`，子进程带 `Setsid: true`（`C/agent/reaper/reaper_unix.go#L63-L103`）。**这样 reap 和 Wait 在两个进程里，不会出现 Daytona 那种竞争。**
- resize：SSH 的 window-change 走 `ptty.Resize(uint16(win.Height), uint16(win.Width))`（`C/agent/agentssh/agentssh.go#L805-L815`）。reconnecting PTY 是 JSON 消息里 `Height/Width` 非 0 就 resize（`C/agent/reconnectingpty/reconnectingpty.go#L287-L296`）。
- 信号：SSH `signal` 请求经 `handleSignal` 发给**进程本身**（不是进程组）（`agentssh.go#L711-L719`、`#L859-L868`）。reconnecting PTY 没有信号字段，Ctrl-C 作为数据走 tty。非 PTY 命令在会话结束时**不杀**：*"to match OpenSSH, we don't actually tear a non-TTY command down"*（`#L674-L678`）。

**结论**：Exec Supervisor 当 PID 1 必须收尸。**照 Coder 拆成两个进程**（PID 1 只做 reap 加信号转发，干活的是子进程），就能避免 Daytona 遇到的 reap 和 `Wait` 抢退出码的问题。resize = 对 pty master 调 `TIOCSWINSZ`。取消要按进程组或子树去杀，并且趁父进程还活着时做。
