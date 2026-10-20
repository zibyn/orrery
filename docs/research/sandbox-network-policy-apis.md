# 别家的沙箱出网策略：接口面、落地机制与文档明写的缺口

调研日期 **2026-10-16**。只收一手来源（厂商官方文档 / 官方源码 / K8s·Cilium 官方文档 / man page），凡 URL 都经 WebFetch 真打开过。
与 ADR-0003 + #24/#26 已定形状重叠的部分**不重复**：本文只写增量。

## 先读这几条

1. **行业默认几乎都是「出网开着」**，本文核到的每一家（E2B、Cloudflare、Modal、Vercel、Daytona Tier3+、Runloop、Northflank、Fly、Claude Code 沙箱之外的所有人）都以「默认可达公网」出厂。**默认关的是两家**：Claude Code / sandbox-runtime 的 Bash 沙箱（`allowedDomains` 初始为空 = 无网）、以及 Cilium·K8s 的 NetworkPolicy 模型（一旦有规则就隔离）。详见 §5。
2. **我们的「`@allow` 排在 `@deny` 之后」是少数派，而且是更安全的那一派。** 核到的两家里，E2B 明写「allow rules always take precedence over deny rules」，Vercel 明写 `subnets.allow: 10.0.0.0/8` 用来「private network access through Secure Compute」——**它们的 allow 能打开私有段**。我们 #24 追补裁定保住的结构，在字段面上是**反先例**。
3. **「按域名匹配有缺口」这句话，各家都是自己写在文档里的**：domain fronting 在 Vercel 和 Modal 是显式小节，E2B 写的是「同一 endpoint 上的其他目的地会被连带放行」+「被阻断的连接从沙箱内部看起来仍可能成功」。§5 逐条附原文。
4. **落地机制只有两种**：宿主/节点侧把沙箱的包拦下来（iptables/nftables、网桥、netns、eBPF），或者**把它导进一个正向代理**（域名白名单几乎总是靠代理，不是靠包过滤）。**我们选的 netns 内 nftables 在后一类里没有先例**——没有一家产品是「容器 netns 内一套自建规则集」。
5. **「谁编译规则」三家三种**：E2B/Daytona/K8s·Cilium 是**节点侧组件编译**（agent / runner 把策略翻成 iptables），Modal/Vercel 是**厂商控制面编译**，Cloudflare 是**控制面拦截**（handler 跑在 Worker 里、沙箱外）。**「控制层只下发策略、Worker 编译」与 E2B/Daytona 同形**，是我们的先例。

---

## 请求面字段对照表

| 产品 | 字段名（各 SDK 拼法） | 取值 | 默认 | 建后能否改 |
|---|---|---|---|---|
| **E2B** | `allowInternetAccess` / `allow_internet_access`；`network.allowOut` / `allow_out`；`network.denyOut` / `deny_out`；`network.rules`；`network.egressProxy` | `allowOut`：IP / CIDR / **域名**（含 `*.x.com`）；`denyOut`：**仅 IP/CIDR，不支持域名**；`'*'` = 80 端口 Host + 443 SNI | **开**（"Every sandbox has outbound access to the internet by default"） | 能。`updateNetwork`/`update_network`，**整体替换不合并**；空对象清空全部规则 |
| **E2B（HTTP API）** | `PUT /sandboxes/{id}/network` 的 body 是 `SandboxNetworkUpdateConfig`：`allowOut` `denyOut` `egressProxy` `rules` **`allow_internet_access`** | 同上；`allow_internet_access: false` ≡ `denyOut: ["0.0.0.0/0"]` | — | 立即生效（204） |
| **Cloudflare Sandboxes** | Containers：`enableInternet`；Dynamic Workers：`globalOutbound: null`；`outbound` / `outboundByHost` handler；SDK 0.x 另有 `allowedHosts` / `deniedHosts` / `interceptHttps` | boolean / handler 映射 | **开**（"By default, a Sandbox allows internet access"） | `enableInternet` **只在 start 时生效**；outbound handler 的改动**能作用于活着的沙箱** |
| **Modal** | `block_network`；`outbound_cidr_allowlist`；`outbound_domain_allowlist`；`inbound_cidr_allowlist`（Go/JS 同名 CamelCase） | `block_network=True` 全断（含 DNS）；CIDR 列表任意协议；域名列表**仅 TLS/443** | **公网全开**（"By default, Sandboxes can make outbound connections to any public IP address"） | allowlist 能改（`_experimental_set_outbound_network_policy`），**但该种 allowlist 必须创建时就设过**；`block_network` 与改策略 API **不兼容** |
| **Vercel Sandbox** | `networkPolicy`：`'allow-all'` / `'deny-all'` / 自定义对象 `{ allow: {...}, subnets: { allow, deny } }`；JS `sandbox.update({networkPolicy})`、Python `update_network_policy` | 域名（`*` 只能整段替换一个 DNS 标签，`api*.x.com` 不支持；可带 `transform` / `forwardURL` 规则）、CIDR | **`allow-all`** | 能，运行时改，「without restarting the process」 |
| **Daytona** | `network_block_all` / `networkAllowList` / `domainAllowList`；REST `POST /api/sandbox/{id}/network-settings`；`outboundProxyUrl` | CIDR **仅 IPv4、≤10 条、必须带前缀**；域名 ≤100 条、`*.` 前缀 | **Tier 3/4 默认全开**（含一批「essential services」）；**Tier 1/2 受限且不可覆盖** | Tier 3/4 能改（"The sandbox keeps running"）；`outboundProxyUrl` **只能创建时设** |
| **Northflank** | 网络策略的 egress：`Deny all traffic` 开关 + `Allow traffic to IP/CIDR/FQDN`（本页只有 UI 文案，无 schema） | IP / CIDR / FQDN | **无规则即全放**（"If no egress rules exist, all outbound traffic is allowed"） | 文档未写 |
| **Blaxel** | `allowedDomains`；`forbiddenDomains`；配套 `firewall: { rulesets: ["proxy"] }` | `[!]METHOD:...:domain[/path]` 语法，`*` 通配 | **空 `allowedDomains` = 除 `forbiddenDomains` 外全放** | **不能**："Firewall rulesets can only be set when the sandbox is created" |
| **Runloop** | `allow_all`；`allowed_hostnames`；`allow_devbox_to_devbox`；`allow_agent_gateway`；`allow_mcp_gateway`（经 `network_policy_id` 挂到 devbox） | 主机名，**只有最左一个标签可用通配** | **无策略即全开**（"By default, Devboxes have unrestricted network access"） | 能，但要**最终一致**（"may take a few moments to propagate"） |
| **CodeSandbox SDK** | **没有**出网字段。只有入站侧的 `privacy` / `automaticWakeupConfig` | — | — | — |
| **Fly Machines** | `network_policies`：`action: allow`（只有 allow）、`direction: ingress\|egress`、`ports: [{protocol, port}]`、`selectors` | 协议 + 端口（**没有域名、没有 CIDR 表达式**） | **该方向一旦有规则即 deny-all** | 能改，但「restart or redeploy the Machines for changes to take effect」 |
| **K8s NetworkPolicy** | `spec.policyTypes`、`spec.egress[].to[]`（`ipBlock{cidr,except}` / `namespaceSelector` / `podSelector`）、`spec.egress[].ports[]`（`protocol`/`port`/`endPort`） | L3/L4 only，TCP/UDP/SCTP | **无策略即非隔离**（"By default, a pod is non-isolated for egress; all outbound connections are allowed"） | 是声明式对象，随改随生效（靠 CNI 实现） |
| **Cilium** | `toFQDNs[].matchName` / `matchPattern`；外加一条 **独立** 的 L7 DNS 规则 | 域名 / 通配模式（`*` 不跨 `.`） | 无策略即全放 | 是 CRD，随改 |
| **Docker** | `--network none` / `--network container:<id>`；`docker network create --internal`；`--dns` / `--dns-search` / `--dns-option` / `--add-host` | 枚举 / 列表 | **bridge 全开** | 容器属性，改需重建/重连 |
| **gVisor** | `--network=sandbox\|host\|none`；`--network-proxy-path=<unix socket>`（实验性）；`--qdisc=tbf` + `--qdisc-tbf-rate/-burst` | 枚举 / 套接字路径 / 字节率 | **sandbox（netstack，可出网）** | 运行时启动参数 |
| **Claude Code / sandbox-runtime** | `network.allowedDomains`；`network.deniedDomains`；`network.strictAllowlist`；`network.httpProxyPort` / `socksProxyPort`；`network.allowUnixSockets`；`allowLocalBinding`；`tlsTerminate` | 域名（支持 `*.x.com`、可带 `:port`）、IPv6 字面量要加方括号；裸 `*` 于 `deniedDomains` = 全禁 | **拒绝**（"all network access is denied by default"，allowedDomains 初始为空） | 能（配置热改；沙箱本身默认关闭，`sandbox.enabled` 开） |

---

## 分产品小节

### E2B

**怎么做的**：Firecracker microVM；域名白名单靠 80 端口 Host 头 + 443 SNI 匹配，**域名只能进 allow 列表**（"Domains are not supported in the deny lists"）；用域名时 API 要求 `denyOut` 里给 `0.0.0.0/0`，否则建沙箱返回 400；用到任何域名时 `8.8.8.8` **被自动放行**以保证解析。`network.rules` 是按 host 挂的 header 注入（转发到已允许的 host 上），本身不授权。`egressProxy` 是一个 **SOCKS5** 代理，流量在 allow/deny 过滤之后被隧道走，"the sandbox is unaware"。

**该抄**
- **`updateNetwork` 是整体替换、不是合并，而且文档明写这一条**（"replaces the current egress configuration — it does not merge"）。我们的「出网档位是 Workspace 记录里的显式事实」正是这份纪律；把它写成一句给调用方看的警告，比事后解释便宜。
- **建时设的策略点与运行时可改的策略点分开列**（create-only：`allowPublicTraffic`、`maskRequestHost`、`httpsPorts`）。我们 `egress` 挂在 Workspace 创建上是同一类决定，值得在字段面上也标出来。
- **把「这个键不在这里生效」的边界写清楚**：`network.rules` 的注释是 "Rules do not grant network access; configure allowOut separately"。

**不该抄**
- ⚠️ **`allow rules always take precedence over deny rules`。** 这条让 `denyOut: ['0.0.0.0/0']` + `allowOut: ['10.0.0.5']` 变成「放行内网某台机器」。**这正是我们 #24 用规则次序结构性堵掉的那件事。**
- ⚠️ **用「域名白名单」表达私有段排除**：E2B 没有「私有一律拒」的档位，得自己列 deny CIDR，而 allow 又能翻过来。

### Cloudflare Sandboxes

**怎么做的**：容器是 microVM，**只能经它自己的 Durable Object 访问**。出网控制有两条路：(a) `enableInternet: false` + 拦截 hostname；(b) `outbound` / `outboundByHost` handler——**handler 跑在 Workers runtime 里，也就是沙箱外面**，因此 "they can hold secrets that the sandbox itself never sees"。凭据注入的建议是「沙箱发一个普通请求到你选定的 hostname，Worker 拦下来把 token 加上」。SDK 0.x 另有一层 `allowedHosts` / `deniedHosts` glob 列表，一旦给 `allowedHosts` 就 "becomes a deny-by-default allowlist"。

**该抄**
- ⭐ **「拦截点默认在沙箱外」这个默认姿态。** 我们的能力通道（bind mount 进容器的 unix socket、宿主侧 Worker 监听）是同一件事的不同实现，措辞可以直接借：「handler 在沙箱外跑，所以它能持有沙箱永远看不到的秘密」。
- ⭐ **DNS 单独交代一句**：关掉互联网时「Only ports 80, 443, and DNS are available，DNS 走 Cloudflare 自己的解析器」，理由是防「用任意 DNS 目的地做数据外泄」。**我们没有做 DNS 收边（ADR-0003 选择显式指公网上游）**，这句为我们将来收紧时的形态留了对照。
- **「哪个旋钮何时生效」逐条标注**：`enableInternet` 在 start 时生效，handler 改动作用于活沙箱。我们对 `egress` 档位也要有这个精度。

**不该抄**
- ⚠️ **`interceptHttps` 默认值在文档里前后不一致**（SDK 0.x 页写 default `false`，检索到的更新版页面有人读作默认 `true`——**本文未证实后者**）。凡「必须开一个开关才真的开始拦 HTTPS」的设计，默认值写错一次就是一条静默放行。
- **handler 只拦 80/443**："Traffic on ports other than 80 and 443 is never routed through outbound or outboundByHost"、非 HTTP 流量在关网时直接拒。域名级控制在端口维度上天然是漏的——与我们按 IP 收边的取向相反，正好说明**两者不是替代关系**。
- **沙箱内看到什么，文档不说**（见 §5）。

### Modal

**怎么做的**：`block_network=True` 是全断开关，连 DNS 都断（"DNS traffic is allowed without restriction **except when all outbound traffic is blocked**"）。细粒度只有两条 allowlist：CIDR（任意协议）与域名（**只有 TLS/443，SNI 匹配**，Modal 自己解析域名、不信目的 IP）。运行时用 `_experimental_set_outbound_network_policy()`（名字里带 experimental，Python SDK），「The new policy takes effect immediately」且「Established connections that the new policy no longer permits are terminated」。

**该抄**
- ⭐ **「改策略要顺手掐掉不再被允许的已建立连接」**——我们对 `@deny` 里会话对端地址漂移的处理（无条件重装）是同一件事，但我们是靠重装规则、不是靠 ct 清理；这句提醒我们 cp 里那条 `ct state established,related accept` 会让**旧连接在新策略下继续活着**，值得在规格里点名。
- **`block_network` 与动态改策略互斥**，文档给了替代写法（用空 allowlist `[]` 表达「全断」）而不是让它悄悄不生效——**「互斥要明说 + 给出等价写法」**值得抄。
- **allowlist 要在创建时设过才能运行时可改**（想后面能收窄就先建时给 `["*"]` / `["0.0.0.0/0"]`）。这是个反直觉的 API 形状，抄它等于抄一个坑；**但它存在的理由是「策略必须能收紧」**——我们的「无条件重装」不需要这样的前置条件，这是自建件的直接收益。

**不该抄**
- **域名白名单只覆盖 443/TLS**：非 TLS 流量的待遇文档明写是「blocked」，而社区转述里这一点表现为**静默超时而不是拒绝**（[未证实]，见 §5）。域名级控制「看起来在拦、实际在丢」是这一票反复出现的形状。
- **`_experimental_` 前缀的函数**做正式出网策略的唯一运行时入口。

### Vercel Sandbox

**怎么做的**：三档 `allow-all`（默认）/ `deny-all` / 自定义对象。自定义对象的形状是 `{ allow: { "域名": [规则] }, subnets: { allow: [...], deny: [...] } }`：**域名负责「连接协商哪个 hostname」，CIDR 负责「能不能到那个地址」**，两者独立。规则 (`transform` / `forwardURL`) **不改变是否放行**（"A rule never changes whether a request is allowed"），只决定命中后是注入凭据还是转给你自己的代理。要做 transform/forwardURL 的域名会**被 TLS 终结**，为此给每个沙箱撒一张 per-sandbox CA。

**该抄**
- ⭐ **「放行」与「处理」两个决策分开。** 「防火墙做两个独立决定：Access（能不能过）与 Handling（过了之后做什么）」，且「规则永远不改变是否允许」。我们把「出网档位」与「能力通道」当两件事，这里是**同一系统内**把它拆成两个字段的先例，措辞可直接用。
- ⭐ **空策略 = deny-all 而不是 allow-all，且文档把 `{}`、`{allow:{}}`、`{subnets:{}}`、只有 `subnets.deny` 四种写法逐个点名。** 「空集合在语义上往哪边倒」是这份字段面里最容易出事的一格（对照我们 #24：`public` 档下 `@allow` 必须是**存在但空**的集合，而匿名空集 `{}` 在 nft 里是语法错误）。
- ⭐ **`subnets.allow` 不限制 DNS**，文档明说「a policy with allowed address ranges and no allowed domains can resolve any hostname… and code can use those lookups to send data over DNS」。**这是我们 §5 那条「DNS 是独立出网面」的最直白表述。**
- **per-sandbox CA 的安装面写得很细**（塞进系统证书 + 一串环境变量 + 自定义 CA bundle 的补救说明）。我们若走代理路线，这就是那份清单。

**不该抄**
- ⚠️ **`subnets.allow: 10.0.0.0/8` 被当作「private network access through Secure Compute」的正当用法写在文档里**——**allow 能打开私有段**，与我们的结构正相反。
- ⚠️ **`0.0.0.0/0` 或 `::/0` in `subnets.allow` 直接抹掉域名级收边**（"Broad ranges override domain scoping"）。我们在 #24 里正是把这条否决理由**结构性删掉**了（`@deny drop` 永远排在 `@allow accept` 前），这里可以看到它没被删掉的市场形态。
- **让 `*` 通配符出现在 allow 里必须配合 `deny` 里的 `0.0.0.0/0`**（E2B 也这样）——「通配 + 全拒基座」是一套要在调用方教的用法，不是一次就能写对的字段面。

### Daytona

**怎么做的**：两条 allowlist 互斥（"mutually exclusive. Set at most one non-empty value"）：`network_allow_list`（**仅 IPv4 CIDR**，≤10 条，每条必须带前缀）与 `domain_allow_list`（主机名，≤100 条，只支持前缀 `*.`），外加 `network_block_all` 全断。**默认策略按套餐分档**：Tier 1/2 网络受限且「cannot be overridden at the sandbox level」，Tier 3/4 全开。给沙箱设了 allowlist 就「replaces the default policy for that sandbox」，且「Essential services do not bypass a sandbox allow list or block-all setting」——**默认放行的那批包管理器域名会被一并关掉，得自己加回来**。

**该抄**
- ⭐ **「默认策略被显式覆盖后，默认放行的那批东西不再自动豁免」**这句话写得毫不含糊。我们的 `public` 就是「默认那批」；`allow:[CIDR]` 是在它之上追加而不是替换，所以这句话用不上——但它是一句**值得我们反过来引用**的对照：别家的 allowlist 是替换式的，所以要有这句警告。
- **`outboundProxyUrl` 只能创建时设，且对地址有约束**（scheme 限 http/https、host 不得是 localhost/私有/环回/link-local 字面量）。**「让调用方指一个代理」这条旋钮必然带来的 SSRF 面**，它提前堵了。

**不该抄**
- ⚠️ **按套餐分档的默认网络策略**（Tier 1/2 强制受限、Tier 3/4 全开）。同一份 API 在同一个字段上按计费档位给出不同默认值，调用方没法从代码里看出边界在哪。我们的 `egress` 是 Workspace 记录里的**显式事实**，正好是这条的反面。
- **IP 列表和域名列表互斥**：表达「公网 + 几个内网镜像源」时要选一个。
- **`networkBlockAll: true` 会静默清掉已存的 CIDR 与域名列表**（"blocks all outbound traffic and clears both the stored CIDR and domain allow lists"）——一个有副作用的开关。

### Northflank

**怎么做的**：把「网络策略」做成一等对象，egress 侧有一个 `Deny all traffic` 开关与 `Allow traffic to tags / projects / IP·CIDR·FQDN`。默认无规则即全放，加第一条规则就变成 allowlist。

**该抄**
- **默认放行 → 加规则即变 allowlist** 这个「第一条规则改变整个方向的默认」的模型，在 Fly（同形）之外又出现一次。它是「策略是白名单、不是补丁」的一种干净表达。

**不该抄**（也是**查不到的**）
- ⚠️ **字段名查不到**：官方文档页只有 UI 文案（"Allow traffic to IP/CIDR/FQDN"），没有 schema、没有 YAML/JSON 示例。**有 FQDN 这一档，但文档不说它怎么落地**（见 §5）。凡在字段面上要抄它的地方，都得先当 [未证实] 处理。

### Blaxel

**怎么做的**：`allowedDomains` / `forbiddenDomains` 两个域名字段，`forbiddenDomains` 无条件赢；空 `allowedDomains` = 只按黑名单放行；被拦的请求拿到 **HTTP 403 + `Proxy-Error: firewall_blocked` 头**。语法是 `[!]METHOD:...:domain[/path]`。⚠️ **默认只靠环境变量**："Domain filtering relies on the sandbox's tools and libraries respecting the standard proxy environment variables"——不认 `HTTP_PROXY` 的工具**默认不被过滤**；要真拦得额外开 `firewall: { rulesets: ["proxy"] }`，而且**只能创建时设**，且仍标 public preview、「not recommended for production use」。

**该抄**
- ⭐ **「被拦时调用方看见什么」被明确设计过**：HTTP 403 + 一个可判断的响应头。我们在 §5 记的「被阻断的连接看起来仍可能成功」，在应用层是可以修掉的——**Blaxel 是这一票里唯一给出被拦信号形状的产品**。我们的 v1 只做计数器、不回显，但这条说明「沙箱内的代码能不能分辨被拦」是一个真实的设计轴。
- ⭐ **诚实标注「当前靠工具自觉」**。这是先例里最值得学的一句标注方式：不说「我们有网络策略」，说「在 X 打开之前，我们依赖 Y」。

**不该抄**
- ⚠️ **默认不拦**的过滤（靠 proxy 环境变量）。这正是 ADR-0003 在「出口正向代理 + 域名白名单」那条否决理由里点的「还得防着有人绕过」，而这里是它没加防的形态。
- **防火墙配置创建后不可改**。

### Runloop

**怎么做的**：`network_policy` 对象，字段是 `allow_all`（默认行为）、`allowed_hostnames`、`allow_devbox_to_devbox`、`allow_agent_gateway`、`allow_mcp_gateway`；经 `network_policy_id` 挂到 devbox 或 blueprint（blueprint 上分 build-time 与 runtime 两个槽）。通配**只有最左一个标签**能用（`*.github.com`；`github.*` 不行）。策略可编辑，改动广播到「all running Devboxes and Blueprints using that policy」，但「eventually consistent」。

**该抄**
- ⭐ **把「平台自己的服务」也做成策略里的显式开关**（`allow_agent_gateway` / `allow_mcp_gateway`）：出口策略不只描述互联网，也描述「能不能够到我们自己的凭据代理」。**对照我们**：能力通道是 bind mount 的 unix socket，不经网络面，所以不需要这格——**但「策略里要不要给平台自己的能力留一个位」这个问法值得留一句**，免得将来有人按域名白名单的思路重做能力通道。
- ⭐ **「无策略即全开」而策略是**挂载**上去的**（默认没有任何策略对象）。默认值在一个**可为空的对象**上，比默认值在一个 enum 的某一档上更容易说清「没配」与「配了」的区别。

**不该抄**：最终一致的策略改动。我们的规则改动是「重装 + 回读指纹」，是同步事实；这里改了策略要「a few moments」才到所有资源。

### CodeSandbox SDK

**怎么做的**：**没有出网控制的字段面**。官方 SDK 文档（Key Terms / Creating Sandboxes / Host Privacy）只讲**入站**：`privacy`（`public` / `private` / `public-hosts`）与 `automaticWakeupConfig`（按 HTTP/WebSocket 唤醒）。**该抄**：没有。**不该抄**：没有可抄的。这是一条**负结果**，进 §5。

### Fly.io

**怎么做的**：Machine 的 `network_policies`，规则形如 `{"action": "allow", "direction": "egress", "ports": [{"protocol": "tcp", "port": 443}]}` + selector（`{"all": true}` / 指定 machine id / 按 metadata）。**`action` 只有 `allow`**——「拒绝」不是一条规则，而是**某方向一旦有规则、该方向其余全部默认拒绝**。私有网络那条线是另一套东西：6PN（`*.internal`）、`flycast`（经 Fly Proxy 的私有地址，HTTP-only）。

**该抄**
- ⭐ **「DNS masking」这个测试坑被写进了 troubleshooting**："Use direct IP addresses (not hostnames) to test blocked traffic to avoid DNS masking."——**用域名去测「拦不拦得住」会得到假结果**。我们 #26 的探针用的是 IP，是对的；这句给了一个更硬的表述。
- ⭐ **规则集有一个明说的盲区**："They do not affect traffic routed through the Fly Proxy." **一个显式的「策略不覆盖哪条路径」清单**，比笼统说「防火墙已启用」诚实得多。（我们的对应物是一条我们已经知道的：`lo` 整条放行、内嵌解析器的 host-netns 上游跳落在边界之外。）

**不该抄**
- **没有域名、没有 CIDR**：只有 protocol:port。要按域名收边，Fly 上做不到。
- **改策略要 restart/redeploy**。

### Kubernetes NetworkPolicy

**怎么做的**：`spec.policyTypes`（`Ingress`/`Egress`/两者）+ `spec.egress[].to[]` + `spec.egress[].ports[]`。`to` 的元素是 `NetworkPolicyPeer`：`ipBlock{cidr, except}` / `namespaceSelector` / `podSelector`。`ports[]` 是 `{protocol, port, endPort}`。**语义是加性的**："Network policies do not conflict; they are additive… order of evaluation does not affect the policy result."

**该抄**
- ⭐ **`ipBlock` 的 `cidr` + `except` 就是我们的 `@allow` / `@deny`**，而且 K8s 明写 `except` 必须在 `cidr` 之内（"Except values will be rejected if they are outside the cidr range"）——**一条「负集合不能越出正集合」的静态校验**，比我们「编译进 Worker 的常量 + 运行期只增不减」更便宜。值得想一下我们的四个集合要不要在编译期做同样的自洽检查。
- ⭐ **`policyTypes` 与规则体分离**：`egress` 数组为空 + `policyTypes: [Egress]` 就表达「default-deny egress」。我们 `public` 档下「`@allow` 是空集合但必须存在」是同一个形状（**空 ≠ 不存在**），可以引它作对照。
- **「additive / 无序」**这句把「多条策略怎么合」一句话说完。我们的规则集是单表单链有序，反过来；两句放在一起能把「为什么我们选有序」讲清楚。

**不该抄**
- ⚠️ **NetworkPolicy 完全依赖 CNI 才有任何效果**，而 API 层不做任何拦截：官方原文 "Creating a NetworkPolicy resource without a controller that implements it will have no effect." 与 "POSTing this to the API server for your cluster will have no effect unless your chosen networking solution supports network policy." **我们把这条反面抄成了正面**：ADR-0003 里 `network.egress_policy` 是**能力声明**（`enforced | unsupported`），`local` 后端硬失败——这正是「不要有会静默失效的策略字段」。
- **节点到 Pod 的流量永远放行**（"traffic to and from the node where a Pod is running is always allowed, regardless of the IP address of the Pod or the node"）。这是 L3/L4 策略的一个恒等豁免，与我们「宿主地址进 `@deny`」正相反。

### Cilium（FQDN 策略与 DNS proxy）

**怎么做的**：L3 侧 `toFQDNs[].matchName`（精确名）/ `matchPattern`（通配）把解析到的 IP 塞进一条自动生成的 CIDR 规则；填这些 IP 的是 **cilium-agent 里的内嵌 DNS Proxy**，它「intercepts egress DNS traffic and records IPs seen in the responses」。**代理本身要一条独立的 L7 规则放行 DNS 请求**——"Interception is, itself, a separate policy rule governing the DNS requests, and must be specified separately." 通配模式产生的 IP **只有走代理才能被 toFQDNs 使用**（"the only method to allow IPs from responses allowed by wildcard L7 DNS matchPattern rules for use in toFQDNs rules"）。另有 DNS polling 兜底（默认关）。

**该抄**
- ⭐ **「按域名放行」被拆成两件事，而且第二件必须显式写**：DNS 请求本身要被策略放行，解析结果才进得了 L3 规则。**这是 §5 那条「DNS 是独立出网面」在实现层的正面形态**——Cilium 没有假装域名白名单是一个字段。
- ⭐ **TTL 与缓存的取舍写得很细**：IP 缓存尊重 TTL、每个 endpoint 每个 FQDN 默认最多 50 个 IP（超出**淘汰最旧的**），而「Live connections are not expired until they terminate」——**规则变更不会掐掉活着的连接**。这一条与我们 cp 里 `ct state established,related accept` 的效果完全一致，可以作为我们命名那条规则时的引用。
- **一个很具体的兼容性坑**：Alpine/musl 镜像把代理回的 `Refused` 当通用失败、会中断 search-list 尝试，于是有了 `--tofqdns-dns-reject-response-code`（默认 `refused`，可改 `nameError` 回 NXDOMAIN）。**「按域名收边会改变 DNS 的失败形态，而某些 libc 会因此更坏」**——我们显式指公网上游将来若要收紧，这是必读的一页。

**不该抄**
- **K8s 里必须显式放行 `service.namespace.svc.cluster.local.` 的 search-list 展开形式**（`matchPattern: *.*.svc.cluster.local.`），否则解析会因为「展开后的名字没被允许」而失败。域名策略与 DNS search list 的相互作用，是一块持续的税。
- **deny 策略不支持 `toFQDNs`**（"Deny policies do not support toFQDNs"）。**按域名只能做白名单、不能做黑名单**，E2B 与 Cilium 在这里一致——这是「域名」作为策略维度的固有不对称。

### Docker（我们规则集的落地底座）

**怎么做的**：官方语义逐条：`none` 驱动 = "Completely isolate a container from the host and other containers"；`host` = "Remove network isolation between the container and the Docker host"；`--internal` = "Restrict external access to the network"，展开是「**没有默认路由，且设了防火墙规则丢掉所有去往/来自其他网络的流量**」，同时「Communication with the gateway IP address (and thus appropriately configured host services) is possible」；DNS 侧 `--dns` / `--dns-search` / `--dns-option`（**注意 flag 名是 `--dns-option` 不是 `--dns-opt`**）/ `--add-host`（支持 `host-gateway` 这个特殊值，解析成宿主内部地址），自定义网络用内嵌解析器 `127.0.0.11`，「forward external DNS lookups to the DNS servers configured on the host」，且 **"There is no IPv6 equivalent; the IPv4 address works even in IPv6-only containers."**

**该抄**
- ⭐ **`--internal` 的定义本身给我们一个用不上的对照**：它的效果是「没默认路由 + 防火墙丢包」，**也正是 #22 里我们绕开的那条路**。把它的原文放在文档里，能让「为什么 `--network none` 走不通、为什么改用 disconnect」有一段外部依据可引。
- ⭐ **`127.0.0.11` 无 IPv6 等价物**是一句可直接引用的实现事实，支撑我们「v6 半张规则在 `enable_ipv6=false` 下是死代码」的结论。**`--add-host` 的 `host-gateway`** 则是宿主地址在 docker 里**可以显式注入**的通道——我们靠运行期枚举把它写进 `@deny`，两条路都通，但值得在规格里点名「别用它」。

**不该抄**：无（这一节是底座，不是产品）。

### gVisor

**怎么做的**：「gVisor implements its own network stack called **netstack**」，全在 Sentry 里；Sentry 出于安全「cannot open host sockets of its own」，只持有一个 `AF_PACKET` 套接字，「ingresses and egresses all non-loopback traffic across that socket」。`--network=none` 时「The sandbox will still contain a loopback provided by netstack」。实验性的**外部网络代理** `--network-proxy-path=<unix socket>` 把主接口接到一个外部进程上，而**「The external peer is responsible for any egress filtering.」**

**该抄**
- ⭐ **`--network-proxy-path` 那句责任划分**：「外部对端负责一切出网过滤」——**这是 ADR-0003 里「换 gVisor」那条出路的最小可行形态**，而且它的接法与我们的能力通道（bind mount 进容器的 unix socket）**形状相同**。真要走到那一步，这是一个已存在的一手接口，不是要自建的目标。
- **只代理主接口**：其他非 loopback 接口「still use AF_PACKET sockets (with host addresses removed) and bypass the proxy」——**一个接口级的旁路，文档主动写出来了**。我们的对应风险是 `lo` 整条放行；两条都说明「策略挂在接口上时，接口清单就是边界清单」。

**不该抄 / 缺口**
- ⚠️ **官方文档里没有网络策略**。user guide 只给 `--network` 三档 + 一个实验性代理 + TBF 限速；architecture guide 通篇不提 netfilter/包过滤落在哪。**「gVisor 自带出网策略」这件事，在一手来源里查不到**（见 §5）。

### Anthropic：sandbox-runtime / Claude Code 沙箱

**怎么做的**：**allow-only 模式**——"all network access is denied by default"，`network.allowedDomains` 初始为空即无网。流量全部导到宿主上的代理，「Both HTTP/HTTPS (via HTTP proxy) and other TCP traffic (via SOCKS5 proxy) are mediated by these proxies」。判定次序是 deny → allow → `strictAllowlist` → 每条命令注册的列表 → ask 回调 → 默认拒。**在连到被允许的主机之前还会做一次解析地址检查**：代理解析一次、丢掉被拒地址，只连存活的那个；被拒集合涵盖**loopback、unspecified、link-local、multicast、broadcast、云 metadata 端点、宿主自身接口地址**。Linux 落地是「**整个 netns 被移除**」+ 请求走一个 unix domain socket；macOS 用 Seatbelt + 只允许连本地代理端口；Windows（alpha）用 WFP 挡掉 `srt-sandbox` 账户的全部出站、只留 loopback 到代理端口。

**该抄**
- ⭐⭐ **「解析后地址检查」的措辞与清单**：`loopback、unspecified、link-local、multicast、broadcast、云 metadata、宿主自身接口地址`——**和我们对 `@deny` 常量的取舍几乎逐项重合**（我们多 `100.64.0.0/10`、少 multicast/broadcast）。我们的清单是「拒绝空间用负向集合编码」，它是「允许一条 hostname 后仍要检查它解析到哪」。**两者是同一个洞察的两个方向，句子可以直接对齐。**
- ⭐⭐ **「按域名白名单 + 代理」的默认姿态是「无网」**：`allowedDomains` 空 = 无网，且文档明说 "You must explicitly allow domains"。**这是 §1 里「默认关」那半边的唯一产品级先例**，也是 ADR-0003 里被推迟的那条路线的现实形态。
- ⭐ **`strictAllowlist`**：非白名单主机从「问」变成「直接拒」。**「默认行为可以靠一个开关从交互式降级成静默拒绝」**，是把「策略」与「体验」解耦的一格。
- **平台差异如实记账**：`allowUnixSockets` 在 Linux 上「**Ignored** (seccomp can't filter by path)」；Windows 上 `proxyAuthToken` 在命令行可见、CRL/OCSP 取不到会报 `CRYPT_E_REVOCATION_OFFLINE`。**「同一个字段在三个平台上三样」是被明说的**。

**该抄反面 / 不该抄**
- ⚠️ **Claude Code 的沙箱默认关闭**（"The sandbox is off by default"），而且**未列出的 shell 命令可以请求「不安全重试」跑出沙箱**（`dangerouslyDisableSandbox`）。这是「默认关 + 有逃生门」的组合——**可用性优先的极端形式**，与我们的「默认开着但收边」是相反的两端。
- ⚠️ **有公开的未修报告说 `allowedDomains` 在 macOS 上没被硬执行**（GitHub issue 里描述内置代理对非白名单 host 仍回 `200 Connection Established`）。这些是**第三方 issue，不是一手规格**，我未逐条核对；但它说明「代理式域名白名单的执行强度是需要单独验证的」这条结论对本项目同样适用。

---

## 落地机制对照（含「谁编译规则」）

| 产品 | 强制点 | 策略编译者 | 策略的表达维度 |
|---|---|---|---|
| E2B | 沙箱外的出口代理/防火墙 | **节点侧**（他们的 orchestrator） | CIDR + 域名（Host/SNI） |
| Cloudflare | Worker 里的拦截点（沙箱外） | **控制面**（你的 Worker 代码即策略） | hostname（80/443 only） |
| Modal | 宿主网络层 | **厂商控制面** | CIDR + 域名（443 SNI） |
| Vercel | SNI-peeking 防火墙 + 可终结 TLS 的代理 | **厂商控制面** | 域名（SNI）+ CIDR + 规则动作 |
| Daytona | 宿主 iptables（per-sandbox 链 `DAYTONA-SB-{id}`，第三方源码分析） | **节点侧 runner** | CIDR / 域名二选一 |
| Northflank | K8s 式网络策略（IP/CIDR/FQDN） | 节点侧 | IP / CIDR / FQDN |
| Blaxel | 默认只在代理环境变量层；开了 `firewall: {rulesets:["proxy"]}` 才落到网络层 | **控制面**（代理） | 方法 + 域名 + 路径前缀 |
| Runloop | 平台出口代理/防火墙 | 控制面 | 主机名（左一标签通配） |
| Fly | Machine 自己的规则集 | **节点侧**（machine 宿主） | protocol:port |
| K8s NetworkPolicy | **CNI**（无 CNI 则零效果） | **节点侧 CNI agent** | IP/CIDR（+ pod/namespace 选择器） |
| Cilium | eBPF + 内嵌 DNS Proxy | **节点侧 agent** | CIDR + FQDN（经 DNS 代理喂 IP） |
| Docker | 网桥 / 规则 | 无所谓（没有域名/策略面） | 网络归属 |
| gVisor | netstack（Sentry 内）/ 外部代理对端 | 外部进程 | 接口级 |
| Claude Code / sandbox-runtime | 宿主代理（HTTP + SOCKS5）+ netns 移除 / Seatbelt / WFP | **本机代理进程** | 域名（可带端口）|
| **Orrery（我们）** | **容器 netns 内的 nftables** | **Worker（宿主侧）** | 档位 + CIDR 追加（**无域名维度**）|

**三条读数**：

1. **没有一家把强制点放在「容器 netns 内的自建规则集」上**——最接近的是 gVisor 的 `--network-proxy-path`（主接口交给外部对端过滤）与 Daytona 的宿主 iptables per-sandbox 链。我们的形状在**强制点位置**上是自立的，在**「宿主侧组件编译」**这点上有 Daytona（官方文档未写实现，第三方源码分析）/ K8s·Cilium（官方文档）两个可引先例，E2B 的「节点编排器决定怎么做」见既有先行形态调研 §1.1（本票未复查那份架构文档）。
2. **只有 Cilium 与 E2B 把域名字段做成了「解析结果 → L3 规则」**；其余全家的域名字段都是**代理的匹配键**。**域名维度 = 代理**这条等式在这一票里几乎无例外，而代理必然带来 TLS 终结（Vercel 明确）/ 环境变量依赖（Blaxel 明确）/ ECH 与 fronting 缺口（Modal·Vercel 明确）。
3. **`DNS 是独立出网面` 出现在两家**：Vercel（`subnets.allow` 不限制解析）与 Cloudflare（关网时 DNS 走自家解析器、理由写的是防 DNS 外泄）。**我们目前对 DNS 只有一个决定（上游显式指公网），没有一条能表达「不许用它外泄」的策略**——这是 §5 里最实的一块缺口。

---

## 文档明写的缺口

逐条附来源。**这一节是本票最有价值的部分**，也是与我们 #24/#26 结论重合度最高的一节。

1. **被阻断的连接，从沙箱内部看起来仍可能成功。** E2B 原话："Due to firewall design, blocked connections may appear successful from inside the sandbox… no packets actually reach the destination."，并建议用应用层响应（HTTP 状态、TLS 握手）而不是 socket 状态来判断。
   <https://docs.e2b.dev/network/internet-access>
   **我们的对应**：#24 只做计数器、不做日志，所以沙箱内的代码**无法分辨**被拦；这条说明这不是我们独有的粗糙，而是这一类强制点的共性。**但 Blaxel 给出了反面**：403 + `Proxy-Error: firewall_blocked`（§Blaxel）。**「能不能让被拦变得可观测」是一条真实的设计轴。**

2. **domain fronting（SNI 与 Host 不一致）。** Vercel 有专门段落：客户端可以「send an allowlisted hostname as the SNI while sending a different hostname in the HTTP Host header」，而防火墙「does not prevent the mismatch by default」，只约束「which hostname a connection negotiates, not which application behind that endpoint ultimately serves the request」；对策是 allowlist 只放单一用途的窄主机名，或用 `transform` / `forwardURL`（那会终结 TLS，从而能改 Host）。
   <https://vercel.com/docs/sandbox/concepts/firewall>
   Modal 同一件事：两个域名共用 TLS endpoint 时可以用已白名单的 SNI 配另一个 `Host` 到达未被允许的域名，对策是 sidecar 代理。
   <https://modal.com/docs/guide/sandbox-networking>
   **我们的对应**：ADR-0003 已把这条列为否决「代理 + 域名白名单」的理由之一。**这里是两家把缺口写在正式文档里的原文，可以直接引用，不必再论证一次。**

3. **ECH（Encrypted Client Hello）不被支持。** Modal 明写："Encrypted Client Hello (ECH) is not supported."，只有外层公有名可见，于是连接按外层名匹配、不在名单里就拒。
   <https://modal.com/docs/guide/sandbox-networking>
   **含义**：域名级白名单的前景比多数人以为的更差——ECH 普及后 SNI 匹配会**大面积失效或大面积误伤**，而不是慢慢变好。

4. **「放行一个 hostname 可能连带放行同一 endpoint 上的其他目的地」。** E2B 写的是 CDN 与对象存储上的这种连带："allowing one hostname can also permit access to other destinations on the same endpoint"；并且警告宽通配（`*.com`）会把注入的凭据发给每一个匹配的目的地。Vercel 用不同措辞说同一件事（`subnets.allow` 与域名规则互相独立，代码可以直接用 IP 绕开 SNI 过滤、也就绕开了凭据注入）。
   <https://docs.e2b.dev/network/internet-access>、<https://vercel.com/docs/sandbox/concepts/firewall>

5. **UDP / QUIC 不被域名过滤覆盖。** E2B："UDP-based protocols like QUIC/HTTP3 are not supported for domain filtering."；Modal 把非 TLS 流量的待遇写成 "Non-TLS traffic (HTTP, raw TCP, UDP) to IPs that are not on a CIDR allowlist is blocked."——**注意它说的是 block，社区有转述说实际表现为静默超时（[未证实]）**。
   <https://docs.e2b.dev/network/internet-access>、<https://modal.com/docs/guide/sandbox-networking>

6. **`allow` 能打开私有段 —— 三家形态。**
   - E2B："Allowed entries always take precedence over denied entries."（API 参考）；指南同一句是 "allow rules always take precedence over deny rules."。所以 `denyOut: ['0.0.0.0/0']` + `allowOut: ['10.0.0.5']` 会放行内网地址。
   - Vercel：`subnets.allow: ["10.0.0.0/8"]` 被文档当作 "private network access through Secure Compute" 的正常用法，且 `0.0.0.0/0` / `::/0` 会「override domain scoping」。
   - Daytona：`domain_allow_list` 与 IP/CIDR 列表互斥，但 IP 列表**没有任何「私有段不可进」的限制**（文档只说「IPv4 only / 必须带前缀 / ≤10 条」）。
   <https://docs.e2b.dev/network/internet-access>、<https://docs.e2b.dev/api-reference/sandboxes/update-sandbox-network>、<https://vercel.com/docs/sandbox/concepts/firewall>、<https://www.daytona.io/docs/network-limits>

7. **DNS 是一个独立出网面，且默认往往不受限。**
   - Vercel："`subnets.allow` leaves DNS unrestricted… code can use those lookups to send data over DNS."
   - Modal："Currently, DNS traffic is allowed without restriction except when all outbound traffic is blocked."
   - E2B：用域名时会**自动放行 `8.8.8.8`**（"to ensure proper DNS resolution"）；DNS over UDP 到 `8.8.8.8:53` 在 `'*'` 配置下也保持放行。
   - Cloudflare：关掉互联网时 DNS 仍可用，但**只走 Cloudflare 自己的解析器**，理由是防「using arbitrary DNS destinations for data exfiltration」。
   - Cilium：域名策略要生效，必须**额外**放行 DNS 请求本身。
   <https://vercel.com/docs/sandbox/concepts/firewall>、<https://modal.com/docs/guide/sandbox-networking>、<https://docs.e2b.dev/network/internet-access>、<https://developers.cloudflare.com/sandbox/guides/outbound-traffic/index.md>、<https://raw.githubusercontent.com/cilium/cilium/v1.17.1/Documentation/security/policy/language.rst>

8. **IPv6：几乎全部沉默。** E2B、Modal、Fly、Northflank、Runloop 的官方出网文档页里**都没有 IPv6 的待遇说明**（Modal 连 "public IP address" 都没说 v4/v6）；Daytona 明文 **"IPv4 only: hostnames, domains, and IPv6 are not supported"**。**唯一把「v6 私有段」写清楚的是我们自己的 #16/#24。**
   <https://www.daytona.io/docs/network-limits> 等（见上）

9. **域名只能做白名单，不能做黑名单 —— 两家一致。** E2B："Domains are not supported in the deny lists."；Cilium："Deny policies do not support toFQDNs."
   <https://docs.e2b.dev/network/internet-access>、<https://raw.githubusercontent.com/cilium/cilium/v1.17.1/Documentation/security/policy/language.rst>

10. **策略字段的「空」往哪边倒，要逐个点名。** Vercel 是唯一把这件事写成一节的：空策略（`{}`、`{allow:{}}`、`{subnets:{}}`、只有 `subnets.deny`）**全都等于 deny-all**，并劝你用显式的 `deny-all` 模式。
    <https://vercel.com/docs/sandbox/concepts/firewall>

11. **元数据地址（169.254.169.254）怎么被排除：只有 sandbox-runtime 写清楚了。** 它的解析地址检查把 **cloud metadata endpoints** 与 loopback / link-local / multicast / broadcast / 宿主自身接口地址并列进拒绝集合。
    <https://raw.githubusercontent.com/anthropics/sandbox-runtime/main/README.md>
    **我们 #24 的处理**是「云 metadata 落在 `169.254.0.0/16` 里，注释点名、不单列」。两种做法等价，但**它把 metadata 当成独立一类写在清单里**——这份措辞更抗「有人后来收窄了 link-local 的 CIDR」的漂移。

12. **FQDN 策略的落地方式，Northflank 文档不说。** 官方页只写 "Allow traffic to IP/CIDR/FQDN"，没有 schema、没有解析/重解析/匹配机制的任何说明。
    <https://northflank.com/docs/v1/application/network/configure-network-policies>

13. **CodeSandbox SDK 没有出网控制字段面。** 官方 SDK 文档页（Creating Sandboxes）只有入站侧的 `privacy` 与 `automaticWakeupConfig`；我逐字找过 egress / outbound / internet / firewall / network policy，**一个都没有**。
    <https://codesandbox.stream/docs/sdk/create>
    **标注**：这是「官方文档没写」，不是「产品一定没有」。第三方对比文章说它「doesn't publish equivalent sandbox egress-control documentation」，与我的观察一致，但那是二手。

14. **gVisor 官方文档里没有网络策略。** user guide 只给 `--network=sandbox|host|none`、实验性的 `--network-proxy-path`（且明说 "The external peer is responsible for any egress filtering"）与 TBF 限速；architecture guide 描述 netstack 与报文路径，**通篇不提 netfilter/iptables/包过滤落在哪**。
    <https://gvisor.dev/docs/user_guide/networking/>、<https://gvisor.dev/docs/architecture_guide/networking/>
    **含义**：ADR-0003 把「换 gVisor」当作出路时，出网收边**不会随 gVisor 一起到来**——要么继续用 netns 内的 nftables（gVisor 在用户态实现 netfilter 是社区/第三方转述，官方文档未证实），要么接 `--network-proxy-path` 把过滤责任交给外部对端。

15. **K8s NetworkPolicy 的静默失效。** 官方原文三处："Your cluster must use a network plugin that supports NetworkPolicy enforcement."、"Creating a NetworkPolicy resource without a controller that implements it will have no effect."、"POSTing this to the API server for your cluster will have no effect unless your chosen networking solution supports network policy."
    <https://kubernetes.io/docs/concepts/services-networking/network-policies/>
    **这是我们 ADR-0003「`egress_policy` 是能力声明、`local` 后端硬失败」的外部依据。**

16. **按域名收边会改变 DNS 的失败形态，且有 libc 会因此更坏。** Cilium：Alpine/musl 把代理回的 `Refused` 当作通用失败、会中断 search-list 尝试，因此提供 `--tofqdns-dns-reject-response-code`（默认 `refused`，可改 `nameError` 回 NXDOMAIN）。
    <https://raw.githubusercontent.com/cilium/cilium/v1.17.1/Documentation/security/policy/language.rst>

17. **Cloudflare：策略只在 start 时生效的那一半，与「活沙箱可改」的那一半，文档是分开说的**；而**被拦时沙箱内看到什么，整页不写**。
    <https://developers.cloudflare.com/sandbox/guides/outbound-traffic/index.md>

18. **Claude Code 沙箱：文档明写它在什么之外。** "The sandbox covers shell commands only."——内置文件与 web 工具（含 WebFetch/WebSearch）、hooks、本地 MCP server、LSP、状态栏命令**全部在边界之外**，`allowedDomains` 完全不约束 WebFetch。此外默认关闭，且有一个 `dangerouslyDisableSandbox` 的不安全重试逃生门。
    <https://code.claude.com/docs/en/sandboxing>
    **对我们的直接含义**：**「哪些东西在边界之外」必须有一张表**，而且列得越具体越好——这正是 ADR-0003 开头「出口只有四条」的写法，只是它列的是反面。

---

## 对 Orrery 的推论

1. **我们的结构（`@allow` 排在 `@deny` 之后）在字段面上是少数派，且是更安全的那一派。** 三处具体的反例都指向同一件事：别家的 `deny` 是「补丁」，我们的 `deny` 是「基座」。**规格里应当把这条写成一句可被引用的对比**，否则将来有人会拿 E2B 的「allow 优先」当作行业惯例来要求我们对齐。
2. **「被阻断的连接看起来像成功」不是我们的实现缺陷，而是这一类强制点的共性**（E2B 明写）。所以我们 v1 只做计数器、不做日志的决定**在可观测性上站得住**；但**要在 Skill 文档里说出来**，就像 E2B 让调用方用应用层响应判断一样——否则 agent 会把「装不上包」误判成网络抖动。
3. **DNS 是我们唯一没有策略面的出网通道。** 三家明写了「地址范围策略不约束 DNS」（Vercel）、「DNS 默认不受限」（Modal）、「关网时 DNS 仍是唯一例外」（Cloudflare）。我们 #26 的结论是「上游显式指公网」——**这在 `public` 档下等于「DNS 可以随便用」**。这条不是 bug，但**必须作为已知残余面进规格**，并在将来要收紧时按 Cloudflare 的形态（DNS 走我们指定的解析器）而不是按域名白名单的形态去收紧。
4. **别家的域名维度 = 代理维度，而且每一家都自认有缺口**（fronting / ECH / UDP / 端口 / 环境变量）。ADR-0003 把这条路推迟是**与所有先例的风险面一致的**，不是保守过度。**可以把这份清单直接当「将来要走这条路时必读」的索引。**
5. **「谁编译规则」我们有先例，且我们的做法多买到一样东西**：Modal 的 allowlist 必须创建时就设过才能后来收窄、Daytona 的 `outboundProxyUrl` 创建后不可改、Blaxel 的 firewall ruleset 创建后不可改——**这些约束都是「策略存储在被控侧」的副作用**。我们「控制层只下发策略、Worker 当场重算期望值、期望值不持久化」因此天然没有这个前置条件。
6. **「策略变更要不要掐掉活着的连接」是一条必须表态的轴。** Modal 的答案是要（"Established connections that the new policy no longer permits are terminated."），Cilium 的答案明确是不要（"Live connections are not expired until they terminate"）。**我们的 `ct state established,related accept` 事实上选了 Cilium 那一侧**——但 #24 与 ADR-0003 都没把这条写成一个决定。**建议补一句。**
7. **一个便宜的加固：把 `ipBlock.except` 的自洽校验搬到编译期。** K8s 明写 `except` 越出 `cidr` 会被拒。我们的集合是「编译进 Worker 的常量 + 运行期只增不减」，同样可以在生成规则前断言「`@allow` 与 `@deny` 不相交」这类不变量——**静态校验比运行期回读指纹便宜。**
8. **`egress: none` 的措辞可以借 Daytona 的反面**：别家是「一个会清空其他字段的开关」（`networkBlockAll: true` clears both lists），我们是「一个永不 `connect` 的档位」。**同一件事，一个说「关掉什么」，一个说「从不做什么」**——后者在 #22 的时序不变量下更不容易出事故。

---

## 未能证实 / 未覆盖

- **Cloudflare 当前版（非 0.x）的 `interceptHttps` 默认值**：SDK 0.x 页明写 default `false`；检索结果显示更新版页面可能已改为默认 `true`，但**我没有打开过那一页**，故标 **[未证实]**。同理 `allowedHosts` / `deniedHosts` 在当前版是否仍是官方推荐入口，未证实。当前版只剩 `enableInternet` 与 outbound handler 两种手段是我在 `developers.cloudflare.com/sandbox/network/` 上读到的（该页是节概览，不含字段参考）。
   <https://developers.cloudflare.com/sandbox/network/>
- **E2B 的 `allow_internet_access` 是否被正式废弃**：官方指南与 API 参考**都在用它**，没有废弃标注。指南里 SDK 字段是 `allowInternetAccess`（camelCase），HTTP API 的 `PUT` body 里是 `allow_internet_access`（snake_case）——**同一概念两种拼法**并存，不是废弃。
- **Modal 域名白名单被拒时的实际表现**（静默超时 vs 立即拒绝）：官方只写 "blocked"；「静默超时」来自二手转述，**[未证实]**。
- **Modal 的 `inbound_cidr_allowlist` 能否运行时改**：文档页没有描述更新机制。**[未证实]**（`proxy_traffic_via_sidecar` 亦只有 Python SDK 名、标 experimental，落地细节未覆盖。）
- **Northflank 的 FQDN 规则怎么落地**（是否解析后生成 IP 规则、重解析节奏、DNS 是否绕行）：官方文档页**完全没有**。`spec.egress.denyAll` / `allowTo` / `allowToTags` / `allowToProjects` 这些字段名我只在第三方镜像站（withone.ai）见过，**没有官方 API 参考页可引**，故对照表里只写 UI 文案并标 [未证实]。
  另：Northflank 官方博客 `northflank.com/blog/how-to-design-networking-for-secure-ai-agent-sandboxes` 我只在检索摘要里看到，**未逐字打开**（[未证实]）。
- **Daytona 用宿主 iptables per-sandbox 链（`DAYTONA-SB-{id}`）落地、以及 60 秒对账**：来自 DeepWiki 的第三方源码分析，**不是一手来源**，标 **[未证实]**。Daytona 的 `Domain Firewall` dotfiles 页我也没有打开，域名规则的实际强制点（是否代理）**未证实**。
- **Runloop 的 `allowed_cidrs` 与 `allow_runloop_mirrors`**：只在第三方 SDK 类型定义/摘要里出现，**官方网络策略文档页的配置表里没有**。以官方页为准 = 只有主机名维度的 egress allowlist。
- **Cilium 的 `--tofqdns-min-ttl` 默认 1 小时**：检索摘要提到，但我读到的 v1.17.1 文档源里**只有「TTLs are respected」与每 endpoint 50 IP 的上限**，没看到 min-TTL 的默认值，标 **[未证实]**。
- **CodeSandbox SDK 是否真的没有出网能力**：只能确证「官方文档没写」，不能确证「产品没有」。
- **Fly.io 的 6PN / `flycast` 与 network policy 的关系**：官方 network policies 页只说「不影响经 Fly Proxy 的流量」，而 flycast 就是 Fly Proxy——**私有网络流量是否被策略覆盖，官方没有明说，[未证实]**。machine 上是否默认有公网 IPv6 egress，该页也未提。
- **未覆盖的产品**：AWS Lambda MicroVMs 的出网控制（本票未查）、microsandbox（ADR-0003 提到的「默认拒私网」姿态来自前一份调研，本票未复查）、以及任何非沙箱类容器平台（EKS/GKE 的 VPC egress 等）。
- **未验证的一手源码**：本票没有读任何厂商的源码；「谁编译规则」一栏里标了「节点侧/控制面」的，来源都是官方文档的架构描述（或注明是第三方的除外）。
