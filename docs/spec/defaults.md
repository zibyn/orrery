# 数字默认值

规格里每个数字都以本表为准。「谁能改」一列：**常量**写死在代码里，改了就要发版；**部署**指 Control Plane 的部署配置；**Node** 指 Node Agent 的配置；**请求**指单次 API 请求的字段。等到真有人需要改某个常量时，再把它升级成开关。purge 类的配置项里，`0` 都表示不删。

## Node 与 Control Plane

| 名称 | 默认值 | 谁能改 | 说明 |
|---|---|---|---|
| 心跳间隔 | 15 秒 | 常量 | 与 Coder 一致 |
| 判定 Unreachable | 30 秒 | 常量 | 心跳间隔的 2 倍（ADR-0014） |
| Enrollment Packet 有效期 | 1 小时 | 部署 | 对应 GitHub runner 注册令牌的有效期；Coder 和 Daytona 只有长期 key，相当于我们的 Node Token |
| purge 周期 | 10 分钟 | 常量 | ADR-0014 |
| 磁盘水位 | 剩余低于 10% | Node | 低于水位就不接新分配，开始按 LRU 淘汰 Resource 缓存、Agent 的 containerd namespace 里未被本 Node 上任何 Sandbox（含 stopped）引用的镜像和 BuildKit 缓存（`runtime.md` §11）；对应 kubelet 的 `nodefs.available<10%` |
| 宿主预留 | 1 CPU + 2 GiB | Node | 从总量里扣掉，不参与分配 |
| 每 GPU 份额 | 可分配量 × 3/4 ÷ 卡数 | Node | CPU 和内存各算一份；剩下的 1/4 给无 GPU 的 Sandbox（ADR-0015） |
| Sandbox 子网 | `10.213.0.0/24` | Node | `--sandbox-subnet`，containerd Node 的 CNI 地址池（`runtime.md` §13）。多台 Node 必须各用一段、互不重叠，例如都从 `10.213.0.0/16` 里各取一个 `/24`；默认值只适合单 Node。重叠的两台 Node 不会互为 Mesh peer（ADR-0027） |
| Mesh 端点 | 不设 | Node | `--mesh-endpoint host:port`，其他 Node 能直接访问到的 WireGuard UDP 地址；不设表示这台 Node 只主动发起（`runtime.md` §15） |
| Mesh 监听端口 | `51820` | Node | `--mesh-port`；设了 `--mesh-endpoint` 时它就是要对其他 Node 开放的 UDP 端口 |
| Mesh keepalive | 25 秒 | 常量 | 对每个带端点的 peer 发送，维持 NAT 映射 |
| Mesh 上报间隔 | 30 秒 | 常量 | `MeshReport` |
| Mesh 通路判定 | 最近一次握手在 180 秒内 | 常量 | WireGuard 每 120 秒重新握手，再留一个上报间隔的余量 |

## 调度与生命周期

| 名称 | 默认值 | 谁能改 | 说明 |
|---|---|---|---|
| 准入期限 | 1 小时 | 部署 | 到期后以 `failed/admission_timeout` 结束 |
| Idle Stop | 900 秒 | 部署给默认值，请求可覆盖 | `idle_stop`，`0` 表示关闭；与 Daytona 一致 |
| Workspace provisioning 重试 | 5 次，间隔 1、2、4、8、16 秒 | 常量 | 第 3 次失败起挂上 `provisioning_stalled`，用尽后 `failed` |
| Resource 拉取重试 | 5 次，间隔 1、2、4、8、16 秒 | 常量 | 与 Workspace provisioning 同一组退避；用尽后 Sandbox 以 `failed/resource_fetch_failed` 结束 |
| 构建资源上限 | 4 CPU + 8 GiB | 部署 | 构建不计入 User Quota（ADR-0021） |
| 构建超时 | 1 小时 | 部署 | 超时以 `failed/timed_out` 结束 |

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
| manifest 条目上限 | 1,000,000 | 常量 | ADR-0012 |
| upload 会话有效期 | 24 小时 | 常量 | 期内带同一份 manifest 重复提交即续传 |

## API 与 Gateway

| 名称 | 默认值 | 谁能改 | 说明 |
|---|---|---|---|
| `wait_for` 最长挂起 | 60 秒 | 常量 | |
| 分页 | 默认 50，上限 200 | 常量 | |
| Idempotency Key 保留期 | 24 小时 | 常量 | |
| Gateway 端口票据有效期 | 60 秒 | 常量 | ADR-0017 |
| 每个 User 的 Secret | 最多 50 个，每个最大 24 KiB | 常量 | ADR-0017 |
| `links.from` 项数 | 最多 16 项 | 常量 | 创建请求里的 Sandbox Link 来源（`api.md` §3） |
| `links.ports` 项数 | 最多 16 项 | 常量 | 每一项在来源和目标两边各渲染成 nftables 规则 |
| `egress.allow` 项数 | 最多 64 项 | 常量 | 创建请求里的 allowlist；每一项在 Node 上渲染成 nftables 规则（`api.md` §3） |
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

## 部署配置

Control Plane 的应用配置只有一个文件：`ORRERY_CONFIG_FILE` 指向的 YAML。安装脚本写入 `/etc/orrery/config/config.yaml`，容器内为 `/config/config.yaml`；文件 `0600`、属主为 server 运行用户，以目录只读挂载。未设置 `ORRERY_CONFIG_FILE` 时，server 读取现有的环境变量，供测试与 CI 使用；那不是部署契约。

键名去掉 `ORRERY_` 前缀、使用 snake_case，按功能分组。标量键可以被「覆盖变量」列的环境变量覆盖；列表和映射只从文件读取。被覆盖的键在启动时逐行打印。未知键、非法值、重复的 provider id、`database.url` 与结构化字段同时出现，都会让启动失败。不做环境变量插值，没有热加载。

### `access`

| 键 | 默认 | 覆盖变量 | 说明 |
|---|---|---|---|
| `url` | 空 | `ORRERY_ACCESS_URL` | 外部可达的 `api` 地址，对应 Coder 的 `CODER_ACCESS_URL`。OAuth `redirect_uri`、problem 里的 `authorize_url`、沙箱内的 `ORRERY_URL` 都由它拼出；空时 Git 关联关闭。沙箱内的 CLI 只信任镜像自带的 CA，部署指南要写明：用私有 CA 时，镜像里得装上它 |
| `apps_domain` | 空 | `ORRERY_APPS_DOMAIN` | 端口转发的泛域名，子域名为 `<port>--<sandbox_id>.<apps_domain>`（`api.md` §6）；空时浏览器 Gateway 关闭，CLI 隧道仍可用 |
| `ticket_key` | 空 | `ORRERY_PORT_TICKET_KEY` | 端口转发票据与 Gateway cookie 的 HMAC-SHA256 密钥；空时每个进程随机生成，重启后已签发的票据和 cookie 失效 |

### `database`

| 键 | 默认 | 覆盖变量 | 说明 |
|---|---|---|---|
| `url` | 空 | `ORRERY_DATABASE_URL` | 完整 DSN，与下面的结构化字段互斥；接外部 PostgreSQL（TLS、额外参数）时用它 |
| `host` | 空 | `ORRERY_DATABASE_HOST` | 内置 PostgreSQL 为 `postgres`；`url` 为空时必填 |
| `port` | `5432` | `ORRERY_DATABASE_PORT` | |
| `user` | 空 | `ORRERY_DATABASE_USER` | `url` 为空时必填 |
| `password` | 空 | `ORRERY_DATABASE_PASSWORD` | `url` 为空时必填 |
| `name` | 空 | `ORRERY_DATABASE_NAME` | `url` 为空时必填 |

### 生命周期、配额、日志与构建

| 键 | 默认 | 覆盖变量 | 说明 |
|---|---|---|---|
| `enrollment_packet_ttl` | `1h` | `ORRERY_ENROLLMENT_PACKET_TTL` | Enrollment Packet 有效期 |
| `quotas.default_gpu` | `0` | `ORRERY_DEFAULT_GPU_QUOTA` | 新 User 的默认 GPU Quota |
| `quotas.default_sandbox` | `0` | `ORRERY_DEFAULT_SANDBOX_QUOTA` | 新 User 的默认 Sandbox Quota |
| `lifecycle.pty_idle_seconds` | `300` | `ORRERY_PTY_IDLE_SECONDS` | PTY 空闲回收，秒 |
| `lifecycle.idle_stop_seconds` | `900` | `ORRERY_IDLE_STOP_SECONDS` | Idle Stop，秒，`0` 表示关闭 |
| `lifecycle.admission_timeout` | `1h` | `ORRERY_ADMISSION_TIMEOUT` | 准入期限 |
| `builds.cpu` | `4` | `ORRERY_BUILD_CPU` | 构建 CPU 上限 |
| `builds.memory` | `8589934592`（8 GiB，字节） | `ORRERY_BUILD_MEMORY` | 构建内存上限 |
| `builds.timeout` | `1h` | `ORRERY_BUILD_TIMEOUT` | 构建超时 |
| `logs.exec_cap` | `16777216`（16 MiB，字节） | `ORRERY_EXEC_LOG_CAP` | Execution Log 上限 |
| `logs.exec_retention` | `720h` | `ORRERY_EXEC_LOG_RETENTION` | Execution Log 保留期，`0` 表示永久 |
| `logs.audit_retention` | `0` | `ORRERY_AUDIT_RETENTION` | Audit Record 保留期，`0` 表示永久 |
| `logs.resource_version_ttl` | `720h` | `ORRERY_RESOURCE_VERSION_TTL` | 非 current、无引用的 ResourceVersion 保留期 |
| `logs.cas_grace` | `24h` | `ORRERY_CAS_GRACE` | CAS 对象与未完成上传的 GC 宽限 |

带 `_seconds` 的键是整数秒；其余时长使用 Go duration 语法（`1h`、`720h`）。字节数以整数表示。

### `egress`

Egress Rules（ADR-0026）。两个列表，只从文件读取，没有覆盖变量：

| 键 | 默认 | 说明 |
|---|---|---|
| `allow` | `[]` | 一定放行的目的地，优先于 `deny`，对任何 Egress Mode 的 Sandbox 都生效 |
| `deny` | `[]` | 一定丢弃的目的地；Egress Mode 无法越过它 |

每一项是 `CIDR[:port]`：IPv4 CIDR（单个地址等于 `/32`），可带端口 1–65535；带端口时只匹配该端口的 TCP 和 UDP。启动时只校验语法，不要求任何条目，非法项让启动失败。Control Plane 经 `HelloAck` 下发给每个 Node（`node-protocol.md`），只作用于此后创建或 start 的 Sandbox。Local Node 不受 Egress Rules 约束。

> **空的 Egress Rules 等于完全放开。** 不写 `egress`，或两个列表都为空时，Sandbox 能到达 Node 能到达的一切：云 metadata、PostgreSQL、对象存储、registry、Node 所在内网，以及其他 Sandbox。平台不加任何隐含条目，`api`、DNS 能否访问也完全由这里决定。部署时应当从下面的推荐配置开始。

**推荐配置**。`deny` 照抄即可；`allow` 是示例，按部署填写：

```yaml
egress:
  allow:
    # 填 access.url 解析到的地址。Sandbox 要靠它做 Git 认证（ADR-0016），
    # 它多半落在下面 deny 的私有网段里，不放行 Git 就会失败。
    # - 10.0.0.5/32:443
    # 填 Sandbox 的 resolv.conf 里的 nameserver（宿主的上游 resolver，runtime.md §2）。
    # 它在私有网段里时不放行，Sandbox 就解析不了域名。
    # - 10.0.0.2/32:53
  deny:
    - 169.254.169.254/32   # 云 metadata
    - 10.213.0.0/16        # Sandbox 子网（与各 Node 的 --sandbox-subnet 一致），Sandbox 之间互不可达；Sandbox Link 不受它约束（ADR-0027）
    - 10.0.0.0/8           # RFC 1918 私有网段：PostgreSQL、对象存储、registry 和 Node 所在内网
    - 172.16.0.0/12
    - 192.168.0.0/16
```

- `deny` 了 `api` 的地址又没有 `allow` 回来，Git 认证会失败，平台不检测（ADR-0026）。
- Control Plane 或 Node 用公网地址时，把这些地址也加进 `deny`，再按需 `allow` `api`。
- 改了 Sandbox 子网，同步改这里的条目。

### `encryption_keys`

列表，每项 `{id, key}`；`key` 是 base64 编码的 32 字节。第一项用于加密，所有项都可用于解密。AES-256-GCM，AAD 为 owner 的 user id（`schema.md`）。空列表时 Secret、Git、Sandbox Token 相关功能返回明确错误。不提供 rewrap 命令。

### `git_providers`

列表，为空表示没有 Git Provider。每一项：

| 字段 | 说明 |
|---|---|
| `id` | `/git-providers/{id}` 里的 id |
| `type` | `github`、`gitlab`、`gitea`、`generic` |
| `client_id`、`client_secret` | OAuth 应用；两者都必填，secret 直接写在文件里 |
| `base_url` | 按类型推导各 endpoint（ADR-0017）。`github`、`gitlab` 默认指向公网；`gitea` 必须写 |
| `auth_url`、`token_url`、`validate_url` | 只用于 `generic`，因为它没有类型可推导：`auth_url`、`token_url` 必须写，`validate_url` 可以不配，不配就跳过校验 |
| `pkce` | 默认 `true`，即 PKCE S256，可按 provider 关闭（ADR-0017） |
| `host` | 匹配仓库 host 的模式 |
| `scopes` | 授权时请求的 scope |

校验路径按类型内置，接在 API 基址后：`github` 为 `/user`，`gitlab` 为 `/api/v4/user`，`gitea` 为 `/api/v1/user`。

### `oidc`

| 键 | 默认 | 覆盖变量 | 说明 |
|---|---|---|---|
| `issuer_url` | 空 | `ORRERY_OIDC_ISSUER_URL` | 空表示 OIDC 关闭 |
| `client_id` | 空 | `ORRERY_OIDC_CLIENT_ID` | 配置 issuer 时必填 |
| `client_secret` | 空 | `ORRERY_OIDC_CLIENT_SECRET` | 配置 issuer 时必填；直接写在文件里 |
| `email_domains` | `[]` | 无（列表仅文件） | 空表示不限 |
| `allow_signups` | `false` | `ORRERY_OIDC_ALLOW_SIGNUPS` | 没有任何 User 时，第一个 OIDC 登录者不受它限制（ADR-0020） |

### `s3`

| 键 | 默认 | 覆盖变量 | 说明 |
|---|---|---|---|
| `endpoint` | `http://rustfs:9000` | `ORRERY_S3_ENDPOINT` | Control Plane 容器可达的内部地址；空表示对象存储关闭 |
| `public_endpoint` | 同 `endpoint` | `ORRERY_S3_PUBLIC_ENDPOINT` | 预签名 URL 的基址（Node 可达） |
| `bucket` | `orrery` | `ORRERY_S3_BUCKET` | 只用一个 bucket |
| `region` | `us-east-1` | `ORRERY_S3_REGION` | |
| `access_key` | `orrery` | `ORRERY_S3_ACCESS_KEY` | |
| `secret_key` | 空 | `ORRERY_S3_SECRET_KEY` | 启用对象存储时必填 |

### `registry`

| 键 | 默认 | 覆盖变量 | 说明 |
|---|---|---|---|
| `addr` | 空 | `ORRERY_REGISTRY_ADDR` | Node 拉取镜像的地址；空时平台 registry 相关功能失败 |
| `push_addr` | 同 `addr` | `ORRERY_REGISTRY_PUSH_ADDR` | Control Plane 推送的地址；容器网络与主机视角不同时覆盖 |
| `username` | 空 | `ORRERY_REGISTRY_USERNAME` | 与 password 一起拼出 auth JSON |
| `password` | 空 | `ORRERY_REGISTRY_PASSWORD` | 与 username 一起拼出 auth JSON |

### `platform_snapshots`

列表，每项 `{name, ref, default}`。`ref` 形如 `…@sha256:…`，`name` 是 registry 里的 `platform/<name>`。Control Plane 启动时导入其中还没导入的条目；导入失败不影响启动，只让用到它的 Sandbox 创建失败。平台 Snapshot 所有 User 可读，API 不能增删（ADR-0021）。`default: true` 最多一项，多于一项启动失败；它是不带 `snapshot` 的 `runtime: container` 创建请求的默认值；没有默认项时该请求返回错误，`runtime: local` 不用 Snapshot。空列表表示没有平台 Snapshot。

**不出现在 `config.yaml` 的键**。`api_addr`、`node_addr`、`node_tls_cert`、`node_tls_key` 由代码默认值给出，只可用环境变量（`ORRERY_API_ADDR`、`ORRERY_NODE_ADDR`、`ORRERY_NODE_TLS_CERT`、`ORRERY_NODE_TLS_KEY`）覆盖。

**环境变量模式**。`ORRERY_CONFIG_FILE` 未设置时保留现有变量名，包括 `ORRERY_ENCRYPTION_KEYS_FILE`、`ORRERY_GIT_PROVIDERS_FILE`、`ORRERY_OIDC_CLIENT_SECRET_FILE`、`ORRERY_PLATFORM_SNAPSHOTS`、`ORRERY_DEFAULT_SNAPSHOT`，用于测试和 CI。`ORRERY_TEST_PURGE_INTERVAL` 是测试钩子，不属于部署配置。

**对象存储布局**。只用一个 bucket：CAS 对象在 `cas/<owner_id>/sha256/<hex>`，manifest 在 `manifests/<owner_id>/<address>`，registry 的 s3 driver 用 `rootdirectory: /registry`。registry 里用户 Snapshot 在 `snapshots/<snapshot_id>`，平台 Snapshot 在 `platform/<name>`，一律按 digest 引用，不打 tag。e2e 和参考部署用 RustFS 1.0.2；安装脚本用一个一次性容器建 bucket。
