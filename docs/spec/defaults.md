# 数字默认值

规格里每个数字都以本表为准。「谁能改」一列：**常量**写死在代码里，改了就要发版；**部署**指 Control Plane 的部署配置；**Node** 指 Node Agent 的配置；**请求**指单次 API 请求的字段。等到真有人需要改某个常量时，再把它升级成开关。purge 类的配置项里，`0` 都表示不删。

## Node 与 Control Plane

| 名称 | 默认值 | 谁能改 | 说明 |
|---|---|---|---|
| 心跳间隔 | 15 秒 | 常量 | 与 Coder 一致 |
| 判定 Unreachable | 30 秒 | 常量 | 心跳间隔的 2 倍（ADR-0014） |
| Enrollment Packet 有效期 | 1 小时 | 部署 | 对应 GitHub runner 注册令牌的有效期；Coder 和 Daytona 只有长期 key，相当于我们的 Node Token |
| purge 周期 | 10 分钟 | 常量 | ADR-0014 |
| 磁盘水位 | 剩余低于 10% | Node | 低于水位就不接新分配，Resource 缓存开始按 LRU 淘汰；对应 kubelet 的 `nodefs.available<10%` |
| 宿主预留 | 1 CPU + 2 GiB | Node | 从总量里扣掉，不参与分配 |
| 每 GPU 份额 | 可分配量 × 3/4 ÷ 卡数 | Node | CPU 和内存各算一份；剩下的 1/4 给无 GPU 的 Sandbox（ADR-0015） |

## 调度与生命周期

| 名称 | 默认值 | 谁能改 | 说明 |
|---|---|---|---|
| 准入期限 | 1 小时 | 部署 | 到期后以 `failed/admission_timeout` 结束 |
| Idle Stop | 900 秒 | 部署给默认值，请求可覆盖 | `idle_stop`，`0` 表示关闭；与 Daytona 一致 |
| Workspace provisioning 重试 | 5 次，间隔 1、2、4、8、16 秒 | 常量 | 第 3 次失败起挂上 `provisioning_stalled`，用尽后 `failed` |

## Exec

| 名称 | 默认值 | 谁能改 | 说明 |
|---|---|---|---|
| Exec `timeout` | 无（不限时） | 请求 | |
| 取消与超时的 grace | 10 秒 | 常量 | 先 SIGTERM，过了 grace 再 SIGKILL；与 `docker stop` 一致 |
| Supervisor 等待 `spec` 的超时 | 30 秒 | 常量 | 超时就以 `start_failed` 结束 |
| Execution Log 上限 | 16 MiB | 部署 | 按 `output` 文件的字节数算，帧头也计入。只由 Supervisor 执行（`spec.log_cap`），所以 Node 上的 Spool 和 PG 里的日志是同一个上限 |
| PTY 空闲回收 | 300 秒 | 部署 | 与 Coder 一致 |
| PTY 环形缓冲 | 64 KiB | 常量 | 与 Coder 一致 |

## 保留与对象存储

| 名称 | 默认值 | 谁能改 | 说明 |
|---|---|---|---|
| Execution Log 保留期 | 30 天 | 部署 | 从 Exec 进入终态起算 |
| Audit Record 保留期 | 0（永久） | 部署 | 与 Coder 一致 |
| 非 current、无引用的 ResourceVersion | 30 天 | 部署 | |
| CAS 对象与未完成上传的 GC 宽限 | 24 小时 | 部署 | 必须长于预签名 URL 的有效期 |
| 预签名 URL 有效期 | 1 小时 | 常量 | 上传和 Node 拉取都用它 |
| 单文件上传上限 | 5 GiB | 常量 | ADR-0017 |

## API 与 Gateway

| 名称 | 默认值 | 谁能改 | 说明 |
|---|---|---|---|
| `wait_for` 最长挂起 | 60 秒 | 常量 | |
| 分页 | 默认 50，上限 200 | 常量 | |
| Idempotency Key 保留期 | 24 小时 | 常量 | |
| Gateway 端口票据有效期 | 60 秒 | 常量 | ADR-0017 |
| 每个 User 的 Secret | 最多 50 个，每个最大 24 KiB | 常量 | ADR-0017 |
| Git token 刷新提前量 | 10 秒 | 常量 | 即 `oauth2` 的 `Token.Valid()`；Coder 也直接用它 |

## 身份

| 名称 | 默认值 | 谁能改 | 说明 |
|---|---|---|---|
| Session 闲置期限 | 24 小时 | 常量 | 每次使用顺延；与 Coder 浏览器 session 一致 |
| 密码最短长度 | 12 | 常量 | 不设复杂度规则 |
| 登录失败限速 | 每个（用户名、来源 IP）15 分钟内 10 次 | 常量 | 计数放在进程内存里，Control Plane 重启后清零 |
| OIDC `allow_signups` | 关 | 部署 | 没有任何 User 时第一个 OIDC 登录者不受它限制（ADR-0020） |
| OIDC `email_domains` | 空（不限） | 部署 | |
| 新 User 的默认 Quota | `{gpus: 0, sandboxes: 0}` | 部署 | OIDC 自动建号和 `POST /users` 不带 quota 时都用它 |

## 部署配置项

不是数字，但和上面的「部署」项一样，由 Control Plane 的部署配置给出。

| 名称 | 未配置时 | 说明 |
|---|---|---|
| `STARS_ACCESS_URL` | Git 关联关闭 | 外部可达的 `api` 地址，对应 Coder 的 `CODER_ACCESS_URL`。OAuth `redirect_uri`、problem 里的 `authorize_url`、沙箱内的 `STARS_URL` 都由它拼出。沙箱内的 CLI 只信任镜像自带的 CA，部署指南要写明：用私有 CA 时，镜像里得装上它 |
| `STARS_ENCRYPTION_KEYS_FILE` | Secret、Git、Sandbox Token 相关功能返回明确错误 | 每行 `<key_id> <base64 编码的 32 字节>`。第一行用于加密，所有行都可用于解密。AES-256-GCM，AAD 为 owner 的 user id（`schema.md`）。不提供 rewrap 命令 |
| `STARS_GIT_PROVIDERS_FILE` | 没有 Git Provider | JSON 数组，见下 |

`STARS_GIT_PROVIDERS_FILE` 的每一项：

| 字段 | 说明 |
|---|---|
| `id` | `/git-providers/{id}` 里的 id |
| `type` | `github`、`gitlab`、`gitea`、`generic` |
| `client_id`、`client_secret_file` | OAuth 应用；secret 从文件读 |
| `base_url` | 按类型推导各 endpoint（ADR-0017）。`github`、`gitlab` 默认指向公网；`gitea` 必须写 |
| `auth_url`、`token_url`、`validate_url` | 只用于 `generic`，因为它没有类型可推导：`auth_url`、`token_url` 必须写，`validate_url` 可以不配，不配就跳过校验 |
| `pkce` | 默认 `true`，即 PKCE S256，可按 provider 关闭（ADR-0017） |
| `host` | 匹配仓库 host 的模式 |
| `scopes` | 授权时请求的 scope |

校验路径按类型内置，接在 API 基址后：`github` 为 `/user`，`gitlab` 为 `/api/v4/user`，`gitea` 为 `/api/v1/user`。
