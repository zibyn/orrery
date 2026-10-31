# Node Mesh（WireGuard、一端在 NAT 后）与 Sandbox Link 规则：实测（spike / #183）

只回答 `#183`「Spike」一节的三个问题，不重新论证设计。每条结论都给出在 `xzai-a800-8` 上跑出来的输出。

**可信度**：本文所有结论都是**【真机实测】**（2026-10-31），没有源码或文档推断。

**实测环境**：Ubuntu 22.04.5，kernel `5.15.0-119-generic`（内核自带 `wireguard.ko`），containerd `2.2.1`，runc `1.5.2`，runsc `release-20261026.0`，nftables `v1.0.2`，CNI bridge plugin `v1.9.1`，`wireguard-tools v1.0.20210914`（本次 `apt install`）。镜像是 `redis:8-alpine`（目标）和 `alpine:3.22`（来源，busybox `nc`），从宿主 docker `save` 后导入 containerd namespace `orrery-spike`。

## 0. 结论（先读这里）

| # | 问题 | 结论 | 要点 |
|---|---|---|---|
| 1 | 两台 Node 经 WireGuard 互相路由 Sandbox 子网，一台在 NAT 后、只有另一台有 `--mesh-endpoint` | **是，但 CNI 的 `ipMasq` 必须换掉** | NAT 侧主动握手并以 keepalive 保活后，两个方向都通，包括从公网侧连进 NAT 侧的 Sandbox。不经中继。CNI `ipMasq: true` 会把跨 Node 流量 SNAT 成 Node 地址，WireGuard 按 AllowedIPs 丢掉；改为 `ipMasq: false`，由 Agent 自己做 `oifname != "wg0"` 的 masquerade（§2）。 |
| 2 | 目标 veth 上按 `oifname` 的入站规则只放行被链接的来源，本机 bridge 和 `wg` 两路，runc 和 runsc | **是，但要挂两个钩子并排在最前** | 同一 bridge 上来的流量走 `forward` 钩子，从 `wg0` 路由进来的走 `output` 钩子，两处都要按 `oifname` 跳进目标的链。`forward` 里目标的跳转必须排在来源 `iifname` 的跳转之前，否则来源的 `accept` 会跳过目标检查（§3.3）。 |
| 3 | 任一侧 stop/start 换了地址后，规则重新渲染 | **是** | 整表重渲染后，新地址立即可达，旧地址不可达。重渲染之前，新来源的地址被目标拒绝，是 fail-closed（§4）。 |

负面结论有两条，都要求修改 `docs/spec/runtime.md` §13 和 `internal/agent/cni.go`：§2 的 masquerade 和 §3.3 的规则顺序。

## 1. 拓扑（全部在 netns 里，不碰宿主网络）

用三个 netns 模拟三台机器，每个「Node」netns 里各有自己的 bridge、Sandbox netns、`wg0` 和 nftables 表：

```
        nodeA (198.51.100.10, 有 endpoint :51820)         rt (路由器)                         nodeB (192.168.50.10, NAT 后)
 T1 runc  ─┐                                       r-a 198.51.100.1                                         ┌─ T2 runsc (redis)
 S1 runc  ─┤ orrery0 10.88.1.0/24 ── wg0 ── a-up ─────────────── rt ── r-b 192.168.50.1 ── b-up ── wg0 ── orrery0 10.88.2.0/24 ├─ S2 runsc
 U1 runc  ─┘                                       masquerade 192.168.50.0/24 → r-a                         ├─ S3 runc
                                                   新连接 r-a → r-b 一律 drop（模拟 NAT 挡入站）            └─ U2 runc
```

- **T1、T2** 是目标（`redis-server`）。**S1、S2、S3** 是被链接的来源，Egress Mode 为 `none`。**U1、U2** 是没有链接的 Sandbox，Egress Mode 为 `open`。
- Sandbox 网络用 CNI `loopback` 和 `bridge` 直接建，参数和 `internal/agent/cni.go` 的 `bridgeConf()` 相同（`isGateway`、host-local `ranges`、默认路由），只有 masquerade 的设置不同，见 §2。
- WireGuard：
  - nodeA 设 `listen-port 51820`，peer B 的 `allowed-ips 10.88.2.0/24`，不设 endpoint。
  - nodeB 不设 listen-port，peer A 的 `endpoint 198.51.100.10:51820`，`persistent-keepalive 5`，`allowed-ips 10.88.1.0/24`。
  - 两边都只有 `ip route add <对端子网> dev wg0`，`wg0` 上**没有地址**。

## 2. Mesh 打通，以及 CNI `ipMasq` 的问题（Q1）

### 2.1 NAT 侧发起，公网侧学到映射后的端点

```
nodeA wg0 peer:  endpoint: 198.51.100.1:35619   latest handshake: 1 minute, 8 seconds ago   transfer: 3.93 KiB received, 3.03 KiB sent
nodeB wg0 peer:  endpoint: 198.51.100.10:51820  latest handshake: 1 minute, 8 seconds ago   transfer: 3.03 KiB received, 4.21 KiB sent
```

nodeA 上的端点是路由器 masquerade 之后的地址和端口，说明 NAT 生效。路由器上「`r-a` → `r-b` 的新连接」计数为 0，说明公网侧从来没有主动连进 NAT 侧，一直走的是 NAT 侧建立的那条映射。`wg show wg0 latest-handshakes` 可以直接拿来做连通矩阵的上报来源。

### 2.2 CNI `ipMasq: true` 让跨 Node 流量不通

照搬 Agent 现在的 `"ipMasq": true, "ipMasqBackend": "nftables"`，S2（nodeB）→ T1（nodeA）超时。在两侧 `wg0` 上抓包：

```
nodeB orrery0: IP 10.88.2.2.29102 > 10.88.1.2.6379: Flags [S]
nodeB wg0:     IP 192.168.50.10.29102 > 10.88.1.2.6379: Flags [S]
nodeA wg0:     0 packets captured
```

CNI 的规则是 `ip saddr 10.88.2.2 ip daddr != 10.88.2.0/24 masquerade`，于是发往别的 Node 的 Sandbox 子网也被 SNAT 成了 Node 地址。WireGuard 的 cryptokey routing 在接收端检查内层源地址，`192.168.50.10` 不在 `allowed-ips 10.88.2.0/24` 里，包被丢掉。即使放宽 AllowedIPs，来源地址也已经丢失，目标的 Link 规则无法按来源匹配。

想在更早的 nat 链里 `accept` 来绕过它，结果无效：

```
chain nat_skip { type nat hook postrouting priority srcnat - 10; ip saddr 10.88.0.0/16 ip daddr 10.88.0.0/16 counter packets 1 bytes 60 accept }
→ S2 -> T1 仍超时，wg0 上的源地址仍是 192.168.50.10
```

nftables 里，一个 base chain 的 `accept` 只结束这条链，同一钩子上的其他 base chain 照样执行，所以 CNI 的 masquerade 还是会生效。

### 2.3 改为 Agent 自己做 masquerade

CNI 设 `"ipMasq": false`，Agent 在自己的表里写：

```
table ip orrery_nat {
  chain nat_post {
    type nat hook postrouting priority srcnat; policy accept;
    ip saddr 10.88.2.0/24 ip daddr != 10.88.2.0/24 oifname != "wg0" counter masquerade
  }
}
```

结果：

```
S2(B,runsc) -> T1(A,runc) cross-node                    expect PONG  got PONG
S1(A,runc) -> T2(B,runsc) into NAT side                 expect PONG  got PONG
U1(A,runc) -> 198.51.100.1 via masquerade (ping)        expect PONG  got PONG
```

跨 Node 保留原始来源地址，访问外部的流量照常 masquerade。

## 3. Link 入站规则（Q2）

### 3.1 规则形状

每个 Node 一张 `bridge` family 的表，整表渲染。下面是 nodeA 上的实际规则和计数，即 phase 1 结束时的状态：

```
chain hk_fwd {   # hook forward
    iifname "veth2129a01b" counter packets 11 bytes 522 jump eg_S1     # 来源 S1
    oifname "veth217dd24b" counter packets 5 bytes 264 jump in_T1      # 目标 T1
}
chain hk_inp {   # hook input
    iifname "veth2129a01b" counter packets 12 bytes 618 jump eg_S1
}
chain hk_out {   # hook output
    oifname "veth217dd24b" counter packets 11 bytes 634 jump in_T1
}
chain eg_S1 {    # Egress Mode none，Link 例外排在前面
    ct state established,related accept; ether type arp accept; ether type != ip drop
    ip daddr 10.88.1.2 tcp dport 6379 counter packets 1 bytes 60 accept
    ip daddr 10.88.2.2 tcp dport 6379 counter packets 1 bytes 60 accept
    counter packets 3 bytes 180 drop
}
chain in_T1 {    # 只放行被链接的来源
    ct state established,related accept; ether type arp accept; ether type != ip drop
    ip saddr 10.88.1.3 tcp dport 6379 counter packets 0 bytes 0 accept
    ip saddr 10.88.2.3 tcp dport 6379 counter packets 1 bytes 60 accept
    counter packets 9 bytes 540 drop
}
```

### 3.2 结果矩阵

```
S1(A,runc,none) -> T1(A,runc) same node, linked         expect PONG  got PONG
U1(A,runc,open) -> T1 same node, unlinked               expect FAIL  got FAIL(timeout)
S2(B,runsc,none) -> T1(A) cross-node, linked            expect PONG  got PONG
U2(B,runc,open) -> T1(A) cross-node, unlinked           expect FAIL  got FAIL(timeout)
S1(A,runc,none) -> T2(B,runsc) into NAT side, linked    expect PONG  got PONG
S3(B,runc,none) -> T2(B,runsc) same node, linked        expect PONG  got PONG
U1(A,runc,open) -> T2(B) cross-node, unlinked           expect FAIL  got FAIL(timeout)
S2(B,runsc,none) -> T2(B) unlinked pair, mode none      expect FAIL  got FAIL(timeout)
S1(A,none) -> U-less dest 198.51.100.1:22               expect FAIL  got FAIL(timeout)
nodeA host -> T1 (gateway addr)                         expect FAIL  got FAIL
nodeB host -> T1 over wg (no wg addr)                   expect FAIL  got FAIL
```

runc 和 runsc 分别作为来源和目标，在同 Node 和跨 Node 两种情况下都符合预期。Node 自己的进程（bridge 网关地址，或者经 `wg0` 过来的）也连不进目标。

### 3.3 钩子：哪路流量走哪个钩子，以及顺序

- **同一 bridge 上来源 → 目标**：二层转发，走 `forward`。来源的 `iifname` 和目标的 `oifname` 都在这一条链里匹配。
- **从 `wg0` 路由进来 → 目标**：由 bridge 设备本身发出，走 `output`，不走 `forward`。计数为证：`hk_out` 里 `in_T1` 的跳转有 11 个包，`in_T1` 里放行 S2（10.88.2.3）的计数为 1。只挂 `forward` 会漏掉全部跨 Node 流量。
- **来源 → 别的 Node 上的目标**：发往网关 MAC，走 `input`，和 ADR-0026 现有的出网过滤路径相同。

**顺序是负面结论**：`accept` 会结束整条 base chain。在 `forward` 里，如果来源的 `iifname` 跳转排在目标的 `oifname` 跳转之前，来源链里的 `accept` 会让目标的检查根本不执行。用一个 Egress Mode 为 `open` 的 U1 实测（它的链里是 `accept`，和 Agent 现在给 `open` 渲染的规则一样）：

```
U1(open chain, egress jump BEFORE target jump) -> T1: PONG      ← 未链接的来源连进了目标
U1(open chain, egress jump AFTER target jump) -> T1:  FAIL(timeout)
```

所以 `forward` 里所有目标的 `oifname` 跳转要排在最前面。顺带的效果是：被链接的一对之间的流量先由目标链 `accept`，来源的 Egress Policy 不再评估。这正好就是 #183 定的「Link 不受 Egress Mode 和 Egress Rules 约束」，不需要另写例外。前提是来源一侧的 `input` 链（跨 Node 的情况）仍然要在 Egress Policy 之前放行 Link 目标，§3.1 的 `eg_S1` 就是这样排的。

## 4. 地址变化后重新渲染（Q3）

```
T1 old=10.88.1.2 new=10.88.1.5
S1 -> T1 new addr                                       expect PONG  got PONG
S2 -> T1 new addr (cross-node)                          expect PONG  got PONG
S1 -> T1 old addr                                       expect FAIL  got FAIL(timeout)
U1 -> T1 new addr                                       expect FAIL  got FAIL(timeout)

S2 old=10.88.2.3 new=10.88.2.6          （runsc 来源，netns 重建）
S2 new addr before re-render                            expect FAIL  got FAIL(timeout)
S2 new addr after re-render                             expect PONG  got PONG
```

host-local 在 DEL 之后再 ADD 会分配新地址，所以 #183 里「每次 start 地址都会变」成立。重新渲染之前，目标只认旧地址，这是 fail-closed。

## 5. 没有测的（实现前要知道）

- **真实 NAT**：路由器是 Linux masquerade，尽量保留端口，属于对 WireGuard 友好的「easy NAT」。云厂商的 NAT 网关和运营商级 NAT 没有测。两端都在 NAT 后的情况没有测；按 #183，那种情况本来就不放置。
- **MTU**：`wg0` 默认 1420，veth 是 1500。redis 的 `PING` 包很小，大包和 PMTU 发现（尤其是 runsc 的 netstack 怎么处理 ICMP frag-needed）没有验证。实现时要么把 Sandbox veth 的 MTU 降到 1420，要么在 `wg0` 上做 MSS clamp。这一点需要单独测。
- **性能、长时间连接、keepalive 间隔对 NAT 映射超时的影响**：都没有测。
- 只测了 kernel 5.15、IPv4、TCP。

## 6. `xzai-a800-8` 上留下了什么

- `wireguard-tools`（apt），`wireguard` 内核模块保持加载。
- 其余都已清理：netns `rt`、`nodeA`、`nodeB`、`sb-*`，namespace `orrery-spike` 里的容器和本次导入的两个镜像，以及 `/root/orrery-spike` 脚本目录。宿主 docker 和它的 nftables 表没有改动。
