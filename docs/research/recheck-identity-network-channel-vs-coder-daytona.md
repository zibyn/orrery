# 重查 #37（身份 / 密钥 / 网络 / 审计，ADR-0010）与 #40（Capability Channel，ADR-0011）：对照 Coder 与 Daytona

调研日期 **2026-10-17**。只用一手来源：官方文档和钉死版本的源码。厂商原文保留英文。找不到一手理由的写「未找到一手理由」，推断标【推断】。

钉死版本与链接简写：

| 项目 | tag | commit | 简写 |
|---|---|---|---|
| coder/coder | v2.38.0 | `c3c6a672821e8ac2472c45f3be7a3c600aa8dae1` | `C/` = `https://github.com/coder/coder/blob/c3c6a672821e8ac2472c45f3be7a3c600aa8dae1/` |
| daytonaio/daytona | v0.190.0 | `01c502bb1f1ff8f2885d0cd490e043736083dca8` | `DT/` = `https://github.com/daytonaio/daytona/blob/v0.190.0/` |

两家的文档都放在各自仓库里（Coder 在 `docs/`，Daytona 在 `apps/docs/src/content/docs/en/`），下文引的文档也是这两个钉死版本里的文件。

**复用的既有调研（下文不再重复取证）**：`sandbox-network-policy-apis.md`（14 家出网字段与缺口）、`nftables-netns-egress-ruleset.md`（规则集真机实测）、`prior-art-sandbox-orchestration.md` §1.10 / §4.3 / §7（Coder agent 形状、注册信任、kubelet 三段式）、`sandbox-git-integration.md` §B.3 / §B.5（Coder askpass、Daytona git 动词）、`control-plane-operations-vendor-practice.md` §1（Coder 的 PG 与 dbpurge）。同批的 `recheck-*.md` 三份只作参考，没有改。

读的是当前工作区里的 ADR-0010 / 0011 / 0013 / 0014，以及新出现的 ADR-0015。ADR-0015 去掉了 Lease 的 TTL，也去掉了 ExecAttempt。

---

## 0. 先读这几条

1. **应简化：Node 身份不需要私有 CA + CSR + 50% 续期。** 两家用的都是「每台机器一个 bearer token，由管理员签发」。Coder 的 provisioner key 存哈希，可以按 org 和 tag 限定作用域（`C/docs/install/operate/provisioners/index.md#L45-L48`、`C/coderd/database/dump.sql#L3010-L3017`）。Daytona 的 runner `apiKey` 在 `POST /runners` 时生成，按明文列查找（`DT/apps/api/src/sandbox/services/runner.service.ts#L97`、`#L262-L264`）。两家都没有 per-agent 证书。Coder 支持 mTLS，但只是整站的客户端证书开关（`--tls-client-auth`），没有为 agent 签证书（`C/docs/reference/cli/server/index.md#L132-L141`）。我们给 mTLS 写下的理由（GitLab 共享 token 无法追溯）只能证明「每 Node 一个 token」是对的，证明不了「必须用证书」。唯一真实的收益是 ADR-0014 让 registry 复用了 Node 证书（`clientcas`）。所以要简化，就得连带改 registry 的认证。
2. **应修改：「沙箱里的服务从外面不可达」比两家都差一大截。** 两家都把它当作核心功能，而且用的都是**不在沙箱网络上开入站口**的做法。Coder 的 workspace app 和 port-forward 是这样走的：coderd 经 tailnet 找到 agent，agent 在 workspace 内部拨 `localhost:PORT`（`C/docs/admin/networking/port-forwarding.md#L9-L16`、`#L95-L106`）。agent 的端口开在 gVisor 用户态 netstack 上，不在 workspace 的网络栈上（`C/tailnet/conn.go#L53-L56`、`#L278-L294`）。Daytona 的 preview URL 能开 `1`–`65535` 任意端口（`DT/apps/docs/src/content/docs/en/preview.mdx#L8`），由 daemon 的 `/proxy/:port` 转到沙箱内的本地端口（`DT/apps/daemon/pkg/toolbox/server.go#L367-L370`）。**「零监听端口、不被连入」可以保留**，同时补一条只给 owner 的 Gateway 端口转发：由 Node Agent 进入 Sandbox 的 netns 拨 `127.0.0.1:PORT`，形状照 Coder。
3. **应修改：「Gateway 的每一次授权判定都写进同库同事务的审计表」，Coder 已经踩过这个坑。** Coder 从 2.25 起把 SSH、端口转发、浏览器这类连接事件从 Audit Log 挪进了单独的 Connection Log，并且裁掉了 90 天以前的旧行（`C/docs/install/operate/upgrade/esr-2.24-2.29-upgrade.md#L106`、`C/coderd/database/dbpurge/dbpurge.go#L23-L27`）。tunnel 授权事件还要按 agent / actor / IP / client / 结果去重，一小时内重复的请求不再记（`C/docs/admin/monitoring/connection-logs.md#L41-L46`）。Gateway 的判定不是状态变更，所以「同事务」对它没有意义，只是在热读路径上多一次写。做法是：控制层的变更保留同事务审计；Gateway 判定只记会话建立和拒绝，并去重。
4. **保留：审计和状态同库同事务，比两家都严，而且不比两家复杂。** Coder 的审计在 handler 返回后由 deferred 的 commit 单独 insert，写失败**只打日志**（`C/coderd/audit/request.go#L509-L606`），OSS 版还是 `nop`（`C/coderd/audit/audit.go#L27-L35`，审计是 Premium，`C/docs/admin/security/audit-logs.md#L1-L8`）。Daytona 先 insert 再执行 handler，insert 失败就返回 500，然后再 update 结果（`DT/apps/api/src/audit/interceptors/audit.interceptor.ts#L64-L118`）。两家都不在同一事务里。我们同库，在同一事务里多一条 INSERT，比两家都省事。
5. **保留：netns 内的 nftables + 创建期大声失败，这正好是 Daytona 踩过的坑的反面。** Daytona 的出网规则装在宿主 `DOCKER-USER` 链上，按**源 IP** 跳到每沙箱一条的链（`DT/apps/runner/pkg/netrules/set.go#L35-L46`），只管 IPv4（`netrules.go#L30`）。规则在容器**启动之后**由 goroutine 异步装上，失败只记一条日志（`DT/apps/runner/pkg/docker/create.go#L242-L264`）。也就是说装失败时是 fail-open，而且沙箱一启动就有一段没有规则的窗口。Coder 的 Agent Firewall（Premium）用的是「在 workspace 里造 netns + veth + iptables」（`C/docs/ai-coder/agent-firewall/index.md#L175-L185`），是「netns 内自建规则集」的一个先例。`sandbox-network-policy-apis.md` 说过「没有一家这么做」，这句要更正。
6. **应简化：Capability Channel 上为 Workspace 写动词预留的 fencing 和 Attachment 规则，现在没有任何动词用得到。** 动词表只有 `git.credential`，它不改 Workspace。socket、Node Agent 服务、逐请求授权、封闭动词表这几样**保留**。Coder 的对应物更重：它把 `CODER_AGENT_TOKEN` 放进 workspace 里每个进程的环境变量（`C/agent/agent.go#L1704-L1705`），askpass 拿着这枚 token 走 HTTPS 回 coderd（`C/cli/gitaskpass.go#L47-L80`）。我们这条通道不带凭据，也不依赖网络，是更省的做法。

---

## 1. 逐条裁决对照表

| # | 裁决（ADR / 票据） | Coder v2.38.0 | Daytona v0.190.0 | 判定 |
|---|---|---|---|---|
| I1 | 每 User 静态 API Token，TLS，库里存哈希，scope 只有 `user` / `admin`；不用 OIDC | 每个用户可以有**多枚具名** token，SHA-256 存哈希（`C/coderd/apikey/apikey.go#L57`、`#L111`、`#L154-L157`；`dump.sql#L1792-L1809`）。有 `expires_at` 和 `last_used`，可选 `resource:action` scope 和 allow-list（`C/docs/admin/users/sessions-tokens.md#L124-L171`）。最长寿命默认 `876600h`，约 100 年（`C/docs/reference/cli/server/index.md#L961-L970`）。另外有会被刷新的短期 session token（同页 `#L7-L33`） | `dtn_` + 32 字节随机数，SHA-256 存哈希（`DT/apps/api/src/common/utils/api-key.ts#L12-L18`）。主键是 (org, user, **name**)，同样多枚具名；可选 `expiresAt`，有 `lastUsedAt`（`DT/apps/api/src/api-key/api-key.entity.ts#L9-L50`）；权限是 `write:sandboxes` 这类 scope（`DT/apps/docs/.../api-keys.mdx#L24`）。登录走 Auth0 JWT | **应修改（小）**：存哈希加 scope，与两家同形。但两家都是**每个用户多枚具名 token，每枚可单独吊销**，各自有过期时间和 `last_used`。ADR 里的「Per-User static API tokens」没写清楚。Agent 应该各拿一枚，泄漏时只吊销那一枚 |
| I2 | `admin` 不能是 Agent 手里的那枚 | Owner 才能替别人造 token（`sessions-tokens.md#L66-L69`）；provisioner 推荐用专用 key，不用用户 token（`provisioners/index.md#L45-L48`、`#L85-L90`） | 管理员和成员是 org 角色；runner、proxy、ssh-gateway 各自用独立的 key 识别角色（`DT/apps/api/src/auth/api-key.strategy.ts#L80-L98`） | **保留** |
| I3 | Node 身份：私有 CA 签客户端证书，一次性 Enrollment Packet 走 CSR 首签，另走一条 RPC 在 50% 寿命时续期；撤销 = 删行 + 拒续期 | **没有 per-agent 证书**。provisioner 有三种认证：scoped key（推荐，存哈希）、用户 token、全局 PSK（不推荐：*"Global pre-shared keys (PSK) make it difficult to rotate keys or isolate provisioners."*，`provisioners/index.md#L109-L113`）。workspace agent 用每个 agent 一枚的 `auth_token uuid`，**明文**存在 `workspace_agents` 表里并建了索引（`dump.sql#L3755`、`#L5003`）。mTLS 只是整站的 `--tls-client-auth` 加 `--tls-client-ca-file`（`server/index.md#L122-L141`） | runner 用一枚 bearer key，`POST /runners` 时生成，**明文**存在 `runner.apiKey` 列（`runner.service.ts#L97`；`runner.entity.ts#L36-L37`）。两个方向用的是同一枚：控制层调 runner 时带 `Bearer ${runner.apiKey}`（`runnerAdapter.v0.ts#L100-L108`），runner 侧逐字比较（`DT/apps/runner/pkg/api/middlewares/auth.go#L41`）。TLS 可选（`ENABLE_TLS`，`config.go#L26`） | **应简化**：改成「admin 签发 → 一枚 per-Node token，存哈希；撤销 = 删行」，即 Coder provisioner key 的形状。「失败语义不对称」那条要留下：它和凭据是什么形式无关。代价是 ADR-0014 的 registry 认证不能再靠 `clientcas`，见 §2.1 |
| I4 | 平台长期密钥 = 部署期的文件路径引用，不进 PG | 用 flag 或环境变量配：`--external-token-encryption-keys`、`--provisioner-daemon-psk`（`C/docs/admin/security/database-encryption.md#L9-L16`） | 用环境变量配：`DAYTONA_RUNNER_TOKEN`、`AWS_SECRET_ACCESS_KEY` 等（`DT/apps/runner/cmd/runner/config/config.go#L19-L45`）；proxy 和 ssh-gateway 的 key 也来自配置（`api-key.strategy.ts#L79-L86`） | **保留**：同形 |
| I5 | 调用方 Secret 与 Git token 加密存在 PG（ADR-0013） | **默认明文**：*"By default, Coder stores external user tokens and other sensitive values in plaintext in the database."* 配置加密后用 AES-256-GCM，每个加密列配一个 `$C_key_id` 列，用来轮换密钥（`database-encryption.md#L5-L7`、`#L18-L45`），`user_secrets.value` 和 `external_auth_links.*` 都在加密范围内 | Secret 是占位符加出口代理（见 `prior-art-sandbox-orchestration.md` §1.2） | **保留**，**另抄一处**：照 Coder 加一列 `key_id`，否则加密密钥将来没法轮换 |
| N1 | 出网默认 `public`；私网、CGNAT、loopback、link-local、metadata、宿主地址无条件拒绝；控制层地址无条件减去 | **核心产品里没有出网策略**。做法留给模板作者：一种是在模板里写 K8s NetworkPolicy，官方示例**只放行 Coder 的 IP 和端口，连 DNS 都不放**（`C/docs/ai-coder/agents/platform-controls/agent-isolation.md#L133-L156`、`#L328`）；另一种是 Premium 的 Agent Firewall，按域名和 HTTP 方法放行（`agent-firewall/index.md#L26-L30`）。指南建议 agent 模板 *"Network egress is restricted to the control plane and git provider."*（`template-optimization.md#L316`） | Tier 3/4 默认全开（`network-limits.mdx#L14-L15`）。runner 源码里**找不到**对私网或 `169.254.169.254` 的拒绝（在 `apps/runner`、`apps/api/src` 里 grep 不到）。`networkBlockAll` 和 `networkAllowList` 都是**替换式**的：一条「允许列表的 RETURN + 最后 DROP」（`set.go#L33-L42`） | **保留**：拒绝集合是编译期常量，代价很小。两家把这件事推给了模板或基础设施，我们这一侧更严。【推断】Daytona 的 metadata 防护在云网络那一层，OSS 源码里看不到 |
| N2 | 调用方的 `allow: [CIDR]` 只是请求，平台维护的 Egress Opening 名单决定它能不能落到被拒绝的目的地；落不进就在创建期失败；名单的每次改动都进审计 | 网络策略写在**模板**里，所以由模板管理员决定，workspace 用户改不了。这和「调用方开不了口子」同构 | 调用方可以直接开私网：官方示例就写着 `192.168.1.0/24` *"private network"*（`network-limits.mdx#L33-L35`） | **应简化（小）**：原则保留，它就是 Coder「模板管理员定网络」在我们这里的对应。但 Egress Opening 不必做成「API 资源 + 审计」，做成**部署配置里的一张 CIDR 表**就够了（和 ADR-0013 的 Git Providers 同一种写法）。改它要改部署配置，审计也就没有必要了 |
| N3 | 强制点在 netns 内的 nftables（`inet` 表，v4/v6 并列），由 Node Agent 在 `connect` 之前装好 | Agent Firewall 的 nsjail：在 workspace 里建 netns + veth + iptables，再透明代理到 HTTP proxy（`agent-firewall/index.md#L175-L185`；`nsjail/index.md#L20-L27`），需要 `CAP_NET_ADMIN`。它自己的对照表说 Landlock 版 *"can bypass by connecting to evil.com:<HTTP_PROXY_PORT>"*（同页 `#L180`） | 宿主 `filter/DOCKER-USER` 第一条按 `-s <容器IP>` 跳到 `DAYTONA-SB-<id>` 链（`set.go#L46`；`utils.go#L53-L58`），只有 IPv4（`netrules.go#L30`）。规则在**容器启动后用 goroutine 异步**装，失败只打日志（`create.go#L242-L264`）。源码注释自己承认，按地址匹配会被跨网桥伪造的包绕过：*"a privileged container on any other bridge can craft a packet with src/dst in the link subnet"*（`link_icc.go#L20-L26`） | **保留**：规则跟着 netns 走，不依赖源 IP，也没有 IP 复用的竞态；我们在 connect 之前装好、装不上就让创建失败，正好补上 Daytona 的 fail-open 窗口。先例是 Coder nsjail（netns 内规则集），不是「没有先例」 |
| N4 | DNS 没有策略面，作为已知残余面写进规格 | Agent Firewall 默认用**假 DNS**，`use_real_dns` 会 *"permits DNS-based data exfiltration"*（`agent-firewall/index.md#L135-L137`）；NetworkPolicy 示例直接不放 DNS（`agent-isolation.md#L328`） | 文档没写 | **保留**：我们的档位只有 `public`（本来就能经 HTTPS 外泄）和 `none`（没有网络），DNS 外泄在这两档下都不是新增的面。等将来出现「受限但有网」的档位，再照 Coder 的假 DNS 去做 |
| N5 | 被拦的连接从里面看可能仍然「成功」，要写进文档 | 示例测试要求：*"A DNS error does not prove that the policy blocks connections to IP addresses."*，还要求用 `--resolve` 直连 IP 再测一遍（`agent-isolation.md#L372-L381`）；*"Kubernetes can accept a NetworkPolicy without enforcing it"*（`#L47-L49`） | 文档没写 | **保留**；Coder 那段「怎么证明策略真的生效」的验收步骤可以照抄进实现规格 |
| G1 | 调用方只经 Control Plane 和 Gateway 进入；Node Agent 只往外拨，不被直连 | agent 往外连 coderd（*"There must not be a NAT between workspaces and the control plane."*，`C/docs/admin/networking/index.md#L71-L74`）。**客户端可以 P2P 直连 agent**（WireGuard），打不通时走 DERP 中继（同页 `#L11-L14`、`#L30-L67`） | v0 runner **自己监听**，被控制层和 proxy 连入（`runnerAdapter.v0.ts#L100-L108`；`DT/apps/runner/pkg/api/controllers/proxy.go#L51-L96`）；v2 runner 改成轮询（`runnerAdapter.v2.ts#L50`） | **保留**：比两家都窄，代价是全部流量都经过 Gateway。我们是单团队、同一内网，这个代价可以接受 |
| G2 | Sandbox 不监听任何端口、永不被连入；**沙箱里的服务从外面不可达** | agent 的服务端口开在 tailnet 的**用户态 netstack** 上（`tailnet/conn.go#L53-L56`、`#L278-L294`），不在 workspace 的网络栈上。workspace app 和 port-forward 都是 agent **在 workspace 内部拨 `localhost:PORT`**（`port-forwarding.md#L83-L98`），共享级别分 `owner` / `authenticated` / `public`（`#L109-L111`）。path-based app 和 Coder API 同源，官方要求关掉：*"Path-based apps can make requests to the Coder API and pose a security risk when the workspace serves malicious JavaScript."*（`server/index.md#L929`） | daemon 的 toolbox 在**沙箱内 TCP 2280 监听**（`DT/apps/daemon/pkg/toolbox/config/config.go#L6`；`server.go#L374-L387`），而且这个 server **没有任何认证中间件**：`/init` 收到的 token 只被塞进 OTel 头（`controller.go#L33`；`telemetry.go#L46`）。runner 直接拨 `http://<容器IP>:2280`（`controllers/proxy.go#L96`）。preview 可以开任意端口；`public: true` 时不需要认证；标准 token 在重启时失效；签名 URL 默认 60 秒（`preview.mdx#L8`、`#L17`、`#L104`） | **应修改**：「不在沙箱网络上开入站口」保留。Daytona 的 2280 正好说明了它为什么值得保留：沙箱间 ICC 默认开着（`INTER_SANDBOX_NETWORK_ENABLED` 默认 `true`，`config.go#L35`；只有关掉它才会建 `enable_icc=false` 的网桥，`client.go#L97-L102`）。【推断】默认部署下，同一 runner 上的沙箱能互相打到对方没有认证的 toolbox。**要删的是「服务不可达」这一条**：照 Coder 补一条 owner-only 的 Gateway 端口转发，Node Agent 进 netns 拨 `127.0.0.1:PORT`，用子域名，不做 public 共享 |
| G3 | Gateway 对每个请求向 Control Plane 状态做授权 | app 请求带一张 1 分钟有效的签名 token（`C/coderd/workspaceapps/provider.go#L14-L15`；`token.go#L61-L62`） | proxy 把「授权有效」缓存 2 分钟，「授权无效」缓存 5 秒（`DT/apps/proxy/pkg/proxy/get_sandbox_target.go#L260-L263`）；转发前会删掉 `Authorization` 头（`auth.go#L27-L28`） | **应简化**：授权在会话或 WebSocket 建立时判一次（PTY 和日志本来就是长连接）；无状态的 HTTP 文件请求可以用一张短期签名票据（≤1 分钟，和 Coder 一样）。「逐请求查库」比两家都重，也没有理由。顺带照抄 Daytona 的一条：**转发前剥掉调用方的 API Token** |
| D1 | 所有权就是删除授权，`admin` 是唯一绕过；不存在和不属于你返回同一个 `404`；有活跃 Attachment 或未终态 Exec 时返回 `conflict` | 用 RBAC 角色（owner / template-admin / auditor / member）；404 刻意含糊：*"ResourceNotFound is intentionally vague. All 404 responses should be identical to prevent leaking existence of resources."*（`C/coderd/httpapi/httpapi.go#L162-L168`） | org 角色加 `delete:sandboxes` 这类 scope（`organization-resource-permission.enum.ts`） | **保留**：比两家都小，404 的写法与 Coder 一致 |
| A1 | 审计是同库同事务的追加表 | 写入点在 handler 返回之后，单独 insert，失败只打日志（`C/coderd/audit/request.go#L509-L606`）；后端可以是同一个 PG 或 slog（`C/enterprise/audit/backends/postgres.go#L11-L39`）；**Premium**；带 diff | 先 insert，再执行 handler，最后 update 结果；insert 失败就返回 500（`audit.interceptor.ts#L64-L118`）；可以换成 OpenSearch（`audit-storage.provider.ts#L26-L35`）；toolbox 动作也进审计（`audit-action.enum.ts#L57-L80`） | **保留**：比两家严，不比两家复杂 |
| A2 | 记：控制层变更、Lease、Node、出网与 Opening、删除、**Gateway 的每一次授权判定**、`git.credential` 每次签发 | Gateway 一类的事件已经**迁出**审计表，进了 Connection Log，并且去重（见 §0 第 3 条）；agent 上报的会话事件 *"their receipt by the control plane is not guaranteed"*（`connection-logs.md#L23-L28`）；Agent Firewall 的每次 allow/deny **不进审计表**，而是作为 coderd 的结构化日志输出（`agent-firewall/index.md#L232-L239`）。取 external-auth token 的 handler 里**没有**审计调用（`C/coderd/workspaceagents.go#L2094-L2405`，grep `audit` 零命中） | 审计动作里有 `get_port_preview_url`（`#L41`），没有 proxy 层的逐请求判定（proxy、runner、daemon 里 grep `audit` 零命中） | **应修改**：Gateway 判定只记**会话建立和拒绝**，按 (actor, Sandbox, 结果) 每小时去重，放进单独的表或单独设保留期。`git.credential` 的签发保留进审计，频率低，也正是安全上要看的 |
| A3 | 保留期 ≥ 执行日志保留期，且被引用对象还在时不得清理 | `CODER_AUDIT_LOGS_RETENTION`，默认 `0`，即永久保留（`server/index.md#L2166-L2174`）；一条批量删除（`dbpurge.go#L279-L288`） | `audit.retentionDays`，用 cron 删（`DT/apps/api/src/audit/services/audit.service.ts#L50-L51`、`#L126-L140`） | **应简化（小）**：改成一个按时间算的保留期，默认永久。「被引用对象还在就不能删」要在 purge 里做 join，两家都没有这条 |
| C1 | 每个 Sandbox 一个 unix socket，由 Node Agent 在宿主侧服务；身份 = 请求来自哪个 socket；不按源地址认证 | 沙箱内有一枚**带凭据的** agent token：`CODER_AGENT_TOKEN` 被放进每个子进程的环境（`agent/agent.go#L1704-L1705`），它的 RBAC 作用域被 allow-list 收窄到这个 workspace、它的模板和 owner（`C/coderd/rbac/scopes.go#L25-L64`）。另有一个 workspace **内部**的 unix socket `/tmp/coder-agent.sock`，权限 0600，由 workspace 里的 agent 服务（`agentsocket/socket_unix.go#L15`、`#L40`），走 drpc，动词是 Ping / Sync* / UpdateAppStatus / context（`agentsocket/proto/agentsocket.proto#L148-L176`） | **没有**沙箱到平台的通道。方向反过来：平台 → runner → 沙箱内的 toolbox；git 也是平台 API 推进去的（`sandbox-git-integration.md` §B.5） | **保留**：我们沙箱里没有常驻 agent，宿主侧 socket 是最省的做法。它不带凭据，`egress: none` 下也能用。Coder 那种「环境变量里放一枚 token、走 HTTPS 回控制层」的做法，在我们这里会和「控制层地址无条件减去」直接冲突 |
| C2 | 控制层逐请求授权，同事务审计，不缓存授权；控制层不可达时 `unavailable` | external-auth 每次调用都现场查 DB，按**模板声明过的** provider 收窄；多个 provider 匹配时报错，不替用户挑：*"Report an error rather than guess"*（`workspaceagents.go#L2172-L2207`）；支持 `listen` 长轮询，一直等到用户完成 OAuth（`cli/gitaskpass.go#L111-L116`） | — | **保留**。可选抄一条：`git.credential` 没有已链接账号时，与其失败后返回授权 URL，不如加一个 `wait` 参数长轮询，和 Coder 的 `listen` 一样，让 Agent 不必自己轮询 |
| C3 | Workspace 写动词另要求持有当前 RW Attachment，Node Agent 本地校验 fencing；Lease 被 fence 时返回 `conflict`、终止进行中的请求 | — | — | **应简化**：动词表里只有 `git.credential`，它不改 Workspace，这一整段目前没有调用者。ADR-0015 又去掉了 Lease 的 TTL。等真的出现写动词，再在那个动词的 ADR 里定 |
| C4 | 封闭动词表，HTTP/1.1 + JSON，`curl --unix-socket` 就能当客户端 | Coder 的 agent socket 用 drpc/protobuf（`agentsocket.proto`），客户端是 `coder` 二进制本身 | — | **保留**：没有常驻二进制，就该用最朴素的协议 |
| C5 | `capability_channel`：Docker/Podman 是 `enforced`，Local Runtime 是 `best_effort` | — | — | **保留** |

---

## 2. 和两家不一样的地方：他们为什么没选

### 2.1 mTLS 私有 CA 做 Node 身份

- **一手理由**：两家都**没有**写过为什么不用 per-agent 证书，**未找到一手理由**。
- 两家写下来的，是他们**想解决的问题**。Coder 否定全局 PSK，理由是 *"make it difficult to rotate keys or isolate provisioners"*（`provisioners/index.md#L113`），给出的解法是 per-key 并存哈希，不是证书。
- 【推断】bearer token 只要 per-Node、存哈希、可删行，就已经覆盖了我们 ADR 里写的全部收益：可追溯、可撤销、控制层不碰私钥之外的东西。证书多出来的只有两样：一是「控制层从不见私钥」，可 token 存哈希时控制层同样拿不到明文；二是「传输层同时完成认证」，可 TLS 加 bearer 也能做到。证书带来的代价是 CA 私钥要管、证书过期会造成停机、续期 RPC 要写、时钟漂移要处理。
- **连带问题（必须一起改）**：ADR-0014 里，registry 通过 `http.tls.clientcas` 复用 Node 证书，所以「不需要 token service」。如果 Node 改用 token，registry 就要另找认证方式。最小的替代是 distribution 的 `htpasswd`，每个 Node 一行，但改动后要 reload；另一种是 registry 只放在 Node 网段，配一个共享的 pull/push 凭据。两者都比现在弱，**这是保留 mTLS 唯一站得住的理由**。如果用户更看重 registry 不另起一个认证面，可以退一步：**mTLS 只留给 registry 和 `node` 监听面，砍掉 50% 续期，改成长有效期证书，到期由运维重新 enroll**。

### 2.2 netns 内自建 nftables

- **一手理由**：Coder 选 netns + iptables 的理由写在对照表里：*"Strong - transparent interception prevents bypass"*，代价是 *"Requires `CAP_NET_ADMIN`"*（`agent-firewall/index.md#L177-L183`）。我们的 `CAP_NET_ADMIN` 在宿主侧的 Node Agent 手里，容器不需要（`oci-hook-netns-injection.md` §0 第 3 条）。
- Daytona 为什么挂在宿主 `DOCKER-USER` 上：**未找到一手理由**。【推断】它要同时管 ICC、link network、限速（`mangle` 打 mark 999，`limiter.go#L9-L39`），宿主侧一张表最方便；代价就是源码注释承认的地址伪造面，以及只有 IPv4。

### 2.3 沙箱零监听端口

- 两家都在沙箱内或 agent 上监听，但都**不是直接暴露在沙箱网络上的入站口**：Coder 用用户态 netstack，Daytona 的 2280 只有 runner 能直接到达。【推断】两家都要服务 IDE / 浏览器 / SSH，所以必须有「进到沙箱里」的路径；我们是 Agent 原生的，只有 Exec / PTY / 文件需要这条路径，而这些已经由 Node Agent 承担。
- 两家都给了**沙箱内服务的入口**（Coder app / port-forward，Daytona preview），我们把它整个去掉了，**未找到一手理由支持去掉**。这是本次最大的功能缺口，见 G2。

### 2.4 unix socket 能力通道

- Coder 选「沙箱内 token + HTTPS」的理由：源码注释写的是 *"Specific Coder subcommands require the agent token exposed!"*（`agent/agent.go#L1704`），也就是 CLI 子命令要能直接调 agent API。它自己的 agent-isolation 文档承认了后果：*"The agent can read any credentials you give it, edit shared files, and send data to Coder."*（`agent-isolation.md#L403`）。
- Daytona 没有反向通道，因为它的平台动作都是**从外向里推**的（git 动词是平台 API）。**未找到一手理由**说明他们为什么不做反向通道。

### 2.5 审计同事务

- **未找到一手理由**说明两家为什么不在同一事务里写。【推断】Coder 的审计是可插拔后端（PG / slog，`postgres.go` 里那个 `internal` 标志就是为外部 PG 准备的），Daytona 可以换 OpenSearch 或 Kafka（`audit-storage.provider.ts`）。后端一旦可插拔，就做不到同事务。我们不要可插拔后端，所以同事务几乎没有成本。

---

## 3. 「我们比两家更复杂」的地方

| 项 | 为什么更复杂 | 建议 |
|---|---|---|
| Node mTLS + CSR + 续期 | 两家都是 bearer key | 应简化（§2.1，要连带改 registry） |
| Gateway 逐请求查控制层并写审计 | Coder 用 1 分钟签名 token，Daytona 缓存 2 分钟，而且连接事件不进审计表 | 应简化 / 应修改（G3、A2） |
| Egress Opening 做成 API 资源加审计 | Coder 由模板管理员定网络，不存在单独的「开口」资源 | 应简化成部署配置（N2） |
| 审计保留要「被引用就不删」 | 两家都只按时间删 | 应简化（A3） |
| 通道上为写动词预留的 fencing | 现在没有写动词 | 应简化（C3） |

## 4. 「两家都踩过、我们没防」的地方

| 坑 | 一手证据 | 我们的状态 |
|---|---|---|
| 连接 / 授权事件把审计表撑爆 | Coder 迁表、裁 90 天、按小时去重（§0 第 3 条） | **没防**：A2 要改 |
| 沙箱内服务需要被人看到（dev server、notebook、TensorBoard） | 两家都有端口入口 | **没有**：G2 要改 |
| 只有一枚永不过期、不能单独吊销的 token | 两家都是多枚具名、可设过期、记 `last_used` | **没写清楚**：I1 要改 |
| 加密存储没法轮换密钥 | Coder 每列配 `$C_key_id` | **没写**：I5 要抄 |
| 规则装失败时 fail-open | Daytona 异步装规则、只记日志（`create.go#L242-L264`） | **已防**：创建期大声失败 |
| 沙箱内有无认证的监听口、沙箱之间默认互通 | Daytona 的 2280 没有认证，ICC 默认开着 | **已防**：零监听端口 |
| path-based 入口和 API 同源，被恶意 JS 利用 | Coder 官方要求关掉 path-based apps | 补端口转发时**只用子域名**（G2） |
| 调用方能自己开私网口子 | Daytona 的示例就开着 `192.168.1.0/24` | **已防**：Egress Opening |

---

## 5. 未能证实 / 未覆盖

- Daytona 线上服务在 2026-06 公开仓库冻结之后有没有给 toolbox 加认证，没有核对。本文只针对 v0.190.0 源码下结论。
- Daytona 云上对 `169.254.169.254` 的防护在不在基础设施层：OSS 源码看不到，**[未证实]**。
- Coder 的 workspace app 从 coderd 到 agent 的完整链路（`workspaceapps/proxy.go` → tailnet → agent 拨本地端口），只核对了文档和 netstack 端口常量，没有逐行读代码。
- 两家 issue 里有没有 toolbox 未认证、agent token 泄漏这类报告：`gh search issues` 两次检索都没有命中，不代表不存在。
