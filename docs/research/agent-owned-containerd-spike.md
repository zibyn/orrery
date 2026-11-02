# Agent 私有 containerd、buildkitd 与 runsc 和 Docker 同机：实测（spike / #206，ADR-0028）

只回答 `#206`「Spike」一节的四个问题，并核对 ADR-0028 里要靠实测才能定的细节，不重新论证打包的决定。

**可信度分档**：
- **【源码读出】** 源码文件 + 行号（containerd `v2.2.1`；containerd/nri `v0.11.0`，即 containerd `v2.2.1` 的 `go.mod` 所引）。
- **【真机实测】** 在 `xzai-a800-8` 上跑出来的输出（2026-11-02）。

**实测环境**：Ubuntu 22.04.5，kernel `5.15.0-119-generic`，cgroup v2，内核带 `wireguard.ko`。宿主上同时在跑 Docker `29.1.3`（containerd 镜像存储，约 28 个容器）和它的 containerd（`/run/containerd/containerd.sock`，root `/ssd/container-runtime/containerd`，NRI socket `/var/run/nri/nri.sock` 在用）。FORWARD 默认策略 `DROP`，`DOCKER-USER` 为空。

**捆绑的二进制**，放在 `/opt/orrery/bin/`：containerd `2.2.1` 与 `containerd-shim-runc-v2`（官方 release tarball，`sha256sum -c` 通过），runc `1.5.2`（官方 `runc.amd64`，`runc.sha256sum` 通过），buildkitd `v0.34.1`，runsc 与 `containerd-shim-runsc-v1` `release-20261026.0`（与 spike #161 同一份，取自宿主 `/usr/local/bin`）。

**做法**：按 ADR-0028 写 `orrery-containerd.service` 和 `orrery-buildkitd.service`，Agent 的默认 socket 改成 `/run/orrery/containerd/containerd.sock`（只改在实测副本里），`ORRERY_E2E_CTR` 指向私有 socket 的 `ctr`，然后在这台机器上跑完整的 `scripts/e2e.sh`，runc 一遍、runsc 一遍。宿主的 hysteria 占了 18080/18081，实测副本把 e2e 端口整体挪到 2808x；这台机器拉不到 Docker Hub 和 Go/npm 源，Compose 镜像经 `docker.m.daocloud.io` 镜像站拉取后改 tag，server 镜像用宿主上编好的二进制。这些只影响 Control Plane 的搭法，不影响下面的结论。

## 0. 结论（先读这里）

| # | 问题 | 结论 | 要点 |
|---|---|---|---|
| 1 | 私有 containerd 与 Docker 的 containerd 并存，socket、cgroup、snapshotter 不冲突 | **是，但 `config.toml` 必须关掉三处默认** | 默认配置会读宿主的 `/etc/containerd/conf.d/*.toml`，NRI 会 `os.Remove` 掉宿主 containerd 正在用的 `/var/run/nri/nri.sock` 再自己监听（§1.2）。`imports = []`、关 NRI、关 CRI 后，宿主 containerd 和 Docker 的 PID 不变，NRI socket 不动。shim socket 目录、runc/runsc 状态目录和 cgroup 父路径两边共用，但以 namespace 区分，不冲突（§1.3）。 |
| 2 | Sandbox 网络（`orrery0`、Agent 的 nftables 表）在 Docker 的 FORWARD drop 和 iptables-nft 规则下照常工作 | **Sandbox 之间和到宿主的流量是**；出公网要放行，与私有 containerd 无关 | `TestEgressRules`、`TestEgressMode`、`TestSandboxLinks` 不加任何 `DOCKER-USER` 规则即通过。Sandbox 到公网（`223.5.5.5:53`）被 Docker 的 FORWARD drop 挡住，这与 `docs/deploy.md` 已写的一致，私有 containerd 不改变它（§2）。 |
| 3 | runsc 经捆绑的 shim 运行，`host-uds=create` 与 spike #161 一致 | **是，但包里要带 `gvisor-bin/`** | `release-20261026.0` 的 runsc 在自己旁边的 `gvisor-bin/` 里找 `gvisor_sentry` 等 sidecar，缺了就 `sidecar "gvisor_sentry" not usable`。带上之后 runsc 全套 e2e 结果见 §3。 |
| 4 | Agent 重启、升级时 Exec 不断；containerd unit 不被无谓重启 | **是，但 `Requires=` 应改成 `Wants=`** | Exec 进行中 `systemctl restart orrery-containerd`：约 60 ms，shim PID 不变，Exec 输出不缺不重，Agent 自己重连。重启 Agent 不碰 containerd。但 `Requires=` 让 containerd 的每次重启（含崩溃后 `Restart=always`）都连带重启 Agent，而 `stop` 再 `start` containerd 之后 Agent 一直停着（§4）。 |

ADR-0028 需要按 §5 修订五处，之后可以接受。

## 1. 两个 containerd 并存（Q1）

### 1.1 能跑起来的 `config.toml`

**【真机实测】** 最终用的 `/etc/orrery-agent/containerd.toml`：

```toml
version = 3
root = "/var/lib/orrery-agent/containerd"
state = "/run/orrery/containerd/state"
imports = []
disabled_plugins = ["io.containerd.grpc.v1.cri", "io.containerd.cri.v1.runtime", "io.containerd.cri.v1.images", "io.containerd.podsandbox.controller.v1.podsandbox", "io.containerd.nri.v1.nri"]
[grpc]
  address = "/run/orrery/containerd/containerd.sock"
[ttrpc]
  address = "/run/orrery/containerd/containerd.sock.ttrpc"
[debug]
  address = "/run/orrery/containerd/debug.sock"
[plugins."io.containerd.internal.v1.opt"]
  path = "/var/lib/orrery-agent/containerd/opt"
```

起服务前后：

```
host containerd=2078978 docker=2079013
active active active active          # orrery-containerd orrery-buildkitd containerd docker
host containerd now=2078978 docker=2079013
/var/run/nri/: srwxr-xr-x 1 root root 0 Aug 14 11:58 nri.sock   # 时间戳没变，仍是宿主 containerd 的
```

`ctr plugins ls` 里除 `skip` 的 snapshotter（btrfs、zfs、devmapper、blockfile、erofs）和 tracing 外全部 `ok`。默认的 overlayfs snapshotter 在私有 root 下，和 Docker 的 `/ssd/...` 互不相干。

### 1.2 不改就会出事的默认值

**【真机实测】** `containerd --config <只写了 root/state/grpc.address 的文件> config dump` 里仍指向宿主全局路径的项：

```
imports = ['/etc/containerd/conf.d/*.toml']
[plugins.'io.containerd.nri.v1.nri']
    disable = false
    socket_path = '/var/run/nri/nri.sock'
[plugins.'io.containerd.internal.v1.opt']
    path = '/opt/containerd'
[plugins.'io.containerd.grpc.v1.cri'] ... cni bin_dirs = ['/opt/cni/bin'], conf_dir = '/etc/cni/net.d'
```

**【源码读出】** NRI 抢 socket：containerd/nri `v0.11.0` `pkg/adaptation/adaptation.go:542-558`：

```go
func (r *Adaptation) startListener() error {
	...
	os.Remove(r.socketPath)
	if err := os.MkdirAll(filepath.Dir(r.socketPath), 0700); err != nil {
	...
	l, err := net.ListenUnix("unix", &net.UnixAddr{
		Name: r.socketPath,
```

第二个 containerd 不关 NRI，就会删掉宿主 containerd 的 `nri.sock` 再自己监听。这一条没有在真机上复现，因为它会破坏这台共用机器上的宿主 containerd。

`imports` 不清空，宿主管理员给自己 containerd 加的 drop-in 会原样进入 Agent 的 containerd。CRI 不用，关掉也免得它碰 `/etc/cni/net.d`。

### 1.3 两边共用、但不冲突的目录

**【源码读出】** shim socket 固定在 `/run/containerd/s/`，不随 `state` 变：`pkg/shim/util_unix.go:76-89`：

```go
const socketRoot = defaults.DefaultStateDir
...
	path := filepath.Join(socketPath, ns, id)
	...
	d := sha256.Sum256([]byte(path))
	return fmt.Sprintf("unix://%s/%x", filepath.Join(socketRoot, "s"), d), nil
```

哈希里有私有 containerd 的 socket 路径，所以和宿主 containerd 的 shim 不会撞名。

**【源码读出】** runc 状态目录默认 `/run/containerd/runc/<namespace>/<id>`：`cmd/containerd-shim-runc-v2/process/init.go:39`（`RuncRoot = "/run/containerd/runc"`），`:84` 在 runtime options 没给 `Root` 时用它。

**【真机实测】** 私有 containerd 里起一个 runc、一个 runsc 容器（namespace `orrery-spike206`）：

```
/run/containerd/runc/:  orrery  orrery-spike  orrery-spike206
/run/containerd/runsc/: orrery-probe173  orrery-spike  orrery-spike206
0::/orrery-spike206/s-runc      # cgroup，containerd 默认 /<namespace>/<id>
```

这三处（shim socket、runc/runsc 状态、cgroup 父路径）两个 containerd 共用，靠 namespace 和容器 id 分开。只有两边**同一个 namespace 里同一个 id 同时有 task** 才会冲突。迁移时宿主 containerd 的 `orrery` namespace 是空的（这台机器上 `ctr -n orrery c ls`、`t ls` 都为空），所以 hard cut 不受影响。

### 1.4 shim 和 runc 必须从包里找，PATH 要显式给

**【源码读出】** `core/runtime/v2/shim_manager.go:384-399`：containerd 先 `exec.LookPath("containerd-shim-<name>-v2")`，**找不到**才去自己二进制旁边找。shim 再按名字从继承的 PATH 里执行 `runc` 或 `runsc`（`process/init.go:87`，`Command: runtime`）。

这台机器 `/usr/bin` 有 `containerd-shim-runc-v2` 和 `runc`（Docker 的包），`/usr/local/bin` 有 `containerd-shim-runsc-v1`。不给 PATH，私有 containerd 会用宿主的 shim 和 runc，捆绑的版本就失去了意义。unit 里写 `Environment=PATH=/opt/orrery/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin` 之后：

```
shim 1446536 -> /opt/orrery/bin/containerd-shim-runc-v2 ppid=1
shim 1447261 -> /opt/orrery/bin/containerd-shim-runsc-v1 ppid=1
      1 /opt/orrery/bin/gvisor-bin/gvisor_sentry
```

buildkitd 的 OCI worker 同样按 PATH 找 `runc`，`orrery-buildkitd.service` 也要这一行。

### 1.5 runsc 不需要在 `config.toml` 里注册

**【真机实测】** 上面的 `config.toml` 里没有任何 runtime 段，`ctr run --runtime io.containerd.runsc.v1 --runtime-config-path runsc.toml` 照常起 runsc 容器。Agent 本来就在每个容器上带 `containerd.WithRuntime("io.containerd.runsc.v1", &runtimeoptions.Options{ConfigPath: ...})`（`internal/agent/containerd.go:167`），runtime 段只有 CRI 才读。ADR-0028 里「`config.toml`（注册 `runsc` runtime）」可以去掉。

## 2. Docker 的 FORWARD drop 之下的 Sandbox 网络（Q2）

**【真机实测】** `DOCKER-USER` 为空、FORWARD `DROP`，runc：

```
--- PASS: TestEgressMode (37.08s)
--- PASS: TestEgressRules (50.37s)
--- PASS: TestSandboxLinks (28.05s)
--- PASS: TestSpike206Internet (13.11s)    # 期望 Sandbox 连不上 223.5.5.5:53；宿主自己连得上
```

Sandbox 之间走 `orrery0` 网桥，到宿主地址走 INPUT，都不经过 iptables 的 FORWARD，所以 Docker 的 drop 不影响。出公网是路由转发，被 Docker 的 FORWARD drop 挡住。`docs/deploy.md` 已写明要放行 `orrery0` 的转发，`scripts/ci-e2e-node.sh` 的两条 `DOCKER-USER` 规则是做法。本次没有在这台共用机器上加这两条规则（权限没给），所以「放行后出网通」没有在这里重测；它只取决于 `orrery0` 和 Agent 的 nftables 表，与 containerd 是哪一个无关。

全套 e2e 里另外三项因此失败，都是 Sandbox 里 `apk add git` 连不上 `dl-cdn.alpinelinux.org`：`TestGitLinkClonePush`、`TestGitUnlinked`、`TestSandboxTokenScope`。

## 3. runsc 经捆绑的 shim（Q3）

### 3.1 `gvisor-bin/`

**【真机实测】** 只放 `runsc` 和 `containerd-shim-runsc-v1`：

```
ctr: failed to create shim task: OCI runtime create failed: creating container: cannot create sandbox: cannot create sandbox process: sidecar "gvisor_sentry" not usable: stat /opt/orrery/bin/gvisor-bin/gvisor_sentry: no such file or directory
```

宿主 `/usr/local/bin/gvisor-bin/` 里是 `gvisor_sentry`（54 MB）、`checkpointgofer`（48 MB）、`runsc-metric-server`（21 MB）和三个小脚本。整个目录复制到 `/opt/orrery/bin/gvisor-bin/` 后 runsc 正常，`dmesg` 是 `Starting gVisor...`。

### 3.2 全套 e2e（`ORRERY_E2E_ISOLATION=runsc`）

**【真机实测】** 两遍全套 `scripts/e2e.sh`，都在私有 containerd 上：

| 失败项 | runc（922 s） | runsc（1871 s） | 原因 |
|---|---|---|---|
| `TestWebConsole` | ✗ | ✗ | 实测用的 server 镜像没有 `-tags embed`，不是 Node 的问题 |
| `TestGitLinkClonePush`、`TestGitUnlinked`、`TestSandboxTokenScope` | ✗ | ✗ | Sandbox 出不了公网，`apk add git` 失败（§2） |
| `TestSDKParityPython` | ✗ | ✗ | 宿主 Python 没有 `httpx` |
| `TestSecretInject` | ✗ | ✓ | 全套里一次 PTY 读超时；单独重跑两次都通过 |
| `TestGvisorParity/egress-rules` | 跳过 | ✗ | 见下 |
| `TestSandboxLinks` | ✓ | ✗ | 见下 |

其余场景全部通过，包括 `TestGvisorParity` 的 `runs-under-gvisor`、`exec-log-stream`、`agent-restart-survives`，以及 PTY、端口转发、Snapshot 构建（经私有 buildkitd）、文件、Workspace 等。`TestCacheEviction` 的镜像部分是空过的，见 §5 末尾。

runsc 下的两项失败**与私有 containerd 无关**：把 Agent 默认 socket 改回宿主的 `/run/containerd/containerd.sock`、在宿主 containerd 上重跑这两项，结果完全一样。Agent 日志里是 `link self-check: the linked probe did not reach the target`，`sandbox_links` 报 `unsupported`；egress-rules 子测试的 Sandbox 因没有可放置的 Node 而 `admission_timeout`。runsc 下 Sandbox 之间的连通在 `e1a55ce`（#205，netlink 建网）之后坏了，需要另开票。

## 4. 重启与升级（Q4）

### 4.1 Exec 进行中重启 containerd

**【真机实测】** 一次性测试 `TestSpike206ContainerdRestart`（只在实测副本里）：起 Sandbox，跑一个 15 秒、每 0.1 秒打一行的 Exec，进行中 `systemctl restart orrery-containerd`，记录 namespace `orrery` 的 shim PID：

```
spike206_test.go:43: containerd restart took 62.949287ms; shims before "1590450" after "1590450"
--- PASS: TestSpike206ContainerdRestart (26.88s)
```

Exec 以 `exited` 结束，150 行不缺不重；之后同一个 Sandbox 里新 Exec、新建一个 Sandbox 都成功，说明 Agent 的 containerd 客户端自己重连了，不需要重启 Agent。

### 4.2 Agent 重启

全套 e2e 的 `TestAgentRestartSurvives`、`TestAgentRestartKeepsPTY` 在私有 containerd 上通过（runc 与 runsc 两遍），Agent 被杀再起，Exec 和 PTY 都接上。

### 4.3 `Requires=` 的连带效果

**【真机实测】** 用一个 `Requires=`/`After=orrery-containerd.service` 的哑 unit 代替 Agent：

```
start:            ctrd=1590617 agent=1592600 bk=1445928
restart agent:    ctrd=1590617 agent=1592606 bk=1445928      # containerd 不动
restart ctrd:     ctrd=1592613 agent=1592648 bk=1445928      # Agent 被连带重启
SIGKILL ctrd:     ctrd=1592681 agent=1592713 bk=1445928      # Restart=always 拉起 containerd，Agent 也重启
stop ctrd:        ctrd=0 agent=0 agent-active=inactive
start ctrd:       ctrd=1592731 agent=0 agent-active=inactive  # Agent 不会回来
```

buildkitd 只有 `After=`，始终不受影响。§4.1 已经说明 Agent 不需要跟着 containerd 重启；`Requires=` 只带来两个坏处：containerd 每次重启都让 Agent 重连 Control Plane、PTY 重挂，运维 `stop`/`start` containerd 之后 Agent 停着不回来。改用 `Wants=` + `After=`。

## 5. 对 ADR-0028 的修订

1. **`config.toml` 写明**：`imports = []`；`disabled_plugins` 至少含 NRI（`io.containerd.nri.v1.nri`）和 CRI（`io.containerd.grpc.v1.cri`、`io.containerd.cri.v1.runtime`、`io.containerd.cri.v1.images`、`io.containerd.podsandbox.controller.v1.podsandbox`）；`ttrpc`、`debug` 地址和 `opt` 插件路径都放在私有目录下。去掉「注册 `runsc` runtime」（§1.5）。
2. **unit 写 PATH**：`orrery-containerd.service` 和 `orrery-buildkitd.service` 都带 `Environment=PATH=/opt/orrery/bin:...`，捆绑目录排在最前（§1.4）。
3. **`orrery-agent.service` 用 `Wants=` + `After=`**，不用 `Requires=`（§4.3）。
4. **包内容和大小**：runsc 要连同 `gvisor-bin/` 一起打包；buildkitd 用 `--oci-worker-net host`，不需要 CNI。amd64 实测：containerd、`containerd-shim-runc-v2`、runc、buildkitd、runsc、`containerd-shim-runsc-v1`、`gvisor-bin/` 共 **300 MB**，`tar.gz` 后 **137 MB**（不含 `ctr`、`buildctl`）。ADR 写的「约 150–200 MB」应改成这个数。
5. **迁移的一个新后果**：私有 containerd 的镜像存储是空的，Agent 自检的探针镜像 `docker.io/library/alpine:3.22` 要重新从 Docker Hub 拉。这台机器拉不到 Docker Hub，Node 停在 `unavailable`（`runtime self-check failed ... probe image: failed to resolve reference "docker.io/library/alpine:3.22"`），把镜像 `docker save | ctr -n orrery images import` 进私有 containerd 后才就绪。这在共用宿主 containerd 时被已有的缓存掩盖了；hard cut 的步骤要提到它，或者另开一张票让探针镜像走平台 registry。

实现时还要改一处测试：`e2e/cache_eviction_test.go:210` 的 Node shell 脚本直接调 `ctr -n orrery images import`，没有走 `ORRERY_E2E_CTR`。本次两遍全套里，它把镜像导进了**宿主** containerd，而 `imageExists` 查的是私有 containerd，所以「未被引用的镜像已被清掉」这条断言是空过的（宿主 containerd 的 `orrery` namespace 里留下了两个 `127.0.0.1:15000/e2e-evict:*`）。

## 6. 本次没有验证的

- arm64。
- `buildkitd.toml` 里按 `--ca` 生成的 `ca = [...]`：e2e 的平台 registry 是明文 HTTP，只验证了 `[registry."127.0.0.1:15000"] http = true` 这一形状和 Snapshot 构建。
- GPU（CDI）在私有 containerd 上：CDI spec 在宿主 `/var/run/cdi`，Agent 在客户端注入，不经 containerd 配置，按 spike #161 的机制应当不变，但这次没跑 GPU 场景。
- 放行 `orrery0` 转发后的出网（§2）。
- install.sh 的 sha256 比对与按需重启：脚本还没写，§4 只验证了它依赖的 systemd 行为。
