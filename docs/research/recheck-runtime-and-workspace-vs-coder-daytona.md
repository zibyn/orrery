# 重查 #30（ADR-0009 Runtime Provider）与 #32（Workspace 位置与生命周期）：对照 Coder 与 Daytona

调研日期 **2026-10-17**。只用一手来源：官方文档与钉死版本的源码；厂商原文保留英文。没有做任何实测。

| 项目 | 钉死 | 简写 |
|---|---|---|
| coder/coder | tag v2.38.0，commit `c3c6a672821e8ac2472c45f3be7a3c600aa8dae1` | `C/` = `https://github.com/coder/coder/blob/c3c6a672821e8ac2472c45f3be7a3c600aa8dae1/` |
| daytonaio/daytona | tag v0.190.0（公开仓库 2026-06 冻结，见 `daytona-sandbox-runtime-semantics.md` §0.1） | `D/` = `https://github.com/daytonaio/daytona/blob/v0.190.0/` |

复用的既有调研：`daytona-sandbox-runtime-semantics.md`（下称 **DSR**）、`prior-art-sandbox-orchestration.md`（**PA**）、`control-plane-operations-vendor-practice.md`（**CPO**）、`sandbox-isolation-and-gpu.md`（**SIG**）、`container-sandbox-architecture.md`、`persistence-vendor-practice.md`（**PVP**）、`decisions-vs-vendor-practice.md`（**DVP**）。这些文件里已经有的事实，这里只给出处，不再取证。

---

## 先读这几条

1. **两家都在沙箱里放了一个进程监管者，我们没有。** Daytona 让 daemon 当容器的 entrypoint，它从 runner 宿主以只读 bind mount 进容器（`D/apps/runner/pkg/docker/container_configs.go#L143-L189`）；命令输出写进沙箱里的 `output.log`，由 daemon 提供（DSR §4.2）。Coder 让 agent 的 `init_script` 当容器的 entrypoint（`C/examples/templates/docker/main.tf#L182`），PTY 由 agent 内的 reconnecting PTY 维持，后端是 `screen` 或环形缓冲（`C/agent/reconnectingpty/reconnectingpty.go#L102-L104`）。ADR-0009 的 `Exec` 是 Node Agent 从容器外 `docker exec`，输出走 Node Agent 持有的 hook。Docker 不能重新接上一个已经启动的 exec，这是 moby 长期未解决的 issue（<https://github.com/moby/moby/issues/9527>）。所以 ADR-0014 说的「Agent 重启后收养容器」，对**正在跑的 Exec 的输出和 PTY**不成立。**应修改**，是本文风险最大的一条。
2. **Workspace 写 Lease 的 TTL 和续租，与 ADR-0014、#33 相冲突，两家也都没有这个东西。** Coder 的共享工作区允许多人同时 `use`，不加写锁（`C/docs/user-guides/shared-workspaces.md#L82-L84`）。Daytona Volume 明写 *"the last write wins"*（DSR §1.6）。我们的 Lease 带 TTL，靠 Node 流上的心跳续租（#31），过期后由 Node Agent 隔离写者、杀掉写进程。可 ADR-0014 规定 Control Plane 停机或 Node Unreachable 时 *"running Execs … are not failed"*。两条规则会在同一个场景下打架。Workspace 绑死在一个 Node 上，能写它的只有那台 Node 的 Node Agent，TTL 在这里防不住任何东西。**应简化**：保留「至多一个读写 Attachment」，删掉 TTL 和续租。
3. **磁盘：我们按预留记了账，却不强制执行。Daytona 两件事都做了。** #34 把 Workspace 和临时磁盘算作硬预留，ADR-0009 却把 disk capacity quota 在三个后端上都写成 `unsupported`。Daytona 的做法：在 XFS 上用 `StorageOpt` 限额（DSR §1.8），runner 上报磁盘使用率并计入调度分数（`D/apps/api/src/sandbox/services/runner.service.ts#L1115-L1125`），另外专门写了一条「撞到存储上限后恢复」的路径（`D/apps/runner/pkg/docker/recover_from_storage_limit.go#L14-L16`）。我们这边，一个 Agent 写满宿主盘，会连带同一 Node 上的所有持久 Workspace 和 Node Agent 的 Log Spool（Spool 写不进就是 `log_storage_failed`）。**应修改**。
4. **Runtime Provider 的 `Stop`/`Destroy` 两段式，加上「Destroy 运行中返回 `conflict`」，比两家都复杂，又没换来什么。** Daytona 的 `Destroy` 对任何状态都强删（`D/apps/runner/pkg/docker/destroy.go#L94-L106`）。Coder 删除运行中的工作区，只要求当前没有正在进行的 build（`C/coderd/wsbuilder/wsbuilder.go#L1383`）。我们的 Provider 没有 `Start`，停掉的 Runtime 永远不会再被启动，留着它没有用处。**应简化**：`Stop` 直接把 Runtime 删掉，Workspace 保留，与 Coder 的 `start_count = 0` 同义。
5. **「Node 级的永久绑定」与 Coder 的 Docker 模板一致，可以保留。Daytona 在这件事上吃过亏，后来靠备份迁移补上。** Daytona 的沙箱 stop 时做一次备份，运行中每小时做一次，用的是 `docker commit` 后推到 registry。runner 不可调度或不在 READY 时，下次 start 就在别的 runner 上从备份恢复，源码见 `D/apps/api/src/sandbox/managers/sandbox-actions/sandbox-start.action.ts#L473-L541` 和 `D/apps/api/src/sandbox/managers/backup.manager.ts#L131-L157`。Phase 1 不做迁移，这个选择与 Coder 一致，站得住。但是 ADR-0014 规定「不驱逐、不强制排空」，Workspace 又只能在 Node 被宣告 lost 时才离开，结果是**一台带持久 Workspace 的 Node 没有退役路径**。**保留，补一条退役路径**（见 W2）。
6. **永久失败没有出口。** Coder 有 `failed` 状态，还有一个 orphan delete 逃生口。源码注释把原因写明了：provisioner 被下线以后 *"these workspaces cannot be deleted"*，而且绕过 build 直接删 *"would result in no audit log entry, which is a deal-breaker"*（`C/coderd/wsbuilder/wsbuilder.go#L549-L558`）。Daytona 有 `error` 状态和 `recoverable` 字段，重试也是有上限的（`D/apps/api/src/sandbox/managers/backup.manager.ts#L727-L733`）。我们写的是「provisioning 失败不设 failed 状态，由 Reconciler 重试」「`deleted` 要等 Provider 确认清理后才报」，两处都没给重试上限，也没给管理员留出口。**应修改（小）**。

---

## 逐条裁决对照表

编号：R* 对应 #30 / ADR-0009，W* 对应 #32。判定分三种：保留、应简化、应修改。

| # | 裁决 | Coder | Daytona | 判定 |
|---|---|---|---|---|
| R1 | Provider 是 Node Agent 内部的适配器，只有六个操作：Create/Exec/Inspect/Stop/Destroy/Capabilities；不含文件、Git、调度和出网 | 没有对应的固定契约。provisioner 只是一个 `Session` 流，里面跑 Init/Parse/Plan/Apply/Graph，资源全部由 Terraform 模板定义（`C/provisionersdk/proto/provisioner.proto#L569-L594`） | runner API 有 create/info/destroy/start/stop/backup/snapshot/resize/recover/network-settings（`D/apps/runner/pkg/api/server.go#L136-L146`），**没有 exec** | **保留**：比两家都小，CRI 也是多数派做法（DVP #10） |
| R2 | `Exec` 由 Node Agent 在容器外调用 Provider 实现，输出走 hook | 命令在**工作区里的 agent** 中执行，SSH 和 reconnecting PTY 都在 agent 内（`C/agent/reconnectingpty/reconnectingpty.go#L102-L123`） | 命令在**沙箱里的 daemon** 中执行，daemon 是 entrypoint（`D/apps/runner/pkg/docker/container_configs.go#L143-L189`），输出落成文件（DSR §4.2） | **应修改**：见下文 R2 |
| R3 | `Exec` 接收 argv，不隐式套 shell | 走 SSH 命令字符串，由 shell 解释【推断：SSH 协议本身如此】 | 命令通过 stdin 喂给 shell（DSR §4.1） | **保留**：argv 严格包含 shell 写法，要 shell 就传 `["sh","-c",…]` |
| R4 | `Stop` 幂等且保留 Workspace；`Destroy` 只对已停止或不存在的 Runtime 幂等，对运行中返回 `conflict` | stop 的含义是计算资源 `count = start_count` 变成 0，**容器被删掉**，volume 保留（`C/examples/templates/docker/main.tf#L174-L190`）；delete 不要求先 stop（`C/coderd/wsbuilder/wsbuilder.go#L1375-L1390`） | `Destroy` 任何状态都可以，运行中就 `Force: true` 删（`D/apps/runner/pkg/docker/destroy.go#L58-L114`）；`Stop` 先 SIGTERM 等 10 秒，失败再 KILL（`D/apps/runner/pkg/docker/stop.go#L34-L44,L74-L98`） | **应简化**：见下文 R4 |
| R5 | `Exec` 绝不隐式启动或重建 Runtime | 用户连接时可以自动启动工作区，前提是开启了 autostart（`C/docs/user-guides/workspace-lifecycle.md#L81-L82`） | SDK 没有隐式启动（DSR） | **保留**：Vercel 的反例见 PA §8 #2 |
| R6 | 公共面只有 `/workspace` 一个读写挂载；不开放任意宿主挂载、host network、特权和自选 uid/gid | 模板想挂什么都行；官方 Docker 模板非特权，也不设资源限制（`C/examples/templates/docker/main.tf`） | 非 GPU 容器默认 `Privileged: true`（`D/apps/runner/pkg/docker/container_configs.go#L196-L206`） | **保留**：比两家都严，而且是产品目标（不可信 Agent）要求的 |
| R7 | Docker/Podman 只接受不可变 digest；Local 在收到 digest 时返回 `unsupported` | 由模板决定 | 镜像必须带 tag 或 digest，不接受 `latest`（DSR §1.6） | **保留** |
| R8 | 共同的硬限制是 CPU、内存、PID；IO、磁盘、显存不在共同保证内 | 官方 Docker 模板一个资源限制都不设 | CPU 和内存走 cgroup；磁盘在 XFS 上走 `StorageOpt`（DSR §1.8）；没找到 `PidsLimit` | **应修改**（磁盘这一项）：见下文 R8 |
| R9 | GPU：调度器分配到具体设备，Provider 只负责挂上或拒绝；走 CDI | 由模板决定 | runner 本地用标签加锁分配，走 CDI（DSR §1.7） | **保留**：形状一样，只是分配点放在了控制面 |
| R10 | Capabilities 三值（enforced/best_effort/unsupported）；required 的能力必须 enforced；注册时上报快照，每次 Create 前**重查关键能力** | 没有能力对象，只有 provisioner tag 做静态 key=value 匹配（`C/docs/install/operate/provisioners/index.md#L128-L160`） | 也没有能力对象，只有 `sandboxClass` 加一张文档矩阵（DSR §3）；runner 每 10 秒上报健康和用量（`D/apps/api/src/sandbox/services/runner.service.ts#L698-L700`） | **保留三值；「Create 前重查」应修改**为「Create 后回读实际生效值」，见下文 R10 |
| R11 | 错误归一成 6 类 | — | runner 靠**错误文本子串匹配**来决定要不要自动恢复（`RECOVERY_ERROR_SUBSTRINGS`，`D/.../sandbox-start.action.ts#L566-L574`） | **保留**：Daytona 正好是反例 |
| R12 | Local Runtime 是真实存在的低隔离后端 | agent 可以直接跑在宿主机上【推断：未逐页核对】 | 没有 | **保留**：DVP §3.3 已经裁定 |
| W1 | Workspace 是任意文件树，可以放 0..N 个 Git 仓库；Repository 是另外的元数据 | home volume 是任意目录 | 沙箱文件系统是任意目录，Git 操作按路径来（DSR §2.2） | **保留** |
| W2 | 创建时选定 Node，从此**永久绑定**；Phase 1 不迁移、不跨 Node 同步 | Docker 模板的 `docker_volume` 留在那台 Docker 宿主上，迁移交给基础设施和 Terraform（`C/docs/admin/templates/extending-templates/resource-persistence.md#L9-L15`） | **会迁移**：沙箱带 `runnerId` 和 `prevRunnerId`（`D/apps/api/src/sandbox/entities/sandbox.entity.ts#L77-L85`），归档后 `runnerId = null`（`#L447`），从备份恢复到新 runner | **保留，补一条退役路径**：见下文 W2 |
| W3 | `ephemeral`：没有 Attachment 时进入宽限期，然后删除；`persistent`：只能显式删除；持久性只到 Node 本地，不做容灾 | 没有内置的 ephemeral；靠 autostop、dormancy、auto-delete（`C/docs/user-guides/workspace-lifecycle.md#L84-L97`） | 有四个计时器加一个 TTL，ephemeral 等于 `autoDeleteInterval = 0`（DSR §1.4）；auto-stop 只看外部交互，所以长任务会被停掉 | **保留**：我们只有两档，而且以 Attachment 为准，避开了 Daytona 那个坑 |
| W4 | Workspace 不属于 Sandbox；可以有多个只读 Attachment，最多一个读写 Attachment | 计算和数据分离靠 `start_count` 加持久 volume；共享工作区不加写锁 | Volume 独立于沙箱，可以多处挂载，*"last write wins"*，不能当块存储（DSR §1.6） | **保留**：「至多一个读写」只是一条数据库约束，便宜 |
| W5 | 所有修改共用一个写 Lease，带平台控制的 TTL、续租和单调 fencing token；过期的写者由 Node Agent 隔离，它的写进程被杀掉 | 没有 | 没有 | **应简化**：见下文 W5 |
| W6 | Lease 冲突在 attach 时直接报出，不在队列里藏着 | 已有 build 在跑时直接返回 409（`C/coderd/wsbuilder/wsbuilder.go#L1383`） | 卷还挂着时删除返回 409（DSR §1.6） | **保留** |
| W7 | 状态：provisioning/ready/unavailable/deleting/deleted/lost；**不设 durable failed**，由 Reconciler 重试 | 有 `failed` 和 `unhealthy`（`C/docs/user-guides/workspace-lifecycle.md#L31-L36`；`C/codersdk/workspacebuilds.go#L28-L37`） | 有 `error` 加 `recoverable`，重试有上限（`D/.../sandbox.entity.ts#L113-L117,L456-L462`；`D/.../backup.manager.ts#L727-L733`） | **应修改（小）**：见下文 W7 |
| W8 | Node 丢失先变 unavailable；只有管理员确认后才变 lost | agent 状态在读取时由时间戳推导，没有 lost 概念（CPO §1e） | runner 60 秒没有健康上报就标 UNRESPONSIVE（`D/.../runner.service.ts#L698-L716`），上面的沙箱在下次 start 时迁走 | **保留**：ADR-0014 已定，我们没有迁移，就不能用计时器去判 lost |
| W9 | API 只用稳定的 Workspace ID 加不透明的 Node 侧句柄，不暴露宿主路径 | 名字从可变属性派生就会被 Terraform 重建并**清空数据**，官方要求用不可变 ID（`C/docs/admin/templates/extending-templates/resource-persistence.md#L55-L67`） | 用沙箱 ID | **保留**：Coder 的坑证明这条是对的 |
| W10 | 删除是异步、幂等的；Provider 确认清理后才报 `deleted`；Node 不可用时删除意图留待对账 | delete 是一次 `terraform destroy` build，失败就停在 failed；有 **orphan delete** 作逃生口，而且仍然写审计（`C/coderd/wsbuilder/wsbuilder.go#L549-L558`；`C/codersdk/workspaces.go#L126-L127`；`C/docs/user-guides/workspace-lifecycle.md#L99-L106`） | 删除幂等，NotFound 当作成功（`D/apps/runner/pkg/docker/destroy.go#L49-L56`）；runner 上还有未销毁的沙箱时不下线（`D/.../runner.service.ts#L721-L770`） | **应修改（小）**：见下文 W10 |
| W11 | 确认 lost 的 Workspace 保留墓碑和审计，从用户可见的元数据里移除 | orphan delete 也要求留审计（同上） | — | **保留** |
| W12 | 职责划分：Control Plane 管语义，Node Agent 管放置和隔离，Provider 只做被授权的本地操作；Gateway、SDK、CLI 不另立语义 | coderd 与 provisionerd、agent 同形 | API 与 runner、daemon 同形 | **保留** |

---

## 需要展开的条目

### R2：Exec 在容器外，没有沙箱内的监管者（应修改）

**两家怎么做**
- Daytona 的 daemon 就是容器的 entrypoint，用户自己的 entrypoint 作为它的 `cmd` 传进去（`D/apps/runner/pkg/docker/container_configs.go#L143-L173`）。daemon 二进制从宿主以只读方式 bind 进容器（同文件 `#L189`）。命令的 stdout/stderr 经 FIFO 写进 `{configDir}/sessions/{id}/{cmdId}/output.log`，退出码写进 `exit_code` 文件（DSR §4.2）。runner 本身不提供 exec 路由（`D/apps/runner/pkg/api/server.go#L136-L146`）。
- Coder 的官方 Docker 模板把 agent 的 `init_script` 设为 entrypoint（`C/examples/templates/docker/main.tf#L182`）。reconnecting PTY 的注释写的是 *"a pty that can be reconnected within a timeout and to simultaneous connections. The reconnecting pty can be backed by screen if installed or a (buggy) buffer replay fallback."*（`C/agent/reconnectingpty/reconnectingpty.go#L102-L104`）。缓冲后端是一个 64 KiB 的环形缓冲（`C/agent/reconnectingpty/buffered.go#L20-L54`）。

**我们为什么和两家都不一样**
ADR-0009 的 `Exec` 带着 *"optional stdin/output hooks"*，hook 挂在 Node Agent 进程上。Docker 的 exec 一旦 start，就不能再接回去：API 只有 create、start、resize、inspect 四个动作，没有 attach（<https://github.com/moby/moby/issues/9527>，社区给的绕法也正是 tmux 或 screen）。

**后果【推断】**
ADR-0014 规定 Node Agent 升级就是 `SIGTERM` 后重启。重启以后容器还在，但每个正在跑的 Exec：
- 输出从重启那一刻起永久丢失。Node Agent 的 Log Spool 根本收不到这部分字节，没有东西可以补；
- PTY Attachment 不能重新接上，与 GLOSSARY 里「重新 attach 只是观察同一次执行」相矛盾；
- 进程能不能活下来，取决于运行时怎么处理被断开的 stdio，比如会不会收到 SIGPIPE。这一点没有实测。

#33 让 Agent 重启后按「Provider 确认仍在运行就回到 running」来收敛，但输出这条链断了，Exec 实际上已经没法用。

**没有找到一手理由说明两家为什么选择放沙箱内 agent**。Coder 讲过 agent 的用途是「无论 OS、架构、云，都能让 coderd 和客户端连到工作区」（PA §1.10），但没有专门讨论过进程存活问题。我的推断是：两家都需要「控制进程重启、断线之后命令照常跑、日志还能续上」，而运行时的 exec API 给不了这个。

**建议（最小改法）**
`Exec` 改成在容器内启动一个平台二进制 shim，形如 `docker exec <shim> -- argv…`。这个 shim 以只读方式挂进容器，做法同 Daytona；它和 ADR-0011 的 Capability Channel 一样，属于平台管理的具名挂载。shim 负责：
- 把 stdout、stderr、PTY 输出和退出码写到每个 Attempt 一个的宿主目录里，这个目录是平台管理的挂载，不放在 `/workspace` 下；
- Node Agent 只是这些文件的读者，重启后按偏移量续读。

PTY 需要 shim 自己持有 pty master。这条改动会落到 ADR-0009（Exec 语义）、#33（Spool 的来源）、ADR-0014（「收养」的范围）三处。Local Runtime 的子进程本来就随 Agent 一起结束（ADR-0014），这条不受影响。

### R4：Stop/Destroy 两段式（应简化）

- Coder 的 stop，对计算资源来说就是删除：*"When `start_count` is 0, `count` is 0, so no `docker_container` is created."*（`C/docs/admin/templates/extending-templates/resource-persistence.md#L31-L40`）。工作区文档写的是 *"Stopped: Ephemeral resources destroyed, persistent resources idle"*（`C/docs/user-guides/workspace-lifecycle.md#L28`）。
- Daytona 保留已停止的容器，因为沙箱的文件系统**就是**容器的 rootfs，停止以后还要再 start 回来（DSR §1.5）。它的 Destroy 不检查状态，直接强删。
- 我们的 Workspace 在 `/workspace`，Snapshot 不包含任何 Workspace 状态，Provider 也没有 `Start`。#34 写的是「stopped 只保留 Workspace 和元数据，重新启动须重新 admission」，重新启动走的是一次新的 `Create`。所以停掉的那个容器不会再被用到。「运行中 Destroy 返回 conflict」只是逼 Reconciler 先调一次 Stop，换不来任何安全性。
- **两家为什么没有「Destroy 前必须先 Stop」**：未找到一手理由。我的推断是：删除本来就隐含停止，分成两步只会多出一种竞态（Stop 和 Destroy 之间 Runtime 被别的什么拉起来）。
- **建议**：Provider 改为 `Create / Exec / Inspect / Stop / Capabilities` 五个操作。`Stop` 的语义是优雅终止，然后删除 Runtime；它幂等、保留 Workspace，宽限期沿用 #33 的 5 秒。删掉 `Destroy` 和 `conflict` 规则。如果想保留停止后的容器用于排障，可以给它设一个短保留期，但 Phase 1 不需要。

### R8：不强制磁盘配额（应修改）

- Daytona 的磁盘限额只在 `d.filesystem == "xfs"` 时设置 `StorageOpt{"size"}`（DSR §1.8），这个机制和 SIG §1.1 核到的 overlay2 加 XFS pquota 是同一个。runner 上报 `CurrentDiskUsagePercentage` 和已分配的磁盘（`D/apps/runner/pkg/api/dto/info.go`），调度分数会用到这两个值（`D/apps/api/src/sandbox/services/runner.service.ts#L1115-L1125`）。撞到磁盘上限有专门的恢复动作，每次扩 5%，最多扩 10%（`D/apps/runner/pkg/docker/recover_from_storage_limit.go#L14-L34`）。「磁盘写满」是他们实际碰到过的故障，所以才有这些代码。
- Coder 的官方 Docker 模板不设任何限制。
- 我们的处境比两家都差：#34 把磁盘当硬预留来记账，但没有任何执行手段，而且 Workspace 是 bind mount，`StorageOpt` 只管容器 rootfs，管不到它。
- **建议（二选一，或两个都做）**
  - ① 每个 Node 加一条磁盘水位线：Node Agent 上报 Workspace 所在文件系统的剩余空间，低于阈值时，这台 Node 停止接收新的分配，并且醒目地告警。代价很小，与 Daytona 的上报方式同形。
  - ② 如果 Node 的 Workspace 根目录在 XFS 上且开了 `prjquota`，就给每个 Workspace 目录设项目配额，`disk capacity quota` 在这种 Node 上报 `enforced`。这和 Daytona「仅 xfs 时才设」是同一种条件化做法。

### R10：「Create 前重查关键能力」（应修改，轻）

两家都没有在每次创建前探测能力。Daytona 靠 runner 周期上报健康，再加上 class 过滤。Daytona 文档还建议从沙箱内部回读 `/sys/fs/cgroup/cpu.max`、`memory.max` 来核实限制（DSR §3.6）。真正的风险是运行时**静默忽略**某项限制，比如 cgroup 控制器没有委派时。预检查发生在 Create 之前，抓不住这种情况。**建议**：去掉 Create 前的预检查，改成 Create 之后由 Provider 回读实际生效值（cgroup 文件、设备节点），任何一项不符就按 `runtime_failed` 销毁并报错。注册时的快照照旧保留，给调度器用。

### W2：永久绑定 Node（保留，补一条退役路径）

- Coder 不管迁移，交给模板：*"Coder templates have full control over workspace ephemerality."*（`C/docs/admin/templates/extending-templates/resource-persistence.md#L9`）。在 Docker 模板里，volume 就留在那台宿主上。
- Daytona 原来也是绑定在 runner 上的，后来补了迁移。源码注释写的是 *"if the runner is unschedulable/not ready and sandbox has a valid backup, move sandbox to a new runner"*，还有 *"temp workaround to move sandboxes to less used runner"*（`D/apps/api/src/sandbox/managers/sandbox-actions/sandbox-start.action.ts#L486-L506`）。备份的来源是：每次 stop 做一次（`backup.manager.ts#L498-L520`），运行中至少每小时做一次，ephemeral 的沙箱除外（`#L131-L157`）；做法是 `docker commit` 加 push（`D/apps/runner/pkg/docker/backup.go`）。runner 上还有任何未销毁的沙箱，就不允许下线（`D/.../runner.service.ts#L721-L770`）。
- **我们为什么不学 Daytona**：Daytona 的整份文件系统就是容器 rootfs，`commit` 一下就打包走了。我们的 Workspace 是 bind mount，要迁移就得另造一条 Workspace 打包路径，而 ADR-0012 明确规定 Snapshot 不包含 Workspace。Phase 1 不做这件事，与 Coder 一致，可以接受。
- **缺的一块**：ADR-0014 规定「不驱逐、不强制排空」，Workspace 只有在 Node 被宣告 lost 时才离开。带持久 Workspace 的 Node 一旦要退役，结局只有「数据丢失」这一种。**建议**在规格里写一句退役做法：用户用 Exec 把 Workspace 打成 tar，声明成 Artifact，再在新 Node 上的新 Workspace 里还原。这条路完全由现有部件拼成，不需要新机制。

### W5：写 Lease 的 TTL、续租和 fencing token（应简化）

- **两家都没有**写 Lease：Coder 的共享工作区里，*"`use` allows for connection via SSH and apps…"*，多人可以同时写（`C/docs/user-guides/shared-workspaces.md#L82-L84`）；Daytona Volume 是 LWW。**为什么不做，两家都没有给出一手理由**。我的推断是：两家里修改工作区内容的进程都在一个 POSIX 文件系统上，并发写交给用户和 Git 自己处理。
- 我们想要单写者，理由成立：多个 Agent 同时改同一个 Git 工作树会把 index 弄坏。「至多一个读写 Attachment」靠一条部分唯一索引就能实现，这条保留。
- **TTL 和续租的问题**
  - 能写这个 Workspace 的只有它所在 Node 的 Node Agent（W2）。下一个写者也必须在同一台 Node 上，经由同一个 Node Agent 获准。所以 Node Agent 自己就能排出先后：先终止旧写者，再放新写者进来。用不着一个「过期后自动失效」的时间界限。
  - TTL 靠 Node 流上的心跳续租（#31）。Control Plane 停机、或者 Node Unreachable 超过 TTL 时，Node Agent 必须自我隔离，杀掉写进程。这与 ADR-0014 的 *"Unreachable stops new placement only; running Execs … are not failed"*、*"A Control Plane outage pauses scheduling… running containers continue"* 直接冲突。
- **建议**
  - 写权限就等于持有当前的读写 Attachment。它只在三种情况下被撤销：显式撤销、Sandbox 进入 stopping 或 deleting、管理员操作。删掉 TTL 和续租。
  - Attachment 带一个代数号，就是行版本，几乎零成本。ADR-0011 让 Node Agent 本地核对的「fencing token」就用这个代数号。
  - 被撤销时，终止**持有读写 Attachment 的那个 Sandbox 里的全部 Exec**。读写挂载下的任何 Exec 都可能写盘，按 Exec 区分「是否依赖写」是做不到的。
  - #34 的 Compute Lease 也有同样的 TTL 问题，不在本文范围内，记一笔待查。

### W7：没有 durable failed（应修改，小）

Coder 的工作区状态里有 `failed`（`C/codersdk/workspacebuilds.go#L33`），文档把 *"Failed: Failure during provisioning"* 当作一种状态（`C/docs/user-guides/workspace-lifecycle.md#L31-L36`）。Daytona 有 `error`，失败的重试都有上限：restore 最多 3 次，backup 也有 *"Bound the retries so a runner stuck reporting NONE doesn't loop forever"*（`D/apps/api/src/sandbox/managers/backup.manager.ts#L727-L733`）。

我们「不加 failed 状态」这一点可以保留，与 Kubernetes 的 conditions 风格一致。但是必须补两件事：
- 重试要有指数退避和次数上限，超过上限就挂一个可见的条件，比如 `provisioning_stalled`，停止自动重试，等人处理；
- `provisioning` 状态下随时可以删除。

### W10：删除没有逃生口（应修改，小）

Coder 的 orphan delete 是因为踩过坑才加的，原文：*"There are cases where tagged provisioner daemons have been decommissioned without deleting the relevant workspaces, and without any provisioners available these workspaces cannot be deleted."*，还有 *"deleting without a workspace build or provisioner job would result in no audit log entry, which is a deal-breaker."*（`C/coderd/wsbuilder/wsbuilder.go#L549-L558`）。

我们这边，Node 整个没了可以走「宣告 lost」（ADR-0014）。可如果 Node 还活着，只是 Provider 的清理一直失败（目录删不掉、存储坏了），Workspace 就会永远停在 `deleting`。**建议**加一个 `admin` 动作：放弃清理，并写一条 Audit Record。效果是把 Workspace 记成 `deleted`，在 Node 侧留一条「待人工清理的句柄」。这和 Coder 的 orphan delete 同形。

---

## 未查 / 未确认

- 没有实测 Docker 和 Podman 下 Node Agent 进程退出后，`exec` 出来的进程会怎样（会不会 SIGPIPE、会不会被收割）。Podman 的 exec session 由 conmon 托管，可能比 Docker 好一些，这一点**未查**。
- Coder 能不能把 agent 当作宿主进程直接运行（对应我们的 Local Runtime），没有逐页核对，只是推断。
- Daytona 的源码是 2026-06 的冻结快照，线上行为可能已经变了（DSR §0.1）。
- 两家「为什么不做写锁」「为什么 Destroy 不要求先 Stop」，都**没有找到一手理由**，上文的说法是推断。
