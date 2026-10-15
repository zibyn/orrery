# 沙箱后端的隔离与 GPU 直通

> 调研 ticket：zibyn/stars-sandbox#3。产出供 #10（Runtime Provider 抽象与首发后端）与 #11（Sandbox Scheduler 放置与 GPU 表达）使用。
> 术语以 `GLOSSARY.md` 为准：Runtime Provider / Sandbox / Workspace / Control Plane。
>
> 所有结论只依据一手来源（官方文档、上游仓库文档），链接见文末 **Sources**。

## 0. 结论先行

1. **三个后端可以收敛到一个接口，但收敛面比想象中窄。** 能统一的是「生命周期 + 命令执行 + Workspace 挂载 + CPU/内存/PID 上限 + 加速器可见性请求」；不能统一的是「磁盘配额、出网白名单、显存上限、rootless 语义」。接口必须是 **能力探测 + 显式降级**，不能让调度器以为三个后端一样。
2. **首发 docker（rootful），同时用 local 做参考实现。** local 用来锁接口、跑测试，零依赖；docker 是第一个生产后端。理由与反转条件见 §6。
3. **GPU 直通的规范表达是 CDI 设备名**（`nvidia.com/gpu=0`），不是 `--gpus`。`--gpus` 是 docker 的便利语法，podman 官方只文档化了 `--gpus all`。CDI 在 docker 与 podman 上都可用，是唯一交集。
4. **「按任务限制显存」没有任何后端开箱能做。** 只有 MIG（硬件切分）或 MPS v3 `dmem.max`（cgroup v2 + CUDA 13.4+ + kernel 6.14+，且与 MIG 互斥）能做到强制上限。这是需要写进调度器契约的硬约束，不是实现细节。
5. **「只出站 + 白名单」没有任何后端原生支持。** `podman network create --internal` 解决的是「完全断外网」，不是白名单。白名单必须由我们自己往容器的 network namespace 里注入 nftables 规则。
6. **国产加速器现在不需要进抽象面，但抽象面必须留一个洞。** 见 §5。

---

## 1. 后端 × 能力矩阵

### 1.1 隔离与资源

| 能力 | local（裸进程） | docker | podman |
|---|---|---|---|
| 默认权限模型 | Worker 自身 uid，无降权 | **rootful**（root daemon + `/var/run/docker.sock`） | **rootless**（daemonless，无 socket） |
| 用户命名空间 | 无 | `userns-remap` 可选，默认关 | rootless 默认开（容器 root ↔ 调用者 uid） |
| 文件系统隔离 | 无（共享宿主 mount ns） | mount ns + overlay2 rootfs | mount ns + overlay（rootless 下 fuse-overlayfs 或内核 ≥5.12 的 native overlay） |
| Workspace 挂载 | 就是宿主目录 | `-v` bind mount | `-v` bind mount（rootless 用 `--userns=keep-id` 保持 uid 对齐） |
| CPU 限流 | `systemd-run --scope -p CPUQuota=` | `--cpus`（= `--cpu-period`/`--cpu-quota`） | `--cpus`；**cgroup v1 rootless 上不支持** |
| 内存上限 | `systemd-run --scope -p MemoryMax=` | `--memory` | `--memory`；**cgroup v1 rootless 上不支持** |
| PID 上限 | `-p TasksMax=` | `--pids-limit` | `--pids-limit`；同上 |
| IO 带宽 | `-p IOReadBandwidthMax=` | `--device-read-bps` | `--device-read-bps`；同上 |
| **磁盘配额** | **无**（除非 Workspace 在独立分区 / XFS project quota / loop 设备上） | `--storage-opt size=`：仅 overlay2 + **XFS `pquota`**；bind mount 的 Workspace **不受限** | `storage.conf` overlay `size=`：同样需 XFS + `pquota`；bind mount 不受限；rootless 下行为**文档未说明** |
| 设备节点级强制 | 需 systemd `DevicePolicy=closed` + `DeviceAllow=`（eBPF 过滤） | 运行时/设备 cgroup | 运行时/设备 cgroup；rootless 下**容器内无法创建设备节点**（无 `CAP_MKNOD`） |
| 前置依赖 | systemd + cgroup v2 | docker daemon（root） | subuid/subgid ≥65536 条目、uidmap 包、kernel ≥5.12 可选 |

**共同前提**：CPU/内存/PID/IO 限制在三个后端上都要求 **cgroup v2 + systemd**。rootless 场景下还必须把控制器委派到用户 slice：

```ini
# /etc/systemd/system/user@.service.d/delegate.conf
[Service]
Delegate=cpu cpuset io memory pids
```

委派 `cpuset` 需要 systemd ≥244。默认只委派 `memory pids`。委派缺失时 docker 的 `--cpus`/`--memory`/`--pids-limit` 是**静默忽略**的（`docker info` 的 `Cgroup Driver` 显示 `none` 即为未满足）；podman 的文档直接写 "This option is not supported on cgroups V1 rootless systems"。

### 1.2 网络

| 能力 | local | docker | podman |
|---|---|---|---|
| 默认真空 | 直接用宿主 netns，**无隔离** | rootful: bridge；rootless: slirp4netns / VPNKit / pasta / gvisor-tap-vsock（按装入情况选） | rootful: bridge（netavark）；rootless: **pasta**（Podman 5.0 起默认） |
| 完全断网 | 自己做 netns | `--network none` | `--network none` |
| 禁外部访问（内网互连） | 自己做 | 自建 bridge + iptables | `podman network create --internal`：bridge 上关闭 IP forwarding、不设默认路由、DNS 只解析容器名（其余 NXDOMAIN） |
| 网络间隔离 | — | 自建 | `podman network create -o isolate=strict`（默认 strict） |
| **出网白名单** | **不支持，需自建** | **不支持**，需往容器 netns 注入 nftables | **不支持**，同上 |
| 客户端源 IP 保留 | N/A | rootless: RootlessKit 转发，**Docker <29.5 不保留源 IP**；29.5+ 支持 | rootless bridge 默认 `rootlessport` **不保留源 IP**；`rootless_port_forwarder="pasta"` 保留但**标记为实验性** |
| 低端口绑定 | N/A | rootless: 需 `net.ipv4.ip_unprivileged_port_start` | rootless: **无法绑定 <1024** |

**结论**：egress-only + 白名单没有一个后端开箱支持。可行路径只有一条——用 OCI hook 或入口包装，在容器的 network namespace 内装 nftables default-deny + allowlist。`--internal` 与白名单是两件事，不要混。

### 1.3 加速器直通

| 能力 | local | docker | podman |
|---|---|---|---|
| 按卡数/卡号选择 | `CUDA_VISIBLE_DEVICES`（**仅运行时约定，非安全边界**） | `--gpus '"device=0,1"'`、`--gpus GPU-<uuid>`、`--gpus 2` | CDI：`--device nvidia.com/gpu=0`；`--gpus` 官方只文档化 `all` 与 vendor-specific identifier |
| 统一表达 | 降级为环境变量 | `--device nvidia.com/gpu=0`（CDI） | `--device nvidia.com/gpu=0`（CDI） |
| CDI 支持版本 | — | 25.0.0 起支持（opt-in `features.cdi=true`），28.2.0/28.3.0 起默认开启 | CDI 自 3.2.0；`--device` 接受 CDI 设备名自 **4.1.0** |
| CDI spec 位置 | — | `/etc/cdi`、`/var/run/cdi`（`cdi-spec-dirs` 可改） | `/etc/cdi`、`/var/run/cdi`（`cdi_spec_dirs`，containers.conf） |
| 驱动能力裁剪 | 无 | `--gpus 'all,"capabilities=compute,utility"'` | 无（CDI spec 决定） |
| **显存上限** | **无** | **无**（除 MIG/MPS） | **无**（除 MIG/MPS） |
| MIG 设备请求 | 靠 `CUDA_VISIBLE_DEVICES=MIG-GPU-<uuid>/<gi>/<ci>` | `--device nvidia.com/gpu=1:0`（GPU 1 上第一个 MIG 实例） | 同 CDI 语法 |
| 内核级 GPU 强制 | systemd `DeviceAllow=/dev/nvidia0 rw` + `DevicePolicy=closed` | 设备 cgroup（运行时自动设） | 设备 cgroup |

**关键差异**：`--gpus` 的 `count` 形式（`--gpus 2`）是 docker 独有；podman 没有对等物。跨后端表达必须落到 CDI 名字或显式卡号列表。

---

## 2. GPU 直通细节

### 2.1 三层机制

1. **NVIDIA Container Toolkit**：安装后 `nvidia-container-runtime` 作为 OCI runtime 包装 runc，在容器创建时按 `NVIDIA_VISIBLE_DEVICES` / `NVIDIA_DRIVER_CAPABILITIES` 挂载驱动库与设备节点。`--gpus` 是 docker CLI 对这套 env 的语法糖。
2. **CDI（Container Device Interface）**：把「设备需要什么」写成一个 YAML spec（`/var/run/cdi/nvidia.yaml`），运行时读 spec 注入。工具链 v1.12.0 起支持生成；v1.18.0 起有 `nvidia-cdi-refresh.service` 自动在安装/升级/驱动变更/重启时重新生成。生成命令：
   ```bash
   sudo nvidia-ctk cdi generate --output=/var/run/cdi/nvidia.yaml
   nvidia-ctk cdi list          # 列出 nvidia.com/gpu=all、nvidia.com/gpu=0 等
   ```
   CDI 的价值：不依赖 `no-cgroups`，因而**与 rootless 兼容**，且跨运行时。
3. **`--gpus` / `--device`**：见 §1.3。

### 2.2 `NVIDIA_VISIBLE_DEVICES` 取值（toolkit 官方定义）

| 值 | 含义 |
|---|---|
| `0,1,2` 或 `GPU-<uuid>` | 逗号分隔的卡号或 UUID 列表 |
| `all` | 全部卡（CUDA 基础镜像默认值） |
| `none` | 无卡，但驱动能力仍启用 |
| `void` / 空 / 未设 | 等同 runc，既不给卡也不给能力 |

`NVIDIA_DRIVER_CAPABILITIES` 是**替换**而非追加，必须显式列全所需项（`compute`、`utility`、`video`、`graphics`、`display`、`compat32`）。最小可跑组合是 `compute,utility`。

### 2.3 按任务限制卡数与显存

- **卡数/卡号**：容器后端靠 CDI 设备名或 `--gpus device=`；local 靠 `CUDA_VISIBLE_DEVICES`。
  - `CUDA_VISIBLE_DEVICES` 的语义是「只列出的设备可见，且**按列出顺序重新编号**」；出现非法项时，只有排在它之前的项可见。`CUDA_DEVICE_ORDER` 默认 `FASTEST_FIRST`，需要与 `nvidia-smi` 对齐时改 `PCI_BUS_ID`。
  - **它不是安全边界**：进程可以自己改这个环境变量，`/dev/nvidia*` 也全都还在。
- **卡数/卡号（内核级，local 唯一可行路径）**：把进程放进一个 `DevicePolicy=closed` + `DeviceAllow=` 的 cgroup。`DeviceAllow=` 由 eBPF 过滤强制执行，**无法被进程用 `+` 前缀绕过**。只放行 `/dev/nvidia0`（以及必需的 `/dev/nvidiactl`、`/dev/nvidia-uvm`），其它卡在设备层就打不开。这是 local 后端能给出的真实限制。
- **显存上限**：只有四条路，且都有代价：

  | 手段 | 隔离强度 | 要求 |
  |---|---|---|
  | MIG | **硬件隔离**：独占显存/SM/L2/带宽 QoS/故障域 | 仅受支持的卡（A100/A30/H100 等）；profile 固定（如 `1g.5gb`、`2g.10gb`、`7g.40gb`），不可任意指定显存数 |
  | MPS + `CUDA_MPS_PINNED_DEVICE_MEM_LIMIT` | 软件；Volta+ 上是硬上限 | 需 MPS 控制守护进程；`CUDA_MPS_ACTIVE_THREAD_PERCENTAGE` 是**软**的算力百分比 |
  | MPS v3 `dmem.max` | 硬上限（分配时拒绝，返回 `CUDA_ERROR_OUT_OF_MEMORY`） | **cgroup v2 + CUDA 13.4+ + kernel 6.14+（dmem 控制器）**，且**与 MIG 互斥** |
  | HAMi 类用户态拦截 | 调度与记账保证，**非安全边界** | 第三方 |

  MIG 的 profile 是离散的：A100 40GB 有 `1g.5gb`(×7)、`2g.10gb`(×3)、`3g.20gb`(×2)、`4g.20gb`、`7g.40gb`。所以「给这个任务 6GB 显存」这个需求**在 MIG 上无法精确表达**，在 MPS v3 上可以但门槛高。
- **time-slicing（共享）**：无显存上限、无故障隔离——一个任务 OOM 会波及同卡其它任务。只适合「反正互相信任」的场景。

### 2.4 docker 与 podman 的实操差异

- **docker**：`--gpus` 需要先装 `nvidia-container-runtime`（toolkit）。CDI 通道走 `--device nvidia.com/gpu=0`，Docker 25.0.0–28.1.1 需要在 `daemon.json` 里开 `{"features": {"cdi": true}}`，28.2.0/28.3.0 起默认开启。
- **podman**：NVIDIA 官方推荐 **CDI** 路径。`--gpus` 在本项目要用到「按卡选择」时**不可用**，必须 `--device nvidia.com/gpu=0`。已知坑：`--device nvidia.com/gpu=all` 会把大量宿主路径只读挂进容器（含 glibc），在部分发行版上会出问题。
- **rootless 的坑**：toolkit 传统上要求 `/etc/nvidia-container-runtime/config.toml` 里设 `nvidia-container-cli.no-cgroups=true` 才能让 rootless 容器看到 GPU，而这**恰好关掉了 cgroup 层的设备/资源限制**——即「要么给 GPU，要么要限制」。CDI 路径绕开了这个二选一，是 rootless + GPU 应该走的路线。libnvidia-container 1.8.0 起支持 cgroup v2，toolkit 1.18.0 修复了 rootless 下的 cgroup 限制问题。
- 另一个已知问题：systemd cgroup driver 下执行 `systemctl daemon-reload` 后，容器可能丢失 GPU 访问。Worker 需要把「GPU 设备消失」当作可检测的运行时故障。

---

## 3. 隔离细节

### 3.1 用户命名空间：rootless vs rootful

- **docker 默认 rootful**：daemon 以 root 跑，`/var/run/docker.sock` 等价于宿主 root。优点：cgroup 限制、设备、磁盘配额、macvlan 全都无条件可用。缺点：Worker 进程一旦被攻破即等于宿主 root。
- **podman 默认 rootless**：daemonless，Worker 直接 exec `podman`，无 socket 可偷。容器 root 映射到调用者 uid，容器 uid n 映射到 `subuid + (n-1)`。需要 `/etc/subuid`、`/etc/subgid` 各 ≥65536 条目和 uidmap 包。
- rootless 的硬限制（podman 官方 "shortcomings" 列表）：cgroup v1 上不支持资源限制；容器内无法创建设备节点（即使 `--privileged`，内核要求 `CAP_MKNOD`）；无法绑定 <1024 端口；NFS / 并行文件系统 home 目录不支持；`podman mount`/checkpoint-restore 能力受限；macvlan/ipvlan 驱动**仅 root 可用**；只有 overlay（可配 fuse-overlayfs）与 vfs 两种存储驱动。
- **对本项目的含义**：map 的信任模型写的是「沙箱主机可信、多租户不可信」。多租户 = 任务代码（智能体生成的代码）。因此：

  | 后端 | 隔离强度 | 在本信任模型下的定位 |
  |---|---|---|
  | local | 无 | 仅测试/开发 |
  | docker rootful | 中（namespaces + cgroups，daemon 是宿主 root） | 可接受，但 Worker 必须唯一持有 socket |
  | podman rootless | 中高（无 root daemon，userns 默认） | 生产首选姿态，代价是资源限制前置条件多 |

### 3.2 文件系统

- **Workspace 的落点决定一切**。绑进容器的 bind mount **不受** 容器存储驱动的配额管辖。也就是说 `--storage-opt size=` 只管容器可写层，管不到 Workspace。
- 要给 Workspace 限容量，只有三条路：给 Workspace 单独的分区/逻辑卷；XFS 文件系统 + project quota（`pquota` 挂载选项，`/etc/projects` + `/etc/projid` + `xfs_quota`）；loop 设备 + 固定大小文件系统。
- 容器可写层的配额：docker `--storage-opt size=`（**仅 overlay2 + XFS pquota**，ext4 不支持——内核有 project quota 但 docker 未接入）；podman `storage.conf` 的 overlay `size=` / `inodes=`（同样要求 XFS + pquota）。**两者在 rootless/fuse-overlayfs 下是否生效，官方文档都没写**——需要实机验证，不能写进契约。
- 因此 Workspace 应该挂在宿主目录（bind mount），配额由宿主文件系统保证，而不是依赖容器存储驱动。这与 `GLOSSARY.md` 里「智能体看见的是 Workspace，不是 Sandbox」一致：Workspace 的生命周期不该绑在容器可写层上。

### 3.3 网络

- 默认都是「能出能进」。要做「只出站 + 白名单」：
  1. 容器网络设为 `none` 或 `--internal`（禁掉默认路由与 NAT）；
  2. 在容器 netns 内装 nftables `policy drop` + 出站 allowlist；
  3. 白名单靠 DNS 解析结果缓存（域名 → IP）还是纯 IP 段，是另一个决策。
- 注入点选 OCI hook 最干净（与后端无关），其次是入口包装脚本。
- **注意**：container 内的路由**不能**当安全边界——持有 `CAP_NET_ADMIN` 的容器可以自己改路由。真正有效的是 netns 内的 nftables。
- rootless 的转发语义与 rootful 不同（源 IP 保留问题、pasta 实验性），如果白名单依赖源 IP，必须在 rootful 与 rootless 分别验证。

### 3.4 cgroup 资源限制

- **内核态统一在 cgroup v2**：单一层级、`cgroup.controllers` / `cgroup.subtree_control` 自上而下委派、no-internal-process 约束（域控制器只能装在无自身进程的非 root cgroup 上）。
- 可用控制器：`cpuset`、`cpu`、`io`、`memory`、`pids`、`hugetlb`、`rdma`（本机另见 `misc`、`dmem`）。
- `memory.max` 是硬上限（触发 cgroup 内 OOM killer，且**允许短暂超限**）；`memory.high` 是节流阀，官方建议以 `high` 为主、`max` 为最后防线。
- `io.max` 是**吞吐/IOPS 限流**（`rbps`/`wbps`/`riops`/`wiops`），**不是容量配额**。cgroup v2 里没有「磁盘空间」控制器——容量只能靠文件系统 project quota。这是必须写进契约的边界。
- `pids.max` 是硬上限，但组织性移动不受限，所以 `pids.current > pids.max` 是可能的；会失败的是 fork/clone（`-EAGAIN`）。
- local 后端唯一可行的落地方式是 `systemd-run --scope -p MemoryMax=... -p CPUQuota=... -p TasksMax=...`，把进程塞进一个 systemd scope。

---

## 4. 版本要求汇总

| 组件 | 最低版本 | 用途 | 来源 |
|---|---|---|---|
| Linux cgroup v2 | 发行版默认（Ubuntu 26.04 / RHEL 9+ 已默认） | 所有资源限制 | kernel cgroup-v2 文档 |
| kernel 5.12 + Podman 3.1 | — | rootless 原生 overlayfs（否则退到 fuse-overlayfs） | podman rootless.md |
| kernel 6.14 | — | MPS v3 `dmem.max` 的 dmem 控制器 | MPS v3 内存分区文档 |
| systemd 244 | — | 向用户 slice 委派 `cpuset` | docker rootless tips |
| Docker Engine 19.03 | — | `--gpus` | docker run 参考 |
| Docker Engine 25.0.0 | — | CDI（需 `features.cdi=true`） | CDI 项目 README |
| Docker Engine 28.2.0 / 28.3.0 | — | CDI 默认开启（两个来源的版本号不一致，见 Sources 注） | CDI README / docker 文档 |
| Podman 3.2.0 | — | 初始 CDI 支持 | CDI 项目 README |
| Podman 4.1.0 | — | `--device` 接受 CDI 设备名 | toolkit CDI 文档 |
| Podman 5.0 | — | rootless 默认 pasta | podman rootless.md |
| nvidia-container-toolkit 1.12.0 | — | `nvidia-ctk cdi generate` | toolkit CDI 文档 |
| nvidia-container-toolkit 1.18.0 | — | `nvidia-cdi-refresh.service`；rootless cgroup 修复 | toolkit CDI 文档 / toolkit issue #189 |
| libnvidia-container 1.8.0 | — | cgroup v2 支持 | nvidia-container-toolkit issue #189 |
| CUDA 13.4 | — | MPS v3 内存分区 | MPS v3 文档 |
| NVIDIA 驱动 R470 / R525 | — | MIG profile `*+me` / `1g.10gb` | MIG supported profiles |
| NVIDIA 驱动 R570 | — | 同卡 MIG 实例间 P2P | MIG supported profiles |
| RootlessKit 3.0（Docker 29.5） | — | rootless 下保留客户端源 IP | rootlesscontaine.rs |

**实测环境**（本机，供参考，非契约）：Ubuntu 26.04.1、kernel 7.0.0、cgroup v2（控制器含 `dmem`）、systemd 259、Docker 29.8.2、runc 1.5.1；**podman 未安装，nvidia 工具链未安装**。

---

## 5. 非 NVIDIA 加速器

**结论：现在不进抽象面，但抽象面必须能容纳它。**

原因：

1. 国产加速器（昇腾 Ascend NPU、寒武纪 MLU、海光 DCU/HCU、天数智芯 Iluvatar、摩尔线程 MThreads、沐曦 MetaX、昆仑芯 XPU 等）**不走标准 CDI**。它们的主流做法是「自定义 OCI runtime + 厂商环境变量 + 厂商 device plugin」，例如昇腾的 Ascend Docker Runtime + `ASCEND_VISIBLE_DEVICES`。CDI 在这些卡上的支持目前主要通过第三方（HAMi、Volcano vgpu-device-plugin）出现，不是厂商一手能力。
2. 每种卡的资源维度都不同：昇腾是 vNPU / `ASCEND_VNPU_SPECS=vir04`，海光是 vDCU（`hy-smi` 切分），NVIDIA 是 MIG/MPS。**没有共同的最小公倍数**可以抽象。
3. 本项目当前没有国产卡的实测环境，抽象一个我们无法验证的维度只会得到错误的接口。

**该留的洞**：Runtime Provider 的加速器表达不要写死成 `gpu_count: int`。写成：

```
accelerator:
  vendor: nvidia | <other>      # 厂商
  device: string                # 厂商原生设备标识（NVIDIA 用 CDI 名）
  kind: whole | mig | mps | vnpu # 切分方式
```

并把「能不能满足这个请求」交给 `Capabilities()` 探测回答。调度器（#11）据此放置。这样将来加昇腾只需要新增一个 vendor 分支，不需要改接口。

---

## 6. 首发后端

**首发 docker（rootful + CDI），local 作为参考实现同期落地（仅测试/开发，不作生产默认），podman 排第三。**

理由：

1. **GPU 直通是三个需求里最不宽容的**，而 docker 的路径最成熟：`--gpus` 自 19.03 起，CDI 自 25.0.0 起、28.2.0/28.3.0 起默认开启；按卡选择有两条官方路径（`--gpus device=` 与 CDI `--device`）。podman 的 `--gpus` 官方只文档化 `all`/vendor identifier，按卡必须走 CDI，少一条退路。
2. **rootful 让 cgroup 限制无条件生效**。podman 默认 rootless，而 rootless 的 `/etc/subuid` 委派、`Delegate=` drop-in、cgroup v1 上「限制被静默忽略」都是部署期的坑，会让接口的第一次实现验证不出真实行为。
3. **磁盘配额路径有文档**（overlay2 + XFS pquota），podman 侧同样机制但 rootless 下行为未文档化。
4. **团队环境已有 docker**（本机实测：Docker 29.8.2，podman 未安装），首发能立刻开工。

**反转条件（写清楚，别含糊）**：如果目标内网是 RHEL / openEuler / 麒麟这类**默认只装 podman、不打包 docker** 的发行版——考虑到 ticket 提到国产卡，这个可能性不低——那就首发 podman rootless，并接受「资源限制需要先做 subuid + Delegate 委派」的部署前置。**接口设计必须对二者都不偏袒**：加速器一律用 CDI 设备名表达，`--gpus` 只作为 docker 后端的内部优化。

**为什么 local 不能当生产默认**：local 没有任何命名空间隔离，与 map 的「多租户不可信」直接冲突。但它实现成本近乎为零，且能让接口在无 docker、无 toolkit 的 CI 里被测试。所以它是**参考实现**，不是**生产后端**。

---

## 7. 抽象面：能依赖什么，不能依赖什么

### 能统一、可以写进接口契约

| 能力 | 落地方式 |
|---|---|
| 创建 / 执行 / 停止 / 销毁 | 各后端原生 |
| 命令执行 + stdout/stderr + 退出码 | 各后端原生 |
| Workspace 目录挂载（bind mount） | 各后端原生 |
| CPU 上限 | cgroup v2（容器用 flag，local 用 systemd scope） |
| 内存上限 | 同上 |
| PID / 任务数上限 | 同上 |
| 加速器可见性（哪些卡） | **CDI 设备名**为规范表达；local 降级为 `DeviceAllow` + `CUDA_VISIBLE_DEVICES` |
| 只出站网络 | `none`/`--internal` + netns 内 nftables（自带，非后端能力） |

### 不能统一、必须降级或拒绝

| 能力 | 为什么不能统一 |
|---|---|
| 磁盘配额 | local 没有；容器侧只管可写层，管不到 bind mount 的 Workspace；rootless 下行为未文档化 |
| **显存上限** | 三个后端都没有原生能力，只有 MIG（离散档位）或 MPS v3（高版本门槛 + 与 MIG 互斥） |
| 出网白名单 | 没有后端原生支持，必须自带 nftables 注入 |
| 网络源 IP 语义 | rootless 下因转发实现而异，且随版本变化 |
| rootless vs rootful | 语义差异渗透到设备、存储、端口、网络，不能抹平 |
| 卡数 `count=N` | docker 有 `--gpus 2`，podman 无对等物 |

### 接口要求

1. `Capabilities()` 必须存在且被调度器读取。**不要**让三个后端假装一样。
2. 请求里区分「必须满足」与「尽力而为」。显存上限、出网白名单属于「后端不保证」的类别，调度器要么据此选择主机，要么在任务提交时就拒绝。
3. 加速器用结构化描述（见 §5），不要 `gpu_count: int`。
4. 所有资源限制的**实际生效值**要能从沙箱侧回读（cgroup 文件 / `docker inspect`），用于回报给控制层——因为 rootless + cgroup v1 的失败模式是**静默忽略**。

---

## 8. 未解决 / 需要在实现时验证

- rootless（docker 与 podman）+ fuse-overlayfs 下 `size=` 配额是否生效——官方文档均未说明。
- 内网目标发行版是 docker 系还是 podman 系（决定 §6 是否反转）。
- 是否真的有国产卡要接，具体型号——决定 §5 是现在留洞还是等需求出现。
- MPS v3 的 kernel 6.14+ / CUDA 13.4+ 门槛在内网是否可达；若不可达，则显存上限在近期内只能是 MIG 的离散档位。
- 出网白名单的注入方式选 OCI hook 还是入口包装（与 #16「工作区的安全与出网边界」重叠，应由那个 ticket 定）。

---

## Sources

一手来源（官方文档 / 上游仓库）：

- NVIDIA Container Toolkit — Specialized configurations（`--gpus`、`NVIDIA_VISIBLE_DEVICES`、`NVIDIA_DRIVER_CAPABILITIES`）：https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/latest/docker-specialized.html
- NVIDIA Container Toolkit — CDI support：https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/latest/cdi-support.html
- NVIDIA Container Toolkit — Install guide（rootless / `no-cgroups`）：https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/latest/install-guide.html
- NVIDIA MIG User Guide — Supported MIG profiles：https://docs.nvidia.com/datacenter/tesla/mig-user-guide/supported-mig-profiles.html
- NVIDIA MPS v3 — Memory Partitioning（`dmem.max` / `dmem.min`）：https://docs.nvidia.com/deploy/mps/latest/mpsv3-memory-partitioning.html
- NVIDIA MPS v3 — Configuration File（`active_thread_percentage` / `pinned_memory_limit`）：https://docs.nvidia.com/deploy/mps/latest/mpsv3-configuration-file.html
- CUDA Programming Guide §5.2 Environment Variables（`CUDA_VISIBLE_DEVICES`、`CUDA_DEVICE_ORDER`）：https://docs.nvidia.com/cuda/archive/13.3.0/cuda-programming-guide/05-appendices/environment-variables.html
- Docker — Resource constraints（CPU/memory/OOM）：https://docs.docker.com/engine/containers/resource_constraints/
- Docker — `docker run` reference（`--gpus`、`--device`、CDI）：https://docs.docker.com/reference/cli/docker/container/run/
- Docker — Rootless mode：https://docs.docker.com/engine/security/rootless/
- Docker — Rootless troubleshooting（"仅 cgroup v2 + systemd 支持"、"`--cpus`/`--memory`/`--pids-limit` 在 cgroup v1 被忽略"）：https://raw.githubusercontent.com/docker/docs/main/content/manuals/engine/security/rootless/troubleshoot.md
- Docker — Rootless tips（Limiting resources、`user@.service` `Delegate=` drop-in、systemd 244）：https://raw.githubusercontent.com/docker/docs/main/content/manuals/engine/security/rootless/tips.md
- rootlesscontaine.rs — Docker / cgroup v2（RootlessKit 3.0 源 IP、slirp4netns/VPNKit/pasta/gvisor-tap-vsock 选择顺序）：https://rootlesscontaine.rs/getting-started/docker/ 、 https://rootlesscontaine.rs/getting-started/common/cgroup2/
- Podman — `podman run` reference（`--cgroups`、资源限制的 cgroup v1 rootless 限制、`--device`、`--device-cgroup-rule`、`--network`、`--userns`）：https://docs.podman.io/en/latest/markdown/podman-run.1.html
- Podman — `podman network create`（`--internal`、`--driver`、`isolate=strict`）：https://docs.podman.io/en/latest/markdown/podman-network-create.1.html
- Podman — `podman network` 概述（Netavark、aardvark-dns、pasta 默认）：https://docs.podman.io/en/latest/markdown/podman-network.1.html
- Podman — rootless 已知缺陷：https://raw.githubusercontent.com/containers/podman/main/rootless.md
- containers/storage — `storage.conf`（overlay `size` / `inodes`、XFS project quota 要求）：https://raw.githubusercontent.com/containers/storage/main/docs/containers-storage.conf.5.md
- Linux kernel — Control Group v2：https://docs.kernel.org/admin-guide/cgroup-v2.html
- systemd — `systemd.resource-control`（`DeviceAllow=` / `DevicePolicy=` 的 eBPF 强制、`CPUQuota=`、`MemoryMax=`、`TasksMax=`、`IOReadBandwidthMax=`）：https://man7.org/linux/man-pages/man5/systemd.resource-control.5.html
- Container Device Interface（spec 目录、docker/podman 版本支持矩阵）：https://github.com/cncf-tags/container-device-interface
- HAMi — 支持的加速器设备与能力矩阵：https://project-hami.io/docs/next/userguide/device-supported
- nvidia-container-toolkit issue #189（rootless cgroup v2，1.18.0 修复）：https://github.com/NVIDIA/nvidia-container-toolkit/issues/189
- nvidia-container-toolkit issue #85（rootless 与 rootful 的 `no-cgroups` 冲突）：https://github.com/NVIDIA/nvidia-container-toolkit/issues/85

> 注：CDI 默认开启的版本号，CDI 项目 README 写 Docker **28.2.0**，docker 自有文档写 **28.3.0**。二者取更保守的 28.3.0。另：昇腾、海光等国产卡的容器文档为厂商站点动态渲染页面，本次未能取得稳定可引的一手 URL，§5 的定性结论依据 HAMi 的支持矩阵与厂商文档摘要，标注为**待实现时复核**。
