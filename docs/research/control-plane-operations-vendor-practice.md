# 控制面运维形状 vs 厂商做法：多副本、作业队列、迁移、Agent 分发与版本、存活、可观测、Registry

调研日期 **2026-10-17**。只用一手来源（官方文档、钉死 tag/commit 的源码）；厂商原文保留英文。标「未核实」的是没找到一手出处的。

钉死的版本（下文链接里的 `@` 均指这些 commit）：

| 项目 | tag | commit |
|---|---|---|
| coder/coder | v2.38.0 | `c3c6a672821e8ac2472c45f3be7a3c600aa8dae1` |
| riverqueue/river | v0.49.0 | `da5a26e21c18c706e0cd48cef3e1e5016e00492c` |
| distribution/distribution | v3.1.2 | `3220848f15d9279c66a41aa0a257469e6aede1e9` |
| gitlab-org/gitlab-runner | v19.5.0 | `70f5c12dad80bfd8ab426843f38cae72fddc4e75` |
| buildkite/agent | v4.6.0 | `b537bbeea152836301b55814e1d6870112fef6ca` |
| actions/runner | v2.338.0 | `197ca027b90de199464dee3bb9ff198a1db60967` |

简写：`C/` = `https://github.com/coder/coder/blob/c3c6a672821e8ac2472c45f3be7a3c600aa8dae1/`，`R/` = `https://github.com/riverqueue/river/blob/da5a26e21c18c706e0cd48cef3e1e5016e00492c/`，`D/` = `https://github.com/distribution/distribution/blob/3220848f15d9279c66a41aa0a257469e6aede1e9/`，`BK/` = `https://github.com/buildkite/agent/blob/b537bbeea152836301b55814e1d6870112fef6ca/`，`GH/` = `https://github.com/actions/runner/blob/197ca027b90de199464dee3bb9ff198a1db60967/`，`GL/` = `https://gitlab.com/gitlab-org/gitlab-runner/-/blob/70f5c12dad80bfd8ab426843f38cae72fddc4e75/`。

---

## 0. 一句话结论

**大家收敛到的形状是：「无状态的控制面进程 × N + 一个 PostgreSQL 当唯一真相」。** 多副本靠共享 PG、行锁 `FOR UPDATE SKIP LOCKED` 抢活、`pg_try_advisory_lock`（Coder）或一张 TTL 租约表（River）选出「这一轮谁跑定时任务」；没有 Raft、没有独立的 leader 服务。迁移在进程启动时跑，用 advisory lock 串行化。Agent 版本由服务端单向决定：服务端要么直接发二进制（Coder），要么在 N 天后拒绝派活（GitHub 30 天）；**没有一家对 Agent 二进制做签名校验**，最多是 SHA-256 / ETag。

---

## 1. Coder（coder/coder v2.38.0）

### 1a. 多副本（HA）

- **共享单个 PostgreSQL，自动进入 HA。** 原文："Coder automatically enters HA mode when multiple instances simultaneously connect to the same Postgres endpoint." —— `C/docs/admin/networking/high-availability.md`。该页在文档清单里标 `"state": ["premium"]`（`C/docs/manifest.json#L923-L926`）；源码里 HA 协调器只有在 license 含 `FeatureHighAvailability` 时才换成 `tailnet.NewPGCoord`（`C/enterprise/coderd/coderd.go#L1092-L1101`）。**结论：多副本是付费功能**；开源版可以连同一个 PG，但跨副本 tailnet 协调不启用。
- **`replicas` 表 + 心跳。** `enterprise/replicasync`：每副本每 `UpdateInterval`（默认 5s）更新自己一行；`updated_at` 超过 3×interval 视为死，`DeleteReplicasUpdatedBefore` 每 `CleanupInterval`（默认 30min）清（`C/enterprise/replicasync/replicasync.go#L55-L61`, `#L148-L170`, `#L249-L252`）。副本之间还会互相 HTTP 探测（同文件 L43 注释）。
- **pubsub。** PG 实现是 `LISTEN/NOTIFY`（`C/coderd/database/pubsub/pubsub.go#L307-L311`, `#L415`）。**v2.38 起默认事件总线换成内嵌 NATS**（experiment `no_nats_pubsub` 可关；没配 `--cluster-host` 时回退 PG），但 `ReplicaSyncPubsub` 仍是 PG（`C/cli/server.go#L864-L911`）。
- **没有 leader 选举。** 单例任务各自用事务级 advisory lock 抢：`database/lock.go` 列出固定 lock id（`LockIDDBPurge`、`LockIDDBRollup`、`LockIDReconcilePrebuilds`…）（`C/coderd/database/lock.go`），用法如 dbpurge 里 `tx.TryAcquireLock(ctx, database.LockIDDBPurge)`，抢不到这轮就跳过（`C/coderd/database/dbpurge/dbpurge.go#L193`）。

### 1b. Provisioner 作业队列

- SQL `AcquireProvisionerJob`：`UPDATE provisioner_jobs SET started_at, worker_id WHERE id = (SELECT id ... WHERE started_at IS NULL AND completed_at IS NULL AND <类型/标签匹配> ORDER BY <人工优先于 prebuild>, created_at FOR UPDATE SKIP LOCKED LIMIT 1) RETURNING *`（`C/coderd/database/queries/provisionerjobs.sql#L1-L36`）。
- 卡死回收：`coderd/jobreaper/detector.go` —— RUNNING 5 分钟无更新算 hung，PENDING 30 分钟算死，每轮最多 10 个（`C/coderd/jobreaper/detector.go#L20-L36`）；回收时用 `GetProvisionerJobByIDForUpdate ... FOR UPDATE SKIP LOCKED`（`provisionerjobs.sql#L47-L57`）。
- 同样的 SKIP LOCKED 模式也用于通知派发（`queries/notifications.sql#L35`）。

### 1c. 数据库迁移

- **golang-migrate** + 内嵌 `iofs` 迁移文件；`coder server` 启动时 `ConnectToPostgres(..., migrations.Up)` 自动跑（`C/cli/server.go#L3241`, `C/coderd/database/migrations/migrate.go#L76-L135`）。
- 自定义驱动 `pgTxnDriver`，锁是 `SELECT pg_advisory_xact_lock($1)`（`C/coderd/database/migrations/txnmigrator.go#L46`），`LockTimeout` 调到 2 分钟以容忍多副本同时启动（`migrate.go#L105-L111`）。
- **有 down 迁移文件**（605 个 `*.down.sql`），`migrations.Down()` 存在，但文档的回滚手段是「升级前备份数据库」（`C/docs/install/operate/upgrade/esr-2.29-2.34-upgrade.md#L220`）；未找到面向运维的 downgrade 命令说明（未核实是否官方支持降级）。
- HA 升级建议：先 `kubectl scale --replicas=0` 或 `=1`，因为旧副本连接会挡住迁移拿锁（`C/docs/install/operate/upgrade/best-practices.md#L32-L59`）。

### 1d. Agent 二进制分发与版本

- **从 coderd 下载。** 模板注入的 bootstrap 脚本：`BINARY_URL=${ACCESS_URL}bin/coder-linux-${ARCH}`，`curl -fsSL` / `wget` 下载后执行（`C/provisionersdk/scripts/bootstrap_linux.sh#L13-L32`）。coderd 在 `/bin/` 挂文件服务（`C/site/site.go#L120`）。所以 **agent 版本 = 当次启动时 coderd 的版本**，重启工作区即升级。
- **完整性：只有 ETag=SHA1**，用途是缓存（`// not used for cryptography`）（`C/site/bin.go#L6`, `#L64`, `#L103`）。bootstrap 脚本不校验哈希或签名。
- **版本偏差提示（只提示，不拒绝）**：前端比较 agent 版本 < server 版本 → `Outdated`；agent API major < server API major → `Deprecated`（`C/site/src/utils/workspace.tsx#L112-L137`）。tailnet API 当前 2.12（`C/tailnet/proto/version.go#L94-L98`）。

### 1e. Agent 存活

- coderd 按 `AgentConnectionUpdateFrequency` 默认 **15s** 刷新 `last_connected_at`；`AgentInactiveDisconnectTimeout` 默认 = 2×，即 **30s**（最小 2s）（`C/coderd/coderd.go#L447-L456`）。
- 状态推导（`C/coderd/database/modelmethods.go#L685-L728`）：从未连上 → `connecting`，超过模板的 `connection_timeout` → `timeout`；`disconnected_at >= last_connected_at` → `disconnected`；`now - last_connected_at > 30s` → `disconnected`；否则 `connected`。**状态是读时从时间戳算出来的，不存枚举。**

### 1f. 可观测性与保留

- Prometheus：`--prometheus-enable`，默认地址 `127.0.0.1:2112`（**独立监听、默认只绑 loopback**）（`C/codersdk/deployment.go#L2646-L2658`）。
- 日志：`--log-human` / `--log-json` / `--log-stackdriver` 三种输出（`C/codersdk/deployment.go#L3398-L3420`）。
- Agent 启动日志硬上限：`workspace_agents` 表 `CONSTRAINT max_logs_length CHECK (logs_length <= 1048576)` + `logs_overflowed` 标记；provisioner 日志同样 1 MiB（`C/coderd/database/dump.sql#L3001`, `#L3781`）。
- **dbpurge**：所有副本都跑 ticker，**每 10 分钟**一次，抢 `LockIDDBPurge` 的才执行（`C/coderd/database/dbpurge/dbpurge.go#L22`, `#L129`, `#L193`）。删：agent 日志（`--workspace-agent-logs-retention` 默认 **7d**，只删非最新构建且 agent 已 7 天未连的；最新构建的日志永远保留）（`C/codersdk/deployment.go#L4951-L4955`）、agent stats、旧 provisioner daemon、通知、过期 API key、审计/连接日志（按配置，0 = 不删），批量 1k–10k 行（`dbpurge.go#L21-L50`）。

---

## 2. GitHub Actions self-hosted runner

- **默认自动更新**："self-hosted runners will automatically perform a software update whenever a new version of the runner software is available"；`config.sh --disableupdate` 关闭 —— https://docs.github.com/en/actions/reference/runners/self-hosted-runners
- **30 天规则**："If you do not perform a software update within 30 days, the GitHub Actions service will not queue jobs to your runner." 任何 major/minor/patch 都算可用更新；**严重安全更新则立即停止派活**直到升级（同上页）。
- 时机："automatically updates itself when a job is assigned to the runner, or within a week of release"（同上页）。
- **校验：只比 SHA-256，没有签名。** 服务端在更新消息里下发 `SHA256Checksum`，runner 下载后 `ValidateRunnerHash`；且注释写明 "Validate Hash Matches **if it is provided**" —— 不给哈希就不校验（`GH/src/Runner.Listener/SelfUpdaterV2.cs#L71`, `#L340-L359`）。

## 3. Buildkite agent / GitLab Runner

**Buildkite**
- **无自更新。** 升级方式只有「包管理器」或「重跑安装脚本」—— https://buildkite.com/docs/agent/v3/upgrading ；源码无 self-update 命令（`BK/clicommand/` 目录只有 `agent_pause/resume/stop/start`）。
- **版本偏差靠支持策略，不靠拒连**：自 2027-01-01 起每个 minor 线支持 1 年（9 个月 Supported + 3 个月 Deprecated 带升级警告），之后 Unsupported；"Unsupported agents are not blocked and may still connect and run jobs" —— https://buildkite.com/docs/agent/v3/version-support-policy
- **pause / stop**：`buildkite-agent pause --note ... --timeout-in-minutes 60`（暂停不接新活，超时自动恢复）、`resume`、`stop`（跑完当前作业再停）/ `stop --force`（取消当前作业）（`BK/clicommand/agent_pause.go#L15-L52`, `BK/clicommand/agent_stop.go#L15-L46`）。
- 信号：SIGTERM/SIGINT 第一次 = 优雅（跑完当前作业），第二次 = 强制（在 cancel-grace-period 内取消作业），**SIGQUIT = 直接强制**（`BK/clicommand/agent_start.go#L1454-L1505`）。注意与 GitLab 相反。

**GitLab Runner**
- **无自更新**（文档未提及，只说 "keep GitLab Runner updated"）—— https://docs.gitlab.com/runner/
- **版本偏差**："the GitLab Runner major.minor version should stay in sync with the GitLab major and minor version." "Older runners may still work with newer GitLab versions, and vice versa." "Backward compatibility is guaranteed between minor version updates." —— 同上页。**不拒连。**
- **优雅停机 = SIGQUIT**："Stop accepting new builds. Exit as soon as the running builds finish (graceful shutdown)."；SIGINT/SIGTERM = 中止所有构建，"Use twice to exit now" —— https://docs.gitlab.com/runner/commands/ ；源码 `GL/commands/multi.go#L143-L147`。`shutdown_timeout`：强制关停的超时秒数（`GL/common/config.go#L1656`）。
- **pause（cordon）在服务端**：UI 里 Pause 一个 runner，"does not accept jobs" —— https://docs.gitlab.com/ci/runners/runners_scope/ 。（API 字段名 `paused` 未在该页核实。）

## 4. Kubernetes（仅作对照）

kubelet 不得比 kube-apiserver 新，最多可旧 **3 个 minor**（<1.25 为 2 个）—— https://kubernetes.io/releases/version-skew-policy/ 。`kubectl drain` = 先把节点标为不可调度（cordon），再以尊重 PodDisruptionBudget 和优雅终止期的方式逐个驱逐 Pod，DaemonSet Pod 需 `--ignore-daemonsets` 跳过；维护后 `kubectl uncordon` 恢复调度 —— https://kubernetes.io/docs/tasks/administer-cluster/safely-drain-node/ 。即：**cordon（停止新分配）与 drain（清空存量）是两个独立动作，状态存在服务端的节点对象上。**

## 5. CNCF distribution registry（v3.1.2）

- **S3 驱动接 MinIO**：`regionendpoint` 指向 MinIO，v3 起必须配 `forcepathstyle: true`；可选 `redirectendpoint` 给下载用的公开地址（`D/docs/content/storage-drivers/s3.md#L20-L54`）。默认**拉取时 307 重定向到预签名 URL**，客户端直连 S3；不想暴露 MinIO 就 `redirect: disable: true` 让数据全走 registry（`s3.md#L10`, `D/docs/content/about/configuration.md#L595-L607`）。
- **认证只能选一个**：`silly`（仅开发）、`token`（外部签发 JWT，registry 用 `rootcertbundle` 或 `jwks` 验签，需配 `realm/service/issuer`）、`htpasswd`（只认 bcrypt，启动时读一次，"Only use ... with TLS"）、或不配（`configuration.md#L609-L728`）。token 规范见 `D/docs/content/spec/auth/token.md`。
- **监听**：主 API 一个 `http.addr`；`http.debug.addr` 是可选的**独立**监听，上面挂 `/debug/vars` 和 Prometheus `/metrics`，文档要求不对公网暴露（`configuration.md#L975-L1000`）。不需要别的 listener。
- **GC 是 stop-the-world**：先以 `maintenance.readonly.enabled: true` 重启（或停服），再跑 `registry garbage-collect [--dry-run] [--delete-untagged] config.yml`（`D/docs/content/about/garbage-collection.md#L83-L126`, `configuration.md#L533-L541`）。

## 6. Go 的 PostgreSQL 作业队列：River（v0.49.0）

- **抢活**：`river_job.sql` 的 fetch 用 `FOR UPDATE SKIP LOCKED`（`R/riverdriver/riverpgxv5/internal/dbsqlc/river_job.sql#L189`, `#L220`）。
- **唤醒**：插入时可选 `pg_notify(...)`，worker 侧 `LISTEN`；同时兜底轮询，`FetchPollInterval` 默认 1s、`FetchCooldown` 100ms；可开 `PollOnly` 完全不用 LISTEN（适配 PgBouncer 事务池）（`R/riverdriver/riverpgxv5/internal/dbsqlc/river_job.sql#L51`, `R/riverdriver/riverpgxv5/river_pgx_v5_driver.go#L1226`, `R/client.go#L46-L49`, `#L340-L353`）。
- **leader 选举 = 一张单行租约表**：`CREATE UNLOGGED TABLE river_leader(elected_at, expires_at, leader_id, name PRIMARY KEY CHECK (name='default'))`；选举 `INSERT ... ON CONFLICT (name) DO NOTHING`，续约 `UPDATE ... SET expires_at = now()+ttl WHERE leader_id=@me AND expires_at >= now()`，过期行被 `DELETE`（`R/riverdriver/riverpgxv5/internal/dbsqlc/river_leader.sql#L1-L40`）。默认每 5s（±1s 抖动）尝试，TTL 留 10s 余量（`R/internal/leadership/elector.go#L28-L30`）。
- **leader 才跑的**：周期作业入队器（PeriodicJobEnqueuer）、JobCleaner、JobRescuer、QueueCleaner 等维护服务（`R/internal/maintenance/queue_maintainer_leader.go`, `R/client.go#L786-L789`）。
- 其他 Go 库（neoq、gue 等）本轮未取证（未核实）。

---

## 对本项目可直接抄的形状

1. **多副本**：先**单副本**。要多副本时照 Coder：所有状态进 PG，进程无状态；副本心跳写一张 `replicas` 表（5s 更新、15s 判死）只用于发现同伴/展示，不选主。Node Agent 的 gRPC 流挂在哪个副本上就由哪个副本服务，跨副本通知用 PG `LISTEN/NOTIFY`（Coder 后来才换 NATS，我们没必要）。
2. **单例定时任务**：不做 leader 选举，照 Coder 每个任务一个固定 lock id，`pg_try_advisory_xact_lock` 抢到才跑，抢不到本轮跳过。比 River 的租约表少一张表、少一个续约循环。
3. **作业队列**：照 Coder `AcquireProvisionerJob` 一条 `UPDATE ... WHERE id = (SELECT ... FOR UPDATE SKIP LOCKED LIMIT 1) RETURNING *`；`NOTIFY` 唤醒 + 1s 轮询兜底；一个 reaper 按「RUNNING 无更新 N 分钟 / PENDING 超时」收尸。真需要重试/周期/唯一性再引入 River，不手写。
4. **迁移**：golang-migrate（或等价物）+ 内嵌 SQL，**启动时自动跑**，`pg_advisory_xact_lock` 串行化；回滚靠「升级前备份」，down 文件可写但不承诺降级。多副本升级先缩到 1。
5. **Agent 分发与版本**：Agent 二进制由 Control Plane 自己的 HTTP 端点按 `os/arch` 提供，版本永远 = 控制面版本（Coder）。gRPC 握手时 Agent 报版本 + API major；major 不同直接拒，minor 落后只标 `outdated`。比 GitHub 的「自更新 + 30 天拒派」简单。完整性：mTLS 通道下载 + 校验 SHA-256（GitHub 只做到这一步）；签名属额外项，没有厂商先例。
6. **Agent 存活**：照 Coder 存 `last_seen_at` / `disconnected_at` 时间戳，状态读时推导（流断 → 立刻写 `disconnected_at`；`now - last_seen_at > 2×心跳周期` → 视为断开）。不存状态枚举，就没有状态机要同步。
7. **Drain / cordon**：服务端一个 `paused` 布尔（GitLab Pause / Buildkite pause / k8s cordon 都是「停止派新活」），与「清空存量」分开。Agent 侧一个信号 = 不接新活、跑完退出（GitLab 用 SIGQUIT，Buildkite 用第一次 SIGTERM；我们选 SIGTERM 第一次优雅、第二次强制，与 systemd 默认一致）。
8. **可观测**：`/metrics` 独立监听、默认绑 loopback；`log/slog` JSON 输出；每条 Agent/Sandbox 日志设硬上限（Coder 1 MiB + `overflowed` 标志，用 CHECK 约束兜底）；一个 10 分钟 ticker 的 purge 任务（advisory lock 保护、分批删、保留期可配置，0 = 不删）。
9. **Registry**：一个 distribution 进程，S3 驱动指 MinIO（`regionendpoint` + `forcepathstyle: true`），`redirect.disable: true` 不暴露 MinIO；认证用 `token`（控制面签 JWT，registry 用 `jwks` 验签）——与控制面同一套短期凭据，不维护 htpasswd 文件。`debug.addr` 单独挂 metrics。GC 按 readonly 重启 + `garbage-collect` 的维护窗口做，不追求在线 GC。
