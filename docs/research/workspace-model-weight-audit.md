# Workspace 模型的分量：独立实体值不值得，哪里可以减

调研日期 **2026-11-17**。问题：Workspace 是独立于 Sandbox 的一等实体：Node 本地目录，bind mount 到 `/workspace`，终身绑定一个 Node，分 persistent 和 ephemeral，有单写者规则，有 declare-lost 和 abandon，可以 publish 成 Resource（GLOSSARY「Workspace」，`GLOSSARY.md:11-13`）。用户的感受是「很重，甚至累赘」：它不能在节点之间同步，里面还放着 Git 仓库；Workspace 页面不能浏览文件，必须进 Sandbox 页面，可它又单独占着侧边栏的一项（只读文件 API 已开 #273）。

已有调研回答的是「怎么让 Workspace 跨节点」（WSS §4.6、WCG）。本文回答另一个问题：**Workspace 作为独立实体，这个模型本身值不值得，是不是过重。** 做了三件事：(1) 补市场对照里没覆盖的事实：各家把可写文件系统建模成什么，有没有「独立、可多挂载、绑节点」的目录实体，Git 放在哪，这个实体有没有一级入口，沙箱不运行时能不能浏览；(2) 读 orrery 代码，盘点 Workspace 带来的全部机制和成本；(3) 给出选项和推荐。

只用一手来源：官方文档、钉死 commit 的源码。厂商原文保留英文。没有做任何实测。

| 项目 | 钉死 | 简写 |
|---|---|---|
| orrery（本仓库） | `main` @ `3b13e0a`（2026-11-17 会话开始时的 HEAD） | 直接写 `path:line` |
| kubernetes/website | commit `f6a84fbda31264f40615872a89ce1e55dfb2168f`（2026-11-17 取） | `K8S/` = `https://github.com/kubernetes/website/blob/f6a84fbda31264f40615872a89ce1e55dfb2168f/content/en/docs/` |
| 厂商文档页 | 2026-11-17 抓取，没有版本号，只给 URL | — |

复用的既有调研，里面已有的事实只标出处、不再取证：`workspace-cross-node-and-git-storage-vendor-practice.md`（**WCG**，2026-11-14，未提交）、`workspace-shared-storage-s3.md`（**WSS**）、`workspace-wsfs-fuse-spike.md`（**WSFS**）、`persistence-vendor-practice.md`（**PVP**）、`recheck-persistence-vs-coder-daytona.md`（**RPC**）、`recheck-runtime-and-workspace-vs-coder-daytona.md`（**RRW**）、`daytona-sandbox-runtime-semantics.md`（**DSR**）、`vendor-file-apis.md`（**VFA**）、`sandbox-git-integration.md`（**SGI**）、`system-feature-gap-vs-coder-daytona.md`（**SFG**）、`decisions-vs-vendor-practice.md`（**DVP**）、`code-vs-training-data-vendor-practice.md`（**CVT**）。

**来源说明**：这次新抓的一手页面是 Vercel Drives、Fly.io Volumes、Modal `volume` CLI 参考、Daytona Volumes、E2B Volumes（含 read-write 子页）和 Kubernetes 存储文档，都能打开。没有登录任何厂商控制台，所以控制台侧边栏长什么样**没有一手核对**，下文只引用文档里提到的控制台页面。

---

## 先读这几条

1. **「独立于沙箱、可被多个沙箱挂载的可写目录实体」不是 orrery 独有的，§1.1 表里八家沙箱厂商有六家有（没有的是 Cloudflare 和 Runloop）。** 这六家是 Vercel Drive、Daytona Volume、Modal Volume、E2B Volume、Blaxel Volume 和 Fly Volume，Kubernetes 的 PersistentVolume 也是这种形态。最接近 orrery 的是两家：**Vercel Drive**（"Only one sandbox at a time can mount a given drive as read-write"，多个只读挂载要走快照，绑定一个 region），以及 **Fly Volume 加 Kubernetes `local` PV**（"Each volume exists on one server"；"Pods that use a PV will only be scheduled to nodes that are selected by the node affinity"，用 `ReadWriteOncePod` 保证单写者）。orrery 的 persistent Workspace 在模型上和它们同构，不算发明（§1）。
2. **orrery 和所有这些厂商真正不同的地方，是每个 Sandbox 都必须挂一个 Workspace 实体。** 每家厂商都有一份**属于沙箱自己**的文件系统（E2B、Daytona、Vercel 的持久沙箱、Runloop 跨 stop 保留，Cloudflare、Fly 随实例消失），Volume、Drive 只是**可选的附加盘**。orrery 的 Runtime 在 stop 时整个删掉（`docs/adr/0009-runtime-provider-contract.md:22`，`docs/spec/runtime.md:109`），能跨 stop 留下来的只有 Workspace。于是每个 Sandbox 都隐式建一个 ephemeral Workspace 行（`internal/api/sandboxes.go:526-540`），它也会出现在 `GET /workspaces` 和 Workspaces 页面里（`internal/store/queries.sql:621-627` 不按 kind 过滤）。用户在侧边栏看到的「Workspace」，大多数就是 Sandbox 的影子。**「累赘感」主要来自这一点，不是来自 persistent Workspace 这个概念本身**（§2.3）。
3. **为一个 `mkdir` 建了一整套状态机。** Workspace 有 6 个状态，provisioning 要走一条独立的 Node 命令（`ProvisionWorkspace` → `WorkspaceObserved`），带 5 次退避、`provisioning_stalled` 条件和 `failed` 终态（`internal/api/node_workspace_retry.go:19-57`，`docs/spec/defaults.md:31`）。Agent 在这条命令里做的事是 `MkdirAll` 加 `Chmod` 加 `chown`（`internal/agent/runtime.go:248-259`）。`CreateSandbox` 在 `prepareSandboxHost` 里又把同一个目录 `MkdirAll` 了一遍（`internal/agent/runtime.go:270-277`），admission 也允许 Workspace 还在 `provisioning` 时就放行 Sandbox（`internal/api/workspaces.go:57-59`）。这一整套可以删，结果不变（§2.1 M2）。
4. **persistent Workspace 的放置不看它将来要跑什么。** `POST /workspaces` 之后，Reconciler 把它绑到**第一台** ready 且已连接的 Node 上（`internal/api/reconcile.go:486-503`），不看 GPU、容量，也不看 Snapshot 兼容性。之后挂它的 Sandbox 只能去那台（`internal/api/admission.go:192-193`、`:422`），那台不满足就是 `unsatisfiable`。【推断，未实测】在 GPU 和 CPU 混合的集群里，「先建 persistent Workspace，再建 GPU Sandbox」有可能永远排不上。ephemeral Workspace 没有这个问题：它在 Sandbox 被 admission 时才一起绑 Node（`internal/api/admission.go:496-498`）。
5. **「绑节点」不是 Workspace 单方面的成本：Sandbox 自己也是终身绑节点的。** `sandboxes.node_id` 只在为空时写入（`internal/store/queries.sql:467-470`），代码里没有任何地方清空它，`tryAdmit` 的注释写的就是 "A Sandbox bound to a Node stays there"（`internal/api/admission.go:391-392`）。所以就算把 Workspace 并进 Sandbox，跨节点问题也一点不会变少。反过来，WSS §4.4 列出的 Control Plane 变更只放松了 `workspaces.node_id`，**漏了 `sandboxes.node_id`**：§4.6 要换 Node 恢复，这一列也得改（§3.4）。
6. **Git 和模型选择无关。** 所有厂商都把 Git 仓库放在沙箱自己的盘或可写卷的**普通目录**里。Vercel 明说 "Cloned repositories … belong on a drive"，E2B 明说 Git 要在本地盘上做（CVT §1.2）。没有一家单独存 `.git`（WCG 先读第 1 条）。orrery 不管仓库（ADR-0013），选项 A、B、C 对 Git 的影响都一样：仓库就是目录里的文件（§1.3、§3）。
7. **「沙箱不运行时能浏览文件」是独立实体这一派的常见能力，orrery 恰好缺了它。** Modal（`modal volume ls/get/put/rm`，参数是卷名，不是容器）和 E2B（Volume 的 SDK 方法 "are meant to be used when the volume is not mounted to any sandbox"）都能不经沙箱读写卷；Vercel 明说做不到（"You currently need to mount a drive into a sandbox to read and write files"）；Daytona 的 Volumes 页没写任何卷级文件 API。所以 #273 补的是独立实体的一项标配能力，不是额外功能（§1.4）。
8. **推荐 C：「Sandbox 自带盘 + 可选的 persistent Workspace」**，也就是 Vercel 的形态。ephemeral Workspace 从用户可见的实体里拿掉，降级为 Sandbox 的盘。persistent Workspace 保留，它的列表和页面只显示 persistent 的那些，并加上 #273 的只读浏览。provisioning 状态机删掉，persistent Workspace 改成在第一个 Sandbox 被 admission 时才绑 Node。这条路不动数据模型，§4.6 照常能做，也不改 Git。B（彻底取消 Workspace 实体）省下的代码更多，但会丢掉「多个只读挂载」和「换镜像换 GPU 时保留数据」这两个确实存在的用例，除非先补一个「从停止的 Sandbox 改 Snapshot 和资源」的操作（§3）。

---

## 1. 市场对照：可写文件系统建模成什么

### 1.1 总表

「独立实体」= 生命周期独立于任何一个沙箱、有自己的 ID 和 API 的可写目录或卷。

| 平台 | 沙箱自己的可写 FS 跨 stop 吗 | 独立的可写实体 | 多挂载 / 写者规则 | 绑定范围 | Git 放哪 | 实体的 API / 控制台入口，沙箱不运行时能否读文件 |
|---|---|---|---|---|---|---|
| E2B | 跨 pause：保存文件系统和内存，"A paused sandbox is kept indefinitely"；`kill` 之后没了（VFA §2.1） | Volume（private beta）："Persistent storage that exists independently of sandboxes and can be mounted across multiple sandboxes"（<https://docs.e2b.dev/volumes.md>） | 能同时挂多个沙箱；锁 "can hang"；没有只读挂载（CVT §1.2） | 文档没写 | "Run builds, package installs, and Git operations on local disk, then copy the output to the volume."（CVT §1.2） | Volume 有自己的 SDK：`readFile`/`writeFile`/`list`（<https://docs.e2b.dev/volumes/read-write.md>），而且 "The SDK methods are meant to be used when the volume is not mounted to any sandbox"（<https://docs.e2b.dev/volumes.md>）：**能** |
| Daytona | container 类保留，GPU 类 "deleted on stop"（DSR §1.5） | Volume：FUSE 加 S3（DSR §1.6） | 多挂载，"the last write wins"；只能在创建沙箱时挂（"Mount a volume to a sandbox at sandbox creation time"，<https://www.daytona.io/docs/en/volumes/>） | 组织 / region，不绑 runner | 沙箱 FS；Git 动词是平台 API（SGI B.5）；Volume 页的用途清单里没有代码（<https://www.daytona.io/docs/en/volumes/>） | 控制台有 Volumes 页（文档写 "You can view your current volume usage in the Daytona Volumes dashboard"）；Volume 页面**没有**卷级文件 API，例子全是 `sandbox.fs.*` |
| Modal | Sandbox 退出就没了，要靠 Filesystem / Exit Snapshot（WCG §1.2） | Volume：提交制，last write wins（PVP §1.1） | 多写 | "distributed by default"（CVT §1.3） | 文档建议应用代码和系统依赖分开，代码用 Directory Snapshot（WCG §1.2）；Volume 定位是 "write-once, read-many"（CVT §1.3） | `modal volume ls/get/put/rm/cp` 的参数是卷名，另有 `modal volume dashboard`："Open the Volume's dashboard page in your web browser"（<https://modal.com/docs/reference/cli/volume>）：**能**【推断：这些命令不需要运行中的容器，页面没有写任何前提】 |
| Vercel Sandbox | **默认持久**："when a sandbox stops, the SDK snapshots its filesystem and restores it on the next resume"（VFA §2.4） | **Drive**："persistent storage that you mount into a sandbox as a working directory"（<https://vercel.com/docs/sandbox/concepts/drives>） | "Only one sandbox at a time can mount a given drive as read-write: use snapshots for shared access"；可以 fork（同上） | 一个 region："A drive's region can't change after creation"（同上） | "Cloned repositories, generated files, datasets, build output, dependency caches, logs, and agent workspace state usually belong on a drive"（同上） | 有 `Drive.list`，能看到 `currentSandboxName`；但 "You currently need to mount a drive into a sandbox to read and write files"（同上）：**不能** |
| Cloudflare Sandbox | "Stopping the instance deletes the files on its disk"（VFA §2.6） | 没有自己的卷，只有 R2 挂载和 `DirectoryBackup`（VFA §2.6） | R2 挂载不能并发写（PVP §1 表） | — | 实例盘 | `Files` "does not start the instance"（VFA §2.6）：**不能** |
| Runloop | suspend 时给磁盘拍快照，resume 时恢复（CVT §1.5） | 没有可写卷；Storage Object 在创建时复制进去（CVT §1.5） | — | — | Code Mount 在创建时 git clone 到 devbox 盘上（CVT §1.5） | 未找到 |
| Blaxel | 文档没写（VFA §2.8） | Volume："survives sandbox destruction and recreation"，"can only be attached to one sandbox at a time"（PVP §1.5） | 只能挂一个沙箱 | 文档没写 | 文档没写 | 未找到 |
| Fly.io Machines | Machine 的 rootfs 不是持久面（VFA §2.7，原文未明写） | **Volume**："a Machine can be destroyed without destroying its volume"（<https://docs.fly.io/volumes/overview>） | "a volume can be attached to only one Machine" | **一台物理机**："Each volume exists on one server in a single region"，"it's tied to that hardware"，挂在 "the same physical server as the Machine on which it's mounted"（同上） | 没有专门说法 | 没有文件 API（VFA §2.7） |
| Coder | stop 时容器删掉，volume 保留（RRW R4） | 没有平台实体；卷是模板里的 Terraform 资源（CVT §1.4） | 由模板决定 | Docker 模板绑宿主（RRW W2） | home 卷 | 文件 API 在 workspace 里的 agent 上（SFG §1.3）【推断：workspace 停止后不能浏览】 |
| GitHub Codespaces | `/workspaces` 跨 stop **也跨 rebuild**；`/workspaces` 之外的文件 rebuild 时丢（CVT §1.6） | 没有；codespace 就是整个环境 | — | — | `/workspaces` | 不启动 codespace 也能把当前分支导出到远端（WCG §1.1） |
| Kubernetes（参照） | `emptyDir`："When a Pod is removed from a node for any reason, the data in the `emptyDir` is deleted permanently"（`K8S/reference/storage/volume-types.md` L89-L93） | PersistentVolume："a lifecycle independent of any individual Pod that uses the PV"（`K8S/concepts/storage/persistent-volumes.md` 引言） | `ReadWriteOncePod`："ensure that only one pod across the whole cluster can read that PVC or write to it"；`ReadOnlyMany`（同上 Access Modes） | `local` PV："Pods that use a PV will only be scheduled to nodes that are selected by the node affinity"；"If a node becomes unhealthy, then the `local` volume becomes inaccessible to the Pod … as well as potential data loss"（同上 Node Affinity；`volume-types.md` local 一节） | — | 无文件 API |
| **orrery** | **不跨**：Stop 删掉 Runtime（`docs/adr/0009-runtime-provider-contract.md:22`）；能跨 stop 的只有 Workspace | **每个 Sandbox 都有一个**：ephemeral 隐式创建，persistent 显式创建（ADR-0018） | 至多一个 rw，可以多个 ro（`internal/store/migrations/000031_drop_attachments.up.sql`） | 一个 Node | Workspace 目录（ADR-0013 不管仓库） | 侧边栏一级入口；文件 API 只在 `/sandboxes/{id}/files` 下，而且要求 `ready`（`docs/spec/api.md:246`）：**不能**（#273 要改） |

### 1.2 从表里读出来的结论

- **两派都成立，而且大多数厂商两样都给。** 沙箱自己的 FS 管「这个沙箱的工作现场」，独立卷管「跨沙箱、跨镜像的数据」。Vercel 的文档把分工写得最清楚："Think of a snapshot as the computer you want to start, and a drive as the files you want to work on"，又说 "This lets you reuse the working files with a different starting environment later"（<https://vercel.com/docs/sandbox/concepts/drives>）。这正是 orrery persistent Workspace 的用途。
- **「终身绑一台物理机」的独立卷有先例，而且是成熟系统的做法**：Fly Volume 和 Kubernetes `local` PV。两家都把「节点坏了数据就可能丢」写进了文档（Fly："tied to that hardware"；Kubernetes："potential data loss"）。Fly 的应对是建议 "Always provision at least two volumes per app"，不做迁移。orrery 的 ADR-0014「Node 丢失由 admin 宣告」与这两家的立场一致。
- **单写者规则也有先例**：Vercel 的 "one read-write mount, many read-only mounts via snapshots"，以及 Kubernetes 的 `ReadWriteOncePod`。orrery 的部分唯一索引 `sandboxes_workspace_rw` 是同一条约束。不同之处在于，Vercel 的只读挂载是**时间点快照**（"does not include any writes made to the drive thereafter"），orrery 的 ro 挂载看到的是**活目录**，写者写到一半的文件也看得到。WSS §4.6 已经打算把 ro 挂载改成「恢复某个 Generation 作只读目录」，那样就和 Vercel 一样了。
- **没有一家把「沙箱自带的盘」也做成一个用户可见的独立实体。** 有卷的厂商，卷都是**可选**的，沙箱不挂卷照样有可写 FS。orrery 的 ephemeral Workspace 恰恰是把「沙箱自带的盘」做成了实体：它有自己的行、状态机、列表项、详情页和删除流程。这一点**没有先例**。【推断：Kubernetes 的 generic ephemeral volume 会为 Pod 生成一个 PVC 对象，算是半个先例；本文没有打开那一页核对，不据此下结论。】

### 1.3 Git 放在哪

| 位置 | 谁这么做 | 来源 |
|---|---|---|
| 沙箱自己的盘，沙箱内 clone | E2B、Daytona、Modal（代码从 readiness probe 里 `git pull`）、Runloop（Code Mount 在创建时 clone）、Cloudflare | CVT §1.2–§1.5；SGI B.5 |
| 平台在宿主侧 clone，再把目录挂进容器 | Codespaces（`/workspaces`）、Gitpod Classic | SGI B.1；WCG §1.2 |
| 可写的独立卷 | Vercel（官方推荐放在 Drive 上）、Coder（home 卷）、Ona（`/workspaces` 数据盘） | <https://vercel.com/docs/sandbox/concepts/drives>；CVT §1.4；WCG §1.2 |
| 明确**不要**放在网络卷上 | E2B（"Run … Git operations on local disk"）、Daytona（FUSE 卷 "generally slower"） | CVT §0 第 1 条 |
| 单独存一份 `.git` | 只有 Replit，而且存的是冗余副本 | WCG 先读第 4 条 |

对 orrery 的含义：Git 仓库在 Workspace 目录里，这个位置本身就是多数派做法，Vercel 和 Codespaces 正是这么放的。Git 的「重」来自跨节点：它要求本地 POSIX 性能（WSFS），换机器后 index 要刷新（WCG §2）。这些问题由 §4.6 解决，与 Workspace 是不是独立实体无关。

### 1.4 一级入口，以及不运行时能不能读文件

- **有独立实体的厂商，实体都有自己的 API 资源**：Vercel `Drive.list/get/delete/fork`、Daytona `POST /api/volumes`、Modal `modal volume …`、E2B `Volume.*`。控制台方面，Daytona 和 Modal 的文档都提到了专门的 Volumes 页面（见 §1.1 表中的引文）。Vercel 和 E2B 的控制台没有核对。所以「Workspace 单独占一个侧边栏入口」本身符合惯例，**前提是那里列的都是用户主动建的卷**。
- **不经沙箱读文件**：Modal 和 E2B 能；Vercel 文档明说不能；Daytona 没写。ADR-0018 拒绝 host-side 访问时写的理由是 "neither vendor offers it"，这里的 vendor 指 Coder 和 Daytona。放到更大的样本里，这个说法不成立。#273 把读和写分开（只读不经过单写者），这和 E2B 的「卷没挂在任何沙箱上时用 SDK 读写」比起来更保守，可以接受。

---

## 2. 审计 orrery 现状

### 2.1 Workspace 带来的机制，逐条判定

判定分三种：**必要**（换哪种模型都得留）、**并入 Sandbox**（只为 ephemeral 存在，等于 Sandbox 的盘）、**可删**（换不换模型都没必要）。

| # | 机制 | 代码位置 | 解决什么 | 判定 |
|---|---|---|---|---|
| M1 | `workspaces` 表：`id, owner_id, kind, sandbox_id, node_id, state, cause, storage_handle, conditions, generation, idempotency_key, deleted_at` | `internal/store/migrations/000003_sandboxes.up.sql:38-55`；`000007`、`000018`、`000026` 加了索引 | 持久化 Workspace 的身份和位置 | **必要**（给 persistent 用）。`storage_handle` 除生成代码外没有任何读写（`grep -rn StorageHandle internal` 只在 `queries.sql.go`、`models.go` 里出现），是**死列，可删** |
| M2 | provisioning 流程：`ProvisionWorkspace` 命令、`WorkspaceObserved{ready}`、5 次退避、`provisioning_stalled`、`failed/provisioning_failed` | `internal/api/reconcile.go:454-484`；`internal/api/node_workspace_retry.go:19-57`；`internal/api/node_observations.go:162-208`；`internal/agent/command.go:105`；`docs/spec/defaults.md:31`；ADR-0015「Workspace」第 1 条 | 在 Node 上建目录 | **可删**。Agent 只做 `MkdirAll` 加 `Chmod` 加 `chown`（`internal/agent/runtime.go:248-259`），`CreateSandbox` 的 `prepareSandboxHost` 又做一次（`:270-277`），admission 也不等它 ready（`internal/api/workspaces.go:57-59`）。Workspace 改成创建即 `ready`，目录由 `CreateSandbox` 建，就能删掉这一套 |
| M3 | persistent 的提前放置 `placeWorkspace` | `internal/api/reconcile.go:486-503` | 让 `POST /workspaces` 之后就有 `node_id` | **可删，改成懒绑定**：照 ephemeral 的做法，在第一个 Sandbox 被 admission 时再绑（`internal/api/admission.go:496-498` 已经是这段逻辑）。现在的做法不看容量和 GPU（先读第 4 条）。懒绑定的代价：从没挂过的 persistent Workspace 没有 `node_id`，对它 publish 应该返回 `conflict`（目录是空的） |
| M4 | 删除：`deleting` → `DeleteWorkspace` 命令 → `WorkspaceObserved{gone}` → `deleted`；ephemeral 在 Sandbox 删除时走 `finalizeAction` 分支 | `internal/api/workspaces.go:151-196`；`internal/api/reconcile.go:593-633`；`internal/api/lifecycle.go:113-121` | Node 上 `rm -rf`，确认后才报 deleted（RRW W10） | **必要**。ephemeral 那一支本来就是「Sandbox 删除时清盘」，**并入 Sandbox** |
| M5 | `abandon`（admin） | `internal/api/workspaces.go:261-299`；ADR-0015「Workspace」第 2 条 | 存活 Node 上删不掉的目录（Coder 的 orphan delete，RRW W10） | **必要**，但很少用到 |
| M6 | `declare-lost`（admin，单个 Workspace） | `internal/api/workspaces.go:198-259`；`internal/store/queries.sql:1503-1521`；ADR-0017 | 存活 Node 上存储坏了：挂它的 Sandbox 变 `failed/workspace_lost` | **必要**。对 ephemeral 来说，它等于「宣告这个 Sandbox 的盘丢了」，**并入 Sandbox** 也说得通 |
| M7 | 单写者：部分唯一索引 `sandboxes_workspace_rw`，加上 stop 和 failed 时把 mode 改成 `ro` | `internal/store/migrations/000031_drop_attachments.up.sql`；`internal/store/queries.sql:444-448`；`internal/api/sandboxes.go:836-838` | 防止两个写者同时改一个 Git 工作树（RRW W5） | **必要**（给 persistent 用）。ephemeral 只会有一个 Sandbox，这条约束对它永远成立。代价只是一条索引，#220 已经把它削到最小 |
| M8 | 调度约束：Sandbox 只能去 Workspace 所在的 Node；paused Node 允许已绑定的 Workspace 起 Sandbox | `internal/api/admission.go:182-196`、`:323-333`、`:415-422`、`:448` | 本地目录不能跨 Node | **必要**，而且和 Sandbox 自己绑 Node（`internal/store/queries.sql:467-470`）是同一回事。对 ephemeral 来说这条是冗余的：Sandbox 已经绑在那台上 |
| M9 | Node 生命周期级联：删除 Node 前数非终态 Workspace；Node lost 时 Workspace 变 `lost` 或 `deleted`，pending 的 publish 失败 | `internal/api/nodes.go:215-226`、`:343-346`；`internal/store/queries.sql:1430`、`:1478-1498` | Node 退役和丢失时不留孤儿 | **必要** |
| M10 | ephemeral 的隐式创建，以及 ADR-0018 的「随 Sandbox 删、不设宽限窗」 | `internal/api/sandboxes.go:521-548`；ADR-0018 | 让 stop/start 保留文件 | **并入 Sandbox**。用户提到的「ephemeral grace window」**已经不存在了**：ADR-0018 删掉了它，代码里也搜不到相关逻辑 |
| M11 | Workspace 列表包含 ephemeral | `internal/store/queries.sql:621-627`；`web/src/routes/_authed.workspaces.index.tsx` | — | **UX 缺陷**：每个 Sandbox 在 Workspaces 页面里都会再出现一次。只列 persistent 就够了 |
| M12 | publish：`POST /workspaces/{id}/publish`，不要求运行中的 Sandbox，Agent 直接读宿主目录 | `internal/api/publish.go`（254 行）；`internal/agent/publish.go`（152 行）；`docs/spec/node-protocol.md:70-72` | 把结果从绑节点的目录搬进对象存储（RPC A1；ADR-0014 的退役路径） | **必要**。换模型的话，只需要把路由挂到 Sandbox 下 |
| M13 | Agent 侧：`<data-root>/workspaces/<id>`；`<data-root>/sandboxes/<id>/workspace` 引用文件，让文件 API 在 Agent 重启后还能找到目录 | `internal/agent/runtime.go:212-214`；`internal/agent/files.go:113-140`、`:579-616` | Sandbox 到目录的映射 | **必要**，量很小 |
| M14 | 文件 API 挂在 Sandbox 下，而且要求 `ready` | `internal/api/routes.go:101-109`；`docs/spec/api.md:246`；ADR-0018 | 写必须经过单写者 | 读的部分**应改**，就是 #273 |
| M15 | Web：侧边栏一项，列表页和详情页 | `web/src/routes/_authed.tsx:70-75`；`_authed.workspaces.index.tsx`（146 行）；`_authed.workspaces.$id.tsx`（203 行）；`web/messages/zh.json:448` | 管理 persistent | **保留但收窄**：只列 persistent，详情页加只读浏览 |
| M16 | CLI：只有 `workspace publish/declare-lost/abandon`，**没有** create/list/delete | `internal/cli/workspace.go:12-45`；`internal/cli/cli.go:160-163` | — | 记一笔：CLI 上 persistent Workspace 其实没法建，只能走 Web 或 REST |

### 2.2 代码量（估算）

方法：`wc -l` 统计专属文件；混合文件用 awk 截出函数名含 `Workspace` 的函数（从 `^func` 到下一个 `^}`）；store 层数 `queries.sql` 里名字含 Workspace 的命名查询。全部是非生成代码，测试另计。

| 部分 | 量 | 说明 |
|---|---|---|
| Control Plane 专属 | 约 650 行 | `workspaces.go` 333、`node_workspace_retry.go` 57、Reconciler 的 `workspaces/placeWorkspace/deleteWorkspaces/finalizeWorkspace` 88、`recordWorkspaceObserved` 47、admission 和 Sandbox 创建里与 Workspace 相关的分支约 120【粗估】 |
| publish | 406 行 | `api/publish.go` 254 加 `agent/publish.go` 152。不管选哪个方案都保留 |
| Agent 专属 | 约 80 行 | provision/delete/observe 约 24，引用文件约 58 |
| store | 24 个命名查询（共 233 个），5 个迁移涉及 | `grep -c` |
| Node 协议 | 4 条消息：`ProvisionWorkspace`、`DeleteWorkspace`、`WorkspaceObserved`、`PublishWorkspace` | `proto/node/v1/node.proto` |
| Web | 349 行页面加少量 lib | 两个路由文件 |
| CLI | 73 行 | `internal/cli/workspace.go` |
| 测试 | 约 560 行 | `workspace_rw_db_test.go` 210、`workspaces_test.go` 62、`e2e/workspace_test.go` 247（其中约一半测 Idle Stop）、`e2e/multinode_test.go` 的一段 |
| 参照 | 全部非生成 Go 代码约 32.7k 行，其中 582 行提到 workspace（不区分大小写） | |

结论：**Workspace 专属的生产代码约 1,100 行（不含 publish），大约占 3%。** 规模上算不上沉重。重的是**概念面**：6 个状态，2 个 kind，3 条 admin 动作，4 条 Node 消息，一个侧边栏入口，以及「每个 Sandbox 都有一个影子 Workspace」。按方案 C 能删的部分（M2、M3、`storage_handle`，以及把 ephemeral 从列表和页面里拿掉）约 250–300 行，概念上少掉 provisioning、`provisioning_stalled`、`failed` 三个，用户可见的 Workspace 数量降到只剩 persistent。

### 2.3 哪些用例真的需要「Workspace 独立于 Sandbox」

| 用例 | 现在靠什么 | 有没有更轻的办法 | 判断 |
|---|---|---|---|
| U1 Sandbox stop/start 保留文件 | ephemeral Workspace | 「Sandbox 自带一块跨 stop 的盘」就够了。这就是 ephemeral 的实际语义（ADR-0018），只是不必把它做成实体 | **不需要独立实体** |
| U2 删掉 Sandbox，数据留着 | persistent | (a) 删除前 publish 成 Resource（M12，已有）；(b) 不删，只 stop（Idle Stop 默认 15 分钟就会停，停着的 Sandbox 不占配额，ADR-0015） | 只为了「留着」，publish 或 stop 足够；要「留着并且以后继续在上面写」，就需要 U3 |
| U3 换 Snapshot 或换 CPU/GPU 规格，数据接着用 | persistent：新 Sandbox 挂同一个 Workspace | orrery **没有**修改 Sandbox 的路由（`internal/api/routes.go` 里 `/sandboxes/{id}` 只有 GET 和 DELETE），所以今天只能靠 persistent。更轻的办法是给**停止的** Sandbox 加一个「改 Snapshot 和资源」的操作，对应 Codespaces 的 rebuild：`/workspaces` 跨 rebuild 保留（CVT §1.6）。另一种办法是 Resource：publish 之后在新 Sandbox 里拷回来，代价是一次全量拷贝 | **这是 persistent 最硬的理由**。在 GPU 场景下很常见：先用 CPU 准备数据，再换 GPU 跑训练 |
| U4 一个写者加多个只读者同时看同一个目录 | ro 挂载 | publish 成 Resource，再以只读方式挂载（ADR-0030 懒加载，不需要全量拉取）。语义从「活目录」变成「时间点」，和 Vercel 的 drive snapshot 一样。WSS §4.6 本来也打算把 ro 改成 Generation | **可以替代**，而且替代后的语义更干净（读不到写了一半的文件）。代价：每次想看新内容都要 publish 一次 |
| U5 一个长期的「项目目录」，被一串短命 Sandbox 轮流写 | persistent 加单写者 | 用 U3 的 rebuild 来轮换 Sandbox 也可以，但「同一个目录、不同 Sandbox 名字和配置并存，交替启动」只有独立实体能直接表达 | Vercel 首页举的正是这个例子（"Agent workspace: store an agent workspace that persists across sandbox runs"）。**合理，保留** |
| U6 沙箱不运行时浏览、下载、publish | publish 已经不要求 Sandbox；浏览要靠 #273 | 实体在不在都能做：按 Sandbox 的盘做也行 | **与模型无关** |
| U7 Node 退役时搬走数据 | publish 到 Resource，再恢复（ADR-0014） | 同上 | **与模型无关** |

结论：真正需要独立实体的只有 **U3 和 U5**，它们都是 persistent 的用例。U1 是 ephemeral 的全部理由，但它并不需要一个实体。U4 可以用 Resource 替代。

---

## 3. 选项

### 3.1 A：保持模型，只修 UX 和明显的冗余

- **删**：M2 provisioning 状态机（Workspace 创建即 `ready`，目录由 `CreateSandbox` 建，只保留 `DeleteWorkspace` 这条 Node 命令）；M3 提前放置（改成懒绑定）；`storage_handle` 死列。
- **改**：`GET /workspaces` 默认只列 persistent（加一个 `kind` 过滤，或者干脆不返回 ephemeral）；Workspaces 页面只显示 persistent；Sandbox 详情页里的「Workspace」改叫「盘 / 文件」，ephemeral 的不再链接到 Workspace 详情；做 #273。
- **保留**：其余全部，包括 ephemeral 行（内部）、ro 活挂载和单写者。
- **迁移代价**：小。不需要数据迁移，只要把卡在 `provisioning` 的行改成 `ready`、`node_id` 为空的 persistent 行保持为空。
- **与 ADR 冲突**：ADR-0015「Workspace」第 1 条（provisioning 重试）和 `defaults.md:31` 要撤回；ADR-0018 的文件访问（#273 已经计划改）。
- **§4.6 和 Git**：没有影响。

### 3.2 B：取消 Workspace 实体，降级为 Sandbox 的持久盘

- **删**：`/workspaces` 全部 6 条路由，Workspace 状态机，`kind`，ro 挂载，单写者索引（一个盘只属于一个 Sandbox，不再需要），侧边栏入口和两个页面。
- **保留**：每个 Sandbox 一块跨 stop 的盘；publish 挪到 `POST /sandboxes/{id}/publish`；declare-lost 和 abandon 挪到 Sandbox 上；Node 级联。
- **必须新增**（否则 U3 和 U5 就没了）：(a) `POST /sandboxes/{id}/rebuild` 或 `PATCH`：只对 `stopped` 有效，改 `snapshot`、`cpu/memory/gpu`，盘保留；或者 (b) 创建 Sandbox 时带 `disk_from: <stopped sandbox id>`，把盘的归属**转移**过去，旧 Sandbox 随之删除。(a) 更简单，也有 Codespaces rebuild 的先例；(b) 在语义上就是「persistent Workspace 换了个名字」。
- **迁移代价**：中到大。现存的 persistent Workspace 要映射到某个 Sandbox；被多个 Sandbox 先后挂过的、或者正被 ro 挂着的，没有自然的映射。API 和 SDK 的 `workspace` 字段（`docs/spec/api.md:93`，`sdk/python/orrery/client.py:70`）是破坏性变更。还要改 GLOSSARY「Workspace」，ADR-0012 的 Rejected alternatives（"duplicates the Workspace"），ADR-0014 的退役路径，ADR-0015、0017、0018 和 #220 也都要改。
- **§4.6**：Generation 改成按盘（也就是按 Sandbox）编号，数据模型不变，只是键换了。跨 Node 恢复就是「Sandbox 换 Node」，`sandboxes.node_id` 本来也得放松（先读第 5 条）。
- **Git**：没有影响。但删除 Sandbox 时，仓库也随之没了，除非先 publish 或 rebuild。
- **省下的**：约 650 行 Control Plane 代码加 349 行页面，减去新增的 rebuild 约 100–200 行【粗估】。概念面省得最多。

### 3.3 C（推荐）：Sandbox 自带盘，persistent Workspace 作可选附加盘（Vercel Drive 形态）

- **对用户**：Sandbox 有自己的盘，stop/start 保留，删除时一起删（就是现在 ephemeral 的语义），但不再以「Workspace」的面目出现。Workspace 只指**用户主动建的 persistent Workspace**：侧边栏入口保留，只列这些，详情页有只读文件浏览（#273）、发布、删除和「用它新建 Sandbox」。
- **删**：A 的全部删除项（M2、M3、`storage_handle`）；ephemeral 从 `GET /workspaces` 和 Web 里消失；Sandbox DTO 里 ephemeral 的 `workspace` 字段改为不返回或只返回 mode【需要拍板：保留 `workspace.id` 对 MCP 和 SDK 的兼容更好】。
- **内部保留**：`workspaces` 表照旧存两种 kind。ephemeral 行就是「Sandbox 的盘」的实现，Generation（§4.6）、publish 和 declare-lost 都按它的 ID 走，不必改键。这是 C 比 B 便宜的根本原因：**改的是可见面，不是数据模型**。
- **publish**：给 Sandbox 加一个别名路由 `POST /sandboxes/{id}/publish`，内部转发到它的 Workspace，这样 ephemeral 的用户不必知道 Workspace ID。`/workspaces/{id}/publish` 保留给 persistent。
- **ro 挂载**：保留，但文档推荐用「publish 加 Resource 挂载」来共享（U4）。等 §4.6 落地时，再把 ro 改成按 Generation 挂。
- **迁移代价**：小。不迁移数据，API 只做加法；唯一的行为变化是列表不再返回 ephemeral。仓库里调 `GET /workspaces` 的只有 Web 的两处：Workspaces 列表（`web/src/routes/_authed.workspaces.index.tsx:39`）和新建 Sandbox 表单，后者本来就只取 persistent（`_authed.sandboxes.new.tsx:60-72`）；CLI、MCP、SDK 都没调。另有一处要跟着改：Resource 详情页的来源列把 `workspace{id}` 链到 Workspace 详情（`web/src/routes/_authed.resources.$id.tsx:320-332`），ephemeral 来源要改成链到对应的 Sandbox，或者不加链接。
- **与 ADR 冲突**：GLOSSARY「Workspace」要分出一句「Sandbox 的盘是它的 ephemeral Workspace，不单独列出」；ADR-0018 的「Ephemeral Workspace」一节补一句「不在 Workspace 列表中出现」；ADR-0015 撤回 provisioning 重试。ADR-0012、0014、0017 和 #220 都不动。
- **§4.6 和 Git**：没有影响。persistent 改成懒绑定，正好也是 §4.6「`node_id` 降级为缓存亲和 Node」的前一步。

### 3.4 三个方案都要处理的事

- **`sandboxes.node_id` 的永久绑定**（`internal/store/queries.sql:467-470`，`internal/api/admission.go:391-392`）：做 §4.6 时，这一列和 `workspaces.node_id` 要一起降级为「上次所在 Node」。WSS §4.4 没写这一条，应补上。
- **persistent Workspace 的 Node 选择**：改成懒绑定以后，第一个 Sandbox 的需求（GPU、Runtime、links）决定 Workspace 落在哪台 Node。后来的 Sandbox 如果需要另一种 GPU，还是会 `unsatisfiable`。在 §4.6 落地之前，这是本地盘模型的固有代价，应该写进文档，并在 `blocked_reason` 里说清楚原因。
- **磁盘配额**：#218 一直没做（RRW #3）。不管选哪个方案，它都是本地盘模型下最大的运维风险，而且与「实体轻不轻」无关。

### 3.5 推荐与理由

**推荐 C，同时把 A 的删除项一起做掉。B 暂缓，等 U3 有了 rebuild 操作再重新评估。**

1. 「累赘感」的来源是**每个 Sandbox 都有一个用户可见的影子实体**，还有一个 `mkdir` 状态机（§2.1 M2、M11），并不是 persistent Workspace 这个概念。C 正好把这两样拿掉。
2. persistent Workspace 有真实的用例（U3、U5），也有一致的外部先例：Vercel Drive 的单写者加只读快照，Fly 和 Kubernetes `local` PV 的绑节点。B 把它们删掉，就得补一个 rebuild 或盘转移操作，那个操作本质上就是在重新发明 persistent Workspace。
3. C 不动数据模型，§4.6 的 Generation、publish 和 declare-lost 都继续按 `workspace_id` 走，迁移基本为零，也不改 Git 的任何行为。
4. 如果将来真的给停止的 Sandbox 加了 rebuild（这对 GPU 场景本身就有价值），persistent Workspace 的使用率会自然下降，那时候再看 B 是否值得，届时迁移面也会更小。

---

## 4. 未确认、需要实测或需要拍板

**需要实测**
- persistent Workspace 被提前放到无 GPU 的 Node 上，之后的 GPU Sandbox 是否真的以 `unsatisfiable` 结束（先读第 4 条只是从代码推出来的）。
- 删除 provisioning 状态机之后，`CreateSandbox` 第一次建目录失败（磁盘满、权限问题）时的表现：要确认会以 `runtime_create_failed` 结束，而不是一直重试。
- ro 活挂载读到写了一半的文件：现状如此，本文没有验证 runsc 下的可见性。

**未能一手确认**
- Vercel、E2B、Daytona 控制台侧边栏的实际结构（没有登录）；Modal `volume` 命令是否真的不需要运行中的容器（页面没写前提）。
- Blaxel、Runloop 能否不经沙箱读卷或盘：没找到。
- Coder workspace 停止后能否浏览文件：文件 API 在 agent 里，推断不能，没有核对。
- Kubernetes generic ephemeral volume 是否给每个 Pod 生成一个可见的 PVC（§1.2 末条）：没有打开那一页。
- Coder 的「workspace 更新到新模板版本时保留卷」能否作为 B(a) 的先例：本文没有取证，只用了 Codespaces rebuild。

**需要用户拍板**
- C 里 Sandbox DTO 的 `workspace` 字段对 ephemeral 是否继续返回 `id`。为了 SDK 和 MCP 兼容，建议继续返回。
- 侧边栏的「Workspace」条目：没有 persistent 时是隐藏，还是显示一个空列表加说明；名字是否改成「持久工作区」，以便和 Sandbox 的盘区分。
- 是否给停止的 Sandbox 加 rebuild（改 Snapshot、改 CPU/GPU 规格）。这决定了 U3 能不能不靠 persistent，也决定了 B 将来有没有可能。
- ro 活挂载要不要直接废掉，改成「publish 加 Resource」。这会少一条语义（读写并发可见），换来更干净的时间点视图。

---

## 来源

**一手文档（2026-11-17 抓取）**
- Vercel Sandbox Drives：<https://vercel.com/docs/sandbox/concepts/drives>
- Fly.io Volumes overview：<https://docs.fly.io/volumes/overview>（由 `fly.io/docs/volumes/overview/` 301 跳转而来）
- Modal CLI `modal volume`：<https://modal.com/docs/reference/cli/volume>
- Daytona Volumes：<https://www.daytona.io/docs/en/volumes/>
- E2B Volumes：<https://docs.e2b.dev/volumes.md>、<https://docs.e2b.dev/volumes/read-write.md>
- Kubernetes（钉死 `f6a84fbda31264f40615872a89ce1e55dfb2168f`）：`K8S/concepts/storage/persistent-volumes.md`（Introduction、Access Modes、Node Affinity）、`K8S/concepts/storage/volumes.md`（引言）、`K8S/reference/storage/volume-types.md`（`emptyDir`、`local`）

**既有调研（只引出处）**
- WCG §1.1、§1.2、§2、先读第 1、4 条；WSS §4.4、§4.6；WSFS；PVP §1、§1.1、§1.4、§1.5；RPC 先读、「三件套是否过重」；RRW 先读第 3、5 条，R4、W2、W5、W10；DSR §1.5、§1.6；VFA §2.1–§2.8；SGI B.1、B.5；SFG §1.3、§1.5；CVT §0、§1.2–§1.6

**orrery 代码与文档（`3b13e0a`）**
- `GLOSSARY.md:11-13`
- `docs/adr/0009-runtime-provider-contract.md:22`；`docs/adr/0012-resource-artifact-and-snapshot-persistence.md:12`、`:38`；`docs/adr/0014-control-plane-and-node-operations.md:22-30`；`docs/adr/0015-execution-workspace-and-scheduling-recheck.md:14`、`:31`、`:38-40`；`docs/adr/0017-whole-system-completion.md:18`、`:48`；`docs/adr/0018-api-surface-ephemeral-workspace-and-tunnels.md`
- `docs/spec/api.md:69-71`、`:93`、`:246`、`:368-372`；`docs/spec/defaults.md:31`；`docs/spec/runtime.md:109`；`docs/spec/node-protocol.md:70-72`
- `internal/api/routes.go:72-77`、`:88`、`:101-109`；`internal/api/workspaces.go`；`internal/api/node_workspace_retry.go`；`internal/api/reconcile.go:454-503`、`:593-633`；`internal/api/node_observations.go:162-208`；`internal/api/lifecycle.go:113-121`；`internal/api/admission.go:182-196`、`:323-333`、`:391-422`、`:496-498`；`internal/api/sandboxes.go:447-548`、`:836-838`；`internal/api/nodes.go:215-226`、`:343-346`
- `internal/store/migrations/000003_sandboxes.up.sql:38-66`、`000007_workspaces.up.sql`、`000031_drop_attachments.up.sql`；`internal/store/queries.sql:444-448`、`:467-470`、`:600-666`、`:1430`、`:1478-1527`
- `internal/agent/runtime.go:212-214`、`:248-277`；`internal/agent/files.go:113-140`、`:579-616`；`internal/agent/command.go:105-120`、`:203`
- `proto/node/v1/node.proto`（`WorkspaceObserved`、`ProvisionWorkspace`、`DeleteWorkspace`、`PublishWorkspace`）
- `web/src/routes/_authed.tsx:70-75`；`web/src/routes/_authed.workspaces.index.tsx`；`web/src/routes/_authed.workspaces.$id.tsx`；`web/messages/zh.json:448`
- `internal/cli/workspace.go`；`internal/cli/cli.go:160-163`；`sdk/python/orrery/client.py:70`
- issue #273、#220、#218（`gh issue view` / `gh issue list`）
