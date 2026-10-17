# Daytona 一手核对：沙箱/进程面、API 权威与生命周期语义

调研日期 **2026-10-17**。只收一手来源：`docs.daytona.io` / `www.daytona.io/docs`（含其 OpenAPI 与 `.md` 原文）、`github.com/daytonaio` 官方仓库源码（tag `v0.190.0`）。每一条断言后附直接 URL 或仓库路径；**凡是标 `[源码]` 的表示读的是 `daytonaio/daytona` 仓库文件**。

与既有调研的分工：`prior-art-sandbox-orchestration.md` §1.2 已写 Daytona 的**三平面与 runner 轮询、secret 占位符**，`sandbox-network-policy-apis.md` 已写**出网字段面**——本文**不重复**，只写增量（进程/PTY/日志/API 权威/资源边界/GPU）。凡是与那两份冲突的，以本文为准并注明。

> **取证强度**：文档原文一律保留英文。**「文档写了的」/「源码读出的」/「实测的」分别标注**；查不到就写 **[未证实]** 并写明查了哪里。本文**没有**做任何真机实测——Daytona 是托管服务，本机无凭据，所有运行时行为都是「文档声称」或「源码写法」，不是「我跑出来的」。**这是本份最大的证据弱点，别把源码读出的行为当成线上行为。**

---

## 0. 先读这几条

1. **公开源码在 2026-06 冻结了，且已经落后于线上 API。** `daytonaio/daytona` 的 README 顶部明写：*"As of June 2026, Daytona's core development has moved to a private codebase. This repository will receive no further updates, fixes, or releases."*（<https://github.com/daytonaio/daytona>）。冻结分支是 `v0.190.0`。**实测比对**：该 tag 里 daemon 生成的 toolbox 规范有 **83** 条 path，而线上发布的 `toolbox-openapi.json` 有 **96** 条——线上多了 `/env`、`/files/upload-v2`、`/git/config`、`/git/config/user`、`/git/credentials`、`/git/init`、`/git/remotes`、`/git/reset`、`/git/restore`、`/system/metrics`、四条 `/computeruse/*/{up,down}`，少了 `/files/upload`。**结论：想读「当前」语义只能读线上的 OpenAPI，想读「实现」只有一份过期快照。**
2. **权威是 HTTP API，不是 SDK 也不是 CLI。** 平台 API 规范是**从 NestJS 源码生成**的（`apps/api/src/generate-openapi.ts` 调 `SwaggerModule.createDocument(app, config)`）；各语言的 `api-client-*` / `toolbox-api-client-*` 都标着 `.openapi-generator/VERSION`，是被生成物；`sdk-*` 包在其上做人机包装；**CLI 用的是同一份生成的 Go 客户端**（`apps/cli/apiclient/api_client.go` 导入 `github.com/daytonaio/daytona/libs/api-client-go`）。**SDK 与 CLI 都不是独立语义，是同一份 HTTP 面的两个门面。**
3. **它是两个 API，不是一个。** 平台 API（对象生命周期，控制层）+ **toolbox API（数据面，跑在沙箱内的 daemon，另有一份独立规范与独立版本号）**。两者靠平台 API 返回的 `toolboxProxyUrl` 拼接：`{toolboxProxyUrl}/{sandboxId}/{endpoint}`。**数据面的字节永远经控制层 proxy 中转，没有直连通道。**
4. **能力不一致有四层表达手段，但没有「能力探测对象」。** ①`sandboxClass` 只有三个值 `linux-vm|container|windows`（**GPU 不是 class，是资源属性**）；②字段描述里逐个写「仅某 class 有效」（`kvm` 仅 linux-vm、`spot` 仅 GPU、`autoPauseInterval` 仅支持暂停的 class）；③文档里的 class × 功能支持矩阵；④**按 SDK 版本头改变响应形状**。**没有任何端点返回「这个后端能做什么」的结构化清单**——`/organizations/{id}/available-sandbox-classes` 只回 `{regionId, sandboxClass, gpuAvailable, allowedGpuTypes}` 四样。
5. **会话是内存态，不跨 daemon 重启恢复；日志是落盘文件。** `SessionService` 的 `sessions` 是 `cmap.ConcurrentMap[string, *session]`（纯内存），只有命令的 `output.log` / `exit_code` / `input.pipe` 落在 `{configDir}/sessions/{id}/{cmdId}/` 下。同一个 PTY 也是内存态 `cmap`。
6. **`exec` 超时默认 10 秒，超时是 SIGKILL 整个进程组。** 会话内解释器默认 600 秒、`0` 关闭。**没有任何取消/中止动词**——只有超时。
7. **auto-stop 的 15 分钟计时器只被「外部交互」重置**，且交互清单是白名单式枚举的：生命周期状态变更、preview 网络请求、活跃 SSH 连接、toolbox SDK 请求。文档**明写**「后台脚本、长时间无外部交互的任务不计」，因此**一个跑 20 分钟不发请求的任务会被自动停掉**。这条对我们「长任务」的语义是直接的反面教材。
8. **容器类是 `Privileged: true` 起的**（源码：`Privileged: gpuIndex == nil`），GPU 沙箱反而是**非特权**的——注释写明是为了让 CDI 的 per-device cgroup 规则生效。**「同一个产品里，特权是默认、非特权是例外」**，与我们的能力集选择方向相反。

---

## 1. 资源边界与生命周期（Sandbox / Workspace / Process / Exec）

### 1.1 对象模型：只有 Sandbox，没有 Workspace

Daytona **没有**「Workspace」这个对象。文档里的 `workspace` 一词只出现在**沙箱内的默认工作目录**语境（`WORK_DIR`、`/work-dir` 端点、git 路径相对家目录）。对我们两级模型（稳定身份的 Worker + 有界的运行）**Daytona 只有一级**：Sandbox 就是「有状态、跨 stop/start 存活、有 ID 的实体」，「运行实例」不单独命名。

一份**近似**的两级映射是 `Sandbox` + `Snapshot`：快照是一级持久对象，沙箱从快照创建。但快照是**模板**（不可变的起点），不是**运行记录**——语义与我们的 Workspace 运行不同。

来源：<https://www.daytona.io/docs/en/sandboxes>、<https://www.daytona.io/docs/en/snapshots>、`[源码] libs/toolbox-api-client/openapi.yaml`

### 1.2 生命周期状态：API 枚举是 22 个值

**源码/规范里**的 `SandboxState` 枚举（`https://www.daytona.io/docs/openapi.json` → `components.schemas.SandboxState`）：

```
creating, restoring, destroyed, destroying, started, stopped, starting,
stopping, error, build_failed, pending_build, building_snapshot, unknown,
pulling_snapshot, archived, archiving, resizing, snapshotting, forking,
pausing, paused, resuming
```

**文档页**（`sandboxes`）给出的是同一集合的**人类可读解释**，措辞值得直接引用几句：

- `Stopped` — 容器类：*"keep their filesystem on the runner"*；VM 类：*"offload filesystem state to nearby storage"*。
- `Paused` — *"The VM sandbox is paused with filesystem and memory state preserved."*（**仅 VM**）
- `Archived` — *"The container sandbox filesystem is stored in object storage."*（**仅容器**）
- `Pending Build` — *"GPU units wait in this state until GPU capacity is available."*（**GPU 产能不足 = 排队，不是失败**）
- `Unknown` — *"The default sandbox state before it is created."*

⚠️ **注意 `destroyed` / `destroying` 与 `deleted` 并存**：文档表格里有 `Deleted`（*"The sandbox has been deleted and no longer exists."*），而 API 枚举里叫 `destroyed`。**文档与规范用词不同**，实现按规范。

**`desiredState` 与 `state` 并列存在**（`components.schemas.Sandbox.properties.desiredState`），即 Daytona 是**声明式编排**：调用方/控制层写 desired，runner 对账 observed。另有 `recoverable`（*"Whether the sandbox error is recoverable."*）与 `errorReason`，以及 `PUT /sandbox/{sandboxId}/state`（`operationId: updateSandboxState`，无请求体）供 runner 回报。

### 1.3 资源上下限（文档数字，组织级）

`https://www.daytona.io/docs/en/sandboxes`：

| 资源 | 单位 | 默认 | 最小 | 最大 |
|---|---|---|---|---|
| CPU | vCPU | 1 | 1 | 4 |
| Memory | GiB | 1 | 1 | 8 |
| Disk | GiB | 3 | 1 | 10 |

上限是**组织配额**层面的，不是硬件的。`/docs/en/limits` 的分档池：

| 档 | vCPU | 内存 | 存储 | 访问门槛 |
|---|---|---|---|---|
| 1 | 10 | 20 GiB | 30 GiB | 邮箱验证 |
| 2 | 100 | 200 GiB | 300 GiB | 绑卡 + $25 |
| 3 | 250 | 500 GiB | 2,000 GiB | $500 |
| 4 | 500 | 1,000 GiB | 5,000 GiB | 每 30 天 $2000 |

⚠️ **`/docs/en/limits` 页面自相矛盾**：Tiers 小节写 Tier 1 是 `10 / 10GiB / 30GiB`，Limits 表写 `10 / 20 GiB / 30 GiB`。**原文如此，没有裁定依据。**

限流（`/docs/en/limits`，均为**每分钟**）：General 1 万 / 2 万 / 4 万 / 5 万（Tier 1→4），Sandbox creation 300 / 400 / 500 / 600，Sandbox lifecycle 与 general 同量级。超限回 **429**，响应体含 `statusCode`/`message`/`error`，**限流是按 header 后缀分族的**（`-anonymous`、`-authenticated`、`-sandbox-create`、`-sandbox-lifecycle`）。重试建议退避 *"1s, 2s, 4s, 8s, etc."*。

### 1.4 自动回收：四个独立计时器 + 一个硬 TTL

`/docs/en/sandboxes` 与 `/docs/en/persistence` 一致：

| 计时器 | 适用 | 默认 | 特殊值 |
|---|---|---|---|
| auto-stop | 容器、GPU | **15 分钟无活动** | `0` 关闭（*"run indefinitely"*） |
| auto-pause | VM（Linux/Windows） | 60 分钟（且此时 auto-stop 关闭） | `0` 关闭；与 auto-stop **互斥**（至多一个非零）；ephemeral 与 GPU 不允许 |
| auto-archive | 容器 | 停止 7 天 | `0` = 用最大值（**30 天**） |
| auto-delete | 全部 | **不自动删** | `-1` 关闭；`0` = 停下即删 |
| TTL（wall-clock） | 全部 | 关闭 | `0` 关闭；**按墙钟计时，任何状态都算**（started/stopped/paused/archived 一视同仁） |

**重置计时器的事件是白名单枚举的**：*"lifecycle state changes, preview network requests, active SSH connections, and Toolbox SDK API requests"*；**不重置**的是：*"non-toolbox SDK calls, background scripts, or long-running tasks without outside interaction"*。原文直接给出后果：*"a >15-minute task may be stopped mid-run."*

有个便宜的补丁端点：`POST /sandbox/{sandboxId}/last-activity`（`operationId: updateLastActivity`）——文档说它*"update[s] the sandbox's last-activity timestamp without altering its runtime state"*，就是给外部编排器用的。**这个「外部心跳」的形状值得单独记一笔**（见 §5）。

### 1.5 停止/暂停/归档各保住什么

`/docs/en/persistence` 的矩阵（**按 class 分**）：

| class | 文件系统跨 stop | 内存跨 stop | stop 时磁盘配额 |
|---|---|---|---|
| container | 保住，*"stays on the runner and counts against disk quota while stopped"* | **清空** | **仍占** |
| Linux VM / Windows | 保住（*"offloads it to nearby storage"*），并**释放配额** | 清空（用 pause 才保） | 释放 |
| GPU | **不保**：*"Ephemeral by design: deleted on stop, filesystem is not preserved."* | 不保 | — |

`/docs/en/limits` 用另一句话重申同一件事：容器停着也占配额是因为 *"occupy disk quota until archived"*，而 VM 停止时 *"their state is offloaded to a storage layer, which keeps resume fast while releasing quota."*

**归档（archive）只在容器类存在**，因为 VM 停止时已经做了等价的事。**Fork 与 pause/resume 只在 VM 类存在。热快照（含内存）只在 VM 类存在。**

### 1.6 Volume 与 Snapshot

- **Volume**（`/docs/en/volumes`）：**FUSE 挂载，后端是 S3 兼容对象存储**。*"Volumes are FUSE-based mounts…"*、*"Volume data is stored in an S3-compatible object store."* 一个卷可被多个沙箱同时挂（*"a single volume can be mounted to multiple sandboxes"*），**无事务、无锁**：*"FUSE-backed volumes are not transactional… If two sandboxes write to the same path concurrently, the last write wins."* 明确**不能**当块设备（*"can not be used for applications that require block storage access (like database tables)"*），且比本地文件系统慢。`subpath` 是 S3 key 前缀，隔离**由 FUSE 挂载边界强制**（*"Isolation is enforced at the FUSE mount boundary"*），不能以 `/` 开头、不能有 `..`、不能有 `//`。**每组织上限 100 个卷**，卷数据**不计入存储配额**。删除是异步的 `pending_delete` → `deleting`，**挂着活跃沙箱时删会 409**，且**不可恢复**。
- **Snapshot**（`/docs/en/snapshots`）：一级对象，从镜像/Dockerfile 构建或从沙箱捕获。状态机 `Pending → Building/Pulling → Active → Inactive`。**闲置两周自动转 Inactive**（*"Snapshots automatically become inactive after 2 weeks of not being used."*），Inactive 必须显式 activate 才能用。镜像引用**必须带 tag 或 digest，且不支持 `latest`/`lts`/`stable`**。
- **Fork**：复制 VM 沙箱的文件系统 + 内存成独立沙箱，有血缘（`/sandbox/{id}/ancestors`、`/parent`、`/forks`），**父沙箱在有活跃 fork 子时不能删**。
- **Warm pool**（`/docs/en/warm-pools`）：每个组织**每个快照每个 region 只能有一个**池；认领条件苛刻——快照、region、资源的默认值、默认用户 `daytona`、**且无自定义环境变量/卷/secret**；认领时把 name/labels/四个间隔/网络设置套上去。**「定制即失配」**（这一点 `prior-art` §1.2 已记过一次）。

### 1.7 GPU（与单节点调度直接相关，源码级）

**文档面**（`/docs/en/sandboxes`、`/docs/en/snapshots`）：

- 类型：NVIDIA **H100 / H200 / B300 / RTX Pro 6000 / RTX 4090 / RTX 5090** 与 **AMD Instinct MI355X**。
- **单沙箱最多 8 张卡**；文档称*"each GPU adding up to 16 vCPUs, 192GB RAM, and 512GB disk"*。
- **GPU 沙箱是 ephemeral 的**，停止即删，文件系统不保（要用 Volume）。
- **spot**：可被抢占，**不占组织的 GPU 配额**，无空闲产能时立即失败。文档另说 spot 需 Tier 2+。
- **独立的 GPU 配额**：*"GPU sandboxes use a separate GPU quota under the Earth region"*。
- **GPU 快照的 region 被强制覆盖**：请求的 `regionId` 被忽略，回报的是 `earth`。

**源码面**（`[源码] apps/runner/pkg/docker/gpu_allocator.go`、`container_configs.go`、`gpu_info.go`）——**这一组是本文最有增量价值的部分**：

- **单节点 GPU 分配器就是一个 Docker 标签 + 一把互斥锁**：`GpuIndexLabel = "daytona.gpu_index"`。`Acquire()` 流程是「加锁 → `ContainerList(All: true)` 扫全部容器 → 收 `daytona.gpu_index` 标签 → 返回 `[0, total)` 里最小的空闲索引」，**调用方必须在 `release()` 之前完成 `ContainerCreate`**（否则并发分配器看不到新标签）。**没有设备插件、没有 CRD、没有独立调度器。**
- **`exited` / `dead` / `removing` 的容器不占卡**：注释写明 *"Docker detaches the CDI device cgroup on exit"*。停止态 GPU 沙箱的重新启动由「按卡碰撞检查」处理。
- **GPU 沙箱被切成固定片**，与宿主机规模无关：`gpuSandboxCPUCores = 16`、`gpuSandboxMemoryGiB = 256`、`gpuSandboxDiskGiB = 512`，注释理由是 *"This keeps user-visible sandbox capacity consistent across heterogeneous GPU fleets (e.g. H100 NVL vs H100 SXM5 hosts)."* **请求里的 CPU/内存/磁盘在 GPU 沙箱上被丢弃。**
- **GPU 沙箱非特权；非 GPU 沙箱特权**：`Privileged: gpuIndex == nil`，注释 *"Privileged mode exposes every /dev/nvidia* node and bypasses the CDI cgroup rules, so GPU sandboxes have to opt out"*。
- **走 CDI，不是 `--gpus`**：`hostConfig.DeviceRequests = []container.DeviceRequest{{Driver: "cdi", DeviceIDs: []string{"nvidia.com/gpu=<idx>"}}}`。**与 `sandbox-isolation-and-gpu.md` §5 的结论同形。**
- **卡数由 `nvidia-smi --query-gpu=name --format=csv,noheader` 探测**（`gpu_info.go`），探测不到就是 0 卡。

⚠️ **文档与源码的一处已知分歧**：文档说「每张卡加 16 vCPU / 192GB / 512GB」（暗示随卡数累加），`v0.190.0` 源码是**固定 16 / 256 / 512 一刀切**。源码是冻结快照、线上可能已改，**两者都可能已过期，不裁定**。

### 1.8 容器怎么起的（`[源码] apps/runner/pkg/docker/container_configs.go`）

对我们最有用的几行：

- 资源限制走 Docker `Resources`：`CPUPeriod: 100000`、`CPUQuota: cpu*100000`、`Memory`/`MemorySwap` 同值。**有一套 `resourceLimitsDisabled` 开关可以整体关掉**。
- **磁盘配额走 `StorageOpt{"size": "<N>G"}`，且只在 `d.filesystem == "xfs"` 时设置**——与 `sandbox-isolation-and-gpu.md` §1.1 里我们自己核到的「overlay2 + XFS pquota」是同一机制。
- 运行时是配置项：`hostConfig.Runtime = config.GetContainerRuntime()`（env `CONTAINER_RUNTIME`）；另有分支显式 `hostConfig.Runtime = ""`。**`linux-vm` 类走的是同一条 Docker 创建路径的另一套 HostConfig，不是别的东西**（runner 目录下只有 `pkg/docker`，没有独立的 VM 包）。
- **沙箱内的 daemon 是从 runner 宿主 bind 进去的**：`binds = append(binds, fmt.Sprintf("%s:%s:ro", d.daemonPath, common.DAEMON_PATH))`——**daemon 二进制以只读挂载注入容器**。这条与我们「hook 与 Worker 是同一个二进制的两个入口」的形状同源。
- 网络：默认接 `RUNNER_BRIDGE_NETWORK_NAME`；linked sandbox 接 `linkNetworkName(id)`。
- runner 侧依赖里有 `github.com/coreos/go-iptables`（per-sandbox 规则）、`minio-go`（S3）、`docker/docker`、`moby/buildkit`、`go-containerregistry`：**节点侧就是一个 Go 服务 + 本地 Docker daemon + S3 + iptables**。

### 1.9 Runner 与控制层的协议

- 文档：*"Each runner **polls the control plane API for jobs**"*，作业类型含 create/start/stop/destroy/**resize**/backup（<https://www.daytona.io/docs/en/architecture>）。平台 API 里对应 `/jobs`、`/jobs/poll`、`/jobs/{jobId}/status`。
- 另有**一份 gRPC proto**：`[源码] libs/runner-proto/proto/runner/v1alpha1/job.proto`，只有三个 `JobType`：`JOB_TYPE_SNAPSHOT_SANDBOX` / `JOB_TYPE_FORK_SANDBOX` / `JOB_TYPE_PAUSE_SANDBOX`，用 buf 生成 Go/TS。
- **两件事的调用点本文没有读完** `[未证实]`：哪些作业走 REST 轮询、哪些走 gRPC，以及 runner 是否同时保留两条通道。
- ⚠️ 一处**版本偏差的实证**：`[源码] apps/runner/go.mod` 锁的是 `github.com/daytonaio/daytona/libs/api-client-go v0.149.0`，而仓库自己是 `v0.190.0`——**runner 落后自己仓库 41 个版本的客户端**。这说明「客户端版本被固定、服务端保持兼容」是他们的实际做法，不是理论。

---

## 2. SDK / HTTP API / CLI：谁是权威、怎么映射

### 2.1 生成链（每一环都有文件为证）

```
apps/api (NestJS, @nestjs/swagger)
  └─ apps/api/src/generate-openapi.ts   ← SwaggerModule.createDocument(app, config)
       └─ dist/apps/api/openapi.json (+ openapi.3.1.0.json 供 webhook 文档)
            └─ libs/api-client{,-go,-java,-python,-ruby}   ← .openapi-generator/VERSION
                 ├─ libs/sdk-{python,typescript,go,ruby,java}   ← 手写人机包装
                 └─ apps/cli/apiclient/api_client.go            ← CLI 直接用

apps/daemon (Go, gin + swaggo)
  └─ apps/daemon/pkg/toolbox/docs/swagger.{json,yaml}
       └─ libs/toolbox-api-client{,-go,-java,-python,-ruby}
            └─ libs/sdk-*（同一个包里 import，如 daytona_toolbox_api_client）
```

证据：

- `[源码] apps/api/src/generate-openapi.ts` — 从 NestJS app 生成文档，并额外注入 `x-enum-varnames`（注释举例：避免 `linux-vm` 被 OpenAPI Generator 生成成 `LINUX_MINUS_VM`）。**规范是从实现生成的，实现是权威。**
- `[源码] apps/daemon/pkg/toolbox/docs/swagger.json` 与 `swagger.yaml` 同时在仓库里；daemon 的 handler 注释带 `// @Router /process/session [post]` / `// @id CreateSession`——**toolbox 规范由 gin handler 注释生成**。
- `[源码] apps/cli/apiclient/api_client.go:16` — `apiclient "github.com/daytonaio/daytona/libs/api-client-go"`；CLI 还会在 transport 里读 `X-Daytona-Api-Version` 响应头，与自己的版本比对，不匹配就打 **warning**（`checkVersionsMismatch`）。
- `[源码] libs/sdk-python/src/daytona/_sync/process.py` — `from daytona_toolbox_api_client import (...)`，`Process.exec` 最终调 `self._api_client.execute_command(...)`。**SDK 的每个方法都能一一对应到一条 HTTP 路径。**
- 文档自述（README 与 `python-sdk` 页）：*"developer-facing SDKs backed by OpenAPI-generated REST clients and toolbox API clients"*。
- ⚠️ **SDK 文档页本身承认不了这件事**：`/docs/en/python-sdk` **没有**写「SDK 由 OpenAPI 生成」，也没有写「SDK 做不到而 API 能做的」。**「谁是权威」这个结论是从源码得出的，不是文档声明的。**

### 2.2 两个 API 的边界与拼接

- 平台 API base：`https://app.daytona.io/api`（SDK 示例）。版本 `info.version = "1.0"`，`description: "Daytona AI platform API Docs"`。
- toolbox API 的 base **不是常量**，规范里写死为：
  > *"The base URL comes from the sandbox's `toolboxProxyUrl` field (returned in sandbox DTO by the main Daytona API) plus the sandbox ID: `{toolboxProxyUrl}/{sandboxId}/{endpoint}`."*
  默认 `https://proxy.app.daytona.io/toolbox/{sandboxId}`。**toolbox 规范自己的 `info.version` 是 `"v0.0.0-dev"`——它不参与对外版本契约。**
- 取这个 URL 的端点是 `GET /sandbox/{sandboxId}/toolbox-proxy-url`（`operationId: getToolboxProxyUrl`）。
- 平台 API 的沙箱 DTO 里 `outboundProxyUrl` 与 `otelEndpointOverride` **只在单沙箱读时返回、列表里永不返回**——**字段级的存在性不一致被显式写进规范**（*"Only returned on single-sandbox reads — never on list responses."*）。

映射表（平台 API ↔ 数据面）：

| 面 | 覆盖 | 谁执行 |
|---|---|---|
| 对象生命周期（create/start/stop/pause/archive/delete/resize/fork/snapshot/backup/TTL/secrets/network/labels/ports） | `/sandbox/**`、`/snapshots/**`、`/volumes/**`、`/warm-pools/**`、`/regions/**`、`/runners/**`、`/jobs/**` | 控制层 API + runner |
| 数据面（文件、git、进程、PTY、LSP、computer-use、telemetry） | `/files/**`、`/git/**`、`/process/**`、`/lsp/**`、`/computeruse/**`、`/system/metrics` | **沙箱内 daemon**，经 proxy 中转 |

### 2.3 版本是协议的一部分（三种头）

| 头 | 方向 | 作用 |
|---|---|---|
| `X-Daytona-Api-Version` | 响应 | 服务端 API 版本；CLI 拿它与自己比对并告警 |
| `X-Daytona-Source` | 请求 | 客户端身份（`cli` / `sdk`），SDK 文档说 WebSocket 握手参数与之等价 |
| `X-Daytona-SDK-Version` | 请求 | **daemon 据此改变响应形状**；WebSocket 上以子协议 `X-Daytona-SDK-Version~<version>` 传递 |

`[源码] apps/daemon/internal/util/version.go`：

```go
func ExtractSdkVersionFromHeader(header http.Header) string {
	if v := header.Get("X-Daytona-SDK-Version"); v != "" { return v }
	// 否则从 Sec-WebSocket-Protocol 里找 "X-Daytona-SDK-Version~<version>"
```

daemon 里**按版本号分叉的兼容分支**（这是「同一端点、不同客户端拿不同形状」的直接证据）：

| 阈值 | 位置 | 行为 |
|---|---|---|
| `0.103.0` | `[源码] apps/daemon/pkg/toolbox/process/session/session.go:42` | 老 SDK 建会话默认落在**家目录**，新 SDK 不 |
| `0.27.0` | `[源码] apps/daemon/pkg/toolbox/process/session/execute.go:58` | 老 SDK 拿**合并后的 output**，不拿分离的 stdout/stderr |
| `0.161.0` | `[源码] apps/daemon/pkg/toolbox/process/session/log.go:71`（注释） | entrypoint 日志开始回 JSON（分离）而不是纯文本 |
| `0.167.0` | `[源码] apps/daemon/pkg/toolbox/process/session/common.go:28`（`SkipServerDemux`） | 新 SDK 自己 demux，服务端不再拆 |

另有逃生舱头 `X-Daytona-Split-Output: true`（`common.go:18` 的 `IsCombinedOutput`）。

**这条设计对我们直接有用**：它们**没有**为每个版本发一套端点，而是**一个端点 + 版本头 + 显式兼容分支**，且**未带头时按最老行为走**（`ExtractSdkVersionFromHeader` 无头返回 `""` → `IsCombinedOutput` 为真 → 合并输出）。

---

## 3. 运行时 / 资源能力不一致时怎么表达

Daytona 的答案是**四层，且全都是「写入 schema/文档的静态声明」**，没有运行时可探测的能力清单。

### 3.1 第一层：`sandboxClass`（只有三个值）

`components.schemas.SandboxClass = {enum: ["linux-vm","container","windows"]}`，`x-enum-varnames: ["LINUX_VM","CONTAINER","WINDOWS"]`。runner 侧的注册也用同一枚举：`CreateRunnerSandboxClass = {enum: ["container","linux-vm","windows", null]}`，*"Defaults to container when omitted or null."*

⚠️ **文档把 GPU 当第四个 class 讲**（`/docs/en/persistence` 的表格里 class 列写着「GPU」），**但 API 枚举里没有它**。按规范，GPU 是 `container` + `gpu: N` 属性。**这是「文档用语 vs 规范真值」的又一处分歧。**

### 3.2 第二层：把「仅某 class 有效」写进字段描述

`components.schemas.CreateSandbox.properties` 原文（这是**可以直接抄的形态**）：

| 字段 | 规范原文里的限定 |
|---|---|
| `kvm` | *"Expose KVM (/dev/kvm) inside the sandbox via nested virtualization. **linux-vm snapshots only. Requires the sandbox_kvm feature for the organization.**"* |
| `spot` | *"**GPU-only.** … **Ignored / rejected** when the sandbox requests no GPUs."* |
| `gpuType` | *"Accepts a single value or **an ordered preference list — the scheduler tries each in order and pins the sandbox to the first that has capacity**."* |
| `autoPauseInterval` | *"**Only supported for sandbox classes that support pausing.** Not allowed for ephemeral sandboxes."* |
| `linkedSandbox` | *"GPU sandboxes **cannot participate in links in either direction**…"* |
| `ttlMinutes` | *"**Subject to the maximum sandbox lifespan configured for the organization region and sandbox class**, in which case it also defaults to that maximum and **cannot be disabled**."* |
| `queueTimeout` | *"Applies only while the sandbox is unassigned in **pending_build or pulling_snapshot**."* |
| `includeMemory`（快照） | *"**VM sandboxes only.** When true the sandbox must be STARTED; when false (default) VM sandboxes must be STOPPED. **Container sandboxes do not support memory snapshots.**"* |
| `outboundProxyUrl` | *"On its own this is **convenience routing, not a security boundary**: it is applied by injecting the standard HTTP(S)_PROXY environment variables at creation, so a process that clears those variables egresses directly."* |

**最后一条值得单独抄下来**：他们把一个「看起来像安全边界、实际不是」的字段的**真实强度写进了 schema 描述**，并指明了升级路径（*"Combine with domainAllowList … which cannot be bypassed from inside the sandbox."*）。这是文档诚实度的正面样本。

### 3.3 第三层：文档里的 class × 功能矩阵

`/docs/en/sandboxes` 直接给一张「生命周期特性 × class」支持表：pause/resume 与 fork 仅 VM；archive 仅容器；内存态快照仅 VM；文件系统快照所有 class 都有。**没有 API 端点，就是一张表。**

### 3.4 第四层：按 SDK 版本协商响应形状（见 §2.3）

### 3.5 唯一的能力查询端点，只回四样东西

`GET /organizations/{organizationId}/available-sandbox-classes` → `AvailableSandboxClass[] = {regionId, sandboxClass, gpuAvailable, allowedGpuTypes[]}`。**这就是全部**——没有「支持 pause 吗」「支持磁盘配额吗」「支持 fork 吗」之类的字段。

### 3.6 资源不一致的其它表达

- **resize 的约束写在 schema 里**：`ResizeSandbox = {cpu≥1, memory≥1, disk≥1}`，文档补一句 *"Disk size can only be increased and cannot be decreased."*，并**警告 `nproc`/`free`/`top`/`/proc/cpuinfo` 读到的是宿主值，验证必须读 cgroup**（`cat /sys/fs/cgroup/cpu.max`、`memory.max`、`df -h /`）。**「限制生效值要能从沙箱内回读」这条与 `sandbox-isolation-and-gpu.md` §7 第 4 条完全撞上**——他们给的就是同一条建议，还给了具体文件。
- **region 之间的能力差异靠 BYOC 旁路**：`/docs/en/bring-your-own-compute` — 共享 region 两个（US/EU），**custom region "have no limits applied for concurrent resource usage"**，但需要自建 Kubernetes + 官方 Helm chart（`daytona-region` chart 部署 region proxy、可选 snapshot manager、注册 job）。**没有单节点部署的说法**。
- **配额池按 (org, region, class) 三元组算**（`/docs/en/scale`）：*"Compute pool caps total vCPU/RAM/storage across running sandboxes, scoped per organization, region, and sandbox class."*

> **缺口（他们自己也没解决）**：`/docs/en/scale` 明说它**不覆盖** autoscaling 策略、warm pool 之外的预热线、单 runner 密度、容量耗尽时的排队/准入控制、超卖比。我们 `sandbox-isolation-and-gpu.md` §7 要求的 `Capabilities()` 对象，**在 Daytona 这里没有对等物**——它靠的是「三个枚举 + 一张文档表 + 一堆字段描述」。

---

## 4. 断线、长任务、日志、PTY、状态持久化

### 4.1 `exec`：一次性、有超时、超时杀进程组

- 默认超时：*"The default timeout is 10 seconds when not specified."*（`/docs/en/process-code-execution`）
- `[源码] apps/daemon/pkg/toolbox/process/execute.go`：命令**通过 stdin 喂给 shell**（注释：*"Pipe command via stdin to avoid OS ARG_MAX limits on large commands"*），`Setpgid: true`，超时用 `time.AfterFunc` + `syscall.Kill(-pid, SIGKILL)`——**杀整个进程组**；超时回的是 `NewRequestTimeoutError("command execution timeout")`；启动失败或 wait 出错时**回 HTTP 200 + `exitCode: -1`**，错误文本放在 `result` 里（**HTTP 层看不出失败**）。
- **没有取消/中止动词**。文档给的方法级清单里没有任何 abort/cancel/kill-command；会话解释器的超时是 *"default 600, 0 disables it"*。

### 4.2 session：一个常驻 shell + 每命令三个 FIFO + 落盘日志

`[源码] apps/daemon/pkg/session/` 整个包。事实：

- **会话 = 一个常驻 shell 进程**（`exec.CommandContext(ctx, common.GetShell())`，`Setpgid: true`，注释说明是为了「signal the entire group … atomically on Delete」），`stdinWriter` 长期持有。
- 每个命令在 `{configDir}/sessions/{sessionId}/{cmdId}/` 下建 **`stdout.pipe` / `stderr.pipe` / `input.pipe` 三个 FIFO** + `cmd.sh`；两条 labeler 子进程分别读 FIFO、加前缀、**追加到同一个 `output.log`**；命令结束后向 `exit_code` 文件写一行。
- **前缀是 1 字节控制字符**：`log.STDOUT_PREFIX` / `STDERR_PREFIX`（源码注释与过滤循环显示是 3 字节重复序列，`0x01 0x01 0x01` 与 `0x02 0x02 0x02`）。demux 就是**扫前缀**（`DemuxLogBytes`），并且要处理**跨 chunk 被切断的前缀**（`processLogChunkWithPrefixFiltering` 里带残留 buffer）。
- **同步命令靠轮询 `exit_code` 文件**：`Execute()` 里 `for { select { case <-ctx.Done(): … default: os.ReadFile(exitCodeFilePath); if IsNotExist { time.Sleep(50ms); continue } } }`——**50ms 轮询一个文件**，然后读整个 `output.log` 返回。
- **`async=true` 时**：立即回 `202` + `commandId`；stdin 用 `while :; do sleep 3600; done > "$ip" &` 顶住（**不关 stdin，以便 `send_session_command_input`**）；同步命令则是 `cat /dev/null > "$ip" &`（**看到 EOF 即退**）。
- 命令 ID 冲突回 **409**（`command with the given ID already exists`）；会话不存在回 404。
- **保留会话 `entrypoint`（`EntrypointSessionID = "entrypoint"`）不可创建/不可删/不可在其中执行**——它是沙箱启动进程的日志出口，有专属端点 `/process/session/entrypoint`、`/process/session/entrypoint/logs`。

### 4.3 日志：同一条资源的两种交付模式

- **快照**：`GET …/process/session/{sid}/command/{cid}/logs` → 读整个 `output.log` 返回。
- **跟随**：同一路径 `?follow=true` **且请求带 `Upgrade: websocket`** → 走 WebSocket 增量推。
- 服务端行为随 SDK 版本变：新 SDK 自己 demux，服务端**只把携带前缀的原始字节流推出去**；`IsCombinedOutput` 时服务端在推送途中**滤掉前缀**（还要处理跨 chunk 残片）。
- WebSocket 侧有**keepalive pong 优先**的写法（`WritePendingPongs`，注释：*"this ensures keepalive pongs are never delayed by data writes"*）。
- 文档给的一条**反面警告**（`/docs/en/log-streaming`）：*"Blocking synchronous callbacks can cause WebSocket disconnections."*
- ⚠️ **文档没有写任何重连/重试逻辑**（`/docs/en/log-streaming` 原文只有上面那句）。`[未证实]`：断线后是「重连同一条流」还是「重新 GET 快照」——文档未表态，源码未见客户端侧实现（客户端在 SDK 里，属私有部分之后的演化）。

### 4.4 PTY：内存态管理器 + 显式 connect + lazy start

`[源码] apps/daemon/pkg/toolbox/process/pty/manager.go`：

- 全局单例 `ptyManager`，`sessions` 是 `cmap.ConcurrentMap[string, *PTYSession]`——**纯内存，无落盘**。
- `VerifyPTYSessionReady`：会话 `!Active` 且 `LazyStart` → **首次客户端连接时才真的起 PTY**；`!Active` 且非 lazy → 报 *"has terminated and is no longer available"*。**「lazy start」是一个 API 字段**（create body 里的 `lazyStart`），不是内部技巧。

文档侧（`/docs/en/pty`）的动词全集：

| 动作 | HTTP |
|---|---|
| 建 | `POST …/process/pty`（`id`,`cwd`,`cols`,`rows`,`envs`,`lazyStart`） |
| **连** | `POST …/process/pty/{sessionId}/connect` |
| 列 / 查 | `GET …/process/pty`、`GET …/process/pty/{sessionId}` |
| 改尺寸 | `POST …/process/pty/{sessionId}/resize`（`{cols,rows}`） |
| kill | `DELETE …/process/pty/{sessionId}`（*"terminating the shell process"*） |

**「Disconnect」与「Kill」是两个动词**：文档明写 disconnect 是 *"clean up resources without killing the process"*，Go 侧注释 *"close the WebSocket without killing the shell"*。**这是本文最值得带走的一条进程面设计**——断连接 ≠ 杀进程，两者各自有名字。

### 4.5 断线与状态回流

- SDK 文档（`/docs/en/python-sdk`）：沙箱状态平时**经 WebSocket 推送**，另有 *"sparse polling safety net"*；`DAYTONA_USE_DEPRECATED_POLLING=1` 可退化为纯轮询（已标 deprecated）；WebSocket 握手参数**等价于** `X-Daytona-Source` / `X-Daytona-SDK-Version` 请求头。
- **系统级事件出口是 Webhook**（`/docs/en/webhooks`）：事件仅 7 个——`sandbox.created`、`sandbox.state.updated`、`snapshot.{created,state.updated,removed}`、`volume.{created,state.updated}`。`sandbox.state.updated` 的 payload 就是 `{id, organizationId, oldState, newState, updatedAt}`。
  - ⚠️ **该页不写重试、不写顺序保证、不写签名校验**。控制台里能看到 *"delivery attempts"* 的历史（含 status code / duration / response body），但**重试策略、幂等键、签名头全部缺失** `[未证实]`。
- **SSH**（`/docs/en/ssh-access`）：token 制，*"SSH access tokens expire automatically after 60 minutes"*，`expires_in_minutes` 可调；可提前吊销。⚠️ **该页对「断线后进程是否继续」一个字都没有** `[未证实]`——考虑到 `auto-stop` 把「活跃 SSH 连接」列为重置事件，**推断**（非证实）SSH 断线不会杀进程，只是停止续命。

### 4.6 状态持久化的完整结论（本文最硬的一条）

| 东西 | 存在哪 | 跨 stop/start | 跨 daemon 重启/沙箱销毁 |
|---|---|---|---|
| 沙箱身份、配置、labels、四个间隔、TTL | 控制层 PostgreSQL | 保住 | 直到 delete 才没 |
| 沙箱文件系统 | 容器：runner 本地盘（占配额）；VM：nearby storage（不占）；GPU：无 | 见 §1.5 | 随类不同 |
| **会话（shell 进程、命令表、退出码缓存）** | **daemon 内存 `cmap`** | **丢**（内存态） | **丢** |
| **命令日志 / 退出码 / 输入 FIFO** | **沙箱文件系统 `{configDir}/sessions/…`** | 随文件系统 | 随文件系统 |
| **PTY 会话** | **daemon 内存 `cmap`** | **丢** | **丢** |
| Volume 数据 | S3 | 独立于沙箱 | 独立于沙箱 |
| Snapshot | 内部 OCI registry + S3 | 独立于沙箱 | 删沙箱不删快照 |

**所以 `stop → start` 之后：文件系统还在，`list_sessions()` 会（推断）是空的，日志文件还在盘上但已无会话引用。** `[推断]`——文档没有直说「会话不跨重启恢复」，这是从「`sessions` 是内存 map 且没有从磁盘恢复的代码路径」读出来的（`SessionService` 只有 `NewSessionService` 建空 map，`Create` 是唯一的写入点）。**这是本文最重要的一条推断，也是我最希望有人真机验证的一条。**

---

## 5. 对本项目：可借鉴 / 不可直接照搬

前提（来自 `sandbox-isolation-and-gpu.md` §6/§7 与 `prior-art`）：我们要的是 **docker / podman / local 三个后端严格同等的能力面 + 单节点 GPU 调度 + 自拨号 Worker（或控制层不伸进 worker 网络）**，而不是一个托管多租户 SaaS。**Daytona 是托管 SaaS，它的很多答案的前提我们不具备。**

### 5.1 可借鉴（形状级，不是实现级）

1. ⭐⭐ **「两个面、两份规范、一个拼接点」的显式化。** 平台 API 管对象，toolbox API 管数据，**靠 `toolboxProxyUrl` 字段把两者粘起来**，而不是把数据面路径硬编码进 SDK。**对我们的直接映射**：控制面 API 与 Worker 数据面本就该是两份契约，把「数据面入口地址」作为控制面返回的**字段**（我们已经有这条出站流，那就把「流的地址/会话 id」做成同样的显式字段），比让调用方拼 URL 好。
2. ⭐⭐ **`Disconnect` 与 `Kill` 是两个动词。** PTY 面明写 *"clean up resources without killing the process"*。我们的进程面如果只有「结束运行」一个动词，调用方断线时就没法表达「我要走了，活继续干」。**这条几乎零成本，且我们现在的形状里缺它。**
3. ⭐⭐ **「同一资源的两种交付模式」**：同一个 `/logs` 端点，`?follow=true` + WebSocket 是跟随，不带就是一次快照。Daytona 用了两次（日志、PTY 之外还有 `?follow` 的对称性），`container-sandbox-architecture.md` 在 Agent Sandbox 的 `Start`/`Execute` 上记过第二次。**我们只有流式一条路时，短命令的调用方被迫开一条流。**
4. ⭐⭐ **版本头协商 + 显式兼容分支，而不是多版本端点。** `X-Daytona-SDK-Version` + `0.27.0/0.103.0/0.161.0/0.167.0` 四个阈值分支 + **无头时按最老行为**。我们要发第二个版本的数据面契约时，这是成本最低的形状。
5. ⭐⭐ **「字段级的存在性不一致要写进 schema」**：`outboundProxyUrl` / `otelEndpointOverride` *"Only returned on single-sandbox reads — never on list responses."* 我们做 `Capabilities()` 或 DTO 差异时，**「哪个接口不返回哪个字段」应当是规范里的一句话，不是实现里的一个 if。**
6. ⭐⭐ **「看起来像边界、其实不是」的字段要自曝强度。** `outboundProxyUrl` 的 schema 描述把「只是注入环境变量、进程一清就绕过」写明白了。我们 ADR-0003 边上那些「残余面」就该用这个格式写。
7. ⭐ **`gpuType` 是有序偏好列表**：*"the scheduler tries each in order and pins the sandbox to the first that has capacity"*。比「只接受一个型号」或「型号集合无优先级」都实用，且**不需要新对象**。
8. ⭐ **单节点 GPU 分配器可以小到「一个标签 + 一把锁 + 一次 `ContainerList`」**（`gpu_allocator.go`，80 行）。关键的两个细节值得照抄：**① 分配必须在 `ContainerCreate` 完成前持锁**（否则并发看不见标签）；**② `exited/dead/removing` 的容器必须视为不占卡**，理由是 *"Docker detaches the CDI device cgroup on exit"*。**这两条正是自己写一遍最容易写错的地方。**
9. ⭐ **GPU 沙箱切统一片、忽略请求值**（`gpuSandboxCPUCores/MemoryGiB/DiskGiB` 常量），理由是**异构 GPU 机队上「用户可见容量一致」**。单节点上这条理由弱化，但「同型号卡给同规格」的**可预期性**仍然成立，而且实现成本为零。⚠️ 注意他们**丢弃调用方请求**——我们要不要这么强硬是另一回事。
10. ⭐ **资源限制的实际生效值必须能从沙箱内回读**，且他们给了具体文件（`/sys/fs/cgroup/cpu.max`、`memory.max`、`df -h /`），并警告 `nproc`/`free` 读的是宿主值。**与 `sandbox-isolation-and-gpu.md` §7 第 4 条同一条，但多了「用哪些文件」这一层。**
11. ⭐ **`POST /sandbox/{id}/last-activity` 这种「只续命、不改状态」的独立动词。** 长任务 + 自动回收同时存在时，**调用方需要一个不带副作用的续命入口**，而不是靠「随便发个别的请求」。
12. ⭐ **`desiredState` 与 `state` 并列**。我们做「期望态 / 观测态」对账时，这是现成的字段命名与语义参照。
13. ⭐ **磁盘配额只在 XFS 时才设 `StorageOpt`**（源码里就是 `if d.filesystem == "xfs"`）。**与 `sandbox-isolation-and-gpu.md` §1.1 我们自己核到的机制完全一致**——这条现在有了第二个来源。
14. ⭐ **daemon 以只读 bind mount 从宿主注入容器**（`daemonPath:DAEMON_PATH:ro`）。如果我们要在容器里放一个沙箱侧入口二进制，**从宿主挂进去比打进镜像更可控**，且不会让镜像成为契约的一部分。

### 5.2 不可直接照搬

1. ❌ **runner 轮询控制层取作业。** 它成立的前提是「控制层与 runner 之间有稳定网络」。我们的场景里 Worker 侧的形状已由 `prior-art` §1.1 定过（自拨号 / 长连接），**照搬轮询会把「控制层不伸进 worker 网络」这条前提反过来**。
2. ❌ **`Privileged: true` 是容器类的默认**（非 GPU 沙箱全部特权）。他们对冲的是「容器里跑 AI 生成的代码需要装包、改内核参数」；我们的信任模型是**多租户不可信**，特权容器＝宿主无边界。**我们的默认能力集方向与他们相反，这条连启发都不能算。**
3. ❌ **`linux-vm` / `windows` / `pause` / `fork` / 热快照这一整族 VM-only 能力。** docker/podman/local 三个后端**全都给不出**同等的暂停/分叉/内存快照。**照搬会把「三后端严格同等」直接打破。** 我们在 `sandbox-isolation-and-gpu.md` §1.1 已经判过「不能统一、必须降级或拒绝」——Daytona 的存在不是反转这条的理由，它恰恰示范了「不等价能力要靠 class 分家」。
4. ❌ **Volume 的 FUSE + S3 后端。** 我们刻意不引入 S3 依赖（见 `s3-short-lived-authorization.md` 的定位），而且它的两个已知代价（*"not transactional"*、*"generally slower"*）对我们「Workspace 就是宿主目录 bind mount」的形状是纯粹退步。**`subpath` 那套多租户前缀方案对我们也不成立——我们的租户边界是宿主文件系统，不是 S3 key 前缀。**
5. ❌ **把「会话内日志」当唯一日志面。** Daytona 的日志是**沙箱文件系统里的文件**，沙箱一删就没了，且只有「当前会话的命令」能查。我们要的日志语义更接近「运行的输出」，**照抄「先在沙箱里存文件、再由 daemon 提供跟随读」会让日志的生命周期绑死在沙箱上。**
6. ❌ **计费/配额那一整套（四档池、每分钟限流、独立 GPU 配额 region）**。单节点部署没有 pool 的概念，「组织级配额」退化成「这台机器的容量」。**唯一值得留的是它的形状：配额按 (org, region, class) 三元组算——我们对应的是 (Workspace, 节点, 后端) 的容量记账。**
7. ❌ **`warm pool` 的「定制即失配」**。`prior-art` §1.2 与 `container-sandbox-architecture.md` 都记过这条，**两次都是「别抄」**。
8. ⚠️ **GPU 沙箱「停止即销毁」**。这是他们为了**让卡立刻可回收**做的取舍，代价是文件系统全丢、必须配 Volume。我们如果照搬，等于把「GPU 任务的中间产物」也一起丢掉；若不照搬，则要自己解决「停了但卡还占着」的对账。**这是一条必须显式表态的取舍，不是默认值。**
9. ⚠️ **GPU 请求值被丢弃（固定 16/256/512）。** 单节点上我们大概更需要「按卡数分配」或「按请求分配 + 上限校验」，而不是一刀切。**可以借「同型号同规格」的可预期性，不要借「忽略请求」。**

### 5.3 一处对既有结论的**强化**

`sandbox-network-policy-apis.md` §未能证实 里写着：**「Daytona 用宿主 iptables per-sandbox 链（`DAYTONA-SB-{id}`）落地、以及 60 秒对账」来自 DeepWiki 第三方分析，不是一手来源，标 [未证实]。**

本文**部分证实了前半条**：`[源码] apps/runner/go.mod` 确实依赖 `github.com/coreos/go-iptables v0.8.0`，且 runner 是「节点侧 Go 服务 + 本地 docker daemon」的形态，与「节点侧编译 iptables」一致。**但「链名 `DAYTONA-SB-{id}`」与「60 秒对账」仍未证实**——本文没有读 runner 的防火墙实现文件，**不要把本文当成那两条的出处**。

---

## 6. 未能证实 / 未覆盖

- **一切运行时行为都没有实测。** 本文没有 Daytona 账号，**所有「行为」都是文档声称或源码写法**。尤其是 §4.6 里标 `[推断]` 的那条（会话不跨 daemon 重启恢复），**强烈建议将来有账号时第一时间验证**。
- **公开源码（`v0.190.0`）与线上服务的差异面没有穷举。** 只比了 toolbox 规范 path 集合（83 vs 96）。**platform API 的差异没有比**——线上 `openapi.json` 与 `v0.190.0` 仓库里生成的那份没有逐字段比对。所以**本文所有 `[源码]` 断言都只保证「曾经如此」，不保证「现在如此」。**
- **runner-proto（gRPC）的调用点没有读。** 只知道它存在、有三个 JobType、生成 Go/TS 代码；**不知道谁在什么条件下用它，与 REST 轮询如何分工**。
- **runner 的防火墙/iCheck 实现没有读**（如 `apps/runner/pkg/firewall/` 之类），所以 `DAYTONA-SB-{id}` 链名与 60 秒对账**仍然 [未证实]**（见 §5.3）。
- **SDK 客户端侧的断线重连没有读**（SDK 的 `_utils/stream.py`、`shared_session.py` 只看了头部）。`/docs/en/log-streaming` 也不写重连。**「WebSocket 断了之后 SDK 干什么」是空白。**
- **webhook 的重试策略、顺序保证、签名校验**：`/docs/en/webhooks` 完全没有。**有理由怀疑存在但没文档，不能断言「没有」。**
- **`/sandbox/{sandboxId}/state`（`updateSandboxState`）的入参**：规范里这个 `PUT` **没有 requestBody schema**，只有 path 参数——**状态是怎么传的（query？header？）没查出来**。
- **`apps/api` 的调度器实现没有读。** 「sandbox manager 怎么选 runner」（`/runners/{id}/scheduling`、`/runners/{id}/draining`、`/runners/{id}/full` 这些端点暴露了它有调度概念）**本文只从端点名推断，没有读代码**——**单节点 GPU 调度最该抄的其实是这一层，本文没覆盖。**
- **`/docs/en/declarative-builder`、`/docs/en/mount-external-storage`、`/docs/en/regions`、`/docs/en/language-server-protocol`、`/docs/en/computer-use`、`/docs/en/mcp`、OTel collection** 未打开。
- **`/docs/en/limits` 页面内部矛盾**（Tier 1 内存 10 GiB vs 20 GiB）**没有找到裁定依据**，原文照录。
- **文档说 GPU「每卡加 16 vCPU / 192GB」而源码是固定 16/256/512**——**不裁定谁对**。

---

## Sources

**官方文档（`www.daytona.io` / `docs.daytona.io`，均为本次真打开的页面；`.md` 后缀即该页的 Markdown 原文）**

- 三平面架构、runner polls for jobs、snapshot builder / sandbox manager：<https://www.daytona.io/docs/en/architecture>
- 沙箱生命周期状态、四类间隔默认值与互斥、资源上下限、GPU 规格、`nproc` 读宿主值：<https://www.daytona.io/docs/en/sandboxes>
- 持久化三层、stop/pause/archive 各类行为、TTL、Fork、快照保留期：<https://www.daytona.io/docs/en/persistence>
- 隔离模型、linked sandbox 网络、出网三模式：<https://www.daytona.io/docs/en/isolation>
- 分档配额、每分钟限流、429 形态、磁盘配额按状态：<https://www.daytona.io/docs/en/limits>
- 三个扩展轴、无 autoscaling/密度/排队 的明示缺口：<https://www.daytona.io/docs/en/scale>
- 快照状态机、两周转 Inactive、镜像必须带 tag/digest、GPU 快照 region 覆盖：<https://www.daytona.io/docs/en/snapshots>
- Volume = FUSE + S3、多挂、last-write-wins、subpath、100 卷上限、异步删除 409：<https://www.daytona.io/docs/en/volumes>
- Warm pool 认领条件（定制即失配）、自动补货、每快照每 region 一个：<https://www.daytona.io/docs/en/warm-pools>
- BYOC / custom region *"no limits applied for concurrent resource usage"*、Helm chart、无单节点说法：<https://www.daytona.io/docs/en/bring-your-own-compute>
- 出网四参数、org 策略优先、`outboundProxyUrl` 只是环境变量：<https://www.daytona.io/docs/en/network-limits>
- SSH token 60 分钟、可吊销（**未写断线语义**）：<https://www.daytona.io/docs/en/ssh-access>
- webhook 七个事件与 payload（**无重试/顺序/签名**）：<https://www.daytona.io/docs/en/webhooks>
- exec 默认 10 秒、session 动词全集、`run_async`、命令状态、`cmd_id`：<https://www.daytona.io/docs/en/process-code-execution>
- PTY 动词全集、`lazyStart`、connect/resize/kill、disconnect ≠ kill：<https://www.daytona.io/docs/en/pty>
- 日志 `?follow=true` + WebSocket vs HTTP 快照、阻塞回调会掉连接：<https://www.daytona.io/docs/en/log-streaming>
- SDK 配置（`DAYTONA_API_KEY`/`API_URL`/`TARGET`）、WebSocket + 轮询兜底、头等价关系：<https://www.daytona.io/docs/en/python-sdk>
- CLI 命令全集（**未写调用哪个 API**）：<https://www.daytona.io/docs/en/tools/cli>
- API 参考入口（两份规范 + analytics）：<https://www.daytona.io/docs/en/tools/api>
- **平台 API OpenAPI（权威之一）**：<https://www.daytona.io/docs/openapi.json>
- **toolbox API OpenAPI（权威之二，`info.version = v0.0.0-dev`）**：<https://www.daytona.io/docs/toolbox-openapi.json>

**官方 GitHub（`github.com/daytonaio`，tag `v0.190.0`；`main` 只余 README）**

- 归档声明（2026-06 转私有）：<https://github.com/daytonaio/daytona>
- OpenAPI 从 NestJS 生成 + `x-enum-varnames`：`apps/api/src/generate-openapi.ts`
- CLI 用生成的 Go 客户端 + `X-Daytona-Api-Version` 比对告警：`apps/cli/apiclient/api_client.go`
- CLI `exec` = 平台 API 取沙箱 + toolbox 执行：`apps/cli/cmd/sandbox/exec.go`
- 会话内存态 + 落盘日志/退出码/FIFO 路径：`apps/daemon/pkg/session/types.go`
- 常驻 shell、`Setpgid`、家目录兼容分支：`apps/daemon/pkg/session/create.go`
- 50ms 轮询 `exit_code`、FIFO labeler、前缀 demux、`cmdWrapperFormat`：`apps/daemon/pkg/session/execute.go`
- `follow`/WebSocket、前缀跨 chunk 处理、pong 优先：`apps/daemon/pkg/session/log.go`
- `0.27.0`/`0.167.0` 阈值与 `X-Daytona-Split-Output`：`apps/daemon/pkg/session/common.go`
- `SessionService` 结构（内存 `cmap`）：`apps/daemon/pkg/session/service.go`
- `X-Daytona-SDK-Version` 提取（含 WebSocket 子协议）：`apps/daemon/internal/util/version.go`
- `0.103.0` 家目录兼容分支：`apps/daemon/pkg/toolbox/process/session/session.go`
- 命令执行（stdin 喂 shell、SIGKILL 进程组、HTTP 200 + `exitCode: -1`）：`apps/daemon/pkg/toolbox/process/execute.go`
- PTY 内存 manager、`LazyStart`、`VerifyPTYSessionReady`：`apps/daemon/pkg/toolbox/process/pty/manager.go`
- `entrypoint` 保留会话常量：`apps/daemon/internal/util/entrypoint_session.go`
- **单节点 GPU 分配器（`daytona.gpu_index` 标签 + 互斥锁 + `ContainerList`）**：`apps/runner/pkg/docker/gpu_allocator.go`
- **容器 HostConfig（`Privileged: gpuIndex == nil`、CDI `DeviceRequests`、`StorageOpt` 仅 xfs、GPU 固定切片常量、daemon 只读 bind）**：`apps/runner/pkg/docker/container_configs.go`
- GPU 数量探测（`nvidia-smi --query-gpu=name`）：`apps/runner/pkg/docker/gpu_info.go`
- 容器运行时配置项（`CONTAINER_RUNTIME` / `GPU_ENABLED`）：`apps/runner/cmd/runner/config/config.go`
- runner gRPC 作业类型（仅 snapshot/fork/pause）：`libs/runner-proto/proto/runner/v1alpha1/job.proto`
- runner 锁在 `api-client-go v0.149.0`（版本偏差实证）：`apps/runner/go.mod`
- daemon 生成的 toolbox 规范（83 path，线上 96）：`apps/daemon/pkg/toolbox/docs/swagger.json`
- SDK 全部走生成的 toolbox 客户端：`libs/sdk-python/src/daytona/_sync/process.py`
- 组织内相关仓库（含 `sysbox-runc` 分支，**未读其内容、未在任何一手页面找到容器类使用 sysbox 的说明** `[未证实]`）：<https://github.com/orgs/daytonaio/repositories>
