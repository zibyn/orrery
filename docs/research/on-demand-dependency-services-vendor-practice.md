# 沙箱里的依赖服务（PostgreSQL / Redis）：创建时声明 vs 运行中按需拉起

调研日期 **2026-10-31**。问题：可比产品如何让沙箱 / 开发环境里的代码使用 PostgreSQL、Redis 这类依赖服务，**尤其是沙箱已经在跑、代码（例如调试中的 AI agent）中途才决定要一个 Postgres** 的情形。只用一手来源（官方文档、API 参考、规范、源码、厂商自己的工程博客）；厂商原文保留英文。源码引用钉在读取时的 commit 上；文档引用给 URL，必要时给读取到的 markdown 行号。

已有调研里能复用的事实直接引用，不重新取证：

- E2B 一沙箱一 Firecracker microVM：`prior-art-sandbox-orchestration.md` §1.1
- Modal 沙箱 runtime 可选 gVisor / VM、GPU 只能 gVisor：`prior-art-sandbox-orchestration.md` §1.3（本文在 §3.3 补了 runtime 章节的最新原文）
- Daytona 非 GPU 容器默认 `Privileged: true`（`container_configs.go`）、link 网络的 ICC 规则按地址匹配可被伪造（`link_icc.go#L20-L26`）：`daytona-sandbox-runtime-semantics.md` §0 第 8 条、`recheck-identity-network-channel-vs-coder-daytona.md` N3
- Coder 的 DERP + WireGuard + STUN 栈、「工作区与控制层之间不能有 NAT」：`prior-art-sandbox-orchestration.md` §1.10
- 各家出网策略字段、Modal 运行中改出网策略、Fly 6PN 与 network policy 的关系 [未证实]：`sandbox-network-policy-apis.md`
- devcontainer 规范「没有资源配额、没有调度」、Compose 的资源字段：`prior-art-sandbox-orchestration.md` §2
- 我们把 privileged / Docker-in-Sandbox 判为「排除」：`system-feature-gap-vs-coder-daytona.md` §0 第 6 条与 §1 对照表
- OpenHands V1 拆仓、`runtime_class == 'sysbox'` 时加 `sysbox-runc`：`container-sandbox-architecture.md` §3

本文新增的一手取证：Modal Sandbox Sidecars、Modal i6pn（含 Sandbox 的 `i6pn=True` 源码）、Modal VM runtime 跑 Docker、Daytona Linked sandboxes 与 Docker-in-Docker 快照、E2B Docker 模板与公网 URL、Coder「Docker in workspaces」四种方法与 Dev Containers 集成、Coder 网络文档的直连 / DERP 条件、Tailscale `derp` 包源码、devcontainer 规范 Compose 字段与 `docker-in-docker` / `docker-outside-of-docker` Feature、Codespaces 主机 VM、Ona tasks & services（含运行时 `ona environment config apply`）、OpenHands sandbox-server 的 `runtime_class`、Testcontainers（Java 文档、Ryuk）与 Testcontainers Cloud、gVisor「Docker in gVisor」、Sysbox、Fly.io 6PN / `.internal` / Machines API、Railway 私有网络、Northflank addon 与项目内网、Kubernetes ephemeral container。

---

## 0. 一句话结论

**「沙箱跑起来以后再要一个 Postgres」在业界只有两种落地形状：要么在沙箱自己的内核/VM 里跑 Docker（E2B、Daytona、Modal VM runtime、Coder+Sysbox、Codespaces、Ona，都靠 microVM / 整机 VM / 特权或 Sysbox 换来一个能跑 `dockerd` 的内核），要么由沙箱之外的 SDK 在同一台宿主上起一个「兄弟容器」并用宿主内网 + 名字解析接上（Modal Sandbox Sidecars、Daytona Linked sandboxes）；跨主机覆盖网（Fly 6PN、Modal i6pn、Railway、Tailscale）只用于长期服务互联，没有一家把它当作「沙箱中途起依赖」的主路径，而由沙箱内代码直接触发兄弟服务的，一家都没有——Modal 文档明写 sidecar「is created through the Modal SDK, not from inside the Sandbox」。**

---

## 1. 对照表

列含义：**何时**＝创建时声明 / 运行中按需；**谁触发**＝沙箱内代码 / 外部 SDK·API / 配置文件；**网络形状**＝共享 netns·localhost / 同宿主网桥 + 名字 / 跨主机覆盖网 / 经服务器中继；**生命周期·计量**＝是否随沙箱结束、资源算在谁头上；**隔离**＝在 gVisor / microVM / VM 下怎么成立。

| 产品 | 何时 | 谁触发 | 网络形状 | 生命周期 · 计量 | 隔离前提 |
|---|---|---|---|---|---|
| **E2B** | 运行中按需（`docker run`），Docker 本身在模板构建时装 | 沙箱内代码 | 沙箱 VM 内部（Docker 网桥 / 端口在 VM 内）；文档未写容器网络细节 | 随沙箱；吃沙箱的 CPU/内存（「建议 ≥2 CPU、2 GB」） | Firecracker microVM，有自己的内核（§1.1 既有） |
| **Daytona**（DinD 快照） | 运行中按需 | 沙箱内代码 | 沙箱内部 | 随沙箱；吃沙箱资源（「建议 ≥2 vCPU、4GiB」） | 容器沙箱默认 `Privileged: true`（既有） |
| **Daytona**（Linked sandboxes） | 父沙箱创建后再建子沙箱 | 外部 SDK/API（`linked_sandbox=parent.id`） | **同一 runner** 上的 link 网络，名字/ID 作 DNS 别名，双向可达 | 子沙箱强制 ephemeral，停即删；删父级联删子；计量文档未写 | 每个子沙箱是独立沙箱 |
| **Modal**（Sandbox Sidecars，beta） | **运行中按需创建 / 终止 / 替换** | **外部 SDK**；明写不能从沙箱内建 | 同宿主内部网桥，`/etc/hosts` 名字（`main`、sidecar 名）；容器间流量不受出网 allowlist 约束 | 沙箱终止即全停；**共享沙箱的 CPU/内存**，每沙箱上限 250 个 | 每个 sidecar 与主容器隔离边界等同独立沙箱（gVisor 或 VM） |
| **Modal**（VM runtime + Docker） | 运行中按需 | 沙箱内代码（`sb.exec("docker","run",…)`） | 沙箱 VM 内 Docker 网桥 | 随沙箱；`/var/lib/docker` 进文件系统快照 | **只在 VM runtime**；文档把 Docker 列为选 VM 的理由 |
| **Modal**（i6pn） | 创建时 `i6pn=True` | 外部 SDK | 跨容器 IPv6 私网，**限同 region、同 workspace**；地址需自己交换 | 各自独立 | — |
| **Coder** | 模板声明（sidecar / Sysbox 运行时）；有 Docker 后运行中 `docker run` | 模板（Terraform）+ 工作区内代码 | 视方法：Sysbox 内部 / Pod 内 `localhost:2375` / Docker 网络 | 随工作区；模板决定 | Sysbox（非特权）、Envbox（外层特权）、rootless Podman、特权 sidecar（「可拿到宿主 root」） |
| **Codespaces / devcontainer** | Compose `runServices` 创建时声明；有 DinD Feature 时运行中 `docker run` | 配置文件；工作区内代码 | Compose 网络或 `network_mode: service:db`（共享 netns） | 默认用户断开时停全部 compose 容器；吃那台专属 VM | 每个 codespace 一台**专属 VM**；DinD Feature `"privileged": true` |
| **Ona（原 Gitpod）** | `postDevcontainerStart` 或 **`manual`**；可在运行中用 CLI 生成新 service | 配置文件 / **环境内 CLI** / 环境外 CLI | 环境 VM 内（service 通常就是 `docker run postgres`） | 随环境；吃环境 VM | runner 为每个环境建**隔离 VM** |
| **OpenHands** | 运行中按需（`docker run`，前提是 runtime 支持） | 沙箱内代码 | 沙箱内部 | 随沙箱 | 远端默认 `gvisor`；字段注释说 `sysbox`「support docker inside runtime」 |
| **Testcontainers（本地）** | 测试运行中按需 | 测试进程经 Docker API | 宿主端口映射，`getHost()`+`getMappedPort()` | Ryuk 按会话标签回收（Ryuk 需特权） | 无；依赖可达的 Docker daemon |
| **Testcontainers Cloud** | 测试运行中按需 | 测试进程经 Docker API → agent | **agent 开 SSH 隧道**到云端 Docker daemon | 测试结束后等一会儿自动释放；每会话 8G，不可调 | 云端 worker VM（约 30 分钟闲置删除） |
| **Docker-in-Docker / Sysbox / gVisor** | 运行中 | 容器内代码 | 嵌套 Docker 网桥；gVisor 内需 `--iptables=false`，端口暴露要 `--network=host` | 随外层容器 | DinD 要特权；Sysbox 非特权但隔离弱于 gVisor/VM；gVisor 要 `--net-raw` 等 |
| **Fly.io Machines** | 任意时刻经 Machines API 建 Machine | 外部 API（也可在私网内走 `_api.internal:4280`） | **跨主机 WireGuard 6PN**，`<app>.internal` AAAA | Machine 各自独立计费；DNS 只返回已启动的 | Firecracker VM |
| **Railway** | 任意时刻加 service | 控制台 / API | WireGuard 私网，`SERVICE_NAME.railway.internal`，每环境隔离 | 各自独立 | 文档未写（本文未查） |
| **Northflank** | 任意时刻经 API 建 addon | 外部 API | 项目内私网（internal 端口） | addon 独立计费与备份 | CPU 沙箱 microVM，GPU 沙箱 gVisor |
| **Kubernetes Pod** | sidecar 只能在 Pod 创建时声明 | 配置 | Pod 共享 netns，`localhost` | 随 Pod；资源在 Pod spec 固定 | ephemeral container **不能有端口、不能设资源**，不适合做服务 |
| **Tailscale**（网络底座） | — | — | 先 DERP 中继，能打洞就升直连 | — | 两端都在「hard NAT」后时只能中继 |

---

## 2. 两条轴先说清

1. **「谁有内核」决定能不能在沙箱里跑 Docker。** 能做 Docker-in-sandbox 的，要么一沙箱一 VM（E2B Firecracker、Modal VM runtime、Codespaces 专属 VM、Ona 隔离 VM），要么容器层放权（Daytona 特权容器、Coder 的 Sysbox / Envbox / 特权 sidecar、devcontainer DinD Feature 的 `privileged`）。gVisor 是第三条路：能跑，但要额外 runsc 标志（§5.2）。
2. **「谁有资格起兄弟服务」决定按需服务是否经过平台。** 两个真正把「沙箱之外的兄弟服务」做成平台原语的厂商（Modal Sidecars、Daytona Linked sandboxes）都只把这个动作给**外部 SDK 持有者**，并把兄弟放在**同一宿主 / 同一 runner**上，用宿主内网和名字解析接起来。

---

## 3. 分产品

### 3.1 E2B

来源：<https://docs.e2b.dev/template/examples/docker.md>、<https://docs.e2b.dev/network/public-url.md>

- Docker 是**模板构建期**装进去的：模板 `.runCmd('curl -fsSL https://get.docker.com | sudo sh')`，然后 `.runCmd('sudo docker run --rm hello-world')` 验证；运行时由调用方经 `sbx.commands.run('sudo docker run --rm alpine echo "Hello from Alpine!"')` 起容器。同页还有 Compose 变体："This example installs Docker and Docker Compose, then validates the setup."
- 资源算在沙箱头上："We recommend at least 2 CPUs and 2 GB of RAM for running Docker containers. **With lower RAM, your sandbox might run out of memory.**"
- 页面**没写**内层容器的网络形状、是否特权；隔离边界是 Firecracker microVM（`prior-art-sandbox-orchestration.md` §1.1），所以沙箱里的 `dockerd` 跑在沙箱自己的内核上。
- 沙箱之间没有私网原语。对外只有公网 URL："Every sandbox has a public URL that can be used to access running services inside it."；且 "By default, a sandbox's public URLs are reachable by anyone who knows them."——若要让 A 沙箱用 B 沙箱里的 Postgres，唯一文档化的路是 B 的公网 URL（且 E2B 的公网 URL 是 HTTP(S) 主机名形式 `https://3000-<id>.e2b.app`，裸 TCP 的 Postgres 协议能否经它走通，文档未写，[未证实]）。

**一句话**：Docker-in-microVM，沙箱内代码自己 `docker run`，服务吃沙箱配额，随沙箱生灭。

### 3.2 Daytona

来源：<https://www.daytona.io/docs/en/snapshots.md#run-docker-in-a-sandbox>、<https://www.daytona.io/docs/en/sandboxes.md#linked-sandboxes>（均为 2026-10-31 读取的 markdown）。注意：`daytonaio/daytona` 仓库 README 现写 "As of June 2026, Daytona's core development has moved to a private codebase."，所以源码只能引用既有调研里钉过的旧 commit。

**(a) Docker-in-Docker 快照（沙箱内按需）**

- "Sandboxes can run Docker containers inside them (**Docker-in-Docker**), enabling you to build, test, and deploy containerized applications."
- 直接把依赖服务当卖点："Agents can interact with these services since they run within the same sandbox environment, providing better isolation and security compared to external service dependencies." 列表首条是 "Run databases (PostgreSQL, Redis, MySQL) and other services"。
- 资源算在沙箱头上："Docker-in-Docker sandboxes require additional resources due to the Docker daemon overhead. Consider allocating at least 2 vCPU and 4GiB of memory for optimal performance."
- 做法是**快照（镜像）里预装 Docker**：基础镜像 `docker:28.3.3-dind` / `docker:28.3.3-dind-rootless`，或 `RUN curl -fsSL https://get.docker.com | VERSION=28.3.3 sh -`；Compose 也是同一条路："With Docker-in-Docker enabled in a Daytona sandbox, you can use Docker Compose to orchestrate services like databases, caches, and application containers."
- 能成立的原因：非 GPU 容器沙箱默认特权（`daytona-sandbox-runtime-semantics.md` §0 第 8 条）。

**(b) Linked sandboxes（外部 SDK 起兄弟沙箱）**

- "Linked sandboxes attach ephemeral child sandboxes to a parent. Daytona schedules each child on the same runner as the parent and joins them into a shared link network so the group can communicate over local connections."
- 网络："Linked sandboxes share an internal link network. Connections work in both directions: the parent can reach each child and each child can reach the parent. Every sandbox on the link network is registered under its sandbox name and ID as DNS aliases, so either works as the host."
- 生命周期："Linked sandboxes are always ephemeral and cannot be persisted or resumed after stop. The auto-delete interval must be exactly `0` on create; this is enforced, not a default. … Once stopped, linked children are auto-deleted. Deleting the parent deletes all of its linked children (cascade). One parent may have many linked children (1:N)."
- 触发方：示例是外部 SDK 先 `parent = daytona.create()`，再 `daytona.create(CreateSandboxFromSnapshotParams(linked_sandbox=parent.id, ephemeral=True))`；REST 字段 `linkedSandbox`。示例里父沙箱已建好（`create` 默认返回已启动的沙箱）后才建子沙箱，所以**运行中追加**是这条路的自然用法；但文档没写「必须/可以在父运行中建」，也没写子沙箱的资源是否计入父或组织配额 [未证实]。
- 实现层面，link 网络的隔离是宿主 iptables 按地址匹配，源码注释承认可被其他网桥上的特权容器伪造（既有：`recheck-identity-network-channel-vs-coder-daytona.md` N3 引 `link_icc.go#L20-L26`）。

**一句话**：两条路并存——沙箱内 DinD（特权换来的），或外部 SDK 在同一 runner 上挂 ephemeral 子沙箱、DNS 名互通、级联删除。

### 3.3 Modal

来源：<https://modal.com/docs/guide/sandbox-sidecars>、<https://modal.com/docs/guide/sandboxes#runtimes>、<https://modal.com/docs/guide/private-networking>、<https://modal.com/docs/guide/sandbox-networking>；源码 `modal-labs/modal-client@10170d9c4fc340cb2126aa3e47d1364ad47d77f6`。

**(a) Sandbox Sidecars（beta）——本文唯一「运行中按需 + 平台原语 + 同宿主」的完整先例**

- 定义："Sandbox Sidecars are isolated containers that run alongside your main Sandbox container on the same host."；用例里直接写了依赖服务："Splitting out complex multi-service applications over separate containers, such as databases, caches or worker processes, similar to Docker Compose. The [Redis example](/docs/examples/sidecar_redis) shows the pattern."
- 每个 sidecar（原文列表，markdown 第 47–63 行）：
  - "Is isolated from the main Sandbox container and other Sidecar containers with the same isolation boundary as separate Sandboxes: gVisor or a VM, depending on the Sandbox's runtime."
  - "Can communicate over an internal bridge network with the main Sandbox container and other Sidecar containers."
  - **"Is created through the Modal SDK, not from inside the Sandbox, so code running in the main Sandbox container can't create or modify it."**
  - **"Can be created, terminated, and replaced dynamically during the Sandbox's lifetime. Terminating the Sandbox stops all of its Sidecars."**
  - "Shares the CPU and memory of the Sandbox, which makes Sidecars more cost-efficient than running the same processes across multiple Sandboxes."
- 名字解析："The main Sandbox container is reachable by the hostname `main`, and each Sidecar is reachable by the `name` it was created with. These hostnames are listed in `/etc/hosts`."；且 "`/etc/hosts` is rewritten on sidecar create/terminate and user changes are not preserved."
- 出网策略与容器间流量："Traffic between containers in the same Sandbox is not subject to any container's outbound allowlists, whether CIDR or domain. … This applies only to containers in the same Sandbox; other private IP addresses remain blocked."；"Each Sidecar has its own outbound network policy, independent of the main container's and open by default."；限制里写 "Sidecars can't be fully network-blocked, and their outbound network policy can't be changed after creation."
- 资源计量："The main Sandbox container and the Sidecar containers share the resource allocation (CPU and memory) of the Sandbox, and resources are configured only on the Sandbox."；"There is a hard limit of **250** concurrent sidecar containers per sandbox"。VM runtime 下要在创建沙箱时**预留**内存（`experimental_options={"vm_sidecar_memory_reserve_mib": 1024}`），"The reserve is carved out of the main VM's memory"，"The request must leave the main VM at least 1024 MiB."
- 其他限制："Sidecar images must already exist … Lazy image building is not supported for sidecars."；"A Sidecar's entrypoint does not expose stdin, stdout, or stderr streams."；"a Sidecar can reach the main container's Connect Token port directly and set any `X-Verified-User-Data` header."
- 接口：Python `_experimental_sidecars`，JS/Go `experimentalSidecars`，"provides methods to create, list, get, and terminate Sidecar containers."

**(b) VM runtime 里跑 Docker**

- "Sandboxes run on one of two runtimes: … **gVisor** … provides strong isolation and is suitable for most workloads. … **VMs** run the Sandbox in a virtual machine with its own Linux kernel. Use the VM runtime for workloads that expect a full Linux environment, such as running Docker inside the Sandbox, mounting FUSE filesystems, or managing resources with nested cgroups."
- "Use the VM runtime to run Docker in a Sandbox. Start `dockerd` as the Sandbox entrypoint and run containers with `sb.exec`. Containers on the Docker bridge can reach each other, and `/var/lib/docker` is included in Filesystem Snapshots."；示例 `runtime="vm"` + `readiness_probe=modal.Probe.with_exec("docker", "info", ...)`。
- "GPU Sandboxes are only supported with `runtime="gvisor"`."——**Modal 文档没有给出在 gVisor runtime 下跑 Docker 的路径**；它把 Docker 归给了 VM runtime。（旧 alpha 页 `docker-in-sandboxes` 的 `experimental_options={"enable_docker": True}` 现已重定向到 Sandboxes 总页，本文以现行页为准。）

**(c) i6pn（跨容器私网）**

- "i6pn (IPv6 private networking) is Modal's private container-to-container networking solution."；"All i6pn network traffic is *Workspace private*."；"i6pn networking is however region-scoped functionality, meaning that only i6pn-enabled containers in the same region can perform network communication."；地址发现要自己做："address sharing must be implemented using an auxiliary data structure, such as a shared `modal.Dict` or `modal.Queue`."
- 文档页只写了 `@app.function(i6pn=True)`。沙箱的支持在 SDK 源码里：`py/modal/sandbox/_sandbox.py#L417-L420`（V2 `_experimental_create`）："Set `i6pn=True` to enable private IPv6 networking so sandboxes in the same workspace can address each other directly at their `i6pn.modal.local` address. i6pn only connects sandboxes co-located on the same routable network, so pin every sandbox in the group to the same specific region"。测试 `py/test/sandbox_test.py#L2707-L2709`：`i6pn=True` 与 `block_network=True` 同用会抛 "`block_network` disables all networking, including i6pn"。
- Sidecar 文档自己做了对比："communication between a Sandbox and its Sidecars is at least 3x faster than between separate Sandboxes in the same region."

**一句话**：Modal 三条路都有——sidecar（外部 SDK、运行中、同宿主、共享配额）、VM runtime 里的 Docker（沙箱内代码）、i6pn 兄弟沙箱（创建时开、同 region）。gVisor runtime 下的服务按需路径是 sidecar，不是 Docker。

### 3.4 Coder

来源：`coder/coder@a2e2a9171722aed4865c5725fe67066235bf8cc0` 的 `docs/`。

**Docker in workspaces**（`docs/admin/templates/extending-templates/docker-in-workspaces.md`）给了四种方法，表格原文（L9–L12）：

- Sysbox："Install the Sysbox runtime on your Kubernetes nodes or Docker host(s) for secure docker-in-docker and systemd-in-docker."；模板里 `runtime = "sysbox-runc"`，`startup_script` 里 `sudo service docker start`。
- Envbox："Requires running the outer container as privileged (the inner container that acts as the workspace is locked down)."
- Rootless Podman："Does not require a custom runtime or privileged containers."
- 特权 sidecar："Requires a privileged container. Workspaces can break out to root on the host machine."；正文 L344–L346："This is insecure. Workspaces will be able to gain root access to the host machine."。Kubernetes 版是同 Pod 的 `docker-sidecar` 容器跑 `dockerd -H tcp://127.0.0.1:2375`，工作区容器设 `DOCKER_HOST=localhost:2375`（L418–L442）——**共享 Pod netns 的 sidecar daemon，工作区里按需 `docker run`**。

**Dev Containers 集成**（`docs/user-guides/devcontainers/index.md` L28–L33）："Your workspace needs Docker (via Docker-in-Docker or a mounted socket) and the devcontainers CLI."；每个 dev container 作为 sub-agent 出现。

**网络（直连 vs 中继）**（`docs/admin/networking/index.md`）：

- L9–L11："When possible, we establish direct connections between users and workspaces. … When NAT traversal fails, connections are relayed through the control plane."
- L43–L47：直连需要两端都能对 STUN 收发 UDP（`udp/3478`），然后 "Both agents and clients will then establish a WireGuard tunnel and send UDP traffic on ephemeral (high) ports. If a firewall between the client and the agent blocks this UDP traffic, direct connections will not be possible."
- L52："There must not be a NAT between workspaces and the control plane."
- `docs/admin/networking/stun.md` L97–L100："If both peers are behind a "hard" NAT, direct connections may take longer to establish or will not be established at all. … In both cases, peers fallback to DERP connections if they cannot establish a direct connection."

注意：Coder 这套网络服务的是**用户 ↔ 工作区**，不是工作区 ↔ 依赖服务；依赖服务在 Coder 里永远在工作区内部或同 Pod。

**一句话**：模板（创建时）决定「有没有 Docker」，之后工作区里按需 `docker run`；隔离代价由运维在 Sysbox / 特权之间选。

### 3.5 GitHub Codespaces 与 devcontainer 规范

来源：devcontainer 规范 <https://raw.githubusercontent.com/devcontainers/spec/main/docs/specs/devcontainer-reference.md>（L125–L127）与 `schemas/devContainer.base.schema.json`；Features <https://github.com/devcontainers/features>（`main`，2026-10-31 读取）；模板 <https://github.com/devcontainers/templates/blob/main/src/postgres/.devcontainer/docker-compose.yml>；Codespaces <https://github.com/github/docs/blob/main/content/codespaces/about-codespaces/deep-dive.md>。

- **创建时声明**：Compose 配置的三个字段——"`dockerComposeFile`: the reference to the Docker Compose file(s) to be used." / "`service`: declares the **main** container …" / "`runServices`: an optional property that indicates the set of services in the `docker-compose` configuration that should be started or stopped with the environment."
- 生命周期（schema `shutdownAction`）："Action to take when the user disconnects from the primary container in their editor. The default is to stop all of the compose containers."
- 网络：官方 Postgres 模板让 app 与 db 共享 netns（L15–L16）："# Runs app on the same network as the database container, allows "forwardPorts" in devcontainer.json function." → `network_mode: service:db`。即 **localhost 上的 Postgres**。
- **运行中按需**靠 Feature：`docker-in-docker/devcontainer-feature.json` 第 78 行 `"privileged": true`，并为 `/var/lib/docker` 挂命名卷；`docker-outside-of-docker` 则把宿主 `/var/run/docker.sock` 挂成 `/var/run/docker-host.sock`（L71–L74），其 NOTES 写明 "the Feature is expected to work when the host is running Docker"，且绑定挂载必须用宿主路径。
- Codespaces 隔离：deep-dive L42 "When you create a codespace, a virtual machine (VM) is created … The VM is both dedicated and private to you. … this also allows you to have full root access to your container."——特权容器的爆炸半径被一台专属 VM 兜住。
- 资源：规范没有配额（既有：`prior-art-sandbox-orchestration.md` §2），所有服务吃同一台 VM。

**一句话**：声明式 Compose 是主路径；按需 `docker run` 依赖特权 DinD 或宿主 socket，安全性由「一人一 VM」兜底。

### 3.6 Gitpod / Ona

来源：<https://ona.com/docs/ona/configuration/tasks-and-services/overview.md>、<https://ona.com/docs/ona/configuration/tasks-and-services/generating-tasks-and-services.md>、<https://ona.com/docs/ona/runners/overview.md>。只查了现行的 Ona；Gitpod Classic 未查。

- 隔离：runner "creates isolated VMs from your Dev Container configuration"。
- Service 就是环境内一条长跑命令："**Services** are long-running processes that stay active throughout your session."；"A service's `start` command must **stay running** (block) for the service to remain active. … For example, `npm start` or `docker run postgres` block and keep the service alive, while `docker run -d postgres` returns immediately and the service stops."
- 触发器：`postDevcontainerStart`（创建/重建时）或 `manual`（"On-demand via the CLI or UI"）。示例 service：`start: docker compose up postgres`、`ready: docker compose exec postgres pg_isready`。
- **运行中新增**：配置文件改了不自动生效（"An environment keeps using the tasks and services configuration that it applied when it was created."），但可以用 CLI 现场生成："Create and update tasks and services at runtime using the CLI. When Ona explores a new repository, it can: … Find services in a monorepo and create start commands for each"，命令是 `… | ona environment config apply -`；也能 "Manage tasks and services from outside an environment using the CLI on your local machine or in external pipelines."

**一句话**：Ona 是「环境内 agent 用 CLI 中途声明一个 service」最直接的先例，但 service 仍是环境 VM 里的进程/容器，平台只负责起停与就绪探测。

### 3.7 OpenHands

来源：`OpenHands/sandbox-server@43e91604ccd0a9960c8d5c52faadd60b30f10e63`。

- `openhands/app_server/sandbox/remote_sandbox_service.py#L1090-L1093`：`runtime_class` 默认 `'gvisor'`，描述原文 `'can be "gvisor" or "sysbox" (support docker inside runtime + more stable)'`；L492–L493 在 `sysbox` 时下发 `runtime_class: 'sysbox-runc'`。
- 本地 Docker 版（`docker_sandbox_service.py#L643-L650`）默认 `extra_hosts={'host.docker.internal': 'host-gateway'}`，"This allows containers to resolve hostnames like host.docker.internal for LAN deployments and MCP connections."——容器访问宿主侧服务走宿主网关地址。
- 没有找到「依赖服务」的平台原语；agent 若要 Postgres，就是在沙箱里 `docker run`（仅 sysbox 时成立）或 `apt install`。

**一句话**：默认 gVisor，要 Docker 就换 Sysbox——源码注释自己把两者的取舍写成了一个枚举。

### 3.8 Testcontainers 与 Testcontainers Cloud

来源：testcontainers-java `docs/features/networking.md`、`docs/features/configuration.md`（`main`）；<https://github.com/testcontainers/moby-ryuk>；<https://testcontainers.com/cloud/docs/>（2026-10-31 读取）。

- 按需是它的全部模型：测试进程经 Docker API 起容器，靠随机端口映射回连——"*From the host's perspective* Testcontainers actually exposes this on a random free port."；"When running with a local Docker daemon, exposed ports will usually be reachable on `localhost`. However, in some CI environments they may instead be reachable on a different host."，所以要用 `getHost()` + `getMappedPort()`；`TESTCONTAINERS_HOST_OVERRIDE` 是 "Docker's host on which ports are exposed."
- 生命周期：Ryuk 按标签回收（README："This project helps you to remove containers, networks, volumes and images by given filter after specified delay."，过滤器形如 `label=testing.sessionid=mysession`），且 "Ryuk must be started as a privileged container."
- **Testcontainers Cloud（远端容器运行时）**："When you trigger your tests in a local environment Testcontainers Cloud agent opens an SSH tunnel and connects to the Docker daemon in the cloud environment. It will pull a Docker image and start the container defined in your Testcontainers-based test, both in the connected cloud environment. Connection to the cloud environment will be active while your tests are running. When your test suite completes, Testcontainers Cloud will wait for a short period of time and then close the connection and remove allocated resources automatically."
- 会话与配额："We use sessions to distinguish containers, so the same process will connecting to the same cloud environment"；"Each Testcontainers Cloud session gets 8G of RAM."；"Currently, it's not possible to extend or limit memory usage for a single Testcontainers Cloud session."；镜像缓存在 "users' Testcontainers Cloud VM, which is deleted automatically after being idle for approximately 30 minutes."
- 发现方式：agent 把自己写进 `~/.testcontainers.properties`，库按 `tc.host` → `DOCKER_HOST` → 默认位置的顺序找 daemon。
- 文档明说不支持裸 Docker 命令："Executing raw Docker commands directly against the TCC Docker context is unsupported."
- **[未证实]**：TCC 下 `getHost()`/`getMappedPort()` 返回的地址与端口转发经由 agent 本地监听还是别的机制，文档未写。

**一句话**：Testcontainers Cloud 是「测试进程不在容器宿主上、经一条出站隧道驱动远端 Docker」的先例——与「NAT 后的计算端只拨出」同形，但它的客户端是测试进程，不是不可信代码。

### 3.9 Docker-in-Docker、Sysbox、gVisor

**gVisor**（<https://github.com/google/gvisor/blob/master/g3doc/user_guide/tutorials/docker-in-gvisor.md>）

- "In gVisor, all basic docker commands should function as expected. The host network driver and the bridge network driver are tested and supported."
- 需要的 runsc 标志（表格 L15–L17）：Docker v27 `--net-raw`；v28 加 `--allow-packet-socket-write`；v29 再要 "Either `tmpfs` mount at `/var/lib/docker` OR `--feature containerd-snapshotter=false`"。
- 代价（L46–L47）："`--allow-packet-socket-write` allows sandboxed code to craft arbitrary network packets."
- 网络限制（L23–L28）："`dockerd` inside gVisor needs to be executed with flags `--iptables=false --ip6tables=false` and additional network setup is needed … With iptables disabled, `docker run --expose=` does not expose the port; if a nested container needs to expose ports, inside gVisor use `docker run --network=host`."
- 存储："gVisor currently requires `tmpfs` as an upper layer for `overlayfs`"，推荐 "Mount `tmpfs` at `/var/lib/docker`"。
- 权限："**gVisor *never* runs with capabilities** on the host Linux kernel, even when the above `--cap-add all` flag. This flag only controls the capabilities *perceived* by the in-sandbox application".

**Sysbox**（<https://github.com/nestybox/sysbox>）

- "Unlike alternative runtimes such as Kata and KubeVirt, **it does not use VMs**. … although it does not provide the level of isolation that VM-based runtimes do."
- "while Sysbox hardens the isolation of standard containers and voids the need for insecure privileged containers in many scenarios, it does not (yet) provide the same level of isolation as VM-based alternatives or user-space OSes like gVisor."
- `docs/user-guide/limitations.md`："Running Sysbox inside a Sysbox container is not supported"。

**一句话**：Docker-in-sandbox 的三档代价——特权（宿主 root）、Sysbox（非特权但弱于 gVisor）、gVisor（要放开 raw/packet socket、端口要 host 网络、镜像层吃 tmpfs 即吃内存）。

### 3.10 Fly.io Machines

来源：`superfly/docs@e58f9d7cef54f42c2bb252e87aa08fd58b93667e`。

- `networking/private-networking.mdx` L9："Fly Apps in an organization are connected by a mesh of WireGuard tunnels using IPv6 called a 6PN. Private networking over your 6PN is always available to apps by default"；L11："The Fly.io platform won't forward packets between different 6PNs, unless you explicitly allow it"。
- DNS（L17、L28）："You can use `.internal` domains to connect your app to databases, API servers, or other apps in your 6PN."；"All AAAA queries to Fly.io `.internal` domains only return 6PN information for started (running) Machines."
- 隔离租户：`custom-private-networks.mdx`："you could have an app on a custom 6PN for each customer instance of your service, or for each customer running untrusted code on your platform."；"You can't add an existing app to a custom 6PN or change an app's 6PN."
- 按需：Machines API 有私网内地址 "**Internal base URL:** `http://_api.internal:4280`"（`machines/api/working-with-machines-api.mdx` L23），即私网里的进程持 token 就能现场起一台 Machine 并经 `.internal` 连上。Machine 是 "Firecracker VMs"（`machines/guides-examples/functions-with-machines.mdx` L15）。
- 6PN 流量是否受 network policy 管：既有调研记为 [未证实]（`sandbox-network-policy-apis.md` Fly 节）。

**一句话**：跨主机 WireGuard 私网 + 平台 DNS 是「兄弟服务在任意主机」的成熟形态，但起服务的是持 API token 的调用方，隔离靠 per-tenant 6PN。

### 3.11 Railway / Northflank

- **Railway**（<https://docs.railway.com/guides/private-networking>，页面为客户端渲染，下列原文经抓取工具渲染后取得）："Services communicate over encrypted Wireguard tunnels using internal DNS."；"Reference other services using their internal DNS name: `SERVICE_NAME.railway.internal`"；"Each environment has its own isolated network"。新 service 是否无需重部署即可解析：页面未写 [未证实]。
- **Northflank**：沙箱就是 deployment service（既有：`prior-art-sandbox-orchestration.md` §1.9）；隔离 "CPU sandboxes run in microVMs, and GPU sandboxes run under gVisor"（<https://northflank.com/docs/v1/application/sandboxes/sandboxes-on-northflank-cloud.md>）。数据库是 addon，`POST` Create addon（<https://northflank.com/docs/v1/api/project/addons/create-addon.md>，`type`、`version` 必填）；连通靠项目内网："Ports serving all supported protocols can use project-internal networking. Internal addresses allow access from resources in the same project by default."（<https://northflank.com/docs/v1/application/network/networking-on-northflank.md>）。沙箱内代码能否调 API 建 addon、addon 是否随沙箱删除：文档未写，按其模型 addon 是项目级独立资源 [推断]。

**一句话**：PaaS 的做法是「托管数据库作为独立资源 + 项目级私网」，生命周期与计费都与沙箱分开。

### 3.12 Kubernetes

来源：kubernetes/website `content/en/docs/concepts/workloads/pods/`（`main`）。

- Pod 内 sidecar 共享 netns："Every container in a Pod shares the network namespace, including the IP address and network ports. Inside a Pod (and **only** then), the containers that belong to the Pod can communicate with one another using `localhost`."——但这是**创建时声明**。
- 运行中加容器只有 ephemeral container，而它明确不适合做服务："Ephemeral containers differ from other containers in that they lack guarantees for resources or execution, and they will never be automatically restarted, so they are not appropriate for building applications."；"Ephemeral containers may not have ports"；"Pod resource allocations are immutable, so setting `resources` is disallowed."

**一句话**：K8s 的共享 netns sidecar 只能创建时声明；运行中的只能是另一个 Pod（Service DNS），即「兄弟沙箱」形状。

### 3.13 Tailscale（直连与中继）

- 源码 `tailscale/tailscale@1334899864168f2801ffe4c69d4d6b231b3b9506` `derp/derp.go#L9-L12`："DERP is used by Tailscale nodes to proxy encrypted WireGuard packets through the Tailscale cloud servers when a direct path cannot be found or opened. DERP is a last resort. Both sides between very aggressive NATs, firewalls, no IPv6, etc? Well, DERP."
- Coder 文档（§3.4）对同一机制的描述："peers fallback to DERP connections if they cannot establish a direct connection"，两端都在 hard NAT 后时直连 "will not be established at all"。
- **[未直接核对]** tailscale.com 在本次调研环境中无法打开（TLS 连接被中断）；以下仅来自搜索结果摘要，未能逐字核对原页：connection-types 文档称所有连接先经 DERP 中继、再尝试升级为直连，失败时尝试 peer relay，仍失败则保持 DERP；其 NAT 矩阵中 Easy NAT·Hard NAT 与 Hard NAT·Hard NAT 两行结果为 "Relayed"；《How NAT traversal works》称典型条件下直连成功率 "well north of 90%"。来源：<https://tailscale.com/docs/reference/connection-types>、<https://tailscale.com/blog/how-nat-traversal-works>。

**一句话**：中继不可避免的条件是「两端都无法预测对方映射（hard NAT / EDM）或 UDP 被封」；中继永远存在、直连是优化。

---

## 4. 对 orrery 的含义

约束回顾：Isolation Runtime 是 Node 级的 `runc` 或 `runsc`，请求不能选（ADR-0026）；每个 Sandbox 一个 netns 挂在 Node 的 CNI 网桥上，出网由宿主侧 `bridge` nftables 按源 veth 过滤（runtime.md §13）；Sandbox 不接受入站，Port Forward 走 Gateway → Agent → unix socket → Supervisor（§14）；Node 可能在 NAT 后、只拨出；Sandbox Token 只能取 Git 凭据「and nothing else」（GLOSSARY）；Workspace 永久绑定一台 Node；privileged 在能力面是 `unsupported`（`system-feature-gap-vs-coder-daytona.md`）。下面只摆选项与代价，不做裁决。

### 模式 A：Docker-in-Sandbox（沙箱内代码自己 `docker run`）

先例：E2B、Daytona DinD、Modal VM runtime、Coder+Sysbox、Codespaces DinD Feature、Ona、OpenHands+sysbox。

- **runc Node**：要么特权（与我们「多租户不可信」直接冲突，Coder 自己写 "Workspaces will be able to gain root access to the host machine"），要么引入 Sysbox 作为第三种 Isolation Runtime（Sysbox 自称弱于 gVisor；且与 runsc 互斥，一台 Node 仍只能一种）。
- **runsc Node**：gVisor 官方支持，但要给 runsc 加 `--net-raw`、`--allow-packet-socket-write`（"allows sandboxed code to craft arbitrary network packets"），这是 Node 级 runtime 选项，会作用于该 Node 上**所有** Sandbox。我们的过滤按 `iifname` 而不是源地址，伪造源 IP 绕不过 Egress Policy [推断]；但链里第 2 步放行 ARP，伪造 L2 帧对同网桥其他 Sandbox 的影响需要单独评估 [未验证]。嵌套容器的出网仍从同一根 veth 出去，所以 Egress Rules / Mode 照样生效 [推断，未实测]。Docker v29 要求 `/var/lib/docker` 用 tmpfs 或关 containerd snapshotter——tmpfs 意味着镜像层算进 Sandbox 内存上限，runsc 下内存超限的后果见 ADR-0026（可能整个 Sandbox `runtime_lost`）。嵌套容器要暴露端口只能 `--network=host`，正好与我们 Port Forward 走 Sandbox 内 `127.0.0.1` 的路径兼容。
- 计量自然：全部算进 Sandbox 自己的 Resource Allocation；生命周期自然随 Sandbox；镜像拉取走 Sandbox 出网，受 Egress Mode 约束（`none` 模式下拉不到镜像）。
- 代价集中在：能力面要新增一项（类似 `nested_containers: enforced|unsupported`），以及 runsc 标志对整台 Node 的放宽。

### 模式 B：创建时声明的同 netns sidecar（Compose / Pod 形状）

先例：devcontainer `runServices` + `network_mode: service:db`、K8s Pod sidecar、Coder 的 Pod 内 `docker-sidecar`。

- 在我们这里就是「一个 Sandbox = 一个 netns 里多个容器」，Postgres 在 `localhost:5432`，Egress Policy 天然共用一根 veth，Port Forward 不变。
- 只能**创建时**声明，覆盖不了「中途决定要 Postgres」——K8s 文档把运行中加容器的唯一手段（ephemeral container）明确排除在服务用途之外。
- 计量：sidecar 资源要么并入 Sandbox 的申请（Modal 的做法），要么单列；runsc 下多个容器在同一 sandbox 里共享一个 Sentry，内存/`pids.max` 的核算会更难解释（ADR-0026 已记录 runsc 下 `pids.max` 计的是宿主任务）。
- 改动面：ADR-0009 的 Provider 契约目前是「一个 Sandbox 一个容器 + Supervisor」，要变成多容器。

### 模式 C：运行中追加、外部 SDK 触发的同 Node 兄弟容器（Modal Sidecars / Daytona Linked 形状）

先例：Modal Sandbox Sidecars（同宿主、`/etc/hosts` 名字、共享配额、外部 SDK 才能建）、Daytona Linked sandboxes（同 runner、DNS 别名、ephemeral、级联删除）。

- 与我们的拓扑最贴：Workspace 本就绑定一台 Node，兄弟放同一 Node、同一 CNI 网桥即可互通，**不需要跨 Node 网络，也不碰 NAT**。
- 网络：我们的 nftables 只看发送方 veth，所以「主 Sandbox → 兄弟」是否可达取决于主 Sandbox 的 Egress Policy；ADR-0026 写死「平台不加隐式条目」「reachability of … other Sandboxes is entirely the operator's configuration」。要让它开箱即用，就得在 Egress Policy 里新增一个「同组兄弟」的隐式放行（Modal 正是这么做的："Traffic between containers in the same Sandbox is not subject to any container's outbound allowlists"），这会触及 ADR-0026 的「无隐式条目」原则——按 AGENTS.md 需要先开 issue 改 spec。名字解析：我们目前没有 DNS 面，Modal 用改写 `/etc/hosts`，Daytona 用 DNS 别名。runsc 下改写运行中 Sandbox 的 `/etc/hosts` 需要经 Supervisor 或 host-backed 挂载完成 [未验证]。
- 触发方：两家都**只允许外部 SDK**。若要让沙箱内的 agent 中途起服务，要么由调用方（外部编排 agent 的进程）代办，要么扩 Sandbox Token 的权限面（GLOSSARY 现在写的是「Git access, and nothing else」）。
- 计量：Modal 共享主沙箱配额（VM 下还要创建时预留内存），Daytona 未写。对我们是「并入父 Resource Allocation」与「独立 Allocation + 由 Scheduler 在同 Node 上准入」二选一；后者在 Node 满时会失败，需要定义错误。
- 生命周期：Daytona 的「子沙箱强制 ephemeral + 删父级联删子」与 Modal 的「Terminating the Sandbox stops all of its Sidecars」一致，可直接借鉴。

### 模式 D：跨主机私网上的兄弟服务（Fly 6PN / Modal i6pn / Railway / Tailscale 形状）

- 好处是服务可以放在任意 Node、甚至长期存在被多个 Sandbox 共享。
- 代价：Node 在 NAT 后，Node 间直连需要 STUN 打洞 + WireGuard，两端都是 hard NAT 时必须中继（Tailscale `derp.go`："DERP is a last resort"）；我们既有调研的结论是「不要引入 tailnet」（`prior-art-sandbox-orchestration.md` §1.10）。Sandbox 子网 `ipMasq` 出宿主，跨 Node 的 Sandbox 地址本就不可路由。Modal 自己也把 i6pn 限制在「same region / same routable network」，说明即便厂商自建也不做任意拓扑。
- 一个不需要覆盖网的变体是**经 Control Plane 中继**：沿用 Tunnel 的 Gateway → Agent → unix socket 路径，再加一条「Sandbox 出站连到另一个 Sandbox 的端口」的反向通道。它能穿 NAT，但每个连接都经过 Control Plane，带宽与延迟成本类似 DERP 的常态化，而 DERP 在 Tailscale 里只是兜底。

### 模式 E：远端容器运行时（Testcontainers Cloud 形状）

- 沙箱里的代码只拿到一个 Docker API 端点（`DOCKER_HOST` 或 `~/.testcontainers.properties`），容器跑在别处（专用 worker VM），由一条出站隧道接上；会话结束后延迟回收，按会话固定配额（TCC 每会话 8G、不可调）。
- 对我们：沙箱内的 Docker API 端点可以就是 Supervisor 暴露在 run 目录里的 unix socket（runsc 下 `host-uds=create` 已让它对宿主可见），Agent 代为在同 Node 起容器——这把模式 A 的「谁有内核」问题移出 Sandbox，又把模式 C 的「谁能触发」交给沙箱内代码。但它等于把 Docker Engine API（或其子集）暴露给不可信代码：必须做 API 白名单（TCC 自己也写 "Executing raw Docker commands directly against the TCC Docker context is unsupported"，其 registry 代理只放行 HEAD/GET 与白名单路径），还要处理端口映射回 Sandbox 的方式（TCC 的这部分机制文档未写，[未证实]）。

### 模式 F：托管依赖作为独立资源（Northflank addon / Railway service 形状）

- Postgres 是一个与 Sandbox 平级、有自己生命周期与计费的资源，经项目私网访问。适合多个 Sandbox 共享、需要持久化与备份的场景；对「调试中临时要一个空库」显得过重。
- 在我们这里它会和 Workspace / Resource 的持久化语义（ADR-0012）发生关系，且仍然要回答模式 C 或 D 的网络问题。

---

本文未覆盖：Gitpod Classic、Kubernetes 上的 ephemeral environment 产品（如 Okteto、Signadot）、Docker Inc. 的 Docker Sandboxes。tailscale.com 页面无法直接打开，相关原文只来自搜索摘要，已标注。
