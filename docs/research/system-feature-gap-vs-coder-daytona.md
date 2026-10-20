# 完整系统功能面对照：Coder 与 Daytona vs 我们

调研日期 **2026-10-17**。这里不逐票重查裁决（逐票重查见 `recheck-*.md`），只做一件事：把两家**用户可见的完整功能面**列全，再逐项对到我们的规格（`GLOSSARY.md`、ADR-0009–0015 的工作区版本、#30–#41 裁决原文），标出「我们有 / 明确排除 / 缺失且未提及」。只用一手来源，厂商原文保留英文，推断标【推断】。

钉死版本与链接简写：

| 简写 | 展开 |
|---|---|
| `C/` | `https://github.com/coder/coder/blob/c3c6a672821e8ac2472c45f3be7a3c600aa8dae1/`（tag v2.38.0） |
| `DT/` | `https://github.com/daytonaio/daytona/blob/v0.190.0/`（commit `01c502bb1f1ff8f2885d0cd490e043736083dca8`） |
| `DD/` | `DT/apps/docs/src/content/docs/en/`（Daytona 文档源码） |
| `DA/` | `DT/apps/api/src/`（Daytona 控制层 API 源码） |

**复用的既有调研**（下文不重复取证）：`recheck-identity-network-channel-vs-coder-daytona.md` G2（端口入口缺口的取证）、`daytona-sandbox-runtime-semantics.md`（Daytona 进程/会话/PTY/webhook/SSH）、`recheck-api-and-exec-vs-coder-daytona.md`（Exec 与契约）、`internal-consistency-audit.md`（与本文重叠的内部缺口：S1、M8、L12、L13、L15）。

---

## 0. 先读这几条

1. **最大的功能缺口是「沙箱里起的服务怎么被看到」，而且补它不需要推翻零监听端口。** 两家都把它当核心功能：Daytona 的 preview URL 覆盖 *"Any process listening for HTTP traffic on ports `1` - `65535`"*（`DD/preview.mdx#L8`），Coder 有 dashboard 端口代理、`coder port-forward`、SSH 转发、Coder Desktop 四种方式（`C/docs/user-guides/workspace-access/port-forwarding.md`）。两家都**不在沙箱网络上对外开入站口**。Coder 的 dashboard 代理最终拨的是 workspace 内的 `127.0.0.1:PORT`（`C/coderd/workspaceapps/request.go#L338`），通过 agent 的 tailnet 走过去。Daytona 的链路是 proxy → runner → daemon `/proxy/:port/*path`，daemon 再拨 `http://localhost:%s`（`DT/apps/daemon/pkg/toolbox/proxy/proxy.go#L25`、`DT/apps/proxy/pkg/proxy/get_sandbox_target.go#L99`）。我们照这个形状做就行：Gateway → Node Agent → 进 Sandbox netns 拨 `127.0.0.1:PORT`。但 ADR-0010 写的是 *"Sandboxes listen on no ports and are never connected into"*，后半句要改写，所以这一项列为**需要用户决策**（推荐补）。
2. **Agent 拿不到「平台现在什么状态」。** 我们没有 Sandbox 列表的过滤和标签，没有状态 watch 或长轮询，也查不到配额用量和剩余 GPU。两家这些都有：Coder 的 `owner:me status:running` 过滤语法（`C/docs/user-guides/workspace-management.md#L55-L85`）、`/workspaces/{id}/watch`（`C/docs/reference/api/workspaces.md#L2603`）；Daytona 的 `labels` 和二十多个列表过滤字段（`DA/sandbox/dto/list-sandboxes-query.dto.ts#L37-L315`）、`/organizations/:id/usage` 里的 `totalGpuQuota/currentGpuUsage`（`DA/organization/dto/organization-usage-overview.dto.ts#L33-L36`）。对一个**排队式**的 GPU 平台，这几样是 Agent 决定等还是换方案的依据，**必须补**。
3. **Tool API 只有名字，形态没定。** 两家都发 MCP server：Coder 有内建的 Streamable HTTP 端点 `/api/experimental/mcp/http` 和 `coder exp mcp server`（stdio）两种（`C/docs/ai-coder/mcp-server.md#L10-L16`、`#L86-L100`）；Daytona 是 `daytona mcp start`（`DD/mcp.mdx#L116`）。两家的工具都是 REST 的薄映射（`C/codersdk/toolsdk/toolsdk.go#L32-L69`）。我们的「Agent Tool API」在 ADR-0014 里只是 `api` 监听面上的一个模块，没有说是不是 MCP、跑在哪、PTY 怎么映射（另见 audit L15）。**需要用户决策**。推荐：MCP，工具从 OpenAPI 映射，Phase 1 走 CLI 的 stdio 子命令。
4. **SSH / IDE 接入完全没提。** 我们的地图目的地写的是「remote development」。两家都把 SSH 当一等入口：Coder 有 `coder ssh` / `coder config-ssh`，然后接 VS Code、JetBrains（`C/docs/user-guides/workspace-access/index.md#L29-L147`）；Daytona 有 token 制 SSH，经独立的 `ssh-gateway` 转到 runner（`DD/ssh-access.mdx#L132`、`DT/apps/ssh-gateway/main.go#L191-L281`）。我们只有 Gateway 上的 PTY WebSocket。**需要用户决策**：人是否要用本地 IDE 接进 Sandbox。
5. **Snapshot → Sandbox 这条路有三处断口：** 不用 Snapshot 时用哪个镜像（audit L12）；按 Snapshot 名字创建时怎么解析成 SnapshotVersion；构建日志怎么看。Daytona 的 Snapshot 有 `build-logs` 端点（`DA/sandbox/controllers/snapshot.controller.ts#L220`），状态里还有 `pulling`（`DD/snapshots.mdx#L1013-L1020`）。**必须补**前两条和构建日志；拉取进度可以先并进 `provisioning`。
6. **大量厂商功能我们有意不做，ADR 也写了理由：** pause、resize、fork、volume、组织/角色、网络热改、privileged/Docker-in-Sandbox、Git API。没写理由、但判为「可不做」的有：LSP、computer use、code interpreter、warm pool、webhook、用量历史、Web UI。理由都列在 §2。

---

## 1. 功能对照表

判定取值：**有**（附我们的出处）、**排除**（附排除它的决定）、**缺·必须补**、**缺·可不做**、**缺·需决策**。

### 1.1 Sandbox 生命周期

| 功能 | Coder | Daytona | 我们 | 判定 |
|---|---|---|---|---|
| 状态集合 | `pending/starting/running/stopping/stopped/failed/canceling/canceled/deleting/deleted`（`C/codersdk/workspacebuilds.go#L28-L37`） | 22 个，含 `pulling_snapshot`、`archived`、`paused`、`resizing`、`forking`、`error`、`build_failed`（`DA/sandbox/enums/sandbox-state.enum.ts#L6-L29`） | `provisioning, ready, stopping, stopped, deleting, deleted` + `unavailable` 条件（GLOSSARY「Sandbox State」） | **有**；但没有排队态、重启边和失败终态，audit S1 已经立项 |
| 创建 | `POST /users/{user}/workspaces`（`C/docs/reference/api/workspaces.md`） | `POST /sandbox`，字段有 name/snapshot/env/labels/public/network*/cpu/gpu/gpuType/memory/disk/auto*Interval/volumes/buildInfo/linkedSandbox（`DA/sandbox/dto/create-sandbox.dto.ts#L35-L231`） | 有（#34 admission bundle；ADR-0013 Secret 映射；ADR-0010 egress） | **有**；创建字段里缺 name、labels，见 1.6 |
| start / stop / restart | 作为 build transition（`POST /workspaces/{id}/builds`）；CLI `start/stop/restart` | `POST /sandbox/:id/start`、`/stop`（`DA/sandbox/controllers/sandbox.controller.ts`） | stop 有；restart 要重新 admission（#34），Provider 没有 `Start`（ADR-0009） | **有**；重启时哪些东西沿用、哪些重新解析，见 audit M9 |
| delete | 有 | `DELETE /sandbox/:id` | 有，ownership 授权，有冲突时不隐式取消（ADR-0010） | **有** |
| 自动停止（idle） | autostop + activity bump；活动来源是 VS Code、JetBrains、Terminal、SSH 会话（`C/docs/admin/templates/managing-templates/schedule.md#L19`） | `autostop/:interval`（`sandbox.controller.ts#L937`）；只认白名单列出的外部交互（`daytona-sandbox-runtime-semantics.md` §0 第 7 条） | Idle Stop：没有运行中的 Exec、没有活连接，默认 15 分钟（ADR-0015） | **有**，比 Daytona 合理 |
| 自动启动 / 定时启动 | autostart cron（`PUT /workspaces/{id}/autostart`） | 无 | 无 | **缺·可不做**：Agent 驱动，不按作息 |
| 延长 deadline / 上报活动 | `PUT /extend`、`POST /usage`（`C/docs/reference/api/workspaces.md#L2320`、`#L2566`） | `POST /sandbox/:id/last-activity`（`sandbox.controller.ts#L915`） | 无 | **缺·可不做**：Idle Stop 已经按 Exec 和连接判定，不需要手动续命 |
| 自动归档 | dormancy（Premium）（`schedule.md#L46-L61`） | `autoarchive/:interval`、`archive`，归档到对象存储（`sandbox.controller.ts#L979`、`#L1132`） | 无；Workspace 永远 Node 本地 | **排除**：#32 把 Workspace 绑在 Node 上，跨 Node/归档格式明确延后（地图 Not yet specified） |
| 自动删除 | dormancy auto-deletion（`schedule.md#L54-L61`） | `autodelete/:interval`、`ephemeral`（`DD/sandboxes.mdx#L710-L716`） | ephemeral Workspace 无 Attachment 时进宽限期后删除（#32）；persistent 只能显式删除 | **有**（Workspace 层）；persistent 无上限的问题见 audit M14 |
| pause（保内存） | 无 | `pause`（`sandbox.controller.ts#L571`） | 无 | **排除**：ADR-0009 Consequences 写明 pause 需要新决定 |
| resize | 无（改 template 参数后重建） | `resize`（`#L605`） | 无 | **排除**：#34 "Phase 1 不支持在线 resize" |
| fork / 克隆 | 无 | `fork`、`forks`、`ancestors`（`#L781`） | 无 | **缺·可不做**：要复制可以开一个 Exec 去 cp/tar，Workspace 跨 Sandbox 本来就能共享（只读多挂） |
| recover（从 error 恢复） | 无（重新 build） | `POST /sandbox/:id/recover`（`#L431`） | Reconciler 收敛，`provisioning_stalled` 可删（ADR-0015） | **有**（形态不同） |
| 备份 | 无 | `backup`（`#L717`） | 无 | **排除**：同 archive；Node 退役靠 Exec 打包，再声明成 Artifact（ADR-0014） |
| 多运行时 / Windows | 由 template 决定 | Windows sandbox（`DD/sandboxes.mdx#L412`） | Docker/Podman/Local（ADR-0009） | **有**（Linux 范围） |
| 关联沙箱 / 沙箱间网络 | 无 | linked sandboxes，DNS 互通，级联删除（`DD/sandboxes.mdx#L556`） | 私网段无条件拒绝，沙箱之间不互通（ADR-0010） | **排除**（隐含）；缺点是 Agent 不能另起一个 DB 沙箱给自己用，需要的话 DB 就跑在同一个 Sandbox 里 |
| Docker-in-Sandbox | 文档专章（`C/docs/admin/templates/extending-templates/docker-in-workspaces.md`） | 文档专章（`DD/snapshots.mdx#L1026`） | privileged 为 `unsupported`（ADR-0009） | **排除**；这是开发场景里一个真实的代价，ADR-0009 没有点名，建议在能力面文档里写一句 |

### 1.2 执行：进程、会话、PTY、日志

| 功能 | Coder | Daytona | 我们 | 判定 |
|---|---|---|---|---|
| 一次性命令 | agent `/api/v0/processes/start`（`C/agent/agentproc/api.go#L58`）；MCP `coder_workspace_bash` | `/process/execute`（`DT/apps/daemon/pkg/toolbox/server.go#L202`） | 持久化的 Exec（#33、ADR-0015） | **有**，比两家都重 |
| 列出进程 / 命令 | `/processes/list`（`agentproc/api.go#L59`） | `GET /process/session`、`/pty`（`server.go#L208`、`#L224`） | 没有写 Exec 列表 | **缺·必须补**：`GET /sandboxes/{id}/execs?state=`，Agent 断线重连后要靠它找回自己的 Exec |
| 信号 / 取消 | `/{id}/signal`（`agentproc/api.go#L61`） | 删 session | Cancellation + Grace Period（#33） | **有** |
| 有状态 shell 会话 | 无（SSH 会话） | session：cwd/env 跨命令保持（`server.go#L208-L217`） | 无；Exec 是 argv | **缺·可不做**：要状态就自己 `sh -c`，或者用 PTY |
| 向运行中命令送 stdin | 无（PTY） | `/session/:id/command/:cid/input`（`server.go#L216`） | 只有创建时的 stdin（ADR-0009）和 PTY 输入 | **缺·可不做**：交互场景走 PTY |
| PTY 创建 / 连接 / resize / 列表 | `GET /workspaceagents/{id}/pty`（reconnecting PTY） | `/pty` CRUD、`/connect`、`/resize`（`server.go#L224-L229`） | PTY Attachment，可多人输入，空闲终止（ADR-0015） | **有**；resize 帧没写，归实现规格 |
| 日志 follow / 快照 | build logs、agent logs | `/command/:cid/logs?follow`；`log-streaming`（`DD/log-streaming.mdx#L21`、`#L276`） | Execution Log，按 seq 续传（#33、ADR-0014） | **有**，比两家强 |
| 代码解释器 | 无 | `/process/code-run`、interpreter context（`server.go#L203`、`#L236-L239`） | 无 | **缺·可不做**：在 Exec 里跑 `python -c` 就行 |
| 启动脚本 / 启动日志 | agent startup script + startup logs（`/workspaceagents/{id}/startup-logs`） | entrypoint session + logs（`server.go#L210-L211`） | 无 entrypoint；Create 不执行用户命令（ADR-0009） | **排除**：ADR-0009「Create … does not execute a user command」 |
| OOM 等平台死因 | OOM/OOD 通知（`C/docs/admin/monitoring/notifications/index.md` Workspace Events） | 无（#5140 反例，见 recheck-api） | Execution Result 有 "platform cause"，但 cause 列表里没有 OOM | **缺·必须补**：cause 加 `oom_killed`（从 cgroup `memory.events` 读）。GPU 训练最常见的死法就是 OOM，Agent 必须能把它和 exit 137 区分开 |

### 1.3 文件与 Git

| 功能 | Coder | Daytona | 我们 | 判定 |
|---|---|---|---|---|
| 文件 API 动词 | list-directory / resolve-path / read-file / read-file-lines / write-file / edit-files / bundle-files（`C/agent/agentfiles/api.go#L59-L66`） | list / download / bulk-download / find / info / search / folder / move / permissions / replace / upload / bulk-upload / delete（`server.go#L178-L196`） | 「File operations stay on the Data Plane」（ADR-0011），走 Gateway HTTP（#31）；**动词清单没有定** | **缺·必须补**：至少 list、stat、read（支持 range）、write、mkdir、move、delete，外加 tar 批量上传和下载。路径安全的取证已经有（`file-transfer-mechanics-and-path-safety.md`），只差落成清单 |
| 搜索 / 替换 / 编辑 | edit-files（search-replace）；MCP `coder_workspace_edit_file(s)` | find（grep）、search（glob）、replace | 无 | **缺·可不做**：Agent 自己在 Exec 里跑 `rg`/`sed`。真要做，放在 Tool 层，不放进 REST |
| Git API | 只有 `/api/v0/git/watch`（`C/agent/agentgit/api.go#L40`） | clone/status/branches/add/commit/push/pull/checkout/history（`server.go#L245-L256`） | 没有平台 Git 操作，Agent 自己跑 `git`/`gh` | **排除**：ADR-0013「no platform clone and no platform push」。#31 和 GLOSSARY「Data Plane」里还留着「Git」两个字，应删（audit L2） |
| Git 凭据 | External Auth，`GIT_ASKPASS`（`C/docs/reference/api/git.md`） | linked accounts（`DA/user/user.controller.ts`） | Git Account Link + `git.credential`（ADR-0013） | **有** |
| Git SSH key | `/users/{user}/gitsshkey` | 无 | 只支持 HTTPS | **排除**：ADR-0013「Git over SSH is not supported」 |

### 1.4 接入：端口、preview、SSH、IDE、Web 终端

| 功能 | Coder | Daytona | 我们 | 判定 |
|---|---|---|---|---|
| 端口 preview URL | dashboard 子域名代理，共享级别 `owner/authenticated/public`（`port-forwarding.md#L59-L111`）；目标是 `127.0.0.1:PORT`（`C/coderd/workspaceapps/request.go#L338`） | `https://{port}-{sandboxId}.{proxyDomain}` + `x-daytona-preview-token`；signed URL 1s–24h，可提前吊销（`DD/preview.mdx#L27`、`#L113`；`sandbox.controller.ts#L1161-L1190`） | 「a service running inside a Sandbox is not reachable from outside」（ADR-0010 Ingress） | **缺·需决策**（见 §3.1） |
| 列出监听端口 | `/workspaceagents/{id}/listening-ports`（`C/agent/api.go#L67`） | `/port`、`/port/:port/in-use`（`server.go#L363-L364`） | 无 | 跟 preview 一起决策；做 preview 就顺手做（Node Agent 读 netns 里的 `/proc/net/tcp`） |
| 本地端口转发 | `coder port-forward --tcp 8000:8080`（`port-forwarding.md#L47`） | 无（只有 preview） | 无 | 跟 preview 一起决策：CLI 经 Gateway WebSocket 隧道 TCP |
| 公开分享 | `share=public`（受管理员上限约束） | `public` 属性，以及 `POST /sandbox/:id/public/:isPublic`（`DD/preview.mdx#L17`；`sandbox.controller.ts#L873`） | 无 | **缺·可不做**：单团队、内网，只做 owner（最多加 authenticated）就够 |
| SSH | 内建 SSH server + tailnet，`coder ssh`、`config-ssh`（`C/docs/user-guides/workspace-access/index.md#L29-L135`） | token 制，默认 60 分钟，可吊销；经 `ssh-gateway` → runner → daemon:22220（`DD/ssh-access.mdx#L132`；`DT/apps/ssh-gateway/main.go#L191-L281`；`DT/apps/daemon/pkg/ssh/config/config.go#L6`） | 无 | **缺·需决策**（见 §3.2） |
| VS Code / JetBrains / Cursor | 一等支持（`index.md#L139-L168`） | 通过 SSH（`DD/ssh-access.mdx#L109-L126`） | 无 | 依赖 SSH 决策 |
| Web 终端 | 有（`index.md#L17`） | 端口 22222 上的 web terminal（`DD/web-terminal.mdx#L43`） | 无 Web UI | **缺·需决策**（见 §3.5） |
| VNC / computer use / 录屏 | 虚拟桌面（Coder Agents） | `/computeruse/*`、`/recordings`（`server.go#L306-L356`） | 无 | **缺·可不做**：GPU 训练和代码开发用不上 |
| LSP | 无 | `/lsp/*`（`server.go#L263-L272`） | 无 | **缺·可不做**：Agent 自己在沙箱里起语言服务器 |
| VPN 进沙箱 | Coder Desktop（Coder Connect） | Tailscale/OpenVPN/Netbird 指南（`DD/vpn-connections.mdx#L22`） | 无 | **排除**：违背 Gateway-only 入口（ADR-0010） |

### 1.5 环境模板：Snapshot / template / volume / Resource

| 功能 | Coder | Daytona | 我们 | 判定 |
|---|---|---|---|---|
| 模板对象与版本 | template + template version + active version（`C/docs/reference/api/templates.md`） | Snapshot，状态 `pending/building/pulling/active/inactive/error/build_failed/removing`（`DD/snapshots.mdx#L1013-L1020`） | Snapshot + SnapshotVersion（OCI digest），`building → ready｜failed｜cancelled`（ADR-0012） | **有** |
| 从镜像 / Dockerfile 构建 | Terraform + envbuilder | public/local/private registry 镜像，或 declarative builder（`DD/snapshots.mdx#L296-L570`） | 只能以平台基础镜像为基底，声明输入（ADR-0012） | **有**；不收用户任意镜像属于**排除**（地图 Out of scope） |
| 构建日志 | `/templateversions/{id}/logs` | `/snapshots/:id/build-logs`（`snapshot.controller.ts#L220`） | 无 | **缺·必须补**：构建本来就是 `system` 优先级的作业，日志直接复用 Execution Log 的存储和续传。构建作业的调度见 audit M8 |
| 从 Snapshot 创建 Sandbox：名字解析 | 用 template 的 active version | `snapshot` 字段写名字（`create-sandbox.dto.ts#L43`） | ADR-0012 有 current 版本的概念，但没写 Sandbox 创建时是传名字还是传 digest | **缺·必须补**：创建时接受 `snapshot: name` 或 `snapshot_version: digest`，解析结果持久化在 Sandbox 上（同 ResourceVersion 的 pin 规则）。不填时用哪个默认镜像见 audit L12 |
| 镜像拉取进度 | — | `pulling_snapshot` 状态 | 并在 `provisioning` 里 | **缺·可不做**：给 `provisioning` 加一个 `pulling_image` condition 就够 |
| 预拉取 / 按镜像亲和调度 | prebuilds | Snapshot 预先拉到 runner；warm pool（`daytona-sandbox-runtime-semantics.md` §1） | 无 | **缺·可不做**：单团队、少数几台 Node，第一次拉取的代价可以接受；warm pool 会占着 GPU，和硬 reservation 冲突 |
| activate / deactivate | template 弃用（deprecated） | `activate/deactivate`（`snapshot.controller.ts#L327-L365`） | 无；用 TTL 过期非 current 版本（ADR-0012） | **排除**：ADR-0012 拒绝了 idle deactivation |
| 从运行中的沙箱抓 Snapshot | 无 | `POST /sandbox/:id/snapshot`（`sandbox.controller.ts#L748`） | 无 | **排除**：ADR-0012「the Workspace is never a build context」，GLOSSARY 的 Snapshot 不含 Workspace 状态 |
| 可变 volume | 持久卷由 template 管理 | `/volumes` CRUD，挂多个，可用 subpath（`DD/volumes.mdx`） | Workspace（单写者）+ 只读 Resource | **排除**：ADR-0012 拒绝 mutable volumes |
| 只读数据集挂载 | 无 | 无（volume 是可写的） | Resource / ResourceVersion（ADR-0012） | **有**，两家都没有 |
| 产出物收集 | 无 | 无 | Artifact（ADR-0012） | **有**，两家都没有 |
| 外部存储挂载 | 无 | `mount-external-storage` | 无 | **排除**：ADR-0009 不提供任意挂载 |

### 1.6 查询、标签、事件、用量、可观测

| 功能 | Coder | Daytona | 我们 | 判定 |
|---|---|---|---|---|
| Sandbox 名字 | workspace name，在 owner 下唯一 | `name`，ID 和名字都可以寻址（`:sandboxIdOrName`） | 未提及 | **缺·必须补**：可选的 name，在 User 下唯一。Agent 用名字幂等地找回自己的 Sandbox，可以和 Idempotency-Key 并用 |
| 标签 / 元数据 | 无用户标签（只有 tags 给 provisioner 路由用） | `labels: {k:v}`、`PUT /labels`（`create-sandbox.dto.ts#L71`；`sandbox.controller.ts#L648`） | 未提及 | **缺·必须补**：`labels: map<string,string>`，创建时写入，可以替换 |
| 列表过滤 / 分页 | `owner/user/name/template/status/outdated/dormant/has-agent/id/healthy`（`workspace-management.md#L64-L85`） | cursor、id、name、labels、states、snapshots、cpu/mem/disk 范围、createdAt、lastEvent 等，加 sort/order（`list-sandboxes-query.dto.ts#L37-L315`） | 未提及 | **缺·必须补**：最小集合是 `owner`（admin 才能看别人的）、`state`、`label`、`node`、`has_gpu`，加 cursor 分页 |
| 状态 watch / 事件流 | `GET /workspaces/{id}/watch`（SSE）、`watch-ws`（`C/docs/reference/api/workspaces.md#L2603-L2634`） | socket.io 网关推 `sandbox.state.updated` 等事件（`DA/notification/gateways/notification.gateway.ts#L120-L156`） | 只有 Exec 输出流 | **缺·必须补**：`GET /sandboxes/{id}?wait_for=ready&timeout=` 长轮询，或者 SSE watch。排队可能要排很久，Agent 不能靠盲轮询 |
| Webhook | 通知可以走 webhook 投递（`C/docs/admin/monitoring/notifications/index.md#L64-L88`） | 7 种事件（`DA/webhook/constants/webhook-events.constants.ts#L7-L13`） | 无 | **缺·可不做**：调用方是 Agent 和 CLI，有长轮询或 watch 就够；以后要接 Slack 再加 |
| 站内通知 / 邮件 | inbox、SMTP，事件含 autostop reminder、OOM/OOD（`notifications/index.md`） | 邮件模块 | 无 | **缺·可不做**：同上 |
| 配额用量查询 | `GET /organizations/{org}/members/{user}/workspace-quota`（`C/docs/reference/api/enterprise.md#L2453`） | `GET /organizations/:id/usage`：GPU/CPU/mem/disk 的 quota 和 current（`DA/organization/controllers/organization.controller.ts#L326`；`organization-usage-overview.dto.ts#L18-L36`） | User Quota 只在 admission 时检查（ADR-0015），没有查询面 | **缺·必须补**：`GET /users/me/quota` 返回 `{gpu: {limit, allocated, held}, sandboxes: {...}}` |
| GPU 可见性（剩多少卡） | 不调度 GPU | runner 列表有 `gpu`、`gpuType` 和已分配的 CPU/mem/disk，但要 `read:runners` 权限（`DA/sandbox/dto/runner.dto.ts#L64-L121`；`runner.controller.ts#L250`）；没有给普通用户的空闲 GPU 计数 | ADR-0014 只说 drained 是「可查询的事实」，Node 查询面没有定 | **缺·必须补**：给 `user` 一个只读的容量视图，每个 Node 列出 GPU 型号、总数、已分配数、paused/unreachable、排队长度，不暴露别人 Sandbox 的细节。单团队里这份透明是排队制的前提 |
| 排队位置 / 为什么还没调度上 | provisioner job 队列位置（`/organizations/{org}/provisionerjobs`） | 无 | #34 有 FIFO，但没有可见面 | **缺·必须补**：把它并进 Sandbox 视图，给出 `queue_position` 和 `blocked_reason`（`capacity｜locality｜node_paused｜node_unreachable`）。排队态本身见 audit S1 |
| 单沙箱资源指标 | agent metadata、resource monitoring（`C/docs/admin/templates/extending-templates/resource-monitoring.md#L9-L14`） | `/sandbox/:id/telemetry/{logs,traces,metrics}`，基于 OTel（`DA/sandbox-telemetry/controllers/sandbox-telemetry.controller.ts#L35-L126`） | 只有平台进程的 Prometheus `/metrics`，挂在 `ops` 监听面（ADR-0014） | **缺·需决策**（见 §3.4） |
| 用量历史 / 洞察 | insights（DAU、模板用量）（`C/docs/reference/api/insights.md`） | usage、billing、analytics | 无 | **缺·可不做**：不计费；有审计表和 Prometheus 就够事后统计 |
| 审计查询 | `GET /audit`（`C/docs/reference/api/audit.md`） | `GET /audit/organizations/:id`（`DA/audit/controllers/audit.controller.ts`） | 有审计表（ADR-0010），没写查询面 | **缺·必须补**（小）：`admin` 的 `GET /audit`，按 actor、target、时间过滤 |
| 健康检查 | `/healthz`、`/debug/health` | `/health`、`/health/ready` | 未提及 | 实现细节：放在 `ops` 监听面 |

### 1.7 身份、组织、凭据

| 功能 | Coder | Daytona | 我们 | 判定 |
|---|---|---|---|---|
| 组织 / 成员 / 邀请 | organizations、members、groups | organizations、invitations、roles（`DA/organization/controllers/*`） | 无 | **排除**：单团队，Tenant 不在范围内（地图 Out of scope），ADR-0010 不做角色模型 |
| 自定义角色 | org roles | `organization-role.controller.ts` | 只有 `user`/`admin` | **排除**：ADR-0010 |
| User 管理 | 创建、暂停、激活、删除（`C/docs/reference/api/users.md`） | `admin/users` | 未定 | **缺·必须补**：已见 audit L13（admin 创建和删除 User） |
| API key：多把、命名、过期、吊销 | `tokens create --lifetime`，可 expire（`C/docs/admin/users/sessions-tokens.md#L59-L113`） | `expiresAt`；按名字删除（`DA/api-key/dto/create-api-key.dto.ts#L42`） | 只定了形态（ADR-0010） | **缺·必须补**：并进 L13。每个 User 可以有多把命名 token，带可选过期时间，可吊销，并写审计 |
| API key 细粒度范围 | `resource:action`，默认 `coder:all`（`sessions-tokens.md#L124-L128`） | `write:sandboxes`、`delete:sandboxes` 等（`DA/organization/enums/organization-resource-permission.enum.ts#L9-L37`） | `user` / `admin` 两级 | **缺·需决策**（见 §3.6） |
| OIDC / SSO | 有 | 有（Auth0） | 无 | **排除**：ADR-0010 |
| Secrets | user secrets（`C/docs/reference/api/secrets.md`） | env；平台 secret | Secret，只写不读，创建时注入（ADR-0013） | **有** |
| Git 账号绑定 | External Auth | linked accounts | Git Account Link（ADR-0013） | **有** |

### 1.8 接口形态：SDK、CLI、MCP、管理

| 功能 | Coder | Daytona | 我们 | 判定 |
|---|---|---|---|---|
| REST / OpenAPI | swaggo 从代码生成 | 从代码生成（`DA/generate-openapi.ts`） | REST/OpenAPI 是唯一权威（#31、ADR-0015） | **有** |
| SDK 语言 | Go（`codersdk`） | Python（同步/异步）、TS、Ruby、Go、Java（`DT/libs/sdk-*`） | 「生成基础客户端 + 薄手写层」（#31），**语言没定** | **缺·需决策**（见 §3.7） |
| CLI | 全量（`C/docs/reference/cli/`） | auth、sandbox（create/list/info/start/stop/archive/pause/delete/exec/ssh/preview_url）、snapshot、volume、organization、mcp（`DT/apps/cli/cmd/`） | 有 CLI（#31；ADR-0013 `orrery token`），命令清单未定 | **有**；命令清单归实现规格，建议照 REST 资源一对一映射 |
| MCP server | 内建 Streamable HTTP（无状态，只有 tools）+ CLI stdio（`C/docs/ai-coder/mcp-server.md#L10-L100`）；工具清单见 `toolsdk.go#L32-L69` | CLI stdio，`daytona mcp init claude/cursor/windsurf`（`DD/mcp.mdx#L54-L116`） | 「Agent Tool API」形态未定 | **缺·需决策**（见 §3.3） |
| 沙箱内 Agent 回报状态 | `PATCH /workspaceagents/me/app-status`；MCP `coder_report_task` | 无 | 无；通道动词只有 `git.credential`（ADR-0011） | **缺·可不做**：Agent 的进度属于 Agent 自己的协议 |
| Web 控制台 | 全功能 dashboard | 全功能 dashboard | 未提及 | **缺·需决策**（见 §3.5） |
| Node 管理：登记、暂停、下线、删除 | provisioner keys、daemons | runner CRUD、`scheduling`、`draining`（`runner.controller.ts#L272-L309`） | Enrollment Packet、`paused`、admin 声明 lost、删除（ADR-0014） | **有** |
| 平台级配额管理 | group allowance（`C/docs/admin/users/quotas.md#L5-L22`） | `PATCH /organizations/:id/quota`（`organization.controller.ts#L346`） | User Quota（#34、ADR-0015），由 admin 设置；设置接口没写 | 并进 L13 的 admin 接口 |
| Egress Opening 管理 | — | 组织级出网配置 | 平台维护，写审计（ADR-0010） | **有**（接口归实现规格） |
| 网络策略热改 | — | `POST /sandbox/:id/network-settings`（`sandbox.controller.ts#L1072`） | 创建时声明（ADR-0010） | **排除**：ADR-0010「declared when it is created」 |
| AI Gateway / LLM 代理 / 花费管理 | AI Gateway、AI spend（Premium） | 无 | 无 | **缺·可不做**：不在本平台的目的地里 |

---

## 2. 「缺·可不做」的理由汇总

| 项 | 理由 |
|---|---|
| autostart、手动续命 | Agent 驱动；Idle Stop 已经按 Exec 和连接判定 |
| fork | 要复制就开一个 Exec 去 cp/tar；Workspace 本来能只读多挂 |
| 有状态 shell 会话、运行中送 stdin、代码解释器 | Exec(`sh -c`) 和 PTY 已经覆盖；两家的 session 都只在内存里，也不比我们强 |
| 文件搜索/替换/编辑 | 在 Exec 里跑 `rg`/`sed`；真要做，放在 Tool 层 |
| 公开分享 preview | 单团队、内网 |
| VNC、computer use、LSP、录屏 | 和 GPU 训练、代码开发的目的地无关 |
| 镜像拉取的独立状态 | 用一个 condition 就能表达 |
| 预拉取、warm pool | 机队小；warm pool 会占着 GPU |
| webhook、站内通知、邮件 | 有长轮询或 watch 就够；以后接 IM 再加 |
| 用量历史、insights | 不计费；审计和 Prometheus 已经能事后统计 |
| 沙箱内 Agent 回报状态 | 属于 Agent 自己的协议 |
| AI Gateway | 不在目的地内 |

---

## 3. 需要用户决策的项（附推荐）

### 3.1 端口 preview / 端口转发

- **两家的做法**：入口都不开在沙箱网络上，而是由平台侧拨沙箱内的 `127.0.0.1:PORT`（§0 第 1 条）。Coder 的共享级别默认 `owner`（`port-forwarding.md#L99-L111`）；Daytona 的 standard token 在沙箱重启后作废，signed URL 默认 60s（`DD/preview.mdx#L27-L113`）。
- **对我们的影响**：ADR-0010 的「Sandboxes listen on no ports and are never connected into」要改写成「Sandbox 网络上没有可从外部到达的入站口；Gateway 经 Node Agent 代拨 Sandbox 内的回环端口」。Node Agent 本来就在宿主侧，进 netns 拨号不会新增网络暴露面。【推断】实现方式是：Node Agent 在 Sandbox 的 netns 里 `dial 127.0.0.1:PORT`，用 `setns` 或 Exec Supervisor 旁路都行。Local Runtime 没有 netns，直接拨宿主回环，capability 为 `best_effort`。
- **推荐**：补。范围是 owner-only 的子域名 HTTP/WebSocket 代理，外加 `orrery port-forward` TCP 隧道，都走 Gateway 和 API Token 鉴权。只用子域名，原因是 Coder 要求关掉 path-based apps（见 `recheck-identity-network-channel-vs-coder-daytona.md` G2）。Gateway 的每次授权判定进 Connection Log 而不是审计表（同文件 §0 第 3 条）。不做公开分享。
- **不补的代价**：Agent 起了 dev server、TensorBoard、Jupyter 以后，人看不到，只能让 Agent 截图或 curl 回来。

### 3.2 SSH / 本地 IDE 接入

- **两家的做法**：Coder 的 agent 内建 SSH server，经 tailnet 到达；Daytona 的 `ssh-gateway` 校验 token 后转给 runner，runner 再进 daemon:22220。两家的沙箱都不对外开 22 端口。
- **推荐**：Phase 1 不做，但要在规格里写明「不做」以及升级路径：Gateway 上加一个 SSH-over-WebSocket 端点，再由 `orrery ssh` 当 `ProxyCommand`（Coder `config-ssh` 的形状）。服务端可以是 Node Agent 在 netns 里起的 SSH 服务，也可以复用 PTY Attachment。如果目的地里的「remote development」包括人用 VS Code 接进来，就提前到 Phase 1。

### 3.3 Agent Tool API 的形态

- **两家的做法**：都是 MCP，工具和 REST 一一对应。Coder 另外有 `coder_workspace_bash`、`read_file`、`write_file`、`edit_file`、`port_forward` 这类面向 Agent 的复合工具（`toolsdk.go#L50-L59`）。
- **推荐**：Tool API = MCP。Phase 1 是 `orrery mcp` 的 stdio 子命令，复用 CLI 的 token；Streamable HTTP 端点留作升级路。工具从 OpenAPI 生成，加少量手写复合工具：「创建 Exec 并等待结果、返回日志尾部」「PTY 一问一答」。后者也解决了 audit L15 的 PTY 映射问题。ADR-0014 的表格里「`api` serves REST and Agent Tool API」要相应改写，因为 stdio 形态不占监听面。

### 3.4 单 Sandbox 资源指标（CPU/内存/GPU 利用率）

- **两家的做法**：Coder 有 agent metadata 和 OOM/OOD 监控；Daytona 有 per-sandbox OTel metrics 端点。
- **推荐**：最小形态是 Node Agent 在 `ops` 的 Prometheus 上导出 per-Sandbox 的 cgroup 指标和 GPU 利用率，带 `sandbox_id`、`owner` 标签，GPU 利用率读 NVML/DCGM。REST 上只给一个当前值的快照：`GET /sandboxes/{id}/usage`。理由：硬 reservation、不抢占，GPU 被闲占是这个平台最贵的浪费，而 Idle Stop 只看 Exec 和连接，看不到「Exec 在跑但 GPU 一直是 0%」。要不要按 GPU 利用率做 idle 判定，是另一个决策，不建议 Phase 1 做。

### 3.5 Web 控制台 / Web 终端

- **推荐**：Phase 1 不做 Web UI，只有 CLI、REST、MCP。规格里写一句「无 Web UI」，免得实现阶段默认要做。两家都有 dashboard，但两家都要服务不写代码的用户；我们的用户是 Agent 和工程师。

### 3.6 API Token 细粒度范围

- **两家的做法**：都有。Coder 是 `resource:action`（`sessions-tokens.md#L124-L128`），Daytona 是 `write:sandboxes`、`delete:sandboxes` 这类权限（`organization-resource-permission.enum.ts#L9-L37`）。
- **推荐**：Phase 1 维持 `user`/`admin`，只加一个 `read_only` scope，给看板和监控脚本用。真正的需求是「Agent 拿着的 token 不能删 Workspace」，这在 ADR-0011 的 Rejected alternatives 里已经被当作风险提过。如果用户认同这个风险，就再加一档 `user_no_delete`；否则不加。

### 3.7 SDK 语言

- **推荐**：Python（Agent 框架和训练脚本的主语言）+ Go（CLI 和平台自己用）。TS 等需求出现再加。Daytona 有五种语言，是因为它要服务公开市场。

---

## 4. 「必须补」清单（汇总）

1. **Sandbox name + labels + 列表过滤与分页**（§1.6）。
2. **Sandbox 状态长轮询或 watch**（§1.6）。
3. **配额用量查询 + 只读容量视图（每个 Node 的 GPU 总数和已分配数）+ 排队位置与阻塞原因**（§1.6，依赖 audit S1 先补排队态）。
4. **Exec 列表**（§1.2）。
5. **Execution Result 的 `oom_killed` cause**（§1.2）。
6. **文件 API 动词清单**（§1.3）。
7. **Snapshot→Sandbox：按名字或 digest 创建、解析结果钉住、默认镜像；构建日志**（§1.5，与 audit L12、M8 合并）。
8. **User 与 API Token 管理面（多把、命名、过期、吊销）、admin 设置 User Quota、审计查询**（§1.7、§1.6，与 audit L13 合并）。

## 5. 未核实

- Daytona 线上服务在 v0.190.0 之后还在演进（`daytona-sandbox-runtime-semantics.md` §0 第 1 条）。本文以钉死 tag 的源码和文档为准，没有对线上 `openapi.json` 逐项比对。
- Coder 的 dashboard 端口代理从 coderd 到 agent 的完整链路，只核对了 `request.go#L338` 的目标地址构造和文档，没有逐行读 tailnet 拨号代码（同 `recheck-identity-network-channel-vs-coder-daytona.md` §5）。
- 「Node Agent 进 netns 拨回环」在 rootless Podman 下是否需要额外权限，没有验证；`oci-hook-netns-injection.md` 里有相关的 netns 取证可以参考。
