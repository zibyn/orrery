# 重查 #38（控制层与 Node 运维，ADR-0014）：对照 Coder 与 Daytona

调研日期 **2026-10-17**。只用一手来源（官方文档、钉死 commit 的源码）；厂商原文保留英文。没找到一手理由的标【推断】。以工作区里的 ADR-0014（含 Exec Supervisor、Node 退役、分流三处今日修订）与 ADR-0015 为准。身份、mTLS、出网由另一份 #37 重查负责，本文只在必要处点到为止。

| 项目 | tag | commit |
|---|---|---|
| coder/coder | v2.38.0 | `c3c6a672821e8ac2472c45f3be7a3c600aa8dae1` |
| daytonaio/daytona | v0.190.0 | `01c502bb1f1ff8f2885d0cd490e043736083dca8` |

简写：`C/` = `https://github.com/coder/coder/blob/c3c6a672821e8ac2472c45f3be7a3c600aa8dae1/`，`DT/` = `https://github.com/daytonaio/daytona/blob/01c502bb1f1ff8f2885d0cd490e043736083dca8/`。Daytona 的官方文档就在仓库 `apps/docs/` 里，按同一 commit 引用。

复用的既有调研：`control-plane-operations-vendor-practice.md`（Coder 的 HA、advisory lock、golang-migrate、agent 分发、存活推导、dbpurge、distribution），下文不再重复取证，只补 Daytona 和 Coder 侧的遗漏。另外参考 `recheck-api-and-exec-vs-coder-daytona.md` 第 64 行（Coder 日志表 UNLOGGED）、`recheck-scheduling-vs-coder-daytona.md` 第 57 行（Daytona 放置过滤）。

---

## 先读这几条

1. **周期任务的锁要换成事务级 advisory lock（应修改）。** ADR-0014 写的是 `pg_try_advisory_lock`，这是会话级锁。Coder 的单例任务全部走 `pg_try_advisory_xact_lock`，SQL 注释写明 *"This must be called from within a transaction. The lock will be automatically released when the transaction ends."*（`C/coderd/database/queries/lock.sql#L8-L13`）。会话级锁绑在连接池里的某条连接上，任务 panic 或忘了 unlock，这条连接就一直持有锁，这个任务从此再也跑不起来。我们的迁移已经用 `pg_advisory_xact_lock`，周期任务应该和它一致。
2. **MinIO 上游已归档，规格里别把它写成依赖（应修改）。** `minio/minio` 仓库已 archived，README 第一行是 *"THIS REPOSITORY IS NO LONGER MAINTAINED."*（`https://github.com/minio/minio`，`archived: true`）。Daytona 有一张开着的 issue 讨论替换（daytonaio/daytona#4617），维护者回复 *"Minio is only a development dependency for us"*。ADR-0012 和 ADR-0014 都直接写了 MinIO。应改成「S3 兼容对象存储」，MinIO 只作为一个可选实现。ADR-0012 里「是否校验 `x-amz-checksum-sha256`」那条待测项，也要换到实际选定的存储上去测。
3. **Agent 的 minor 比控制层新时要拒绝（应修改）。** Coder 的 `apiversion.Validate` 遇到 major 相同、对方 minor 更高时直接报错：*"server is at version %d.%d, behind requested minor version %s"*（`C/apiversion/apiversion.go#L49-L63`）。agent 和 provisioner daemon 握手都走这个函数（`C/coderd/workspaceagentsrpc.go#L52`、`C/enterprise/coderd/provisionerdaemons.go#L257`）。我们写的是「同一 major 内任何 minor 偏差都接受」，同时又要求「先升控制层」。只有拒绝更新的 minor，这条顺序才真正被执行。
4. **两家都防住了而我们没防：Docker 挂了但 Agent 还活着（应修改）。** Daytona 的 runner 每次心跳都带 `serviceHealth`，只要有一项不健康就把 runner 置为 `UNRESPONSIVE`，放置时只挑 `READY` 的 runner（`DT/apps/api/src/sandbox/services/runner.service.ts#L475-L488`、`#L297-L301`）。我们的心跳只刷新 `last_seen_at`。Runtime 已经死了，Agent 还在心跳，Scheduler 会继续往这个 Node 上放。应规定：Runtime 健康状况随心跳上报，不健康的 Runtime 像 Unreachable 一样停止接收新放置。
5. **部署件数不算多。** 我们有五件：控制层、Agent、PostgreSQL、S3 存储、registry。Coder 最少两件（coderd 和 PG，PG 甚至能由 coderd 自己拉起），Daytona 的 compose 核心是九件。多出来的 S3 和 registry 是 ADR-0012 的 Resource/Snapshot 功能需要的，Coder 没有这些功能，Daytona 有，而且 Daytona 也是「distribution + S3」这个形状。**PostgreSQL 存执行日志**这件事，Coder 是正例（加了上限，也删数据），Daytona 根本不往 PG 存日志，但也没有出事的反例。Coder 的教训是 *"PostgreSQL typically isn't technically fantastic at holding these types of event-based data indefinitely"*（coder/coder#20743），所以要配默认保留期，不能默认永久。
6. **可以少一个监听面：`api` 和 `gateway` 合并（应简化）。** 两者都用 API Token 认证。Coder 的 REST、PTY WebSocket 和 agent 连接共用一个 `--http-address`（`C/codersdk/deployment.go#L1790-L1792`）。`node` 和 `enroll` 要不要合并属于 TLS 配置问题，交给 #37。

---

## 逐条对照表

| # | ADR-0014 裁决 | Coder | Daytona | 判定 |
|---|---|---|---|---|
| 1 | **单进程**：REST、Tool API、Gateway 编进一个二进制；无权威状态；多副本只作为升级路 | 一个 `coderd`，内置 3 个 provisioner daemon（`C/codersdk/deployment.go#L3270-L3276`）；HA 是 premium 功能（既有调研 §1a） | API 是 NestJS 单进程，*"The snapshot builder and sandbox manager run as internal processes within the API"*（`DT/apps/docs/src/content/docs/en/architecture.mdx#L37`）；proxy、ssh-gateway 是独立进程；多副本靠 Redis 锁 | **保留**：三家都是「一个 API 进程 + PG」。Daytona 拆出 proxy 是为了按主机名路由预览，我们没有这个需求 |
| 2 | **不引作业队列**：意图表即队列，`NOTIFY` 唤醒加轮询兜底；周期任务各自 `pg_try_advisory_lock` | provisioner 作业用 `SKIP LOCKED`，jobreaper 回收（既有调研 §1b）。单例任务用**事务级** `pg_try_advisory_xact_lock`（`C/coderd/database/queries/lock.sql#L8-L13`）。PG pubsub 有 watchdog，*"time without receiving a heartbeat ... before we consider the watchdog to have timed out"*，5 分钟没收到就让 server 退出（`C/coderd/database/pubsub/watchdog.go#L17-L22`、`C/cli/server.go#L1393-L1394`） | 有一张 `job` 表，按 runner 投递：写 PG 后在事务外 `LPUSH` 到 Redis，*"If transaction rolls back, notification is harmless (runner will poll and find nothing)"*；runner 用 `BRPOP` 长轮询，DB 认领兜底（`DT/apps/api/src/sandbox/services/job.service.ts#L88-L90`、`#L122-L140`）；每分钟一个 cron 按类型把超时作业判 FAILED（30–120 分钟，`#L28-L34`、`#L403`）。单例 cron 用 Redis `SET NX EX` 带 TTL 的锁（`DT/apps/api/src/sandbox/common/redis-lock.provider.ts#L26`） | 意图表与 NOTIFY 加轮询：**保留**（比 Daytona 少一个 Redis；Coder 的 watchdog 说明 LISTEN 会悄无声息地断，所以轮询兜底**不能省**）。锁的函数：**应修改**为 `pg_try_advisory_xact_lock`，原因见「先读」第 1 条 |
| 3 | **迁移**：启动时 golang-migrate，`pg_advisory_xact_lock` 串行，只向前；回滚 = 恢复备份；升级 = 停机 → 备份 → 起新版 | 形状相同。文档原文 *"take a database snapshot since Coder does not support rollbacks"*（`C/docs/install/operate/upgrade.md#L7-L9`）。踩过的坑：*"Liveness probes can kill pods during legitimate long-running migrations"*，v2.30 起 Helm chart 默认关掉 liveness probe（`C/docs/install/operate/upgrade/best-practices.md#L66-L75`） | TypeORM；compose 里 `RUN_MIGRATIONS=true` 让 API 启动时跑迁移（`DT/apps/api/src/app.module.ts#L85`、`DT/docker/docker-compose.yaml#L93`）。生产用 expand/contract，分 `pre-deploy/` 和 `post-deploy/` 两个目录 *"to support zero-downtime deployments"*；另有 `--migration-revert`（`DT/apps/api/src/migrations/README.md#L1-L18`、`DT/apps/api/src/main.ts#L78-L91`） | **保留**：与 Coder 一致。Daytona 的 expand/contract 是 SaaS 滚动发布才需要的，单团队可以停机。补一句：`ops` 以后如果加健康检查，只能是 readiness，不能做成会在迁移中途杀进程的 liveness |
| 4 | **Node 准入**：只有 `admin` 签发 Enrollment Packet，签发要审计；建流并上报 Runtime Capabilities 即可调度；一台主机一个 Agent | 外部 provisioner 推荐用 scoped key，*"Save this authentication token, it will not be shown again"*；key 只存哈希（`C/docs/install/operate/provisioners/index.md#L45-L60`、`C/coderd/provisionerkey/provisionerkey.go#L19-L33`）。不推荐全局 PSK：*"make it difficult to rotate keys or isolate provisioners"*（同文档 `#L109-L113`）。外部 provisioner 是 premium 功能 | admin 或组织创建 runner 时返回一次性 `dtn_…` key（`DT/apps/docs/src/content/docs/en/bring-your-own-compute.mdx#L336-L346`）。key **明文**存在 `runner.apiKey` 列，按值直接查（`DT/apps/api/src/sandbox/entities/runner.entity.ts#L36-L37`、`runner.service.ts#L262-L263`）。安装脚本会问 *"Daytona Admin API Key"*，拿它在主机上自注册（`bring-your-own-compute.mdx#L842-L856`）。首次心跳后进入 `READY`，默认可调度（`runner.service.ts#L455`） | admin 签发、上报后即可调度、一机一 Agent：**保留**（三家同形；我们的一次性 Packet 比 Daytona 把 admin key 放到主机上更窄）。Packet 换证书这套机制比两家的静态 key 重，是否值得交 #37 审 |
| 5 | **存活**：`last_seen_at` 读时推导，2× 心跳间隔判 Unreachable；只停新放置；`lost` 只由 admin 宣告 | 读时推导，15s/30s（既有调研 §1e）；工作区从不因 agent 断线被判死。provisioner daemon 7 天没出现就自动删除（`C/coderd/database/queries/provisionerdaemons.sql#L150-L158`），但它是无状态 worker | **存储**状态 `UNRESPONSIVE`：每 10 秒一个 cron 检查，60 秒没心跳就标记（`runner.service.ts#L561`、`#L698-L719`）。runner 每 30 秒推一次健康信息（`DT/apps/runner/cmd/runner/config/config.go#L57`）。API 重启后给一段宽限期，*"Allow the runner a grace period to re-establish health checks"*（`#L702-L710`）。**没有 lost 概念**：沙箱不会因为 runner 失联被改状态（在 `apps/api/src/sandbox/managers/` 里 grep `UNRESPONSIVE` 零命中）。心跳带 `serviceHealth`，有一项不健康就判 `UNRESPONSIVE`（`#L475-L488`） | 读时推导、admin 宣告 lost：**保留**（比 Daytona 的存储状态简单；两家都不用计时器杀有状态的工作负载）。**应修改**：心跳要带 Runtime 健康，Runtime 不健康就停止新放置（「先读」第 4 条）。控制层重启后所有 Node 会短暂显示 Unreachable，因为这只影响放置，所以不需要 Daytona 那样的宽限期 |
| 6 | **Paused Node**：服务端一个 `paused` 标志；不驱逐；drained 是可查询的事实 | 不管节点，无对应 | **两个**标志：`unschedulable`（等同 cordon）和 `draining`（`runner.entity.ts#L161-L169`）。draining 时每 10 秒把已停止且已备份的沙箱迁到别的 runner，或原地归档；运行中的沙箱只有 `DRAINING_FORCE=true` 时才强停，默认关（`DT/apps/api/src/config/configuration.ts#L351-L356`、`DT/apps/api/src/sandbox/managers/sandbox.manager.ts#L300-L357`）；连续 3 轮没有存活沙箱就自动 `DECOMMISSIONED`（`runner.service.ts#L721-L787`） | **保留**：Daytona 的 draining 靠它的沙箱备份设施，我们没有 Workspace 备份，用一个 `paused` 够了。Daytona 默认也不强停运行中的沙箱，与我们的「不驱逐」一致 |
| 7 | **Agent 重启与升级**：运维动作；不自更新，不验签名；第一次 SIGTERM 优雅退出，第二次强制；Exec 由 Exec Supervisor 托着继续跑 | agent 二进制从 coderd 下载，重启工作区就升级（既有调研 §1d）；agent 退出时杀掉所有进程（`recheck-api-and-exec` 第 22 行） | 安装脚本下载 runner 二进制并装成 systemd 服务，Terraform 用 `runner_version` 钉版本（`bring-your-own-compute.mdx#L726`、`#L849-L856`）；无自更新。runner 收到 SIGINT/SIGTERM 只停自己的 API server，然后 `return 143`（`DT/apps/runner/cmd/runner/main.go#L311-L323`）；沙箱是 Docker 容器，不受影响 | **保留**：与 Daytona 同形（容器不随 runner 退出）。「第二次 SIGTERM 强制退出」两家都没有，但成本只是一个分支，可以留 |
| 8 | **Node 退役**：不迁移，不驱逐；调用方自己 archive → Artifact → restore；然后 admin 删除 Node | 无对应 | 有自动迁移和归档（见第 6 条）。**删除有护栏**：*"Cannot delete runner which is available for scheduling sandboxes"*、*"Cannot delete runner which has sandboxes associated with it"*（`runner.service.ts#L395-L415`） | 不迁移：**保留**。**应修改**：删除 Node 要加 Daytona 那样的护栏，只有已 paused、且没有未终结的 Workspace 时才能删；否则必须走显式的「宣告 lost」，不能让 delete 顺手把 Workspace 判死 |
| 9 | **协议兼容**：握手带 Agent 版本和 proto major、minor；major 不同就拒（`incompatible`）；major 内任何偏差都接受，只标 `outdated`；先升控制层；换 major 时停机一起换 | `Validate`：major 不同拒；major 相同时**对方 minor 更高也拒**；`WithBackwardCompat` 可以额外接受旧 major（`C/apiversion/apiversion.go#L28-L72`）。agent API 从 v2.0 起两年没换过 major（*"Shipped in Coder v2.8.0"*，`C/tailnet/proto/version.go#L13-L15`、`#L94-L95`）。前端只做 `Outdated`/`Deprecated` 提示（既有调研 §1d） | runner 的 `apiVersion` 在注册时就选定 `'0'` 或 `'2'`，存在 runner 行上，决定走哪条协议（`runner.service.ts#L101-L131`）；`appVersion` 只随心跳上报和展示（`DT/apps/runner/pkg/runner/v2/healthcheck/healthcheck.go#L108`）；**不拒任何版本** | major 门和 `outdated`：**保留**（与 Coder 同形）。**应修改**：Agent minor 大于控制层 minor 时拒绝（「先读」第 3 条） |
| 10 | **流**：一条控制流承载心跳、命令和状态；每个 Exec 和每个 PTY 走同一 HTTP/2 连接上的独立 gRPC 流 | *"dRPC over yamux over websocket"*（`C/tailnet/proto/version.go#L15`），一条连接上多路复用多个流 | runner v2 用 HTTP 长轮询取作业（`POLL_TIMEOUT` 30s）加单独的心跳请求（`config.go#L52-L58`）；输出不经过控制面（日志在沙箱文件里，`recheck-api-and-exec` 第 64 行） | **保留**：与 Coder「一条连接，多条流」同形 |
| 11 | **执行日志存 PG**：分块；每个 Exec 有上限；触顶写截断帧并标 `log_truncated`；保留期从终态起算；溢出转对象存储是升级路 | agent 日志和构建日志存 PG，上限 1 MiB 加 `logs_overflowed`；agent 日志表是 UNLOGGED（`recheck-api-and-exec` 第 64 行）。保留期：*"Logs from the latest build of each workspace are always retained"*（`C/docs/admin/setup/data-retention.md#L123-L129`）。PG 变大的教训：coder/coder#20743，*"PostgreSQL typically isn't technically fantastic at holding these types of event-based data indefinitely"*，提到的代价包括 *"Improved migration performance"* | 命令日志是沙箱文件系统里的文件，随沙箱删除；遥测走 OTEL，查询走 ClickHouse（`DT/apps/api/src/sandbox-telemetry/services/sandbox-telemetry.service.ts#L7`）；审计日志存 PG，可选推到 OpenSearch | **保留**：Coder 是正例；Daytona 不存 PG 是因为它的日志不独立于沙箱存在，不构成反例。补两点：(a) 日志表**不能**照抄 Coder 的 UNLOGGED，因为 PG 崩溃会清空 UNLOGGED 表，这违反 Exec 日志持久的承诺；(b) 保留期要有非零默认值，Coder 也是事后才补的 |
| 12 | **可观测**：结构化 JSON 输出到 stdout；`/metrics` 在 `ops`；不做分布式追踪，用 correlation id；每 10 分钟一个 purge 任务，删过期执行日志、审计记录、TTL 版本 | `--log-json`、Prometheus 独立监听 loopback（既有调研 §1f）；追踪**可选**，`--trace` 导出 OTEL（`C/codersdk/deployment.go#L3226`）。dbpurge 删的远不止日志：agent stats、旧 provisioner daemon、通知、过期 API key、telemetry lock、编排表……（`C/coderd/database/dbpurge/dbpurge.go#L206-L320`），分批 1k–10k 行（`#L21-L58`）。dbrollup 每 5 分钟把原始统计汇总成 insights（`C/coderd/database/dbrollup/dbrollup.go#L16-L19`、`#L57-L61`） | compose 默认开 OTEL，并带 Jaeger 和 otel-collector（`DT/docker/docker-compose.yaml#L8-L9`、`#L275-L287`）。审计清理每天一次，需配置 `AUDIT_LOG_RETENTION_DAYS`（`DT/apps/api/src/audit/services/audit.service.ts#L117-L136`） | 日志、metrics、不做追踪：**保留**（单进程用 correlation id 就够；Coder 的追踪也是默认关的）。rollup：不需要（我们没有使用统计）。**应修改**：purge 的范围不应写死三类，应定成「每张有过期行的表」，至少加上过期或已兑换的 Enrollment Packet、过期 API Token、过期 Idempotency Key、终态的准入请求；分批删 |
| 13 | **五个监听面**：`api`、`gateway`、`node`（mTLS）、`enroll`（TLS 无客户端证书）、`ops`（loopback） | 对外一个 `--http-address`/`--tls-address`；REST、PTY WebSocket、agent 连接都在上面（`C/codersdk/deployment.go#L1790-L1802`）；另有 loopback 上的 prometheus `127.0.0.1:2112` 和 pprof `127.0.0.1:6060`（`#L2656-L2658`、`#L2717-L2719`）。workspace proxy 是 premium 的独立进程，用于跨地域转发应用流量（`C/docs/admin/networking/workspace-proxies.md`） | 按进程分端口：API 3000（runner、proxy、ssh-gateway、otel-collector 都用各自的 key 打这个端口，`DT/apps/api/src/auth/api-key.strategy.ts#L79-L96`、`#L159-L162`）、proxy 4000、runner 3003、ssh 2222（`DT/docker/docker-compose.yaml#L5-L6`、`#L125-L126`、`#L149-L150`、`#L164-L165`） | **应简化**：`api` 和 `gateway` 认证相同，Coder 把它们放在一个端口上，合并后剩四个。`ops` 只绑 loopback：**保留**（与 Coder 相同）。`node`/`enroll` 拆不拆交 #37 |
| 14 | **OCI registry**：独立的 distribution 进程；S3 驱动接同一个 MinIO；`redirect.disable: true`；`clientcas` 用 Node 证书认证；blob GC 离线做 | 无 registry | `snapshot-manager` 是把 distribution v3 当库嵌进去的 Go 程序：S3 驱动，认证用 htpasswd basic，**没有设置 redirect**，所以 distribution 默认 307 到预签名 URL（`DT/apps/snapshot-manager/internal/server/config.go#L18-L79`、`DT/apps/snapshot-manager/go.mod#L6`）。registry 的用户名和密码存在 PG，随作业下发给 runner（`DT/apps/api/src/docker-registry/entities/docker-registry.entity.ts#L23-L27`、`DT/apps/runner/pkg/api/dto/registry.go#L6-L11`）。compose 用 `registry:2.8.2`（`docker-compose.yaml#L236-L252`）。GC 的做法：未找到 | 独立 distribution + S3、证书认证：**保留**（与 Daytona 同形，且不用像 Daytona 那样下发口令）。**应简化**：去掉 `redirect.disable`，理由见下文 §14。离线 GC：**保留**。MinIO 换成 S3 兼容存储的写法：**应修改**（「先读」第 2 条） |
| 15 | **部署件数**：控制层、Agent、PG、MinIO、registry | 最少 coderd + PG；`--postgres-url` 留空时 *"PostgreSQL binaries will be downloaded ... and store all data in the config root"*（`C/codersdk/deployment.go#L3606-L3608`）；workspace agent 在工作区里 | compose 核心：api、proxy、runner、ssh-gateway、dex、postgres、redis、registry、minio，共 9 件；开发辅助另有 pgadmin、registry-ui、maildev、jaeger、otel-collector（`DT/docker/docker-compose.yaml`） | **保留**：比 Coder 多出来的是 S3 和 registry，它们由 Resource/Snapshot 功能决定，Coder 没有这些功能；比 Daytona 少 Redis、dex、proxy、ssh-gateway |

---

## 需要展开的几条

### §2 / 「先读」1：事务级锁

Coder 的 dbpurge 和 dbrollup 都是 `InTx` 包住 `tx.TryAcquireLock(...)`（`C/coderd/database/dbrollup/dbrollup.go#L93-L109`、既有调研 §1a）。用 xact 锁有三个好处：锁的生命周期就是事务；不依赖连接池把同一条连接还给你；进程崩了，PG 会话结束时自动释放。ADR-0014 里的 Reconciler sweep、purge、GC sweep 都应写 `pg_try_advisory_xact_lock`。只有一种情况需要会话级锁：一轮工作必须跨多个事务（比如分批删除，每批单独提交）。那时要在一条专用连接上拿锁，并且写明这一点。【推断】purge 分批删时就属于这种情况。Coder 的做法是整轮放在一个事务里，每张表 `LIMIT` 一批，下一轮再继续（`C/coderd/database/dbpurge/dbpurge.go#L193-L320`），这样仍然可以用 xact。

### §5 / 「先读」4：Runtime 健康

Daytona 的写法：*"Runner ... services reported unhealthy"* → `updateData.state = RunnerState.UNRESPONSIVE`（`runner.service.ts#L481-L488`），而放置只挑 `state: READY`（`#L299`）。Coder 不管宿主，所以没有对应。我们的 GLOSSARY 已经把 Runtime Capabilities 定义成 *"what a Runtime Provider can enforce on that Node now"*。最小改法是：每次心跳都带上当前 Runtime 的健康，某个 Runtime 不健康时，Scheduler 把它当作不可放置（作用范围是这个 Runtime，不是整个 Node）。这样就不必新增状态。

### §8：删除 Node 的护栏

ADR-0014 的 Liveness 段写着 *"`lost` is an explicit `admin` act (declare the Node lost, or delete it)"*。也就是说，删除本身就等于宣告 lost。Daytona 把这两件事分开了：删除要求 `unschedulable`，并且没有未归档、未销毁的沙箱，否则报错（`runner.service.ts#L401-L410`）。【推断】Daytona 这样做，是为了防止 admin 误删一个还挂着数据的 runner。建议：`DELETE /nodes/{id}` 要求 Node 已 paused、且没有非终态的 Workspace，否则返回冲突错误，并提示先「宣告 lost」。宣告 lost 仍然是唯一一个能把 Workspace 判死的动作。

### §9 / 「先读」3：minor 方向

Coder `Validate` 的注释举了例子：*"given majors {1, 2} and minor 2 ... 2.0, 2.1, and 2.2 are supported, 2.3+ is not supported"*（`C/apiversion/apiversion.go#L37-L48`）。也就是：服务端可以比客户端新，客户端不能比服务端新。我们已经要求「先升控制层再升 Agent」，补上这条拒绝，这个顺序就由系统强制执行了。拒绝时复用 `incompatible` 的处理：Agent 保留身份、慢速重试、大声报错。

### §11：执行日志与 PG

- 正例：Coder 把构建日志和 agent 日志都放在 PG，靠 1 MiB 上限加 dbpurge 控制规模。
- 反例：两家都没有「PG 存日志出了事」的一手记录。最接近的是 coder/coder#20743 的复盘。那张 issue 说的是审计、连接、API key 这类事件表**默认永久保留**导致库变大、迁移变慢，修法是加保留期，不是把数据搬出 PG。
- 不要照抄的地方：Coder 的 agent 日志表是 UNLOGGED，理由是 *"Set the table to unlogged to speed up the inserts"*。UNLOGGED 表在崩溃恢复时会被清空，这与我们「Execution Log 是持久的、截断必须显式标出」直接矛盾。
- Daytona 为什么不存：它的命令日志就是沙箱里的 `output.log`，沙箱删了日志也就没了（`daytona-sandbox-runtime-semantics.md` §4.2）。我们的 Exec 日志要比 Sandbox 活得久，所以不能照它的做法。

### §13：`api` 与 `gateway`

ADR-0014 拆开这两个面，理由是认证不同。但表里两者都写着 *API Token*。两者都要加进出网的减法集，分开也没有给减法集带来任何好处。Coder 把 REST 和 PTY WebSocket（`/api/v2/workspaceagents/{id}/pty`）放在同一个 listener 上。【推断】拆开唯一可能的好处是以后可以单独扩容 Gateway，而这正是 ADR 已经否决的多副本方向。合并成一个 `api` 面之后：五个监听面变四个，配置少一项，减法集少一个地址。

### §14：`redirect.disable`

既有调研给出的理由是 *"不想暴露 MinIO 就 `redirect: disable: true`"*。但按 ADR-0012，Node Agent 本来就要从对象存储拉 Resource，用的是预签名 URL，所以 Node 到 S3 的网络本来就是通的，关掉 redirect 什么也藏不住。代价却很实在：每一层镜像的字节都要经过 registry 进程中转，而 GPU 镜像动辄几十 GB。Daytona 没有设置这一项，用的是 distribution 默认的 307 重定向。未找到 Daytona 的一手理由。【推断】用默认值，是为了让 runner 直接从对象存储拉 blob，不让 registry 成为吞吐瓶颈。前提是：redirect 生成的预签名 URL 的主机名，必须是 Node 能访问到的地址（`redirectendpoint` 可以单独配置，见既有调研 §5）。

---

## 判定汇总

- **保留**：单进程；意图表加 NOTIFY 加轮询兜底；只向前的启动迁移；admin 签发 Packet；读时推导存活；admin 宣告 lost；`paused` 单标志；运维手动升级 Agent；Exec Supervisor 托住 Exec；major 门与 `outdated`；单连接多流；执行日志存 PG 带上限；JSON 日志、`/metrics`、不做追踪；`ops` 绑 loopback；独立的 distribution + S3 + 证书认证；离线 GC；五件部署。
- **应简化**：合并 `api` 与 `gateway`；去掉 `redirect.disable`。
- **应修改**：周期任务改用 `pg_try_advisory_xact_lock`；MinIO 改写为「S3 兼容存储」；拒绝 Agent minor 大于控制层；心跳携带 Runtime 健康，不健康的 Runtime 停止新放置；删除 Node 加护栏，与「宣告 lost」分开；purge 覆盖所有有过期行的表；日志表禁用 UNLOGGED，保留期给非零默认值。

## 未覆盖

- Daytona registry 的 GC 怎么做：没找到。
- Daytona 云上实际部署了几个 API 副本：只能从 Redis 锁的存在推出是多副本，没有一手数字。
- Enrollment Packet 与 mTLS 的取舍：交给 #37。
