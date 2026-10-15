# 既有 agent 沙箱与工作区编排的先行形态调研

> 对应 issue #4（wayfinder:research）。结论服务于后续 grilling：`#6` Worker 注册与身份、`#7` 任务模型与下发语义、`#8` 状态回报与断线恢复、`#9` Workspace 生命周期、`#10` Runtime Provider、`#11` 沙箱调度器放置与 GPU 表达、`#12` Credential Broker、`#16` 安全与出网边界。
>
> 每条断言附一手来源 URL。无法从一手来源证实的写 **[未证实]**，不猜测。⚠️ 标记表示该处先例**与我们的既定形状相抵触**。

---

## 0. 先读这三条

1. **几乎没有人做自拨号 Worker。** Fly 把 `flyd` 放在宿主上；Cloudflare 必须有一个 Durable Object 才能碰到容器；Vercel、E2B 由控制层驱动；Modal 直接 RPC worker；Daytona 的 runner 是**轮询**。它们都能这么做，是因为它们拥有到机器的网络路径。真正有"沙箱侧主动拨号"的只有 **Coder** 和 **Buildkite**，而这两家恰恰都是为**你不拥有的机器**造的。**这两个是我们最该精读的先例，其余主要是反面教材与词汇来源。**

2. **大家一致的两级对象模型，我们已经有了。** Vercel 说得最干净：Sandbox 是"跨多次 VM 启动存活的长期实体"，Session 是"其中一个运行中的 VM 实例"（[persistent-sandboxes](https://vercel.com/docs/sandbox/concepts/persistent-sandboxes)）。我们的 **Worker = sandbox（稳定身份，跨重连存活）**，**任务的 Workspace 运行 = session（有界，有终止记录）**。分开命名是防止"worker 死了"和"任务失败"塌缩成同一个 bug 的唯一办法。

3. **唯一被所有先例用同一句话否定的东西：内存快照。** E2B、Modal、AWS、Blaxel、Fly 从五个不同方向得出同一结论——机器极其庞大、收益极窄。**要做就做文件系统级，或者不做。**

---

## 1. Agent 沙箱产品

### 1.1 E2B

**怎么做的**：Firecracker microVM 一沙箱一 VM；后端是开源的 Go（[e2b-dev/infra](https://github.com/e2b-dev/infra)）。**控制层决定"在哪"、节点编排器决定"怎么做"**——这句话是他们自己写在架构文档里的（[ARCHITECTURE.md](https://github.com/e2b-dev/infra/blob/main/docs/ARCHITECTURE.md)）。控制层经 gRPC `:5008` 拨节点：`SandboxService`、`TemplateService`、`InfoService`，其中 `InfoService` 上报节点身份、角色、**`outstanding_work`** 与 **`max_sandboxes`**，供 API 做节点发现。放置是 best-of-K：采样 K 个健康节点，按 CPU 负载与 hugepage 池负载的较高者打分取最低。状态回流两条路：SDK 回调流（`onStdout`/`onStderr`，每 50s keepalive），以及**生命周期事件的 REST 轮询**（`GET /events/sandboxes/{id}`，类型 `sandbox.lifecycle.{created,paused,resumed,updated,snapshotted,killed}`，保留 7 天）。pause 同时保存文件系统与内存，代价约 **4 秒/GiB**；fork 一次最多 20 个。凭据用 `Secret.fill('name')` 返回**引用**，出口代理在沙箱**外面**替换真值。

**该抄**
- 那句"API 决定在哪、节点决定怎么做"，写进我们自己的文档（[ARCHITECTURE.md](https://github.com/e2b-dev/infra/blob/main/docs/ARCHITECTURE.md)）。
- **`outstanding_work` + `max_sandboxes` 进注册/心跳报文**：Worker 自己告诉控制层它还能接多少，调度器不用猜。
- **`execution_id` 守卫陈旧生命周期**（陈旧的生命周期不得删除更新的那条记录）。对我们就是：Worker 重连开新流时控制层还握着旧流——**注册报文里放单调递增的 epoch，拒绝来自陈旧 epoch 的结果**。这是我们最可能撞上的 1 号并发 bug。
- 显式生命周期事件类型 + 小的 `eventData`。六个名字 + 可空载荷，回答"这个任务为什么死的"。
- **fail-open 还是 fail-closed 是一个要写下来的决定**。E2B 明说：标记未解析就整体丢掉该 header 并继续转发（[secrets/inject](https://docs.e2b.dev/secrets/inject.md)）。
- keepalive 配"没有任何单条连接保证超过一小时、重连是调用方的责任"的契约（[faq/sandbox-lifetime](https://docs.e2b.dev/faq/sandbox-lifetime.md)）。

**不该抄**
- ⚠️ **内存快照。** 4s/GiB 的 pause，以及为它存在的 `userfaultfd` + NBD + COW overlay + 脏页 diff 整套机器。内网几台服务器上这是几个月的工程量，换来一个更好看的暂停。
- best-of-K 放置 + 可在线调的 feature flag：几百节点才有意义。
- fork。
- **在 VM 里再放一个独立 agent（envd）走自己的端口和自己的 RPC。** 他们需要是因为控制层离得远、流量必须能进 VM；**我们的 Worker 就是那个 agent，而且已经有流了——不要再加第二条通道。**
- REST 轮询生命周期事件。我们有流，往流上推。

### 1.2 Daytona

**怎么做的**：三个平面（interface / control / compute）。控制层是 NestJS REST API，**快照构建器与沙箱管理器作为内部进程跑在 API 里**，外加 Redis + PostgreSQL + Auth0。计算层的 **runner「轮询控制层 API 取作业」**（[architecture](https://daytona.io/docs/en/architecture/)）——方向与 E2B 相反（worker 拉），但性质相同：控制层从不需伸进 worker 的网络。状态回流是**每命令一条 WebSocket**：`wss://proxy.app.daytona.io/toolbox/{sandboxId}/process/session/{sessionId}/command/{commandId}/logs?follow=true`，同一 URL 去掉 `follow` 就是一次性日志快照 HTTP GET（[log-streaming](https://www.daytona.io/docs/en/log-streaming.md)）。凭据是他们规范得最精确的地方：组织级 secret，创建时以「环境变量名 → secret 名」传入，变量里拿到**占位符 `dtn_secret_<random>`**；代理只在**白名单 host** 上替换真值，其他目的地原样转发；**响应方向反向擦洗**，防止上游回显泄漏；轮换值时**占位符不变**，存量沙箱 15 秒内生效（[secrets](https://www.daytona.io/docs/en/secrets.md)）。

**该抄**
- 占位符 + host 白名单这个**取舍的表述**："同一个 secret 在不同沙箱里可以挂到不同的变量名下"、"沙箱里拿着一个除了你点名的 host 之外哪儿都用不了的值"。
- **响应方向擦洗**——做代理替换时所有人都会忘的一步。
- **同一条资源的两种交付模式**（`?follow=true` 跟随 / 不带即一次性）。API 设计上很干净。
- warm pool 作为**思路**而非特性：如果 Workspace 预热（clone 仓库、装依赖）主导了任务延迟，每个仓库保一个热工作区是小的、高价值的想法。

**不该抄**
- ⚠️ **热快照（文件系统 + 内存）只给 VM 档。** 同样的内存快照复杂度，外加把产品切成"容器档做不了、VM 档能"——三个后端各自一张支持矩阵。
- **把快照仓库做成 OCI distribution registry**（聪明的去重与标准工具，但整个子系统换来的东西，用对象存储里的一个 tar 包 50 行就有了）。
- NestJS/Postgres/Redis/Auth0/PostHog 那套栈：多租户 SaaS 合理，不是内网小控制层的模型。
- 每命令一条 WebSocket + 自建代理 URL。我们只有一条双向流，把命令多路复用上去。
- warm pool 的"任何定制（env/volume/secret）即失配"约束。

### 1.3 Modal

**怎么做的**：沙箱是容器，runtime 可 `gvisor` 或 VM 二选（⚠️ GPU 沙箱**只能** gvisor，[guide/sandbox](https://modal.com/docs/guide/sandbox)）。最有价值的是他们重构调度的复盘：Kubernetes 式集中调度的问题是算法 O(n×p)、调度被串行化、etcd 在 keyspace 内不可分片；于是改成 **worker 自己是 source of truth**（发布状态到 Redis stream）+ **一组并行调度服务读内存缓存**（"更像负载均衡而不是传统调度"）+ **direct worker RPC**——调度器挑一个 worker 并调用它，**"Worker 有空闲资源就接受调度请求，否则拒绝"**。关键路径是两次网络跳 + 一次 CPU 操作、**没有数据库写**。结果是调度在几十毫秒、一分钟内创建 100 万沙箱、用户代码中位 <0.5 秒到可运行（[scaling-to-1-million](https://modal.com/blog/scaling-to-1-million-concurrent-sandboxes-in-seconds)）。快照是**相对 base image 的文件系统 diff**；内存快照是实验性的、⚠️**快照即终止沙箱**、恢复后**不能跑 GPU**、且只能在同一 instance type 上恢复。

**该抄**
- **"worker 自己决定接不接"**——自拨号 worker **天然就有这个**：一条连着的流就是活的容量广告，worker 可以在流上拒绝任务。**我们的调度器不需要容量数据库，它需要的是最后一次心跳。**
- **派发关键路径不写 DB**：挑 worker + 发任务 = 一次内存操作 + 一次流写；任务行异步落库。
- 文件系统 diff 快照（"只存改动过的文件"）——映射到 git 上就是最便宜的版本。
- 把隔离选型的理由**诚实写下来**（他们说得很直白：选 gVisor 是为了能快照）。写清楚我们为什么是 `local`/`docker`/`podman`，可以避免读者假定一个我们没有的安全属性。

**不该抄**
- ⚠️ **内存快照**，以及 `snapshot_filesystem()` 这种"快照即销毁"的 API，再叠上 30 天 TTL 和**没有 API 能列出你自己建的快照**（得自己存 ID）。三个锐边，全是为了我们不需要的冷启动优化。
- ⚠️ **快照绑定 instance type / CPU 特性 / 驱动版本**。这是异构内网里"在我这台机器上能跑"型 bug 的标准配方。**任何有效性取决于"落在哪台服务器上"的产物都不要建。**
- 区域绑定的产物（同 Vercel 的 `snapshot_region_mismatch`）。
- Redis stream 当协调原语：十万 worker 时合适，十台机器时是白加的依赖。
- 两套 runtime 两套能力集。三个后端本来就会有差异，别翻倍。

### 1.4 microsandbox

**怎么做的**：本地优先的 microVM 运行时/库，**没有控制面、没有守护进程**——"SDK 直接启动沙箱进程…… VM 作为子进程在你的应用里启动"。镜像 OCI 兼容。自称 beta（[github.com/microsandbox/microsandbox](https://github.com/microsandbox/microsandbox)、[microsandbox.dev](https://microsandbox.dev/)）。⚠️ 常被引用的 `libkrun` **在官方文档和 README 里没有任何一处提到**，文档只说"一个带自己内核的真 microvm"——**具体 hypervisor 视为 [未证实]**。安全模型一句话：**"guest 不可信，host 可信，两者之间是硬件 hypervisor。"** 网络默认：公网可达，但**私网段、loopback、link-local、云 metadata、宿主全部拒绝**，入站只到 publish 过的端口。凭据一句话：**"你绑定的凭据从不进入 VM，guest 只能看到一个无意义的占位符"**，且"绑定的凭据只在你允许的那个 host 上被揭示"。

**该抄**
- ⭐ **host/guest 信任那句话的改写**。在我们系统里：**Worker 是可信侧的 broker，Workspace 里检出的仓库与 agent 生成的代码是不可信的 guest。** 写进 GLOSSARY。
- **默认拒绝私网 / loopback / link-local / metadata 的出口，入站除非 publish 否则关闭。** 这是对任何后端都成立的正确默认姿态，也是今天我们就能拿到的、最便宜的一条安全收益——agent 在内网能干的最大破坏就是摸到内部服务或云 metadata 端点。
- **`local` 后端做成无守护进程的子进程**：不用装任何服务，正是 dev/test 后端该有的范围。
- `<100ms` 那个数字带着脚注（M1 上的 **guest boot**，不是端到端 ready）——要发布延迟数字就发布机器和测量方法。

**不该抄**
- ⚠️ 单机 microVM 当后端：要 KVM/HVF/WHP，实际只在 Linux x86_64/aarch64 上，正是 `local`/`docker`/`podman` 想绕开的部署税。
- beta 级的快照语义。
- 把第三方数字（`libkrun`、"<100ms"）当成一手事实转述。

### 1.5 Fly Machines

**怎么做的**：Machine = Firecracker microVM。**状态模型值得整段搬走**：持久态 `created/started/stopped/suspended/failed`；瞬态 `creating/starting/stopping/restarting/suspending/destroying/launch_failed/updating/replacing`；终态 `destroyed/replaced/migrated`；还给卡住判定——`starting`/`stopping`/`restarting`/`destroying` 超过约 5 分钟、`updating` 超过 10 分钟视为卡住（[machine-states](https://docs.fly.io/machines/machine-states)）。**create/start 分离**是核心延迟思路：create 是"拉镜像 + 建 rootfs"（低两位数秒），start 是"已经装配好，只是启动"（**远低于一秒**）。调度是**"市场而不是队列"**：`flyd` 是该宿主上所有 VM 的 source of truth（append-only 本地日志），`flaps` 是无状态的 best-fit 代理，"不持有任何状态"；决策是 **immediate-or-cancel**——"要 MAD 的 VM 就给你 MAD 的 VM，要么什么都没有"，明确**不要 pending 状态**；他们用 gossip + SQLite 替掉 Consul，理由是"分布式状态根本不是共识问题，只是复制问题"，以及那句"**只要你在任何地方看到 Raft，就是哪里出错了**"。⚠️ **VM 里没有出站拨号的 agent**。API 上有 **lease + `fly-machine-lease-nonce` header**，作为 fencing token 防止两个控制器同时操作一台 Machine。

**该抄**
- ⭐ **persistent / transient / terminal 三段状态 + 具名瞬态 + 卡住判定**。这就是 `WorkerState` 枚举和一张表，几乎免费，让 reconcile 逻辑从一堆 boolean 变成显然。
- ⭐ **create/start 拆分映射到我们的拓扑**：贵的那部分做一次，便宜的那部分做很多次。**我们的贵是"Worker 连上并预热"，便宜是"在已有流上发一条任务消息"**——这正是我们已有的架构。Fly 的数字是**支持我们这么做的论据**，不是要我们改。
- **`wait` 原语**："阻塞到目标状态"胜过轮询；双向流下就是按 worker ID 的条件变量。
- ⭐ **lease + nonce**：正是我们"Worker 在新流上重连、控制层还以为旧流活着"的 hazard。**注册报文里一个单调递增的 per-worker epoch，拒绝陈旧 epoch 的任务结果**，就是全部修复。
- **append-only 的 per-unit 事件日志**（`worker_events` 表：state、reason、timestamp）。凌晨三点你会想要它。
- **"放置策略留在客户端，控制层保持笨"**：他们的原话是"把复杂、策略密集的功能从平台里切出去，移到客户端"。我们的控制层应该只做"把任务给一个合格的 worker"，别的不做。

**不该抄**
- ⚠️ **市场/竞价调度、gossip + SQLite 复制、多区 `flaps`**：这些只在"每个区有很多候选宿主、客户端能重试别处"时成立。自拨号 worker 已经给了他们engineering 出来的那个性质（连着的 worker 就是活容量）。**`SELECT ... FOR UPDATE SKIP LOCKED` 扫任务表就是正确的调度器剂量。**
- ⚠️ **immediate-or-cancel、没有 pending**：这是超大规模取舍。小系统里**排队严格优于"这一毫秒没有空闲 worker 就失败"**。
- ⚠️ **独立的 NATS 日志管道**：日志走一条与其它一切不同的路，是经典的事后后悔项。
- host-pinned machine：把调度问题变成可用性问题。
- suspend 当默认：他们的文档自己列了三个陷阱——快照不保证持久、连接看着活着其实死了、日志会丢。

### 1.6 Cloudflare Sandbox SDK / Containers

**怎么做的**：容器是 Firecracker microVM，**"一个 Container 只能通过它的 Durable Object 访问"**，DO 通过 `ctx.container` 操作它。状态回流是**全套里规范得最精确的一处**：SSE，`event: ${event}\ndata: ${JSON.stringify(data)}\n\n`，事件类型 `stdout`/`stderr`/终止的 `exit`，外加每 5 秒一条 `: keep-alive\n\n`（[stream-command-output](https://developers.cloudflare.com/sandbox/commands/stream-command-output/)）。两条与协议无关、但一定会咬人的实现备注：stdout 与 stderr **必须并发读**，因为"没人读的流会阻塞一个一直在往里写的进程"；杀进程要用一个 `exited` 标志守卫，因为"给已经退出的进程发信号会记录内部错误"。

**⭐⭐ 最有价值的一件可移植产出：stop-record 模式。** 他们文档写明了自拨号机队必然面对的问题："运行时不会记录哪个名字属于哪个用户……它不会在容器停止时唤醒它的 Durable Object。要列出用户的沙箱或在沙箱停止时动作，你的 Worker 得自己保留记录。" 他们的答案是：

```ts
interface SandboxRecord {
  state: "running" | "stopped";
  lastActivity: number;
  reason?: string;            // exited | idle | stopped | unknown | 任意错误文本
  exitCode?: number | null;
  stoppedAt?: number;
}
```

外加一个在构造函数里**重新武装**的 `container.monitor()` promise，和一个**在每次操作之前、每次 alarm 时都跑**的 `reconcile()`：如果记录说 running 而 `ctx.container.running` 为 false，就记一次 stop。`onStop` 守卫让记录**幂等**。停止原因里 `unknown` 的意思是"DO 在 OOM 时停止的"（[manage](https://developers.cloudflare.com/sandbox/manage/)、[run-code-when-a-sandbox-stops](https://developers.cloudflare.com/sandbox/manage/run-code-when-a-sandbox-stops/)）。他们的安全页也很直白：容器里"每个进程都有和 root 一样的 Linux 能力"，所以**"沙箱因此是最小的信任单元"**，以及⚠️**"你允许的每个目的地都给了代码一条把它能读到的东西送出去的路径。"**

**该抄**
- ⭐⭐ **stop-record 形状 + 停止原因分类 + 幂等终态写 + reconcile 循环**。对我们：流断开**就是** stop 事件，但控制层重启会漏掉它，所以启动时必须把所有"状态说 running 但没有活流"的 Worker 收尾，原因记为 **`unknown`**。约 60 行，解决的却是自拨号机队的定义性弱点——**控制层的视图永远可能是陈旧的**。
- **`unknown` 作为显式原因**而非 null：区分"我们失去了可见性"和"发生了具体的事"。
- ⭐ **凭据不进 Worker，由控制层发起调用**（见 §5）。
- **Worker 上默认拒绝出口**（`enableInternet: false` 默认）。
- 三条流实现备注，即使我们用 gRPC 也成立：并发读 stdout/stderr；用 `exited` 守卫终止；消息形状是 `{stream, data}` chunk + **一条独立的终态消息**带 exit code。
- **为了正确的理由配 keep-alive**：他们的 5 秒心跳是为了在命令静默时暴露死连接。用 gRPC HTTP/2 keepalive 干同样的事，否则**一个静默死掉的 Worker 会永远看起来活着**。

**不该抄**
- ⚠️ **用 Durable Object 当沙箱的身份与所有者**：在 Go 里意味着自建一个带放置、迁移、单写者保证的分布式 actor 运行时。**我们 Worker 的身份就是它的流，状态是一行 + 流的存活。**
- ⚠️ **alarm / `setInactivityTimeout` 当存活机制**：他们有 alarm 是因为"实例里跑的代码不算活动"。我们有一个严格更好的信号，而且是免费的：**存活 = 流开着。** 保留一个粗粒度 reaper 作兜底即可。
- 三条并行传输（HTTP / RPC / 废弃的 WebSocket，加上 SSE 输出和另一个终端 WebSocket）。我们有一条双向流，任务、状态、日志都用它，别再挂一个 SSE 端点。
- rootfs 快照 + 应用自管 ID + 30 天 TTL（恢复完全手工，快照不含挂载，快照缺失是不可恢复错误且没有回落到 base image）。**块级文件系统快照是大工程**；Workspace 要持久化就把目录 tar 到对象存储然后说清楚。
- 同时跑 V8 isolate 和 microVM。
- ⚠️ **信任客户端传来的 name / id**：他们要警告"这个名字不标识发送者"，所以"在你的 Worker 里认证调用方，从那个身份推导沙箱名"。**永远不要信任报文体里的 worker id 或 task id——从已认证的流上推导。**
- 15 分钟关闭宽限：托管任意服务的平台合理，小机队里把死 worker 留在 `stopping` 太久。**我们要秒级。**

### 1.7 Vercel Sandbox

**怎么做的**：Firecracker microVM。⭐ **sandbox/session 两级模型**说得最清楚（见 §0）。状态枚举五值：`pending | running | stopping | stopped | failed`——**`stopping` 是"正在关闭、不再接新活"的独立状态，这是大家都会忘的那个。** 状态回流是 **ND-JSON** 精确规范：`{data: string, stream: string}`，stream ∈ `stdout | stderr | error`（`error` 用于流失败，例如 `code: "sandbox_stream_closed"`），Content-Type `application/x-ndjson`；SDK 暴露成 async generator，`exitCode` 对 detached 命令**起始为 `null`**，`wait()` 后填充（[stream-command-logs](https://vercel.com/docs/rest-api/sandboxes/stream-command-logs)）。持久化**默认开启**：stop 时自动快照文件系统，resume 时用它启动新 session；resume 是**隐式**的——对一个已停止的持久沙箱调 `runCommand` 会静默开新 session 并重试。`snapshotExpiration` 默认 30 天，`keepLastSnapshots`(1–10) 是"推荐设置，它让快照存储保持平坦"；⚠️ 快照**区域绑定**，跨区创建报 `snapshot_region_mismatch`。有 `onCreate`（一次）和 **`onResume`（每次 session 恢复都跑）**，后者被描述为"重启后台服务或重新水合缓存"的地方——等于承认**resume 不是 continuation**。

**该抄**
- ⭐ **sandbox/session 两级模型**（已述）。
- ⭐ **五值状态枚举含 `stopping`**。
- ⭐ **ND-JSON 的 chunk 形状译成 proto**：`oneof` 里 `OutputChunk{stream, data}` + `ExitEvent{exit_code, reason}`。**把 `error` 当作一种 stream type 而不是独立的错误通道**是个好手法：流的关闭变成同一序列里的普通消息，而不是带外事件。现在就写进 `StatusUpdate` proto。
- **`exitCode == null` 表示仍在运行**：显式哨兵值胜过另加一个 boolean。
- ⭐ **audience 绑定的短期 token**：`aud` 钉死到一个目的地，`sub` 是 worker/任务身份，验证签名、issuer、过期、以及**精确的 aud 匹配**。这是正好嵌进我们 Credential Broker 的小而高价值的原语——我们的 STS 三元组就是它的 S3 版本。
- **`onResume` 这类"恢复≠继续"的显式 API**：Worker 在控制层重启后重连时，跑一个等价的钩子去重建状态，而不是假连续。
- **按数量而非仅 TTL 的保留策略**：`keepLastSnapshots: 1..10` "让存储保持平坦"。
- **凭据注入时同名 header 会被代理覆盖**——这是代理路线里最容易被忽略、也最容易让整个 brokering 失效的细节。

**不该抄**
- ⚠️ **默认开持久化 + 每次 stop 自动快照**：他们自己的文档带着代价警告（"每次自动快照都消耗单独计费的快照存储"）。一次性 scratch 检出通常会被丢弃，**默认关、逐 Workspace opt-in**。
- ⚠️ **在任意 SDK 调用上隐式 auto-resume**。"派发一个任务给 worker"会静默把那个 worker 拉起来，使任务延迟无界，并且**用表面存活盖住一个死掉的机队**。**派发必须要求活流，否则大声失败**——这与我们"流存在即调度信号"直接冲突。
- ⚠️ **`snapshot()` 会销毁它的对象**：一个叫 snapshot 的方法静默杀掉主语是令人意外的 API。stop 和 snapshot 要两个显式调用。
- **整套凭据代理防火墙**：SNI 匹配、TLS 中间人、per-sandbox CA 撒进十几个 CA bundle 环境变量、RE2 匹配器，外加他们自己承认的 domain fronting 缺口。⭐ 而且注意：**在我们的架构里控制层已经终结了 Worker 的连接，brokering 是一次函数调用，不是 TLS 中间人。** 这是自拨号设计真正的结构性优势，**别为了重新引入代理而把它丢掉。**
- 区域绑定的快照（不要把位置烘进产物身份）。
- Drives、FUSE 挂载、failover 区域、多用户 Linux 用户/组：每个都是子系统，YAGNI。

### 1.8 AWS Lambda MicroVMs

**怎么做的**：Firecracker microVM；`Dockerfile` → `create-microvm-image`（启动你的应用并**捕获已完全初始化环境的快照**）→ `run-microvm`（从快照快速启动）。入站是**每个 MicroVM、每组允许端口、带过期的加密 JWE**，放在 `X-aws-proxy-auth` 里；请求未授权端口返回 403（[microvms-networking](https://docs.aws.amazon.com/lambda/latest/dg/microvms-networking.md)）。

**⚠️ 全篇最锋利的一条运维教训：唯一性清单。** 快照会连同**后台守护进程**一起捕获，于是下列东西**在每个实例上完全相同**：生成的 UUID、用时钟播种的 PRNG、**在快照时抓来的 secret/token**、缓存的 `/dev/urandom` 字节、从 TCP 连接派生的状态。修法有明确次序：推迟到首次使用 → 在 **`/run` hook** 里生成（"创建 per-VM 唯一状态的正规位置"）→ 用 CSPRNG（Go 的 `crypto/rand`，**不是 `math/rand`**）→ 用 `runHookPayload` 传 per-VM 数据。还有一条：**"所有出站（非本地）连接在 run 和 resume 时都被杀掉。"**

**该抄**
- ⭐⭐ **唯一性清单当成我们 Workspace 初始化的测试用例。** Workspace 一旦从 warm 模板或恢复的快照创建，每一项都会撞上。
- ⭐ **"resume 后所有出站连接都死了"**——在自拨号设计里这本来就是自然行为：**重连就是恢复。**
- **端口 + 过期绑定的沙箱访问 token**：一个小小的、可审计的原语，映射到控制层的 per-Worker 凭据。
- **`/run` hook payload 作为 per-instance 定制通道**：比把 per-task 数据烘进镜像干净得多。

**不该抄**
- ⚠️ **内存快照与 eager 内存恢复**：代价曲线与直觉相反——"内存是 eager 恢复、磁盘是按需分页"，所以内存镜像越胖恢复越慢，他们的指导是裁剪内存镜像。这是 E2B 和 Modal 从另外两个方向得出的同一结论。
- **把后台守护进程快照进镜像**：这正是他们需要整套唯一性机器的原因。**如果我们的 Workspace 准备会启动服务，那是"在任务时准备，而不是在模板时准备"的信号。**
- AWS 专有面（network-connector ARN、PrivateLink、VPC egress、endpoint policy）。

### 1.9 Runloop / Blaxel / Northflank（压缩）

| | 怎么做的 | 该抄 | 不该抄 |
|---|---|---|---|
| **Runloop** | Devbox = 隔离的一次性 VM；状态 `Provisioning/Initializing/Running/Suspending/Suspended/Resuming/Failure/Shutdown`；⚠️**只保存磁盘，不保存内存**，"守护进程之类要手工重启"；ID 与 SSH key 跨 suspend 保留（[lifecycle](https://docs.runloop.ai/docs/devboxes/lifecycle)）。凭据走 Agent Gateways / MCP Hub，"不把你的真凭据暴露给 devbox" | ⭐ **disk-only suspend 的诚实契约**——便宜、秒级、并且告诉调用方到底丢了什么。**我们若要 pause，就抄这个，别碰内存。** 8 状态枚举含 `Suspending`/`Resuming`；"只有这些状态计费"那行定义（我们等价于"Worker 在哪些状态占用任务槽"） | Axons 那套分布式事件流做可观测（"日志另走一条路"的极致形态）；快照 + volumes + Agent Drive + archive **三套重叠的持久化答案** |
| **Blaxel** | perpetual sandbox：空闲 5 秒自动 scale-to-zero，standby 恢复 **<25ms**；内存状态含运行中的进程跨 scale-down 存活；镜像底是只读 EROFS，可写层是**全在 RAM 里的 tmpfs + OverlayFS**，⚠️约**一半内存**留给 tmpfs；⚠️网络连接（数据库、消息队列、HTTP 连接池）不跨 standby | "resume 在 25ms 内、哪怕隔了几周"作为设计目标，**加上对"什么会断"的诚实记账**——他们明说网络连接不存活。**无论我们建什么，发布同一句话。** 预留端口约定（API/系统/应用）让 SDK 默认值可猜 | ⚠️ 以内存保留的 standby 为默认；⚠️ tmpfs 可写层占一半内存（Go worker 跑多个 Workspace 时是隐形内存耦合，失败模式是"写超了沙箱就坏"，不是"磁盘满"）；⚠️ 只能在创建时设置的配置轴（`extraArgs` 开 iptables）——永久工单；两套重叠的持久化产品 |
| **Northflank** | 沙箱就是**一个 deployment service**：建、然后把实例数调到 1、**轮询 deployment 状态到 `COMPLETED`**（意思是 rollout 完成、容器起来了，**不是**进程结束）。⚠️ pause "会结束任何运行中的进程与终端会话"，**并删除临时数据**；pause 是异步的，**resume 可能和 pause 竞争**；扩缩容 = 重启。JS 侧 ⚠️ **必须"先注册 stdout/stderr listener 再 await 结果，否则早期输出会丢"** | ⭐ **"先订阅再启动"**——这是任何流式 exec 的真实 bug，包括 gRPC。把 Go 的 receive loop 先跑起来再派发任务。⭐ **"resume 可能和 pause 竞争"**：与我们的重连 epoch 是同一类 bug，也是"要显式状态机而不是一个 `paused` bool"的理由。"扩缩容 = 短暂中断，不是在线 resize"——诚实且正确 | ⚠️ 会删临时数据的 pause（命名陷阱；要停就明说，更好的是做 Runloop 那样的磁盘快照）；以 deployment service 为沙箱原语（create 是多步 rollout、pause 是 scale-to-zero、scaling 是重启）；按工作负载分类用两种 substrate；轮询 `COMPLETED` |

### 1.10 ⭐⭐ Coder（与 Stars Sandbox 最近的架构亲属）

**为什么重要**：Coder 是全篇唯一一个**以"工作区侧组件主动拨号控制层"为主要机制**的产品，而它这么设计的原因跟我们的**完全一样**：工作区在 NAT 后面、在客户的网络里、在你不拥有的机器上。

**怎么做的**：agent 是"跑在用户远端工作区里的 Coder 进程"，让 `coderd` 与客户端能到达工作区，与操作系统、架构、云无关。网络文档把要求写死在客户端和 agent 侧：工作区**必须能主动到达**控制层的 `CODER_ACCESS_URL`，且中间任何代理/ingress **必须支持 WebSocket**，"工作区与控制层之间不能有 NAT"（[admin/networking](https://coder.com/docs/@main/admin/networking)）。传输是**到 coderd 的 yamux 多路复用 WebSocket** + ping/pong + 连接监视器（`workspaceagentsrpc.go` 里的 `startAgentYamuxMonitor`、`agentConnectionMonitor`、`AgentStatsRefreshInterval`、`AgentConnectionUpdateFrequency`）。

**注册与 agent 侧 API**：注册常用**实例身份**——`POST /api/v2/workspaceagents/aws-instance-identity`（也有 Azure/GCP），body `{agent_name, document, signature}`，返回带 **`session_token`** 的响应；`agent_name` 用于**同一实例跑多个 agent 时消歧**。之后 agent 在 **`/api/v2/workspaceagents/me/`** 下工作：`PATCH me/app-status`、`GET me/external-auth`、`GET me/gitsshkey`、`POST me/log-source`、**`PATCH me/logs`**、`GET me/reinit`（[REST API: Agents](https://coder.com/docs/@v2.36.0/reference/api/agents)）。

**日志形状值得几乎逐字抄**：agent 先注册一个 **log source**（`POST me/log-source`，含 `display_name`、`icon`、`id`），然后 **PATCH 日志**为 `{log_source_id, logs: [{created_at, level, output}]}`——**批量的、结构化的、带 source 身份的**，不是裸字节流。读取端 `GET .../logs?follow=true&format=json|text`。

**⭐ `external-auth`**：支持对 GitHub/GitLab/Bitbucket/Artifactory 等的 OAuth2 外部认证；agent 在**工作区里**按 provider ID **代表用户**取 token（`coder external-auth access-token <ID>`），线上响应是 `{access_token, expires_at, password, token_extra, type, url, username}`。⚠️ 关键：`GET me/external-auth` 接受 **`listen` 参数——"等待新 token 被签发"**，即请求**长轮询直到用户完成 OAuth 流程**（[admin/external-auth](https://coder.com/docs/@main/admin/external-auth)）。这正是"Workspace 需要阻塞在'人类去授权 GitHub'上"而不轮询的原语。

**该抄**
- ⭐⭐ **整个形状，因为这就是我们的形状。** 隔离环境里的 agent 进程持一条到控制层的出站多路复用连接；它注册；它通过那条连接向控制层暴露**自己的** API 面（`me/...`）；它推批量状态；它按需从控制层取凭据。Coder 是这套设计最成熟的公开实现。
- ⭐ **`/me/` 式寻址**：把 agent 当作"me"、由控制层持有 ID→连接映射，**彻底消灭了"这个 worker 在哪个地址"这个问题**——**一个 Worker 可达，当且仅当它的流开着。** 直接适用于我们。
- ⭐ **批量、结构化、带 source 标签的日志消息**：`{log_source_id, logs:[{created_at, level, output}]}`。Workspace 跑多条命令和多个服务时，**log source 是让输出可归因的东西**，而批量是防止话痨构建变成逐行 RPC 风暴的东西。
- ⭐⭐ **`external-auth` + `listen`**："取这个 provider 的凭据，并阻塞到用户授权完成"正是 Credential Broker 需要的原语，把一次 OAuth 流程塌缩成一次流调用（ticket #12）。
- ⭐ **实例身份 bootstrap + `session_token`**：`{agent_name, document, signature}` → `session_token` 是"在你本来就信任的网络上（云 IMDS 或内网等价物）"的干净登记流程。注意 `agent_name` 那个消歧——**同一台机器跑多个 Worker 时我们也会遇到同样的碰撞**（ticket #6）。
- **先为 relay 路径设计**：他们偏好直连，但**永远**有 relay（DERP）作兜底，并把"没有 NAT"当作硬要求而不是期望。对我们的启示：**控制层地址应该是唯一需要被知道的那个地址。**
- **把延迟拆成几个独立数字**（控制层往返 vs worker 链路延迟，并明确标注"不是 P2P"）。这个分解是"机队感觉慢"能被诊断的前提。

**不该抄**
- ⚠️ **DERP relay + WireGuard + STUN 的 NAT 穿透栈**：它是为了**客户端到工作区**（浏览器、IDE、SSH）可达、以及打穿任意 NAT。我们的控制层↔Worker 关系完全不需要：Worker 拨一个已知地址。**不要引入 tailnet。**
- ⚠️ **yamux-over-WebSocket-over-WireGuard-over-relay 四层传输**：因为 Coder 必须服务交互式 PTY、端口转发和浏览器。**一条 gRPC 流 + `oneof` 消息体就是任务派发和状态该有的量。**
- `CODER_EXTERNAL_AUTH_<N>_*` 那种编号环境变量配置。
- 整套 template/provisioner 机器（Terraform 模板、`provisionerd`、per-workspace 资源图）。
- workspace proxy 作为独立产品层。

### 1.11 ⭐⭐ Buildkite agent（与"派发机制"本身最近的先例）

**为什么重要**：Buildkite 的 agent 就是一个常驻进程，**主动拨号中央服务、注册、领作业、把输出流回来、汇报终态**，跑在**客户自己的机器上、在客户的防火墙后面**。那就是我们的 Worker，只是外面那层不是沙箱。

**怎么做的**：旧的——"agent 使用出站拨号模型，通过 HTTPS 轮询 Buildkite 的 agent API，所以不需要转发端口或开入站防火墙"；启动即"把自己注册到 Buildkite"，然后"定期轮询平台找新活"。路由按 **queue**，可用 agent 按**"最近一次成功完成作业的先后"**排序以利用**热缓存**（[agent/v3/architecture](https://buildkite.com/docs/agent/v3/architecture)）。存活：注册后定期心跳，连续 **3 分钟**缺心跳 → **接下来 60 秒内**标记 lost → 之后不再收到作业。

**⭐ 2026 年从轮询切到流式——全篇与我们最相关的一个工程决策。** 旧的是"每 10 秒轮询一次"，于是刚过轮询点到达的作业"可能要等最多 20 秒"。新的把作业**推**给 agent：新边缘服务 `agent-edge.buildkite.com`（Go 写的）**维持到每个已连接 agent 的 ConnectRPC stream**，**"作业接受延迟降到 1 秒以内"**；v3.122.0（2026 年 4 月）起为默认。**⭐ 迁移方式是该抄的部分**：`--ping-mode` / `BUILDKITE_AGENT_PING_MODE` 取 `auto`（默认）/ `poll-only` / `stream-only`，而⚠️**在 auto 模式下"两种机制不是互斥的——它们并发运行。流健康时流胜出，流连接变得不健康时轮询循环自动接管。"** `stream-only` 则是"流失败 agent 就停"。取消是分阶段的：发 `cancel-signal`（默认 SIGTERM）→ 等 `cancel-signal-timeout`（默认 **10s**）→ SIGKILL；agent 关闭时另有一个 **5s** 的 `cancel-cleanup-timeout`"用来在退出前上传日志和 artifact"，且"取消之后的上传不受时间限制"。另外有区分控制层级结果的退出码（255 = 优雅终止，-1 = "Buildkite 失去联系"）。

**该抄**
- ⭐⭐ **`ping-mode: auto`——流式为主 + 自动轮询兜底。** 如果我们的 Worker 需要有降级模式（某个代理杀长连接、某条内网链路抖动），一条"流不健康时自动接管"的并发备用路径是**已经被验证过的**设计，也是针对"那条子网上 gRPC 流就是起不来"最便宜的保险。控制层本来就要有任务表，加一个低频轮询端点是小增量。
- ⭐⭐ **把他们的延迟数字当作我们的论据**：同样架构下，10 秒轮询 → 最多 20 秒死时间；流式 → **1 秒以内**。有人问"为什么 Worker 要持长连接而不是轮询"，这就是引用。
- ⭐ **分阶段取消时间线**：SIGTERM → 10s → SIGKILL，外加 5s 的退出后上传窗口；以及"取消后的上传不受时间限制"这个细节——它保证你不会丢掉**恰好是你最关心的那次运行**的日志。
- ⭐ **带具体数字的失联规则**："3 分钟没心跳 → 60 秒内标记 lost → 不再派任务"。两个常数一个查询。写下来。
- **把控制层级结果与命令自身结果分开的退出码**：流式系统里必须区分"命令失败了"和"我们把 worker 弄丢了"，把两者塞进同一个进程退出码就是这件事被搞混的方式。
- ⭐ **优先派给最近成功做过这类工作的 worker**（利用热缓存）。每个仓库有几个 Worker 时，调度器可以优先选**上次跑过这个仓库**的那个——它的 clone 与依赖缓存是热的。这是 Fly "已经在宿主上装配好"的廉价、非超大规模版本。
- **由消费者持有的 secret manager**：指向客户自己的 Vault/SSM 而不是集中存，是内网部署的好默认，与我们的 Credential Broker 可组合（Broker 是客户自有存储之上的一层薄签发者）。

**不该抄**
- ⚠️ **以轮询为主路径**：他们为了"可靠且在所有网络配置下都能用"把 10 秒轮询保留了很多年，代价是最多 20 秒的作业接受延迟。**我们的自拨号流就是这个问题的修复版，别把它退回去。**
- ⚠️ **`stream-only`**："流失败 agent 就停"对于一个全部工作就是挂在机器上的 worker 是不可接受的。永远要有兜底或重试循环。
- 基于 shell hook 的环境注入（`environment` hook）：在任意客户机器上是可行的约定；我们的控制层直接把凭据发到流上就行。
- 把 queue 当唯一的放置信号：queue 是好的粗筛；小机队里放置决策是"哪个连着的 Worker 有容量且缓存最热"。

### 1.12 隔离底座（压缩）：Firecracker / gVisor / Kata

**Firecracker**：KVM 硬件虚拟化；官方 README 给的是"**启动 <125ms**"和"< 5 MiB 内存占用"（[site](https://firecracker-microvm.github.io/)），AWS 发布文说"今天最快 125ms 启动 microVM"。⚠️ **广泛流传的"~5ms 快照恢复"在一手文档里找不到**——官方快照文档没给毫秒数，反而警告"cgroups V1 下快照恢复延迟高"。威胁模型写得很硬："所有 vCPU 线程从被启动的那一刻起就被视为在运行恶意代码"；⚠️**"Firecracker 不做任何网络流量过滤"**，"来自 guest 的所有出站流量因此都被视为不可信"。部署成本：要 `/dev/kvm`，且 AWS 指出"EC2 只在 `.metal` 实例类型上支持 KVM"（[design.md](https://github.com/firecracker-microvm/firecracker/blob/main/docs/design.md)、[getting-started](https://raw.githubusercontent.com/firecracker-microvm/firecracker/main/docs/getting-started.md)）。

**gVisor**：用户态内核，**永不把系统调用透传给宿主**；Sentry"需要用 Go 重新实现 Linux"，文件 I/O 由"稍微更受信任的伴随进程"Gofer 代理。逃逸需要**同时**打穿 Sentry 内核**和**宿主 Linux 内核。默认平台 **systrap** 依赖 `seccomp` 的 `SECCOMP_RET_TRAP`，⚠️**"如果你不得不在 VM 里跑 gVisor，systrap 通常比 KVM 性能更好"**，强行在 VM 里用 KVM"性能很差，而且是历史上的安全问题来源"。⭐ **这意味着 gVisor 的默认路径不需要 `/dev/kvm`、不需要嵌套虚拟化，是三者里摩擦最小的。** 已知缺口：只实现 Linux 系统调用 ABI 的子集；块文件系统"不原生支持"；`io_uring` 默认关闭；"不支持在沙箱内使用 KVM"；iptables 只部分支持（[platforms](https://gvisor.dev/docs/architecture_guide/platforms/)、[compatibility](https://gvisor.dev/docs/user_guide/compatibility/)）。

**Kata Containers**：VM-per-pod、OCI 兼容接口；支持 QEMU / Cloud-Hypervisor / Firecracker。⚠️ **启动时间数字在一手来源里找不到**（官网与安装文档都不给毫秒数）。部署更重：要裸金属，或**开了嵌套虚拟化的 VM**，外加 `/dev/kvm`、`vhost_vsock`/`vhost_net`。而且直接关系到我们的后端清单：⚠️**"Kata Containers 不支持 Podman。"** 另有：无 checkpoint/restore、无 host networking、不共享网络 namespace（[Limitations](https://github.com/kata-containers/kata-containers/blob/main/docs/Limitations.md)）。

**该抄**
- ⭐ **可插拔后端的模式就是我们的 `Runtime Provider`。** gVisor 有一个沙箱抽象 + 可替换后端（`systrap`/KVM/ptrace），且文档说"你可以用不同平台配置多个 runtime"。那正是 `local`/`docker`/`podman` 一个接口。**把接口收窄到再加第四个后端只需要一个文件。**
- ⭐ **默认走无 KVM 的路径。** systrap 是 gVisor 的默认，正是因为它哪儿都能跑。**我们的默认后端必须能在没有 `/dev/kvm`、没有嵌套虚拟化的内网 VM 上工作。**
- ⭐ **发布限制清单，并写明 `local` 不是安全边界。** Kata 有一份 `Limitations.md`；gVisor 直说"沙箱不能替代安全架构"。给我们三个后端各写一份等价物："`local`：不是安全边界，只跑可信代码"；"`podman`：无 host networking"……
- **出口策略属于宿主，不属于沙箱层。** Firecracker："不做任何网络流量过滤"；gVisor："网络策略控制应该在容器层施加。" **不要把包过滤建进 Go 的沙箱抽象里。**
- **一个 Sandbox 一个信任单元。** Firecracker 的"每个 Firecracker 进程对应单租户的单一工作负载"和 Kata 的"每个 VM 只跑一个 agent 进程"，都指向：**一任务一 Sandbox，事后拆除**。
- **明确 gVisor 是未来的升级路径而非现在的依赖**：它是三者里唯一在"需要真安全边界又不想上 KVM"时最不坏的，而且它是 OCI runtime，所以将来能塞在 `docker`/`podman` **后面**，而不是变成第四个后端。

**不该抄**
- ⚠️ **在内网几台服务器上引入 Firecracker 或 Kata**：都要 `/dev/kvm`，Kata 还要嵌套虚拟化（worker 本身通常就是 VM）和 `vhost_vsock`/`vhost_net`，并且**直接放弃 `podman` 支持**。在我们的规模上运维成本远超收益。
- ⚠️ **声称一个自己溯源不到的启动延迟数字。** Kata 不给，Firecracker 的官方文档也不给快照恢复的毫秒数。我们要发布数字，就把机器和方法一起发。
- **自建任何 VMM、jailer、seccomp profile 或设备模型**：按设计这些全部委托给 `docker`/`podman`。

---

## 2. devcontainer spec：能不能直接当 Workspace 的环境声明格式？

**结论：可以当「容器运行时声明」的子文档，不能当 Workspace 的环境清单，更不能当 Task 清单。**

四条硬条件：
1. **只在 Sandbox 后端是 docker/podman（且走 Docker-CLI 兼容接口）时直接有效。** 规范里**没有** `local`/进程隔离运行时的概念，得自己映射。
2. **包起来，不要在它里面扩展。** 根 schema 是 `unevaluatedProperties: false`（无变体时 `additionalProperties: false`），**顶层加未知键即非法校验**。规范认可的扩展点只有 `customizations.<uniqueToolName>`。
3. **配额、GPU 设备请求、放置、出网策略、Task 全部要我们自己提供**——规范里一个都没有。
4. **不要用 `secrets` 属性投递秘密。** 它只是 UI 提示的存根，**不携带任何值**。

推荐形状：一个超集文件，把 `devcontainer.json` 作为**嵌套子文档**，而不是往它里面塞非规范键：

```jsonc
{ "devcontainer": { /* 原样 devcontainer.json，base schema */ },
  "starsSandbox": { "resources": {...}, "placement": {...}, "network": {...}, "task": {...} } }
```

**它是什么**：JSONC（`allowComments: true`、`allowTrailingCommas: false`）。根是 `oneOf` 三个来源变体（`image` / `build`+`dockerFile` / `dockerComposeFile`）加一个公共块。`devContainerCommon` 的**完整**属性集是：`$schema, name, features, overrideFeatureInstallOrder, forwardPorts, portsAttributes, otherPortsAttributes, containerEnv, remoteEnv, remoteUser, containerUser, updateRemoteUserUID, userEnvProbe, overrideCommand, shutdownAction, init, privileged, capAdd, securityOpt, mounts, customizations, initializeCommand, onCreateCommand, updateContentCommand, postCreateCommand, postStartCommand, postAttachCommand, waitFor, hostRequirements, secrets`（[base schema](https://raw.githubusercontent.com/devcontainers/spec/main/schemas/devContainer.base.schema.json)）。

**版本与合规**：**规范没有版本号、没有 changelog、没有发布流程、没有合规程序或测试套件。** 只有 schema 文件隐式版本化。实际兼容性是一张手工维护的矩阵：VS Code 扩展不支持 `workspaceMount`/`${localWorkspaceFolder}`；Codespaces 忽略除 Docker socket 外的 bind mount、不支持 `"host:port"`、`${localEnv}` 在云主机上解析；CodeSandbox 用 rootless Podman 且把 `remoteUser` 覆盖成 root（[supporting-tools](https://raw.githubusercontent.com/devcontainers/spec/main/docs/specs/supporting-tools.md)）。**"合规"不是一个你能买到的可测试属性——钉一个具体 CLI 版本和 schema 修订作为兼容目标。**

**变量替换**：`${localEnv:VAR}`（宿主环境变量，有 `${localEnv:VAR:default}` 形式，未设置时**留空**而非报错）、`${containerEnv:VAR}`（**仅限 `remoteEnv`**）、`${localWorkspaceFolder}`、`${containerWorkspaceFolder}`、`${localWorkspaceFolderBasename}`、`${containerWorkspaceFolderBasename}`、`${devcontainerId}`（限用于一份白名单属性）。**未文档化**（是空白而不是禁止）：嵌套/递归替换、如何转义出字面量 `${...}`。**[未证实]** ⚠️ 对我们：用 `${localEnv:STARS_TASK_ID}` 注入 per-task 值是**在曲解语义**（它本意是"用户宿主机上的值"），更适合放进我们自己的 wrapper 块。

**生命周期**：严格顺序 `initializeCommand`（**宿主**）→ `onCreateCommand` → `updateContentCommand` → `postCreateCommand` → `postStartCommand` → `postAttachCommand`，都在容器里跑，且**失败即 fail-fast 中断后续**（`postCreate` 失败则 `postStart` 不跑）。`updateContentCommand` 会被云服务"定期执行以刷新缓存或预热容器"——⚠️ **这对确定性的 per-task 流水线是有害的**。整个生命周期是为**长期存在的开发环境**设计的，**没有任何钩子对应"跑这个任务"**。

**Features**：OCI artifact 分发（media type `application/vnd.devcontainers`），`install.sh` 以 root 在镜像构建期执行。安装顺序默认是**"由实现工具决定的最优顺序"**——即两个实现同样按规范写可以合法地得出不同的安装顺序。`dependsOn` 是硬依赖，`installsAfter` 是软的且不递归。**可复现性真正的答案是 `devcontainer-lock.json`**（`resolved` 带 sha256、`integrity`、`dependsOn`），CLI 在 `build`/`up` 时默认生成，有 `--frozen-lockfile`——但**那个文档至今仍以"提案"口吻写**，从不说它是必需还是可选。⚠️ 跨 Feature 副作用：`privileged`/`init` 只要有**任何一个** Feature 想要就生效；`capAdd`/`securityOpt` 是**拼接**的。**一个 Feature 可以静默地把整个容器提权——对我们这是直接的多租户 hazard。**

**secrets**：规范原文**"Secrets are not part of dev containers specification"**（[secrets-support](https://raw.githubusercontent.com/devcontainers/spec/main/docs/specs/secrets-support.md)）。schema 里的 `secrets` 是「环境变量名 → 仅 `description`/`documentationUrl`」，**永远没有值**。对照 Codespaces 的真实实现——它在 `devcontainer.json` **之外**，账户/仓库/组织级存储，**构建期不可用**（Dockerfile、Feature 里都拿不到），只在 codespace 跑起来后作为环境变量导出（[Codespaces secrets](https://docs.github.com/en/codespaces/managing-your-codespaces/managing-secrets-for-your-codespaces)）。**对我们的含义**：秘密的投递必须走我们自己的通道；规范自己的提案推荐**secrets file**，这跟"控制层按任务签发凭证"很契合；并且要注意先例的时序——**秘密必须落在构建之后、需要它的命令之前**（即我们映射里的 `postCreate` 之前）。

### 硬边界（决定性的一节）

每一条"没有"都是**文档化的缺失或明确的 non-goal**，不是我的推断：

- **它明确不是编排器、明确不管硬件**：原文"重点……是描述如何为一个开发目的的容器增补配置，而**不是充当多容器编排格式**"、"**它不关注硬件或 VM 供给**"。[#10](https://github.com/devcontainers/spec/issues/10)（OPEN）说"让 devcontainer.json 成为又一个多容器编排格式是一个 non-goal"。
- **资源配额：没有。** `hostRequirements.cpus/.memory/.storage` 是**宿主选择的最小值**，不是上限。schema 里没有 `limits`/`quota`/`reservations`/`pids`。对照：Compose 有 `cpus`/`mem_limit`/`mem_reservation`/`pids_limit`/`deploy.resources.limits` 和 `deploy.placement.constraints`（[Compose services](https://docs.docker.com/reference/compose-file/services/)）；Devfile 的 container 组件有 `cpuLimit`/`cpuRequest`/`memoryLimit`/`memoryRequest`。
- **GPU/设备：是信号，不是请求。** 全部 GPU 面就是 `hostRequirements.gpu: true | "optional" | {cores, memory}`。[#82](https://github.com/devcontainers/spec/issues/82) 提案过的 `"type": "nvidia"` **不在发布的 schema 里**；[#144](https://github.com/devcontainers/spec/issues/144)（OPEN）要求的卡数、架构、显存、计算能力、驱动/CUDA 版本**全部不支持**。**根本没有 `devices` 属性**——设备直通只能靠 `runArgs` 里的 `--device`/`--gpus` 或 `capAdd`/`securityOpt`/`privileged`。`hostRequirements` 是**咨询性的、且按工具而异**。**对调度器来说这当设备请求是不可用的**，而且这个转换是未规定的。
- **调度/放置：没有。** 没有 node selector、亲和性、标签、优先级。[#672](https://github.com/devcontainers/spec/issues/672)（OPEN）还在要 Kubernetes Pod / Podman POD 作为 Compose 的替代。
- **网络/出网策略：没有。** 网络相关属性全是转发/UI（`forwardPorts`、`portsAttributes`、`appPort`）。`runArgs` 能塞 `--network`，但那是引擎直通，正是 [#11](https://github.com/devcontainers/spec/issues/11) 说需要抽象的。
- **多租户：没有。** 没有租户、隔离级别或信任边界概念。`remoteUser`/`updateRemoteUserUID` 是本地开发的便利修复，不是安全控制；`privileged`/`capAdd` 是削弱的旋钮。rootless 运行时实际上是有问题的（[#479](https://github.com/devcontainers/spec/issues/479) OPEN，写宿主文件系统失败）。
- **非容器运行时：格式假定一个 Docker-CLI 形状的容器引擎。** 没有 runtime 选择器；Podman 只靠 **CLI 模仿**工作，而 [#11](https://github.com/devcontainers/spec/issues/11) 自己承认："Docker CLI 是互操作容器引擎时最接近于标准的东西……大体上应该支持任何模仿 Docker CLI 的东西……然而在**文件挂载和权限**方面出现过细微差别。" **`local`/进程隔离沙箱完全无法表达**——`overrideCommand` 默认就是真的跑一个 `while sleep 1000` 的空转进程，这是"这个格式建模的是持久容器，不是隔离进程"最清楚的证据。
- **能否编码一个 Task？不能。** 没有 `commands`、没有 `tasks`、没有 `steps`、没有 entrypoint/argv、没有退出码或期望输出语义、没有超时、没有重试、没有产物、没有输入参数（属性全集见上）。唯一被认可的放 task 类信息的地方是 `customizations.<tool>`，而按设计不会有别的工具认它。

### 该抄 / 不该抄

**该抄**：把 `devcontainer.json` 当成我们 manifest 里的一个**子文档**；校验目标是 `devContainer.base.schema.json`（**不是**会通过 `$ref` 把 VS Code 和 Codespaces schema 一起拉进来的 `devContainer.schema.json`），并用严格校验器让根上的 `unevaluatedProperties: false` 真正生效；用 `devcontainer-lock.json` + `--frozen-lockfile` 才谈得上可复现，**且注意安装顺序仍是实现定义的，别让 Feature 顺序承载语义**；`local` 后端映射成"我们自己的方言"——只认 `containerEnv`/`remoteEnv`/`mounts`/`initializeCommand` 与创建钩子，**忽略** `image`/`build`/`dockerComposeFile`/`runArgs`/`capAdd`/`securityOpt`/`privileged`/`init`/`overrideCommand`/`shutdownAction`，并把"这是方言"写进文档（没有别的实现有这个）。

**不该抄**：不要把 `secrets` 属性当投递通道；不要把 `hostRequirements` 当设备请求或配额；不要指望它表达放置/出网/租户；不要在顶层加键；不要把 Feature 的安装顺序当契约。

**缺的词汇从哪借**：
- **Docker Compose**：`cpus`、`mem_limit`、`pids_limit`、`deploy.resources.limits`、`deploy.placement.constraints`、`gpus`、`network_mode`、`networks.internal`、以及**一等公民的 secrets**（顶层 + service 级授权，source 可为 `file` 或 `environment`）。它是缺失部分最好的词汇捐赠者——而且 devcontainer **已经**通过 `dockerComposeFile` 把它嵌进来了。
- **Devfile 2.x**（[devfile.io](https://devfile.io/docs/2.2.0/devfile-schema)）：唯一在"范围相当"前提下**已经有资源限额**（`cpuLimit`/`memoryLimit`）**和命令/事件模型**（`commands[]` 的 exec/apply/composite + `events{}`）的格式，而且**真的带 `schemaVersion`**。代价是生态与工具采用率远小于 devcontainer，且它的事件是生命周期绑定的（`preStart`/`postStart`/`preStop`/`postStop`），**命令仍不是带期望输出的参数化任务**，出网/配额/调度同样没有。**在定稿前值得完整读一遍。**
- **GitHub Actions `jobs.<id>.container`**：**Task 那一半最好的参照**——一个会跑步骤然后终止的容器（对照 devcontainer 的空转长驻容器），外加 `services` 侧车模型。
- **Nix flakes / devShell**：可复现性最强，且**非容器，所以它能描述 `local`**——devcontainer 唯一做不到的那件事。但它描述的是 shell 环境而不是运行时（无镜像、无端口、无挂载、无生命周期、无秘密、无任务），且强依赖 Nix。
- `mise`/`asdf` 只解决一个轴（工具版本），可以作为任意容器之上的补充。**[未证实：本轮未读一手文档]**

---

## 3. Kubernetes 的调度与设备：借形状，不引 K8s

### 3.1 device plugin（旧路）

gRPC 两个服务：kubelet 侧的 `Registration.Register`，插件侧的 `DevicePlugin.{ListAndWatch, Allocate, GetPreferredAllocation, PreStartContainer}`。插件上报健康设备列表，kubelet"负责把这些资源作为节点状态更新的一部分广告到 API server"——于是 `nvidia.com/gpu: 4` 成为一个可调度的 extended resource（[device-plugins](https://kubernetes.io/docs/concepts/extend-kubernetes/compute-storage-net/device-plugins/)）。

**规范原文承认的限制**（这就是为什么会有 DRA）：
- **"Extended resources are only supported as integer resources and cannot be overcommitted."**
- **"Devices cannot be shared between containers."**
- **对调度器完全 opaque**：它只看到一个名字和一个整数，不知道这些设备是什么、能不能互换、是否在同一个 NUMA 节点、是否健康。DRA 文档说得更直白：device plugin "需要 per-container 设备请求、不支持设备共享、不支持基于表达式的设备过滤"。
- 健康处理：设备一旦标记不健康，kubelet **只下调 allocatable，不动 capacity**；**已经分配到故障设备的 Pod 保留分配、不被迁移**，进入 `Failed` 或 crashloop。
- 连成熟度都带着警告：**"Although the Device Manager component of Kubernetes is a generally available feature, the *device plugin API* is not stable."**

### 3.2 DRA（新路）

全部在 `resource.k8s.io/v1`（当前 K8s 是 v1.37）。对象：

| 对象 | 文档自己的描述 |
|---|---|
| **DeviceClass** | "定义一类可被申领的设备，以及如何在 claim 中选择具体设备属性" |
| **ResourceClaim** | "描述对集群中已挂载资源（如设备）的访问请求。每个 claim 有 *requests*，引用一个 DeviceClass 并从该 class 中选设备" |
| **ResourceClaimTemplate** | "定义一个模板，K8s 用它为工作负载创建 per-Pod 的 ResourceClaim……绑定到特定 Pod，Pod 终止时删除对应 claim" |
| **ResourceSlice** | "表示挂载到节点的资源……**Kubernetes 用 ResourceSlice 找出哪些节点能访问被申领的资源**" |
| **DeviceTaintRule** | 管理员设置的 taint，"给匹配设备选择器的设备加一条 taint" |

DRA 之于设备，正如动态卷供给之于存储（文档明确做 PVC/PV ↔ ResourceClaim/ResourceSlice 的类比）。

**生命周期**：1) **发布**——driver 写 `ResourceSlice`（`spec.driver`、`pool{name,generation,resourceSliceCount}`、节点访问四选一、`devices` 上限 128 条带 attribute 与 capacity）；generation 一致性规则是"消费者只能使用 generation 最高的那些 ResourceSlice"。2) **申领**。3) **分配**——**调度器**用 **first-fit** 从已发布的 slice 里分配（structured parameters），结果写进 `status.allocation`，消费者记录在 `status.reservedFor`；claim spec 不可变，所有状态在 status。4) **绑定**。5) **prepare/unprepare**——kubelet 与 driver 经 gRPC 协调（`NodePrepareResources`/`NodeUnprepareResources`）。

**经典 DRA vs structured parameters**：经典 DRA（KEP-3063）由 driver 侧分配、经 `PodSchedulingContext` 来回协商——**已在 1.32 撤回并移除代码**，理由原文是"集群 autoscaler 无法推理资源可用性"和"**调度器与 DRA driver 之间经 apiserver 的来回流程很复杂**"；它自己的 Drawbacks 写"通过 PodSchedulingContext 的信息流很复杂"。structured parameters（KEP-4381）把分配移进 kube-scheduler，从已发布的 slice 里算，**不需要 vendor 回调**。

**CEL 选择器**：对 `device.attributes["..."]` 的 CEL 表达式；**表达式必须 ≤ 10 Ki**，cost 上限 1000000；每设备最多 32 个 attribute+capacity。约束 `matchAttribute`（跨设备同值，例如同 NUMA）与 `distinctAttribute`（两两不同，例如两个必须来自不同物理网卡）。

### 3.3 值得借的分配语义

- **claim vs template = 需求形状与本次实例预留分离。** 文档给了直接的选择规则：**ResourceClaim** 用于"多个 Pod 共享对特定设备的访问；你手工管理生命周期"；**ResourceClaimTemplate** 用于"Pod 需要各自独立的、配置相似的设备"。这是最该搬的一个想法。
- **两阶段分配**：**Reserve**（主调度周期里定下结果，但**先不写 status**，存在 in-flight claims 的内存 map 里，避免阻塞 API 调用）→ **PreBind**（另起 goroutine 校验并写盘）→ **Unreserve** 回滚（"把 Pod 从 `claim.status.reservedFor` 移除"，避免两个互斥 claim 的 Pod 死锁）。分配前设 finalizer，防止已分配的 claim 被误删。
- **⭐ claim 本身就是锁。** 这是 DRA 里最好的想法。API 参考原文：多个调度器实例并发调度时，"两个调度器都尝试把自己的 Pod 加进 `claim.status.reservedFor`，但**只有先到达 API server 的那次更新被存下来**。另一个失败并认识到必须把 Pod 放回队列。" **预留就在被争用的对象上，用 CAS 写，输的重试。不需要分布式锁服务。**
- **taint/effect 模型**：`NoSchedule`（调度器忽略被 taint 的设备）、`NoExecute`（连带驱逐已调度的 Pod）、**`None`**（"被调度器和驱逐控制器忽略。DRA driver 可以用它向管理员或其它控制器传达例外情况，例如设备健康度下降"）——一个 no-op effect 换来做 dry-run 和可观测性，非常便宜。
- **幂等、按 key 的两阶段 prepare/unprepare**：请求按 claim UID 为键返回 map，**每个 claim 都返回一条条目，即使是 nil**；漏掉的会被再次调用。per-claim 失败是**带内**的（`NodePrepareResourceResponse.error`）；RPC 级错误则整份响应被忽略。并且明说：**driver "不应该指望 `UnprepareNodeResources` 一定会被调用"**。
- **`skipNodeOperations`**：driver 可以声明某类节点侧操作可跳过，kubelet 就"完全绕过调用那个 gRPC hook"。跳过 prepare 必须同时跳过 unprepare，理由是"避免节点侧插件缺失时 Pod 卡在 Terminating"。而且这个设置**会在分配时被复制进 claim**，所以运行中的分配保持它被调度时的配置。

### 3.4 调度器形状与节点存活

- **Filter / Score**：每次调度分**调度周期**（串行）与**绑定周期**（可并发）。Filter 插件按配置顺序跑，"任何一个标记节点不可行，其余插件对该节点不再调用"；PreScore/Score/NormalizeScore 排名；**Reserve**"发生在真正把 Pod 绑定到节点之前，它的存在是防止调度器等待绑定成功期间出现竞态条件"。
- **`NodeResourcesFit`**：三种打分策略 **`LeastAllocated`（默认）**、`MostAllocated`、`RequestedToCapacityRatio`。
- **scheduling gates**：控制 Pod 何时可被考虑调度；门在创建/准入时初始化，**之后可以任意顺序移除，但"不允许新增"**；被 gate 的 Pod 计入 `scheduler_pending_pods{queue="gated"}`——是**被留住**而不是被尝试。
- **存活是 Lease 与 status 的解耦**——对我们的 Worker 直接相关：
  - kubelet **每 10 秒**更新它的 Lease 对象（"Lease 是轻量资源。用 Lease 做心跳降低了这些更新在大集群里的性能影响"），失败时**指数退避，200ms 起、上限 7s**。
  - Node 的 `.status` **默认 5 分钟**更新一次，而"不可达节点的默认超时是 40 秒"——**刻意比 status 频繁得多**。
  - `Ready` 条件：`Unknown` 表示"节点控制器在最近 `node-monitor-grace-period`（默认 **50 秒**）内没听到该节点"。每个条件带 `lastHeartbeatTime` 与 `lastTransitionTime`——**"最后一次听到你"与"你最后一次变状态"分开，是关键建模细节**。`Ready` 翻成 `Unknown` 是一个**存活**陈述，不能被当成状态变化。
  - 升级路径：不可达 → `Ready=Unknown` → 打 `node.kubernetes.io/unreachable` taint → 但**"节点控制器在标记 Unknown 与提交第一个驱逐请求之间等 5 分钟"**，且驱逐限速（`--node-eviction-rate` 默认 0.1/s），小集群里会更早停止驱逐。
  - **设计结论**：**一个断流 20 秒的 worker 不该失去它的任务；一个消失了 5 分钟的 worker 应该。**

### 3.5 该抄 / 不该抄

**该抄（借形状，不引 K8s）**
1. **claim-as-lock + 乐观并发。** worker 的可用容量是一行 + 一个 version 列；抢占就是 `UPDATE ... WHERE version = $n AND available >= $count`，输的重试。
2. **需求形状 vs 本次预留**（任务类型上的默认设备需求 vs 每次任务运行的具体预留）。
3. **把库存当数据发布，从数据分配。** 调度器从不调用 driver——**我们的控制层调度时绝不该需要再问 worker "你有几张卡"，心跳载荷已经说过了。** 用单调 seq，不要 pool generation。
4. **taint/effect 模型**（含 `effect: None`），压成 `worker.state ∈ {ready, draining, quarantined}` + `reason`。
5. **Lease 与 status 解耦 + 宽限期**：频繁便宜的存活 ping、稀疏昂贵的状态更新、以及"不可达"到"回收"之间的长间隔。数字可直接借：**10s 续约、~40-50s 判不可达、5 分钟才回收、回收限速、心跳失败退避 200ms→7s。**
6. **幂等、按 key 的两阶段 prepare/unprepare**，per-item 错误带内，并写明"unprepare 可能永远不来"。
7. **整数 count 是一等接口**（"这个任务要 2 张这种卡"就是 `count`），**只有当真的有任务需要时才长出更丰富的选择器**。
8. ⭐ **把 extended-resource 名字当成刻意的逃生口。** K8s 保留整数、按名字的形态作为进入更丰富模型**文档化的桥**（DeviceClass 上的 `extendedResourceName`，于是 `example.com/gpu: 2` 依然可用）。我们照做：**从第一天就支持 `{"kind": "gpu", "count": 2}`**。

**不该抄**
- **ResourceSlice / pool / generation / resourceSliceCount / slice 完整性。** 它存在是因为 driver 是往共享 API server 写的独立进程、调度器必须知道视图是否完整。**你的 worker 发一次心跳——把整份清单放进去。**
- **CEL 选择器。** 我们需要 `kind == "a100"`，也许再要 `vram >= 40`。带 1 MiB cost budget 的 CEL 引擎在这里是荒谬的。
- **ResourceClaimTemplate 当 API 对象**：它就是你任务类型定义里的一个默认值。
- **PodSchedulingContext 式的来回协商**：KEP-3063 被撤回正是因为这条流程"很复杂"。我们面对的是同型的问题（只有 worker 知道自己真实状态）——**解法是把状态发布在心跳里，不是协商。**
- **为 prepare/unprepare 另开一条 gRPC 服务**：我们的 worker 已经握着一条双向流，两个阶段放那条流上就好；不需要第二个服务、CDI ID 契约和 `skipNodeOperations` 清单。
- **把 device taint 做成一等 API 对象**（`DeviceTaintRule`，带自己的 controller、condition、`timeAdded` 记账）。**worker 记录上的一个字段就够。**
- **consumable capacity / shared counters / MIG 分区**：整数 + kind。只有真有任务需要半张卡时再说。

**一句话**：我们的"哪个 worker 认领这个任务"是一个**带原子容量约束和租约的撮合问题**，不是资源模型问题。

| K8s 概念 | Stars Sandbox 的最小形状 |
|---|---|
| Node | Worker（控制层 DB 里按稳定 worker ID 为键的一行） |
| DeviceClass | Worker 在注册时声明的能力标签 / 种类（`gpu:a100`） |
| ResourceSlice + attributes/capacity | Worker 注册/心跳里的能力载荷：`{kind, count, attributes{}}` + 单调 `seq` |
| CEL selector | 结构化匹配：kind 相等 + 可选的 attribute 数值范围。到此为止 |
| `extendedResourceName` 桥 | **主接口**：任务要 `{kind: "gpu", count: 2}` |
| ResourceClaim | 任务上的设备需求字段 `{kind, count, constraints[]}`，由控制层解析到一个具体 worker |
| ResourceClaimTemplate | 任务类型上的默认设备需求 |
| `claim.status.reservedFor`（那把锁） | `allocations` 行 `(worker_id, kind, count)` + version 列；认领 = CAS |
| 两阶段 Reserve/PreBind | 内存里的 pending-claim map，然后一次持久写；任何失败都有显式 `Unreserve` 路径 |
| `NodePrepareResources`/`UnprepareResources` | 在**已有** gRPC 流上的 `PrepareTask`/`UnprepareTask` 帧，按 task ID 为键，幂等，per-task 错误在回复里 |
| `skipNodeOperations` | 不需要——我们只有一个 worker 二进制，不是每家一个 driver |
| Device taint / DeviceTaintRule | `worker.state ∈ {ready, draining, quarantined}` + `reason`；effect 隐含在状态里 |
| Node `Ready` + `lastHeartbeatTime`/`lastTransitionTime` | Worker 的 `last_seen_at`（存活）与 `state_changed_at`（状态）——**保持分开** |
| Lease | worker 在流上的周期 ping；`now - renew_at > lease_duration` 判不可达 |
| `node-monitor-grace-period`(50s) / 5 分钟驱逐延迟 | 同形：短的存活阈值 + 长得多的回收宽限 + 限速回收 |
| Scheduling gate | `task.hold_until` / `pending_reason`：任务存在但暂不可派发；只能移除不能新增 |
| Filter + Score（`NodeResourcesFit`、`LeastAllocated`） | 资格过滤（有容量、state==ready）然后打分（打紧还是打散——**选一个并做成可配置**） |
| Node `unschedulable` / cordon | Worker 的 `draining` 标志：不再接新任务，不动正在跑的 |

---

## 4. CI runner 模型：注册信任与断线语义

### 4.1 GitHub Actions

**注册信任模型**
- 注册 token 三个作用域各一个端点（repo / org / enterprise），返回 `{"token", "expires_at"}`，文档原文**"The token expires after one hour."**（[REST docs](https://docs.github.com/en/rest/actions/self-hosted-runners)）。
- ⚠️ **纠错一：GitHub 从未弃用 runner 注册 token。** 有人穷举了全部 changelog 语料（~3,808 篇，2018-04 → 2026-10-14）并按关键词检索，**没有**任何一篇宣布弃用或移除注册 token 或其 API；当前 REST 文档也没有弃用横幅（那里的"deprecated"只指 *runner 版本 EOL* 端点）。真正收紧的是**最低 runner 版本**（低于 2.329.0 无法注册）。常被误读的 [actions/runner#3915](https://github.com/actions/runner/issues/3915) 说的是一个**示例脚本**用了过时的 media type，不是端点。**JIT 是推荐，不是强制迁移。**
- ⚠️ **纠错二：文档里没有"注册 token 必须当秘密"的警告。** 全文本检索注册 token 的 REST 段、add-runners、remove-runners、安全参考，**没有**任何"持有者即可注册 runner"的警告。**"注册 token 很危险"是民间说法，不是 GitHub 文档。** 对照 GitLab——后者明确写了风险并移除了机制。**两个供应商在同一个设计上的证据质量差异本身就是一个结论。**
- **JIT（`--jitconfig`）**：blob 是 **base64(JSON{相对路径 → base64 文件内容})**，启动时写进 runner 根目录；内含 `AgentId`、`PoolName`/`PoolId`、`ServerUrl`、`WorkFolder`、**`Ephemeral`** 等（[Runner.cs](https://github.com/actions/runner/blob/main/src/Runner.Listener/Runner.cs)、[ConfigurationStore.cs](https://github.com/actions/runner/blob/main/src/Runner.Common/ConfigurationStore.cs)）。签发端点 `POST .../generate-jitconfig`，body `{name, runner_group_id, labels, work_folder?}`。GitHub 推它的理由原文（2023-06-02）："让你在不使用**有时限的注册 token** 的情况下创建一次性自托管 runner……通过限制**长生命周期凭据的暴露**来提升自托管 runner 基础设施的安全性"（[changelog](https://github.blog/changelog/2023-06-02-github-actions-just-in-time-self-hosted-runners)）。
- ⚠️ **JIT config 的时限是 [未证实]——没有任何一手来源给出 TTL。** 有文档的是**按作业数的单次使用**（"自动移除前最多执行一个作业"）与从未使用的 JIT runner 会被清理。官方客户端的规则是一句处理约定：**"Treat JIT configs as secrets until consumed."**（[actions/scaleset](https://github.com/actions/scaleset/blob/main/README.md)）
- **runner groups 是安全边界**：每个组织有一个默认组，不带 `--runnergroup` 注册的 runner 落进去且之后可移动；组 `visibility` 默认 `all`，`allows_public_repositories` 默认 `false`（"出于安全原因，公共仓库默认不能使用组里的 runner"）；一个 runner 只能在一个组里；作业在 60 秒内没被取走会重新排队，排队超 24 小时则失败（[runner groups](https://docs.github.com/en/actions/concepts/runners/runner-groups)、[routing precedence](https://docs.github.com/en/actions/reference/runners/self-hosted-runners)）。
- **ephemeral**：`--ephemeral` 是 configure 标志，"会在处理完一个作业后自动注销 runner"。官方推荐理由原文："GitHub **无法保证**作业不会在持久化 runner 关闭期间被分配给它。用 ephemeral runner 这一点**可以**保证，因为 GitHub 只给一个 runner 分配一个作业。" 外加一条尖锐的警告：有些客户"试图通过实现'每次作业执行后自动销毁 runner'的系统来部分缓解这些风险。然而这种做法可能没有预期中有效，因为**无法保证**一个自托管 runner 只运行一个作业"，并举了 `ps x -w` 能看到另一个作业以命令行参数形式带进来的秘密作为例子（[security reference](https://docs.github.com/en/actions/reference/security/secure-use)）。

**消息流**：**不是流，是 HTTPS+JSON 长轮询**。`MessageListener.GetNextMessageAsync` 是一个 `while(true)` 包住一次阻塞调用（[MessageListener.cs](https://github.com/actions/runner/blob/main/src/Runner.Listener/MessageListener.cs)），客户端侧每次轮询由 `SendTimeout` 默认 **100 秒**界定（可经 `GITHUB_ACTIONS_RUNNER_HTTP_TIMEOUT` 覆盖，钳制在 [100, 1200] 秒）。**服务端把轮询挂多久——[未证实]。** 现代路径下消息体只是**引用**（`RunnerJobRequestRef`），runner 再用第二个连接 `POST acquirejob` 取真正的作业。

**断线语义——两条独立连接，作业的命运由锁而不是轮询决定**
- 监听器**在启动 worker 之前**就开始续约任务，且"如果第一次续约失败，连 worker 都不启动"；续约节奏**每 60 秒**一次，首次续约重试 **5 次、间隔 10 秒**，之后一直重试到 `request.LockedUntil + 5 分钟`，然后记 "Lock renewal has run out of retry" 并放弃（[JobDispatcher.cs](https://github.com/actions/runner/blob/main/src/Runner.Listener/JobDispatcher.cs)）。
- 放弃时 runner **取消自己的 worker** 并以 `Abandoned` 结束请求；如果是取消令牌触发（用户取消/关闭）则结果 `Canceled`。
- **所以：runner 既不重试作业、也不自己判失败。** 要么它还能连上服务并报 `Abandoned`/`Canceled`，要么把作业留给**服务端锁过期**。**服务端的过期时长是 [未证实]——没有文档化的值。**
- ⚠️ **那句 "lost communication with the server" 不在 runner 源码里，也不在 github/docs 里**（全库检索 0 命中），它是**服务端**渲染在 UI 上的作业注解。**这不是一个可以照抄的措辞或格式约定。** 日志分段行为同样 [未证实]。
- **回来是重连，不是重注册**；如果注册没了就直接退出并清理本地配置（"The runner no longer exists on the server. Please reconfigure the runner."）。当它在前一次派发还没结束时看到**新**作业，会在 `EnsureDispatchFinished` 里**与服务端对账**：服务端说旧请求已有结果，就取消僵尸 worker 并继续；服务端说没有结果，就当作服务端故障抛错。那里的设计假设写在注释里："**服务端一次只会给一个给定的 runner 发一个作业**。"

**日志为什么会随 runner 一起死——五个机制，全在源码里**：① 死的那一刻当前**未满页（最多 8 MB）**里的内容从未入队（页只在涨到 8 MB 时或 `ExecutionContext.Complete()` 时才入队）；② 每条上传路径都是 best-effort 且静默丢弃，重试被**注释掉**了（`// TODO: figure out how should we retry paging log upload.`）；③ 唯一的抢救路径只在监听器杀掉不肯退出的 worker 时跑，且**只覆盖旧的 `IJobServer` 路径**；④ 如果监听器进程本身死了，③ 也不会跑；⑤ 诊断包只在开启 debug 时、在 "Complete job" 里上传，硬死时丢失。**实时控制台通道被源码注释明确标为 "always best effort"，作业结束时只上传最后约 200 行缓冲。**

### 4.2 GitLab Runner

**注册模型**
- 旧的 registration token 是**每个作用域一个共享秘密**。GitLab 自己的架构蓝图原文极硬："registration token 被多个 runner 共享"、"被复制到很多地方用于 runner 自注册"、"**单一 token 降低了审计的价值，让可追溯性几乎不可能**"、**"没有任何可能知道一个泄漏 token 的来源。"** 官方 DAST 检查把它的暴露评为 **Risk: High, CWE: 798**。CVE-2018-20500 的公告：注册 token 曾无法重置，"如果某个 maintainer 离开团队而且他们知道这个 token，就是一个安全风险"。
- 新的 **`glrt-` 认证 token**：`POST /user/runners` **以用户身份**创建（创建者记为 owner 以便审计），参数含 `runner_type`、`group_id`/`project_id`、`paused`、`locked`、`run_untagged`、`tag_list`、`access_level`、`maximum_timeout`，返回一次性的 `token` + `token_expires_at`（"务必保存，之后无法再取回"）。之后注册/验证是 `POST /runners/verify`，参数 `token` + **`system_id`（token 以 `glrt-` 开头时必填）**。
- ⚠️ **关键差异**：`tag_list` / `run_untagged` / `locked` / `access_level` / `maximum_timeout` 在**创建时**由授权方设定，**runner 自己不能声明**（旧的注册流程里 runner 能传这些）；新模型里 runner 认证时只发 `token` + `system_id`。**worker 不能向上声明自己的能力。**
- **一个 token 对应多台机器**：每个 runner manager 有一个 `system_id`（从 `/etc/machine-id` 派生、哈希后带 `s_` 前缀；否则随机 `r_`；存在 `config.toml` 旁边的 `.runner_system_id`），UI 里它们"出现在同一个 runner 下，但带一个可辨识的 system ID"。⚠️ 轮换的文档化失败模式：轮换时"**只有第一个 runner manager 收到新 token。其余的继续用失效的 token 并断开连接**"。
- **弃用时间线**：15.6 宣布弃用 → 16.0 新流程默认启用、`glrt-` 被接受 → 16.2 引入 legacy-compatible 注册 → **17.0 旧注册默认关闭并返回 `410 Gone - runner registration disallowed`** → 当前文档写的移除目标是 **20.0**。⚠️ **标注了一个不一致**：15.6 的弃用记录计划 17.0 移除（`removal_date: 2024-04-22`），但 17.0 只是**关闭**了旧注册，移除滑到了 20.0。

**作业生命周期端点（一条很好的派发先例）**
- `POST /jobs/request` —— 长轮询。声明 `201`（有作业）/`204`（无作业）/`403`/`409`/`422 Runner is orphaned`/`429`。请求体：`token`、`system_id`、`last_update`、`info{name, version, revision, platform, architecture, executor, features, config.gpus, labels}`、`session`。
- **作业载荷**（runner 侧 `type Job`）：`id`, `token`, `job_info`, `git_info`, `runner_info`, `inputs`, `variables`, `steps`, `image`, `services`, `artifacts`, `cache`, `credentials`, `dependencies`, `features`, `secrets`, `hooks`, `run`, ...  ⚠️ 是 **`runner_info`** 不是 `runner`，且 `inputs` 是独立字段。
- `PUT /jobs/:id` —— 状态更新。`state` 取值 **"`running`, `success`, or `failed`"**，另有 `exit_code`、`failure_reason`、`output.checksum`、`output.bytesize`。服务端状态机：`running` → touch；`success` → `build.success!`；`failed` → `drop_with_exit_code!`；**其他一律 400**。还有 trace 分块待处理时返回 **202** 加一个计算出的退避。
- `PATCH /jobs/:id/trace` —— 带字节区间的日志。**要求 `Content-Range` header**（否则 400）。分块 **1 MiB**，日志上限 **4 MiB**。⚠️ **服务端要求精确 offset 匹配**（`current_length == offset`），否则 **416** 并回 `Range: 0-<stream_size>`。已知的差一错误：服务端代码里有个 TODO 说 runner 格式化的 Content-Range"指向 byte+1"。
- ⚠️ **纠错三：认证头是 `Runner-Token`，不是 `X-Gitlab-Runner-Token`**（后者在 gitlab-runner 全量克隆和 Rails 源码里都查不到；`RUNNER_TOKEN_HEADER = 'Runner-Token'`）。只有公开的 Jobs API 用 `JOB-TOKEN`。
- **`CI_JOB_TOKEN`**：是 JWT，`exp = max(60 秒, job.timeout_value) + 5 分钟` 宽限，**作业结束后即撤销**，作用域按 API 逐个收窄（Jobs API 只能 `GET /job`）。

**断线语义**
- ⚠️ **作业留在 `running`，不重试、不转派**，之后被服务端清扫器按**命名的失败原因**丢弃：

| 作业状态 | 阈值 | 失败原因 |
|---|---|---|
| Pending | 24 小时 | `stuck_pending_with_matching_runners` |
| Pending | 1 小时 | `stuck_pending_no_matching_runners` |
| **Running** | **30 分钟无更新** | **`no_updates_running`** |
| Canceling | 30 分钟无更新 | `no_updates_canceling` |
| **Running** | **配置的 `timeout` + 15 分钟** | **`server_timeout_running`** |

  实现是 `BUILD_RUNNING_OUTDATED_TIMEOUT = 30.minutes`，由**每小时**跑一次的 `StuckCiJobsWorker` 驱动，所以**实际检测时间是 30 分钟加上最多约 1 小时的错峰**。⚠️ **标注的不一致**：文档散文说"一小时无输出就丢弃"，而代码常数是 30 分钟——代码与故障排查表一致，散文不一致。
- **心跳是真的**：runner 周期性地发 `PUT /jobs/:id` 带 `state=running`（`DefaultUpdateInterval = 3s`、`MinTraceForceSendInterval = 30s`、`MaxUpdateInterval = 15m`）。服务端的 `needs_touch?` 是 `updated_at > 15 分钟`，而那个 `updated_at` 正是 30 分钟清扫器检查的列。
- **runner 存活是另一套**（2 小时 online、7 天 stale），只驱动在线/离线标记，**不会让正在跑的作业失败**。
- **没有"runner 死了 → 立刻判作业失败"的快路径。** 只有：① 30 分钟没有 `PUT`/`PATCH`；② 超时 + 15 分钟；③ runner 自己报终态。
- ⚠️ **trace 跨 runner 重启不保留**（每个作业一个临时文件，已发送的 offset 只在内存里），而且因为死掉的 runner 的作业**永不被重新排队**，也就没有可续的东西。**同一进程内**确实会重新同步，方式是**回绕**：416 时按服务端回的 `Range: 0-<size>` 把已发送位置设回去；412 时**把 offset 重置为 0 从头重发**。
- ⚠️ **产物随 runner 一起死**：产物在**最后**（`after_script` 之后）作为阶段上传，中途死在 `build_script` 的 runner **一件产物都不上传**。对照：已经确认收到的 trace 分块会存活。
- 文档化的 issue：`#39847` 优雅关闭（SIGQUIT）会取消在飞的 `jobs/request`、**孤儿化 GitLab 已经分配的作业**——"在 GitLab 侧作业显示 `running`，带着已停止 manager 的 `system_id`、0 字节 trace，然后**在那里躺满它的整个作业超时**"（报告说占 drain 的约 2-4%）。

**并发与匹配**
- `concurrent`（全局，"0 是禁止的"）、`limit`（per-`[[runners]]`，0 = 不限）、`request_concurrency`（默认 1）、`check_interval`。默认行为是收到作业后"立即再轮询更多作业，直到没有作业或运行数达到 `concurrent`/`limit`"。
- **标签匹配是 AND 不是 OR**（PostgreSQL 的 contained-by），所以**作业上的每个标签都必须在 runner 的标签集合里**。
- ⭐ **避免双重分配的三层**（这是"任务不能被两个 worker 同时领"的干净先例）：① **Redis 独占租约 3 秒**（`build/register/#{build_id}`），拿不到说明另一个连接在处理，跳过该作业并返回 **409**，runner 重试；② **乐观状态转换**，代码注释把竞态点名了："当两个 runner 试图分配同一个 build 时，第二个会因 `StateMachines::InvalidTransition` 或 `StaleObjectError` 被拒"，两者都被 rescue 并继续循环，队列耗尽仍是 409；③ **队列成员关系**——pending 队列只装 pending 的 build，离开 pending 时删除 `ci_pending_builds` 行，所以一个 running 的 build **在结构上不存在于未来任何一次扫描里**。

### 4.3 注册信任模型：该抄 / 不该抄

**该抄**
- **在登记时签发 per-worker 身份凭据，永远不发共享注册秘密。** 这是 GitLab 的全部教训，也是本节最有价值的发现。共享 token 让"**可追溯性几乎不可能**"，且"**没有任何可能知道一个泄漏 token 的来源**"。对照 GitHub 的注册 token——一小时有效的 bearer、可转让、**没有撤销 API**、只有过期拦着你。
- **抄 GitHub 的 JIT 形状，不抄它的管道。** 值得偷的想法是：**一个不透明的、单次使用的包，携带身份 + 组 + 标签 + ephemeral 标志，由控制层签发、首次连接时消费。** 对我们就是一份 protobuf/JSON 登记包：worker id、沙箱能力标签、runtime 后端、TTL。**并把处理规则逐字照搬："Treat JIT configs as secrets until consumed."——这正是我们 Credential Broker 的契约。**
- ⚠️ **注意 GitHub 唯一没文档化的东西：JIT config 的时限。** 没有任何一手来源给出有效期。**所以不要从 GitHub 抄一个数字——自己选一个短的 TTL**（登记包必须几分钟内被消费，否则作废），因为这个 TTL 正是限制暴露面的那个东西。**这是先例沉默、决定真的属于我们的一处。**
- **把机器身份带进每一次注册。** GitLab 的 `system_id` 是便宜且高价值的一个字段，是运维能说"**宿主 X 上的这个 Sandbox**"而不是"某个 worker"的原因。
- **让撤销等于删除，并让 worker 在 404 时自清。** Coder/GitHub 都这么做：撤销 = 删掉 Worker 行 → 下一次开流返回 not-found → worker 抹掉本地凭据、回到需要新登记包的状态。**这是一个完整的撤销故事，不需要 token 黑名单。**
- **回收从未连接上的注册。** 一行 reaper，防止一张死 Worker 表。
- **抄"一个 Sandbox 恰好一个任务然后销毁"作为默认。** GitHub 的 ephemeral 保证是好部分：有它才能说"GitHub 只给一个 runner 分配一个作业"。并且注意那条警告——**假装的 ephemeral 不是 ephemeral**（一个作业放在命令行上的秘密能被下一个作业通过 `ps x -w` 看到）。对一个卖点就是"隔离边界 = Sandbox"的系统，**一 Workspace 一 Sandbox 应该是控制层强制的不变量，而不是靠 worker 自觉。**
- **把"这是哪台机器"与"它被允许做什么"分开。** GitLab 的新模型是好的拆分：`token` + `system_id` 认证，而 `tag_list`/`locked`/`maximum_access_level`/`maximum_timeout` 由授权方在创建时设定。**worker 不能向上声明自己的能力。**（→ 我们的 Worker 应该从控制层拿到能力标签，而不是从 worker 自己的注册报文里拿。）
- ⭐ **超出两者：两家都不用 mTLS，都是 TLS 上的 bearer token。在一个我们自己拥有控制层、worker 主动拨号的内网上，mTLS（worker 持私钥、控制层在登记时签客户端证书）严格优于以上全部**，且顺理成章地成为 Credential Broker 的载体。

**不该抄**
- **共享注册 token，永远不要。** 它在其有效期内可复用、没有撤销 API，且在 maintainer 离职时变成永久负债（CVE-2018-20500）。"任何已登记的 worker 都能再拉起更多 worker"是一个你不需要的提权原语。
- **三层作用域层级**（repo/org/enterprise，各带独立端点、token 类型、权限集）：那是被 GitHub 的多租户产品面塑形出来的 RBAC，不是被工程塑形出来的。内网里我们需要**一个**概念——worker 有能力标签，任务声明所需标签与允许的 worker 集合。直到真的有第二个租户为止。
- **默认开放的 runner group。** 一个隐含的"什么都能碰到什么"的默认，是能力边界腐烂的方式。**如果我们加标签作用域，默认必须拒绝。**
- **双 token 编排**（register token vs remove token）和 PAT 交换路径。
- **GitLab Premium 的 token 过期/轮换截止功能面**：它存在是因为 GitLab 必须支持"管理员不是操作者"的机队。而且它有一个文档化的失败模式——**轮换是真的很硬，半实现的轮换比没有更糟**。**如果要轮换，就 per-worker 轮换并留重叠窗口，让旧凭据在新凭据被确认之后才失效。**

### 4.4 断线语义：该抄 / 不该抄

**该抄**
- **核心教训：任务归属由服务端租约决定，不由观察 worker 决定。** 两个系统都收敛到这一点：GitHub 每 60 秒续约作业锁并在 `LockedUntil + 5 分钟`放弃；GitLab 从周期性 `running` 心跳追踪 `updated_at` 并在 30 分钟清扫。**两者都不让 worker 的意见成为裁决。我们的控制层拥有任务租约，worker 只持有租约上的一个 claim。**
- ⭐ **我们的 gRPC 流让这件事比两者都便宜——要利用这一点。** GitHub 的整套装置（100 秒 send timeout、15-30s 然后 30-60s 退避、session 重建、"reconnected" 横幅）**只**因为 HTTP 长轮询是单向的、不提供存活信号而存在。**双向流给了我们"流关闭 = 即时的、无歧义的失活信号"，再叠上 gRPC keepalive。不要重建轮询/退避/session 脚手架。抄租约纪律，完全跳过轮询机器。**
- **带显式 offset 的可续传状态/日志——但用流的序号，不用 HTTP 字节区间。** GitLab 的 `Content-Range` + 416-带-`Range: 0-<size>` 回绕是一个真正好的先例，形状是"**接收方精确告诉发送方它到哪了**"。但**不要**抄它的 HTTP 编码，也**不要**抄 412 → 从头重发（见下）。
- ⭐ **重连时先与服务端对账再继续。** GitHub 的 `EnsureDispatchFinished` 是正确直觉：worker 重新出现而控制层已经往前走时，worker 问、服务端仲裁。对我们：流重开时 worker 报"**我持有 Task X 的 claim，generation N**"；控制层要么确认（租约仍有效、generation 匹配 → worker 继续并重放缓冲的输出），要么拒绝（租约已重分配或 generation 已前移 → worker **中止并丢弃，且不得写结果**）。**generation 是让这件事安全的那一块。**
- ⭐ **把任务派发做成两阶段：assigned → acked → running。** GitLab 文档化的孤儿 bug 正是跳过这一步的失败：作业在 `201` 到达 runner **之前**就被标记 `running`，于是一个在这个窗口里死掉的 runner 留下一个幽灵 `running` 作业、0 字节 trace 和已死 manager 的 `system_id`，"然后在那里躺满它的整个作业超时"。GitHub 有同样的 60 秒窗口。**因为我们的 worker 主动拨号，ack 是免费的**：控制层发任务、worker ack、**然后**任务才变 `running`。在 ack 之前，断开是一次干净的无操作。
- **给终态失败原因命名，并区分"worker 死了"与"任务失败"。** GitLab 的 `no_updates_running`、`server_timeout_running`、`stuck_pending_no_matching_runners` 是好想法。**一个随它的 Sandbox 一起死掉的任务是一个基础设施事件，不是任务结果**；混为一谈会让系统无法运维。给 `Task` 一个失败原因枚举，至少含：worker-lost、lease-expired、task-failed、control-plane-cancelled。
- ⚠️ **不要自动重试在飞的任务。重试要显式 opt-in。** GitLab 把被丢弃的作业留在 `running` 并由清扫器丢弃，重新派发只经显式的 `retry:when` 列表；GitHub 交给锁过期，runner 从不自己重试。**对一个已经跑过带副作用命令的沙箱，这是对的。永远不要静默重跑一个第一次尝试可能已经半发生的 Task**——暴露出来，让调用方决定。
- **刻意地缓冲与 flush 日志，因为两个系统都会丢尾巴。** GitHub 只在 8 MB 页翻页或 `End()` 时才入队，硬死丢最多 8 MB；实时控制台明确是 best-effort 且结束时只发最后约 200 行。**抄那个修复而不是缺陷**：在流上按短定时器 **且**在每个 chunk 边界 flush；流在任务中途断开时，**把该任务的日志标为 "truncated"**，让读者永远不会把丢失的尾巴误认为干净的结束。**看起来像成功的沉默是最坏的结果。**

**不该抄**
- **不要抄 GitHub 的超时常数。** `LockedUntil + 5 分钟`、5×10 秒首次续约重试、100 秒 send timeout、15-30s/30-60s 退避——全部是为一个**全球多租户、在公网上长轮询**的服务标定的。我们的 worker 在内网、有持久流、丢线是**瞬间**可知的。**选一个具名租约 TTL 作为配置项**，按心跳间隔的小倍数定尺寸，**并让这个关系在代码里可见**。不要像 `JobDispatcher.cs` 那样把魔法区间撒得到处都是。
- **不要把 GitLab 的清扫数字当成设计抄。** 30 分钟的 `no_updates_running` 是一个**为 Redis/Postgres 作业队列、每小时跑一次的 worker 加错峰**而设的**清扫器**，所以真实检测是 30-60+ 分钟。更糟的是文档与代码**互相矛盾**。**如果我们的控制层只在流关闭时才知道 worker 死了，就完全不需要清扫器**——断开**就是**信号，可以立刻判任务失败。租约 TTL 只作为"控制层自己宕过、漏掉了那次关闭"的兜底。
- **不要建长轮询/session/broker-migration 那套机器。** `POST session` / `GET message` / `DELETE session` / 服务端在飞行中推一条带新 `BrokerBaseUrl` 的 `BrokerMigration` 消息 / 只带引用的作业体还要**第二次** HTTPS 调用来真正取作业——**每一层都只是为了绕过"HTTP 是单向的"以及让 GitHub 能不停机迁移主机。** 自拨号 gRPC 流把任务**内联**下发。**这是最该跳过的最大一块。**
- **不要抄 412 → 从 offset 0 重发。** 因为一次校验和不匹配就重发几兆日志。**在带序号的有序流上，一次不匹配的代价应该是一个 chunk，不是整份记录。**
- **不要抄"杀掉进程组"式终止而没有有界且保证的 flush。** GitHub 的清理是 best-effort，重试被字面注释掉，抢救路径只覆盖一条旧代码路径。**既然我们的 Workspace 就是被保护的对象，就要在过程中把要紧的东西（git 状态、产出的产物）持久化到 Sandbox 外面**，而不是在拆除时才发现唯一的副本已经随容器死了。
- **不要把 runner 侧那句 "lost communication" 措辞当规范。** 它不在任何一方的源码或文档里。**我们自己要保证的是：worker 掉线时控制层发出清晰、结构化的原因。**

---

## 5. 横切：凭据注入（直接面向我们的 Credential Broker）

先例分成**两条立场**，我们的选择与大多数相反，这一点必须写清楚。

### 路线 A：代理替换——凭据从不进入执行边界

沙箱里只有**占位符/无意义的 token**，真值在**出口代理**按 host 白名单替换后注入到**网络边界**之外。参与者：E2B、Daytona、microsandbox、Cloudflare、Vercel、Runloop、Blaxel、以及 OpenKruise / 阿里云 Agent Sandbox（[OpenKruise credential injection](http://openkruise.io/kruiseagents/user-manuals/security/credential-injection)、[阿里云](https://www.alibabacloud.com/help/en/cs/user-guide/inject-credentials-for-agent-sandbox-egress)）。

OpenKruise 的实现是这个形状最完整的公开描述：`SecurityProfile`/`GlobalSecurityProfile` + `tokenTransformation` 规则，`credentialRef.secret` 指向一个 K8s Secret，代理**按请求**读取、渲染目标 header 值（`value.template: "Bearer {{ .Token }}"`）并在转发前替换；`match` 块用 `domains`（必填，支持一个前导通配符）+ `schemes`/`paths`/`methods` 限定范围；`failStrategy` 默认 **`Block`**——"如果 Secret 无法解析，请求被拒绝而不是在没有凭据的情况下转发"。后果是凭据"**不会被写进 Pod spec、Sandbox 环境或 Sandbox 元数据**"，也"**不会进入沙箱的 env 或文件系统**"，且"**轮换 Secret 无需重启 Sandbox 即生效**"。文档化的限制也值得看：必须有 `traffic-proxy` runtime（从池里认领的 Sandbox **不能**注入 sidecar）；**"删除 profile 会停止改写请求，但不会撤销一个已经签发出去的凭据"**，所以指导是"尽量让 Secret 值短期化，并在应急处置时在 provider 侧轮换"。

**这类路线里值得记住的三个细节：**
1. **按 host 白名单替换。** 缺了白名单，占位符就变成万能钥匙（Daytona 的文档直接写：**"给每个 secret 都设一个 host 白名单。"**）。
2. **响应方向反向擦洗。** Daytona：代理在回程把真值换回占位符，这样上游回显也泄漏不了。
3. **fail-open 还是 fail-closed 是一个要明写的决定。** E2B 是 fail-open（未解析就整体丢掉该 header 并继续转发）；OpenKruise 默认 fail-closed（`Block`）。

⚠️ **已知缺口（做了代理也会留着的）**：E2B 说"被阻断的 TCP 连接从沙箱内部看起来可能是成功的"，必须应用层验证，且域名匹配只在 HTTP/80 与 TLS/443、不支持 QUIC；Modal 与 Vercel 都承认**按 SNI 匹配无法防 domain fronting**。

### 路线 B：交付真实但短期、限定范围的凭据

- **GitHub App installation token**：**恰好一小时**过期，可按 `repositories`（上限 500）与 `permissions` 收窄；⚠️ **关键语义是"求交集，绝不放宽"**——token 不能拿到 installation 没有的仓库，也不能拿到 App 没被授予的权限。
- **Vault 动态秘密**：一次 `vault read database/creds/<role>` 返回 `lease_id`、`lease_duration`、`lease_renewable`；`default_ttl`/`max_ttl` 配置；到期自动撤销，另有显式 `vault lease revoke` 与 `-prefix` 批量清理（[database secrets](https://developer.hashicorp.com/vault/docs/secrets/databases)、[dynamic secrets](https://developer.hashicorp.com/vault/validated-designs/vault/administration-guide/dynamic-secrets-management)）。**到期是兜底，撤销是主路径**——这是它比"只靠 TTL"强的地方。
- **IRSA / EKS Pod Identity**：把身份 token **投影成文件**（`/var/run/secrets/eks.amazonaws.com/serviceaccount/token`），kubelet 在 token 超过总 TTL 的 **80%**（或 24 小时）时**自动轮换**，"AWS SDK 负责在轮换后重新加载 token"；Pod Identity 走节点上的 DaemonSet agent 调 `AssumeRoleForPodIdentity` 并加会话标签（`kubernetes-namespace`、`kubernetes-service-account`、`kubernetes-pod-name/uid`）以支持 ABAC；另有 `AWS_CONTAINER_CREDENTIALS_FULL_URI=http://169.254.170.23/v1/credentials` 这个本地端点（[EKS IAM best practices](https://docs.aws.amazon.com/eks/latest/best-practices/identity-and-access-management.html)）。**注意这条路线和路线 A 是并存而非互斥的**：IRSA 把真凭据（临时 STS）交付进了 Pod，但通过投影文件 + 自动轮换 + 极短寿命让它可用。
- **SPIFFE/SPIRE**：SVID 是短期（常见 ~1 小时）的 X.509 证书或 JWT，**在约一半寿命时自动轮换**，经本地 Workload API（Unix socket 上的 gRPC）交付，**不需要显式秘密**；信任建立在两段式证明上（节点证明 + 工作负载证明，按 selector 匹配注册条目）（[Working with SVIDs](https://spiffe.io/docs/latest/deploying/svids/)）。
- **GitLab `CI_JOB_TOKEN`**：JWT，**随作业结束即撤销**，作用域按 API 逐个收窄。

### ⚠️ 我们站在哪一边，以及必须写下来的差别

我们的 GLOSSARY 定义 Credential Broker 是"按『谁授权了哪个仓库』**换发**短期、限定范围的凭据，使智能体与工作区代码都接触不到长期密钥"——**这是路线 B 的立场，与路线 A 的全部成员相反。**

这对 S3 是**正确且更好**的选择：STS 三元组是一个**标准协议产物**，AWS SDK 原生理解它、原生带作用域与过期，而路线 A 的占位符只对"HTTP header 形状"的凭据有效。**但必须写清楚两条差别**：

1. **我们不能继承路线 A 那种"凭据从头到尾没进过沙箱"的强保证。** 路线 B 里真凭据**在沙箱内**，只是短期且范围窄。**这是"缩小爆炸半径"与"消除爆炸半径"的差别，不是同一个主张。**（沙箱产品调研的先例报告自己也独立指出了这一点。）GLOSSARY 里应各加一行，避免有人误以为我们有更强的属性。
2. **对非 S3 的目标，路线 A 或"控制层代发"更合适。** 对 GitHub、对 LLM API，占位符替换或"控制层持有并代为调用"是更好的形状——**Coder 的 `external-auth`（带 `listen` 长轮询阻塞到用户授权）、Runloop 的 Agent Gateways / MCP Hub、Cloudflare 的 `interceptOutboundHttps`+gateway fetch，都是"控制层持有并代发"的实例化，而且它们是从三个完全不同的起点走到同一个答案的。** 建议 Broker 明确分成两类：**S3 类 → 交付限定范围的短期凭据；HTTP API 类 → 控制层代发或按 host 白名单替换。**

### ⭐ 一个真正的结构性优势，别丢掉

**因为 Worker 主动拨号、连接由控制层终结，brokering 对我们是一次函数调用，不是 TLS 中间人。** Vercel 为了做同一件事必须布 SNI 匹配 + TLS 拦截 + per-sandbox CA 洒进十几个 CA bundle 环境变量 + RE2 匹配器，**并且还留着 domain fronting 的已知缺口**。**不要为了重新引入一个代理而把自拨号设计的这个优势丢掉。**

**该抄**：路线 A 的三条细节（按 host 白名单、响应反向擦洗、fail-open/fail-closed 明写）；路线 B 的三条（短期 + 可按资源收窄 + 有显式撤销）；**GitHub App token 的"只做交集不放宽"是收窄语义最干净的表述**；Vault 的"到期是兜底、撤销是主路径"；IRSA 的"轮换时占位符不变 / 在 80% TTL 轮换 / 客户端负责重载"。

---

## 6. 横切：断线重连与状态回流（面向 #8）

**先例收敛出的通用模式**（gRPC 长流）：每条事件带**单调 seq**；重连时客户端发 `resume(last_seq)`；服务端在**保留窗口内**重放缺口，**窗口外回落到全量快照**；可选 **producer_epoch** 用来丢弃陈旧连接的迟到消息（"旧 epoch 的消息一律丢弃"）；失败模式是"**接受了 `since_seq` 却只把它当编号起点而不真的重放**"——客户端以为序号连续而事件已经丢了，**静默丢数据**。这是我们要刻意避免的那一个具体 bug。

**先例里的具体做法**：
- E2B：50 秒 keepalive、**"没有任何单条连接保证超过一小时"**、重连是调用方的责任。
- GitLab：`Content-Range` 精确 offset，不对就 **416 + `Range: 0-<size>`**，接收方按回值重新对齐。
- GitHub：靠 **job lock 续约**决定作业归属（这条与流无关，它管的是"任务归属"而不是"日志续传"）。

**⭐ 关键结论：我们需要两件不同的事，不要混成一个机制。**
1. **任务归属的租约**：谁拥有这个 Task，用 **generation/epoch** 防止陈旧连接写结果（Fly 的 lease nonce、E2B 的 `execution_id`、K8s 的 `reservedFor` 都是同一个东西）。
2. **状态/日志的续传**：用流上的**单调序号**，不要用字节区间。

K8s 的 **Lease 与 status 解耦**（频繁便宜的存活心跳 vs 稀疏昂贵的状态更新）正是这个拆分的模板，而且它给了可以直接借的具体数字：**每 10 秒续约、~40-50 秒判不可达、5 分钟才回收、回收限速**。GitLab 里 runner 存活（2 小时/7 天）与作业存活（30 分钟）也是刻意的两套。

**该抄**：单调 seq + resume + 窗口内重放/窗口外快照回落；producer_epoch / generation 丢弃陈旧连接的结果；两个阶段的心跳分离（存活 vs 状态）；流关闭作为**即时**失活信号（这是双向流相对长轮询的真优势，别退回轮询）；**日志按短定时器 + chunk 边界 flush，断流时标记 truncated**。

**不该抄**：只把 `since_seq` 当编号起点而不真重放（静默丢数据）；用字节区间做流上的续传；用一个机制同时承担存活和状态。

---

## 7. 补位先例：注册信任的另一种形状（kubelet TLS bootstrapping）

CI runner 之外，K8s 自身给了一个更完整的"**一次性引导凭证换长期身份**"模型（[bootstrap-tokens](https://kubernetes.io/docs/reference/access-authn-authz/bootstrap-tokens/)、[kubelet-tls-bootstrapping](https://kubernetes.io/docs/reference/access-authn-authz/kubelet-tls-bootstrapping/)）：

- **引导凭证**：bootstrap token 形如 `abcdef.0123456789abcdef`（正则 `[a-z0-9]{6}\.[a-z0-9]{16}`），前半是公开的 Token ID、后半是 Token Secret，存为 `kube-system` 下类型 **`bootstrap.kubernetes.io/token`** 的 Secret，名字必须是 `bootstrap-token-<token id>`。**认证后映射为固定身份**：用户 `system:bootstrap:<token id>`、属组 **`system:bootstrappers`**，另有可选的 `auth-extra-groups`（必须以 `system:bootstrappers:` 开头）。有 `expiration`（RFC3339 绝对时间）字段，过期即拒绝；`tokencleaner` controller 可自动删除。
- **三段式**：① kubelet 用引导 token 认证，创建 `signerName: kubernetes.io/kube-apiserver-client-kubelet` 的 CSR；② `system:bootstrappers` 被绑定到 **`system:node-bootstrapper`**（权限**只够创建和读取 CSR**）和 **`...:nodeclient`**（首次客户端证书可自动批准）；③ 续期时用另一条路径——**`system:nodes`** 绑 **`...:selfnodeclient`**。**首次签发与续期用不同的组、不同的 ClusterRole。**
- 官方给出的安全约束原文："你应该确保 token 被绑定到一个 RBAC 策略上，把（使用该 bootstrap token 的）请求**严格限制在与证书签发相关的客户端请求上**"，并且"把 token 限定到某个组能在之后有很好的灵活性，例如你可以在节点供给完成后**禁用某个引导组的访问**"。
- **serving 证书刻意不自动批准**，并明确列出批准前必须校验的三条：`spec.username` 形如 `system:node:<nodeName>` 且 `spec.groups` 含 `system:nodes`；`spec.usages` 含 `server auth` 且不含其它用途；`subjectAltNames` 只含该节点的 IP/DNS、**无 URI 与 Email**。内置批准器的行为也写明了："为避免与其它批准器冲突，内置批准器**不会显式拒绝** CSR，它只是忽略未授权的请求。"

**该抄**
- **整个"一次性引导凭证 → 一次性身份签发 → 凭身份续期"的三段式。** 我们的 Worker 注册应该长这样：控制层签发一个短 TTL、单次使用的登记包（§4.3），worker 用它换一份长期身份（mTLS 客户端证书 / 或长期 token），之后重连**只凭身份**，不再需要登记包。
- **引导凭证的权限被严格限制在"只能申请身份"上**，且**首次签发与续期走不同的角色**。这条对我们的含义是：**worker 重连续期时用的权限，不该等于它注册时用的权限。**
- **批准策略要显式列出校验项**（身份、用途、SAN），而不是"来申请就批"。
- 引导 token 带 `expiration` + 自动清理 controller。

**不该抄**：把 RBAC / SubjectAccessReview 那套机制搬进一个小系统——我们要的是同样的**三段式形状**，不是它的实现。

---

## 8. ⚠️ 先例指出我们既定形状可能有问题的地方

| # | 我们的既定形状 | 先例说什么 | 影响 |
|---|---|---|---|
| 1 | 沙箱侧**主动拨号**控制层 | 全篇只有 Coder 和 Buildkite 这么做；其余全靠"拥有到机器的网络路径"（宿主 agent / Durable Object / 控制层驱动 / 直接 RPC / 轮询） | **不是错，但没得抄。** Coder 与 Buildkite 是仅有的两个可以照着定契约的先例，必须精读 |
| 2 | 派发任务给 worker | Vercel 在任意 SDK 调用上**隐式 auto-resume**，会静默把 worker 拉起来、用表面存活盖住死掉的机队 | **派发必须要求活流，否则大声失败。** 绝不做隐式拉起 |
| 3 | Workspace 生命周期（#9） | E2B、Modal、AWS、Blaxel、Fly 五家从不同方向劝退**内存快照** | **只做文件系统级快照，或不做。** 现在就该把"不做内存快照"定下来，省掉数月工程量 |
| 4 | 状态回流（#8） | Fly 的 NATS 链、Runloop 的 Axons 都是"扇出给很多订阅者"的答案；我们只有一个订阅者 | **日志与状态必须在同一条流上**，不要另开管道或 SSE |
| 5 | Workspace 持久化 | Runloop（快照+volumes+Agent Drive+archive）、Blaxel（volumes+Agent Drive+tmpfs）都展示了多套持久化答案的代价：支持矩阵 + 每次调用都要问"这个持久吗" | **只有一个答案，并写进 GLOSSARY** |
| 6 | 环境声明格式（#10） | devcontainer 规范**明确**不是编排器、不管硬件；无配额、无设备请求、无放置、无出网、无租户、表达不了 `local`、没有 task | **它是子文档，不是 manifest。** 半个架构要自己定（配额/设备/放置/出网/task） |
| 7 | 沙箱调度器与 GPU 表达（#11） | DRA 的有用部分只有"调度期分配 + claim 即锁"；外层对象模型（ResourceSlice/pool/generation/CEL/DeviceTaintRule）是为"外部 vendor driver 往共享 API server 写 + autoscaler + 多调度器实例"而生 | **别引 K8s 的资源模型。** 我们的问题是有原子容量约束和租约的撮合问题，四五个字段就够 |
| 8 | 任务下发与断线恢复（#7/#8） | GitHub/GitLab 的长轮询 + session + broker-migration + 100s send timeout + 15-30s 退避，全是 HTTP 单向的补偿；GitLab 还要第二次请求才能取到作业体 | **自拨号双向流把任务内联下发，流关闭就是失活信号。** 抄租约纪律，跳过整套机器 |
| 9 | 超时与租约常数 | GitHub 的 60s renew / +5min 放弃、GitLab 的 30 分钟（且文档与代码不一致）都是为全球多租户公网标定的 | **不要抄常数。** 自己定一个具名 TTL，作为心跳间隔的小倍数，并让这个关系在代码里可见 |
| 10 | Worker 注册与身份（#6） | 共享注册 token 被两家自己的文档否定（GitLab: "可追溯性几乎不可能"、"没有可能知道泄漏 token 的来源"；DAST 评为 Risk: High） | **用一次性登记包 / 实例身份 + 控制层签发。** 和我们所有先例都不同的一点：**我们内网有条件上 mTLS，应该上** |
| 11 | Credential Broker（#12） | 除我们之外**所有**沙箱产品都把凭据挡在执行边界之外，靠代理替换 | **我们的立场相反且对 S3 更优，但不能继承"凭据从没进过沙箱"的强保证。** 差别必须写进 GLOSSARY，并明确 HTTP API 类目标走"控制层代发" |

---

## 9. 汇总：最该抄的（按价值排序）

1. ⭐⭐ **Cloudflare 的 stop-record + `reconcile()`**（§1.6）：`{state, lastActivity, reason, exitCode, stoppedAt}`，原因 `exited`/`idle`/`stopped`/`unknown`，幂等终态写，**每次控制层启动和每个 tick 都跑一遍 reconcile**。约 60 行，解决的却是自拨号机队的定义性弱点——**控制层的视图永远可能是陈旧的**。（→ #8、#9）
2. ⭐⭐ **Coder 的 `/me/` 式寻址 + `external-auth`（带 `listen` 长轮询）**（§1.10）：把 Worker 当作"me"经它自己的流寻址，**可达性 = 流开着**；让它按需**从控制层取**凭据，并且这个请求可以阻塞到用户完成授权。既解决寻址，又是 Credential Broker 的通用情形。（→ #6、#12）
3. ⭐⭐ **任务归属由服务端租约决定 + generation/epoch 拒绝陈旧连接的结果 + 两阶段 `assigned → acked → running`**（§3.4、§4.4、§6）：GitHub 的租约续约、GitLab 的孤儿 bug 教训、Fly 的 lease nonce、E2B 的 `execution_id` 是同一件事的四种表述。（→ #7、#8）
4. ⭐⭐ **claim-as-lock 的乐观并发抢占 + Lease/status 解耦**（§3.3、§3.4）：worker 的容量是一行 + version，抢占是一次 CAS，输的重试——不需要分布式锁；存活心跳与状态更新用两套节奏，具体数字可直接借 10s / 40-50s / 5min。（→ #8、#11）
5. ⭐ **注册：一次性、单次使用、控制层签发、含身份+标签+ephemeral 的引导包**（§4.3、§7），撤销 = 删行、worker 自清本地凭据；**并上 mTLS（优于我们所有先例）**。（→ #6）
6. ⭐ **几条近乎免费、但事后很难补的形状决定**：Buildkite 的 **`ping-mode: auto`**（流为主 + 轮询兜底，§1.11）；Vercel 的 **`OutputChunk{stream,data}` + `ExitEvent{exit_code}` oneof** 与含 `stopping` 的五值状态枚举（§1.7）；Fly 的 **persistent/transient/terminal 状态枚举 + 卡住判定**（§1.5）；Coder 的 **批量、结构化、带 log source 的日志消息**（§1.10）；Vercel 的 **sandbox/session 两级命名**（§0）。（→ 契约 #17）
7. ⭐ **`devcontainer.json` 作为 Workspace manifest 里的容器子文档**（§2），校验 `devContainer.base.schema.json`，用 lockfile，`local` 后端做成我们自己的方言；缺的部分（配额/设备/放置/出网/task）自己做，词汇从 Compose 借，Task 那半参照 GH Actions `jobs.<id>.container`；**并在定稿前读一遍 Devfile**。（→ #10）
8. ⭐ **把 HTTP API 类的凭据注入定为"控制层代发"或"按 host 白名单替换 + 响应反向擦洗"**，并把与 S3 类（交付限定范围短期凭据）的差别写进 GLOSSARY（§5）。（→ #12、#16）

## 10. 先例里明确被劝退的（写下来省得重新论证）

1. **内存快照**——E2B / Modal / AWS / Blaxel / Fly 五家从不同方向收敛到同一结论。
2. **任何 NAT 穿透 / relay / overlay 网络栈**——Coder 的 DERP + WireGuard + STUN 是为"客户端到工作区"可达而存在的；我们的 Worker 拨一个已知地址，这个问题不存在。
3. **不在流上的日志管道**——Fly 的 NATS、Runloop 的 Axons 都是"要扇出给很多订阅者"的答案；我们只有一个订阅者。
4. **第二套、第三套持久化机制**——Runloop 和 Blaxel 展示了代价。
5. **把 worker 的容量/身份交给 worker 自己声明**——GitLab 的新模型刻意反着做（能力由授权方在创建时设定）。
6. **为 prepare/unprepare 另开通道、把 taint 做成一等对象、引用 CEL**——都是 DRA 为"外部 vendor driver"付出的复杂度，我们不该付。

---

## 附：本轮未能从一手来源证实的事项（决定真的属于我们）

- GitHub 注册 token 是否可重复使用（文档两种说法都没有）。
- GitHub 是否**没有**文档化"注册 token 应当作秘密"的警告——检索结果是**没有**，但这是"未找到"而非"明确否定"。
- `encoded_jit_config` 的**时限**（无任何文档化 TTL）与其中**确切文件清单**（机制已证实，成员名未证实）。
- GitHub 服务端的锁/租约在 worker 掉线后多久判作业失败/放弃（无文档值，只有一个报告者测得的 2700 秒）。
- GitHub broker 把长轮询挂多久（runner 侧 100 秒有据，服务端无据）。
- GitHub 是否会把掉线的作业重新排队到另一个 runner（runner 源码与文档均未涉及）。
- GitHub 日志分段行为、以及那句 "lost communication" 的正式措辞（均不在 github/docs 里）。
- GitLab：`PUT /jobs/:id` 的 412 在当前 master 里由谁产生（runner 仍处理它，`UpdateBuildStateService` 里找不到生产者）。
- GitLab：无输出丢弃的"文档说一小时 / 代码常数 30 分钟"的不一致（未从源码调和）。
- GitLab：`gitlab-runner#37201` 的详情来自搜索而非直接抓取。
- devcontainer：变量替换的**嵌套**、**如何转义出字面量 `${...}`**、以及在 `image` 里是否允许（文档**沉默**，不是禁止）；lockfile 是否被每个实现遵循（规范文档仍写成提案，只有 CLI 的默认行为有据）。
- K8s：device plugin 是否**硬性**禁止两个插件在同一节点广告同一资源名（只找到命名约定的间接证据）。
- K8s：DRA GA 到底记在 1.34 还是 1.35（KEP 说 1.34，DRA 概览页说"Stable since v1.35"，博客有 1.34 的 GA 文章——**未调和**）；`resource.k8s.io/v1beta1` 的移除版本（来自 issue 检索，未见发布说明）。
- modal 的字节如何在线上传输（页面级协议未文档化）。
- microsandbox 的 hypervisor 是不是 `libkrun`（官方文档与 README 均未提）。
- Kata 与 Firecracker 的启动时间数字（两家一手来源都不给毫秒数，除了 Firecracker 的"<125ms 启动"）。
- Nix `flake.lock` 的 pinning 语义与 `mkShell`/`shellHook`（本轮读的 `nix develop` 页未覆盖）；`mise`/`asdf`、Earthly、Tilt（本轮未读一手文档，仅作方向性提及）。

## 两处需要带进设计文档的纠错

1. **GitHub 从未弃用 runner 注册 token。** 如果任何笔记里写了"已弃用"，那是错的。真正收紧的是最低 runner 版本。
2. **GitLab Runner 的 API 认证头是 `Runner-Token`，不是 `X-Gitlab-Runner-Token`。**
