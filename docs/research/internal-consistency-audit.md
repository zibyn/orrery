# 规格内部一致性审计（2026-10-17）

范围：地图 #29 正文；已关票据 #30–#38、#40、#41 的正文与裁决评论；`docs/adr/0009`–`0015`（以工作区未提交版本为准）；`GLOSSARY.md`。不对照厂商。

判定规则：ADR-0015 与今天修订的 ADR-0009 / ADR-0014 是最新决定；被它们**明确**取代的旧票据内容不计矛盾（文末附清单）。只有没被明确处理的冲突、悬空引用、未闭合状态、走查缺口才列为问题。

出处写法：`#33 L16` 指 `gh issue view 33 --comments` 输出的第 16 行；`#29 L24` 指地图正文（`gh issue view 29 --json body`）第 24 行；`#38 正文`、`#40 正文` 指票据问题正文；ADR 与术语表用文件名加行号。

---

## 一、严重（实现无法照写，或两条规则直接冲突）

### S1. Sandbox 状态机未闭合：没有重启边、没有排队态、没有失败终态，`unavailable` 没有出口

- 状态只有 `provisioning → ready → stopping → stopped → deleting → deleted`，另有 `unavailable` 条件（#33 L21；GLOSSARY.md:113-115）。
- 缺的边：
  - **重启**：#34 L16 说「重新启动须重新 admission」，ADR-0009:22 说「a restarted Sandbox gets a new one from `Create`」，但状态机里没有 `stopped → provisioning`（或别的）这条边。
  - **排队**：admission 排队期间 Sandbox 处于哪个状态没写（#34 L13、L15；ADR-0015:32）。
  - **失败终态**：`admission_timeout`（ADR-0015:32）、`unsatisfiable`（#34 L15）、`quota_exceeded`（#34 L13）、排队中取消（#34 L17）、`Create` 后回读 cgroup 不符而创建失败（ADR-0009:34）、镜像 / Resource 拉取失败、Workspace 卡在 `provisioning_stalled`（ADR-0015:38），这些情况 Sandbox 最后停在哪个状态，都没有写。
  - **`unavailable` 的出口**：#33 L21 写的是「可在同一 Node 上恢复」，可是没有 `Start` 操作（ADR-0009:22），Runtime 不在了靠什么恢复没写。admin 宣告 Node lost（ADR-0014:20）之后 Sandbox 怎么样也没写，Sandbox 没有 `lost` 态。
- 建议修法：在 ADR-0015 加一段「Sandbox 状态」，写清四件事。
  1. 排队并入 `provisioning`，带 `queued` 条件。
  2. 加 `stopped → provisioning` 重启边。
  3. 加终态 `failed`，并带 cause：`admission_timeout` / `unsatisfiable` / `create_failed` / `node_lost`。
  4. `unavailable` 的出口：Agent 确认 Runtime 已不存在时，Reconciler 把 Sandbox 置为 `stopped` 并释放 Compute Lease；admin 宣告 Node lost 时，Sandbox 进入 `failed(node_lost)`。

### S2. 活 Node 上 Runtime 死掉时，Exec 没有终态，GPU 会一直被占

- ADR-0015:18 写的是「Only an `admin`-declared lost Node ends its Execs」。假如容器被 OOM 杀、Docker daemon 重启、主机重启，Exec Supervisor 会跟着死，退出码也写不出来。这时 Node 是活的，按这条规则 Exec 永远停在 running。
- 连锁后果：
  - Idle Stop 要求「no running Exec」（ADR-0015:33），所以不会触发。
  - Compute Lease 只能靠一个动作结束（ADR-0015:14），所以 GPU 永远不释放。
  - ADR-0015:8 自己引用的 Daytona #5140 就是这个场景。
- 另一个同类缺口：已接受但还没送达的 Exec，如果它所在的 Node 变成 Unreachable，也没有出口。超时从 Provider 开始执行时才计时（#33 L16），取消又需要终止确认（#33 L16；GLOSSARY.md:105-107）。
- 建议修法：在 ADR-0015:18 后补一句：「活 Node 上，Agent 观察到 Supervisor 记录里没有退出状态，并且 Runtime 或进程已不存在，这就是确认，Exec 以 `runtime_lost` 结束」。这是确认，不是计时，不违反「不靠计时器判定」。

### S3. Lease 靠「动作」结束，但动作没有列出来；停止的 Sandbox 是否还持有 RW Attachment 也没写

- ADR-0015:14 与 GLOSSARY.md:158 写的是「ends by an explicit act」，但没有列出是哪些动作。#32 L12 原来靠 TTL 过期来撤销，这条路已经取消。
- #34 L16 说 stopped「只保留 Workspace 和元数据」。这句话没说清 Workspace Attachment / write Lease 算不算「元数据」。ADR-0010:26 的冲突条件是「live Attachment」，「live」是什么意思也没定义。
- 后果：
  - 停止的 Sandbox 如果保留 Attachment，ephemeral Workspace 的宽限期（#32 L10，条件是「无 Attachment」）永远不会开始。
  - 别的 Sandbox 永远拿不到写权限。
  - 删除 Workspace 时永远 `conflict`。
  - 反过来，如果停止时释放 Attachment，那么次日重启时谁先抢到写权限没有写。
- 建议修法：在 ADR-0015 Lease 段加两句。
  1. 「Sandbox 进入 `stopping` 即释放其 Workspace Attachment（RW Lease 随之结束）；重启时重新申请，冲突在 admission 时报 `conflict`」。
  2. 列出全部结束动作：Sandbox stop、Sandbox delete、admin 撤销、Node 被宣告 lost。

### S4. 删除带未终态 Exec 的 Sandbox：一处说先取消，一处说返回 conflict

- #33 L11：「Sandbox 停止、删除……时，先持久化取消意图，再终止相关 Attempt」。
- ADR-0010:26（以及 #37 L38）：「collide with … an unfinished Exec returns a conflict instead of implicitly cancelling」。
- 这两条没有被任何一方明确处理。ADR-0011:22 也写「`deleting` 时 in-flight cancelled」，那里说的是通道请求，不是 Exec。
- 建议修法：保留 ADR-0010 的写法，在 ADR-0015 写明「#33 L11 中『删除』半句作废；停止仍按 #33 先取消」。

### S5. PTY 输出到底算不算持久 Execution Log

- ADR-0015:22：「Replay is a bounded ring buffer on the Node, not PostgreSQL」。
- 冲突方：
  - #33 L18：stdout / stderr / PTY 共享单调全局序列号，并持久化。
  - GLOSSARY.md:101-103：Output Frame 包含 PTY output。
  - GLOSSARY.md:65-67：Execution Log 是持久的。
  - ADR-0014:32：Output Frames 存进 PostgreSQL。
  - #31 L12：重连超出保留窗口时，用完整日志快照补齐。
- 按 ADR-0015 的写法，PTY 超出 ring buffer 之后就没有「完整日志」可以补了。
- 建议修法：在 ADR-0015 PTY 段补一句「PTY 输出不进 Execution Log，不入 PostgreSQL，超出 ring buffer 不可回放」，并把 GLOSSARY Output Frame 里的 PTY 删掉；或者反过来，把 PTY 也落库，ring buffer 只做快速回放。两者选一。

---

## 二、中等（会在实现时卡住，或悬空引用指向已失效的规则）

### M1. ADR-0014 把 Unreachable 时的 Exec 交给 #33，而 #33 那条是计时器规则

这和「Lease TTL」是同一类问题。

- ADR-0014:20 写「running Execs follow `#33`」。
- #33 L16 是「Node failure deadline 后才可按 `node_lost` 或终止未确认条件收敛」，这是计时器判 Node 失败。
- ADR-0015:18（「Only an admin-declared lost Node ends its Execs」）实际上推翻了它，但没有点名撤销。`node_lost` 这个 cause 也就悬空了。
- 建议修法：ADR-0014:20 改成指向 ADR-0015；ADR-0015:18 补「`#33` 的 Node failure deadline 撤销；`node_lost` 只在 admin 宣告后写入」。

### M2. Node 退役流程走不通

- ADR-0014:26 要求先用一个 Exec 把 Workspace 归档成 Artifact。但：
  - Exec 需要一个 `ready` 的 Sandbox（#33 L11）。
  - 已停的 Sandbox 要重启，就得在绑定的 Node 上重新 admission（#34 L10）。
  - 退役时 Node 一般已经 `paused`，而 paused Node 不接受新 allocation（ADR-0014:22）。
- 其它缺口：
  - 归档文件必须写在 `/workspace` 内（ADR-0012:20），所以需要 RW Attachment 和足够的磁盘。
  - admin 的越权只写了删除（ADR-0010:26），admin 能不能替别人停 Sandbox 或建 Exec 没写。
  - 「admin deletes the Node」之后的级联没有定义：剩下的 Workspace 怎么处理、stopped Sandbox、Allocation、quota hold、Resource / 镜像缓存。
- 建议修法：在 ADR-0014 退役段写清顺序和级联。
  - 顺序：通知 owner 归档 → `paused` → drained → delete。
  - delete Node 等同于宣告 lost：Workspace 变为 `lost`（留墓碑），Sandbox 进入 S1 的终态，释放 Allocation 和 quota。

### M3. Idempotency-Key 的适用范围和 body 比较被收窄，但没交代

- #31 L14：所有异步 mutation 都用 Idempotency-Key。
- #33 L12：同一个 key 配不同的请求体，返回 `conflict`。
- ADR-0015:16 把它改成「Exec 上 caller key 的唯一约束」。这样一来：
  - Sandbox / Workspace 创建、Resource 发布还要不要幂等，没写。
  - 只有唯一约束的话，判断不了「不同请求体」，因为那需要另存请求摘要。
- 建议修法：在 ADR-0015 写明「每类创建请求各自对 key 建唯一约束，并另存请求摘要用于判 `conflict`」，或者明确放弃 body 比较。

### M4. 「两个具名挂载例外」与实际挂载数不符

- ADR-0009:24 写「the Capability Channel socket … and the read-only Exec Supervisor are the two named mount exceptions」。
- 实际还有：
  - ADR-0012:18：Resource 以只读方式挂进 Sandbox。
  - ADR-0013:16：credential helper「mounted next to the Capability Channel socket」，`stars-sandbox token` CLI 也必须在 Sandbox 里。
- ADR-0010:20 和地图 #29 L28 还停在「the one named exception」。
- 建议修法：ADR-0009:24 改为三类例外：通道目录（socket、helper、CLI）、Exec Supervisor、只读 Resource 挂载。ADR-0010:20 删掉「one」。

### M5. Provider 契约没有对单个 Exec 的控制面

- ADR-0009:12-20 只有五个操作，Exec 由 Supervisor 启动。下面这些需要控制单个 Exec，但都没有对应机制：
  - 取消或超时后先发 SIGTERM、等 grace 再强杀（#33 L16）。
  - 撤销 Lease 时终止 Sandbox 内全部 Exec（ADR-0015:14）。
  - 运行中写 stdin、调整 PTY 窗口大小。
  - Agent 不在时，超时由谁执行。
- 今天的修订删掉了「PTY lifecycle … remain above this contract」这句。现在没写 PTY 主端和 ring buffer 由 Supervisor 还是 Agent 持有，也没写 PTY 能不能活过 Agent 重启。ADR-0015:18 只说了 Exec 能活过。
- 建议修法：在 ADR-0009 Exec 段补一句：「Node Agent 经 Supervisor 的 Node 本地控制目录下发 signal / stdin / resize；超时由 Supervisor 本地执行；PTY 主端与 ring buffer 由 Supervisor 持有，同样跨 Agent 重启」。

### M6. Artifact 收集没有失败出口，还会和 Idle Stop、ephemeral 回收赛跑

- ADR-0012:20 写的是「Exec 终态后由 Node Agent 收集」。缺口有四个：
  1. 收集失败时（路径不存在、对象存储不可用、Node Unreachable）没有任何状态或 cause。
  2. 收集结果发布到哪个 Resource 名下没定义。
  3. Exec 一到终态，Idle Stop 的「no running Exec」条件（ADR-0015:33）就满足了。ephemeral Workspace 的宽限期（#32 L10）可能在收集完成前就删掉文件。
  4. Exec 已经是终态，收集失败无法反映到 Execution Result。
- 建议修法：
  - Exec 增加 `artifacts_collected_at` 和收集失败 cause。
  - Idle Stop 与 ephemeral 宽限期都加上「无待收集 Artifact」条件。
  - Artifact 声明时必须给出目标 Resource 名。

### M7. Node Agent 访问对象存储用什么凭据，没有写

- Node Agent 要发布 Artifact（ADR-0012:20），也要拉取 ResourceVersion（ADR-0012:18）。
- 但对象存储密钥只存在部署配置里（ADR-0010:16）。#37 L20 的「预签名 URL」只说了 Sandbox。用 Node 证书认证的只有 registry（ADR-0014:48）。
- 建议修法：在 ADR-0012 加一句：「Node Agent 读写 MinIO 一律使用 Control Plane 签发的预签名 URL，与 Upload 段同一路径」。

### M8. Snapshot 构建作业没有接入调度和状态机

ADR-0012:22 只写了「built as a `system`-priority job on a Node」。下面这些都没写：

- 它是不是 Sandbox 或 Exec，占不占 User Quota，有没有 admission deadline。
- 选哪类 Node：`docker build` 在 Podman / Local 上能不能做，Capability 表里也没有这一行。
- 构建所需的 ResourceVersion 怎么送到 Node（同 M7）。
- Build Secrets 从哪里来。ADR-0013:18 只定义了 Sandbox 创建时的注入。#36 L14 原来写的「Secrets may be short-lived build inputs」已随短时 Secret 一起作废，现在是悬空引用。
- `building` 状态如果落在 Unreachable Node 上，没有出口。
- Snapshot 有没有 current 指针。ADR-0012:24 的保留规则依赖它，:22 却没定义。
- 失败的版本怎么清理。

建议修法：定义「构建 = owner 名下一个 `system` 优先级的 Sandbox，跑一个构建 Exec」，这样调度、Secret 注入（ADR-0013）、终态、清理全部复用现有规则。Snapshot 和 Resource 一样有 current 指针，只有 `ready` 的版本能成为 current。

### M9. Sandbox 重启时，创建期绑定的东西是沿用还是重新解析，没有写

以下几项都受影响：

- Secret 只在「创建时」注入（ADR-0013:18），而重启意味着重新 `Create`（ADR-0009:22）。重启时 Secret 取当前值还是原值没写；被引用的 Secret 已删除时怎么办也没写。
- Resource 在创建时解析成具体版本（ADR-0012:18）。这个版本变成非 current 后，stopped Sandbox 算不算「引用」（ADR-0012:24）？如果不算，它可能被 TTL 回收。
- Egress Opening 可能在停止期间被撤销（ADR-0010:22）。
- SnapshotVersion 的 pin 同理。

建议修法：一句话定死：「重启沿用创建时解析的全部绑定；Secret 在重启时按名重新取值，缺失则重启失败；stopped Sandbox 计为引用」。

### M10. 证书过期后重新注册，会让原 Node 的持久 Workspace 成为孤儿

- Node 长时间离线、证书过期后，续期返回 `unauthenticated`，Agent 会自清身份（ADR-0010:14），只能用新的 Enrollment Packet 重新注册。
- 因为「a Node is that Agent」（ADR-0014:18），重新注册得到的是一个新 Node。
- 磁盘上的持久 Workspace 仍属于旧 Node 行，只能由 admin 宣告 lost。
- 建议修法：允许 admin 签发 Enrollment Packet 时绑定已有的 Node ID，重新注册即复用原 Node 行。

### M11. #40 的「不绑 Repository 就连 push 都不行」随 Repository 一起消失，但没有替代

- #40 L11 写授权目标是「Workspace 绑定的 Repository」，#40 L13 写「要连 push 都不行，就不给 Sandbox 绑 Repository」。两处都是悬空引用。
- 现状：
  - ADR-0011:26 规定通道不能 opt-out。
  - ADR-0013 下，每个 Sandbox 都能经通道拿到 owner 的 token。即使 `egress: none`，token 也能经 stdout 带出去。
- 建议修法：二选一。
  - 在 ADR-0013 Consequences 写明「无法对单个 Sandbox 禁用 `git.credential`」。
  - 或者加创建期参数 `git_credentials: false`。

### M12. 地图正文过期，也没有收录 ADR-0015

以下条目与现行决定不符：

| 位置 | 地图写的 | 现行决定 |
|---|---|---|
| #29 L18 | six-operation | 五个操作（ADR-0009:12） |
| #29 L22 | 「stable Attempt identities, bounded pre-start retries, single-controller PTY」 | 全被 ADR-0015 删除 |
| #29 L24 | `normal/high/system` | `user` / `system`（ADR-0015:29） |
| #29 L28 | 「only short-lived, revocable」 | 已被 #35 推翻（地图自己的 L30 也说 dropped，L28 与 L30 自相矛盾） |
| #29 L28 | 「credential-free」「the one named exception」 | 同 M4 |
| #29 L29 | 「Phase 1」 | 见第五节 |

另外两处：

- 地图没有 ADR-0015 这一行。
- 「Not yet specified」（#29 L40）缺 ADR-0015 引入的参数：admission deadline 默认值、PTY ring buffer 大小、provisioning 退避上限、每 GPU 的 CPU / 内存份额、磁盘水位。Idle 15 分钟和 PTY 5 分钟虽有默认值，也应登记。

建议修法：按上表重写这几行，并补一条 ADR-0015 索引行。

### M13. Workspace 状态机还有两个口子

- 存储故障发生在**活** Node 上时，Workspace 是 `unavailable`（#32 L14）。但 admin 只能对 Node 宣告 lost（ADR-0014:20），没有针对单个 Workspace 的 lost 动作。
- provisioning 退避到上限之后（ADR-0015:38），只多了一个 `provisioning_stalled` 条件，仍停在 `provisioning`。等它的 Sandbox 是否一直占着 Allocation 和 quota，没写。
- 建议修法：
  - 允许 admin 宣告单个 Workspace `lost`，并写一条 Audit Record。
  - 退避达到上限时，等待它的 Sandbox 进入 `failed` 并释放 Allocation；Workspace 保留，可以删除。

### M14. 持久 Workspace 没有任何上限，一个 User 就能让 GPU Node 对所有人不可调度

- stopped Workspace 的磁盘只计入 Node 磁盘预算，不计入 User Quota（ADR-0015:30）。
- 没有存储配额（ADR-0012:28）。
- Node 低于磁盘水位后，拒绝所有新分配（ADR-0015:35）。
- 建议修法：二选一。
  - User Quota 增加「持久 Workspace 个数」一维。
  - 或在 ADR-0015 Consequences 写明这是已接受的风险。

---

## 三、低（措辞、悬空小术语、局部未定）

- **L1. 审计粒度互相打架。** ADR-0010:28 / #37 L40 要求审计「every Gateway authorization decision」。#37 L32 又说 Gateway 对每个请求都授权，而 ADR-0010:28 / #37 L41 说不记数据面逐请求读写。两条合起来，每个文件 HTTP 请求都会产生一条审计。建议：Gateway 审计粒度定为「每次 WebSocket / 会话建立」。
- **L2. Gateway 范围和 Data Plane 里的「Git」不一致。** ADR-0014:41 的 `gateway` 一栏只写了「Data Plane WebSocket」，而 #31 L11 说 Gateway 还承载文件的 HTTP 数据。GLOSSARY.md:134 和 #31 L10-11 的 Data Plane 包含「Git」，但平台已经没有 Git 操作（ADR-0013:27），这是悬空引用。建议：表格改成「Data Plane HTTP and WebSocket」，术语表删去 Git。
- **L3. 通道 Capability 的取值说漏了 `best_effort`。** ADR-0010:33 写「reports it honestly as `enforced` or `unsupported`」，但 ADR-0011:26 与 ADR-0009:49 在 Local 上是 `best_effort`。建议：补上 `best_effort`。
- **L4. 两个悬空小术语。** ADR-0010:22 的「session addresses」没有定义，ADR-0014:46 的减法集里也没有它。GLOSSARY.md:172 用了「Node Identity」，但没有这个词条。建议：前者删掉或换成 ADR-0014 的五个监听面；后者补词条，或改成「Node certificate」。
- **L5. 通道 fencing 规则没有对象。** ADR-0011:20、:22 的「Workspace-mutating verbs + fencing」找不到对应的动词，唯一的动词 `git.credential` 不改 Workspace。#35 L6 的「no credential may travel on the channel」已被 #35 裁决推翻，这条没问题。建议：注明「目前没有改 Workspace 的动词，此规则留给将来」。
- **L6. 参数不一致。** ADR-0013:16 中 `stars-sandbox token <provider>` 按 provider 取，而动词 `git.credential {host}` 按 host 取。建议：动词同时接受 host 或 provider id。
- **L7. Local Runtime 上的 Git 凭据没交代。** `capability_channel` 在 Local 上是 `best_effort`，而 ADR-0009:34 规定 required Capability 必须 `enforced`，所以 Local 上 Sandbox 还能不能用 `git.credential` 没写。ADR-0013:16「Node Agent sets `credential.helper`」在 Local 上等于改宿主的 git 配置。建议：写明 Local 上照常可用但无隔离，helper 用 `GIT_CONFIG_*` 环境变量注入，不改宿主文件。
- **L8. 磁盘配额是否必需没写。** ADR-0009:45 中 disk capacity quota 在非 XFS 上是 `unsupported`，再加上 ADR-0009:34 的「required 必须 enforced」，非 XFS 的 Node 可能被判为不能创建；而 ADR-0015:35 的意思是这种 Node 照常可用。建议：声明磁盘配额从来不是 required Capability。
- **L9. PTY 闲置终止缺 cause。** PTY 闲置被终止后的 Execution Result cause 没定义（ADR-0015:22）。#33 L19「普通断开……不终止 Exec」对 PTY 已被实质推翻，但没点名。建议：cause 用 `pty_idle`，并注明 #33 L19 的那句对 PTY 改写。
- **L10. Node 本地记录不回收。** Supervisor 记录和 Log Spool 在 Node 上什么时候删除没写（ADR-0009:20；GLOSSARY.md:73-75），这与 ADR-0015:10「plug unbounded disk」的目标相悖。建议：落库确认后删除，并计入 Node 磁盘预算。
- **L11. 术语表与 #34 对不上。** GLOSSARY.md:217「Resource Reservation」的定义是「accepted request before ready」持有的容量，但 #34 L13 说排队只产生 quota hold，不锁 Node 容量；quota hold 也没有词条。建议：把 Reservation 定义为「已选中 Node、尚未 ready」，并补「Quota Hold」词条。
- **L12. 无 Snapshot 的 Sandbox 用哪个镜像没写。** ADR-0009:24 只说接受平台镜像 digest，ADR-0012:22 的基础镜像名单只是给 Snapshot 当基底用的。建议：创建请求必须填 SnapshotVersion 或 Platform Base Image digest，二选一。
- **L13. User 与 API Token 的生命周期没有决定。** ADR-0010:12 只定了 token 的形态，User 怎么创建、token 由谁签发、怎么轮换和吊销都没写。建议：admin 经 REST/CLI 创建 User、签发 token，吊销即删哈希，并写审计。
- **L14. OAuth 回调落在哪个监听面没写。** OAuth 回调 `/external-auth/<id>/callback`（ADR-0013:14）是浏览器发起的，不带 API Token；ADR-0014:38-44 的监听面表里没有它，用 OAuth `state` 把回调绑定到 User 也没写。建议：放在 `api` 上，作为不要求 API Token 的例外路径，用一次性 `state` 绑定 User。
- **L15. Tool API 怎么承载 PTY 没写。** Agent Tool API 是工具调用式接口，PTY 是 Gateway WebSocket（#31 L11、L15；ADR-0014:40）。「打开 PTY / 发输入 / 读输出 / 断线重连」映射成哪些工具调用、由谁持有 WebSocket，都没写。建议：在 #31 的薄层里写一句「Tool API 服务端代持 WebSocket，以 `pty.write` / `pty.read(after_seq)` 暴露」。
- **L16. ADR 里残留一处 phase。** ADR-0012:35 写「Which phase ships Resource and Snapshot is `#39`'s call」。

---

## 四、端到端走查

图例：✔ 有决定覆盖；△ 部分覆盖；✘ 没有任何决定覆盖。

### (a) 新 User → 1 GPU Sandbox（新持久 Workspace）→ clone 私有仓库 → 训练 Exec 看日志 → Agent 重启 → 收 Artifact → 闲置停止 → 次日重启

| 步骤 | 覆盖 | 出处 / 缺口 |
|---|---|---|
| 1. 创建 User、拿 API Token | ✘ | 只定了 token 形态（ADR-0010:12），创建与签发没有（L13） |
| 2. 创建 Sandbox：gpu_count=1，资源规范化 | ✔ | #34 L11-12 |
| 3. quota 检查与排队 | ✔ | #34 L13；ADR-0015:30-32 |
| 4. 选 Node，与新 Workspace 同 Node | ✔ | #34 L10 |
| 5. 排队期间的 Sandbox 状态 | ✘ | S1 |
| 6. 选镜像 | ✘ | L12 |
| 7. Workspace persistent、provisioning | △ | #32 L10、L14；ADR-0015:38；退避上限后的出口缺（M13） |
| 8. 创建 Runtime：CDI GPU、cgroup 回读、能力检查 | ✔ | ADR-0009:34、:43 |
| 9. Runtime 创建失败的出口 | ✘ | S1 |
| 10. Egress 默认 `public`，Secret 以环境变量注入 | ✔ | ADR-0010:22；ADR-0013:18 |
| 11. Idle Stop 默认 15 分钟 | ✔ | ADR-0015:33 |
| 12. 关联 GitHub 账号（OAuth） | △ | ADR-0013:14；回调的监听面和 User 绑定缺（L14） |
| 13. git clone：helper → `git.credential` → 审计 → 直连远端 | ✔ | ADR-0011；ADR-0013:16 |
| 14. clone 需要 RW Attachment | ✔ | #32 L12；ADR-0015:14 |
| 15. 建训练 Exec：Idempotency-Key、Sandbox 必须 ready | △ | #33 L11；ADR-0015:16；key 的范围与 body 比较未清（M3） |
| 16. WebSocket 看日志、按 seq 续传 | ✔ | #31 L11-12；ADR-0014:30、:32 |
| 17. Agent 重启：Supervisor 继续写，Agent 收养 Runtime 并从 last seq 续传 | ✔ | ADR-0009:20；ADR-0014:24；ADR-0015:18 |
| 18. Agent 重启期间客户端 WebSocket 的行为（断开或保持） | △ | 只能按 #31 L12 的重连处理 |
| 19. 期间若 Runtime 本身死了 | ✘ | S2 |
| 20. 训练结束，收集 Artifact | △ | ADR-0012:20 |
| 21. 收集结果的目标 Resource、失败出口、上传凭据 | ✘ | M6、M7 |
| 22. 闲置停止：Idle Stop → stopping → 释放 Compute → Runtime 删除 | ✔ | ADR-0015:33；#34 L16；ADR-0009:22 |
| 23. 闲置停止与未完成的 Artifact 收集赛跑 | ✘ | M6 |
| 24. 停止时 RW Attachment 是否释放 | ✘ | S3 |
| 25. 次日重启：在绑定 Node 上重新 admission | △ | #34 L10、L16；ADR-0015:32 |
| 26. 重启在状态机里的边、超时后的状态 | ✘ | S1 |
| 27. 重启时 Secret / Resource / Egress 绑定是否重新解析 | ✘ | M9 |
| 28. 重启时 Workspace 写权限冲突 | ✘ | S3 |
| 29. Node 离线超过证书寿命后，Workspace 是否还能找回 | ✘ | M10 |

### (b) Agent 用 Tool API 在沙箱里开 PTY、断线、重连

| 步骤 | 覆盖 | 出处 / 缺口 |
|---|---|---|
| 1. Agent 凭 `user` token 调 Tool API | ✔ | ADR-0010:12；ADR-0014:40 |
| 2. 创建 PTY 模式的 Exec | ✔ | #33 L19 |
| 3. Tool API 如何承载 PTY 的双向流 | ✘ | L15 |
| 4. Provider 接口里的 PTY 参数、窗口大小 | ✘ | M5 |
| 5. 通过 Gateway WebSocket attach，多个 attachment 都可输入 | ✔ | #31 L11；ADR-0015:22 |
| 6. 断线：不关闭 stdin、不终止 | ✔ | #33 L19 |
| 7. 5 分钟无 attachment 终止 | ✔ | ADR-0015:22 |
| 8. 闲置终止的 cause | ✘ | L9 |
| 9. 重连回放 | △ | ADR-0015:22 的 ring buffer |
| 10. 超出 ring buffer 后、以及与 Execution Log 的关系 | ✘ | S5 |
| 11. 退出状态是最后一个数据帧 | ✔ | ADR-0015:22 |
| 12. 断线期间 Agent 重启时 PTY 和 ring buffer 是否还在 | ✘ | M5 |
| 13. 断线原因是 Control Plane / Gateway 重启 | △ | 同一进程，重启后按 seq 重连（#31 L12；ADR-0014:12）；PTY 部分同 S5 |
| 14. PTY 连接维持 Idle Stop | ✔ | ADR-0015:33 |

### (c) admin 退役一台 GPU Node

| 步骤 | 覆盖 | 出处 / 缺口 |
|---|---|---|
| 1. 置 `paused`：不接新分配，未启动的分配回队列，不驱逐 | ✔ | ADR-0014:22 |
| 2. 等待 drained | △ | ADR-0014:22；Idle Stop 会帮忙，但 idle=0 的 Sandbox 永远不 drained，admin 也无权停别人的 Sandbox（M2） |
| 3. 迁走持久 Workspace：Exec 归档 → Artifact → 在别处恢复 | △ | ADR-0014:26；在 paused Node 上无法起 Sandbox、归档需要写 `/workspace`、owner 与 admin 由谁操作都没写（M2）；收集失败的出口缺（M6） |
| 4. 在新 Node 恢复：Resource 只读挂载后复制进 Workspace | ✔ | ADR-0012:18 |
| 5. 回到排队中、绑定在此 Node 的请求 | ✔ | 由 admission deadline 结束（ADR-0015:32） |
| 6. admin 删除 Node：吊销 = 删行并拒绝续期 | ✔ | ADR-0010:14；ADR-0014:20 |
| 7. 删除 Node 后的级联：剩余 Workspace、stopped Sandbox、Allocation、quota、缓存 | ✘ | M2、S1 |
| 8. 删除时仍有运行中的 Sandbox：拒绝还是强制 | ✘ | M2 |

### (d) 从 Snapshot 创建 Sandbox 并挂一个 Resource

| 步骤 | 覆盖 | 出处 / 缺口 |
|---|---|---|
| 1. 准备 Resource：manifest + 预签名 PUT + 发布 | ✔ | ADR-0012:12-16 |
| 2. 构建 SnapshotVersion：基底在名单上，输入声明 | △ | ADR-0012:22 |
| 3. 构建作业的调度、quota、Node 类型、Build Secret 来源、`building` 卡住、current 指针 | ✘ | M8 |
| 4. 创建请求引用 Snapshot 名还是 SnapshotVersion | ✘ | M8（只有 Resource 写了创建时解析，ADR-0012:18） |
| 5. Node 用证书按 digest 拉镜像 | ✔ | ADR-0014:48 |
| 6. Local Runtime 不接受镜像 digest，调度排除 | ✔ | ADR-0009:24、:53 |
| 7. 拉取失败后 Sandbox 的状态 | ✘ | S1 |
| 8. Resource 解析为具体版本、按内容地址缓存、只读挂载 | ✔ | ADR-0012:18 |
| 9. Resource 挂载被算作挂载例外 | ✘ | M4 |
| 10. Node 拉取 Resource 的凭据 | ✘ | M7 |
| 11. Resource 缓存的回收与磁盘计账 | ✘ | 同 L10 |
| 12. 挂载路径 | ✘ | 实现细节，可留到实现规格 |
| 13. 在 Local 上能否只读挂载 | ✘ | 未写 |
| 14. 被引用的 SnapshotVersion / ResourceVersion 不会被 TTL 回收 | △ | ADR-0012:24「nothing references」；stopped Sandbox 是否算引用缺（M9） |

---

## 五、仍写着 "Phase" 的地方

ADR 里只剩一处：ADR-0012:35（L16）。票据与地图中如下：

| 出处 | 原文要点 |
|---|---|
| #29 L4 | 「Phase 1 must define a runnable Docker-based core loop …」 |
| #29 L29 | 「a closed verb list (Phase 1: only `git.credential` …)」 |
| #31 L11 | 「Phase 1 has no multiplexed Exec stream and no HTTP-polling fallback」 |
| #32 L8 | 「Phase 1 provides no migration or cross-Node Workspace synchronization」 |
| #34 L11 | 「Phase 1 只支持整卡独占」 |
| #34 L16 | 「Phase 1 不支持在线 resize」 |
| #35 L6（上下文评论） | 「the channel's Phase 1 verb list」 |
| #35 L21 | 「Phase 1 不做文件交付，也不做原地刷新」 |
| #35 L23 | 「ADR-0011 的 Phase 1 动词表」 |
| #36 L49 | 「Phase 1 不做存储配额」 |
| #36 L50 | 「Resource / Snapshot 落在哪个阶段交给 #39」 |
| #37 L10 | 「Phase 1 不引入 OIDC」 |
| #38 L15 | 「Phase 1 单个控制层进程」 |
| #40 L15 | 「Phase 1 只放 #35 定义的 Git 远端动词」 |
| #41 正文 | 「Egress-proxy placeholder substitution is the upgrade path, not Phase 1」 |
| #39（OPEN） | 整张票就是 Phase 1/2/3 路线图，ADR-0012:35 指向它 |

另有若干「留给实现规格阶段」（#33 L23、#34 L23、#37 L42、#37 L44、#38 L28）。它们指的是规格写作阶段，不是 Phase 编号，可以保留。

---

## 附：已被明确取代、本审计不计为矛盾的旧内容

| 旧内容 | 出处 | 取代者 |
|---|---|---|
| `Destroy` 与六个操作 | #30；#31 L13 | ADR-0009:12、:63 |
| Workspace write Lease 的 TTL / 续租 | #32 L12 | ADR-0015:14 |
| Compute Lease 的 TTL | #34 L16 | ADR-0015:14 |
| ExecAttempt、Attempt `lost`、3 次重试、结果聚合 | #31 L10、L13；#33 L10、L12-14 | ADR-0015:16 |
| `log_storage_failed` | #33 L18 | ADR-0015:20 |
| 单一 PTY 控制者 | #33 L19 | ADR-0015:22 |
| 单一 gRPC 流承载输出 | #31 L13 | ADR-0015:24；ADR-0014:30 |
| 手写 OpenAPI | #31 L8 | ADR-0015:26 |
| `normal` / `high` 优先级 | #34 L14 | ADR-0015:29 |
| CPU / 内存 / 磁盘 quota；降 quota 时取消 hold | #34 L12-13、L20 | ADR-0015:30-31 |
| 排队无隐式 TTL；`timed_out` | #34 L15 | ADR-0015:32 |
| 短时、可吊销 Secret；0600 文件；原地刷新 | #37 L18；#40 L16；#41 正文 | #35 L21、L23；#41 评论；ADR-0013:18 |
| Repository 资源 | #35 正文 | #35 L17；ADR-0013:27（#32 L8、#40 L11、L13 的残留见 M11） |
| 冻结屏障；策略版本 | #36 L12、L14 | #36 L40、L42；ADR-0012:20、:22 |
| Agent 退出时 Local Runtime 子进程随之结束 | #38 L21 | ADR-0014:24 |
