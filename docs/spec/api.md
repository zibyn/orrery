# 公开 REST API

REST/OpenAPI 是唯一语义权威（ADR-0015）。本文锁定资源、路径、错误码和约定；逐字段的 OpenAPI 文档由代码生成，以本文为准。数字默认值以 `defaults.md` 为准。

## 1. 约定

- **前缀**：`/api/v1`。`api` 监听面上另有 `/metrics`（`admin`）和端口转发子域名（§6）。
- **ID**：服务端生成的 UUIDv7，不带类型前缀。Sandbox 的 `name` 每个 User 内唯一，只用于过滤，不进路径。
- **认证**：`Authorization: Bearer <token>`。API Token 形如 `ssk_<id>_<secret>`，Sandbox Token 形如 `sst_<id>_<secret>`；服务端按 id 取行、比较 secret 的哈希。前缀便于 secret scanning。
- **Scope**：`read_only` 只能 `GET`；`user` 能操作自己的资源；`admin` 另能调用标 **admin** 的接口。Sandbox Token 只能调用 `POST /git/credential`。
- **存在性**：非 owner 访问别人的资源一律 `404 not_found`，不透露是否存在。`admin` 可读写所有资源。
- **创建**返回 `201` 和初始状态的资源；**动作**（`POST /{资源}/{id}/{动词}`）返回 `202` 和当前资源；删除用 `DELETE`，返回 `202`，资源进入 `deleting`。
- **幂等**：所有创建接口接受 `Idempotency-Key` 请求头，按 (User, 接口, key) 唯一，重复请求返回已有资源，不比较请求体；key 保留 24 小时。
- **等待**：`GET /sandboxes/{id}?wait_for=<state>` 最多挂起 60 秒，到期返回当前状态，不报错。
- **分页**：`?limit=`（默认 50，上限 200）和不透明的 `?cursor=`；响应 `{items, next_cursor}`。
- **条件更新**：`If-Match` 只用于移动 Resource 的 current 指针（ADR-0012）。

## 2. 错误

错误体是 RFC 9457 `application/problem+json`，扩展字段 `code` 取自下表，这是封闭目录：

| `code` | HTTP | 含义 |
|---|---|---|
| `invalid_request` | 400 | 请求不合法（含保留的 Secret 名、路径穿越） |
| `unauthenticated` | 401 | 没有 token、token 无效、过期或已吊销 |
| `permission_denied` | 403 | token 的 scope 不够 |
| `not_found` | 404 | 不存在，或不属于调用方 |
| `conflict` | 409 | 与当前状态冲突：读写挂载已被占用、Sandbox 不在 `ready`、只读挂载上写文件、Node 删除前提不满足 |
| `precondition_failed` | 412 | `If-Match` 不匹配 |
| `payload_too_large` | 413 | 单文件超过 5 GiB、Secret 超过 24 KiB |
| `unsupported` | 422 | 所选 Runtime 不能 `enforced` 一项必需能力 |
| `quota_exceeded` | 429 | 创建时已超出 User Quota 或 Secret 个数上限 |
| `internal` | 500 | 平台缺陷 |
| `unavailable` | 503 | Sandbox 所在 Node 不可达；可以重试 |

`incompatible` 只出现在 Node Agent 的 gRPC 握手里（`node-protocol.md`）。

生命周期原因不是错误码：它们写在资源的 `cause` 字段里（Sandbox 的 `admission_timeout`、`unsatisfiable`、`quota_exceeded`、`runtime_create_failed`、`image_pull_failed`、`runtime_lost`、`node_lost`；Exec 见 §4）。创建时就能判定的配额超限直接返回 `429`；`start` 一个 `stopped` 的 Sandbox 时重新准入，超限则以 `failed/quota_exceeded` 结束。

## 3. 资源与路径

未标注的接口要求 `user` scope，作用于调用方自己的资源。

**User 与 token**

| 方法与路径 | 说明 |
|---|---|
| `POST /users` · `GET /users` · `GET /users/{id}` · `DELETE /users/{id}` | **admin**。第一个 admin 由 `stars-sandbox-server bootstrap` 创建 |
| `PUT /users/{id}/quota` | **admin**。`{gpus, sandboxes}` |
| `GET /users/me` · `GET /users/me/quota` | quota 返回上限与当前用量 |
| `POST /users/me/tokens` · `GET /users/me/tokens` · `DELETE /users/me/tokens/{id}` | `{name, scope, expires_at?}`；明文只在创建响应里出现一次；列表带 `last_used_at` |

**Workspace**

| 方法与路径 | 说明 |
|---|---|
| `POST /workspaces` · `GET /workspaces` · `GET /workspaces/{id}` · `DELETE /workspaces/{id}` | 只创建持久 Workspace；临时 Workspace 随 Sandbox 隐式创建（ADR-0018） |
| `POST /workspaces/{id}/publish` | `{path, resource}` → 新 ResourceVersion 并移动 current 指针（ADR-0012） |
| `POST /workspaces/{id}/declare-lost` · `POST /workspaces/{id}/abandon` | **admin**。存活 Node 上的单个 Workspace 宣告 lost；放弃卡在 `deleting` 的清理 |

字段：`kind`（`ephemeral`/`persistent`）、`node_id`、`state`（`provisioning`、`ready`、`deleting`、`deleted`、`failed`、`lost`）、`unavailable`（由 Node 可达性派生）、`conditions`（如 `provisioning_stalled`）。

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
workspace?: {id, mode: rw|ro}     # 不写就隐式创建临时 Workspace，rw
snapshot?: SnapshotVersion id 或 Snapshot 名   # 不写就用部署的默认 digest
resources?: [{resource, version?, mount}]   # 创建时解析成具体版本，只读挂载
secrets?: {ENV_NAME: secret_name}
cpu, memory, gpu?: {count, model?}
idle_stop?: 秒数，默认 900，0 表示关闭
```

每个 Sandbox 只挂一个 Workspace，挂在 `/workspace`（ADR-0009）。响应字段另有 `state`、`cause`、`node_id`，`pending` 时还有 `queue_position` 和 `blocked_reason`。

**Exec**：`GET /execs/{id}` · `POST /execs/{id}/cancel` · `GET /execs/{id}/output` · `GET /execs/{id}/pty`，见 §4。

**Node 与容量**

| 方法与路径 | 说明 |
|---|---|
| `POST /enrollment-packets` | **admin**。返回一次性 Packet，写审计 |
| `GET /nodes` · `GET /nodes/{id}` · `DELETE /nodes/{id}` | **admin**。删除要求 Node 已 paused 且没有非终态 Workspace，否则 `409` |
| `POST /nodes/{id}/pause` · `/resume` · `/declare-lost` | **admin** |
| `GET /capacity` | 所有 scope 可读：每个 Node 的 GPU 型号、总数、已分配、状态（`ready`、`paused`、`unavailable`、`outdated`、`lost`） |

**Resource**

| 方法与路径 | 说明 |
|---|---|
| `POST /resources` · `GET /resources` · `GET /resources/{id}` · `DELETE /resources/{id}` | |
| `POST /resources/{id}/uploads` | 请求体是 manifest；返回缺失对象的预签名 PUT（带 `x-amz-checksum-sha256`） |
| `POST /resources/{id}/versions` | `{upload_id}`：校验对象都在后发布 ResourceVersion |
| `GET /resources/{id}/versions` · `GET /resources/{id}/versions/{v}/archive` | archive 是派生的 tar 流 |
| `PUT /resources/{id}/current` | `{version}`，可带 `If-Match` |

**Snapshot**

| 方法与路径 | 说明 |
|---|---|
| `POST /snapshots` · `GET /snapshots` · `GET /snapshots/{id}` · `DELETE /snapshots/{id}` | |
| `POST /snapshots/{id}/versions` | `{import: digest}` 或 `{build: {base, dockerfile, resources?, secrets?}}` |
| `GET /snapshots/{id}/versions` · `GET /snapshots/{id}/versions/{v}` · `DELETE /snapshots/{id}/versions/{v}` | 有 Sandbox（含 stopped）引用时删除返回 `409` |
| `POST /snapshots/{id}/versions/{v}/cancel` | |

构建中的 SnapshotVersion 带 `build_exec_id`，构建日志用 `GET /execs/{id}/output` 读。

**Secret**：`PUT /secrets/{name}`（创建或覆盖，只写）· `GET /secrets`（只返回名字和更新时间）· `DELETE /secrets/{name}`。

**Git**

| 方法与路径 | 说明 |
|---|---|
| `GET /git-providers` | 部署配置的 Git Provider |
| `GET /git-providers/{id}/authorize` | 302 到 provider 的授权页（PKCE） |
| `GET /external-auth/{id}/callback` | OAuth 回调 |
| `GET /users/me/git-links` · `DELETE /users/me/git-links/{provider_id}` | |
| `POST /git/credential` | **只接受 Sandbox Token**。`{host}` 或 `{provider}` → `{username, password}`；未关联或校验失败时返回 `404 not_found`，`authorize_url` 写在 problem 扩展字段里 |

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

**输出**（仅限非 PTY）：`GET /execs/{id}/output?after=<seq>` 返回一页 `[{seq, stream, data, encoding?}]`。`stream` 取 `stdout`、`stderr`、`truncated` 之一。`data` 是 UTF-8 文本，Exec Supervisor 只在字符边界上切分；遇到非法字节时，该帧带 `encoding: base64`。加 `&follow=true` 就升级为 WebSocket，推送同样的帧，Exec 结束后以一条 `{type: "result", ...}` 帧收尾。

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

只有 owner 能访问，子域名格式为 `<port>--<sandbox_id>.<apps_domain>`。泛域名和证书由部署配置。

- 浏览器：`POST /sandboxes/{id}/ports/{port}/ticket` 返回一个带签名票据的 URL，票据有效期不超过 1 分钟。Gateway 收到票据后，把它换成该子域名上的 cookie。转发前去掉 API Token 和 cookie。
- CLI：`stars-sandbox port-forward` 带 API Token 打开 `GET /sandboxes/{id}/ports/{port}/tunnel` WebSocket，承载原始 TCP 字节。

非 owner 访问一律返回 `404`。

## 7. MCP Tool API

`stars-sandbox mcp` 通过 stdio 提供服务，使用调用方的 API Token。

- 每个 `user` scope 可调用的非流式 OpenAPI operation 生成一个工具，名字取 operationId 的 snake_case。标 **admin** 的接口和 WebSocket 端点不生成工具。
- 另有两个组合工具：
  - `exec_run_and_wait`：创建 Exec 并跟随到结束，返回输出尾部、`result` 和 `log_truncated`。
  - `pty_send_and_read`：向 PTY 写入，然后读取到输出静默或超时为止。
