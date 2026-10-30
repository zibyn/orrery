# containerd 2.x 的 CDI、runsc 的 cgroup 回读、runsc + CNI netns：三条前提的实测（spike #161 / spec #160）

只回答 `#161` 的三个问题，不重新论证 ADR-0026 的设计。每条结论都给出处与在 `xzai-a800-8` 上跑出来的输出。

**可信度分档**：
- **【源码读出】** 源码文件 + 行号（containerd `v2.2.1`，tag 对象 `b4360bae`，提交 `dea7da592f5d`；gVisor `release-20261026.0`）。
- **【文档明说】** 一手文档或二进制自带的 `--help` 原文。
- **【真机实测】** 在 `xzai-a800-8` 上实际跑出来的输出（2026-10-30）。

**实测环境**：Ubuntu 22.04.5，kernel `5.15.0-119-generic`，cgroup v2（`stat -fc %T /sys/fs/cgroup` = `cgroup2fs`），containerd `2.2.1`，runc `1.5.2`，nftables `v1.0.2`，nvidia-ctk `1.19.1`，驱动 `590.48.01`，8× A800-SXM4-80GB（`nvidia-smi -L`，不是 2 张），CDI spec `/var/run/cdi/nvidia.yaml`。客户端是一个约 70 行的一次性 Go 程序，依赖 `github.com/containerd/containerd/v2 v2.2.1`（间接引入 `tags.cncf.io/container-device-interface v1.1.0`），用 containerd 原生 gRPC 在自建 namespace `orrery-spike` 里建容器。

## 0. 结论（先读这里）

| # | 问题 | 结论 | 机制 |
|---|---|---|---|
| 1 | containerd 2.x Go 客户端怎么注入 CDI 设备；runc 容器能否这样看到 GPU | **是** | `github.com/containerd/containerd/v2/pkg/cdi.WithCDIDevices(devices ...string) oci.SpecOpts`，内部调 `cdi.Refresh()` + `cdi.InjectDevices(spec, …)`。**不是** `oci.WithCDIDevices`，`pkg/oci` 里没有这个函数。runc 容器只看到被请求的那一张卡，其余卡的设备节点被设备 cgroup 挡住（`EPERM`）。 |
| 2 | runsc 下 Sandbox 的 CPU、内存、PID 上限能否在宿主上回读 | **是**（PID 有语义差异，见 §2.3） | runsc 按 OCI spec 的 `linux.cgroupsPath` 建宿主 cgroup（containerd 默认 `/<namespace>/<id>`），把 `runsc-sandbox`、`runsc-gofer` 和 systrap stub 进程放进去，写入 `cpu.max`、`memory.max`、`pids.max`。回读路径 `/sys/fs/cgroup/<cgroupsPath>/{cpu.max,memory.max,pids.max}`，与 runc 相同。 |
| 3a | runsc 能否用调用方建好的 CNI bridge netns（spec 里给 netns 路径，`--network=sandbox`） | **是** | runsc 从该 netns 抓走接口、地址和路由，复制进 Sentry 网络栈，再**把地址和路由从宿主侧 netns 里删掉**。客户机里 `eth0` 拿到 CNI 分配的地址，宿主经 bridge 可以访问它。 |
| 3b | 宿主进程 setns 进记录下来的 netns 路径，能否访问 Sandbox 里的端口（Tunnel 的做法） | **否（runsc 下）**；runc 下是 | runsc 启动后这个 netns 里只剩 `lo` 和一个没有地址的 `eth0`，没有路由（`Network is unreachable`）。客户机的 `127.0.0.1` 在 Sentry 网络栈里，宿主内核的 netns 碰不到。可行的路径是：仓库现有的 Supervisor 转发器走运行目录挂载上的 UDS（runsc 需要 `host-uds=create`），或者 `runsc port-forward --stream`（§3.3）。 |
| 3c | 宿主侧 veth 上的 nftables 能否过滤 runsc Sandbox 的出网 | **是** | runsc 的包照常走 veth 出去。用 `bridge` family 表、`iifname "<host veth>"` 匹配，`input` 钩子管发往宿主的流量，`forward` 钩子管 Sandbox 之间的流量；`ct state established,related` 在 5.15 的 bridge family 上可用。 |

**负面结论只有 3b**，已作为评论发在 `#160` 上，写明它要求 ADR-0026 改什么。

## 1. CDI 注入（Q1）

### 1.1 API

**【源码读出】** `pkg/cdi/oci_opt.go:31`（<https://github.com/containerd/containerd/blob/v2.2.1/pkg/cdi/oci_opt.go>）：

```go
// WithCDIDevices injects the requested CDI devices into the OCI specification.
func WithCDIDevices(devices ...string) oci.SpecOpts {
	return func(ctx context.Context, _ oci.Client, c *containers.Container, s *oci.Spec) error {
		...
		if err := cdi.Refresh(); err != nil {
			// We don't consider registry refresh failure a fatal error.
			...
		if _, err := cdi.InjectDevices(s, devices...); err != nil {
			return fmt.Errorf("CDI device injection failed: %w", err)
		}
		// One crucial thing to keep in mind is that CDI device injection
		// might add OCI Spec environment variables, hooks, and mounts as
		// well. Therefore it is important that none of the corresponding
		// OCI Spec fields are reset up in the call stack once we return.
```

这里的 `cdi` 是 `tags.cncf.io/container-device-interface/pkg/cdi`（go.mod 第 90 行，`v1.1.0`）。它用默认 registry，spec 目录是 CDI 库的默认值（`/etc/cdi`、`/var/run/cdi`）。

**`ctr` 自己的用法，也是 Agent 应该照抄的**（`cmd/ctr/commands/run/run_unix.go:367-378`、`:495-505`，<https://github.com/containerd/containerd/blob/v2.2.1/cmd/ctr/commands/run/run_unix.go>）：用 `parser.IsQualifiedName(dev)` 区分 CDI 名和普通设备路径；有 CDI 名时先加一个 `withStaticCDIRegistry()`，它调 `cdi.Configure(cdi.WithAutoRefresh(false))`，**关掉 registry 的 inotify 自动刷新**，然后再 `cdispec.WithCDIDevices(cdiDeviceIDs...)`。CRI 插件则在启动时调一次 `cdi.Configure(cdi.WithSpecDirs(c.config.CDISpecDirs...))`（`internal/cri/server/service_linux.go:105`）。顺序上要注意（来自上面源码里那句注释）：`WithCDIDevices` 会往 spec 里加 env、hook 和 mount，所以它必须排在**会重置这些字段的 SpecOpts 之后**。

### 1.2 实测：runc + `nvidia.com/gpu=1`

**【真机实测】** 用 `cdi.WithCDIDevices("nvidia.com/gpu=1")`，镜像 `nvidia/cuda:12.4.1-base-ubuntu22.04`（从 docker `save` 后 `ctr -n orrery-spike images import` 进来），命令 `nvidia-smi -L`：

```
devices: ['/dev/nvidia-modeset', '/dev/nvidia-uvm', '/dev/nvidia-uvm-tools', '/dev/nvidiactl', '/dev/nvidia1', '/dev/dri/card2', '/dev/dri/renderD129']
hooks: [['nvidia-cdi-hook', 'create-symlinks', '--link'], ..., ['nvidia-cdi-hook', 'enable-cuda-compat', '--host-driver-version=590.48.01'], ['nvidia-cdi-hook', 'update-ldcache', '--folder'], ['nvidia-cdi-hook', 'disable-device-node-modification'], ...]
GPU 0: NVIDIA A800-SXM4-80GB (UUID: GPU-2619d375-d5a3-09f2-b92f-bd293b17d3c5)
```

容器里看到的 UUID 正是宿主上的 GPU 1。也就是说，`nvidia-smi` 和驱动库都是 CDI spec 从宿主挂进来的，镜像里不需要自带。

隔离检查（同一个设备，命令 `ls /dev/nvidia*; mknod /tmp/n0 c 195 0 && head -c1 /tmp/n0`）：

```
/dev/nvidia-modeset  /dev/nvidia-uvm  /dev/nvidia-uvm-tools  /dev/nvidia1  /dev/nvidiactl
head: cannot open '/tmp/n0' for reading: Operation not permitted
```

`/dev/nvidia0` 不存在；自己 `mknod` 出 GPU 0 的节点（`195:0`）也打不开，被设备 cgroup 挡住。ADR-0009 要求的「回读设备节点」可以直接用 `spec.Linux.Devices`，或者在容器里 `ls /dev/nvidia*`。

### 1.3 顺带：gvisor Node 必须自己拒绝 CDI 请求

**【文档明说】** `runsc flags` 原文："LEGACY: enable support for Nvidia GPUs. GPU support gets automatically enabled if Nvidia devices are present in the OCI spec." 意思是 OCI spec 里一旦有 NVIDIA 设备，runsc 会**自动开启 nvproxy**，不会报错拒绝。所以「gvisor Node 上 GPU 为 `unsupported`」只能由 Agent 在 Create 之前拒绝请求，不能指望 runsc 失败。gvisor 下的 CDI 没有实测（属于 #160 的范围外）。

## 2. runsc 下的 cgroup 回读（Q2）

### 2.1 机制

**【源码读出】** containerd 默认 spec 把 `CgroupsPath` 设为 `filepath.Join("/", ns, id)`（`pkg/oci/spec.go:209`）。runsc 按这个路径建 cgroup v2：`runsc/cgroup/cgroup.go:408 NewFromPath`、`runsc/cgroup/cgroup_v2.go:132 (*cgroupV2).Install(res *specs.LinuxResources)`，写入 `cpu.max`、`memory.max`（`cgroup_v2.go:45-46`）和 `pids.max`（`cgroup_v2.go:688`）。只有 runsc 配置里 `systemd-cgroup = "true"` 时才走 systemd 路径（`pkg/shim/v1/runsc/container.go:214`）。shim 本身不在这个 cgroup 里，留在 `containerd.service`。

### 2.2 实测：三项上限的回读

**【真机实测】** `oci.WithCPUCFS(150000, 100000)`、`oci.WithMemoryLimit(268435456)`、`oci.WithPidsLimit(64)`，运行时 `io.containerd.runsc.v1`，命令 `sleep 600`：

```
spec: {"cgroupsPath":"/orrery-spike/lim-runsc", "resources":{"memory":{"limit":268435456},"cpu":{"quota":150000,"period":100000},"pids":{"limit":64}}}
/proc/<task pid>/cgroup: 0::/orrery-spike/lim-runsc
/sys/fs/cgroup/orrery-spike/lim-runsc/cpu.max:    150000 100000
/sys/fs/cgroup/orrery-spike/lim-runsc/memory.max: 268435456
/sys/fs/cgroup/orrery-spike/lim-runsc/pids.max:   64
cgroup.procs: runsc-gofer, runsc-sandbox (= task.Pid()), 以及 3 个 systrap stub 进程
shim: /proc/<shim pid>/cgroup = 0::/system.slice/containerd.service
```

`task.Pid()` 返回的是 `runsc-sandbox` 的宿主 PID。runc 和 runsc 可以共用同一个回读函数：读 `/proc/<task.Pid()>/cgroup`，或者直接读 `/sys/fs/cgroup/<spec.Linux.CgroupsPath>/`。

### 2.3 PID 上限的语义和 runc 不同（不是负面结论，但实现要知道）

**【真机实测】** 同一个上限 `pids.max=512`，在客户机里起 200 个 `sleep`：

```
runc  idle pids.current=1    after 200 sleeps pids.current=201  events=max 0
runsc idle pids.current=51   after 200 sleeps pids.current=490  events=max 0
```

再用 `pids.max=64` 做 200 个 `sleep`：宿主 `pids.events` 变成 `max 1`，**整个 Sandbox 退出**（`urpc method "containerManager.WaitPID" failed: EOF`，task 变为 `STOPPED`），而不是客户机里的 `fork()` 返回 `EAGAIN`。

所以在 gvisor 上，`pids.max` 限制的是**宿主任务数**，包括 Sentry 的线程（空闲时约 20–51 个）和每个客户机线程对应的 systrap stub，不是客户机进程数 1:1。回读仍然准确，上限也仍然是一道边界（总数超不过去），但：① 同一个数值在 gvisor 上能容纳的客户机进程更少；② 撞到上限时整个 Sandbox 死掉。是否要在 gvisor Node 上给 PID 请求加一个固定开销，或者把这个差异写进 `runtime.md`，留给 docs PR 决定。内存只做了粗测：`memory.max=256MiB` 时，在 runc 和 runsc 下各读 600 MiB 的 `/dev/zero`，`memory.events` 的 `max` 计数都在增长（2257 / 2360），没有 OOM kill，Sandbox 都还活着。没有进一步深挖。

## 3. runsc + CNI netns，Tunnel 和出网过滤（Q3）

### 3.1 搭网络（不碰 docker 的任何东西）

**【真机实测】** 网桥 `orrspk0`，子网 `10.213.0.0/24`（先用 `ip route` 确认没有冲突），直接调 CNI 插件二进制（libcni 做的也是这件事）：

```sh
cat > net.json <<EOF
{"cniVersion":"1.0.0","name":"orrery-spike","type":"bridge","bridge":"orrspk0","isGateway":true,"ipMasq":false,
 "ipam":{"type":"host-local","subnet":"10.213.0.0/24","routes":[{"dst":"0.0.0.0/0"}],"dataDir":"/run/orrery-spike/ipam"}}
EOF
ip netns add orrery-spike-a
CNI_COMMAND=ADD CNI_CONTAINERID=orrery-spike-a CNI_NETNS=/var/run/netns/orrery-spike-a CNI_PATH=/opt/cni/bin CNI_IFNAME=lo   /opt/cni/bin/loopback < lo.json
CNI_COMMAND=ADD CNI_CONTAINERID=orrery-spike-a CNI_NETNS=/var/run/netns/orrery-spike-a CNI_PATH=/opt/cni/bin CNI_IFNAME=eth0 /opt/cni/bin/bridge   < net.json
# -> interfaces ['orrspk0', 'veth388bb06a', 'eth0'], ips ['10.213.0.4/24']
```

`ipMasq:false` 时 bridge 插件不碰 iptables（之后 `iptables-save | grep -ci cni` = 0）。宿主 `ip filter FORWARD` 的策略是 docker 设的 `policy drop`，所以这个网桥**出不了宿主**。本 spike 只测了发往网关 `10.213.0.1` 和 Sandbox 之间的流量；出公网需要 masquerade 和 FORWARD 放行，那会改动 docker 的表，这台共享机上不做。

把 netns 交给容器：`oci.WithLinuxNamespace(specs.LinuxNamespace{Type: specs.NetworkNamespace, Path: "/var/run/netns/orrery-spike-a"})`（`pkg/oci/spec_opts.go:341`）。runsc 的 `--network` 默认就是 `sandbox`（`runsc flags`："specifies which network to use: sandbox (default), host, none"），不需要额外配置。

### 3.2 runsc 接管 netns（3a = 是）

**【源码读出】** `runsc/sandbox/network.go:431-434`："createInterfacesAndRoutesFromNS scrapes the interface and routes from the net namespace with the given path, creates them in the sandbox, and **removes them from the host**."；`:494-502` 对每条 FD-based link 调 `removeLinkAddresses`（只有 network-proxy 模式下主接口才会保留地址）。

**【真机实测】** runsc 启动后：

```
# 宿主视角
$ ip -n orrery-spike-a -br addr
lo               UNKNOWN        127.0.0.1/8 ::1/128
eth0@if1063      UP                                   <- 地址没了
$ ip -n orrery-spike-a route                          <- 空
# 客户机视角（ctr task exec）
    inet 10.213.0.4/24 scope global dynamic eth0
default via 10.213.0.1 dev eth0 scope link
[   0.000000] Starting gVisor...
```

两点后果：① runsc 启动后这个 netns 不能再复用。Stop 时要按 ADR-0026 拆掉 netns 并做 CNI DEL，Create 时重建，这和 #160 的设计一致。② 第一次实验里容器很快退出了，netns 仍然没有地址，证明删地址发生在启动时，而不是退出时。

### 3.3 setns 进 netns 访问端口（3b = runsc 下否）

**【真机实测】** 两个 Sandbox 都跑 `while true; do echo hello-from-$X | nc -l -p 8080; done`：runsc 在 netns a（`10.213.0.4`），runc 在 netns b（`10.213.0.3`）：

```
host->runsc 10.213.0.4             -> hello-from-runsc   rc=0   (经 bridge)
setns(a)->10.213.0.4               ->                    rc=1
setns(a)->127.0.0.1                ->                    rc=1
nsenter --net=…/orrery-spike-a ip route get 10.213.0.4 -> RTNETLINK answers: Network is unreachable
host->runc 10.213.0.3              -> hello-from-runc    rc=0
setns(b)->127.0.0.1                -> hello-from-runc    rc=0
setns(b)->10.213.0.3               -> hello-from-runc    rc=0
```

在 runc 下，`setns` 后 dial `127.0.0.1:PORT` 可行（ADR-0017 描述的就是这条路）。在 runsc 下不可行：客户机的回环和 socket 都在 Sentry 里。走 bridge IP 也不等价：只绑在 `127.0.0.1` 的服务在 bridge 上访问不到（下面的 `8081`）。

**runsc 下实测可行的两条路：**

(1) **`runsc port-forward`**（【文档明说】`runsc help port-forward`："Stream mode forwards a single connection on a UDS to the specified port in the container"）。客户机再开一个只绑回环的监听 `nc -l -s 127.0.0.1 -p 8081`：

```
R="runsc --root=/run/containerd/runsc/orrery-spike"
$R port-forward net-runsc 28080:8080   ->  nc 127.0.0.1 28080 = hello-from-runsc
$R port-forward net-runsc 28081:8081   ->  nc 127.0.0.1 28081 = hello-loopback-only
nc 10.213.0.4 8081 (bridge)            ->  rc=1
nc -lU u.sock & ; $R port-forward --stream u.sock net-runsc 8081  ->  u.sock 收到 hello-loopback-only
```

local 模式会监听 **`*:PORT`**（`ss` 显示 `*:28080`，即所有接口），不能在生产上用；stream 模式是 runsc 去连调用方给的 UDS。`--root` 是 shim 按 namespace 给的：`/run/containerd/runsc/<namespace>`。这条路是 runsc 专有的，Agent 需要按 Isolation Runtime 分支。

(2) **仓库现有做法：Supervisor 转发器 + 运行目录上的 UDS。** 今天的 `internal/agent/tunnel.go` 本来就**不 setns**："The Supervisor stays in the Sandbox's network namespace, so the Agent never needs netns privileges"（`forwardSandboxPort`），它在 Sandbox 里执行 `supervisor forward PORT /.orrery/run/ports/PORT.sock`（`internal/supervisor/forward.go:35`），再由 Agent 连这个 UDS。PTY 控制 socket 也一样，是 Sandbox 里创建的 UDS（`internal/supervisor/run.go:112`）。在 runsc 下：

**【文档明说】** `runsc flags`："-host-uds value  controls permission to access host Unix-domain sockets. Values: none|open|create|all, default: none"。

**【源码读出】** 不改 containerd 配置也能逐容器传 runsc flag：shim 从 runtime options 里解出 `runtimeoptions.Options.ConfigPath`，再把这个 TOML 解码进 `Options`（`pkg/shim/v1/runsc/container.go:96-108`），其中 `RunscConfig map[string]string \`toml:"runsc_config"\``（`pkg/shim/v1/runsc/options.go:91-92`）就是全部 runsc flag。Go 客户端写法：`containerd.WithRuntime("io.containerd.runsc.v1", &runtimeoptions.Options{TypeUrl: "io.containerd.runsc.v1.options", ConfigPath: p})`；`ctr` 写法：`--runtime-config-path`。

**【真机实测】** 静态 Go 程序在客户机里 `net.Listen("unix", "/s/<v>.sock")`，`/s` 是宿主 ext4 目录的 rw bind 挂载：

```
default (host-uds=none):  guest ls: default.sock | host ls: (空)         | host connect: No such file or directory
host-uds=create:          guest ls: hostuds.sock | host ls: hostuds.sock | host connect: hi-uds
# runsc-hostuds.toml:
[runsc_config]
  host-uds = "create"
```

所以现有转发器和 PTY 控制 socket 在 gvisor 上能用，**前提是** Agent 为每个 gvisor 容器传 `host-uds=create`。代价是：客户机可以在它所有 host-backed 的挂载（Workspace、运行目录）里创建宿主可见的 UDS；`open` 方向（客户机连宿主 socket）仍然关闭。

### 3.4 宿主侧 veth 上的出网过滤（3c = 是）

`inet` family 不行：发往宿主或经网桥转发的帧，在没有 `br_netfilter` 时，`inet` 钩子看到的 `iifname` 是网桥 `orrspk0`，不是 veth。这台机器上 `br_netfilter` 没加载，也不应该加载，否则会改变 docker 容器的过滤行为。所以用 `bridge` family，这样还能按 veth 名字精确匹配，而不靠 Sandbox 可以伪造的源地址。

**【真机实测】** 宿主在 `10.213.0.1` 上开两个 `python3 -m http.server`，端口 28001（放行）和 28002（拒绝）：

```
table bridge orrery_spike {
	chain sbx_forward {
		type filter hook forward priority filter; policy accept;
		iifname "veth388bb06a" ct state established,related counter accept
		iifname "veth388bb06a" ip daddr 10.213.0.3 counter drop
	}
	chain sbx_input {
		type filter hook input priority filter; policy accept;
		iifname "veth388bb06a" ct state established,related counter accept
		iifname "veth388bb06a" tcp dport 28002 counter drop
	}
}
```

（注意：`fwd`、`in` 在 nft 1.0.2 里是关键字，不能当 chain 名。）

```
                                  无规则               有规则（只挂在 runsc 的 veth 上）
runsc -> 10.213.0.1:28001         port-28001           port-28001
runsc -> 10.213.0.1:28002         port-28002           wget: download timed out
runsc -> runc 10.213.0.3:8080     hello-from-runc      (无输出，被丢弃)
runc  -> runsc 10.213.0.4:8080    hello-from-runsc     hello-from-runsc   <- 回包走 ct established
counters: established 4 pkts / drop 3 pkts (forward);  established 4 pkts / drop 2 pkts (input)
```

runsc 的 netstack 发出的包和 runc 一样过宿主 veth，过滤和 Isolation Runtime 无关。由 runc 发起、发往 runsc Sandbox 的连接，回包被 `ct state established` 放行，说明 5.15 上 bridge family 的 conntrack 能用。IPv6、出公网和 netdev `ingress` 钩子都没测。

## 4. 其他发现（后续实现要知道）

1. **gVisor shim 不支持 `cio.LogFile`（`file://` stdio URI）。**【真机实测】`failed to start io pipe copy: gvisor-containerd-shim: opening file:///tmp/… failed: open file:///tmp/…: no such file or directory`；【源码读出】`pkg/shim/v1/proc/io.go:93-121` 只把 stdio 当 FIFO 路径打开。init 进程用 `cio.NullIO` 或 FIFO（Exec 输出本来就走 Supervisor，不受影响）。
2. **runsc 不能执行放在宿主 tmpfs bind 挂载上的静态 Go 二进制。**【真机实测】同一个文件：从 `/run`（tmpfs）挂进去执行是 `Segmentation fault`（rc=139）；复制到客户机的 `/tmp`，或从 ext4 目录只读挂进去执行，都正常。Supervisor 是从 Agent 数据目录只读挂进去的，**gvisor Node 的数据目录不能放在 tmpfs 上**。根因没有深挖。
3. **`ctr run --rm`（非 `-d`）配 runsc 时直接返回 128、没有输出**，`-d` 加 `task exec` 正常。只影响手工调试。
4. runsc 会在 `/run/containerd/runsc/<namespace>/null-netns` 上 bind-mount 一个空 netns（gofer 用），**删除 namespace 后它还在**，要 `umount` 再 `rm`。
5. host-local IPAM 在 DEL 之后不会马上复用刚释放的地址（`.2` 释放后下一次拿到 `.4`），按轮转分配。

## 5. `xzai-a800-8` 上装了什么（保留，后续票复用）

| 组件 | 版本 | 路径 | 来源 / 校验 |
|---|---|---|---|
| runsc | `release-20261026.0`（spec 1.2.1） | `/usr/local/bin/runsc` | <https://storage.googleapis.com/gvisor/releases/release/20261026.0/x86_64/gvisor.tar.bz2>，`sha512sum -c` OK |
| gVisor sidecar 二进制 | 同上 | `/usr/local/bin/gvisor-bin/`（`gvisor_sentry`、`checkpointgofer`、`runsc-metric-server` 等） | 同一个 tarball；**必须**放在 runsc 同目录的 `gvisor-bin/` 下（`runsc flags`："sidecar binaries must always be installed in `gvisor-bin`"） |
| containerd-shim-runsc-v1 | 自报 `2.1.5+unknown`，go1.27.3 | `/usr/local/bin/containerd-shim-runsc-v1` | 同一个 tarball；containerd 2.x 从 PATH 解析 `io.containerd.runsc.v1`，没有改 containerd 配置 |
| CNI plugins | `v1.9.1` | `/opt/cni/bin/`（全套，含 bridge、host-local、loopback、portmap） | <https://github.com/containernetworking/plugins/releases/tag/v1.9.1>，`sha256sum -c` OK |
| buildkitd / buildctl | `v0.34.1`（`f5b59a1f`） | `/usr/local/bin/buildkitd`、`/usr/local/bin/buildctl` | <https://github.com/moby/buildkit/releases/tag/v0.34.1>（只装了这两个；tarball 里的 `buildkit-runc`、`buildkit-cni-*` 没装） |
| Go | `go1.27.3` | `/usr/local/go`（不在 PATH 里，用 `/usr/local/go/bin/go`） | <https://go.dev/dl/>，sha256 按 `?mode=json` 校验 OK |
| Go 模块缓存 | — | `/root/go/pkg/mod`（含 containerd v2.2.1 及其依赖） | 构建 spike 客户端时下载 |

已清理：`orrery-spike` 容器、镜像、快照和 namespace，netns `orrery-spike-a/b`，网桥 `orrspk0`，nft 表 `bridge orrery_spike`，`/run/orrery-spike`，`/root/orrery-spike*`，`/run/containerd/runsc/orrery-spike`（含 null-netns 挂载）。清理后 `ctr ns ls` 只剩 `moby`，`nft list tables` 和开始前一致。

## Sources

- containerd `v2.2.1`：`pkg/cdi/oci_opt.go` <https://github.com/containerd/containerd/blob/v2.2.1/pkg/cdi/oci_opt.go>；`cmd/ctr/commands/run/run_unix.go` <https://github.com/containerd/containerd/blob/v2.2.1/cmd/ctr/commands/run/run_unix.go>；`pkg/oci/spec.go`、`pkg/oci/spec_opts.go` <https://github.com/containerd/containerd/tree/v2.2.1/pkg/oci>；`internal/cri/server/service_linux.go` <https://github.com/containerd/containerd/blob/v2.2.1/internal/cri/server/service_linux.go>
- Container Device Interface v1.1.0：<https://github.com/cncf-tags/container-device-interface/tree/v1.1.0/pkg/cdi>
- gVisor `release-20261026.0`：`runsc/sandbox/network.go` <https://github.com/google/gvisor/blob/release-20261026.0/runsc/sandbox/network.go>；`runsc/cgroup/cgroup_v2.go`、`cgroup.go` <https://github.com/google/gvisor/tree/release-20261026.0/runsc/cgroup>；`pkg/shim/v1/runsc/options.go`、`container.go` <https://github.com/google/gvisor/tree/release-20261026.0/pkg/shim/v1/runsc>；`pkg/shim/v1/proc/io.go` <https://github.com/google/gvisor/blob/release-20261026.0/pkg/shim/v1/proc/io.go>
- gVisor 文档：networking <https://gvisor.dev/docs/user_guide/networking/>；containerd 配置（`runsc_config`、`ConfigPath`）<https://gvisor.dev/docs/user_guide/containerd/configuration/>；安装 <https://gvisor.dev/docs/user_guide/install/>
- CNI plugins：bridge <https://www.cni.dev/plugins/current/main/bridge/>，host-local <https://www.cni.dev/plugins/current/ipam/host-local/>
- nftables bridge family：<https://wiki.nftables.org/wiki-nftables/index.php/Nftables_families>
- 仓库内：`internal/agent/tunnel.go`（`forwardSandboxPort`、`dialSandboxPort`），`internal/supervisor/forward.go:35`，`internal/supervisor/run.go:112`；ADR-0017（Tunnel 进 netns 后 dial `127.0.0.1:PORT`）；ADR-0009（limits 回读）
