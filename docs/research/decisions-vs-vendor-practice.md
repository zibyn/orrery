# 已关票据 vs 厂商做法：重新确认（简单优先）

调研日期 **2026-10-16**。同批四份一手调研：`container-sandbox-architecture.md`（架构与接口）、`file-transfer-mechanics-and-path-safety.md`（文件搬运与路径安全）、`sandbox-git-integration.md`（git 接缝）、`sandbox-network-policy-apis.md`（出网策略接口）。

本文件只做一件事：**拿厂商做法反查 #2–#26 的每条已裁决定**，逐条判「保留 / 可简化」，并把**便宜且不需要重新裁决**的追补列成清单。凡引用厂商事实，一律注明来自哪份文档的哪节；本文件不新增一手取证。

---

## 0. 一句话结论

**整体比厂商实践更小，不是更大。** 三处结构性更小：出网三档 vs 别家四个旋钮（`sandbox-network-policy-apis.md` §字段对照表）、单一持久化答案 vs 别家三种并存（`container-sandbox-architecture.md` §7 ⚠️7）、凭据零入沙箱 vs 四家四个形式都放进执行侧（同上 ⚠️8）。

真正**比厂商大**的只有三处——协议代际窗口（#19）、日志续传握手（#8）、local 第二后端（#10）。三处都已在 2026-10-16 裁定（§3）：前两处删掉，第三处**反向**——`local` 被提为一等的部署形态。其余全是便宜的追补，已按 §4 全部落进 ADR 与规格。

---

## 1. 判定表

| 票 | 我们的决定（要点） | 厂商对照 | 判定 |
|---|---|---|---|
| **#6** Worker 注册与身份 | 登记包 → 24h mTLS 证书 → 50% 自助续期；流上不发身份（TLS 层）；撤销不吊销证书 | 与 kubelet 的 bootstrap token → CSR → 轮转同形；`prior-art` §4 的 GitHub/GitLab runner 注册是更弱的形式 | **保留**——这是标准形状，不是自定义 |
| **#7** 任务模型 | Task 是完整信封、一条命令、exit code + 日志流 | `sandboxd` 的 `Start`（配置 `{command, env_vars, cwd}` + `oneof{InitEvent, stdout, stderr, ExitEvent}`）逐项对应（`architecture` §8.2） | **保留**；借三条追补（§4） |
| **#8** 状态回报与断线恢复 | 单条 BIDI 多路复用；终态控制层独占；epoch + 逐任务裁决 + 水位线续传 + 三档心跳 + `unknown` 不粘 | 断线期间**继续跑、绝不重派**在别家没有对应物（别家控制面失联＝沙箱消失）；但自拨号这一族里 Coder/Buildkite 的 stop-record + `reconcile()` 是同一形状（`prior-art` §4.4） | **保留主体**；续传握手**已删**（§3.2） |
| **#9** Workspace 生命周期 | 热/冷两档、机器绑定、绝不复用 id、回收裁决在控制层 | Agent Sandbox 的 `shutdownPolicy: Delete\|Retain`（默认 Retain）是同一个旋钮（`architecture` §8.10）；Codespaces 干脆不持久化（git 即持久化）、Docker Sandboxes 三种并存 | **保留**——我们是这批里持久化答案最少的 |
| **#10** Runtime Provider | Worker 进程内 Go interface，五动词 + `Capabilities()`；文件读写不进接口；v1 只认镜像 digest | CRI 的 RuntimeService 37 个 RPC **零个文件操作**（`file-transfer` §发现 7）——文件面不属于运行时接口，我们是多数派 | **保留接口**；`local` 后端见 §3.3 |
| **#11** 调度器 | 控制层内同步纯函数、不排队、不抢占、占用由控制层记账 | Agent Sandbox 用 K8s 调度 + WarmPool；Modal 已把集中调度改掉（`prior-art` §1.3） | **保留**——我们的比两家都小 |
| **#12 / #13** Broker 与 Git Provider | 授权 / 租约两域对象；铸造面与注入面拆开；Worker 代码里不出现 provider 名 | **Argo 是正面先例**：凭据只挂 init/wait/supervisor 容器，主容器没有；反面是 DevPod/Tekton/OpenHands/`sandboxd` 四家（`architecture` §8.5、⚠️8） | **保留**，并把正反两面写进 ADR-0001（§4） |
| **#14 + #5** 平台存储 | 一次存取一个预签名 URL；中转区无主、按前缀切 | 中转区之所以存在是**拓扑决定的**：Worker 只拨出、不可被连入 ⇒ 字节必须落在 Worker 能去取的地方（rustFS 已经是那个地方） | **保留**——这不是多余的一层，是「沙箱不可被连入」的直接代价 |
| **#15** 能力面 | CLI ⊇ MCP；无复合工具、无文件面工具（要文件＝文件面三动词） | Agent Sandbox 的 `sandboxd` 把文件面与命令面切成**两个协议端口**并写死了理由；OpenHands「读写在命令通道上 / 批量搬运独立」；Tekton 的 Workspace 与 Result 本就是两个对象（`architecture` §8.3） | **保留**——三处独立确认「两个面」，我们站在多数派 |
| **#16** 出网边界 | 默认开、三档、私有无条件拒、控制层地址无条件减去、零监听端口 | 默认开是行业默认（14 家里只有两家默认关）；`@allow` 排在 `@deny` 之后是**反先例**但更严（`network-policy` §先读 2） | **保留**；两处补表态（§4） |
| **#17 / #18** 对外 API | 手写 OpenAPI 单一真源；长轮询；problem+json；两类 Bearer | 未逐家对照（本批未覆盖对外 API 形态） | **保留**；无新证据 |
| **#19** Worker 升级 | 闸门是协议代际；控制层必须说得出口窗口内每一代；不排空、收养 | **先例里没有一家讲过 agent 怎么装/升/灰/退**（`prior-art` 结论，本批仍未找到） | **已简化：单代 + 停机升级**（§3.1，落 ADR-0005） |
| **#20 / #25** 文件面 | 按路径四动词；元数据走流、字节走对象存储；上传两阶段 + 完成调用 | Docker Sandboxes 只有 `sbx cp` 一个动词（无列目录、无完成信号）；Agent Sandbox 有四个 HTTP 方法但**也没有完成信号**（`architecture` §8.4） | **保留**——第四动词是「字节不经控制层」的直接代价，不是多余 |
| **#23** 中转区 | 无主前缀、时间驱动回收、单一保留期、不记账 | Argo 的 `ArtifactGCStrategy` 默认 `Never`（不删）——差别是**中转区是平台自有的无主前缀，不是用户产物仓**（`architecture` ⚠️6） | **保留**，把差别写出来（§4） |
| **#24 / #26** 出网规则集 | 两条 `policy drop` + 四集合 + 只做计数器 + 无条件重装 | 没有一家产品是「容器 netns 内自建规则集」（`network-policy` §先读 4）；但「控制层下发策略、节点侧编译」与 E2B/Daytona 同形 | **保留**——已真机实测（#26） |

---

## 2. 我们比厂商小的三处（值得写进规格，防止将来被「对齐」）

1. **出网请求面**：别家是 `allowOut`/`denyOut`/`rules`/`egressProxy`（E2B）、三套 allowlist（Modal）、一整个 `networkPolicy` 对象（Vercel）、防火墙规则集语法（Blaxel）；我们是三档 + 一个 CIDR 列表。
2. **持久化答案**：别家三套并存（宿主目录 + 云端 volume + 模板快照 / 十一后端 + 插件）+ 默认不删；我们只有宿主目录一棵树。
3. **凭据位置**：别家凭据进执行侧；我们零凭据 + 动词通道 + 宿主代发（合起来无先例，见 `sandbox-git-integration.md`）。

---

## 3. 三处「比厂商大」的逐个表态与裁定

**裁定（2026-10-16）**：§3.1 **砍成单代 + 停机升级**（不是我原先推荐的「加法演进纪律」）；§3.2 **按推荐删掉续传**；§3.3 **反向**——`local` 提为一等的真实后端。三处都已落（§3.1 → ADR-0005，§3.2 → `#8` 追补，§3.3 → ADR-0003 的 `local` 一条 + `#10` 追补）。

### 3.1 #19 协议代际窗口 —— 裁定：砍成单代 + 停机升级

**落地**：控制层只说自己的那一代，`StreamWelcome.protocol_version` 的闸门、`INCOMPATIBLE`（不自清、拒接活、60s 慢速重试、大声）全部保留；**「全机队最低可接受代际」这个旋钮取消**（单代之下没有可动的东西）。`protocol_generation` 变了就在**同一个停机窗口内两侧一起换**；代际之内，滚动 Worker（`SIGTERM → 换二进制 → 拉起 → 收养`）与控制层自身重启照旧不打断任务。ADR-0005 已就地改。

**与被否掉的「加法演进纪律」相比**：那一版保留不停机、代价是协议评审上要守一句「这是加法吗」；这一版连那句守门也不要，代价是换代际时必须停机。裁定理由是内网单控制层、机队小，停机窗口是可接受的运维动作——而**减掉的日常成本是实打实的**（原案要求控制层常备第二套消息处理）。

**我们怎么做**：控制层必须说得出口窗口内每一代（N 与 N-1），下沿是控制层唯一旋钮，双向可动；不排空；升级＝`SIGTERM` → 换二进制 → 拉起 → 收养。

**有没有先例**：没有。行业空白（`prior-art` §19 已记，本批仍未找到）。

**问题**：这句话的代价是**每一次协议改动都要写两遍**（两代消息都要能收发），而且是永久性的纪律——它是这份设计里唯一一处「为了不停机，日常开发成本翻倍」的承诺。

**（以下是未被采纳的原推荐，留作记录）更简单的形状**：把「窗口」从**机制**改成**纪律**——
- Control↔Worker 的 proto 只做**加法演进**（proto3 未知字段忽略、新字段可选；#18 对外 API 已经定了「加法演进」，同一套纪律上移一层）；
- 于是旧 Worker 与新控制层在常见改动下**天然互通**，控制层不需要维护两套消息处理；
- 只有**破坏性变更**才付出双实现或停机窗口的代价，而破坏性变更本身应当罕见，且它换来的是「代际 +1」这个显式事实（闸门、`INCOMPATIBLE`、大声失败、不自清——这些机制全保留）。

**代价**：把「可不可以互通」从**控制层代码的事实**变成**协议纪律**，需要一次 code review 上的守门（proto 变更评审时问一句「这是加法吗」）。**这条方案未被采纳**：它与「单代」买到的东西相同（不停机），但保留了「协议要能被旧端读懂」这条常备纪律；裁定选了停机窗口，换掉这条纪律。上面的「砍成单代」才是现行形状。

### 3.2 #8 日志续传握手 —— 裁定：删续传，只留「追加或标缺口」

**落地**：日志发出去不等确认，控制层收到即追加；**连接断过的那段不补、不重放，在那条任务上标缺口**；三值完整性（`complete`/`gapped`/`truncated`）与 `from_seq`/`max_bytes` 分页**全部保留**（分页读的是落下来的那份，与续传无关）。删掉的是**水位线序号 + 按序号续传 + 超窗口丢最旧字节 + `delivered_seq` 逐任务裁决**——也就是控制层唯一一处「为单个任务缓冲它当下发不出去的数据」的状态。`complete` 不再需要「可撤回」这个动作（缺口当场就标，事后没有要收回的话）。

**我们怎么做**：服务端水位线序号、按序号续传、超窗口丢最旧字节并插**显式 gap**、`delivered_seq` 逐任务裁决、日志完整性三值且**可撤回**。

**有没有先例**：没有一家厂商做日志续传；E2B 的官方口径是「没有任何单条连接保证超过一小时、重连是调用方的责任」（`prior-art` §1.1）。

**问题**：为了「控制层重启后日志逐字不断」，控制层必须持有**它当下发不出去的字节**（水位线 + 丢弃策略 + 序号确认）。这是整个设计里唯一一处**控制层为单个任务缓冲数据**的地方，也是「日志与水位线的存储形态与保留期」至今悬在 fog 里的原因。

**更简单的形状**：日志**发出去就不等确认**（fire-and-forget），控制层收到即追加；连接断过的那些字节**不补、不重放**，在那条任务的日志上**标一个缺口**。三值语义（`complete`/`gapped`/`truncated`）与 `from_seq` 分页**全部保留**（分页读的是落下来的那份，与续传无关）。

**代价**：控制层重启期间跑着的那几条任务，日志会出现一段缺口——**能力上不比今天弱**（今天超窗口同样丢最旧字节并插 gap），只是缺口的成因多一种。要逐字完整的调用方，本来就应该把要留下的东西放进工作区树（#20 那句可教的话）。

### 3.3 #10 的 `local` 后端 —— 裁定：**反向**，提为一等的部署形态

**裁定理由（用户给的，比我的推理更贴现场）**：部分现有机器上再次调用 docker 比较难，任务必须在那种机器上真跑起来；因此 `local` **不是**「让接口看起来像接口」的参考实现，也**不许降级成进程内 fake**——它要真实支持。

**由此改写的两处**：
- ADR-0003 里那条「`local` 必然 `unsupported`、`required.egress` 硬失败」从一句脚注升级为**一个必须被调用方知道的部署事实**：那台机器上 `egress` 只能按 `best_effort` 提，边界在那里不存在，谁来用这台机器是一个信任判断。ADR-0003 已就地补。
- 「隔离等级是后端上报的事实」（ADR-0003 的承重句）在 `local` 上是最低档：没有容器边界、`Capabilities()` 必须如实报——`#10` 的三值上报机制本来就是为这种后端写的，此处是它的第一个真实用例。

**未定，需要一次表态**：`local` 上「什么是沙箱」要写死。至少三条待定：① 命令以谁的身份、在哪个 cwd 跑（宿主直接跑？`unshare` 一个 mount/net ns？）；② `egress` 在 `local` 上到底做不做得了（`unshare -n` 只够 `none`，`public` 要重造容器网络）；③ Workspace 目录在 `local` 上与宿主其它用户的隔离（容器档靠一 Workspace 一 uid/0700，宿主上要另说）。这一条进规格的「沙箱侧」一节。

**我们怎么做**：`local` 同接口但生产禁用，作为参考实现。

**问题**：这是一个**完整的第二个 Runtime Provider 实现**，唯一用途是「让接口看起来像接口」与开发期免 docker。YAGNI 的那条线正好压在它上面：接口已经有 docker + podman 两个真实现要落，参考实现不是必需的第三个。

**（未被采纳的原推荐，留作记录）更简单的形状**：v1 规格里只写 docker（podman 作为已知的第二个后端形状），把 `local` 降级为**实现期的测试替身**（进程内 fake，不是一份要维护的后端）。**这条推理漏了现场约束**——「有些机器上 docker 用不了」不是假设，是现实，所以裁定反着来。

---

## 4. 已落的追补（便宜、不需要重新裁决）

**已按裁定落进 ADR 与规格**：ADR-0001 / 0002 / 0003 / 0006 / 0007 就地改；无 ADR 归属的两批（Task 消息三条、shim 四条硬约束）落 `#7` / `#17` 的评论。

### ADR-0001（凭据只交付给可信侧）

- **Argo 的正面先例**：凭据只挂 init/wait/supervisor 容器，主容器没有（`architecture` §5.1、§8.5）。论证从「我们这么想」升级成「有正反两面一手材料」。
- **反面清单**（四家四个形式，同一个取舍）：DevPod *"forward git credentials to a remote machine"*、Tekton `git-clone` 的 `basic-auth`/`ssh-directory` 工作区、OpenHands 把 `OH_SESSION_API_KEYS_0` 写进容器环境、`sandboxd` 在沙箱内校验 bearer token（`architecture` ⚠️8）。

### ADR-0003（出网边界）

- **`@deny` 是基座、别家的 `deny` 是补丁**：E2B 原文 "allow rules always take precedence over deny rules"、Vercel 拿 `subnets.allow: 10.0.0.0/8` 当 private network access 的正常用法、Daytona 的 IP 列表无私有段约束（`network-policy` §缺口 6）。**把这条写成一句可被引用的对比**，否则将来有人拿 E2B 要求对齐。
- **活连接轴要表态**：Modal 会掐掉不再允许的已建立连接，Cilium 明确不掐；我们的 `ct state established,related accept` 事实上选了 Cilium 那侧，但 #24 与 ADR-0003 都没写成决定（`network-policy` §推论 6）。
- **Considered Options 补两条默认值**：Agent Sandbox 的 `NetworkPolicySpec` 空列表 = ingress/egress 双 Default Deny；Docker Sandboxes 初始化后白名单制且推荐 `balanced` 中间档（`architecture` ⚠️4）。
- **Claude Code devcontainer 为了在容器里装防火墙必须给 `NET_ADMIN`+`NET_RAW`**——「容器保持默认能力集」的反面对照，也是我们选「Worker 在宿主侧 `nsenter` 装规则」的正面理由（`architecture` §8.8）。进 Consequences。
- **DNS 是唯一没有策略面的出网通道**：Vercel「`subnets.allow` leaves DNS unrestricted… code can use those lookups to send data over DNS」、Modal「DNS traffic is allowed without restriction except when all outbound traffic is blocked」、Cloudflare 关网时 DNS 仍是唯一例外（`network-policy` §缺口 7、§推论 3）。#26 的「上游显式指公网」在 `public` 档下等价于 DNS 随便用——**作为已知残余面进规格**，将来要收紧按 Cloudflare 的形态（指定解析器）走。
- **gVisor 不带来出网收边**：官方文档通篇没有网络策略，只有 `--network` 三档、实验性 `--network-proxy-path`（且明说 "The external peer is responsible for any egress filtering"）与 TBF 限速（`network-policy` §缺口 14）。ADR-0003 把「将来换 gVisor」当出路时，边界要重算。
- **被拦的连接从沙箱内看起来仍可能成功**（E2B 原文）——不是我们的实现缺陷，是这一类强制点的共性；v1 只做计数器站得住，**但要在 Skill 里说出来**，否则 agent 会把「装不上包」误判成网络抖动（`network-policy` §推论 2）。

### ADR-0006 / ADR-0008（文件面与落地侧）

- **照抄 moby 这一代**：整条解包链走 `os.Root`（openat2 语义）+ `filepath.IsLocal` 拒绝，**不在用户态改写路径**；containerd 那一代是 `strings.HasPrefix` + 越界时静默 clamp 到 root，**不要抄**（`file-transfer` §发现 1）。
- **规格级禁令：本系统任何一处都不用 strip-then-compare。** 它就是那批 CVE 的共同根因（GNU tar `safer_name_suffix` 剥离而非拒绝、node-tar 只剥一层 path root、kubectl cp 的 `stripPathShortcuts`+`isRelative` 修了六轮）（`file-transfer` §发现 2、§CVE 清单）。
- **`os.Root` 不禁止 bind mount / 设备文件 / `/proc` 特殊文件，`O_NOFOLLOW` 只管最后一段** ⇒ 「只处理普通文件」必须自己判，且要 `Lstat` → `open` → `fstat` **复查 inode 一致**（`file-transfer` §发现 4）。
- **`os.Root` 没有 `CreateTemp`**，`rename` 的原子性只在同一挂载点内成立 ⇒ 临时名自己编、临时文件与目标同目录（`file-transfer` §发现 5）。
- **新缺口：树内崩溃残留的临时文件没有扫帚**（中转区有保留期，树里没有）（`file-transfer` §发现 5）。
- **Agent Sandbox 的 `/v1/files` 有一份可直接对着实现的语义清单**，逐条过一遍我们的四动词：`GET`/`HEAD`/`PUT`/`DELETE`、原子写（temp + rename）、自动建父目录、非空目录 `DELETE` 回 **409**、越界回 **403**（不是 404）、`symlink-aware sanitization`、**无跨请求事务、无 CAS，协调责任在调用方**（`architecture` §8.4）。
- 印证（不需要改）：CRI 零文件 RPC（`file-transfer` §发现 7）；预签名 URL 可重复使用、无法撤销，下载已开始的传输不受过期影响（`file-transfer` §发现 8，撑住 ADR-0007/0008 的地基）。

### Task 消息与能力通道（#7 / #17）

- **借 `sandboxd` 三条**（便宜且事后难补）：一个「短命令不必开流」的一元 `Execute` 形态；一个显式的 `InitEvent`（命令真的起来了才从 `assigned` 变 `running`）；`exit_code` 用 `128+n` 表示被信号杀死（"as a shell reports it"）（`architecture` §8.2）。
- **shim 的硬约束**（`sandbox-git-integration.md`：git v2.53.0 文档 + 源码 + 实测）：
  - `list` 返回空表 = git 判定「空仓库」并**退出 0** ⇒ 通道不可用时 shim **必须死掉**，不能"礼貌地"回空表；
  - `option cas` 是**静默的必需项**，回 `unsupported` 会让 `--force-with-lease` 退化成盲 force；
  - `fetch` **没有任何带内错误通道**（只能 stderr + 非零退出，实测 clone 128），`push` 的 `error <dst> <why>` 是把宿主侧 `stderr_head` 送到 agent 眼前的**唯一**正式通道；
  - 空仓库**只在「不指定 ref」时**不是失败（`fetch <ref>` 与 `clone -b` 在空仓库上都是 fatal）——Tekton `git-init` 的 `git init` + `fetch --update-head-ok` + `checkout FETCH_HEAD` 是现成配方。
- **「clone 是平台动作」有强先例**：Codespaces 的 clone 在平台侧、容器创建之前发生，`/workspaces` 是「持久目录、mount 进容器」——与我们逐字同构；Copilot coding agent 明文「cannot directly run `git push`」，push/PR 全由平台代做（`sandbox-git-integration.md`）。

### 域模型与规格

- **「树在不在」应当是记录里的事实**，不靠 `ls` 去猜（Tekton 的 `$(workspaces.<name>.bound)` 是可查询的一等值）（`architecture` §8.6）。
- **零监听端口是能力缺口，不只是优势**：厂商都在沙箱里放监听端口、控制面都有进沙箱的路径（Agent Sandbox 的 router/`X-Sandbox-ID`、OpenHands 的 `0.0.0.0` 服务器、Codespaces/Ona 的端口转发）。我们换来的是「NAT 后可部署」与「沙箱里没有可认证物」，代价是**沙箱内服务不可达**——写进能力面文档，别让调用方以为是自己配错了（`architecture` §8.11、⚠️1）。
- **`SandboxRecord` / `SandboxInfo` 二分**＝「控制层的视图永远可能陈旧」的正确落点：存下来的那一半与活着的另一半不要共用一个类型（`architecture` §8.7）。
- **中转区默认删 vs Argo `artifactGC` 默认 `Never`**：差别要说出来（平台自有存储的无主前缀 ≠ 用户产物仓）（`architecture` ⚠️6）。
- **先例补正**：`vendor-file-apis.md` §0 第 3 条「唯一例外是 Buildkite」要补 Argo——它是「声明的产物」完整形态的另一极（`architecture` §8.9）。

---

## 5. 仍然悬着的 fog（本批没能收口）

- **shim 怎么进镜像**：remote helper 的查找路径（规范只写"自动调用 `git remote-<transport>`"，**没写**按 `PATH` 还是 `GIT_EXEC_PATH` 找）——差一步一手依据，需要一个探针（`sandbox-git-integration.md`）。
- **日志与水位线的存储形态与保留期**：本批没有可借的厂商对照；与 §3.2 的简化建议互为前提（简化了续传，落哪、留多久仍然要定）。
- **gVisor 那条出路要重算**（§4 ADR-0003 最后一条）。

---

## 6. 依据索引

| 本文件小节 | 一手事实来自 |
|---|---|
| §1、§2 | 四份新调研的横向对照小节（`architecture` §7、§8、⚠️ 汇总；`network-policy` §字段对照表；`file-transfer` §CVE 清单；`git` 接缝对照） |
| §3.1 | `prior-art` §19 + 本批未找到先例 |
| §3.2 | `prior-art` §1.1；`architecture` §7（日志不在任何厂商对象模型里） |
| §4 ADR-0001/0003 | `architecture` §5.1/§8.5/§8.8/⚠️4/⚠️8；`network-policy` §缺口 6/7/14、§推论 2/3/6 |
| §4 ADR-0006/0008 | `file-transfer` §发现 1/2/4/5/7/8、§CVE 清单；`architecture` §8.4 |
| §4 Task/通道 | `architecture` §8.2；`sandbox-git-integration.md`（remote helper 规范节 + 实测节） |
| §4 域模型 | `architecture` §8.6/§8.7/§8.9/§8.11、⚠️1/⚠️6 |
