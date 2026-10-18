# 公开 REST API

REST/OpenAPI 是唯一语义权威（ADR-0015）。本文锁定资源、路径、错误码和约定；逐字段的 OpenAPI 文档由代码生成，以本文为准。数字默认值以 `defaults.md` 为准。

## 1. 约定

- **前缀**：`/api/v1`。`api` 监听面上另有 `/metrics`（`admin`）、端口转发子域名（§6）和 `/` 下的 Web 控制台（§8）。
- **ID**：服务端生成的 UUIDv7，不带类型前缀。Sandbox 的 `name` 每个 User 内唯一，只用于过滤，不进路径。
- **认证**：`Authorization: Bearer <token>`，或者浏览器的 Session cookie。API Token 形如 `ssk_<id>_<secret>`，Sandbox Token 形如 `sst_<id>_<secret>`；服务端按 id 取行，API Token 比较 secret 的哈希，Sandbox Token 解密 `token_ct` 后做常数时间比较（`schema.md`）。前缀便于 secret scanning。
- **Session cookie**：`stars_session; HttpOnly; Secure; SameSite=Lax; Path=/`，不带 `Domain`，端口转发子域名收不到。值的格式同 API Token，对应 `api_tokens` 里 `kind=session` 的一行。cookie 与 Bearer 等价；用 cookie 认证时，非安全方法要过 `http.CrossOriginProtection`，WebSocket 升级要求 `Origin` 等于 `api` 自己的源（ADR-0020）。
- **Scope**：`read_only` 只能 `GET`；`user` 能操作自己的资源；`admin` 另能调用标 **admin** 的接口。Session 的 scope 是 `user`，admin User 是 `admin`。Sandbox Token 只能调用 `POST /git/credential`。
- **存在性**：非 owner 访问别人的资源一律 `404 not_found`，不透露是否存在。`admin` 可读写所有资源。
- **创建**返回 `201` 和初始状态的资源；**动作**（`POST /{资源}/{id}/{动词}`）返回 `202` 和当前资源；删除用 `DELETE`，返回 `202`，资源进入 `deleting`。
- **幂等**：创建接口接受 `Idempotency-Key` 请求头，按 (User, 接口, key) 唯一（接口含路径参数，所以沙箱下的创建按 (Sandbox, key) 唯一），重复请求返回已有资源，不比较请求体；key 保留 24 小时。两个例外：`POST /enrollment-packets` 和 `POST /sessions` 的浏览器 Session 分支——响应里的 secret 只存哈希，重试拿不回旧值，只能新发一个，所以不读该头；`POST /sessions` 带 `token` 的分支仍幂等。
- **等待**：`GET /sandboxes/{id}?wait_for=<state>` 最多挂起 60 秒，到期返回当前状态，不报错。
- **分页**：`?limit=`（默认 50，上限 200）和不透明的 `?cursor=`；响应 `{items, next_cursor}`。
- **条件更新**：`If-Match` 只用于移动 Resource 的 current 指针（ADR-0012）。

## 2. 错误

错误体是 RFC 9457 `application/problem+json`，扩展字段 `code` 取自下表，这是封闭目录：

| `code` | HTTP | 含义 |
|---|---|---|
| `invalid_request` | 400 | 请求不合法（含保留的 Secret 名、路径穿越） |
| `unauthenticated` | 401 | 没有 token、token 无效、过期或已吊销 |
| `permission_denied` | 403 | token 的 scope 不够；修改平台 Snapshot |
| `not_found` | 404 | 不存在，或不属于调用方 |
| `conflict` | 409 | 与当前状态冲突：读写挂载已被占用、Sandbox 不在 `ready`、只读挂载上写文件、Node 删除前提不满足、宣告可达的 Node lost、删除 current 版本、删除被引用的 SnapshotVersion、发布时对象缺失或 sha256 不符、所用平台 Snapshot 没有 `ready` 版本 |
| `precondition_failed` | 412 | `If-Match` 不匹配 |
| `payload_too_large` | 413 | 单文件超过 5 GiB、Secret 超过 24 KiB |
| `unsupported` | 422 | 所选 Runtime 不能 `enforced` 一项必需能力；`runtime: local` 带了 `snapshot` 或 `resources` |
| `quota_exceeded` | 429 | 创建时已超出 User Quota 或 Secret 个数上限 |
| `rate_limited` | 429 | 登录失败次数超限；带 `Retry-After` |
| `internal` | 500 | 平台缺陷 |
| `unavailable` | 503 | Sandbox 或 Workspace 所在 Node 不可达；可以重试 |

`incompatible` 只出现在 Node Agent 的 gRPC 握手里（`node-protocol.md`）。

生命周期原因不是错误码：它们写在资源的 `cause` 字段里（Sandbox 的 `admission_timeout`、`unsatisfiable`、`quota_exceeded`、`runtime_create_failed`、`image_pull_failed`、`resource_fetch_failed`、`workspace_lost`、`runtime_lost`、`node_lost`；Exec 见 §4；SnapshotVersion 的 `timed_out`、`build_interrupted`、`node_lost`、`import_failed`、`build_failed`）。创建时就能判定的配额超限直接返回 `429`；`start` 一个 `stopped` 的 Sandbox 时重新准入，超限则以 `failed/quota_exceeded` 结束。

## 3. 资源与路径

未标注的接口要求 `user` scope，作用于调用方自己的资源。

**User 与 token**

| 方法与路径 | 说明 |
|---|---|
| `POST /users` · `GET /users` · `GET /users/{id}` · `PATCH /users/{id}` · `DELETE /users/{id}` | **admin**。`{name, email?, login_type, password?, is_admin}`；`login_type` 取 `password`、`oidc`、`none`。第一个 admin 由 `stars-sandbox-server bootstrap` 交互式设密码创建，或者是没有任何 User 时第一个 OIDC 登录者 |
| `PUT /users/{id}/password` | **admin**。重置密码 |
| `PUT /users/{id}/quota` | **admin**。`{gpus, sandboxes}` |
| `GET /users/me` · `GET /users/me/quota` | quota 返回上限与当前用量 |
| `PUT /users/me/password` | `{old, new}`，仅 `login_type=password`。改密码和重置密码都会吊销该 User 的全部 Session |
| `POST /users/me/tokens` · `GET /users/me/tokens` · `DELETE /users/me/tokens/{id}` | `{name, scope, expires_at?}`；明文只在创建响应里出现一次；列表带 `kind`（`token`/`session`）和 `last_used_at`，Session 也能单独吊销 |

**登录**（不需要已有凭据）

| 方法与路径 | 说明 |
|---|---|
| `POST /sessions` | `{username, password}` → `Set-Cookie` 一个 Session。带 `token: {name, scope, expires_at?}` 时改为返回一枚具名 API Token，不设 cookie（`stars-sandbox login` 用）。用户名不存在和密码错误都返回同一个 `401`；失败超限返回 `429 rate_limited` |
| `DELETE /sessions/current` | 退出登录，删掉当前 Session |
| `GET /oidc/login?return=` | 302 到 IdP（authorization code + PKCE），`state` 和 verifier 存在 HttpOnly cookie 里 |
| `GET /oidc/callback` | 先按 (`issuer`, `sub`) 匹配 User，再按已验证的 email 匹配；匹配不到时，`allow_signups` 打开或还没有任何 User 才建号，否则 `403`。`email_domains` 之外和 `email_verified=false` 一律 `403`。成功后设 Session cookie 并 302 回 `return` |

**Workspace**

| 方法与路径 | 说明 |
|---|---|
| `POST /workspaces` · `GET /workspaces` · `GET /workspaces/{id}` · `DELETE /workspaces/{id}` | 只创建持久 Workspace；临时 Workspace 随 Sandbox 隐式创建（ADR-0018）。删除 `lost` 的 Workspace 直接变成 `deleted`，不向 Node 下发命令 |
| `POST /workspaces/{id}/publish` | `{path, resource}` → `202` 和一个 `pending` 的 ResourceVersion；变成 `ready` 时移动 current 指针，失败时不动（ADR-0012、`node-protocol.md`）。接受 `Idempotency-Key`；不要求有运行中的 Sandbox；Node 不可达返回 `503` |
| `POST /workspaces/{id}/declare-lost` · `POST /workspaces/{id}/abandon` | **admin**。存活 Node 上的单个 Workspace 宣告 lost：挂着它的 Sandbox（运行中的和 stopped 的）变成 `failed/workspace_lost`，Reconciler 停掉它们的 Runtime，其中的 Exec 以 `runtime_lost` 结束。放弃卡在 `deleting` 的清理 |

字段：`kind`（`ephemeral`/`persistent`）、`node_id`、`state`（`provisioning`、`ready`、`deleting`、`deleted`、`failed`、`lost`；后三个是终态）、`unavailable`（由 Node 可达性派生）、`conditions`（如 `provisioning_stalled`）。

**Sandbox**

| 方法与路径 | 说明 |
|---|---|
| `POST /sandboxes` | 见下文请求体 |
| `GET /sandboxes` | 过滤：`owner`（admin）、`state`、`label`、`node`、`has_gpu` |
| `GET /sandboxes/{id}` | 支持 `?wait_for=` |
| `DELETE /sandboxes/{id}` | 先取消正在跑的 Exec，再删除；临时 Workspace 一并删除 |
| `POST /sandboxes/{id}/start` · `POST /sandboxes/{id}/stop` | `start` 重新进入 `pending` |
| `POST /sandboxes/{id}/execs` · `GET /sandboxes/{id}/execs` | 见 §4 |
| `/sandboxes/{id}/files/...` | 见 §5 |
| `POST /sandboxes/{id}/ports/{port}/ticket` · `GET /sandboxes/{id}/ports/{port}/tunnel` | 见 §6 |

创建请求体：

```
name?, labels?
runtime?: container|local       # 默认 container；调用方分不出 Docker 和 Podman
workspace?: {id, mode: rw|ro}     # 不写就隐式创建临时 Workspace，rw
snapshot?: SnapshotVersion id 或 Snapshot 名   # 名字解析到 current 版本，先找自己的再找平台的；container 不写就用 STARS_DEFAULT_SNAPSHOT，local 没有 Snapshot
resources?: [{resource, version?, mount?}]   # 创建时解析成具体版本，只读挂载到 /resources/<mount>；mount 是单段名字，默认取 Resource 名
secrets?: {ENV_NAME: secret_name}
cpu, memory, gpu?: {count, model?}
idle_stop?: 秒数，默认 900，0 表示关闭
```

每个 Sandbox 只挂一个 Workspace，挂在 `/workspace`（ADR-0009）。响应字段另有 `runtime`、`state`、`cause`、`node_id`、`conditions`（如 `fetching_resources`），`pending` 时还有 `queue_position` 和 `blocked_reason`。

`runtime`（ADR-0009）：

- `container` 只放到 Docker 或 Podman 的 Node 上，`local` 只放到 Local Node 上；重启后仍作为准入约束。
- `local` 带了 `snapshot` 或 `resources` 时返回 `422 unsupported`。
- Local 上没有 `/workspace`：cwd 是 Workspace 在宿主上的路径，也写在环境变量 `STARS_WORKSPACE` 里；Exec 的 `cwd` 相对于这个路径。
- Local 上所有 User 共用 Node 配置里的同一个非特权 uid，User 之间没有隔离（文件、信号、loopback 端口都互通），只给可信的内部用户用。GPU 可见性为 `best_effort` 时也接受。

**Exec**：`GET /execs/{id}` · `POST /execs/{id}/cancel` · `GET /execs/{id}/output` · `GET /execs/{id}/pty`，见 §4。

**Node 与容量**

| 方法与路径 | 说明 |
|---|---|
| `POST /enrollment-packets` | **admin**。返回一次性 Packet，写审计 |
| `GET /nodes` · `GET /nodes/{id}` · `DELETE /nodes/{id}` | **admin**。删除见下 |
| `POST /nodes/{id}/pause` · `/resume` · `/declare-lost` | **admin**。宣告 lost 见下 |
| `GET /capacity` | 所有 scope 可读：每个 Node 的 GPU 型号、总数、已分配、状态（`ready`、`paused`、`unavailable`、`outdated`、`lost`） |

Paused Node 不接收新的 Workspace 和构建。

**宣告 lost**（ADR-0014）：

- Node 当前可达时返回 `409`；存活 Node 的存储坏了，用 Workspace 级的 `declare-lost`。
- 不可撤回：同时清空 Node Token 并置 `paused`。Agent 重连时收到 `UNAUTHENTICATED`，清掉本地身份；这台宿主只能作为新 Node 重新注册。
- 级联在一个事务里完成：
  - 这台 Node 上所有未 `deleted` 的 Sandbox（含 `stopped`，以及已绑定到这台 Node 的 `pending`）变成 `failed/node_lost`，释放 Allocation 和 GPU，结束读写 Attachment；
  - 未结束的 Exec 以 `node_lost` 结束；
  - `deleting` 的 Workspace 直接变成 `deleted`，其余非终态的 Workspace 变成 `lost`；
  - 进行中的构建变成 `failed/node_lost`；
  - 进行中的 publish：对应的 ResourceVersion 变成 `failed`，cause 为 `node_lost`。

**删除 Node**：

- 前提：已 paused，没有非终态的 Workspace，也没有非终态的 Sandbox（含构建行），否则 `409`。
- 这一行留作墓碑：写 `deleted_at`，清空 Node Token，删掉它的 `gpus` 行；墓碑不清理。
- 删除后的 Node 不出现在 `GET /nodes` 和 `/capacity` 里。
- 还连着的 Agent 收到 `UNAUTHENTICATED`，流被关闭。

**Resource**

| 方法与路径 | 说明 |
|---|---|
| `POST /resources` · `GET /resources` · `GET /resources/{id}` · `DELETE /resources/{id}` | |
| `POST /resources/{id}/uploads` | 请求体是 manifest，每个文件带 sha256；返回 upload 会话和缺失对象的预签名 PUT（带 `x-amz-checksum-sha256`）。24 小时内带同一份 manifest 重复提交即续传：只为仍然缺失的对象重新签发 PUT |
| `POST /resources/{id}/versions` | `{upload_id}`：对每个对象做 `HeadObject`（`ChecksumMode`），全部存在且 sha256 一致就发布 `ready` 的 ResourceVersion 并移动 current 指针，否则 `409` 并列出问题对象 |
| `GET /resources/{id}/versions` · `GET /resources/{id}/versions/{v}` | |
| `GET /resources/{id}/versions/{v}/manifest` | 返回规范化的 manifest |
| `POST /resources/{id}/versions/{v}/downloads` | `{sha256[]}` → 预签名 GET；tar 由 CLI 在本地拼装，Control Plane 不读内容（ADR-0012） |
| `PUT /resources/{id}/current` | `{version}`，可带 `If-Match` |

manifest 的条目是 `{path, type: file|dir|symlink, size?, sha256?, executable?, target?}`：权限只记可执行位，空目录保留，最多 1,000,000 条。服务端规范化后，规范化 manifest 的 sha256 就是地址。publish 单个文件，得到只含这一个条目的树。

ResourceVersion 的 `state`：`pending → ready | failed`，另有 `unavailable`（对象在对象存储上 404）。对象缺失时上传会再次为它签发 PUT，重新上传同样的内容即修复：之后任何一次发布校验通过该对象，引用它的 `unavailable` 版本复查后回到 `ready`。

**Snapshot**

| 方法与路径 | 说明 |
|---|---|
| `POST /snapshots` · `GET /snapshots` · `GET /snapshots/{id}` · `DELETE /snapshots/{id}` | |
| `POST /snapshots/{id}/versions` | `{import: {ref, auth_secret?}}` 或 `{build: {dockerfile, base?, contexts?, secrets?}}`，见下；接受 `Idempotency-Key` |
| `GET /snapshots/{id}/versions` · `GET /snapshots/{id}/versions/{v}` · `DELETE /snapshots/{id}/versions/{v}` | current 版本或有 Sandbox（含 stopped）引用时删除返回 `409` |
| `PUT /snapshots/{id}/current` | `{version}`，用于回滚；版本变成 `ready` 时指针自动移过去 |
| `POST /snapshots/{id}/versions/{v}/cancel` | 取消构建或导入 |

- **状态**：`building → ready | failed | cancelled`，导入和构建共用。
- **删除 Snapshot**：写墓碑；被引用的版本保留到最后一个引用消失，然后删掉 registry 里的 manifest。名字只在未删除的 Snapshot 间唯一。
- **平台 Snapshot**：来自部署配置 `STARS_PLATFORM_SNAPSHOTS`（`defaults.md`），出现在每个 User 的 `GET /snapshots` 里；对它增删改返回 `403`。
- **import**：`ref` 必须是 `ref@sha256:…`，`auth_secret` 是调用方一个 Secret 的名字，值为 docker `config.json`。Control Plane 用 go-containerregistry 把整个 index 流式复制到平台 registry，保留原 digest。Node 只从平台 registry 拉镜像（ADR-0021）。
- **build**：
  - `dockerfile` 是文本；没有默认构建上下文。
  - `contexts?: {name: resource[@version]}`，创建时解析为具体版本，作为命名上下文传入。
  - `base?` 是平台内的 Snapshot 引用（同 Sandbox 的 `snapshot`），以命名上下文 `base` 注入；`FROM` 也可以写外部 `ref@digest`。
  - `secrets?: {id: secret_name}`，Dockerfile 用 `RUN --mount=type=secret,id=<id>` 读取；带 `secrets` 的构建不用缓存。
  - 构建以 `system` 优先级准入，资源上限和超时见 `defaults.md`，不计入 User Quota；只放到 `snapshot_build` 为 `enforced` 的 Node 上，与 Node 用哪种 Runtime 无关。provenance 记录 BuildKit 实际解析出的 FROM digest 和各 context 的版本地址。执行细节见 `runtime.md` §12。

构建中的 SnapshotVersion 带 `build_exec_id`，构建日志用 `GET /execs/{id}/output` 读。

**Secret**：`PUT /secrets/{name}`（创建或覆盖，只写）· `GET /secrets`（只返回名字和更新时间）· `DELETE /secrets/{name}`。

**Git**

| 方法与路径 | 说明 |
|---|---|
| `GET /git-providers` | 部署配置的 Git Provider |
| `GET /git-providers/{id}/authorize` | **只接受 Session cookie**，没有就 302 到控制台登录页。302 到 provider 的授权页（PKCE），`state` 绑定当前 User |
| `GET /external-auth/{id}/callback` | OAuth 回调。**只接受 Session cookie**，且 `state` 绑定的 User 必须是 Session 的 User，否则 `403` |
| `GET /users/me/git-links` · `DELETE /users/me/git-links/{provider_id}` | |
| `POST /git/credential` | **只接受 Sandbox Token**。`{host}` 或 `{provider}` → `{username, password}`；未关联或校验失败时返回 `404 not_found`，`authorize_url` 写在 problem 扩展字段里，指向控制台的 Git 关联页 |

Git Provider 来自部署配置 `STARS_GIT_PROVIDERS_FILE`（`defaults.md`）。OAuth `redirect_uri` 和 `authorize_url` 都以 `STARS_ACCESS_URL` 为基址；它未配置时 Git 关联关闭。

沙箱内的 helper 收到 `404` 时，把 `authorize_url` 写到 stderr，stdout 不输出，退出码为 0，由 git 继续尝试下一个 helper 或报认证失败。

**审计**：`GET /audit`，**admin**，过滤 `actor`、`action`、`target`、时间区间。

## 4. Exec

`POST /sandboxes/{id}/execs`：

```
argv: [string]        # 不经过 shell
env?: {k: v}
cwd?: 相对 /workspace 的路径
timeout?: 秒数
stdin?: string        # 一次性写入后关闭；PTY 模式下忽略
pty?: {rows, cols}    # 带上就是 PTY 模式
```

Sandbox 不在 `ready` 时返回 `409`。字段：`state` 从时间戳派生（`pending`、`running`、`canceling`、`finished`）；`result: {cause, exit_code?, signal?}`；`log_truncated`。

`cause` 的取值：`exited`、`canceled`、`timed_out`、`oom_killed`、`runtime_lost`、`node_lost`、`pty_idle`、`start_failed`。`start_failed` 表示命令从未开始，可以重试（ADR-0015）。

**输出**（仅限非 PTY）：`GET /execs/{id}/output?after=<offset>` 返回一页 `[{seq, stream, data, encoding?}]`。`offset` 是上一帧末尾在 Node 记录里的字节偏移（`seq + 5 + len(data)`，base64 帧按解码后的长度算）；首屏省略或传 `0`。`stream` 取 `stdout`、`stderr`、`truncated` 之一。`data` 是 UTF-8 文本，Exec Supervisor 只在字符边界上切分；遇到非法字节时，该帧带 `encoding: base64`。加 `&follow=true` 就升级为 WebSocket，推送同样的帧，Exec 结束后以一条 `{type: "result", ...}` 帧收尾。

**PTY**：`GET /execs/{id}/pty` 是 WebSocket，子协议为 `sandbox.v1`。二进制帧传终端字节，文本帧传控制消息（`resize {rows, cols}`）。服务端先回放 Node 上的环形缓冲；进程退出时，在关闭连接之前发送 `{type: "exit", ...}` 文本帧。多个连接可以同时写入。

## 5. 文件

挂在 Sandbox 下。路径参数 `path` 相对于 `/workspace`，只能访问 Workspace：Resource 挂载点和 rootfs 要通过 Exec 访问。前提条件：Sandbox 必须是 `ready`，否则 `409`；只读挂载下写入返回 `409`；Node 不可达返回 `503`。

| 方法与路径 | 说明 |
|---|---|
| `GET /sandboxes/{id}/files?path=` | 列目录 |
| `GET /sandboxes/{id}/files/stat?path=` | |
| `GET /sandboxes/{id}/files/content?path=` | 支持 `Range` |
| `PUT /sandboxes/{id}/files/content?path=` | 写到临时文件后原子 rename |
| `POST /sandboxes/{id}/files/mkdir` · `POST /sandboxes/{id}/files/move` | `{path}` · `{from, to}` |
| `DELETE /sandboxes/{id}/files?path=` | |
| `GET /sandboxes/{id}/files/archive?path=` · `PUT /sandboxes/{id}/files/archive?path=` | tar 下载 · tar 上传并解包 |

路径用 `os.Root` 解析，越界一律拒绝，不做改写；只处理普通文件、目录和符号链接（`file-transfer-mechanics-and-path-safety.md`）。

## 6. 端口转发

只有 owner 或 admin 能访问，子域名格式为 `<port>--<sandbox_id>.<apps_domain>`。泛域名和证书由部署配置。

- 浏览器：`POST /sandboxes/{id}/ports/{port}/ticket` 返回一个带签名票据的 URL，票据有效期不超过 1 分钟。Gateway 收到票据后，把它换成该子域名上的 cookie。转发前去掉 API Token 和 cookie。
- CLI：`stars-sandbox port-forward` 带 API Token 打开 `GET /sandboxes/{id}/ports/{port}/tunnel` WebSocket，承载原始 TCP 字节。

非 owner 且非 admin 访问一律返回 `404`。

## 7. MCP Tool API

`stars-sandbox mcp` 通过 stdio 提供服务，使用调用方的 API Token。

- 每个 `user` scope 可调用的非流式 OpenAPI operation 生成一个工具，名字取 operationId 的 snake_case。标 **admin** 的接口、WebSocket 端点和登录类接口（`/sessions`、`/oidc/*`、密码修改、Git 授权与回调）不生成工具。
- 另有两个组合工具：
  - `exec_run_and_wait`：创建 Exec 并跟随到结束，返回输出尾部、`result` 和 `log_truncated`。
  - `pty_send_and_read`：向 PTY 写入，然后读取到输出静默或超时为止。

## 8. Web 控制台

独立源码，构建成静态文件后嵌进 `stars-sandbox-server`，由 `api` 监听面在 `/` 下提供，与 `/api/v1` 同源，不开 CORS。它只调用本文列出的接口，用 Session cookie 认证（ADR-0020）。范围以下面的页面清单为准，不追求与每个 operation 对等；清单外的操作用 CLI。

技术栈与构建：

- Vite + React + TypeScript + TanStack Router（文件路由）+ TanStack Form + TanStack Query + Zod + shadcn/ui。
- 纯 SPA，不引入 TanStack Start 运行时；目录结构参考 Start 模板。
- 源码在 `web/`，产物经 `go:embed` 嵌入，未命中静态文件的路径回退到 `index.html`。仓库提交一个占位 `index.html`，不装 node 也能 `go build`；真正的构建放在 Makefile 和 Dockerfile 的多阶段构建里，`dist` 不进仓库。

| 页面 | 用到的接口 |
|---|---|
| 登录 `/login?return=` | `POST /sessions`、`GET /oidc/login` |
| CLI 授权 `/cli-auth` | `POST /users/me/tokens`，页面显示明文，用户粘贴回 `stars-sandbox login` |
| Git 关联 `/git` | `GET /git-providers`、`/users/me/git-links`、`authorize`；`authorize_url` 指向这里 |
| Sandbox 列表与详情 | 创建、start、stop、删除；`queue_position`、`blocked_reason`、`conditions` |
| Exec | 列表、`output?follow=true` 跟随 |
| 终端 | `GET /execs/{id}/pty` |
| 文件 | §5 的列目录、上传、下载 |
| 端口 | `POST /sandboxes/{id}/ports/{port}/ticket` 后打开返回的 URL |
| Snapshot | 列表、版本、构建日志 |
| Secret | 名字列表、覆盖写入、删除 |
| 我的 token 与 Session | `/users/me/tokens`、`PUT /users/me/password`、`DELETE /sessions/current` |
| admin | User（含重置密码、Quota）、Node、`GET /audit` |
