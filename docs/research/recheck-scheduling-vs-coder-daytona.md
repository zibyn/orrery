# 重查 #34（GPU 调度与 User 配额）：对照 Coder 与 Daytona

调研日期 **2026-10-17**。只用一手来源：钉死版本的源码与随源码发布的文档；厂商原文保留英文。标【推断】的是我的推理，不是厂商说的。

| 项目 | tag | commit / 说明 |
|---|---|---|
| coder/coder | v2.38.0 | `c3c6a672821e8ac2472c45f3be7a3c600aa8dae1` |
| daytonaio/daytona | v0.190.0 | 公开仓库的冻结点（2026-06 起核心开发转私有，见 `daytona-sandbox-runtime-semantics.md` 第 1 条） |

简写：`C/` = `https://github.com/coder/coder/blob/c3c6a672821e8ac2472c45f3be7a3c600aa8dae1/`，`D/` = `https://github.com/daytonaio/daytona/blob/v0.190.0/`。

复用的已有调研：`daytona-sandbox-runtime-semantics.md` §1.3–1.7（资源上下限、auto-stop、GPU 分配器）、`sandbox-isolation-and-gpu.md`（磁盘配额只在 XFS pquota 下生效）、`prior-art-sandbox-orchestration.md` §3（DRA 的「claim 即锁」）、`control-plane-operations-vendor-practice.md`（Coder 的 `FOR UPDATE SKIP LOCKED`）。那几篇第 402 行自己列出的缺口是「`apps/api` 的调度器实现没有读」，**本文补的就是这一块**。

---

## 先读这几条

1. **两家都没有「资源调度器 + 等待队列」。** Coder 根本不管算力：CPU/内存/GPU 由 Terraform 模板写死，交给底层（K8s 调度器、Docker 宿主）去放，它只有一个 **provisioner 作业队列**（排的是 Terraform 执行者，不是算力）。Daytona 有放置逻辑，但它是「按 runner 上报指标算 TOPSIS 分，前 10 名里随机挑一个」，**找不到 runner 就直接 400 `No available runners`**；只有「从镜像声明式构建」这条路径会进 `PENDING_BUILD` 让对账器每 10 秒重试。**我们的「全局优先级 + 可行集 FIFO 持久队列」比两家都重。** 但形状上并非无先例：Coder 的作业获取就是「按 tag 过滤可行集 → 人发起的优先 → `created_at` FIFO → `SKIP LOCKED`」，与我们的可行集 FIFO 同形。
2. **配额 hold 两家各有对应物，不是我们的发明。** Daytona 在 Redis 里先 `INCRBY` 一个 *pending usage*，再拿「当前 + pending」比配额，失败就回滚，并且 `PENDING_BUILD` 状态**计入算力配额**——这正是「排队请求占 quota hold」。Coder 没有 hold，配额在 Terraform plan 出 `daily_cost` 后、于 `SERIALIZABLE` 事务里一次提交。**保留**；我们用同一个 PG 事务写 hold，比 Daytona 的「Redis 计数 + 60 秒 TTL 兜底」更简单。
3. **「Compute Lease 带 TTL 与续租」在我们自己的其它决定下是空转的，应简化。** #34 自己写「unknown 不释放」，ADR-0014 写「Node 从不因计时器被判 lost，Unreachable 只停新放置」。那么 Compute Lease 过期后**不能**释放 GPU（否则就是双分配），TTL 与续租就没有任何效果。两家都没有算力租约：Daytona 的注释原话是 GPU 卡 *"stays reserved for that sandbox until it is fully destroyed"*。**留一个单调 generation（fencing token）挂在 Resource Allocation 行上即可，删掉 TTL/续租。**
4. **两家都防了而我们没防：闲置回收。** Daytona 默认 **15 分钟无活动 auto-stop**，Coder 有模板级 default autostop + activity bump。我们的 Sandbox 没有任何闲置停止或最长寿命；而我们又是**硬预留、不超卖、不抢占**——一个忘了停的 GPU Sandbox 会无限期占住卡，后面的队列永远等。**应修改：至少对 GPU Sandbox 加闲置 auto-stop。**
5. **两家都防了而我们没防：CPU Sandbox 挤占 GPU Node。** Daytona：*"GPU runners are exclusively reserved for GPU sandboxes."*，并把每个 GPU Sandbox 切成固定 16 vCPU / 256 GiB / 512 GiB。我们的可行集 FIFO 允许无 GPU 的 Sandbox 落在 GPU Node 上吃光 CPU/内存，把 GPU 变成碎片。**应修改。**
6. **排队没有隐式 TTL，是两家都踩过的坑。** Coder 文档原话：作业会 *"hang indefinitely (these are in a `Pending` state)"*，原因是 *"misconfigured tags or unavailable provisioners"*；它的 job reaper 把 **pending 超过 30 分钟**的作业终止。我们的「暂时不可满足继续排队、无隐式 TTL」会让绑定在 paused/Unreachable Node 上的 Workspace 请求永远挂着、永远占 quota hold。**应修改：给一个默认 deadline。**

---

## 逐条裁决对照表

| # | #34 裁决 | Coder | Daytona | 判定 |
|---|---|---|---|---|
| 1 | admission 是原子 bundle：GPU+CPU+内存+磁盘+Workspace locality 同时满足，无部分就绪 | 无此概念；资源由模板决定，交给 Terraform/K8s | 选 runner 与写 `runnerId` 在一次请求内完成；GPU 用区域级 Redis 锁串行化 | **保留** |
| 2 | Scheduler 在单 Node 分配 GPU UUID；整卡独占，无虚拟化/跨 Node | GPU 由模板参数（`attach_gpu`）交给云/K8s，Coder 不分配 | 控制面只按「GPU 沙箱个数 ≥ runner.gpu」排除满载 runner；具体卡号由 runner 侧「标签 + 互斥锁」取最小空闲 index | **保留** |
| 3 | CPU/内存/磁盘硬预留、不超卖 | 示例 K8s 模板 `requests 250m/512Mi`、`limits` 取参数——依赖 K8s 超卖 | 分配量只是打分输入（optimal 100%、critical **500%**），未见硬 fit 检查 | **保留 CPU/内存；磁盘应修改**（只是记账，见下） |
| 4 | 排队请求产生 User quota hold；quota 覆盖运行中+已分配+排队中；超额直接 `quota_exceeded` | 无 hold；plan 后 `CommitQuota` 在 SERIALIZABLE 事务里比 `daily_cost` 总和与 budget，超额 **build 失败** | 先加 pending usage 再比较，超额抛错并回滚；`PENDING_BUILD` 计入算力配额 | **保留** |
| 5 | quota 维度：GPU、CPU、内存、磁盘多维 | 单一标量 credits（`daily_cost`），按 group allowance 求和 | 按 (org, region, class) 的 cpu/memory/disk/gpu 四维 | **应简化**（Phase 1 只限 GPU） |
| 6 | 优先级 `normal`/`high`/`system`；User 只能用获授权级别 | 隐式两级：人发起 > prebuild，写死在 SQL `ORDER BY` | 无优先级；前 10 名随机 | **应简化**（去掉用户可见的 `high`） |
| 7 | 全局按优先级、同级可行集持久 FIFO；不可满足的不阻塞后续 | 作业获取：tag 过滤 → 优先级 → `created_at` → `FOR UPDATE SKIP LOCKED` | 无队列；`PENDING_BUILD` 由 10 秒 cron 重试，无 FIFO 保证 | **保留**（Coder 同形） |
| 8 | 静态不可满足立即 `unsatisfiable`；暂时不足继续排队；无隐式 TTL，显式 deadline 到期 `timed_out` | pending 超 30 分钟被 reaper 终止 | 快照路径无 runner 立即 400；未找到 `PENDING_BUILD` 超时 | **应修改**（加默认 deadline） |
| 9 | Compute Lease 独立于 Workspace write Lease：lease ID、单调 fencing token、TTL、续租 | 无算力租约；provisioner 作业有 `worker_id` 归属检查（`you don't own this job`）与 5 分钟 hung 判定 | 无算力租约；状态变更用 Redis lock code 做围栏，卡在容器销毁前一直占着 | **应简化**（只留 generation，删 TTL/续租） |
| 10 | stopping 释放算力；stopped 只留 Workspace；重启重新 admission；不 resize | stop 即 Terraform 删掉 `start_count` 资源；quota 只剩持久卷的 `daily_cost` | GPU 沙箱强制 ephemeral（停即删）以便放卡；容器 stopped 仍占磁盘配额；支持热扩 CPU/内存 | **保留**；但**停着的 Workspace 磁盘应计入 quota**（见下） |
| 11 | 无闲置回收（#34 未提，GLOSSARY/ADR 也无） | default autostop + activity bump（默认 1 小时） | auto-stop 默认 15 分钟 | **应修改**（两家都有，我们缺） |
| 12 | 已有 Workspace 只在绑定 Node 排队；新 Workspace 与算力同 Node 选 | 不适用（持久卷由模板/K8s 决定） | 容器 stopped 后粘在原 runner；runner 不可调度且有备份时才迁走 | **保留**（我们不迁，#32 已定） |
| 13 | GPU Node 可放无 GPU Sandbox（#34 未禁止） | 不适用 | GPU runner 只给 GPU 沙箱；GPU 沙箱固定切片 | **应修改** |
| 14 | 不抢占、不迁移 | 运行中的 prebuild 作业 *"cannot be preempted"* | 有 spot（可抢占）、有迁移（stopped + 备份） | **保留** |
| 15 | quota 降低：不动既有，按持久化顺序取消超额的排队 hold | 降 budget 不动既有；新 build 若不增加花费，**超额也放行** | 未找到 | **应简化**（不主动取消排队） |
| 16 | draining：阻止新分配；未启动的回队列；运行中不抢占 | 无（底层 K8s cordon） | `unschedulable`/`draining` 两个布尔；draining 的 runner 连续 3 次检查无存活沙箱后下线 | **保留** |
| 17 | PostgreSQL 是调度状态源；allocation/hold/locality/UUID/lease 一个事务 | 作业获取与配额提交都在 PG 事务里 | 状态在 PG，配额 pending 与 GPU 分配锁在 Redis；用 `whereCondition` 条件更新做 CAS | **保留**（比 Daytona 少一个 Redis） |

---

## 逐条细节与出处

### 1. 原子 bundle 与放置（裁决 1、12、17）

**Coder** 没有放置逻辑。资源完全由模板里的 Terraform 资源决定，例如示例 K8s 模板的 Deployment 用 `count = data.coder_workspace.me.start_count` 控制启停、`wait_for_rollout = false`（`C/examples/templates/kubernetes/main.tf#L224-L229`）——**容量不够时 Pod 会在 K8s 调度器里 Pending，Coder 不感知**【推断：`wait_for_rollout = false` 意味着 Terraform 不等 Pod 就绪，排队实际落在 K8s 里】。

**Daytona** 放置在 `RunnerService.findAvailableRunners`：过滤 `state = READY`、`unschedulable ≠ true`、`draining ≠ true`、`availabilityScore ≥ 阈值`，GPU 请求再过滤 `gpu ≥ 请求数` 并排除满载 runner，有快照要求时只取已有该快照的 runner，最后 `sort((a, b) => b.availabilityScore - a.availabilityScore).slice(0, 10)`（`D/apps/api/src/sandbox/services/runner.service.ts#L297-L361`），再在这 10 个里 `Math.random()` 挑一个（同文件 `#L803-L821`）。GPU 型号偏好列表按顺序逐个尝试，全失败抛 `No available runners with GPU type: ...`。

并发安全：GPU 放置前拿区域级 Redis 锁，注释原文 *"two concurrent creates can pick the same already-full runner. Hold the lock until the runnerId is written to the DB."*（`D/apps/api/src/sandbox/services/sandbox.service.ts#L599-L606`）；状态变更用条件更新 `whereCondition: { pending: false, state: sandbox.state }`（同文件 `#L2131`）。

Workspace locality 对应：stopped 的容器沙箱粘在原 runner；只有 runner `unschedulable`/非 READY/分数过低且有完整备份时才清 `runnerId` 换 runner（`D/apps/api/src/sandbox/managers/sandbox-actions/sandbox-start.action.ts#L467-L511`）。

**判定：保留。** 我们在一个 PG 事务里同时写 allocation、GPU UUID、hold，比 Daytona「Redis 区域锁 + PG 写 + runner 侧互斥锁」三段更简单。DRA 的「claim 即锁、CAS 输了重试」（`prior-art` §3.2）是同一个思路。

### 2. GPU 分配（裁决 2、13）

**Coder**：GPU 只是模板参数，例如文档里的 `data "coder_parameter" "attach_gpu"`（`C/docs/admin/templates/extending-templates/parameters.md#L381-L386`）；envbox 模板用 `CODER_ADD_GPU=true` *"add detected GPUs and related files to the inner container"*（`C/examples/templates/kubernetes-envbox/README.md#L35`）。**Coder 不知道有几张卡、哪张空闲。**

**Daytona**：
- 控制面只数沙箱个数：*"A runner with runner.gpu = N can host up to N concurrent GPU sandboxes"*；STOPPED/ERROR 等状态都算占用，因为 *"the GPU index is pinned into the container's HostConfig.DeviceRequests at create time and survives any subsequent restart, so the physical card stays reserved for that sandbox until it is fully destroyed."*（`D/apps/api/src/sandbox/services/runner.service.ts#L980-L999`）
- 具体卡号在 runner 侧：Docker 标签 `daytona.gpu_index` + 互斥锁 + 扫全部容器取最小空闲 index（`D/apps/runner/pkg/docker/gpu_allocator.go`，已在 `daytona-sandbox-runtime-semantics.md` §1.7 详述）。
- **GPU runner 专用**：*"GPU runners are exclusively reserved for GPU sandboxes."*（`runner.service.ts#L313`）
- **固定切片**：`gpuSandboxCPUCores = 16`、`gpuSandboxMemoryGiB = 256`、`gpuSandboxDiskGiB = 512`，请求值被覆盖（`D/apps/runner/pkg/docker/container_configs.go#L26-L28`、`#L223-L224`）。
- **GPU 沙箱强制 ephemeral**：*"GPU sandboxes are always ephemeral: they get exclusive ownership of a runner for their lifetime and are auto-deleted on first stop."*（`sandbox.service.ts#L572-L575`）

**判定：GPU UUID 保留；「GPU Node 放无 GPU Sandbox」应修改。** UUID 比 index 稳定，控制面直接分配也省掉 Daytona 的两段式。但 Daytona 用「GPU runner 专用 + 固定切片」防住了「CPU 负载吃光 GPU 机器的 CPU/内存，卡空着却放不下 GPU Sandbox」——我们的可行集 FIFO 没有任何东西防这个。最小改法二选一：GPU Node 只接 `gpu_count > 0` 的请求（Daytona 做法）；或每张 GPU 在 Node 上预留一份 CPU/内存，无 GPU 请求不能动用。

### 3. 硬预留 vs 超卖（裁决 3）

**Coder**：示例 K8s 模板 `requests = { cpu = "250m", memory = "512Mi" }`，`limits` 取用户参数（`C/examples/templates/kubernetes/main.tf#L298-L306`）——requests 远小于 limits，**即依赖 K8s 的 Burstable 超卖**；示例 Docker 模板的 `docker_container` 不设 CPU/内存限制（`C/examples/templates/docker/main.tf#L174`）。

**Daytona**：每个 runner 上报 `currentAllocatedCpu/MemoryGiB/DiskGiB`，只作为 TOPSIS 打分的 7 个输入之一（`runner.service.ts#L1115-L1178`），默认 `optimal.allocCpu = 100`、`critical.allocCpu = 500`（`D/apps/api/src/config/configuration.ts#L230-L241`）——分配量到 500% 才视为「临界」。`assertRunnerCanHost` 的注释声称有 *"an explicit resource-fit check against the runner's currently reported allocations"*，但函数体只查 READY/unschedulable/draining/分数（`runner.service.ts#L824-L849`）。**未见硬 fit 检查**【推断：Daytona 有意按实际负载而非申请量放置，即超卖】。

**为什么两家不做硬预留**：未找到一手理由。【推断】Coder 把放置交给 K8s，自己不该再算一遍；Daytona 是多租户 SaaS，沙箱大多闲置，按申请量预留会浪费大量算力，所以用实测负载打分。

**判定：CPU/内存保留硬预留；磁盘应修改。**
- 理由：在单团队、几台机器的规模下，「Σ申请 ≤ 容量」是最简单的正确 admission，不需要 Daytona 那套 7 维 TOPSIS + 惩罚指数 + 节点指标上报。超卖必须有实时负载数据才安全。代价是闲置 Sandbox 浪费预留——这正是第 11 条要加闲置回收的原因。
- 磁盘：ADR-0009 已写 *"disk capacity quota | unsupported | unsupported | unsupported"*，Daytona 也只在 `d.filesystem == "xfs"` 时设 `StorageOpt`（`sandbox-isolation-and-gpu.md` §1.1、`daytona-sandbox-runtime-semantics.md` 第 144 行）。所以「磁盘硬预留」**只是 admission 记账，不是隔离保证**。#34 把三者并列叫「硬 reservation」会误导，应写明磁盘只是记账、不被强制。

### 4. User quota 与 hold（裁决 4、5、15）

**Coder**：
- 文档：*"Quotas are a mechanism for controlling spend by associating costs with workspace templates and assigning budgets to users."*；成本来自 `coder_metadata` 的 `daily_cost`，*"Since costs are associated with resources, an offline workspace may consume less quota than an online workspace."*；budget 是用户所在 group 的 allowance 之和（`C/docs/admin/users/quotas.md`）。
- 执行时机：*"Coder enforces Quota on workspace start and stop operations. The workspace build process dynamically calculates costs, so quota violation fails builds as opposed to failing the build-triggering operation."*（同上 `#L101-L106`）。实现：Terraform plan 完成后 `r.commitQuota(ctx, planComplete.GetDailyCost())`（`C/provisionerd/runner/runner.go#L991`），服务端在 `sql.LevelSerializable` 事务里算「每个 workspace 最新 build 的 `daily_cost` 之和 + 本次」与 budget 比较（`C/enterprise/coderd/workspacequota.go#L25-L107`，SQL 见 `C/coderd/database/queries/quotas.sql`）。
- 超额放行规则：*"If the new build will reduce overall quota consumption, then we allow it even if the user is over quota."*（`workspacequota.go#L66-L67`）——即降低 budget 不会追溯，也不会阻止「变便宜」的操作。

**Daytona**：
- 配额按 (org, region, sandboxClass) 分四维 cpu/memory/disk/gpu（`D/apps/api/src/organization/services/organization-usage.service.ts#L789`、`#L808` 的 Redis key 结构）。
- hold 机制原文：*"If a user action will result in increased quota usage, we will first increment the pending usage. When the user action is complete, this pending usage will be transfered to the actual usage. As a safeguard, an expiration time is set on the pending usage cache to prevent lockout for new operations."*（同文件 `#L885-L889`），TTL 是 `CACHE_TTL_SECONDS = 60`（`#L62`）。
- 校验：先 `incrementPendingSandboxUsage`，再比较 `current + pending > totalXxxQuota`，超额抛 `Total CPU limit exceeded...`，失败时 `rollbackPendingUsage`（`sandbox.service.ts#L170-L305`、`#L698-L709`）。区域无 GPU 配额时 *"Fail fast"*（`#L251-L253`）。
- **排队计入配额**：`SANDBOX_STATES_CONSUMING_COMPUTE` 包含 `PENDING_BUILD`；`SANDBOX_STATES_CONSUMING_DISK` 还包含 `STOPPED`（`D/apps/api/src/organization/constants/sandbox-states-consuming-compute.constant.ts`、`sandbox-states-consuming-disk.constant.ts`）。

**判定**：
- **hold 本身保留**——Daytona 正是这么做的，我们在 PG 同事务里做，不需要它的 TTL 兜底。
- **quota 维度应简化**【推断】：单团队里真正稀缺、需要在人与 Agent 之间分配的是 GPU；CPU/内存/磁盘的争用由 Node 容量与硬预留本身挡住。Coder 用一个标量都够用。Phase 1 只给 GPU 数一个 User quota（可选再加一个并发 Sandbox 数），其它维度等出现争用再加——与 ADR-0012「Phase 1 无存储配额」同一个口径。
- **quota 降低取消排队 hold 应简化**：Coder 降低 budget 不追溯，只在下一次 build 时生效。我们可以同样只在 admission 时检查；已接受的排队请求照常分配。省掉一个「按持久化顺序取消」的流程。
- **停着的 Workspace 磁盘**：两家都把停机仍存在的持久存储算进配额（Coder 的 `docker_volume` 带 `daily_cost` 例子；Daytona 的 *"counts against disk quota while stopped"* 与 `CONSUMING_DISK` 含 `STOPPED`）。#34 的 quota 覆盖「运行中、已分配未 ready、排队」，**没说 stopped Workspace 的磁盘算不算**。若 Phase 1 按上面只限 GPU，这条可以显式写「Workspace 磁盘不计 User quota，只计 Node 磁盘预算」；不写就是遗漏。

### 5. 优先级与队列（裁决 6、7、8）

**Coder** 的 provisioner 作业队列（`C/coderd/database/queries/provisionerjobs.sql#L7-L37`）：

```sql
WHERE potential_job.started_at IS NULL AND potential_job.completed_at IS NULL
  AND potential_job.organization_id = @organization_id
  AND potential_job.provisioner = ANY(@types :: provisioner_type [ ])
  AND provisioner_tagset_contains(@provisioner_tags :: jsonb, potential_job.tags :: jsonb)
ORDER BY
  -- Ensure that human-initiated jobs are prioritized over prebuilds.
  potential_job.initiator_id = 'c42fdf75-3097-471c-8c33-fb52454d81c0'::uuid ASC,
  potential_job.created_at ASC
FOR UPDATE SKIP LOCKED LIMIT 1
```

即：**可行集过滤（tag 匹配）→ 两级优先级 → FIFO**，形状与我们一致。但它排的是 Terraform 执行者，不是 CPU/GPU。文档：*"Human-initiated jobs have priority over pending prebuild jobs, but running prebuild jobs cannot be preempted."*（`C/docs/admin/templates/extending-templates/prebuilt-workspaces.md#L328`）。

排队的坑，Coder 自己写明：*"provisioners can cause jobs to fail or hang indefinitely (these are in a `Pending` state)"*，取消场景 *"It has been stuck in **Pending** for too long. This can be due to misconfigured tags or unavailable provisioners."*（`C/docs/install/operate/provisioners/manage-provisioner-jobs.md#L10`、`#L61`）。兜底是 job reaper：`PendingJobDuration = 30 * time.Minute`，`HungJobDuration = 5 * time.Minute`（`C/coderd/jobreaper/detector.go#L22-L28`）。

**Daytona** 没有队列：快照路径 `getRandomAvailableRunner` 抛 `No available runners` 后整个 create 回滚 pending 配额并返回 400（`sandbox.service.ts#L623-L629`、`#L698-L715`）。只有声明式构建（`buildInfo`）路径捕获这个错误并置 `PENDING_BUILD`（`#L998-L1007`），由 `sync-states` 每 10 秒的 cron 调 `handleUnassignedRunnerSandbox` 重试选 runner（`sandbox-start.action.ts#L73-L75`、`#L168-L209`；`D/apps/api/src/sandbox/managers/sandbox.manager.ts#L831`）。没有优先级、没有 FIFO 保证。⚠️ `daytona-sandbox-runtime-semantics.md` 第 50 行引用线上文档 *"GPU units wait in this state until GPU capacity is available."*；v0.190.0 随源码发布的 `sandboxes.mdx` 只写 *"Build Pending: the sandbox build is pending and will start shortly"*（`D/apps/docs/src/content/docs/en/sandboxes.mdx#L1896`）——**线上（闭源后）可能已把 GPU 改成排队，冻结源码里不是**。未找到 `PENDING_BUILD` 的超时。

**为什么两家都不做「算力排队 + 优先级」**：未找到一手理由。【推断】Coder 把算力交给 K8s，K8s 自带 Pending 队列与 PriorityClass，它再做一层是重复；Daytona 是低延迟沙箱 SaaS（文档卖点是快速创建），调用方是 SDK 程序，直接失败让其退避重试（限流文档就建议 *"1s, 2s, 4s, 8s"*）更符合它的形态。

**「排队等资源」vs「直接拒绝让调用方重试」**：
- 两家都更接近「直接拒绝」（Coder 在自己这层；Daytona 的主路径）。
- 直接拒绝的问题：没有公平性——谁重试得勤谁先拿到卡；Agent 会写出忙等循环；一张 GPU 释放时一群重试者同时抢。对 GPU 稀缺、多个 Agent 长时间等卡的单团队，这比队列更糟。
- 排队的问题：Coder 已经踩过——请求无限挂起（tag 配错、执行者不在），需要 reaper；队列积压挤掉人的请求（prebuild 那段原文）；还要额外的取消/超时/hold 语义。
- **我们的选择（排队）有产品理由**（单团队、Agent 原生、GPU 稀缺，Agent 能等），而且 Coder 证明「可行集过滤 + FIFO + `SKIP LOCKED`」在 PG 上很小。

**判定**：
- **可行集 FIFO：保留**。
- **`high` 优先级：应简化**。两家都没有用户可选的优先级；Coder 的两级是「人 vs 系统后台」，正对应我们的 `system` vs 用户。单团队里 `high` 需要一套授权、还会引出饥饿问题，Phase 1 留 `system` 与用户两级即可。
- **无隐式 TTL：应修改**。Coder 明确踩过无限 pending，并用 30 分钟 reaper 兜底。我们的「暂时不可满足」包括「绑定 Node 被 paused 或 Unreachable」——ADR-0014 规定 Node 不会因计时被判 lost，所以这种请求会永远排队并永远占 quota hold。给每个请求一个平台默认 deadline（调用方可改短或显式要求更长），到期 `timed_out` 释放 hold。

### 6. Compute Lease（裁决 9）

**Coder**：没有算力租约。provisioner 作业靠 `worker_id` 归属：每次回报都检查 *"you don't own this job"*（`C/coderd/provisionerdserver/provisionerdserver.go#L1095-L1096` 等处），超过 `HungJobDuration` 由 reaper 终止。workspace 的 `deadline`/`max_deadline` 是**闲置回收**，不是算力围栏（`C/coderd/database/queries/activitybump.sql#L1-L7`：*"Max deadline is respected, and the deadline will never be bumped past it. The deadline will never decrease."*）。

**Daytona**：没有算力租约。GPU 卡 *"stays reserved for that sandbox until it is fully destroyed"*（见 §2）。状态变更用 Redis lock code 围栏：*"Lost our lock: abort instead of returning. Callers create runner jobs right after the save, so a silent no-op would still fire them, duplicating containers and leaving a stale runnerId."*（`D/apps/api/src/sandbox/managers/sandbox-actions/sandbox.action.ts#L45-L64`）——这是一个**围栏 token**，没有续租。

**我们的内部矛盾**：#34 写 *"unknown 不释放、不复制、不迁移"*；ADR-0014 写 *"A Node is never declared `lost` by a timer"*、*"Unreachable stops new placement only"*。那么 Compute Lease 的 TTL 到期时只有两种可能：
- 释放资源 → 违反「unknown 不释放」，Node 恢复后同一张卡上跑两个 Sandbox（双分配）；
- 不释放 → TTL 与续租没有任何可观察效果。

**为什么两家不做**：未找到一手理由。【推断】TTL 租约的价值是「持有者失联后别人可以安全接管」；算力绑在物理机上，失联时没人能安全接管同一张卡，所以两家都把「占用」绑在容器/作业的生命周期上，只在**写操作**上做围栏。

**判定：应简化。** Compute Lease 退化为 Resource Allocation 行上的一个单调 generation：Node Agent 的启动/停止/回报都带 generation，Control Plane 以条件更新拒绝陈旧 generation（就是 Daytona 的 lock code、Coder 的 `worker_id`、`prior-art` §6 的 epoch）。释放只发生在确认停止、取消或 admin 宣告 lost。Workspace write Lease 的 TTL 有意义（写者可以换人），保持不变；GLOSSARY 的 **Lease** 定义随之只覆盖 Workspace。

### 7. 停止、闲置回收、resize、抢占（裁决 10、11、14）

**闲置回收**：
- Daytona：*"If the parameter is not set, the default interval of `15 minutes` will be used."*，且 *"The auto-stop triggers even if there are internal processes running in the sandbox."*（`D/apps/docs/src/content/docs/en/sandboxes.mdx#L1962`、`#L1974`）；实现是 10 秒 cron `auto-stop-check`（`sandbox.manager.ts#L97`）。它也承认代价：*"the sandbox may auto-stop mid-process because the process itself doesn't count as "activity""*（`sandboxes.mdx#L2077`）。
- Coder：*"Default autostop: How long a workspace runs without user activity before Coder automatically stops it."*，*"Activity bump ... (default: 1 hour)"*，另有 Dormancy 自动删除（`C/docs/admin/templates/managing-templates/schedule.md#L14-L22`）。

我们：GLOSSARY 与 ADR 里**没有**任何 Sandbox 闲置停止或最长寿命（只有 Exec 级 timeout）。

**判定：应修改。** 两家都有，而我们在「硬预留 + 不超卖 + 不抢占 + 排队」的组合下比两家更需要它：被遗忘的 GPU Sandbox 是唯一能永久堵住队列的东西。最小形态：GPU Sandbox 有平台默认闲置 auto-stop（以 Exec/会话/Gateway 流量为活动，比 Daytona 的白名单更宽以避免误杀长任务），可显式关掉。

**stop 释放算力、重新 admission：保留。** Daytona 强制 GPU 沙箱 ephemeral 就是为了 stop 即放卡；Coder 的 stop 删掉 `start_count` 资源。我们 stop 释放 GPU/CPU/内存、保留 Workspace，与两家方向一致。

**不 resize、不抢占、不迁移：保留。** Daytona 支持热扩 CPU/内存与 spot 抢占，但那是 SaaS 的弹性需求；Coder 也明说不抢占。少做是我们比两家简单的地方。

### 8. Draining（裁决 16）

Daytona：两个布尔 `unschedulable`、`draining`，放置时都排除；draining runner 连续 3 次检查其沙箱都 `desiredState = DESTROYED` 后下线（`runner.service.ts#L732-L775`、`#L789-L800`）。Coder 无此概念（交给 K8s cordon）。ADR-0014 的 `paused` 已与此同形。**保留。**

---

## 应简化 / 应修改 汇总

| 条目 | 判定 | 一句话 |
|---|---|---|
| Compute Lease 的 TTL 与续租 | 应简化 | 与「unknown 不释放」「Node 不按计时判 lost」矛盾，只能空转或导致双分配；留 allocation generation 做 fencing 即可。 |
| `high` 优先级 | 应简化 | 两家都无用户可选优先级；只留 `system` vs 用户两级（Coder 的「人 vs prebuild」同形）。 |
| quota 多维（CPU/内存/磁盘/GPU） | 应简化 | Phase 1 只限 GPU 数（可加并发数），与 ADR-0012 无存储配额同口径；Coder 只用一个标量。 |
| quota 降低时取消排队 hold | 应简化 | Coder 降 budget 不追溯；只在 admission 时检查即可。 |
| 排队无隐式 TTL | 应修改 | Coder 踩过无限 pending 并用 30 分钟 reaper 兜底；绑定在 paused/Unreachable Node 的请求会永远占 hold。 |
| 无 Sandbox 闲置回收 | 应修改 | 两家都有（15 分钟 / autostop+bump）；硬预留+不抢占下被遗忘的 GPU Sandbox 会永久堵队列。 |
| GPU Node 可放无 GPU Sandbox | 应修改 | Daytona 明确 GPU runner 专用并固定切片；否则 CPU 负载把 GPU 变成碎片。 |
| 磁盘「硬预留」 | 应修改（措辞） | 三个 Provider 都不强制磁盘容量，只是 admission 记账，应写明。 |
| stopped Workspace 磁盘是否计 quota | 应修改（补表态） | 两家都计；#34 未说，按简化后的「只限 GPU」应显式写不计。 |

## 未覆盖 / 未核实

- Daytona 线上（闭源后）的 GPU 排队行为：只有线上文档一句话（见 §5），无源码可读。
- Daytona `PENDING_BUILD` 是否有超时：在 `sandbox.manager.ts`、`sandbox-start.action.ts` 中未找到。
- 两家为何不做算力排队、不做硬预留、不做算力租约：均**未找到一手理由**，文中理由都标了【推断】。
- 线上 `daytona.io/docs` 在本次环境中无法访问，Daytona 文档一律引用 v0.190.0 随源码发布的 `apps/docs`。
