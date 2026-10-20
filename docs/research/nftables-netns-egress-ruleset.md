# 容器 netns 出网规则集：nftables 语义与 docker 侧事实核查（wayfinder #24，真机实测 #26）

只核查事实，不重新论证设计。设计前提（= `#24` 定稿、`#26` 实测的那个规则集）：`inet` 表、`output`/`input` 两条 base chain 均 `policy drop`，OUTPUT 次序 `ct state established,related accept` → `oifname "lo" accept` → `ip daddr @deny drop` → `ip daddr @allow accept` → `ip daddr != @deny accept`，**IPv6 逐条并列**（`ip6 daddr @deny6 drop` / `ip6 daddr @allow6 accept` / `ip6 daddr != @deny6 accept`）；装规则时 netns 里只有 `lo`。

**可信度分档**（每条结论都标了）：
- **【文档明说】** 一手文档原文（man / wiki / 官方 schema）。
- **【源码读出】** 源码文件+行号，未经运行。
- **【实测确认】** 本机非 root 可做的验证（只做只读 / 解析级）。
- **【真机实测】** `#26` 在真机上把整份规则集跑完得到的输出（docker 29.8.2 / 内核 7.0.0-38-generic / Ubuntu 26.04.1，2026-10-16）。无 root 的等价路径：把带 `NET_ADMIN` 的辅助容器 join 进目标 netns——同一 netns、同一条 netlink 路径，宿主 netfilter 一个字没动。**连 `nft list ruleset` 都要这个能力**，没有它直接 `Operation not permitted (you must be root)`。
- **【推测】** 依据不足的推断，**不要当事实用**。

**取证环境**：`nftables` 仓库 `91373de3`；`torvalds/linux` master `045958ac`；`moby/moby` master `20b6b386`（三者均 2026-10-15/16 拉取）。man page 为随二进制安装的 nftables 1.1.6。

---

## A. nftables 语法与语义

### A1. 一条 `inet` 表的 `output` chain 同时承载 `ip daddr` 与 `ip6 daddr` —— 合法
**【文档明说】** nftables wiki《Nftables families》："Within a table of *inet* family, both IPv4 and IPv6 packets traverse the same rules. Rules for IPv4 packets don't affect IPv6 packets and vice-versa." 并给出同一 chain 里并列 `ip saddr` 与 `ip6 daddr` 规则的示例。
<https://wiki.nftables.org/wiki-nftables/index.php/Nftables_families>

**【文档明说】** 官方测试套件里的真实规则集 `tests/shell/testcases/nft-f/dumps/sample-ruleset.nft`：`table inet filter` 的 `chain INPUT { type filter hook input priority filter; policy drop; iif "lo" accept ... }`，同一条链内混用 `ip saddr {...}` 与 `ip6 saddr {...}`。

**【实测确认】** 本机 `nft -c -f`（非 root，仅解析阶段）对同一条 inet chain 内 `ip daddr != {...}` 与 `ip6 daddr != {...}` 并列无语法报错。

**IPv4 规则不误伤 IPv6 的机制**【源码读出】：协议不匹配时 payload 表达式返回 BREAK，该规则被跳过（这就是下面 D-1 那条后果的成因）。

### A2. 匿名集合 + `!=`
**【文档明说】** `nft(8)` SETS 节："Anonymous sets are sets that have no specific name. The set members are enclosed in curly braces... They cannot be updated." 又说："Equality of a value with a set is given if the value matches exactly one value in the set (which for intervals means that it's contained in any of them)." → `!=` 即该式的取反：**不在集合里就匹配**。
**【文档明说】** 官方测试套件到处在用：`tests/shell/testcases/sets/typeof_sets_1:14` `vlan id != { 1, 2 }`；`tests/shell/testcases/optimizations/single_anon_set:23` `tcp dport != { 22 } drop`；`tests/shell/testcases/nft-f/dumps/sample-ruleset.nft:123` `ip6 saddr != { fd00::/8, fe80::/64 } ... reject`。
**【实测确认】** `nft -c -f` 对 `ip daddr != { 10.0.0.0/8, 172.16.0.0/12 } accept` 无语法报错。

**与 `policy drop` 组合 = 「非私有即放行」**：是，但**仅限 IPv4**（见 D-1）。

**文档里确实有的坑**（两条，都不是 `!=` 本身的问题）：
1. **空集是语法错误。**【源码读出】`src/parser_bison.y:4634` `set_expr : '{' set_list_expr '}'`，而 `set_list_expr` 至少要一个成员；`'{' '}'` 只出现在 `initializer_expr`（`elements = { }`，`src/parser_bison.y:4811`）。**【实测确认】** `ip daddr != { } drop` → `Error: syntax error, unexpected '}'`。
2. **位掩码的 `{a,b}` ≠ `a,b`。**【文档明说】`nft(8)` BITMASK TYPE 节："Note that expression bit[,bit]... is not generally the same as expression {bit[,bit]...}..."，例如 `ct state established,related` 与 `ct state { established, related }` 因位互斥恰好等价，但 `tcp flags { syn, ack }` 与 `tcp flags syn,ack` 不等价。本规则集里 `ct state established,related` 是位列表（正确写法），不必改成集合。
3. **区间集合元素写法**：范围用 `10.0.0.0-11.0.0.0`、前缀用 `172.16.0.0/16`，同一集合内可混用。【源码读出】`tests/shell/testcases/sets/0001named_interval_0`。
   另：**未找到** `!=` 配区间集合的专门测试（`grep '!= {…-…}' tests/shell/` 为空），其语义只能按上面那条「集合等价性」定义推；**不是坑的记录，只是没找到针对性用例**。

### A3. 命名集合
**`ip daddr @deny` / `ip daddr != @deny` 合法**【文档明说】wiki《Sets》原文即 `nft add rule ip filter output ip daddr != @blackhole accept`；官方 dump `tests/shell/testcases/listing/dumps/0022terse_0.json-nft` 里有 `"op": "!="` + `"right": {"set": [...]}` 与 `"right": "@example"` 两种形态。

**元素可以是 CIDR / 区间，但集合必须声明 `flags interval`**【源码读出+测试即规约】`tests/shell/testcases/sets/0003named_interval_missing_flag_0`：`add set t s { type ipv4_addr; }` 之后 `add element t s { 192.168.0.0/24, ... }` **必须失败**（该测试断言 nft 拒绝并以此 exit 0）。声明了 `flags interval` 才能装 `10.0.0.0-11.0.0.0` / `172.16.0.0/16`（`0001named_interval_0`）。
（`nft(8)` 另注：缺 flag 时 nftables **可能**自动推断，「recommended to specify all required features in the set/map definition manually」。）

**同一集合跨 `ip`/`ip6` 复用：不行。**【文档明说】集合只有一个 `type`，取值 `ipv4_addr` / `ipv6_addr` / `ether_addr` / `inet_proto` / `inet_service` / `mark` / `ifname`（wiki《Sets》+ `nft(8)` Table 8）。
**正确做法**：两张集合 `@deny4`(`ipv4_addr`)、`@deny6`(`ipv6_addr`)，规则各写一条 `ip daddr @deny4` / `ip6 daddr @deny6`。
**注意反例**：`inet_proto` 型集合**可以**跨 L3 复用（`0001named_interval_0` 里同一 `@s3` 同时被 `ip protocol` 和 `ip6 nexthdr` 引用）——所以「跨协议复用」不是一律禁止，是**取决于集合类型**。

### A4. `counter` 与 `nft -j` JSON 形状
**写法**【文档明说】：`counter` 是 statement，挂在 verdict 前：`ip daddr @deny counter drop`（本项目规则集即如此）。`nft(8)` 的 `counter` 语句与 JSON schema 均支持；json 里匿名 counter 是 rule 的一个 expr。

**真实 JSON 片段**（取自官方测试套件随仓库提交的 `nft -j list ruleset` 输出，非我手写）：

chain（注意字段名是 `prio` 不是 `priority`）：
```json
{"chain": {"family": "inet", "table": "filter", "name": "forward", "handle": 0,
           "type": "filter", "hook": "forward", "prio": 0, "policy": "drop"}}
```
（`tests/shell/testcases/sets/dumps/0037_set_with_inet_service_0.json-nft`；schema 见 `doc/libnftables-json.adoc:251-262`）

规则里的 counter 与集合匹配（匿名集合给数组，命名集合给 `"@名字"` 字符串）：
```json
{"rule": {"family": "ip", "table": "x", "chain": "y", "handle": 0, "expr": [
  {"match": {"op": "==", "left": {"payload": {"protocol": "tcp", "field": "dport"}},
             "right": {"set": [80, 123]}}},
  {"counter": {"packets": 0, "bytes": 0}},
  {"accept": null}]}}
```
（`tests/shell/testcases/optimizations/dumps/merge_counter.json-nft`；`"!="` + 集合与 `"right": "@example"` 见 `tests/shell/testcases/listing/dumps/0022terse_0.json-nft`）

集合的 `elem`（interval 集合 + 前缀元素就是下面这个形状，前缀用 `{"prefix": {"addr","len"}}`）：
```json
{"set": {"family": "inet", "name": "s", "table": "t", "type": "ipv4_addr", "handle": 0,
         "flags": ["interval"], "elem": [{"prefix": {"addr": "192.168.0.0", "len": 24}}]}}
```
（`tests/shell/testcases/cache/dumps/0002_interval_0.json-nft`）

**别混淆**：rule 里动态写集合的 `{"set": {"op": "add", "elem": …, "set": "@name"}}` 与上面集合对象自身的 `"elem"` 数组是两个东西（schema `doc/libnftables-json.adoc:950-965` vs `337-351`）。
`metainfo.json_schema_version` 为 1（真机里 nft 1.1.1 的容器与 1.1.6 的宿主同为 1）。

**【真机实测】** 本规则集装完之后的真实 JSON 里，三处值得记下（回读代码直接照这个写）：
- **`counter` 是内联在 `rule.expr` 数组里的一项**（`{"counter": {"packets": N, "bytes": M}}`），**不是具名对象**——按名字找对象会找不到，必须遍历 `expr`。
- **空集与「集合不存在」分得开**：空集**没有 `elem` 键**（不是空数组 `[]`），集合不存在则**连 `set` 对象都没有**。判据写成「`set` 对象在 && `set.get('elem', []) == 期望成员`」，两种情况自然分开。
- 实际形状与本节引的官方 dump 逐字同构：`{"match": {"op": "==", "left": {"payload": {"protocol": "ip", "field": "daddr"}}, "right": "@deny"}}` + `counter` + `drop`；`"!="` 那条的 `op` 就是 `"!="`，`right` 是 `"@deny"` 字符串。

### A5. ★ `oifname` 不要求接口已存在；`oif` 要求 —— 约束成立
**【文档明说】** 两处独立原文：
- `nft(8)` meta 节（= upstream `doc/primary-expression.txt:22-40`）："`filter input meta iif "foo"` — Then this rule can only be added if the interface "foo" exists." 紧接着："In these cases, the name is used so **the interface doesn't have to exist to add such a rule**, it will stop matching if the interface gets renamed and it will match again in case interface gets deleted and later a new interface with the same name is created."
- 同文件类型表：`iface_index` = "Can be specified numerically or as name of an existing interface."；`ifname` = "Interface name (16 byte string). **Does not have to exist.**"

**【源码读出】** 成因在 `src/meta.c:125-150`：`ifindex_type_parse()` 对 `oif` 的字符串做 `nft_if_nametoindex()`，为 0 且不是合法数字就直接 `error(..., "Interface does not exist")`；而 `oifname` 绑定的是 `ifname_type`（16 字节字符串，无查表）。`src/meta.c:636,638` 可见 `NFT_META_OIF → ifindex_type`、`NFT_META_OIFNAME → ifname_type`。

**【实测确认（仅语法级）】** `nft -c -f` 对 `oifname "eth-nonexistent" accept` 与 `oif eth0 accept` 都**不报语法错**——但 `oif` 的存在性检查发生在求值/缓存阶段，非 root 时先被 `cache initialization failed: Operation not permitted` 挡住。

**【真机实测】已确认**（`#26`）：在**没有 eth0** 的容器 netns 里（`create` → `disconnect` → `start`，见 §B11），`nft add rule … output oifname "ethX" accept` → **exit 0**；`nft add rule … output oif ethX accept` → **exit 1，`Error: Interface does not exist`**。两半都对上了。

**结论：「装规则时 eth0 还不存在」这条约束成立**，前提是只用 `oifname`/`iifname`（不用 `oif`/`iif`）。

### A6. `nft -f <file>` = 一个事务；失败时先前规则原样保留
**【文档明说】** wiki《Atomic rule replacement》："nftables will read in all of the included config files, create the config object in memory alongside the existing config, and then in one atomic operation it swaps the old config for the new one meaning there is no moment when the firewall is partially configured." 又："The kernel handles the rule commands in the file in one single transaction"。
<https://wiki.nftables.org/wiki-nftables/index.php/Atomic_rule_replacement>

**【源码读出】用户态**：`src/libnftables.c:57-78` `nft_netlink()` 对整个 `-f` 输入只做一次 `mnl_batch_begin()` … `mnl_batch_end()`，然后才 `do_mnl_batch_talk()`。解析/求值出错时（`goto out`）**根本不会发送批**，只在本地 `mnl_batch_reset()`。

**【源码读出】内核侧**：`net/netfilter/nfnetlink.c:553-558` "We don't stop processing the batch on errors, thus, userspace gets all the errors that the batch triggers." 并置 `NFNL_BATCH_FAILURE`；`:566-593` 一旦 `status != NFNL_BATCH_DONE` 就调用 `ss->abort()` 而**不是** `ss->commit()` → 整批回滚，**先前已加载的规则原样保留**。

**两点补充**（都不影响结论，但要知道）：
- 内核是「整批收完后一次性 commit」，所以批内先 `destroy table` 再重建不会有中间窗口。
- `nft -c`（check）不是「不发批」而是「发批不带 END 标记」→ 内核走到 `abort` 分支，等于试算不落地。

**【真机实测】**（`#26`）两条探针都跑了，**并带出一条探针设计的教训**：
- 解析期错误：`ip daddr @nosuchset drop` → `Error: syntax error, unexpected @, expecting string or last`，exit 1。但它证的是「解析失败**根本不发批**」，**不是**内核原子性——`@` 后面不接集合名是语法层面的错。
- 内核期错误（才有意义）：`delete table inet nosuchtable` → `Error: Could not process rule: No such file or directory`，exit 1，且同批里先前 `add table` / `add chain` 的 `inet txprobe3` **没有建出来** → **内核侧整批回滚**。
- **反面教训**：`add chain` 同名连写两次**不报错**（exit 0，第二次只是重新声明属性），拿它当内核错误探针会得出反结论。

### A7. 幂等重置
**`destroy table` 从哪个版本起**【源码读出】nftables 提交 `e1dfd5cc`（2023-02-06，"src: add support to command \"destroy\""），**首个带它的发布是 v1.0.7**（`git tag --contains e1dfd5cc` 首项）。该提交的 `src/mnl.c` 即 `if (cmd->op == CMD_DESTROY) msg_type = NFT_MSG_DESTROYTABLE;`。

**语义：表不存在也不报错**【源码读出】内核 `net/netfilter/nf_tables_api.c:1803-1806`（`nf_tables_deltable`，DELTABLE 与 DESTROYTABLE 共用回调）：
```c
	if (IS_ERR(table)) {
		if (PTR_ERR(table) == -ENOENT &&
		    NFNL_MSG_TYPE(info->nlh->nlmsg_type) == NFT_MSG_DESTROYTABLE)
			return 0;
```
→ 返回 0，**且是在批内返回 0**，所以 `destroy table; add table; …` 可以写在同一个事务里。

**内核版本下限：Linux 6.3**【源码读出】nftables 自己的能力探测文件 `tests/shell/features/destroy.nft` 首行注释就写着 `# f80a612dd77c ("netfilter: nf_tables: add support to destroy operation")` / `# v6.3-rc1~162^2~264^2`；配套提交 `dc913f03` "tests/shell: skip destroy tests if kernel lacks support"。

**不可用时的替代**：`delete table inet x` 表不存在时返回 `-ENOENT`（同上代码的 else 分支）→ **在文件/事务里会令整批 abort**。替代写法只有两条：
1. 事务外先来一发 `nft delete table inet x 2>/dev/null || true`（代价：两次事务、有窗口）；
2. 或者用 `add table` 语义但不清理旧规则（见下）。
**`add table` 在已存在时**：不报错。`nft(8)` TABLES 节："The only difference between add and create is that the former will not return an error if the [object] exists"；`create table` 已存在则报错（`File exists`）。所以**不要**指望 `add table` 做幂等重置——它保留旧链旧规则。

**【真机实测】**（`#26`）本规则集（`add table` + `flush table` + 逐条 `add`）连装三次：每次 exit 0，且 `output` 恒 8 条、`input` 恒 3 条、4 个 set、1 张表 —— **规则没有叠倍**。⇒ 重启对账的「无条件重装」安全，不必先判在不在。

### A8. `nsenter -t <pid> -n nft -f <file>` 的坑
- **netns 只是网络视图，nft 在宿主 fs 里跑**：`nsenter -n` 只切 netns，不动 mount ns，所以 `-f` 的路径按**宿主**文件系统解析（传宿主路径即可，不要指望容器内路径）。**【源码读出/常识，未实测】**
- **需要的是目标 netns 里的 `CAP_NET_ADMIN`**，宿主 root 天然满足（rootful docker）。
- **`nft` 除 netlink 外确实读宿主文件**：`/etc/services`、`/etc/protocols`、`/etc/passwd`、`/etc/group`、`/etc/connlabel.conf`，以及 include 默认目录（`nft -h` 可查，`nft(8)` 也这么说）。**建议规则集里全用数字**（`53` 而非 `domain`，`ip daddr` 而非反查），就不用管这些文件、也不受 `-N` 影响。**【文档明说】**
- **先例**：nftables 官方测试套件自己就是用 `ip netns exec $testns $NFT …` 在别的 netns 里装规则的（`tests/shell/testcases/transactions/30s-stress:192,262`）。**【源码读出】**
- **PID 语义**：`-t` 要的是**宿主 PID 命名空间里可见的** PID（`docker inspect -f '{{.State.Pid}}'`），不是容器内 PID。容器主进程退出/重启 → netns 消失或换一个，规则随之丢失。**【推测，依据 nsenter 语义】**
- `-f -`（从 stdin 读）经 `nsenter` 传递 stdin 可用，属常规行为。**【推测】**

### A9. 内核版本依赖
| 特性 | 最低版本 | 依据 |
|---|---|---|
| `destroy` 命令 | **nft ≥ 1.0.7 且内核 ≥ 6.3** | 提交 `e1dfd5cc` + `tests/shell/features/destroy.nft` 注释（上引） |
| `inet` family | 未确证版本号；本机可用 | 本机 `CONFIG_NF_TABLES_INET=y`；wiki 有该 family 专页并明确 `inet` 的 ingress hook 是「nftables 0.9.7 / 内核 5.10」（即 family 本身远早于此）。搜索结论称 3.14，但**只有 wiki/发行版 changelog 这类转述，我没拿到内核一手提交，标未确证** |
| interval 集合（`flags interval`） | 未确证 | 官方测试与 man 都在用；未找到一手版本下限 |
| `counter` statement / `oifname` | 未确证（老到无意义） | 无版本标注 |

**对本票的实际意义**：目标机是内核 7.0.0（见 C），上表唯一会咬人的版本下限是 `destroy`（6.3），已满足。**如果将来要跑在内核 < 6.3 的宿主上，幂等重置的写法必须换掉。**

---

## B. docker 侧

### B10. 内嵌 DNS（127.0.0.11）在**容器 netns 内**，且不会被打断
**位置：容器 netns，不是宿主。**【源码读出】
- 解析器 socket 在容器 netns 里 bind：`daemon/libnetwork/sandbox_dns_unix.go:87` `sb.osSbox.InvokeFunc(sb.resolver.SetupFunc(0))`；`resolver.go:148-171` 用 `net.ListenUDP/ListenTCP(r.listenAddress, port)`，`listenAddress` = `127.0.0.11`（`sandbox_dns_unix.go:26 resolverIPSandbox = "127.0.0.11"`、`:67 NewResolver(resolverIPSandbox, …)`）。**注意端口传 0 = 内核分配临时端口**。
- DNAT/SNAT 规则也在容器 netns 里：`resolver_unix.go:21-93` 全程包在 `r.backend.ExecFunc(func(){…})` 里，而 `Sandbox.ExecFunc` 就是 `osSbox.InvokeFunc`（`sandbox_linux.go:132-139`）→ 真的在容器 netns 执行 `iptables`。规则内容：nat/OUTPUT 链 `DOCKER_OUTPUT` 里 `-d 127.0.0.11 -p udp --dport 53 -j DNAT --to-destination <127.0.0.11:临时端口>`，nat/POSTROUTING 里 `DOCKER_POSTROUTING` 做 `--to-source :53`。
- bridge 驱动**没有**任何 127.0.0.11 / DOCKER_OUTPUT 代码（`grep` 于 `daemon/libnetwork/drivers/bridge/bridge_linux.go`、`setup_bridgenetfiltering.go` 均为空）→ 排除宿主侧实现。
- 官方文档也确认地址：<https://docs.docker.com/engine/network/> "The embedded DNS server address is `127.0.0.11`."（文档只说地址，不说实现位置。）

**所以：DNS 查询在我们这套规则下照常可用**，链路是——
1. 容器进程发往 `127.0.0.11:53`，dst 是回环 → 输出路由 dev = `lo`；
2. nat OUTPUT 链优先级 `-100` **先于** filter OUTPUT 链的 `0` 执行（`nft(8)`：优先级数值小的先跑，**"regardless of the table to which they belong"**，Table 6 亦列 dstnat=-100 / filter=0）→ DNAT 把 dst 改成 `127.0.0.11:<临时端口>`，**地址仍在回环**；
3. 我们的 filter OUTPUT 看到的是 DNAT 之后的包：`oifname "lo"` 命中 → **第 2 条规则就 accept 了**，根本没走到 `ip daddr` 那几条；
4. 回程：解析器进程也在同一 netns，其响应同样经 OUTPUT（`ct state established,related` 命中），容器侧的入向包在 INPUT 走 `ct`/`iifname "lo"`；
5. `oifname` 在 OUTPUT hook 里可用且可靠**【源码读出】**：`nft_meta.c:243-250` 用 `nft_out(pkt)` = `pkt->state->out`（`include/net/netfilter/nf_tables.h:71-74`），IPv4 本地出方向传的是 `skb_dst_dev(skb)`（`net/ipv4/ip_output.c:119-121`）即已解析的输出设备。
**前置条件**：`oifname "lo" accept` 必须在 `ip daddr` 那几条**之前**——设计里正是如此。**若把 lo 规则挪到 deny/allow 之后，DNS 立刻被打断**（即使 `127.0.0.11` 不在 `@deny` 里：`ip daddr != @deny accept` 会放行，但那是"侥幸"；真正会死的是"`@deny` 里塞进 `127.0.0.0/8`"这种情况）。

**两个必须知道的附加事实**：
- **上游 DNS 转发同样受我们规则约束——但有一类上游例外。** `resolver.go:494-500` 转发到外部 DNS 时用 `r.backend.ExecFunc` 在**容器 netns 里** `net.DialTimeout`，唯一的例外是 `HostLoopback`（指宿主自己的解析器）：那种上游 docker 从**宿主 netns** 拨号。**【真机实测】**（`#26`）三组 A/B 与源码一致（每组先测无规则基线）：上游 `host(127.0.0.53)`（宿主是 systemd-resolved stub）→ 装完规则解析**照常成功、零丢包**；`--dns 192.168.50.2`（私有、非 loopback）→ 装规则后 **SERVFAIL**，`ip daddr @deny counter drop` **+6 包**；`--dns 223.5.5.5`（公网）→ 成功、零丢包。⇒ 「容器能不能解析外网域名」= 「我们的规则允不允许它出到那台 DNS」**当且仅当上游不是 loopback**；上游是 loopback 时整跳落在边界之外，会被**静默豁免**。
- **同一 netns 里的 `nft list ruleset` 不再干净**（【真机实测】）：`connect` 之后会列出 docker 的 `table ip nat`。所以回读用表级（`nft -j list table inet <我们的表>`），**别用 `list ruleset`**——这正是 `#24` 判「只看表级」的原因。
- **docker 会往容器 netns 里写 nat 表规则**（连接 user-defined 网络之后）。我们用 `inet` family 自建表，两者**不同表可共存**；但**重置时必须只 `destroy` 我们自己的表**，别碰 `filter`/`nat`。本机 docker 的 `FirewallBackend` 是 `iptables`（见 C），而系统的 `iptables` 是 `iptables v1.8.11 (nf_tables)` → docker 的规则以 nft 的 `ip nat` 等兼容表落地。若宿主改配置为 docker 的 `nftables` 后端，DNS 规则落在 `ip docker-dns` 表（`resolver_unix.go:95-141`），同样不冲突。

### B11. `--network none` 的 netns 里有 `lo`（UP）
**【源码读出】** `daemon/libnetwork/osl/namespace_linux.go:144`（`NewSandbox`）与 `:187`（`GetSandboxForExternalKey`）都调 `n.loopbackUp()`；`:276-281` 实现为 `LinkByName("lo")` + `LinkSetUp`。非 `--network host`（`useDefaultSandBox=false`）的容器都会走 `osl.NewSandbox(..., osCreate=true, ...)`（`daemon/libnetwork/controller_linux.go:84-89`）→ **独立 netns + lo UP**。
**【源码读出】** netns 在 **start** 时创建，不是 create 时：`daemon/start.go:140` `daemon.initializeNetworking(...)`；`daemon/container_operations.go:450-496` 里 `initializeNetworking` → `Controller.NewSandbox`。
→ 所以 `nsenter` 之后 netns 一定存在且 `lo` 已 up。**【真机实测】**（`#26`）：`docker create` 时 `State.Pid = 0`、`SandboxKey` 为空；`start` 之后两者都有 → **netns 确实在 `start` 时才建**。`start` 完立刻看：只有 `1: lo: <LOOPBACK,UP,LOWER_UP>`，**没有 eth0**，且 `nft list ruleset` **无输出（exit 0）**。

**⚠️ `--network none` 这条装法走不通（【真机实测】，修正见 `#22` / `#26`）**：`create --network none` 起的容器**再也 `connect` 不上**任何网络，docker 直接拒：`container cannot be connected to multiple networks with one of the networks in private (none) mode`。guard 在 `daemon/container_operations.go` 的 `updateNetworkSettings()`：`containertypes.NetworkMode(sn.Name()).IsNone() || NetworkMode(n.Name()).IsNone()`（`docker network connect` 的入口与容器启动路径都会走到），且 `integration-cli/docker_cli_run_test.go` 有一条断言它的用例 → **有意为之、有测试锁定**。运行中的、未启动的 none 容器一律拒。

**可用的时序**（已端到端跑通）：`docker create --network <共享网络>` → **`start` 之前 `docker network disconnect <该网络>`** → `start`（netns 只有 `lo`、规则集为空）→ 装规则 → `docker network connect <该网络>`。绕得过的原因：`disconnect` **不改容器的 NetworkMode**、只摘 endpoint。connect 之后实测：`eth0` 就位、docker 落了它自己的 `table ip nat`、**我们的表没被冲掉**、`/etc/resolv.conf` 变成 `nameserver 127.0.0.11`。

**⚠️ `restart` 会把规则丢掉（【真机实测】，新窗口）**：`docker restart` 后 `SandboxKey` 变了（`/var/run/docker/netns/618f…` → `d8af…`）→ **netns 是新造的**，我们的表随之消失（只剩 docker 的 `table ip nat`），而 `eth0` 在 `start` 时就回来了 ⇒ **容器是带着网络、没有规则回来的**。所以启动不变量是：**沙箱容器只在断网态被 `start`**；docker restart policy 必须 `no`，Worker 绝不 `docker restart`、也不裸 `docker start`，每次启动（含冷档重建）都走上面五步。Worker 与 docker daemon 自身重启**不动容器**，netns 与规则都在（`#19` 的收养不受影响）。

**`docker network connect` 会不会在容器 netns 内做依赖出网的动作：不会。**【源码读出】连接流程做的是 netlink 级的接口/IP/路由配置（`osl`），加 endpoint 后 `endpoint.go:551 → sb.populateNetworkResources → sandbox_linux.go:354-371` 只在 `ep.needResolver()` 为真时**启动内嵌解析器**（`sb.startResolver(false)`），后者是 bind + 写 nat 表规则，全是本地动作，无需出网。

### B12. `connect` 之前，netns 的 netfilter 是干净的
**【源码读出】** `null` 驱动**完全不碰 netfilter**：`daemon/libnetwork/drivers/null/null.go` 全文只维护一个 network id，`Join`/`CreateEndpoint` 都是 `return nil`。
**【源码读出】** 容器 netns 里唯一可能出现 docker 规则的路径是内嵌解析器的 `setupNAT`，而它只在 endpoint join（`needResolver()`）时触发 → `--network none` 期间不会发生。
→ **connect 之前 netns 内规则集应为空**（`nft list ruleset` 无输出），connect 之后才会多出 docker 的 DNS 表。**【真机实测】确认**（`#26`）：断开态的 netns 里 `nft list tables` 与 `nft list ruleset` 都无输出（exit 0）；`connect` 之后同一 netns 里出现 `table ip nat`（docker 内嵌解析器的规则）与我们的 `table inet orrery` **并存**。

---

## C. 本机（dev machine）

**【实测确认】**
- `nft --version` → `nftables v1.1.6 (Commodore Bullmoose #7)`
- `uname -r` → `7.0.0-38-generic`（Ubuntu 26.04.1 LTS，`ai-coding`）
- `docker` → Client/Server `29.8.2`（API 1.56），containerd 2.3.6，runc 1.5.1
- `docker info` → `FirewallBackend: {"Driver": "iptables"}`（**不是** docker 的 nftables 后端）；`Driver: overlayfs`，`CgroupVersion: 2`
- `iptables --version` → `iptables v1.8.11 (nf_tables)`（即 docker 的"iptables"实际经 nft 兼容层落地）
- `docker network ls` → 存在内置 `none`（DRIVER `null`，`docker network inspect none` 显示 `Driver: null`、`EnableIPv4: true`）→ `--network none` 走 null 驱动，与本票设计一致
- 内核配置：`CONFIG_NF_TABLES=m`、`CONFIG_NF_TABLES_INET=y`、`CONFIG_NFT_CT=m`、`CONFIG_NFT_NAT=m`、`CONFIG_NFT_COMPAT=m`（counter 语句属 nf_tables 核心，无独立 `CONFIG_NFT_COUNTER` 符号，`/boot/config` 里查不到）
- **非 root 实测**：`nft -c -f <file>` 会先完成解析再死在 `netlink: Error: cache initialization failed: Operation not permitted` → **语法错误可以在非 root 下查出来**（坏例：`ip daddr != { }` 报 `syntax error, unexpected '}'`；本文档 §A 各条语法结论即由此得出）。**语义/内核接受度查不了，必须 root。**

---

## D. 事实核查带出来的三条设计后果

1. ~~**IPv6 会整条掉进 `policy drop`。**~~ **已作废**——`#24` 定稿的规则集补了 `ip6 daddr @deny6 drop` / `ip6 daddr != @deny6 accept` 两条镜像，这条后果只对当时那份纯 v4 草稿成立：镜像补齐后 v6 全空间都被覆盖，落在最后一条上的**公网 v6 是放行的**，`policy drop` 只吃没被任何规则匹配的包。**【真机实测】**（`#26`）本机**测不了**：docker 网桥 `enable_ipv6=false`，容器里连 v6 地址都加不上（`Error: ipv6: IPv6 is disabled on this device`），`ping6` 死在**路由**（`Network unreachable`）而不是 netfilter。⇒ v6 半张在 v6 关着的宿主上是**死代码**（容器压根没有 v6 路径，比规则更严）；一旦共享网络上开 v6 就立刻变成承重件，`#16` 的「IPv6 私有段同待遇」正是为那一刻写的。真机验证需要一台 `enable_ipv6` 的宿主。
2. **最后一条 `ip daddr != @deny accept` 在当前次序下是恒真的**——它前面的 `ip daddr @deny drop` 已经吃掉了集合内所有包，走到这条的必然不在 `@deny` 里，写成裸 `accept` 语义完全相同（每一包少一次集合查询）。保留它的唯一价值是"自解释"。同理 INPUT 侧若要对称也得单独写。
3. **DNS 的第一跳不是风险点，上游那一跳是——但只在非 loopback 上游时。** 装规则时 netns 里只有 `lo`、docker 的 DNS 规则稍后才进同一 netns 的 nat 表——按 A/B10 的链路，`oifname "lo" accept` 排在前面，`127.0.0.11` 走回环，解析照常。**【真机实测】**（`#26`）上游那一跳：非 loopback 的私有上游（`--dns 192.168.50.2`）实测 **SERVFAIL + `@deny` 计数 +6 包**；公网上游（`--dns 223.5.5.5`）成功；loopback 上游（宿主 stub `127.0.0.53`）因 docker 改从宿主 netns 拨号而**整跳落在边界之外**。⇒ Q9 的答案（沙箱上游解析器显式指公网）必须，且恰好落在被放行的那一侧。

---

## 附录：探针清单与实测结果（`#26` 已全部跑完）

**跑法与权限**：本机没有免密 `sudo`，但登录用户在 `docker` 组里 ⇒ 凡是要 `nsenter -t <pid> -n` 的地方，改成把带 `NET_ADMIN` 的辅助容器 join 进目标 netns。同一个 netns、同一条 netlink 路径，宿主 netfilter 未被动过。

```bash
docker build -t nftprobe-tools - <<'DOCKERFILE'
FROM alpine:3.21
RUN apk add --no-cache nftables iproute2 bind-tools
DOCKERFILE
# 目标 netns 里的任何 nft / ip 命令：
#   docker run --rm --net container:<cid> --cap-add NET_ADMIN nftprobe-tools <cmd>
```

**⚠️ 起探针容器的姿势变了**（`--network none` 之后接不上网，见 §B11）：在共享网络上 `create`，`start` **之前**摘掉。

```bash
# 0) 语义检查（不改任何 netns）                                          → 实测 exit=0 ✅
nft -c -f egress.nft

# 1) 探针容器
docker network create probe-net
cid=$(docker create --network probe-net --name nftprobe alpine sleep 900)
docker network disconnect probe-net $cid      # 实测 exit=0（未启动也能摘）✅
docker start $cid
docker inspect -f '{{.State.Pid}}' $cid       # 实测非 0（create 时是 0、SandboxKey 为空）✅

# 2) B11/B12：connect 之前 netns 里有什么
$N ip -o link show        # 实测：只有 lo: <LOOPBACK,UP,LOWER_UP>，没有 eth0 ✅
$N nft list ruleset       # 实测：无输出、exit 0 ✅（没有 NET_ADMIN 则是 Operation not permitted）

# 3) 装规则 + 取真实 JSON（§A4 的实测片段）
$N nft -f /w/egress.nft                                  # 实测 exit 0 ✅
$N nft list table inet orrery                     # 实测：两条 chain 都 policy drop、四个集合齐全 ✅
$N nft -j list table inet orrery > ours.json      # 实测：读回判据全部可判（见 §A4）✅

# 4) A6：整文件一个事务、失败不留痕（要内核期错误才测得到）
printf 'add table inet txprobe2\nadd chain inet txprobe2 c\ndelete table inet nosuchtable\n' > tx-kern.nft
$N nft -f /w/tx-kern.nft      # 实测 exit 1「No such file or directory」✅
$N nft list tables            # 实测：没有 inet txprobe2 → 内核侧也整批回滚 ✅

# 5) A7：幂等重装（本设计用 add + flush，不是 destroy）
$N nft -f /w/egress.nft ; $N nft -f /w/egress.nft
$N nft -j list table inet orrery    # 实测：output 恒 8 条、input 恒 3 条，没叠倍 ✅

# 6) ★ DNS：接网之后（§B10 + Q9）
docker network connect probe-net $cid          # 实测 exit=0 ✅
docker exec $cid cat /etc/resolv.conf          # 实测 nameserver 127.0.0.11 ✅
docker exec $cid nslookup <同网络内另一容器名>   # 实测成功（内嵌解析走 lo）✅
docker exec $cid nslookup example.com          # 实测成功——但原因是上游是 loopback，见对照组
#   对照组（每组先测无规则基线）：--dns 192.168.50.2 装规则后 SERVFAIL、@deny +6 包；
#                                --dns 223.5.5.5   装规则后照常成功、零丢包
$N nft list tables    # 实测：table ip nat（docker 的）+ table inet orrery 并存 ✅

# 7) A5：oifname / oif 对照
$N nft add rule inet orrery output oifname "ethX" accept   # 实测 exit 0 ✅
$N nft add rule inet orrery output oif ethX accept         # 实测 exit 1「Interface does not exist」✅

# 8) A2 收尾：空集
nft -c -f <(printf 'table inet z {\n chain c {\n  ip daddr != { } drop\n }\n}\n')  # 实测 exit 1 syntax error ✅

# 9) D-1：IPv6   → 实测测不了：网桥 enable_ipv6=false，容器里 ip -6 addr add 报
#                  「IPv6 is disabled on this device」，ping6 死在路由而不是 netfilter ⚠️

# 10) 拆除
docker rm -f $cid ; docker network rm probe-net
```

**效果侧（比语法检查硬）**：容器直连私网网关 → 失败，紧跟其后 `ip daddr @deny counter drop` **+3 包**（3 次 SYN 重传 = 被**丢弃**而不是被拒）；直连公网 `223.5.5.5:53` → 成功、计数器零增。⇒ **规则确实在包路径上**，不只是"装进去了"。

**新窗口（`#22` 的追补）**：`docker restart` 之后 `SandboxKey` 变了、我们的表没了，而 `eth0` 在 `start` 时就回来 ⇒ **容器是带着网络、没有规则回来的**。启动不变量：**沙箱容器只在断网态被 `start`**，docker restart policy 必须 `no`，Worker 绝不 `docker restart` / 裸 `docker start`。

**判据汇总（原表 → 实测）**：#2 空规则集 → B12 成立 ✅；#3 exit 0 且 chain 里 `policy: drop` + 四集合 → 规则集形状可落地 ✅；#4 非 0 且无 `inet txprobe` → A6 成立（**内核侧**已证）✅；#5 连装不叠倍 → A7 成立 ✅；#6 内嵌解析不被打断 ✅，而上游那一跳**按上游是不是 loopback 分岔**（⚠️ 细化）；#7 的对照 → A5 成立 ✅；#8 → A2 成立 ✅；#9 → **测不了，且原判据理由作废**（最终规则集有 ip6 镜像，公网 v6 是放行的）。
