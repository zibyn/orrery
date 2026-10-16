# 「以容器为沙箱」的系统：架构与接口的一手核对

对应本批调研（与另外三份：`file-transfer-mechanics-and-path-safety.md`、`sandbox-git-integration.md`、`sandbox-network-policy-apis.md` 同批）。本份聚焦**容器/编排器优先**的形态——microVM 优先的那批（E2B / Modal / Fly / Vercel / AWS / Daytona）已由 `prior-art-sandbox-orchestration.md` 覆盖，本文**只写增量**，不重复它的结论；需要时直接引用它。

> **取证强度**：每条断言附 URL，且该 URL 在本次调研中被真正打开过（WebFetch / WebSearch 定位后 WebFetch / `gh api` 读官方仓库文件）。文档原文一律保留英文。**「文档写了的」/「源码读出的」/「实测的」分别标注**；查不到写 **[未证实]** 并写明查了哪里。不猜、不用二手博客充数。调研日期 2026-10-16。

## 0. 先读这几条

1. **⭐ 最接近我们形状的一份一手材料，是 Agent Sandbox 里的 `sandboxd`，不是任何一家 SaaS。** 它是一份**写在 proto + OpenAPI 里的「沙箱运行时契约」**：[`packages/sandboxd/`](https://github.com/kubernetes-sigs/agent-sandbox/tree/main/packages/sandboxd)，明确区分 **gRPC `ProcessService`（:9090）** 与 **REST `FilesystemService`（:8080）**，并给出「**两者必须指向同一个 workspace 与同一个执行环境**」的兼容性判据。它是我们 `Runtime Provider` + 数据面两份设计**同时**的对照组（§1.3）。
2. **容器为沙箱的那批，全部是「沙箱里有监听端口」。** Agent Sandbox 的 `sandboxd` 默认 `--listen-host 0.0.0.0`，原文一句话把取舍说穿了：**"Containment is provided by pod isolation and NetworkPolicy, not loopback binding."** Docker Sandboxes 反过来——它用**宿主侧代理**截住沙箱的出网，且沙箱本身是一个 microVM。⚠️ **两家都不是「零监听端口、不可被连入」的沙箱侧主动拨号**（§1.4、§2.4）。
3. **⭐ 「容器没了工作区还在」在这批里有两个成熟表述，都该引用。** ①Agent Sandbox 有一份**逐字的 KEP**：控制器会 `explicitly *not* delete` PVC 与 Service，恢复时「新 Pod 绑回同一批 PVC」；②**Gitpod/Ona 的 dual disk** 把它做成了产品特性，措辞是 *"Dual disk preserves environment data, **not the identity of the VM**"*（§4.3）——**「保留的是数据、不是机器的身份」正是我们 `Workspace` 定义的那句话。** 反过来，**Codespaces 把持久化外包给了 git**（删掉 codespace 就等于删掉工作，§4.1）——**那是唯一一个与我们的模型真正分道扬镳的**，也是「我们为什么必须自己管保留期」的答案。
4. **接口上「命令」与「文件」被每一家都切成了两个面**，只是刀口不同：Agent Sandbox 按**协议/端口**切（gRPC vs REST，理由写死在 KEP 里），Docker Sandboxes 按**CLI 动词**切（`sbx exec` vs `sbx cp`），OpenHands 把文件读写塞进命令通道、只把列表与批量搬运分出去，Tekton 的 Result 与 Workspace 本来就是两个对象。**没有一家把它们合成一个面**，这反驳了我们「文件面可以就是流上的一个 tar 双向通道」的省事想法（§1.3、§2.3、§3.2）。
5. ⚠️ **本票最集中的一类冲突是「凭据进执行侧」**：DevPod *"forward git credentials to a remote machine"*、Tekton 的 `basic-auth` 工作区、OpenHands 的 `X-Session-API-Key` 写进容器环境、`sandboxd` 在沙箱内校验 bearer token——**四家四个形式，同一个取舍**。而唯一与我们同侧的是 **Argo**（凭据只挂 init/wait 容器，主容器没有，§5.1）。`ADR-0001` 的论证现在有正反两面的一手材料了。
6. ⚠️ **「对已停止的沙箱执行命令会把它拉起来」在这里是主路径，且写在 CLI 的 help 文本里**：`sbx exec` 原文 *"If the sandbox is stopped, it is started first."*，`sbx ports publish` 同理（§2.4）。这是 `prior-art` §8 第 2 条判为不可抄的那件事——**但注意它的前提是「沙箱就在本机隔壁」，我们的前提不同，所以理由要写成前提差异而不是品味差异。**

---

## 1. Kubernetes Agent Sandbox（`kubernetes-sigs/agent-sandbox`）

**身份**：Kubernetes SIG Apps 的正式子项目，2025-11 在 KubeCon Atlanta 发布（[Google Open Source Blog, 2025-11-11](https://opensource.googleblog.com/2025/11/unleashing-autonomous-ai-agents-why-kubernetes-needs-a-new-standard-for-agent-execution.html)：*"we are launching Agent Sandbox as a formal subproject of Kubernetes SIG Apps"*，其官网称自己 *"a Kubernetes-native platform for managing isolated, stateful, singleton workloads"*，[docs](https://agent-sandbox.sigs.k8s.io/docs/)）。

### 1.1 对象模型：两级 CRD，外加一个「池」

| 对象 | API group/version | 作用（原文） |
|---|---|---|
| **Sandbox** | `agents.x-k8s.io/v1beta1` | "Declarative API for a single, stateful pod with a stable hostname and optional persistent storage"（[docs](https://agent-sandbox.sigs.k8s.io/docs/)） |
| **SandboxTemplate** | `extensions.agents.x-k8s.io/v1beta1` | "Provides a way to define reusable templates for creating Sandboxes"（[README](https://github.com/kubernetes-sigs/agent-sandbox)） |
| **SandboxClaim** | `extensions.agents.x-k8s.io/v1beta1` | "Allows users to create Sandboxes from a `SandboxWarmPool`"（同上） |
| **SandboxWarmPool** | `extensions.agents.x-k8s.io/v1beta1` | "Manages a pool of pre-warmed Sandboxes that can be quickly allocated to users"（同上） |

group 字符串取自源码，非文档转述：`schema.GroupVersion{Group: "agents.x-k8s.io", Version: "v1beta1"}`（[api/v1beta1/groupversion_info.go](https://github.com/kubernetes-sigs/agent-sandbox/blob/main/api/v1beta1/groupversion_info.go)）、`Group: "extensions.agents.x-k8s.io"`（[extensions/api/v1beta1/groupversion_info.go](https://github.com/kubernetes-sigs/agent-sandbox/blob/main/extensions/api/v1beta1/groupversion_info.go)）。**没有 `SandboxSet`**，README 与源码里都不存在。

**嵌套关系是「模板 → 池 → 认领 → 沙箱」**，而不是包含：`SandboxClaim` 的 spec 指向 **warm pool**（不是 template），源码注释：`WarmPoolRef` "the specific pre-warmed infrastructure pool to check out from."（[sandboxclaim_types.go](https://github.com/kubernetes-sigs/agent-sandbox/blob/main/extensions/api/v1beta1/sandboxclaim_types.go)），且带 `+required`。

### 1.2 生命周期：Pod 死、PVC 活——一份可以逐字引用的 KEP

`SandboxSpec` 只有两个生命周期字段（[sandbox_types.go](https://github.com/kubernetes-sigs/agent-sandbox/blob/main/api/v1beta1/sandbox_types.go)）：

- `shutdownTime *metav1.Time`（`json:"shutdownTime,omitempty"`）："shutdownTime is the absolute time at which the Sandbox expires."
- `shutdownPolicy *ShutdownPolicy`（`json:"shutdownPolicy,omitempty"`）："shutdownPolicy determines what happens to the Sandbox object itself when it expires"，取值 `Delete` / `Retain`，**默认 `Retain`**。
- `operatingMode SandboxOperatingMode`：`Running` / `Suspended`，"Defaults to Running if not specified."

**暂停的语义（源码 KEP，不是博客）**——这是本节最有价值的一条。控制器看到 `Suspended` 时 *"explicitly triggers a graceful deletion of the underlying Pod"*，而 *"the controller will explicitly **not** delete any attached PersistentVolumeClaims (PVCs) or stable network identities (Services)"*；恢复时 *"The new Pod is seamlessly bound to the existing PVCs, ensuring state is retained."*（[KEP-694](https://github.com/kubernetes-sigs/agent-sandbox/blob/main/docs/keps/694-kep-for-suspend-and-resume-for-beta/README.md)）

⚠️ **但官方博客的说法比 KEP 大**。K8s 官方博客（2026-03-20）写的是 *"Agent Sandbox supports scaling these idle environments to zero to save resources, while ensuring they can resume exactly where they left off."*（[kubernetes.io](https://kubernetes.io/blog/2026/03/20/running-agents-on-kubernetes-with-agent-sandbox/)）——**「exactly where they left off」在 KEP 里对应的只是「新 Pod 绑回同一批 PVC」**，进程与内存不在其中。**这是一处官方博客与官方 KEP 的措辞落差，别把博客那句当成能力。**

**沙箱本身的终态条件（源码读出）**：`Suspended` 条件 *"is never removed"*，消费者应以 `Ready` 为准；`Ready` 为 False 的原因里含 `SandboxSuspended`、`SandboxExpired`、`MultiplePods`；`PodIPs` 与 `NodeName` 在 Pod 缺席时被清空（"cleared when the pod is absent (e.g. while suspended)"）。**「暂停中」与「有 IP」是互斥的**——这是个值得抄的建模细节。

### 1.3 接口：一个 pod 里两个面，且理由写死了——这是本票最该抄的一节

`sandboxd` 是 KEP-539.2 定义的**可移植沙箱运行时守护进程**，跑在 sandbox pod 里（[USER_GUIDE.md](https://github.com/kubernetes-sigs/agent-sandbox/blob/main/packages/sandboxd/USER_GUIDE.md)）：

```text
sandboxd (runtime daemon)
├── gRPC  :9090  →  ProcessService    (streaming process I/O)
└── HTTP  :8080  →  FilesystemService (stateless file operations & runtime probes)
```

给出的分工理由逐字如下：**"Process execution is served over gRPC because `Start` is a long-lived server-streaming RPC: `stdout`/`stderr` flow continuously until the process exits."** 而 **"Filesystem transfers are served over REST so file bytes move as raw `application/octet-stream` payloads with no protobuf wrapping, and any plain HTTP client works without generated stubs."**

**`ProcessService`（gRPC，源码 proto）**——它就是我们的 Task 形状的现成对照组（[process.proto](https://github.com/kubernetes-sigs/agent-sandbox/blob/main/packages/sandboxd/spec/process/v1/process.proto)）：

| RPC | 形态 | 说明（proto 注释） |
|---|---|---|
| `Start(StartRequest) returns (stream StartResponse)` | 服务端流 | "Start runs a command and streams its stdout/stderr events in real time." |
| `Execute(ExecuteRequest) returns (ExecuteResponse)` | 一元 | "runs a short-lived command synchronously to completion and returns stdout, stderr, and the exit code atomically." |
| `WriteStdin` / `SendSignal` / `ResizeTTY` | 一元 | 信号限 `SIGINT=2` / `SIGKILL=9` / `SIGTERM=15` |

`StartResponse` 是 `oneof`：`InitEvent{process_id}` / `bytes stdout` / `bytes stderr` / `ExitEvent{exit_code}`；`ProcessConfig{command[], env_vars{}, cwd}`。**这与 `prior-art-sandbox-orchestration.md` §1.7 从 Vercel ND-JSON 推出的 `OutputChunk{stream,data}` + `ExitEvent{exit_code}` oneof 是同一个形状，且这次是 proto 一手**。

**`FilesystemService`（REST，`/v1/files`）**——纯按路径寻址，与我们数据面同构（[USER_GUIDE API summary](https://github.com/kubernetes-sigs/agent-sandbox/blob/main/packages/sandboxd/USER_GUIDE.md)）：

| 方法 | 端点 | 语义 |
|---|---|---|
| `GET` | `/v1/files/{path}` | 文件 → 原始字节（`application/octet-stream`）；目录 → JSON `DirectoryListing` |
| `HEAD` | `/v1/files/{path}` | 存在性/元数据探测，不传 body |
| `PUT` | `/v1/files/{path}` | **原子写（temp file + rename）、自动建父目录**；可选 `mode` 查询（`^0[0-7]{3}$`，默认 `0644`）；接受原始字节或 `multipart/form-data` |
| `DELETE` | `/v1/files/{path}` | `recursive=true` 才等价 `rm -rf`；否则非空目录回 **409** |
| `GET` | `/v1/health`、`/v1/metadata` | 就绪探针；`/v1/metadata` 只暴露白名单前缀（默认 `SANDBOX_`）的 orchestrator 注入变量 |

**路径安全与并发语义写得比我们现有的任何一份都细**，值得整段对照：

- 所有 `{path}` *"resolved against the sandbox root (`/workspace` by default) via symlink-aware sanitization; traversal attempts return `403 {"code":"PERMISSION_DENIED"}`"*。
- 并发：**"Requests are handled in parallel, with no server-side serialization."** 逐条列了 write-vs-read / write-vs-delete / delete-vs-read / 同路径并发写的后果，并明说 **"There is no cross-request transaction or compare-and-swap; agents that need ordering or coordination on shared paths must layer it themselves."**
- **兼容性判据里有一条对我们的数据面是硬要求**：*"A file written through REST must be visible to a process launched through gRPC."*（同一个 sandbox root、同一棵树的两个视图）

**鉴权（源码读出，flag 表）**：`--auth-token-sha256` / `--auth-token-file`（"Hex SHA-256 digest of the bearer token clients must send"），原文补一句 *"Neither leaves the API unauthenticated."* ⚠️ **即：认证主体是沙箱里那个守护进程**，而我们的既定形状是**沙箱侧唯一认证主体是主动拨号的 Worker，沙箱里没有任何可被认证的东西**。

**接口是怎么被「标准化」的（KEP-539.2 原文）**：他们**没有**做能力协商或自检上报，而是**符合性档位**——*"A runtime is considered \"Agent Sandbox Compliant\" if it meets the following criteria."*，并分了 Core / *"**Full Conformance:** Must support all Core methods PLUS **Jupyter Support** and filesystem **Watch** capabilities"*。版本化是一句硬话：*"Treat `process.v1` and `/v1` as protocol versions. Backward-incompatible wire changes require a new version instead of silently changing the v1 contract."*
⚠️ **这与 `GLOSSARY` 的「Runtime Capabilities 是后端自检后上报的**事实**，不是它承诺能做到的」是两条路**：他们让**契约**承担标准化（要么符合要么不符合），我们让**运行时自检**承担（`enforced | unsupported` 是事实，不是声明）。**我们的路要求每个后端都能诚实自检；他们的路要求接口足够小以至于不需要自检。** 两者不冲突，但**别在同一个接口上混用**——如果 `Capabilities()` 里的值其实来自实现者的声明，那句话就不成立了。
还有一条**版本断代的操作方式**值得记：新协议**不与旧的 `python-runtime` API 兼容**，且迁移期 **"the legacy `python-runtime` remains available and is still the default when `sandboxd` is not selected"**，同时 *"the supported SDKs do not inspect these variables or automatically fall back between runtimes"*——**它选择让两代并存但绝不自动回退**。对照 `ADR-0005` 的代际窗口（控制层必须说得出口窗口内每一代）：**他们把这个窗口留在了客户端，我们留在了控制层。** 对我们更有利，因为我们只发布一个客户端。

### 1.4 网络：per-sandbox NetworkPolicy，但**默认拒绝 egress**（与我们的 `egress: public` 相反）

`SandboxTemplateSpec` 带 `NetworkPolicy *NetworkPolicySpec` 与 `NetworkPolicyManagement`（`Managed` 默认 / `Unmanaged`）；`NetworkPolicySpec` 就是 `networkingv1.NetworkPolicyIngressRule` / `EgressRule` 的列表，且源码注释写死默认值：**"If this list is empty, all ingress traffic is blocked (Default Deny)."**、**"Egress ... same default-deny behavior for egress."**（[sandboxtemplate_types.go](https://github.com/kubernetes-sigs/agent-sandbox/blob/main/extensions/api/v1beta1/sandboxtemplate_types.go)）

官方示例把分层策略写全了（[Network Policy API 示例](https://agent-sandbox.sigs.k8s.io/docs/use-cases/examples/network-policy-api-sandbox/)）：Admin 层 `priority: 100` 的 catch-all `Deny` 打到 `0.0.0.0/0` 与 `::/0`；DNS 必须单独放行且 *"CoreDNS is a `Pass`, not an `Accept`"*（否则会 *"silently reopen it for every sandbox"*）；**"DNS traffic has to be allowed before any FQDN rule can match."**；`domainNames` **只能出现在 `Accept` 规则里**，建议 *"accept by name at higher precedence and deny by CIDR at lower precedence"*。评估顺序 **Admin → NetworkPolicy → Baseline → cluster default**，`priority` 越小越先（0 最高，1000 最低）。

⚠️ **这条与 `ADR-0003` 的 `egress: public` 是正面冲突的两个默认值**：他们从「默认全拒 + 按名字开洞」出发，我们从「全空间减私有段」出发，而且我们明确否决过按域名匹配（SNI 挡不住 domain fronting）。**我们的理由没有被这份材料推翻**（我们的调用方是拿 `pip/npm/go mod` 的开发者，他们的调用方是 `SandboxClaim` 的编排器），但**「默认拒绝」在这里有了一个 K8s 官方项目级的先例**，值得记进 ADR 的 Considered Options，而不是当作已被我们排除的选项。

`EnvVarsInjectionPolicy`（`Allowed`/`Overrides`/**`Disallowed` 默认**）与 `VolumeClaimTemplatesPolicy`（`Disallowed`/`Allowed`/`Overrides`）默认都是**拒绝**——**模板作者不放权，认领方就不能改沙箱环境**。这与 `prior-art` §4.3「worker 不能向上声明自己的能力」是同一条纪律。

### 1.5 runtime 怎么选：RuntimeClass，API 不管隔离

官方博客原文把边界划得很清：*"The Sandbox custom resource natively supports different runtimes, like gVisor or Kata Containers. This provides the necessary kernel and network isolation required for multi-tenant, untrusted execution."*（[kubernetes.io](https://kubernetes.io/blog/2026/03/20/running-agents-on-kubernetes-with-agent-sandbox/)）官网一句更干脆：**"Isolation depth is a deployment choice, not a limitation of the API."**（[docs](https://agent-sandbox.sigs.k8s.io/docs/)）——即**具体 runtime class 不是 API 字段，是 Pod 部署侧的事**。这与我们从 gVisor 文档推出的「可插拔后端的模式就是我们的 Runtime Provider」同形。

### 1.6 沙箱怎么被「进入」：一个反向代理 + 一个 header

这是与我们分歧最大的一处。客户端不直接连沙箱，而是打一个**静态入口**，用 header 指定目标（[sandbox-router README](https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/python/agentic-sandbox-client/sandbox-router/README.md)）：

> "An external client sends a request to a single, static IP address provided by a Gateway. The request must include a header (e.g., `X-Sandbox-ID`) that specifies the unique name of the target sandbox."
> "The router pod reads the `X-Sandbox-ID` header, constructs the internal Kubernetes DNS name for the target sandbox's headless service, and proxies the original request to it."

另有 in-cluster 直连形态：`http://{sandbox_id}.{namespace}.svc.cluster.local:{server_port}`，以及四种模式（Gateway / `kubectl port-forward` 隧道 / in-cluster 直连 / Direct URL），并自己警告 *"Direct access bypasses the sandbox-router's authorization checks"*。默认运行时 *"listens on a port other than 8888"* ——**沙箱里有一个监听服务，这是这个产品的承重假设**。

⚠️ **这与我们「沙箱零监听端口、控制层不可连入沙箱」直接矛盾。** 他们的必要条件是「控制层拥有到 Pod 的网络路径」（K8s 给了），我们放弃的正是这个条件，换来自拨号 Worker 能在 NAT 后面工作（`prior-art` §0 第 1 条：除 Coder/Buildkite 外所有产品都拥有到机器的网络路径）。**两者不是优劣，是前提不同**；但要注意**它换来的东西比我们多**：Agent Sandbox 的沙箱可以直接跑 HTTP 服务（`PROXY_TIMEOUT_SECONDS` 默认 180s、`WEBSOCKET_*` 一整套参数），而我们的沙箱跑不了任何要被人访问的服务。

### 1.7 该抄 / 不该抄

**该抄**
- ⭐⭐ **`ProcessService` + `FilesystemService` 的「两个面、两条协议、一个 root」拆分**，以及它给出的**两条兼容性判据**："A file written through REST must be visible to a process launched through gRPC." 与 "Treat `process.v1` and `/v1` as protocol versions." 它把「文件面与命令面同源」从我方的一句直觉，变成了一份可引用的契约。
- ⭐⭐ **`Start` 的 `oneof` 事件形状**（`InitEvent` / stdout / stderr / `ExitEvent`）+ **`Execute` 这个「一次性同步版」动词**。我们只有流式一条路，而 `Execute` 承认了一个真实需求：短命令不值得开一条流。「同一条资源的两种交付模式」在 `prior-art` §1.2 里已从 Daytona 的 `?follow=true` 见过一次，这里是第二次，且是 proto。
- ⭐⭐ **`Suspended` 条件的原文措辞**：`SandboxSuspended` / `SandboxExpired` 是两个不同的 `Ready=False` 原因，**「暂停中」不是「出错」**。以及 *"PodIPs and NodeName are cleared when the pod is absent"*——**暂停中的对象没有地址，这是它对的状态**。映射到我们：`Workspace` 冷档没有 Sandbox，是该状态的自然表达，不该被读成异常。
- ⭐⭐ **KEP 里那句 `explicitly *not* delete ... PVCs`**，作为我们「沙箱没了工作区还在」的一手论证。**别再自己论证一遍。**
- ⭐ **路径安全的完整措辞**：`symlink-aware sanitization`、越界回 `403 PERMISSION_DENIED`（**不是 404**）、`PUT` 是 **temp file + rename 的原子写**、自动建父目录、`mode` 有一个正则校验的查询参数、非空目录 `DELETE` 回 **409**。这是一份可以直接对着实现的清单。
- ⭐ **并发语义的诚实记账**：明说无服务端串行化、无 CAS、列了四组竞态的后果、并把协调责任交还调用方。**我们的文件面（`upload_files` / `complete_upload` / `download_file`）必然有同一类竞态，照这个粒度写下来。**
- ⭐ **`NetworkPolicyManagement` 的 `Managed`/`Unmanaged` 二分 + 模板侧默认拒绝放权**（`EnvVarsInjectionPolicy` / `VolumeClaimTemplatesPolicy` 默认 `Disallowed`）。
- **`--listen-host` / `--root-dir` 这类「默认值就是安全默认值」的 flag**（`--root-dir` 默认 `/workspace`，且 "Created if missing."）。

**不该抄**
- ⚠️ **沙箱内监听端口 + 反向代理 + `X-Sandbox-ID` 路由**。它成立的前提是控制层能连进 Pod；我们放弃的正是这个前提。**我们的寻址是「一个 Worker 可达当且仅当它的流开着」（`prior-art` §1.10 的 `/me/` 式寻址），不要为了对齐它的形状而把端口开回来。**
- ⚠️ **沙箱内守护进程自己校验 bearer token**（`--auth-token-sha256`）。我们沙箱侧的唯一认证主体是 Worker；在沙箱里放一个能验证凭据的进程，等于在信任单元内部再造一个鉴权点，与 `ADR-0001`（凭据只交付给可信侧）方向相反。
- **WarmPool / `replicas` / `/scale` 子资源 / HPA**：`Replicas *int32` 的注释是 *"This field is controlled by an HPA if specified."*——它是一个**给 K8s 自动扩缩容用的池**。我们的「新 Workspace 落到哪台 Worker」是撮合问题（`prior-art` §3.5），不是池容量问题；**池只值得抄它的「事件驱动替换」而不是副本数**：`OnReplenish` 策略下陈旧未认领的沙箱 *"is replaced when it is manually deleted, or when it is claimed"*，且 *"metadata-only edits (annotations or labels) do not make a sandbox stale"*。
- **`SandboxClaim` 的两条「强制冷启动」副作用**：`Env` 字段一加就 *"the Sandbox will always be cold-started from the template of the warmpool"*，`VolumeClaimTemplates` 一加 *"forces a cold start because warm pool pods will not have these volumes."* **「定制即失配」这个约束 `prior-art` §1.2 已经从 Daytona 的 warm pool 记过一次，这里第二次出现，同样不要抄。**
- **`shutdownPolicy: DeleteForeground`**（claim 挂着 `deletionTimestamp` 等 Pod 终止完）。我们的终态写是幂等的 stop-record，不需要第三种停机花色。
- **`NetworkPolicy` 对象本身**：我们要的是 netns 里的 nftables 规则集（`ADR-0003`），不是 K8s 对象；**它的价值是「默认拒绝」这个默认值与分层评估顺序，不是 API 形状。**

---

## 2. Docker Sandboxes（`sbx` / `sandboxd`）

**它存在，而且是一手文档。** `docs.docker.com` 首页把它列为产品线："Docker Sandboxes: Run sandboxes on Docker-managed cloud infrastructure"、"Manage cloud sandboxes with the API and TypeScript SDK"、"Compose sandbox environments with kits v3"（[docs.docker.com](https://docs.docker.com/)）。CLI 参考由 `docker/docs` 仓库的 `data/sbx_cli/*.yaml` 生成（[data/sbx_cli](https://github.com/docker/docs/tree/main/data/sbx_cli)），本节的命令原文全部取自那些 YAML——**这是源码级一手，不是页面转述**。

**它在讲什么**：*"Docker Sandboxes creates isolated sandbox environments for AI agents, powered by Docker."*（[sbx.yaml](https://github.com/docker/docs/blob/main/data/sbx_cli/sbx.yaml)）

### 2.1 隔离：microVM，且敢于自称安全边界

⚠️ **注意：它虽然叫 Docker Sandboxes，隔离底座不是容器。** 官方对比表把它列作 "Sandboxes (microVMs)" / "Full (hypervisor)" / "Isolated daemon"，与 "Partial (namespaces)" 的容器明确对立，并给出取舍句：*"Sandboxes trade higher resource overhead (a VM plus its own daemon) for complete isolation."*（[architecture](https://docs.docker.com/ai/sandboxes/architecture/)）

而安全页**正面**说它是边界——这与 Cloudflare（"沙箱因此是最小的信任单元"）和我们的「沙箱不是安全边界」都不同：*"The primary trust boundary is the microVM."* / *"The agent has full control inside the VM, including sudo access."*（[security](https://docs.docker.com/ai/sandboxes/security/)）

**这对本票的意义**：**「以容器为沙箱」在 Docker 自己的产品里是走不通的**——它选了 microVM，然后用**宿主侧代理**补齐网络与凭据。我们要借鉴它的接口，但不要以为它能支持「容器就够」这个判断。

### 2.2 对象模型与生命周期：沙箱活得比 agent 久

- **对象就是 sandbox**，靠 `--name` 给稳定身份：*"Use `--name` to give a sandbox an explicit identity"*、*"Once a named sandbox exists, reattach from any working directory with `sbx run --name`."*（[usage](https://docs.docker.com/ai/sandboxes/usage/)）
- ⭐ **沙箱比 agent 进程活得久，原文写死**：*"Sandboxes persist after the agent exits."* / *"A detached sandbox keeps running until you stop it with `sbx stop` or remove it with `sbx rm`."* **这与我们「Workspace 比 Sandbox 活得久」是同一个直觉，只是他们的层级少一级**（他们没有「工作区」这个比沙箱更长寿的对象——**宿主目录就是工作区**）。
- `sbx stop`：*"Stopped sandboxes retain their state and can be restarted with `sbx run`."*（[sbx_stop.yaml](https://github.com/docker/docs/blob/main/data/sbx_cli/sbx_stop.yaml)）本地档的 state 是沙箱自己的文件系统；**云端档不一样**：*"stop suspends each sandbox in place: its full state (memory + disk) is preserved, the host is released, and the sandbox keeps its ID."* ——**内存+磁盘快照，正是 `prior-art` §10 第 1 条点名劝退的那件事**，这里有了明确的商业实现。
- `sbx rm` 之后 **"only host workspace files remain"**（[usage](https://docs.docker.com/ai/sandboxes/usage/)）——**「工作区在宿主上、沙箱删了它还在」这句话他们有。**
- **TTL 只在云端**：`sbx ttl` *"Cloud-only: local sandboxes are not TTL-managed."*，且有 **"the sandbox's hard 24h-from-creation ceiling"**，服务端只能延长不能缩短（*"The server cannot shorten an expiration, so DURATION must be positive."*）；云端默认 **1 小时过期**，*"On expiration, the service stops sandboxes that can be resumed and deletes the rest."*，并自带一句警告 *"Check the timeout action before relying on a sandbox to retain your work"*（[sbx_ttl.yaml](https://github.com/docker/docs/blob/main/data/sbx_cli/sbx_ttl.yaml)、[cloud](https://docs.docker.com/ai/sandboxes/cloud/)）。
- **`sbx template` 是第二套持久化答案**：*"Templates are saved snapshots of sandboxes that can be reused to create new sandboxes"*，且有 *"Cloud snapshots and loads typically produce multi-GB artifacts and take several minutes."*（[sbx_template.yaml](https://github.com/docker/docs/blob/main/data/sbx_cli/sbx_template.yaml)）
- ⚠️ **`sbx volume` 的脚注是一条会丢数据的坑**：*"Data is saved as a snapshot when a sandbox exits, not continuously synced. If multiple sandboxes mount the same volume concurrently, the last sandbox to exit wins — its snapshot overwrites the others."*（[sbx_volume.yaml](https://github.com/docker/docs/blob/main/data/sbx_cli/sbx_volume.yaml)）**「退出时才快照 + 最后退出者胜」是 `prior-art` §8 第 5 条（多套持久化答案的代价）的一个新变种。**

### 2.3 文件面：`sbx cp`，一个动词，两条路由

`sbx cp` 的完整契约（[sbx_cp.yaml](https://github.com/docker/docs/blob/main/data/sbx_cli/sbx_cp.yaml)）：

> *"Either SRC or DST must be a sandbox path, written as `SANDBOX:PATH`. The other must be a local path. **Copying between two sandboxes is not supported.** ... Cloud transfers go through the Docker Sandboxes Cloud file API instead of the local sandboxd."*
> *"When copying a directory, the directory itself is placed at the destination. If the destination path does not exist it is created; if it already exists as a directory, the source is placed inside it."*
> 选项只有 `--follow-link`（`-L`）。

**这是 `docker cp` 的形状**（`prior-art`… 不，是 `vendor-file-apis.md` §1 的那张表），**不是我们 `ls_files`/`download_file`/`upload_files`/`complete_upload` 的四动词形状**：没有列目录、没有「完成上传」的显式终态、没有元数据。**字节走哪条路它写出来了**：本地走 `sandboxd`，云端走 "Docker Sandboxes Cloud file API"——**动词不变、路由在下面换**，这恰是我们「文件面的元数据走流、字节走对象存储」想表达的分层，只是他们的第二段是平台的 file API 而不是预签名 URL。

### 2.4 命令面与入口：`sbx exec` 在主路径上做隐式拉起

`sbx exec`（[sbx_exec.yaml](https://github.com/docker/docs/blob/main/data/sbx_cli/sbx_exec.yaml)）：

> *"Execute a command in a sandbox. **If the sandbox is stopped, it is started first.**"*
> *"Flags match the behavior of `docker exec`, except **detached exec (-d/--detach) is not supported**."*

- ⚠️⚠️ **`sbx exec` 的隐式 auto-start 与 `sbx ports publish` 的 *"Publishing a local port starts a stopped sandbox before creating the host binding."* 是同一件事的两次出现。** 这是 `prior-art` §8 第 2 条判为**不可抄**的那条（Vercel 的隐式 auto-resume：把「派发」变成一个可能无界等待的拉起动作，并用表面存活盖住死掉的机队）。**Docker 把它做进了 help 文本，说明在「本地单机、沙箱就在隔壁」的前提下它是舒服的**——**我们的前提不同（跨机器、Workspace 只能回到归属 Worker），所以结论仍是不抄，但理由要写清是前提差异不是品味差异。**
- **不支持 detached exec**——对我们是一个反向证据：**「一条命令可能在后台继续跑」不是普遍需求**，我们的 Task 全是同步等终态的，这没问题。
- `sbx exec` 支持 `-i/-t/-u/-w/-e/--privileged`，`--cloud` 下 `-d/--user/--privileged` 被 **rejected rather than silently ignored**（原文）——**「能力不支持就大声拒绝，不静默降级」**，与 `ADR-0003` 的 `enforced | unsupported` 三值同一条纪律。

### 2.5 工作区挂载：三种模式，且云端没有工作区

（[architecture](https://docs.docker.com/ai/sandboxes/architecture/)）

- **Direct（默认）**：*"A directly mounted workspace appears at the same absolute path as on your host."* / *"the directory is mounted into the sandbox through a filesystem passthrough"*。
- **Clone（`--clone`）**：*"the host repo is mounted read-only at `/run/sandbox/source`"*，agent 在 *"a private clone inside the sandbox"* 里干活，**提交通过宿主上的 `sandbox-<name>` git remote 取回**（[sbx_create.yaml](https://github.com/docker/docs/blob/main/data/sbx_cli/sbx_create.yaml)：*"the agent's commits are accessible via the sandbox-<name> git remote on the host"*）。⚠️ *"Clone mode is fixed at create time."*
- **Mountless**：*"the sandbox has no host workspace bind mount"*，退回镜像 `WORKDIR`（默认 `/home/agent/workspace`）。
- ⚠️ **云端档根本没有宿主工作区**：*"Cloud sandboxes have no host workspace, so no path follows the agent."* / *"Cloud sandboxes don't accept a local workspace path."* ——**「工作区」在他们的云端产品里不存在，沙箱的文件系统就是全部。** 这是我们与它最大的模型差异，也是「工作区比沙箱活得久」在他们那里无处安放的原因。

**对我们**：Direct 的「宿主绝对路径 = 沙箱内绝对路径」正是我们的工作区树；`--clone` 那种「宿主只读原仓 + 沙箱内私有克隆 + 一个具名 remote 取回」**是一条值得记下的形状**（它与我们「远端 git 动词由 Worker 在宿主侧代执行」不同，但解决的是同一个问题：让 agent 的提交能回到宿主仓）。

### 2.6 出网与凭据：宿主侧代理，与我们同侧

（[architecture](https://docs.docker.com/ai/sandboxes/architecture/)、[security/defaults](https://docs.docker.com/ai/sandboxes/security/defaults/)、[sbx_secret.yaml](https://github.com/docker/docs/blob/main/data/sbx_cli/sbx_secret.yaml)）

- *"All outbound TCP traffic from the sandbox routes through a proxy on your host."* / *"Agents use a forward proxy for HTTP and HTTPS; other TCP traffic is forwarded transparently."*
- **默认姿态要读准**：策略**未初始化**前没有默认（`sbx policy init` *"must be run before adding custom allow/deny rules or starting a sandbox for the first time"*），初始化之后是**白名单制**——*"All outbound TCP traffic, including HTTP, HTTPS, and SSH, is blocked unless an explicit rule allows the destination."*、*"Outbound UDP is disabled by default."*、*"ICMP is blocked."*、*"DNS queries use the sandbox's internal resolver, which enforces network policy."*
- **策略档位有三个，名字在 CLI 里**：`sbx policy init allow-all|balanced|deny-all`，其中 *"balanced — Typical development traffic is allowed, such as AI services and package registries"* 是**文档推荐的默认**。这是「档位化 egress」最直白的一个产品级先例——**且 `balanced` 这个档位恰好承认了我们 `egress: public` 的那条理由**（改代码要能装依赖）。
  ⚠️ **与本批的 `sandbox-network-policy-apis.md` §0.1 对读**：那份的结论是「行业默认几乎都是出网开着，默认关的只有 Claude Code / sandbox-runtime 与 Cilium·K8s NetworkPolicy 两家」。**本节要补进去的是两个新数据点**——Agent Sandbox 的 `NetworkPolicySpec` 空列表 = ingress/egress 双 Default Deny（§1.4），以及 Docker Sandboxes 的**白名单制 + `balanced` 档**（本节）。前者其实是 Cilium·K8s 那家的一部分（它就用 NetworkPolicy），**真正的新增量只有 `balanced` 这个「够用的中间档」**。
- 凭据：*"Credential values never enter the VM."* / *"The proxy replaces the sentinel with the real credential after the request has left the microVM."* / *"The credential remains on the host."* ——**这就是 `ADR-0001` 的路线 A，一字不差**；`sbx_secret` 还区分 **service secrets（代理注入）** 与 **registry secrets（宿主专用，"not injected into sandboxes unless --all-sandboxes or --sandbox is set"，"the credential never enters the sandbox filesystem"）**。
- ⚠️ 策略不可改动的清单（原文列举）：宿主文件系统（挂载点与共享 skills store 之外）、Host Docker daemon、**沙箱之间直连**、外部 ICMP。**「沙箱之间不能直连」是这条边界的一等公民。**

### 2.7 该抄 / 不该抄

**该抄**
- ⭐ **`sbx policy init allow-all|balanced|deny-all` 的档位命名与 `balanced` 的定位**。`ADR-0003` 现在把 `public` 做成默认并说「白名单是追加入口」；**`balanced` 这个名字比自己发明一个更值得直接借**——它把「够用的默认」和「全开/全关」并排摆出来，且用文档明确了每个档位放行什么。
- ⭐ **`sbx stop`（留状态）与 `sbx rm`（删净）是两个动词，且 `sbx rm` 之后 "only host workspace files remain"**。这正是我们要写进 GLOSSARY 的那句「冷档只剩宿主目录」的产品级措辞。
- ⭐ **「云端档没有宿主工作区」这条反例**：它反证了「工作区树」不是免费概念——**没有宿主目录时，你就退化成「沙箱的文件系统就是全部」，而那就必须上快照/volume/TTL 一整套。** 我们的「工作区树是唯一活着的东西」之所以能换来简单，代价是**每台机器都要有可写的宿主目录**。这条要写进 `ADR-0006` 的 Consequences。
- ⭐ **`--clone` 模式 + `sandbox-<name>` git remote**：宿主原仓只读、沙箱内私有克隆、具名 remote 取回。**这是我们「远端动词由 Worker 代执行」之外的第二条可行路**，值得在 `sandbox-git-integration` 那份里对照。
- ⭐ **能力不支持就拒绝**（`--cloud` 下 `-d/--user/--privileged` *"are rejected rather than silently ignored"*）。
- **`sbx daemon`（`sandboxd` 的 start/stop/status/log-level）**：宿主侧守护进程被做成**可运维的对象**，而不是黑盒。我们的 Worker 是同侧角色。

**不该抄**
- ⚠️ **`sbx exec` 对已停止沙箱的隐式拉起**（详见 §2.4，与 `prior-art` §8 第 2 条同一条）。
- ⚠️ **云端 stop = 内存 + 磁盘快照**（*"its full state (memory + disk) is preserved"*）：`prior-art` §10 第 1 条已从五家收敛到「不做内存快照」，这是第六个数据点，**不改变结论**。
- ⚠️ **`sbx volume` 的「退出时才快照、最后退出者胜」**：一个会静默丢数据的默认，任何形态都不要抄。
- ⚠️ **`sbx template` 这套「快照即制品」的持久化答案**：与 volume、宿主工作区并排成为第三套（`prior-art` §8 第 5 条）。我们只有一个答案。
- **云端的 `--cloud` 全局 flag 一家两制**：同一个动词在本地走 `sandboxd`、在云端走云 API，能力集不同（*"Cloud and local sandboxes have separate state and different capabilities."*），且要用户自己记住哪些 flag 被拒。**我们的三种后端共用一套动词与一个 `Capabilities()`，不要引入「同一个动词在两个后端上语义不同」这层。**
- **`sbx ttl` 的「服务端只能延长不能缩短」**：一个有意思的单调性，但我们的保留期由控制层单方裁决（`ADR` 里的 Retention），不需要这种不可逆。

---

## 3. OpenHands（原 OpenDevin）

> **先纠正一个前提：仓库已经重构，你点名的那个路径今天不存在。** `All-Hands-AI/OpenHands` 跳转到 `OpenHands/OpenHands`，其主分支已是 TypeScript 控制面（"Agent Canvas"）；`.../tree/main/openhands/runtime` 返回 **404**。Python 的 V0 runtime 在 **2026-04-24** 被删除（commit `e86067c15b54242fd611877aa9038a2f7a219658`，改动 "1,296 additions & 31,968 deletions" across 134 files，含 `openhands/runtime/action_execution_server.py`）。V1 拆成三个仓：**`OpenHands/sandbox-server`**（在宿主上造容器的服务）、**`OpenHands/software-agent-sdk`**（含 `openhands-workspace/` 运行时客户端与 `openhands-agent-server/` 容器内服务）、`OpenHands/legacy`（归档）。**本节的 V0 引文全部钉在删除前一个 commit `137bede1f5e867a7eb17ae36e78a05ffef6ec09a` 上**，V1 引文钉在各仓 `main`。

### 3.1 V0 的对外 REST 面：八个平铺端点，没有前缀

（[action_execution_server.py](https://raw.githubusercontent.com/OpenHands/OpenHands/137bede1f5e867a7eb17ae36e78a05ffef6ec09a/openhands/runtime/action_execution_server.py)，源码读出）

```
@app.get('/server_info')      @app.post('/execute_action')   @app.post('/update_mcp_server')
@app.post('/upload_file')     @app.get('/download_files')    @app.get('/alive')
@app.get('/vscode/connection_token')                          @app.post('/list_files')
```

`/execute_action` 的载荷就是「一个 action 事件」：客户端 `{'action': event_to_dict(action)}`，服务端 `return event_to_dict(observation)`；失败是 `400 'Invalid action type'` 与 `500 f'Internal server error: ...'`。`/alive` 是一行存活探针（`{'status': 'not initialized'}` / `{'status': 'ok'}`），调用方在用它之前先探。

### 3.2 文件与命令：一个面，加一个补充面——比 Argo/Agent Sandbox 都更「合一」

⭐ **读、写、编辑文件走的是那条命令通道本身**，因为它们是**同为 action 的另一种类型**（[action_execution_client.py](https://raw.githubusercontent.com/OpenHands/OpenHands/137bede1f5e867a7eb17ae36e78a05ffef6ec09a/openhands/runtime/impl/action_execution/action_execution_client.py)）：

```python
def run(self, action: CmdRunAction)  -> Observation: return self.send_action_for_execution(action)
def read(self, action: FileReadAction) -> Observation: return self.send_action_for_execution(action)
def write(self, action: FileWriteAction) / def edit(self, action: FileEditAction)
```

**只有「列目录 + 批量搬运」走第二个面**：`/list_files`（10s 超时）、`/upload_file`（300s）、`/download_files`（30s，服务端把目录打成 zip 后 `FileResponse(media_type='application/zip')`）。

**V1 保留了同一条切法**：`/api/bash/*`（`/start_bash_command`、`/execute_bash_command`、`/bash_commands/{id}/stop`）与 `/api/file/*`（`/upload`、`/download`、`/create_directory`、`/archive`、`/search_subdirs`）（[bash_router.py](https://raw.githubusercontent.com/OpenHands/software-agent-sdk/main/openhands-agent-server/openhands/agent_server/bash_router.py)、[file_router.py](https://raw.githubusercontent.com/OpenHands/software-agent-sdk/main/openhands-agent-server/openhands/agent_server/file_router.py)）。

**对我们的意义**：`vendor-file-apis.md` 的九家厂商是「文件面独立于命令面」，Agent Sandbox 的 `sandboxd` 是「两个面、两条协议」，**OpenHands 是第三种**——**文件读写本身就是命令**。我们选了厂商那一路（`ls_files`/`download_file`/`upload_files` 与 Task 无关），**但要注意 OpenHands 这条路的代价与收益**：代价是每个文件操作都要一个 agent 级的 action 往返与一套 action 类型表；收益是**文件与命令天然共享同一个信任与超时模型**。**我们的 `ADR-0006` 把文件面做成「与 Task 无关」是对的，但那条「上传 300s / 下载 30s / 列表 10s」的分档超时值得抄**——三个动词的合理时间尺度差一个数量级。

### 3.3 谁造容器：控制层，而且是「容器里住一个服务器」

- **一个会话一个容器**：README 原文 *"Creates and manages a Docker container for each session"*；造它的是**持有 docker client 的后端进程**：`self.docker_client.containers.run(...)`（[docker_runtime.py](https://raw.githubusercontent.com/OpenHands/OpenHands/137bede1f5e867a7eb17ae36e78a05ffef6ec09a/openhands/runtime/impl/docker/docker_runtime.py)）。**没有任何节点侧 agent。**
- ⚠️ **容器里有一个监听 `0.0.0.0` 的 HTTP 服务器**：`run(app, host='0.0.0.0', port=args.port, ...)`，端口作为**第一个位置参数**传给模块（`python -m openhands.runtime.action_execution_server <port> --working-dir ...`）。V0 的端口取自 `EXECUTION_SERVER_PORT_RANGE = (30000, 39999)` 且 **`self._container_port = self._host_port`**（宿主端口 = 容器端口，不做 NAT）；**V1 固定 8000**（`ExposedPort(name=AGENT_SERVER, ..., container_port=8000)`，另有 VSCODE 8001、WORKER_1 8011、WORKER_2 8012）。
- **鉴权是创建者铸造的 per-sandbox 密钥**：V0 从 env 读 `SESSION_API_KEY`，配 `APIKeyHeader(name='X-Session-API-Key')`；V1 在造容器时 `base62.encodebytes(os.urandom(32))` 然后写成 `env_vars['OH_SESSION_API_KEYS_0'] = session_api_key`（[docker_sandbox_service.py](https://raw.githubusercontent.com/OpenHands/sandbox-server/main/openhands/app_server/sandbox/docker_sandbox_service.py)、[session_auth.py](https://raw.githubusercontent.com/OpenHands/sandbox-server/main/openhands/app_server/sandbox/session_auth.py)）。⚠️ **密钥随容器创建被写进容器自己的环境变量**——**这与 Agent Sandbox 的 `--auth-token-sha256` 是同一类选择，同样与我们的「沙箱里没有可认证的东西」相反。**

### 3.4 工作区挂载：`volumes` 字典，没配就什么都不挂

工作区是 **docker `volumes` 的绑定挂载**，不是 `--mount`：配置串 `sandbox.volumes` 按逗号切、再按冒号切成 `host:container[:mode]`，默认 `rw`：

```python
volumes[host_path] = { 'bind': container_path, 'mode': mount_mode, }
```

并且有一条**明确的降级**：*"If no volumes were configured … Mount dir is not set, will not mount the workspace directory to the container"*。V1 从环境变量 `SANDBOX_VOLUMES` 读同一套串（[config.py](https://raw.githubusercontent.com/OpenHands/sandbox-server/main/openhands/app_server/config.py)，注释说 *"This is set by the CLI's `--mount-cwd` flag"*）。

**与我们对照**：宿主绝对路径 → 容器绝对路径的绑定挂载**是同一种东西**；差别是我们的工作区**必挂且是唯一命名空间**，他们是**可选的、可多个的、没配就没有**。⭐ **「没配就没有工作区、且容器照常起来」这一点值得看**：它说明「工作区」在很多实现里是独立于沙箱的可选项，而我们把两者绑成 1:1 是一个更强的约束——**更强不等于更好，但它让「冷档只剩宿主目录」这句话成立**。

### 3.5 生命周期：容器不随会话结束消失，且可以 pause

- `remove=False`；V0 用 `docker pause` / `docker unpause` 冻结容器内所有进程（[docker_runtime.py](https://raw.githubusercontent.com/OpenHands/OpenHands/137bede1f5e867a7eb17ae36e78a05ffef6ec09a/openhands/runtime/impl/docker/docker_runtime.py)）。
- V1 的抽象服务面：`search_sandboxes` / `get_sandbox` / `start_sandbox` / `resume_sandbox` / `pause_sandbox` / `delete_sandbox`，状态枚举 **`SandboxStatus(Enum): STARTING, RUNNING, PAUSED, ERROR, MISSING`**，`MISSING` 注解为 *"Missing - possibly deleted"*。
- ⭐⭐ **嵌套关系是 `Sandbox ⊃ Conversation`**（[sandbox_models.py](https://raw.githubusercontent.com/OpenHands/sandbox-server/main/openhands/app_server/sandbox/sandbox_models.py)），且模型分成两层：**`SandboxRecord`**——*"Persisted identity fields for a sandbox — no live runtime data. Contains only what is stored in the app server's own database (id and owner)"*，与 **`SandboxInfo`**——*"Use `SandboxInfo` when you additionally need live status, exposed URLs, or the plain-text session key returned by the runtime."*
  ⭐ **「持久身份」与「活体状态」分成两个类型**，正是我们 `prior-art` §3.4 从 kubelet 的 `lastHeartbeatTime` / `lastTransitionTime` 推出的「存活与状态解耦」，这里是**类型系统层面**的表达，而且它的理由是自拨号设计里最本质的那条：**存下来的那一半是控制层知道的，另一半只有活着的那条连接知道。**
- **`remote` 后端暴露了一个独立的 Runtime API**（`X-API-Key`）：`GET /sessions/{sandbox_id}`、`GET /sessions/batch`、`POST /start`、`POST /resume`、`POST /pause`、`POST /stop`、`GET /list`（[remote_sandbox_service.py](https://raw.githubusercontent.com/OpenHands/sandbox-server/main/openhands/app_server/sandbox/remote_sandbox_service.py)）。⚠️ **`/start` 的请求体是控制层指定的**：`{'image': ..., 'command': ..., 'working_dir': '/workspace', 'session_id': ..., 'run_as_user': 10001, 'run_as_group': 10001, 'fs_group': 10001}`，`runtime_class == 'sysbox'` 时再加 `runtime_class: 'sysbox-runc'`。**即「在哪里跑、以谁的身份跑」由控制层裁决、由远端执行**——**这正是我们 `#10`「匹配权威在调度器、Provider 拒绝即 bug 信号」的形状**，也是本票里唯一一个把它写成 HTTP 契约的先例。
- **K8s 后端就是接 Agent Sandbox**：`KubernetesSandboxService` 自述 *"Kubernetes implementation backed by kubernetes-sigs/agent-sandbox"*；SDK 侧还有 `openhands/workspace/agent_sandbox/` 目录，带 `deploy/sandboxtemplate.yaml` 与 `deploy/sandboxwarmpool.yaml`。**§1 与 §3 是同一个东西的两层**——这解释了为什么 Agent Sandbox 的 `sandboxd` 与 OpenHands 的 agent server 形状这么像。

### 3.6 隔离：容器路径上什么都没声明

**V0 与 V1 的 `containers.run(...)` 参数表里都没有 `security_opt`、没有 `privileged`、没有 `user`**（源码逐字读出的完整参数列）。V1 的两处**可选**放宽：`devices = ['/dev/kvm:/dev/kvm:rwm'] if self.kvm_enabled else None`、`network_mode = 'host' if self.use_host_network else None`。用户/组隔离只出现在 remote 路径（`run_as_user`/`run_as_group`/`fs_group` = 10001，可选 Sysbox）。

**项目自己的话**（[runtime/README.md](https://raw.githubusercontent.com/OpenHands/OpenHands/137bede1f5e867a7eb17ae36e78a05ffef6ec09a/openhands/runtime/README.md)）：*"**Important: This runtime provides no isolation as it runs directly on the host machine.**"*（说 local runtime）；对 remote runtime 只说 *"Potential for improved security through isolation"*；sandbox 模块的框架句是 *"Since agents can do things that may harm your system, they are typically run inside a sandbox (like a Docker container)."* ——**「减害」而不是「边界」**，与 `ADR-0003`「沙箱不是安全边界」同调。

### 3.7 该抄 / 不该抄

**该抄**
- ⭐ **`SandboxRecord` / `SandboxInfo` 的类型二分**（持久身份 vs 活体状态）。这是自拨号设计里「控制层的视图永远可能陈旧」在类型系统上的正确表达。
- ⭐ **remote Runtime API 的 `/start` 请求体**（控制层指定 image / command / working_dir / uid）。它是 `#10` 那条分工的现成契约形状：**控制层说「要什么」，执行侧说「做不到就拒」。**
- ⭐ **三个文件动词的分档超时**（列表 10s / 下载 30s / 上传 300s）。数量级差异是真实的，写进我们的能力面文档比一个统一超时好。
- ⭐ **「列 + 批量搬运」与「单个文件读写」分成两个面**，后者可以搭在命令通道上。我们的 `ls_files`/`download_file`/`upload_files` 属于前者（且字节走对象存储），**`read`/`write`/`edit` 那些我们刻意不做**——这条切法支持了我们的取舍。
- **`docker pause/unpause` 作为廉价的暂停**（不销毁、不卸载）。⚠️ 但见下。

**不该抄**
- ⚠️ **容器里一个监听 `0.0.0.0` 的服务器 + 创建者铸造的 `X-Session-API-Key` 写进容器环境变量**。与 §1 同一个问题，且这里更直接：**密钥在容器里**。
- ⚠️ **`EXECUTION_SERVER_PORT_RANGE` + 宿主端口 = 容器端口**：为了绕开端口冲突而占一个 10000 宽的端口段，是小规模实现的典型权宜。我们不用端口，这个问题不存在——**但它是「沙箱里有服务」这条路的隐性税，值得记一笔**。
- ⚠️ **`network_mode='host'` 与 `/dev/kvm` 作为可选档**：两个都能单独抹掉这条边界，且是**建时**决定的开关。
- ⚠️ **V0 那个 `**(self.config.sandbox.docker_runtime_kwargs or {})` 逃生口**：把整个 docker API 参数表开给配置。`prior-art` §1.9 已经记过 Blaxel 的同类（「只能在创建时设置的配置轴 = 永久工单」）。
- **`remove=False` 的默认 + `docker pause` 当暂停机制**：pause 冻结的是进程，容器与它的 netns 都还在，**于是「暂停中」的沙箱仍然占着宿主资源、仍然有一个 netns**。我们的冷档是**容器真的没了**（`ADR-0003` 的启动时序、`#9` 的保留），**更彻底也更简单**。
- **V0 那八个平铺端点与 V1 的仓拆分**：`/update_mcp_server`、`/vscode/connection_token` 这类把 IDE 与 MCP 塞进沙箱侧 API 的做法，我们不需要；而三个仓的拆分是 V1 迁移的产物，不是设计目标。

---



只取与「容器为沙箱 + 文件面 + git + 网络」有关的**接口设计**，不写调度。

## 4. 「每工作区一个容器」：Codespaces / DevPod / Gitpod(Ona)

### 4.1 GitHub Codespaces：容器与它的整块盘一起消失，**git 才是持久化**

- **对象模型**：仓库（+ 其中的 `devcontainer.json`）→ codespace → Docker 容器 → VM。*"Each codespace you create is hosted by GitHub in a Docker container, running on a virtual machine."* / *"When you connect, you are placed within the Docker container."* / *"You have limited access to the outer Linux virtual machine host."*（[overview](https://docs.github.com/en/codespaces/overview)）
- ⚠️ **一 codespace 一 VM，绝不共用**：*"Two codespaces are never co-located on the same VM."* / *"Every time you restart a codespace, it's deployed to a new VM"* / *"Each codespace has its own isolated virtual network."* / *"Only the creator of a codespace can connect to a codespace."*（[security](https://docs.github.com/en/codespaces/reference/security-in-github-codespaces)）——**「一次重启 = 一次换机器」在我们这里不可能**（`#9`：Workspace 只能回到归属 Worker）。
- **谁建容器**：**GitHub 控制面**，动作是一次鉴权的 HTTP 调用——`POST /user/codespaces` 或 `POST /repos/{owner}/{repo}/codespaces`（[REST](https://docs.github.com/en/rest/codespaces/codespaces)）。节点侧没有任何东西触发它。
- **接口三个**：`gh codespace` CLI（`code`/`cp`/`create`/`delete`/`edit`/`jupyter`/`list`/`logs`/`ports`/`rebuild`/`ssh`/`stop`/`view`）、SSH（*"The default dev container image includes an SSH server, which is started automatically."*）、以及上列 REST。**能进容器的是 GitHub 发的凭据，不是容器里某个 agent 拨出来的。**
- **端口转发**：*"your application becomes available at the URL `https://CODESPACENAME-PORT.app.github.dev`"*，三级可见性 `private`（默认）/ `org` / `public`（[forwarding-ports](https://docs.github.com/en/codespaces/developing-in-a-codespace/forwarding-ports-in-your-codespace)）。⚠️ **要转发就得有端口**——又一个「沙箱里有服务」的前提。
- **生命周期**：空闲默认 **30 分钟**（可设 5–240 分钟），**硬上限 12 小时**；保留期默认 **30 天**（范围 0–30，且 *"set when the codespace is created and does not change"*）；停后 *"only storage costs"*，运行才计 CPU（[timeout](https://docs.github.com/en/codespaces/setting-your-user-preferences/setting-your-timeout-period-for-github-codespaces)、[retention](https://docs.github.com/en/codespaces/managing-codespaces-for-your-organization/restricting-the-retention-period-for-codespaces)、[stopping](https://docs.github.com/en/codespaces/developing-in-a-codespace/stopping-and-starting-a-codespace)）。
- ⭐ **工作区树的位置**：*"cloned into the `/workspaces` directory of the VM and subsequently mounted into the dev container."*（[deep-dive](https://docs.github.com/en/codespaces/about-codespaces/deep-dive)）——**宿主目录 + bind mount，与我们同形**；而且它是**唯一跨 rebuild 存活的东西**：*"changes you have made outside the `/workspaces` directory are cleared"*（[rebuilding](https://docs.github.com/en/codespaces/developing-in-a-codespace/rebuilding-the-container-in-a-codespace)）。
- ⚠️⭐ **但他们把持久化交给了 git，不是交给宿主目录**：*"if your codespace is deleted, then your work will be deleted too."* / *"To persist your work, you will need to commit your changes and push them to your remote repository"*（[lifecycle](https://docs.github.com/en/codespaces/about-codespaces/understanding-the-codespace-lifecycle)）。**这是一个真正的模型分歧**：Codespaces 里「工作区树」是**缓存**，权威在远端仓库；我们这里工作区树是**唯一活着的东西**，所以我们必须有「宿主目录 + 保留期」这一层，他们不需要。**这条差别要写进 `ADR-0006` 的 Consequences，它是「为什么我们要管保留而他们不用管」的答案。**
- 仓库里有 `devcontainer.json` 的 **prebuild** 机制（*"creating a codespace from a prebuild can be substantially quicker"*）。**容器内是否有 agent、连接方向如何——[未证实]**（查了 overview / deep-dive / security / troubleshooting-your-connection / forwarding-ports / CLI 页）。

### 4.2 DevPod：**本批唯一的「客户端往工作区里部署一个 agent」**，且把 git 凭据转发进远端

- **对象模型**：workspace（容器化环境）+ provider（造宿主的 CLI 程序）+ agent + `devcontainer.json`。*"Each developer environment runs in a separate container and is specified through a devcontainer.json."*（[what-is-devpod](https://devpod.sh/docs/what-is-devpod)）
- ⭐⭐ **agent 的故事讲得最清楚，而且与我们的方向相反**：
  > *"Devpod uses a client-agent architecture, where the client deploys it's own agent"* … *"to host various servers, such as a grpc server or SSH server"*
  > *"While creating the workspace DevPod deploys an agent to the machine running the container"* … *"as well as to the container itself"* … *"to provide useful functions such as port forwarding, credential forwarding and log streaming."*
  > *"Once the devcontainer is running DevPod deploys an agent to the container."* / *"The DevPod agent starts a SSH server using the STDIO of the secure tunnel"*
  （[how-it-works](https://devpod.sh/docs/how-it-works/overview)）
  **方向是 client → workspace**：客户端经**provider 特有的 tunnel**（AWS 是 instance connect、K8s 用 `kubectl`）连进去，agent 在里面起一个 SSH/gRPC 服务器。**它不是自拨号 worker——但它证明了「一个往工作区里放的常驻进程 + 一条隧道 + 多路复用的服务器」这套形状在容器世界里可行。**
- **谁建容器**：**用户自己的客户端**。*"Client-only: No need to install a server backend, DevPod runs only on your computer."* / `devpod up` *"wrapping your conventional CLI tools such as kubectl, docker, gcloud"*。商业版 DevPod Pro 才有一个服务器，把同一条命令经 websocket 流出去（文档在 loft.sh，不在 devpod.sh）。
- ⚠️⚠️ **工作区是「拷进去」的，不是挂进去的；而且 git 凭据被转发进远端机器**：
  > *"DevPod will sync the folder into the remote machine and create a development environment from the `devcontainer.json`."*
  > *"DevPod will forward git credentials to a remote machine so that you can also pull private repositories."*
  （[create-a-workspace](https://devpod.sh/docs/developing-in-workspaces/create-a-workspace)）
  **后一句是 `ADR-0001` 的正面反面**：凭据为了「能拉私有仓」被送进了执行侧。**这是我们最该引用的一条反例**——它与 Tekton 的 `basic-auth` 工作区、OpenHands 的 `X-Session-API-Key` 是同一类取舍，而这三家都选了「送进去」。
- 容器内路径 `/workspaces`：**devpod.sh 文档里没有**，是从官方仓库源码读出的（`cmd/ssh.go` 的 `filepath.Join("/workspaces", ...)`，及仓库自带测试夹具里的 `"workspaceFolder": "/workspaces"`）——**源码读出，非文档**。
- **生命周期**：stop 由 provider 决定（停容器或关 VM）；*"Deleting a workspace means to erase all state of an existing workspace"*；**重建是有损的**：*"When recreating a workspace, changes only to the project path or mounted volumes will be preserved"*，而 *"All other changes made in the container will be lost."*（[stop](https://devpod.sh/docs/developing-in-workspaces/stop-a-workspace)、[delete](https://devpod.sh/docs/developing-in-workspaces/delete-a-workspace)）
- `devpod exec` 的文档页 **[未证实]**（本轮未打开）。

### 4.3 Gitpod → **Ona**（改名已确认），以及「双盘」这个我们最该看的形状

- ⭐ **改名确认**：官方仓库 README 首句 *"Gitpod has been renamed to Ona - mission control for software projects and software engineering agents"*（[gitpod-io/gitpod README](https://raw.githubusercontent.com/gitpod-io/gitpod/main/README.md)）；`https://www.gitpod.io/` 返回 **308 Permanent Redirect** 到 `https://ona.com/`；changelog 里 2026-06-26 一条 *"Stale Gitpod branding in the dashboard has been replaced with Ona branding."*。**改名的确切日期 [未证实]**（README 与本轮读过的 changelog 都没有宣布日期）。
- **对象模型**：environment（就是工作区）/ environment class（`instance type` + `disk size`，"Only name and description can be edited after creation."）/ runner（造算力的基础设施）。**没有独立的 workspace 对象**。
- ⭐⭐ **「谁建容器」的答案是 `runners execute, management plane coordinates`**：*"Runners are the infrastructure layer that provisions and manages your environments and agents."* / *"The management plane coordinates. Runners execute."*（[runners/overview](https://ona.com/docs/ona/runners/overview.md)）——**这正是我们控制层 ↔ Worker 的分工**，而且 runner 跑在**客户自己的 VPC** 里（AWS CloudFormation 部署，AMI `gitpod/images/gitpod-next/ec2-runner-ami-*`）。
- ⭐⭐ **runner → 管理面是出站的，而且出网是启动的承重条件**：*"The runner verifies reachability by dialing each endpoint directly, bypassing any HTTP proxy."* / *"The ECS task security group must allow outbound HTTPS (port 443) to the AWS service endpoints."* / *"Results are reported to the management plane as a network readiness status."*；以及 *"Without a public IP, the runner tasks will have no outbound connectivity and the runner will fail to start."*（[networking](https://ona.com/docs/ona/runners/aws/networking.md)、[setup](https://ona.com/docs/ona/runners/aws/setup.md)）
  **这是本票里离我们最近的一条**：一个部署在客户 VPC 里的执行层，**自己拨向管理面并上报状态**。⚠️ 但**「管理面是否会反向连进 runner」与「是否有 per-environment 的容器内 agent」都是 [未证实]**（查了 runners/overview、aws/overview、aws/setup、aws/networking、enterprise-runner/overview、environments/overview、persistent-storage、dual-disk）。
- **接口**：REST `POST /api/gitpod.v1.EnvironmentService/CreateEnvironment`（*"Creates a development environment from a context URL (e.g. Git repository) and starts it."*）；CLI `ona environment create|start|stop`、`ona environment port list|open|close`、`ona environment ssh-config`，然后 `ssh <environment-id>.ona.environment`；*"The CLI is pre-installed and automatically authenticated with limited access."*（[cli](https://ona.com/docs/ona/integrations/cli.md)）**start/stop/delete/list 的确切 verb+path [未证实]**。
- ⚠️ **端口必须监听 `0.0.0.0`**：*"Listen on `0.0.0.0` (not `localhost` or `127.0.0.1`)"*，端口范围 1024–65535，*"By default, the CLI opens the port for the environment creator only."*（[ports](https://ona.com/docs/ona/integrations/ports.md)）Gitpod Classic 时代是 `3000-yourworkspace.ws-eu45.gitpod.io` 那种子域模型（[classic ports](https://ona.com/docs/classic/user/configure/workspaces/ports.md)）。**再一个「沙箱里必须有服务」的前提。**
- ⭐⭐⭐ **「双盘」是我们既定形状最强的一个结构先例**（Enterprise 档）：
  > *"Dual disk environments separate the VM system disk from the environment data disk"*；系统盘放 *"the operating system and runner-managed boot files"*，数据盘放 *"the workspace and development state that should survive VM replacement"*，数据盘内容含 *"Repository contents and uncommitted changes under `/workspaces`"*
  > **"Dual disk preserves environment data, not the identity of the VM"**
  （[dual-disk](https://ona.com/docs/ona/runners/dual-disk.md)）
  **这就是我们的「Workspace 比承载它的 Sandbox 活得久」，用两块盘实现**：机器身份可以丢，工作区数据不能丢。**在论证我们那条 GLOSSARY 定义时，这是唯一一个把它做成产品特性并写明「保留的是数据、不是 VM 的身份」的一手来源。** 同页也有我们那条硬约束：*"Deleting an environment still permanently removes its environment data."*
- **停后什么活着**：*"Everything on disk persists across environment stops and starts"*，包含 `/workspaces/<repository-name>` 与 `/home/gitpod`；状态表 `Stopped` = *"Persists - resume anytime"*、`Archived` = *"Persists - can be unarchived"*、`Deleted` = *"Permanently removed"*；rebuild 只靠 bind mount 保住代码（[persistent-storage](https://ona.com/docs/ona/environments/persistent-storage.md)）。
- **保留期**：自动停默认 **30 分钟**（活动按 5 分钟粒度计，*"may take several more minutes to stop"*）；Core 档**闲置 3 天归档、归档后 4 天自动删除**，且 *"Only archived environments can be restored."*（[timeout](https://ona.com/docs/ona/organizations/policies/environment-timeout.md)、[archive-auto-delete](https://ona.com/docs/ona/environments/archive-auto-delete.md)）对照 **Gitpod Classic**：*"Workspaces are deleted after 14 days"*、*"Only files and directories inside `/workspace` are preserved"*（[classic lifecycle](https://ona.com/docs/classic/user/configure/workspaces/workspace-lifecycle.md)）——**两代的保留策略差了一个数量级，且 Classic 的「只保 `/workspace`」正好对应我们的「只保工作区树」。**

### 4.4 该抄 / 不该抄

**该抄**
- ⭐⭐ **Ona 的 dual disk**：*"preserves environment data, not the identity of the VM"*。这是我们「工作区比 Sandbox 活得久」最好的一句外部措辞，直接进 GLOSSARY 的旁证。
- ⭐⭐ **Ona 的「management plane coordinates, runners execute」+ runner 自己拨向上报状态**。这是本票里唯一一个与我们的控制层/Worker 分工同形、且部署在**客户自己的 VPC** 里的产品。
- ⭐ **Codespaces 的 `/workspaces`：宿主目录 + bind mount，且是唯一跨 rebuild 存活的东西**。与我们同形，可作为「工作区树挂在宿主目录上」的常规做法引用。
- ⭐ **Codespaces 的保留参数（空闲 30 分钟 / 硬上限 12 小时 / 保留期 30 天且创建后不可变）** 是一组现成的、有文档的量级参考；**尤其「保留期在创建时设定且之后不改」**——比我们「控制层可随时裁裁决保留期」更简单，值得在 `ADR-0007` 里当成一个被否决的选项写下来。
- **DevPod 的 agent 形状**（往工作区里放一个进程、里面跑多路复用服务器、经 tunnel 连入）——**形状值得看，方向不要抄**（见下）。

**不该抄**
- ⚠️⚠️ **DevPod 把 git 凭据转发进远端机器**（*"forward git credentials to a remote machine"*）。这是 `ADR-0001` 最直接的正面反面，**与 Tekton 的 `basic-auth` 工作区是同一类**。
- ⚠️ **DevPod 的客户端进容器（client → workspace tunnel）**：它不是自拨号 worker，而是「客户端拥有到工作区的路径」那条老路（`prior-art` §0 第 1 条）。**我们的 Worker 是沙箱侧唯一认证主体，方向相反。**
- ⚠️ **Codespaces 的「一 codespace 一 VM、重启即换机器、从不共置」**：托管多租户下的正确选择，代价是**每次重启都换机器**——我们的 Workspace 绑机器，做不到也不该做。
- ⚠️ **Codespaces 把持久化外包给 git**（删掉 codespace 就等于删掉工作）。**我们不行**：工作区树是唯一活着的东西，所以「宿主目录 + 保留期」这一层必须自己管。**这条差别要写进 `ADR-0006`。**
- ⚠️ **Ona 与 Codespaces 的端口模型都要求容器里监听 `0.0.0.0`**（`ona environment port open`；Codespaces 的 `*.app.github.dev`）。与 §1.6 同一个冲突。
- **DevPod 的 `devpod delete --force`**（*"the workspace will only be deleted on the DevPod side locally"*，provider 报错被忽略）——一个会留下幽灵资源的删除语义。

---



只取与「容器为沙箱 + 文件面 + git + 网络」有关的**接口设计**，不写调度。

## 5. K8s 原生的执行/工件形态：Argo Workflows 的 artifact 与 Tekton 的 workspace / Results

只取与「容器为沙箱 + 文件面 + git + 网络」有关的**接口设计**，不写调度。

### 5.1 Argo：**声明的产物**在这里是完整的形态

**产物由「步骤自己的模板」声明，不由调用方声明。** 调用方只能按**名字**绑定别人的产出（[artifacts walk-through](https://argo-workflows.readthedocs.io/en/latest/walk-through/artifacts/)）：

```yaml
arguments:
  artifacts:
    - name: message
      from: "{{steps.generate-artifact.outputs.artifacts.hello-art}}"
```

`Artifact` 的字段（源码，[workflow_types.go](https://github.com/argoproj/argo-workflows/blob/main/pkg/apis/workflow/v1alpha1/workflow_types.go)）：`name`（"must be unique within a template's inputs/outputs"）、`path`（"Path is the container path to the artifact"）、`from`（"allows an artifact to reference an artifact from a previous step"）、`optional`、`subPath`、`mode`（"recommended to set the mode value to ensure the artifact has the expected permissions in your container"）、`recurseMode`、`globalName`、`archive`（`tar` / `none` / `zip`）、`artifactGC`。路径可以是目录：*"the artifact's `path` may be a directory rather than just a file."* 默认打包：*"Artifacts are packaged as Tarballs and gzipped by default."*

**仓库配置与「哪个桶、哪个前缀」**（[configure-artifact-repository](https://argo-workflows.readthedocs.io/en/latest/configure-artifact-repository/)）：ConfigMap `workflow-controller-configmap` 的 `artifactRepository` 键；S3 侧键为 `bucket` / `keyFormat` / `endpoint` / `insecure` / `region` / `useSDKCreds` / `accessKeySecret` / `secretKeySecret` / `addressingStyle` 等。默认 key 模板来自源码常量 `DefaultArchivePattern = "{{workflow.name}}/{{pod.name}}"`，落盘时再 `path.Join(key, fileName)`（[artifact_repository_types.go](https://github.com/argoproj/argo-workflows/blob/main/pkg/apis/workflow/v1alpha1/artifact_repository_types.go)、[executor.go](https://github.com/argoproj/argo-workflows/blob/main/workflow/executor/executor.go)）。⚠️ `keyPrefix` 在源码里标着 `// Deprecated: Use KeyFormat instead.`

⭐ **谁搬运、谁持凭据——本节对我们最有价值的一条。** 搬运由 `argoexec` 在 pod 的 **init 容器（Load）** 与 **wait 容器（Save）** 里完成，跑在 pod 自己的网络命名空间里；凭据不是从 API 拿的，而是**从一个只读 Secret 卷里读文件**：`os.ReadFile(filepath.Join(common.SecretVolMountPath, accessKeyName, accessKey))`，而 `SecretVolMountPath = "/argo/secret"`。控制器**只把这份卷挂给 init / wait / supervisor 容器**（`workflowpod.go` 里逐个 `container.Name == common.WaitContainerName || common.SupervisorContainerName ... break`）——**用户的主容器默认拿不到 artifact 仓的凭据**（只有 `tmpl.Data` 这类模板例外）。

**这是 `prior-art` §5「凭据只交付给可信侧」在 K8s 原生世界里最完整的一次实现**，而且它与我们的形状**同构**：搬运者（init/wait 容器 ≈ 我们的 Worker）持凭据，被执行的代码（main 容器 ≈ 沙箱）没有。v4.0 起还有插件驱动：*"Artifact plugins run as regular sidecars rather than as init containers."*，通过 unix socket 通信（`/tmp/artifact-plugins/{plugin-name}/socket`，名字必须是 `socket`，[artifact-plugin](https://argo-workflows.readthedocs.io/en/latest/artifact-plugin/)）——**与我们「能力通道 = bind mount 进容器的 unix socket」是同一个机制**。

**GC / 保留**（[environment-variables](https://argo-workflows.readthedocs.io/en/latest/environment-variables/)）：`ArtifactGCStrategy` 三值 `OnWorkflowCompletion` / `OnWorkflowDeletion` / `Never`，**未设置时源码直接返回 `Never`**；失败重试窗口 `ARGO_ARTIFACT_GC_RETRY_WINDOW` 默认 **60m**；上传端点上限 `ARGO_SERVER_MAX_ARTIFACT_UPLOAD_BYTES` 默认 **1073741824**（413 溢出）。

⚠️ **一条直接打我们脸的警告**：该文档写明 **key 里的 `../` 是被显式允许的、可能导致路径穿越**（[artifacts walk-through](https://argo-workflows.readthedocs.io/en/latest/walk-through/artifacts/)）。**我们的中转区 key 由平台生成、绝不含用户输入**——这条要写进 `file-transfer-mechanics-and-path-safety.md` 的对照。

### 5.2 Tekton：Workspace 是「绑进来的目录」，Result 不是产物

**Workspace 由 Task 声明、由 TaskRun 绑定**（[workspaces](https://tekton.dev/docs/pipelines/workspaces/)）：Task 侧字段 `name` / `description` / `readOnly`（"Defaults to `false`"）/ `optional`（"Defaults to `false`"）/ `mountPath`（"A path to a location on disk where the workspace will be available to `Steps`"），默认挂载点 **`/workspace/<name>`**；Run 侧每条绑定**只能有一种 `VolumeSource`**（"You can only use a single type of `VolumeSource` per `Workspace` entry."），可选 `subPath`。⚠️ **没有名为 `workspaceBindings` 的字段**（我按这个名字找过，页面上不存在）。

⭐ **容器内寻址是一等的**：`$(workspaces.<name>.path)` / `.bound` / `.claim` / `.volume`，其中 `.bound` *"either `true` or `false`, specifies whether a workspace was bound. Always `true` if the workspace is required."* ——**「这个工作区到底绑上了没有」是一个可查询的事实，而不是靠路径存在性去猜**。必绑而未绑则 PipelineRun 直接失败。另有一条对我们的冷档有直接映射的细节：**读一个未绑定的 optional workspace，`.path` 展开成空字符串**。

**Result 是「任务交回来的东西」，且刻意做得很小**（[tasks](https://tekton.dev/docs/pipelines/tasks/)）：`results:` 声明 `name` / `type`（`string` / `array` / `object`），容器里写 `$(results.<name>.path)`；**上限由 Kubernetes 的 termination message 决定，原文 "At present, the limit is \"4096 bytes\"."**，超限报 `Termination message is above max allowed size 4096, caused by large task result`。并有两句对我们很重要的限定：

> *"Tekton does not perform any processing on the contents of results; they are emitted verbatim"*
> *"Tekton does not enforce Task results unless there is a consumer"*

⭐⭐ **这是对我们「没有声明的产物」最有力的反向确认**：Tekton 想表达「任务产出」时，只给了 4 KiB 的一个字符串，而且**对超过 1 KB 的建议是「改用 Workspace」**。`vendor-file-apis.md` §0 第 3 条说「没有一家沙箱厂商有『声明的产物』概念，唯一例外是 Buildkite 的 `artifact_paths`」——**现在可以补一句：Argo 有完整形态（声明 + 名字绑定 + GC 策略），Tekton 明确放弃了这个方向。两条路都有先例，我们选的是「不声明」，而 Tekton 站在我们这边。**

**Tekton Results 是独立组件、独立真源**（[results](https://tekton.dev/docs/results/)、[results/api](https://tekton.dev/docs/results/api/)）：*"separate out long term result storage away from the Pipeline controller"*，存 records（含 logs），组件是 *"A queryable gRPC API server backed by persistent storage"* 加 Watcher 与 retention agent；目的是让 *"the original CRD object can be safely removed from the cluster."* API 版本串只有 **`v1alpha2`**（`/apis/results.tekton.dev/v1alpha2/...`）。⚠️ **它不替代 CRD status，而是与之并存**——于是「一次执行的事实」有两个来源。**我们这个规模不要第二份事实**（`prior-art` §10 第 4 条）。

**⚠️ git 凭据：Tekton 的 `git-clone` 把凭据送进了执行容器。** 当前版本 0.10（标签上带 `tekton.dev/deprecated: "true"`），工作区契约是（[git-clone.yaml](https://github.com/tektoncd/catalog/blob/main/task/git-clone/0.10/git-clone.yaml)）：

- `ssh-directory`（可选）：*"A .ssh directory with private key, known_hosts, config, etc."*
- `basic-auth`（可选）：*"A Workspace containing a .gitconfig and .git-credentials file."*
- 步骤脚本把这些文件 `cp` 进 `$HOME` 再 `chmod 400` / `chmod 700`——**凭据是「以文件形式挂进执行容器，再由容器内的步骤脚本使用」**，而不是由平台在容器外代执行。

**这与 `ADR-0001`（凭据只交付给可信侧）和我们的「远端 git 动词由归属 Worker 在宿主侧代执行」正面冲突**，是本票在 git 一侧最强的反向证据。参数面（`url` / `revision` / `refspec` / `submodules` / `depth` 默认 `1` / `sparseCheckoutDirectories` / `deleteExisting` 默认 `true` / `subdirectory` / `sslVerify` / `httpProxy`…）**值得抄的是它的默认值选择**（浅克隆 + 先清空目标目录），不是它的凭据模型。

**网络策略：Tekton 一手文档里没有 step 容器的故事。** [operator 的 networkpolicy 页](https://tekton.dev/vault/operator-main/networkpolicy/)只管 Tekton 自己的组件负载（`pipeline-default-deny`），[taskruns 页](https://tekton.dev/docs/pipelines/taskruns/)通篇不提隔离或 egress。**唯一的信号是官方仓库里一个尚未关闭的 issue**（[tektoncd/pipeline#9603](https://github.com/tektoncd/pipeline/issues/9603)），它自己承认文档不存在，并说 *"egress should be restricted to prevent data exfiltration or access to internal services and cloud metadata."* —— **[未证实]：Tekton 没有关于 step 容器出网的官方答复。**

### 5.3 该抄 / 不该抄

**该抄**
- ⭐⭐ **Argo 的「凭据只挂给搬运容器，主容器没有」**（init/wait/supervisor 三个名字的 `break` 循环）。这是 `ADR-0001` 在容器世界里最贴近的同构实现，**可以逐字引用来论证我们的形状不是自创**。
- ⭐⭐ **Argo 插件的 unix socket 契约**（`/tmp/artifact-plugins/<name>/socket`，名字固定、协议是 gRPC、跑在 sidecar/init 里）——**与我们的能力通道同机制**，且它证明了「socket 的名字是契约的一部分」这件事有人认真对待过。
- ⭐ **Tekton 的 `$(workspaces.<name>.bound)`**：「这个工作区绑上了没有」是一个显式事实。映射到我们：**文件面与 Task 的第一个动作应该能问「工作区树在不在」，而不是靠 `ls` 一个目录去推断**。
- ⭐ **Argo 的 `optional` + `subPath` + `mode`/`recurseMode`** 这三个字段的分工（「可以没有」/「只取一个子路径」/「落盘权限」）是产物描述的最小完整集；**我们的 `download_file` 若要做目录或子路径，照着这三个名字设计不会走偏**。
- **Tekton 的 `readOnly` 与「一条绑定只能有一种 VolumeSource」**：把「这块存储是怎么来的」限制成一个枚举，避免「同一个工作区一半是 PVC 一半是 emptyDir」这种没人能推理的状态。
- **Argo 的 GC 默认 `Never`**：**默认不删**，要删得显式说。我们 `ADR-0007` 的中转区保留期是**默认删**——两者相反，但我们是对的（中转区是我们自己的无主前缀，不是用户的产物仓）；**差别要写清**。

**不该抄**
- ⚠️ **Argo 的 artifact key 允许 `../`**（文档明写可能导致穿越）。我们的 key 由平台拼、不接受用户输入。
- ⚠️ **Tekton 的 git 凭据进容器**（`basic-auth` / `ssh-directory`）。
- ⚠️ **Tekton Results 与 CRD status 两份事实并存**。
- **Tekton 的 `results-from: sidecar-logs` 逃生口**（beta，把结果写进 sidecar 的 stdout，绕开 4 KiB 上限）：多一条路径、多一套大小语义，**而我们本来就没有这个上限问题**。
- **Argo 的 `artifactRepository` 十一后端 + 插件驱动 + `artifactRepositoryRef` + key-only artifacts**：`vendor-file-apis.md` 的结论是「只有一个答案」，这里是「十一个答案加一套引用机制」。**我们只有一块平台存储。**
- **Tekton 的 Workspace VolumeSource 六选一**（`volumeClaimTemplate` / `persistentVolumeClaim` / `emptyDir` / `configMap` / `secret` / `projected` / `csi`）：把「工作区在哪」变成用户的选择，等于把我们的 `Runtime Provider` 职责漏进 API。

---

## 6. Anthropic 的 sandbox-runtime 与 Claude Code 的容器化沙箱

### 6.1 `anthropics/sandbox-runtime`：**它不是容器，而且明确说不用容器**

仓库存在，`anthropics/sandbox-runtime`（`anthropic-experimental/sandbox-runtime` 解析到同一个仓；Apache-2.0，约 5.5k stars，持续提交中）。自述：

> *"A lightweight sandboxing tool for enforcing filesystem and network restrictions on arbitrary processes at the OS level, **without requiring a container**."*
> *"The tool **avoids containers**, relying on namespaces, sandbox profiles, seccomp, and ACLs instead."*

（[README](https://raw.githubusercontent.com/anthropics/sandbox-runtime/main/README.md)）机制逐平台：macOS 用 `sandbox-exec` + 动态生成的 **Seatbelt** profile；Linux 用 **bubblewrap** 做容器化并配网络命名空间隔离；Windows 把进程跑在一个专用本地账户 `srt-sandbox` 下，用基于该账户 SID 的 Windows Filtering Platform 围出网边界。

- **网络：默认全拒、只允许名单。** *"By default, all network access is denied. You must explicitly allow domains. **An empty allowedDomains list means no network access.**"* 全部流量走**宿主上的代理**（HTTP/HTTPS 代理 + 一个 SOCKS5 处理其余 TCP）；Linux 侧最彻底：*"The network namespace of the sandboxed process is **removed entirely**, so all network traffic must go through the proxies running on the host."*
- **文件系统是非对称的**：读**默认全允许**、用拒绝名单收；写**默认全拒**、用允许名单开。*"By default, read access is allowed everywhere"* / *"By default, write access is denied everywhere."*
- **接口是「进程包装器」**，CLI 与库两条：`srt "curl example.com"`、`await SandboxManager.wrapWithSandbox('curl https://example.com')`；配置 `~/.srt-settings.json`；npm 的 `srt` 与 PyPI 的 `sandbox-runtime`（`srt-py`）。
- ⚠️ **它自己点名了两条绕过**：*"allowing access to `/var/run/docker.sock` effectively grants access to the host system through the Docker socket."*，以及 domain fronting（*"possible to bypass the network filtering through [domain fronting]"*）——**后者与 `sandbox-network-policy-apis.md` §0.3 的「各家自己写在文档里的缺口」是同一句**。
- ⚠️ **Docker 在这里是「削弱项」**：Linux 侧有一个 `enableWeakerNestedSandbox` 模式，用于 *"inside of Docker environments without privileged namespaces"*，文档说 *"This option considerably weakens security and should only be used in cases where additional isolation is otherwise enforced."*

### 6.2 Claude Code 的官方说法：容器是**外层**边界，不是沙箱本身

官方文档给了一张六行的对照表——"Sandboxed Bash tool" / "Sandbox runtime" / "Dev container" / "Custom container" / "Virtual machine" / "Cloud sessions"，并写明 *"The first two approaches in the table below run on the host operating system without containers. The rest place Claude Code inside a container or virtual machine."*（[sandbox-environments](https://code.claude.com/docs/en/sandbox-environments)）

- **谁造容器**：dev container 是**编辑器**造的，不是 Claude Code——*"A dev container runs as a Docker container ... An editor that supports the Dev Containers spec, such as VS Code, GitHub Codespaces, a JetBrains IDE, or Cursor, connects to that container"*（[devcontainer](https://code.claude.com/docs/en/devcontainer)）。**云端会话是 Anthropic 造的 VM**，且 *"A network proxy enforces a default allowlist, and a separate proxy holds your GitHub token **outside the sandbox** while issuing scoped credentials for repository access inside it."* ——**与我们 `ADR-0001` 同一条**。
- **网络**（[sandboxing](https://code.claude.com/docs/en/sandboxing)）：*"No direct route out. Connections go through a proxy on your machine that checks each host against your allowed domains, **which start empty**."*；*"The built-in proxy enforces the allowlist based on the requested hostname and, by default, **does not terminate or inspect TLS traffic**."*；以及一条与 `ADR-0003` 的 netns 规则集直接呼应的：**"Anything that isn't TCP: UDP, HTTP/3 over QUIC, and ICMP tools such as `ping` can't leave the sandbox"**。
- **文件系统**：写默认只有 *"the current working directory and its subdirectories ... plus the per-user temp directory that `$TMPDIR` points to"*；而读默认是 *"read access to the entire computer"* —— **与 sandbox-runtime 的 README 完全一致的非对称默认**。
- ⭐ **参考 devcontainer 的防火墙是一份可以直接读的 iptables 脚本**（[init-firewall.sh](https://raw.githubusercontent.com/anthropics/claude-code/main/.devcontainer/init-firewall.sh)）：`iptables -P INPUT/FORWARD/OUTPUT DROP`，再 `-A OUTPUT -j REJECT --reject-with icmp-admin-prohibited`；允许名单是 `registry.npmjs.org`、`api.anthropic.com`、`sentry.io`、`statsig.com`、`marketplace.visualstudio.com`、`vscode.blob.core.windows.net`、`update.code.visualstudio.com`，加上从 `https://api.github.com/meta` 用 `jq -r '(.web + .api + .git)[]'` 现场取的 GitHub CIDR、53/22 端口、loopback 与宿主 `/24`。容器侧为它加了 **`NET_ADMIN` + `NET_RAW`** 两个 capability（`runArgs`），`postStartCommand` 跑脚本，`remoteUser` 是 `node`（[devcontainer.json](https://raw.githubusercontent.com/anthropics/claude-code/main/.devcontainer/devcontainer.json)、[Dockerfile](https://raw.githubusercontent.com/anthropics/claude-code/main/.devcontainer/Dockerfile)，`FROM node:20`）。
  ⚠️ **`NET_ADMIN` 这一条与我们 `ADR-0003` 的承重条（「容器必须保持默认能力集」）正面冲突**：他们为了在容器里装规则**必须**给出 `CAP_NET_ADMIN`，并在文档里承认 *"The firewall script and these capabilities are not required for Claude Code itself"*。**这正是我们「规则由 Worker 在宿主侧 nsenter 装」那条时序选择所避开的税**——**记下来当作我们那个决定的一个正面理由。**
- **信任边界的三句原文**（都在文档里，不在容器文件里）：
  > *"This is a **convention rather than an enforcement boundary**, because Claude Code does not require a container."*
  > *"While the dev container provides substantial protections, no system is completely immune to all attacks. ... dev containers **do not prevent** a malicious project from exfiltrating anything accessible inside the container, including the Claude Code credentials stored in `~/.claude`."*
  > *"Only use this setting when **the outer container already provides the isolation boundary you need**."*
- 参考实现**不是维护中的镜像**：*"It is provided as a working example rather than a maintained base image."*

### 6.3 该抄 / 不该抄

**该抄**
- ⭐⭐ **「读默认全开 + 写默认全拒」的非对称文件系统默认**。我们的文件面只有「工作区树」一个命名空间，边界由路径决定，所以这条不直接适用；**但它是一句更普遍的话：沙箱的默认应该按「危害方向」定，不是按「方便方向」定。** 写比读危险——我们的 `download_file` 与 `upload_files` 应当有不对称的宽松度。
- ⭐ **「非 TCP 一律出不去」被两家都当成一等事实写出来**（Claude Code 文档与 `ADR-0003` 的规则集）。**文档里明说 UDP/QUIC/ICMP 出不去**，比含糊的「网络受限」诚实得多。
- ⭐ **代理**默认**不终止 TLS**（只按请求的 hostname 匹配）——这是一个低成本、可解释的取舍，比 MITM 那套（Vercel 的 per-sandbox CA）便宜得多。**`ADR-0003` 里被推迟的「正向代理 + 域名白名单」如果做，应该从这一档起步。**
- ⭐ **协议开放 / 能力封闭**：`sandbox-runtime` 是**库 + CLI**（`SandboxManager.wrapWithSandbox`），**不是一个服务、不开端口**。这是本票里唯一一个「沙箱侧没有任何监听物」的一手先例——**与我们的形状同侧**，值得在论证「沙箱零监听端口」时引用。
- **参考 devcontainer 的防火墙脚本本身**：一份 60 行左右、带「先 DROP 再按需开」的完整例子，将来给 `local` 后端或文档写等价物时可以直接对照。

**不该抄**
- ⚠️ **把容器当「外层边界」、沙箱当内层**（*"the outer container already provides the isolation boundary"*）。**我们的层级正好相反**：容器就是沙箱，没有内层。这句话对我们的意义是**反面的**——它说明在 Anthropic 的模型里容器是最后一道，而在我们这里容器是第一道也是唯一一道（`ADR-0003`：唯一硬假设是宿主与内核可信）。**不要因为这段话就以为「容器够了」；它说的是「容器是用来兜住内层的」。**
- ⚠️ **为了在容器里装防火墙而给 `CAP_NET_ADMIN`**（见上）。
- ⚠️ **`enableWeakerNestedSandbox`**：一个「在 Docker 里跑就削弱安全」的开关。我们的后端**本来就跑在容器里**，这条路对我们等于放弃。
- **`srt-sandbox` 专用本地账户 + Windows Filtering Platform 那一套**：Windows 专属，且我们的 `local` 后端只是参考实现。
- **把 devcontainer 当作「团队标准环境」的约定**（*"commit the example dev container to your repositories"*）：那是编辑器生态的用法，`prior-art` §2 已经就 devcontainer 作为 manifest 的硬边界写过了。

---

## 7. 横向对照

| | 对象模型（谁包含谁、谁更长寿） | 谁建容器 | exec 形态 | 文件面 | 默认网络 |
|---|---|---|---|---|---|
| **Stars Sandbox**（我们） | Worker（稳定身份，跨重连）⊃ Workspace（比 Sandbox 长寿）⊃ Task（串行） | **归属 Worker**（沙箱机器上），控制层只决定「在哪」 | Task 多路复用在那**一条** Worker→控制层 BIDI 流上 | 按路径 + 对象存储预签名：`ls_files`/`download_file`/`upload_files`/`complete_upload`，元数据走流、字节走对象存储 | `egress: public`（公网可达，私有段/loopback/link-local/metadata/宿主全拒）；能力通道一条 UDS |
| **Agent Sandbox** | Sandbox（单个 stateful pod，稳定 hostname）← SandboxClaim ← WarmPool ← SandboxTemplate | K8s 控制器（从模板或预热池） | 沙箱内 `sandboxd`：gRPC `ProcessService` :9090（`Start` 服务端流）；外面经 router（`X-Sandbox-ID`）或 Service DNS 进入 | 同一守护进程的 REST `FilesystemService` :8080，`/v1/files/{path}` 原始字节，两个**独立协议端口** | `NetworkPolicySpec` 空列表 = ingress 与 egress **双 Default Deny** |
| **Docker Sandboxes** | sandbox（稳定名）+ 宿主目录 = 工作区；另有两套持久化（template / cloud volume） | 本地 `sandboxd`（宿主守护进程）；云端 Docker Cloud Sandboxes API | `sbx exec`（**容器内是 microVM，自带 docker daemon**）；不支持 detached | `sbx cp SANDBOX:PATH`（`docker cp` 语义，无列表、无完成信号）；云端走 Cloud file API | 未初始化前无默认；初始化后白名单制（`allow-all`/`balanced`/`deny-all`），**宿主侧代理** |
| **OpenHands** | `Sandbox ⊃ Conversation`；`SandboxRecord`（持久身份） vs `SandboxInfo`（活体状态）分型 | **控制层**（持 docker client 的进程，或 remote Runtime API 的 `POST /start`） | 容器内一个 HTTP 服务器（V0 端口取 30000–39999 且宿主端口 = 容器端口；V1 固定 8000），`X-Session-API-Key` | **两个面**：读/写/编辑是 action、走命令通道；列表 + 批量搬运走 `/api/file/*` | 未声明；可选 `network_mode='host'` |
| **Codespaces / DevPod / Ona** | Codespaces：仓库 → codespace ⊃（容器 + 整块盘）；DevPod：workspace + provider + agent；Ona：environment（= 工作区）+ environment class + runner，**无独立 workspace 对象** | Codespaces = **GitHub 控制面**（`POST /user/codespaces`）；DevPod = **用户自己的客户端**（`devpod up`，无服务端）；Ona = **runner 造算力、管理面协调** | Codespaces：`gh codespace ssh` / `exec` / REST；DevPod：`devpod ssh`（经 provider tunnel，客户端→工作区）；Ona：`ona environment` CLI + REST `CreateEnvironment` | Codespaces：`/workspaces`（宿主目录 bind mount）；DevPod：**把源目录 sync 进去**（不是挂载）；Ona：`/workspaces/<repo>` + `/home/gitpod` | Codespaces / Ona 的端口转发都要求容器内**监听 `0.0.0.0`**；Ona 的 runner→管理面是出站 HTTPS |
| **Argo / Tekton** | Argo：artifact 由 step 声明、调用方按名字绑定；Tekton：Workspace 由 Task 声明 / TaskRun 绑定，Result 是另一个对象 | K8s | step 就是容器进程本身；搬运由 `argoexec` 在 init/wait 容器里做 | Argo：对象存储 artifact（**凭据只挂 init/wait 容器**）；Tekton：挂进来的卷 + 4 KiB 的 Result | Argo 未文档化；**Tekton 无 step 容器策略文档 [未证实]** |
| **Anthropic `sandbox-runtime`** | 没有对象模型：一个被包装的进程 + 一份 `~/.srt-settings.json` | **不建容器**（"without requiring a container"） | 进程包装器：`srt "<命令>"` / `SandboxManager.wrapWithSandbox` | 路径级 ACL：读**默认全开**、写**默认全拒** | `allowedDomains` 默认**空 = 无网**；宿主上 HTTP 代理 + SOCKS5；Linux 侧进程的 netns 被整个移除 |
| **Claude Code devcontainer** | 容器由**编辑器**（VS Code / Codespaces / JetBrains / Cursor）造，不是 Claude Code | 编辑器 | 容器内正常进程；沙箱由 bubblewrap/Seatbelt 那层负责 | 写默认 = cwd + `$TMPDIR`；读默认 = 整机 | `init-firewall.sh`：`iptables -P OUTPUT DROP` + 按名字/CIDR 开洞；容器需 `NET_ADMIN`+`NET_RAW` |

**读表要点**：**「谁建容器」一列里只有我们、OpenHands 与 Ona 是「控制层裁决、执行侧落地」**——而且**只有我们是执行侧主动拨号的**。Ona 的 runner 也自己拨向管理面（出站 HTTPS，§4.3），是本票里离我们最近的一个；其余每一家的 exec 形态都依赖控制层能连进沙箱（K8s Service、router、宿主端口、编辑器、provider tunnel）。**「文件面」一列里只有我们和 Agent Sandbox 是纯按路径的独立面。**

---



## 8. 对 Stars Sandbox 的推论

1. **「Runtime Provider」这个词在这批材料里对应两件不同的东西，别混。** 我们的 `Runtime Provider`（Go interface，`Create`/`Exec`/`Inspect`/`Stop`/`Destroy` + `Capabilities()`）跑在 **Worker 进程内**，对应的是 OpenHands remote API 的 `/start` 请求体所表达的那件事——**「在哪里跑、以谁的身份跑、跑什么镜像命令」由调用方裁决，执行侧只负责落地或拒绝**。而 Agent Sandbox 的 `sandboxd` / OpenHands 的 agent server 是**沙箱内的第二个守护进程**，对应的是我们**刻意不做**的那一层。**两份材料里形状最像我们的，恰恰是那个 HTTP `/start` 契约，而不是那个 proto。** 读的时候要把这两层分开。
2. **`sandboxd` 的 proto 值得当作我们 Task 消息的对照检查表**：`Start`（服务端流，`oneof{InitEvent, stdout, stderr, ExitEvent}`）+ `Execute`（一元同步）+ `WriteStdin` + `SendSignal(SIGINT/SIGTERM/SIGKILL)` + `ResizeTTY`，配置是 `{command[], env_vars{}, cwd}`。**我们已经有 `command` / `cwd` / `env` / `timeout` → exit code + 日志流**；差的是：**一个「短命令不必开流」的 `Execute` 形态**、**一个显式的 `InitEvent`（命令真的起来了，才从 `assigned` 变 `running`）**、以及 **`exit_code` 用 `128+n` 表示被信号杀死**（*"as a shell reports it"*）这条约定。这三条都是便宜的、且事后难补的。
3. ⭐ **「文件面与命令面是两个面」在本票里得到三次独立确认**（`sandboxd` 按协议切并写死了理由、OpenHands 按「读写在命令通道上 / 批量搬运独立」切、Tekton 的 Result 与 Workspace 本来就是两个对象），**没有一家合成一个面**。我们的 `ADR-0006` 站在多数派，且理由比他们都强：**字节走对象存储 + 预签名，命令走流。**
4. ⭐ **Agent Sandbox 的 `/v1/files` 有一份现成的、可以直接对着实现的语义清单**，应当**逐条**过一遍我们的文件面：`GET`/`HEAD`/`PUT`/`DELETE` 四个方法；**原子写（temp + rename）**；自动建父目录；`mode` 参数带正则校验；非空目录 `DELETE` 回 **409**；越界回 **403**（不是 404）；`symlink-aware sanitization`；以及**四组并发竞态各自的后果**与「无跨请求事务、无 CAS，协调责任在调用方」的明确声明。**我们 `ADR-0006`/`ADR-0008` 现在缺的正是这一段。**
5. ⭐⭐ **Argo 那句「凭据只挂给搬运容器（init/wait/supervisor），主容器没有」是 `ADR-0001` 最贴近的一手论证**，而且它与我们的形状**同构**：搬运者持凭据、被执行的代码没有。**下次有人问「为什么凭据不给沙箱」，这两句（KEP 级的 `explicitly *not* delete` 那句 + Argo 的 `SecretVolMountPath` 那句）就是答案，不必再论证。**
6. ⭐ **Tekton 的 `$(workspaces.<name>.bound)` 提醒我们补一个显式事实**：`bound`（"whether a workspace was bound"）是**可查询的一等值**，不是靠路径存在性推断。**我们的文件面与 Task 应当能直接问「这个 Workspace 的树在不在」，而不是靠 `ls` 一个目录去猜**——冷档与热档的差别应该是记录里的事实。
7. **OpenHands 的 `SandboxRecord` / `SandboxInfo` 类型二分，正是我们「控制层的视图永远可能陈旧」那句话的正确落点。** `SandboxRecord` 只存控制层自己有的（id、owner），`SandboxInfo` 才带活体状态与 URL。等于承认：**存下来的那一半与活着的另一半永远可能是两回事**，所以不要让它们共享一个类型。
8. ⚠️ **Claude Code 的参考 devcontainer 为了在容器里装防火墙，必须给容器 `NET_ADMIN` + `NET_RAW`**（文档自己承认 *"The firewall script and these capabilities are not required for Claude Code itself"*）。**这是 `ADR-0003` 那条承重约束（「容器必须保持默认能力集，有 `CAP_NET_ADMIN` 就能自己改路由」）的反面对照**，也是我们选「Worker 在宿主侧 `nsenter` 装规则」而不是「容器里装防火墙」的正面理由。**补进 ADR-0003 的 Consequences。**
9. **「声明的产物」现在有完整的两端先例：Argo 一路走到底（step 声明 `outputs.artifacts`、调用方按名字绑定、`artifactGC` 三值默认 `Never`），Tekton 明确放弃（Result 上限 4 KiB，超过 1 KB 就劝你改用 Workspace）。** 我们选「不声明」不是孤例——**`vendor-file-apis.md` §0 第 3 条那句「唯一例外是 Buildkite」要补一句：Argo 是完整形态的另一极。**
10. **保留与终态：Agent Sandbox 的 `shutdownTime` + `shutdownPolicy`（`Delete`/`Retain`，默认 `Retain`）与我们 `ADR-0007` 的保留期是同一种旋钮**，差别是**它的默认是「留着」、我们的中转区默认是「删掉」**——因为中转区是平台自有的无主前缀，不是用户的产物仓。**这个差别要在两份文档里都写出来，否则读者会以为我们抄错了默认值。**
11. **三家「容器里住着一个 HTTP 服务器 + 一把密钥」的实现（`sandboxd` 的 `--auth-token-sha256`、OpenHands 的 `X-Session-API-Key`/`OH_SESSION_API_KEYS_0`、Agent Sandbox 的 router `X-Sandbox-ID`）是同一个形状**，它们的共同前提是「控制层拥有到沙箱的网络路径」。**我们放弃这个前提换来了两件事：沙箱可以在 NAT 后面；沙箱里没有任何可被认证的东西。** 代价要诚实记下：**我们的沙箱跑不了任何需要被人访问的服务**（`sbx ports`、`PROXY_TIMEOUT_SECONDS`、`WEBSOCKET_*` 那一整套在我们这里无对应物）。**这是一个真正的能力缺口，不是纯粹的优势。**

### ⚠️ 与既定形状相抵触的先例（本票汇总）

| # | 我们的既定形状 | 先例说什么 | 影响 |
|---|---|---|---|
| 1 | 沙箱**零监听端口、不可被连入** | Agent Sandbox 的沙箱**必须有**一个监听端口（默认运行时非 8888；`sandboxd` 默认 `--listen-host 0.0.0.0`），外面靠 router/PortForward 进去；OpenHands 的容器住着一个 `0.0.0.0` 上的 HTTP 服务器 | **不改变决定，但要知道代价**：我们换来了 NAT 后可部署与「沙箱里没有可认证物」，代价是**沙箱内的服务不可达**。这条要写进能力面文档，别让调用方以为是自己配错了 |
| 2 | 沙箱**唯一认证主体是主动拨号的 Worker** | 三家都在沙箱里放了一个**校验密钥的进程**，密钥还是创建时写进容器环境的（`OH_SESSION_API_KEYS_0`） | **不抄**，但与 `ADR-0001` 的论证一致：凭据不进不可信侧，沙箱内也没有可验的东西 |
| 3 | **派发要求活流，绝不隐式拉起** | `sbx exec` 原文 *"If the sandbox is stopped, it is started first."*；`sbx ports publish` 同理 | **不抄**。他们的前提是「沙箱就在本机隔壁」，我们是跨机器、Workspace 只能回归属 Worker。理由要写清是前提差异 |
| 4 | `egress: public`（全空间减私有段） | Agent Sandbox 的 `NetworkPolicySpec` 空列表 = ingress/egress **双 Default Deny**；Docker Sandboxes 初始化后是**白名单制**，并有一个推荐的 `balanced` 档 | **不改变决定**（我们的调用方要装依赖、且我们明确否决过按域名匹配），**但 `balanced` 这个「够用的中间档」的命名与定位值得借**；`ADR-0003` 的 Considered Options 应记入这两个新默认值 |
| 5 | 文件面**四动词**、与 Task 无关 | Docker Sandboxes 只有 `sbx cp` 一个动词（`docker cp` 形状，无列目录、无完成信号、无元数据） | **不抄**，我们的四动词是被「元数据走流、字节走对象存储」逼出来的，比 `cp` 多出的正是可观测性 |
| 6 | 中转区**默认删除**（保留期） | Argo 的 `ArtifactGCStrategy` 未设置时源码返回 **`Never`**（默认不删） | **不抄**。我们的中转区是平台自有存储的无主前缀，不是用户产物仓——**差别要写出来** |
| 7 | 「**唯一一套持久化答案**」（只剩宿主目录） | Docker Sandboxes 同时有宿主工作区（direct/clone/mountless）+ 云端 volume + template 快照；Argo 有十一后端 + 插件驱动 | **不抄**，这是 `prior-art` §8 第 5 条的又一次确认 |
| 8 | **凭据只交付给可信侧**（`ADR-0001`），远端 git 动词由 Worker 在宿主侧代执行 | DevPod *"forward git credentials to a remote machine"*；Tekton 的 `git-clone` 用 `basic-auth` / `ssh-directory` **工作区**把凭据送进 step 容器；OpenHands 把 `X-Session-API-Key` 写进容器环境；`sandboxd` 在沙箱内校验 bearer token | **不抄**，四家四个形式同一个取舍。**唯一站在我们这边的是 Argo**（凭据只挂 init/wait/supervisor，主容器没有，§5.1）——`ADR-0001` 的论证因此有了正反两面的一手材料 |

---

## 9. 未能证实 / 未覆盖

**本节的写法**：每条都写明「查了哪里」。凡本文件写成 **[未证实]** 的，都表示**没有一手来源**，不是「我记得是别的」。

### 9.1 [未证实]

- **Tekton 的 step 容器 / TaskRun pod 的出网与隔离，官方没有任何文档。** 查了 [workspaces](https://tekton.dev/docs/pipelines/workspaces/)、[taskruns](https://tekton.dev/docs/pipelines/taskruns/)、[operator networkpolicy](https://tekton.dev/vault/operator-main/networkpolicy/)（后者只覆盖 Tekton 自己的组件负载）。唯一的信号是官方仓库里**仍开着的** [tektoncd/pipeline#9603](https://github.com/tektoncd/pipeline/issues/9603)，它自己说没有这份文档，并称 *"egress should be restricted to prevent data exfiltration or access to internal services and cloud metadata."*
- **Argo 没有 `ARGO_ARTIFACT_DRIVER` 这个环境变量。** 在 environment-variables、artifact-plugin、configure-artifact-repository、artifact-repository-ref 四页与全仓代码检索里都是 0 命中。**驱动选择只由 ConfigMap 里的 `artifactDrivers` 列表（name + image）完成。**
- **Argo 的字段参考页没读完。** `https://argo-workflows.readthedocs.io/en/latest/fields/` 有 653,829 字符，只读了 0–100k 与 250k–350k；`Artifact` / `ArtifactLocation` 的字段释义**取自生成该页的 Go 源码**（同一个仓），不是页面正文。
- **Argo：调用方传的 `arguments.artifacts` 条目能否自带 `path`**（与模板里 `inputs.artifacts[].path` 谁赢）——两页文档都没有一句话说清，结构体是共享的所以语法上允许。
- **OpenHands 的 remote Runtime API（`/start`、`/pause`、`/list`…）没有公开的 HTTP 规格或 OpenAPI。** `sandbox-server` 仓的目录树里没有 spec 文件，也没有在官方文档站上找到发布版。
- **OpenHands：`docs.openhands.dev/usage/architecture/runtime` 这个页面是否存在**——V0 的 README 链向它，但本轮的调研分支没有打开该 URL，因此不作任何断言。
- **OpenHands 是否给沙箱容器配了 seccomp profile**——在 `sandbox-server` 树里没找到；**「没找到文件」不等于「不存在」**。
- **Anthropic：`sandbox-runtime` 是「正式产品」还是研究预览。** Claude Code 文档称它是 *"a beta research preview, and its configuration format may change as the package evolves."*（[sandbox-environments](https://code.claude.com/docs/en/sandbox-environments)），而仓库本身在被读到的段落里**没有**这句话。
- **Anthropic：`sandbox-runtime` 是否有正式威胁模型文档**——仓里有 `docs/` 目录，但本轮没有打开其中任何文件。
- **Anthropic：`anthropics/claude-code` 是否提供维护中的镜像**——文档**明确否认**：*"It is provided as a working example rather than a maintained base image."*
- **Anthropic：devcontainer 文档里没有任何关于挂载 docker socket、seccomp profile 或 `--privileged` 的表述**（只提 `NET_ADMIN` 与 `NET_RAW`）。
- **Docker Sandboxes 云端 API 的端点与 OpenAPI spec 没拿到。** `https://docs.docker.com/ai/sandboxes-api/` 只说存在「可下载的 OpenAPI specification」并链到 API 参考，**该页没有给出基址、端点路径、HTTP 动词或 SDK 包名**。
- **Agent Sandbox：「resume exactly where they left off」与 KEP 的落差未调和。** 官方博客写的是 *"resume exactly where they left off"*，而 KEP-694 写的只是 *"The new Pod is seamlessly bound to the existing PVCs"*，且明说控制器会删 Pod、不删 PVC 与 Service。**进程与内存的存续在两份一手材料里说法不同，本轮不裁决。**
- **Codespaces 容器内是否有 agent、以及连接方向。** 查了 overview、deep-dive、security-in-github-codespaces、troubleshooting-your-connection、forwarding-ports、using-github-codespaces-with-github-cli，**没有一页**点名容器内的 agent 或说明连接方向。唯一的方向性证据是一个微软托管的连接服务（探针 URL `https://global.rel.tunnels.api.visualstudio.com/api/version`，文档建议把 `*.visualstudio.com` 加进防火墙白名单）加上 *"Codespaces are allowed to make outbound connections to the internet."*——**推断方向是 client → relay ← codespace，不是 client → codespace 直连，但没有一手页面这么写。** 另：Codespaces **没有** REST rebuild 端点（rebuild 只有 CLI/编辑器有）。
- **DevPod：容器内的 `/workspaces` 路径在 devpod.sh 文档里不存在**，本轮是从官方仓库源码读出的（`cmd/ssh.go` 与仓库自带的测试夹具）；`devpod exec` 的文档页本轮未打开。**工作区在容器内到底是 docker volume 还是 bind mount，devpod.sh 没有说。**
- **Ona：改名的确切日期与官方公告文本**——只拿到 README 首句 + 两次 308 跳转 + changelog 佐证，**没有**从一手页面拿到带日期的改名公告。**今天的 Ona 端口 URL 用哪个域名**也是 [未证实]（当前页面只写「Ona creates a URL with automatic TLS termination」）。**start/stop/delete/list environment 的确切 verb+path 也 [未证实]**——只有 `CreateEnvironment` 的路径出现在本轮打开过的页面上。
- **Ona：管理面是否会反向连进 runner、以及是否存在 per-environment 的容器内 agent**——查了 runners/overview、aws/overview、aws/setup、aws/networking、enterprise-runner/overview、environments/overview、persistent-storage、dual-disk，均未找到。**runner 拨向管理面出站这一侧有原文（见 §4.3），反向没有。**

### 9.2 本文件**未覆盖**的

- **devcontainer 规范本身**、**K8s 调度/设备模型**、**CI runner 的注册信任与断线语义**——三者已由 `prior-art-sandbox-orchestration.md` §2 / §3 / §4 覆盖，本票按约定不重复。
- **microVM 优先的那批**（E2B / Modal / Fly / Vercel / AWS / Blaxel / Runloop / Northflank / Daytona）——同上。
- **`sandboxd` 之外，Agent Sandbox 是否还有别的运行时实现**（KEP-539.2 提到旧的 `python-runtime` 仍是默认当 `sandboxd` 未被选中时），本轮**没有**去读 `python-runtime` 的接口。
- **Docker Sandboxes 的本地 `sandboxd` 的对外 API**（不是 `sbx` CLI）——CLI 参考里只说云端走 Cloud file API，本地那侧的进程边界本轮没有验证。

### 9.3 取证方式

- **本人直接打开**：Agent Sandbox（官网 docs、K8s 官方博客、Google Open Source Blog、仓库源文件与 KEP，经 WebFetch 与 `gh api` 读 GitHub 官方仓的原始文件）、Docker Sandboxes（`docs.docker.com` 各页、`docker/docs` 仓的 `data/sbx_cli/*.yaml`）、Argo / Tekton（见 §5，各推断页与源文件）。
- **本轮派出的两个调研分支用同一套 WebFetch 纪律打开、并在报告里带回原文引用与 URL**：§3 OpenHands + §6 Anthropic（一个分支）、§4 Codespaces / DevPod / Gitpod(Ona)（另一个分支）。这两部分的每条引文都由分支标明了一手 URL；其中 OpenHands 的 V0 引文钉在**删除前一个 commit** 上，Codespaces 的引文有几条是长度截断的原文片段（约 125 字符），**按片段引用而不是整句**——这一点在分支报告里已声明。
- 引文的英文原文一律保留；本文档只做翻译与归纳，不改写引文。
- **行数说明**：本份超出了 `#4` 给的 150–350 行目标（约 590 行）。理由是它必须覆盖**六个互不相干的产品族**且每族都要「怎么做的 / 该抄 / 不该抄」，而减去任何一段都意味着删掉一条有一手来源的结论。**同批的 `sandbox-git-integration.md` 是 431 行、`sandbox-network-policy-apis.md` 是 354 行**——本份按同一密度写，长度差异来自产品数量而不是段落更长。如果需要压缩，最该砍的是 §1.3 的接口细节表与各节的「该抄/不该抄」（它们复述了上文已给的事实）。
