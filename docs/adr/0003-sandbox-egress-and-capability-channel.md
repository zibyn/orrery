# 沙箱的出口与能力通道

沙箱的出口只有四条——挂载、能力通道、日志、网络。#16 决定：**网络默认开着**（`egress: public`，公网可达而私有段、loopback、link-local、云 metadata 与宿主地址全拒，控制层与会话地址在规则生成时被**无条件减去**），因为远程依赖是调试私有仓的刚需，密闭沙箱对这个系统没有用；**能力通道**是沙箱唯一条人造通道，一个 bind mount 进容器的 unix domain socket，只认 `{fetch, push}` 两个 git 远端动词、只作用于本 Workspace 的那一个仓库、一次操作一次租约。由此三件必须一起记住的事：**沙箱不是安全边界**（隔离等级是后端上报的事实，唯一硬假设是宿主与内核可信，出路是将来换 gVisor）、**一机一租户 v1 不承诺**、**出网档位是 Workspace 记录里的显式事实**。

**Status**: accepted（`#16`）

## Considered Options

- **默认无网（密闭沙箱）**：把私有仓代码的外泄面收到只剩产物、日志与写回本仓三条，且出网白名单从可选变成不必存在。否决理由：改代码要能装远程依赖（`pip` / `npm` / `go mod` / `apt`），密闭沙箱把核心用途一并砍掉。**这是用可用性换保证，不是用自由度换保证**，与 ADR-0001 不同构。
- **出口正向代理 + 域名白名单**（不做 MITM 的 CONNECT 版，只放行只读源）：能把「缩小外泄面」推到接近「消除」，比 Vercel 那套 SNI 匹配 + TLS 拦截 + per-sandbox CA 轻得多。否决的是**时机**不是方向：先例都承认按域名匹配有缺口（SNI 挡不住 domain fronting；E2B：被阻断的连接从沙箱内部看起来还可能是成功的），而它换来的是包管理器不再透明可用——每个工具都要配 proxy 环境变量，还得防着有人绕过。进 map 的 fog，等真有需要收紧的仓库。
- **调用方声明 CIDR 白名单**：最灵活，也最容易在某人图省事时写成 `0.0.0.0/0`（等于把 `public` 的默认保护抹掉）。收成 `public` 的**追加入口**，不是替换。
- **把白名单做进 Runtime Provider 的 Go 接口**：v1 的 `network.egress_policy` 是**能力声明**（`enforced | unsupported`），不是策略位；策略归控制层下发，Provider 只负责在 netns 里落地。与 #10「匹配权威在调度器、Provider 拒绝即 bug 信号」同形。

## Consequences

- **容器必须保持默认能力集**（无 `CAP_NET_ADMIN`）。有它就能自己改路由——netns 内的 nftables 是唯一真正的强制点，「不给路由」不构成边界。这条从「顺带的严谨」升级为**承重**：出网策略的全部效力挂在它上面。
- `local` 后端必然 `unsupported`，请求 `required.egress` 硬失败——它连命名空间都没有（#10）。
- netns 内 nftables **不经 OCI hook**（**修正 #22**）：docker 没有通用 hook 配置面，边界改由 Worker 在宿主侧落地——`docker create --network none` → `start` → `nsenter -t <pid> -n` 装规则 → `docker network connect`（每台 Worker 一个 user-defined 网络，容器内 `127.0.0.11` 内嵌解析器由此而来）。于是「#3 里被推迟的自建件」落成 Worker 的一个步骤，**不再是独立部署件**。`egress: none` 的实现就是永不 `connect`——连路由都没有，比接网后全拒更彻底。
- **每次沙箱内 `git fetch` / `push` 都要一次控制层往返、都可能失败**——能力通道是租约的一次操作一次模型的直接后果，agent 的手感会比本地 git 差。Skill 文档要把它说出来，别让调用方以为是网络抖动。
- **控制层不可达靠两条互相独立的堵法**（密钥不进挂载 / 地址无条件减去），任一条单独成立都不足以安心：前者防身份借用，后者防 `Enroll` 这个唯一未认证入口被从沙箱侧够到。
- **规则集的形状**（`#24`，把上面的档位编码成能实现的东西）：一张 `inet` 表、`output` 与 `input` 两条 base chain 都 `policy drop`（**不挂 `forward`**——netns 里没有第二个接口，容器又拿不到 `CAP_NET_ADMIN`，转发路径不可达，挂一条永远不走包的链只是噪音）。`output` 的次序是「回包 → `lo` → `@deny` drop → `@allow` accept → `!= @deny` accept」，其中 **`@allow` 排在 `@deny drop` 之后**就是「无条件减去」的全部实现，也顺带让 `allow: [0.0.0.0/0]` 不再能抹掉 `public` 的保护——上面 Considered Options 里「最容易在某人图省事时写成 `0.0.0.0/0`」这条否决理由到此结构性消失。`public` 就是「全空间减私有」，用负向集合编码；私有段清单是**编译进 Worker 的常量**（`127/8` 与 `::1` 不进清单，netns 里只可能走 `lo`；`ff00::/8` 不动，邻居发现要靠它），再加运行期枚举的宿主自身地址；集合分四个族（CIDR 元素必须 `flags interval`，且不能跨 v4/v6 复用）。`egress: none` 照装同一套（效力来自永不 `connect`，规则是第二道）。由此推出一条**新的残余面**：容器内嵌解析器的**上游转发也在容器 netns 里出网**，所以沙箱的上游解析器必须显式指到公网——否则内网 DNS 会被 `@deny` 吃掉，`public` 档连远程依赖都装不了（`#16` 的「容器内解析器必须是 loopback 那个」只解决了第一跳）。推断链（`oifname` 不要求接口存在、`nft -f` 是单事务、内嵌 DNS 的 nat 早于 filter）见 `docs/research/nftables-netns-egress-ruleset.md`；真机实测见 `#26`。
