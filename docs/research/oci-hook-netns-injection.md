# OCI hook 的配置面：往容器 netns 注入 nftables 需要的事实

面向 `#16`（注入点定为 OCI hook、v1 必须实现）与 `#19`（hook 与 Worker 是同一个二进制的两个入口）。`#3`/`#16` 定了「用 OCI hook 注入 nftables」，但 `docs/research/sandbox-isolation-and-gpu.md` 与 `#3` 的决议里**没有一个字**讲 hook 怎么配。这份只回答事实与出处，不回答「选哪个」。

> **取证强度**：逐条打开一手页面与一手源码核对（OCI Runtime Spec 的 `config.md`/`runtime.md`、runc 源码、crun 源码、podman 源码、containers-common 的 `oci-hooks(5)`、CDI `SPEC.md` 与其库源码、`dockerd` 参考页、moby 源码），引文保留原文。文档里没写、只能从源码读出来的结论标「**源码读出**」。未能证实的一律留在 `# 附`，不填。

## 0. 先读这三条

1. **OCI 规范里没有「hook 配置放在哪里」这回事。** 规范只定义 `config.json` 里的 `hooks` 对象——一个容器一份，由 runtime caller 生成。「把某个 JSON 文件放进某个目录，于是所有容器都挂上这个 hook」是 **podman/containers-common 在规范之上加的一层**，不是规范的一部分。
2. **docker 上没有「配一个通用 hook 目录」这种入口。** `daemon.json` 没有对应键，`dockerd` 没有对应 flag（`# 1`），上游 RFE #36987 自 2018 年 open；dockerd 自己写的两处 hook 都是硬编码的 `prestart`。**唯一能从配置文件走到 `spec.Hooks` 的路径是 CDI spec 的 `containerEdits.hooks`**（见 `# 1` 同一节）——但它只在容器**请求了一个 CDI 设备**时才被应用，而 `#3` 定的加速器表达正是 CDI 设备名。其余只能换 OCI runtime（`--add-runtime` / `default-runtime` + `reload dockerd`）。
3. **hook 的权限来自运行时，不来自容器。** `createRuntime`/`createContainer` 都跑在容器进程**降权之前**（runc 顺序：`prepareRootfs` → `finalizeNamespace`），拿到的是宿主 root 的 `CAP_NET_ADMIN`；容器进程本身**不需要任何额外能力**——与 `#16`「容器保持默认能力集、无 `CAP_NET_ADMIN`」相容（`# 4`）。

## 1. docker：没有通用配置面，只有一条 CDI 窄路（Q1）

三条独立证据，都是「数出来的」，不是推断：

| 证据 | 结果 |
|---|---|
| `dockerd` 参考页（<https://docs.docker.com/reference/cli/dockerd/>，2026-10-15 取，681 KB） | 全文 "hook" 出现 **0** 次；对照：`default-runtime` 9 次、`cdi-spec-dir` 4 次、`daemon.json` 21 次 |
| moby `daemon/config/config.go`（master `df9adbfaf592`，2026-10-15，889 行） | "hook"（忽略大小写）出现 **0** 次——**`daemon.json` 的键结构里没有 hook** |
| moby 全仓库代码搜索 `specs.Hook` / `CreateRuntime` | 构造 OCI hook 的地方**只有两处**，都是硬编码、都无配置键：`daemon/devices_nvidia_linux.go`（NVIDIA）与 `daemon/internal/builder-next/executor_linux.go`（buildkit 的网络 setkey），见下 |

来源：<https://raw.githubusercontent.com/moby/moby/master/daemon/config/config.go>、<https://raw.githubusercontent.com/moby/moby/master/daemon/devices_nvidia_linux.go>。

**上游自己也认这件事**：moby/moby **#36987「[RFE] Support for OCI hooks [2018 edition]」自 2018-05-02 开到现在仍是 open**（最后更新 2019-06-04）。issue 里描述的就是当前状态：

> Today Docker supports swapping `runc` with another OCI compliant runtime... However, this approach is **not composable**, you can't combine custom runtimes. If I want to customize the behavior of runc for Docker today, I need to **fork runc and add my modifications, then ship the whole binary**.

issue 里的 proposal（在 `daemon.json` 里加 `hooks` 段）**从未实现**；issue 评论里别人给的绕法正是换 runtime（"I just did something similar to what nvidia is doing now to add a 'systemd' runtime so I can use the relevant OCI hooks"）。来源：<https://github.com/moby/moby/issues/36987>。

**第一处例外，正好也是我们关心的那一类问题**：moby 自己会注入**一个** OCI hook——NVIDIA 的（`daemon/devices_nvidia_linux.go`，`injectNVIDIARuntimeHook`）：

```go
// This implementation uses prestart hooks, which are deprecated.
// CreateRuntime is the closest equivalent, and executed in the same
// locations as prestart-hooks, but depending on what these hooks do,
// possibly one of the other hooks could be used instead (such as
// CreateContainer or StartContainer).
s.Hooks.Prestart = append(s.Hooks.Prestart, specs.Hook{ //nolint:staticcheck // FIXME(thaJeztah); replace prestart hook with a non-deprecated one.
    Path: path, ...
```

触发条件是**硬编码**的：宿主 `$PATH` 上存在 `nvidia-container-runtime-hook`、且容器用了 `--gpus`（`exec.LookPath(nvidiaContainerRuntimeHookExecutableName)`）。**没有对应的配置键**——这说明 dockerd 内部确实能往 `spec.Hooks` 写东西，但外部只能靠硬编码，也解释了 nvidia-container-runtime 为什么要以「替换 runtime」的形态存在。另外 NRI（改 OCI spec 的扩展机制）在 moby 里**明确拒绝**加 hook：`daemon/internal/nri/nri.go` 里 `if adj.Hooks != nil { ... unsupported = append(unsupported, "hooks") }`。

**第二处更值得看，因为它是「hook 里做网络 namespace 的事」的现成先例**（`daemon/internal/builder-next/executor_linux.go:170-176`，buildkit 构建容器）：

```go
// attach netns to bridge within the container namespace, using reexec in a prestart hook
s.Hooks = &specs.Hooks{
    Prestart: []specs.Hook{{ //nolint:staticcheck // ignore SA1019 (Prestart is deprecated); FIXME(thaJeztah): see https://github.com/moby/moby/issues/52488
        Path: filepath.Join("/proc", strconv.Itoa(os.Getpid()), "exe"),
        Args: []string{"libnetwork-setkey", "-exec-root=" + ..., iface.sbx.ContainerID(), shortNetCtlrID},
    }},
}
```

三件可照抄的事实：① 它们是 **`prestart`** hook（规范已废弃、主线上仍有人在用）；② hook 的 `path` 可以指向**自己这个二进制的 `/proc/self/exe`**，靠 **argv 子命令**分流——形状与 `#19`「同一个二进制的第二个入口」一致（这是先例，不是本票的结论）；③ 普通容器（非 buildkit）的同类动作已经**不再走 hook**：`daemon/start_linux.go` 的 `initializeCreatedTask()` 直接在 daemon 里按 `/proc/<pid>/ns/net` 设 sandbox key 再 `allocateNetwork`（moby#44385、#53849、#52488 就是这条演进）。

**结论**：docker 侧没有「通用 hook 配置目录」——dockerd 自己生成 `config.json` 并直接调 runc，用户没有往 `hooks` 里塞任意东西的入口；dockerd 自己写过的 hook 只有两处硬编码的 `prestart` hook（NVIDIA、buildkit setkey）。**唯一能靠「往目录放文件」影响 `spec.Hooks` 的是 CDI spec（下一节）。**

### CDI spec 的 `containerEdits.hooks`：docker 上唯一从配置文件进 hook 的路径

**这是 docker 侧唯一能靠「往某个目录放文件」把 hook 送进 `spec.Hooks` 的机制**，整条链每一环都核对过：

1. **CDI spec 允许 hooks**。CDI `SPEC.md`（<https://github.com/cncf-tags/container-device-interface/blob/main/SPEC.md>，第 250–256 行）：`containerEdits` 能做的编辑是 `env`、`devices`、`mounts`、`hooks`；`hooks` (array of objects, OPTIONAL) 里每项有 `hookName`、`path`、`args`、`env`、`timeout`，其中——**"`hookName` is the name of the hook to invoke, if the runtime is OCI compliant it should be one of {createRuntime, createContainer, startContainer, poststart, poststop}."**（注意：**不含 `prestart`**，实现里仍接受。）
2. **CDI 库把它落到 OCI 规范的 `spec.Hooks.*`**（<https://github.com/cncf-tags/container-device-interface/blob/main/pkg/cdi/container-edits.go>、`internal/ociedit/spec_editor.go`）：`case CreateRuntimeHook: editor.AddCreateRuntimeHook(ociHook)` → `e.spec.Hooks.CreateRuntime = append(...)`（现有 hook 保留，CDI 的追加在后面）。
3. **docker 走这条链**：`daemon/cdi.go` 的 `injectCDIDevices(s *specs.Spec, dev *deviceInstance)` 调 `c.registry.InjectDevices(s, cdiDeviceNames...)`。
4. **门是「请求了 CDI 设备」**：它是 `deviceDriver.updateSpec`，只在容器带 CDI 设备请求时才跑（`docker run --device vendor.com/class=dev`）。

docker 官方文档对这一能力的描述（dockerd 参考页 "Configure CDI devices" 节，原文）：

> CDI is currently only supported for Linux containers and **is enabled by default since Docker Engine 28.3.0**. …
> The default specification directories are: `/etc/cdi/` for static CDI Specs, `/var/run/cdi` for generated CDI Specs

`daemon.json` 的 `cdi-spec-dirs`（或 `--cdi-spec-dir`）可改目录；用 `"features": {"cdi": false}` 关掉。

**⚠️ 但 docker 文档里没有一个字提到 hook**：整张 dockerd 参考页 "hook" 出现 0 次；「Docker 会应用 CDI spec 里的 hooks」这句话在 Docker 文档里**不存在**，上面的链条是我从源码读出来的——而 `#3` 定的加速器表达正是 CDI 设备名，所以这条路径**值得实测**（见 `# 附`）。

**docker 那一侧真实存在的旋钮**（同一份参考页原文）：

- `--add-runtime runtime` —— "Register an additional OCI compatible runtime (default [])"
- `--default-runtime` —— "Default OCI runtime for containers (default \"runc\")"
- `daemon.json` 里对应 `runtimes` 键（页面示例：`{ "runtimes": { "<runtime>": { "path": "/path/to/bin", "runtimeArgs": ["...args"] } } }`）

这**不是** hook 配置——是把 runc 换成一个包装器。`sandbox-isolation-and-gpu.md` §3.3 写的「注入点选 OCI hook 最干净（**与后端无关**）」，在 docker 上要兑现，靠的就是这个旋钮（换 runtime，由包装器往 `config.json` 里加 `hooks`），而不是配置目录。

**docker 侧确实能传 annotation**：`docker run --annotation`（docker run 参考页：`--annotation API 1.43+ Add an annotation to the container (passed through to the OCI runtime)`），moby 侧实现是 `coci.WithAnnotations(c.HostConfig.Annotations)`（`daemon/oci_linux.go:1047`）→ 落进 `config.json` 的 `annotations`，也就是 hook 从 stdin 收到的那份 state 里的 `annotations`。**「per-task 数据经 annotation 进 hook」这条通道两端都有**；差别只在 docker 没有消费它的机制（见 `# 1` 的证据）。

## 2. podman：hook 配置目录（Q2）

### 2.1 目录与开关

`podman(1)` 的 `--hooks-dir=path`（全局持久 flag，源码登记于 `cmd/podman/root.go`：`pFlags.StringArrayVar(&podmanConfig.HooksDir, "hooks-dir", ...)`，描述 "Set the OCI hooks directory path (may be set multiple times)"）原文：

> Each `*.json` file in the path configures a hook for Podman containers. For more details on the syntax of the JSON files and the semantics of hook injection, see `oci-hooks(5)`. Podman and libpod currently support both the 1.0.0 and 0.1.0 hook schemas, although the 0.1.0 schema is deprecated.
>
> This option may be set multiple times; paths from later options have higher precedence (`oci-hooks(5)` discusses directory precedence).
>
> If `--hooks-dir` is unset for root callers, Podman and libpod currently default to `/usr/share/containers/oci/hooks.d` and `/etc/containers/oci/hooks.d` in order of increasing precedence. Using these defaults is deprecated. Migrate to explicitly setting `--hooks-dir`.

来源：<https://github.com/containers/podman/blob/main/docs/source/markdown/podman.1.md>（渲染版 <https://docs.podman.io/en/latest/markdown/podman.1.html>）。

**⚠️ 三份一手来源对「默认目录是什么」互相矛盾——这是本次调研里最该被记住的一条不确定：**

| 来源 | 默认目录 |
|---|---|
| 代码（真正跑的那个） | `DefaultHooksDirs = []string{"/usr/share/containers/oci/hooks.d"}`，由 `defaultConfig()` 里 `c.HooksDir.Set(DefaultHooksDirs)` 生效 |
| `podman(1)` 的文字 | `/usr/share/...` 与 `/etc/...` **两个**，仅 root caller，且标为 deprecated |
| `containers.conf(5)` | `hooks_dir=["/etc/containers/oci/hooks.d", ...]` |
| 发行的 `containers.conf` 模板 | `#hooks_dir = [` / `#  "/usr/share/containers/oci/hooks.d",` ——**注释掉的** |

来源：<https://raw.githubusercontent.com/containers/common/main/pkg/config/default.go>（第 102–103、386 行）、<https://raw.githubusercontent.com/containers/container-libs/main/common/docs/containers.conf.5.md>、<https://raw.githubusercontent.com/containers/common/main/pkg/config/containers.conf>（第 601–605 行）。

**按代码读**：`Engine.HooksDir` 有非空编译默认值，所以 `/etc/containers/oci/hooks.d` 这个「override 目录」在 stock 配置下**不会被读**；podman 源码里那段 deprecated 的「两个目录都读」分支只有在用户显式把 `hooks_dir` 清空时才走到（`libpod/container_internal.go` 的 `setupOCIHooks()`：`if len(c.runtime.config.Engine.HooksDir.Get()) == 0 { if rootless.IsRootless() { return nil, nil } for _, hDir := range []string{hooks.DefaultDir, hooks.OverrideDir} { ... } }`）。`--hooks-dir` 也只在显式指定时才生效：`pkg/domain/infra/runtime_libpod.go` 里 `if fs.Changed("hooks-dir") { ... libpod.WithHooksDir(...) }`。三个来源我没有找到哪个是「对的」的官方说明，**要落地必须实测目标机器上的 podman**。

**没有开/关开关**：`containers.conf(5)` 里除 `hooks_dir` 外没有第二个提到 hook 的键，`podman(1)` 也没有 on/off flag。目录里放什么就挂什么；要禁用只能把 `hooks_dir` 设成空列表。**也没有 per-user 目录**：`$HOME/.config/containers/oci/hooks.d` 在一手来源里不存在（`$HOME/.config/containers/containers.conf` 是「设置 `hooks_dir` 的配置文件」，不是「放 hook JSON 的地方」）。

### 2.2 hook 配置文件的 JSON schema

权威文档是 `oci-hooks(5)`（podman.1 的 SEE ALSO 指向它）：<https://github.com/containers/container-libs/blob/main/common/pkg/hooks/docs/oci-hooks.5.md>。**1.0.0 schema** 正是我们要的：

```json
{
  "version": "1.0.0",
  "hook": { "path": "/usr/libexec/oci/hooks.d/xxx", "args": ["xxx", "--flag"] },
  "when": { "annotations": { "^stars\\.sandbox/kind$": "^task$" } },
  "stages": ["createRuntime"]
}
```

逐字段（`oci-hooks(5)` 原文）：

- `version` (required string)：本 schema 下取值 `1.0.0`。
- `hook` (required object)：**"The hook to inject, with the hook-entry schema defined by the 1.0.2 OCI Runtime Specification."** —— 即 `path`/`args`/`env`/`timeout`。
- `when` (required object)，**至少给一条**：
  - `always` (optional boolean)
  - `annotations` (optional object)：**"If all `annotations` key/value pairs match a key/value pair from the configured annotations, this condition matches. Both keys and values must be POSIX extended regular expressions."**
  - `commands` (optional array of strings)："If the configured `process.args[0]` matches an entry, this condition matches."
  - `hasBindMounts` (optional boolean)
- `stages` (required array of strings)：**"Entries must be chosen from the 1.0.2 OCI Runtime Specification hook stages or from extension stages supported by the package consumer."** → 我们要写 `createRuntime`。

合法 stage 值在源码里是白名单（`pkg/hooks/1.0.0/hook.go` 的 `Validate()`）：`createContainer, createRuntime, prestart, poststart, poststop, startContainer`，外加消费方自己的扩展 stage。

**⚠️ 规范式的直通格式（`{"hooks": {"createRuntime": [...]}}`）podman 不支持**（【源码读出】，`pkg/hooks/read.go`）：读取器按**必填的 `version` 字段**分派，只注册了 `1.0.0`、`0.1.0`、`""`（无 version → 按 0.1.0 解析）三个 reader，一个裸的 OCI `hooks` 对象会以「missing required property: hook」或 `unrecognized hook version` 失败。

### 2.3 文件名约定

【源码读出】+ `oci-hooks(5)` 原文：

- **`.json` 后缀必需且大小写敏感**（`read.go`：`if !strings.HasSuffix(path, ".json") { return nil, ErrNoJSONSuffix }`，`ReadDir` 里静默跳过）；`.JSON` 不算。
- **注入顺序** = 文件名**转小写后**按 Unicode code point 排序（`hooks.go` 的 `sort.Slice(... strings.ToLower(...))`；`oci-hooks(5)` 同义："It is strongly recommended to make the sort order unambiguous depending on an ASCII-only prefix (like the `01`/`02` above)"）。数字前缀是**建议**，不是要求。
- **目录间遮蔽**：`oci-hooks(5)`——"a JSON filename in a preferred directory masks entries with the same filename in directories with lower precedence"（同名文件高优先级目录遮蔽低优先级目录）。

### 2.4 podman 独有的 `precreate` stage（不是 OCI stage）

`podman(1)` 原文：「Podman and libpod currently support an additional `precreate` state which is called before the runtime's `create` operation. Unlike the other stages, which receive the container state on their standard input, `precreate` hooks **receive the proposed runtime configuration on their standard input**. They may alter that configuration as they see fit, and write the altered form to their standard output.」——这是 **podman 自己执行**的、能改 `config.json` 的 hook，只在 podman 上有，不是 OCI stage。

## 3. stage 与时序：hook 跑的时候 netns 建好了吗（Q3）

### 3.1 规范定义的六个 stage

OCI Runtime Spec 的 stage 表（`config.md`，[main `b6f46de`](https://raw.githubusercontent.com/opencontainers/runtime-spec/main/config.md)，与 v1.3.0 一致）：

| Name | Namespace | When |
|---|---|---|
| `prestart`（**已废弃**） | runtime | create 期间、runtime 环境建好之后、`pivot_root` 之前 |
| `createRuntime` | runtime | 同上 |
| `createContainer` | container | 同上（在 `createRuntime` 之后） |
| `startContainer` | container | start 之后、用户进程 exec 之前 |
| `poststart` | runtime | 用户进程执行之后、start 返回之前 |
| `poststop` | runtime | 容器删除之后、delete 返回之前 |

**netns 在 `createRuntime` 时已经存在**——`config.md` 的原句就是为这件事写的：

> On Linux, for example, they are called after the container namespaces are created, so they provide an opportunity to customize the container (**e.g. the network namespace could be specified in this hook**).

`createRuntime` 的条目里并列写了同义句。**能不能在里面装 nftables 规则**：能——hook 在 runtime namespace（宿主）以 root 身份跑，容器 netns 已经存在且可通过 `/proc/<pid>/ns/net` 拿到（§3.2）；`createContainer` 更直接，hook 本身**已经在那个 netns 里**（§3.2、`# 4`）。

规范同时给了一句**限制**（`config.md`，`createRuntime`/`createContainer` 两节都写了）：

> The definition of `createRuntime` hooks is currently underspecified and hooks authors, should only expect from the runtime that the mount namespace have been created and the mount operations performed. Other operations such as cgroups and SELinux/AppArmor labels might not have been performed by the runtime.

### 3.2 容器 id / netns 路径从哪来（runc 的实现，【源码读出】）

规范只保证一件事：**state JSON 从 stdin 进来**（`config.md`："The [state](runtime.md#state) of the container MUST be passed to hooks over stdin"）。state 的字段（`runtime.md` §State）：`ociVersion`、`id`（容器 id）、`status`、`pid`、`bundle`（bundle 目录绝对路径）、`annotations`（可选）。**没有 netns 路径字段**——netns 要从 `pid` 自己推。

runc（main `e6d915b`，2026-10-14）实际传的是：

| stage | 代码位置 | `status` | `pid` |
|---|---|---|---|
| `prestart` / `createRuntime` | `libcontainer/process_linux.go:1026-1041` | `StateCreating` | `s.Pid = p.cmd.Process.Pid`（**宿主视角**的容器 init pid） |
| `createContainer` | `libcontainer/rootfs_linux.go:226-231` | `StateCreating` | `s.Pid = unix.Getpid()`（**容器视角**） |
| `startContainer` | `libcontainer/standard_init_linux.go:290-295` | `StateCreated` | `s.Pid = unix.Getpid()` |

所以：

- **`createRuntime`**：hook 在宿主上跑，`state.id` 是容器 id，`state.pid` 是宿主视角的 init pid，`/proc/<pid>/ns/net` 就是容器 netns —— 用 `nsenter -t <pid> -n` / `setns(2)` 进去装规则。
- **`createContainer`**：hook 已经在容器 netns 内跑，**不需要 netns 路径**，直接 `nft` 即可（但此时还没 `pivot_root`，`path` 仍要在宿主/runtime 侧可解析，见 `# 5`）。
- **⚠️ 规范并不保证 `pid` 一定在**：`runtime.md` 写的是 "`pid` (int, **REQUIRED when `status` is `created` or `running`** on Linux)"，而 runc 在 create 阶段传的是 `creating`。runc 恰好填了 `pid`，但这是**实现行为，不是规范保证**——hook 不能把「`pid` 一定有」当成契约。

### 3.3 hook 跑的时候 netns 里到底有什么（【源码读出】）

**规范只保证「netns 存在」**（§3.1 引文），**不保证网卡/IP 已经配好**。两家的时序都不是「先配网再 create」：

**podman**（`ad0b6ca`，2026-10-15）有两条路径：

- **rootful 默认（容器无自己的 userns，`PostConfigureNetNS=false`）**：podman 在 `prepare()` 里就 `createNetNS()` → `configureNetNS()`（netavark），**在 OCI create 之前**就把 netns 建好并配好，然后把 **netns 路径**写进 `config.json`（`libpod/container_internal_linux.go` 的 `addNetworkNamespace()`：`g.AddOrReplaceLinuxNamespace(NetworkNamespace, c.state.NetNS)`）。→ hook 在 `createRuntime` 看到的是**已经配好网卡的 netns**。
- **容器带自己的 userns（`PostConfigureNetNS=true`，rootless/pasta 一类）**：`config.json` 里的 netns 路径是**空串**（同上函数调 `AddOrReplaceLinuxNamespace(NetworkNamespace, "")` → 由 OCI runtime 新建一个空 netns），podman 在 `ociRuntime.CreateContainer()` **之后**才 `completeNetworkSetup()` → `setupNetNS()` → netavark 配置。→ hook 在 `createRuntime` 看到的是**只有 `lo` 的空 netns**。

**docker**（moby `df9adbfaf592`）：只有后一种。`daemon/start_linux.go` 的 `initializeCreatedTask()`（注释："performs any initialization that needs to be done to prepare a **freshly-created** task to be started"）里，先按 `nspath == ""`（**runtime 新建 netns**）从 `/proc/<pid>/ns/net` 取沙箱 key，再 `daemon.allocateNetwork(...)`；调用点在 `daemon/start.go:248`（`initializeCreatedTask`）与 `:252`（`tsk.Start`）——**网络是在 OCI create 之后、start 之前才接上去的**。→ `createRuntime`/`createContainer` 看到的是空 netns；`poststart` 在那之后（有上游用户报告 #51077 说 ≥28 版本里 poststart 时网卡还没出现，与这段时序冲突，**该报告未经确认**）。

**对我们的意义（推断，不是一手事实）**：nftables 规则挂在 netns 上、不挂在网卡上，所以「空 netns 时先装 `policy drop` + allowlist，网卡随后再插」在机制上可行；但**没有一手来源**说明我们与 netavark/libnetwork 的防火墙规则谁先谁后、会不会互相覆盖（见 `# 附`）。

### 3.4 hook 失败会怎样

规范把 create 阶段的失败定死为**中止**（`runtime.md` §Lifecycle）：

- `prestart` / `createRuntime` / `createContainer` / `startContainer`：**"If any ... hook fails, the runtime MUST generate an error, stop the container, and continue the lifecycle at step 12."**（step 12 = 把 create 阶段做的事全部撤销，容器被销毁，不留半成品）
- `poststop`：**"log a warning, but the remaining hooks and lifecycle continue as if the hook had succeeded."**
- `poststart`：⚠️ **规范改过口**——v1.0.2 到 v1.2.0 都是「仅 warning」，**v1.3.0（2025-11-04）起改成「generate an error, stop the container」**。逐版本核对结果：

  | spec 版本 | poststart 失败 |
  |---|---|
  | v1.0.2 / v1.1.0 / v1.2.0 | log a warning，继续 |
  | v1.3.0 / main | generate an error, stop the container |

  来源：<https://raw.githubusercontent.com/opencontainers/runtime-spec/v1.3.0/runtime.md> 与 v1.0.2/v1.1.0/v1.2.0 同名文件。

- podman 自己那两个扩展 stage（【源码读出】，`libpod/container_internal.go`）：`precreate` 失败**中止创建**（`setupOCIHooks` 返回的 error 被包成 `setting up OCI Hooks: %w`）；`poststop` 失败**只 Warnf**。
- runc 侧还有一个可配的 `timeout`（规范字段）：超时按失败处理（`configs.Command.Run`：kill 掉进程并返回 `hook ran past specified timeout of %.1fs`）。

**含义**：`createRuntime` hook 失败 ⇒ **容器根本不会启动**且不留残骸。hook 要设计成「失败即拒绝开工」，而不是「失败就放行」（这是一条事实推论，不是调研结论）。

### 3.5 crun（podman 的默认 runtime）与 runc 的差异

podman 默认用 crun，**所以 runc 的行为不能直接当成 podman 的行为**。【源码读出】crun main `013baa83`（2026-10-14）`src/libcrun/container.c`：

| 项 | runc | crun |
|---|---|---|
| `createRuntime` 的 `status` | `creating` | **`created`**（`do_hooks(..., "created", "createRuntime", ...)`，3037 行） |
| `createContainer` / `startContainer` 的 `status` | `creating` / `created` | `created` / `created`（1386、1690 行） |
| `pid` | 宿主视角 init pid（createRuntime）/ 容器视角（容器内 stage） | 同形（createRuntime 传 `pid`，容器内 stage 传 `getpid()`） |
| state JSON 额外字段 | 无 | **多一个 `root`**（rootfs 路径，884 行）——规范允许（"The state MAY include additional properties"） |
| `env` 未设时 | **继承 runc 进程自己的环境**（`exec.Cmd.Env` 为 nil；Go 文档："If Env is nil, the new process uses the current process's environment."） | **继承 crun 自己的 `environ`**（`do_hooks`：`char **env = environ;`） |
| 失败的 stage | error | 除 `poststop`（`keep_going=true` → warning）外都是 `libcrun_error` + `break` |

⇒ 两条实践含义（推断，非规范）：hook **不要假定 `status` 是 `creating`**；hook **要么自己声明 `env`，要么不要依赖环境变量**——两家给的默认不同。另有 crun 专属扩展 annotation `run.oci.hooks.stdout` / `run.oci.hooks.stderr`（1517、1526 行）可重定向 hook 输出。

## 4. 身份与权限（Q4）

**规范对 uid/gid 一个字都没写。** 它只规定了 hook 在**哪个 namespace** 里执行（§3.1 表），以及 `path` 必须在 **runtime namespace** 里能解析（`config.md`："The value of `path` MUST resolve in the [runtime namespace]"）。所以「hook 以什么身份跑」只能从实现读。

**【源码读出】runc**（`libcontainer/configs/config.go` 的 `Command.Run`）：构造 `exec.Cmd{Path, Args, Env, Stdin, Stdout, Stderr}` 就 `Start()` 了——**没有任何 `Credential`/`SysProcAttr` 设置，也没有 `setns`**。于是 hook 继承**调用它的那个 runc 进程**的身份与 namespace：

| stage | 谁 fork 的 | 效果 |
|---|---|---|
| `createRuntime` / `prestart` / `poststart` / `poststop` | 父进程 runc（宿主） | rootful 下 = **宿主 root，带 `CAP_NET_ADMIN`**；不自动进容器 netns，要自己 `nsenter`/`setns` |
| `createContainer` | 容器 init 子进程，**在 `finalizeNamespace` 之前**（`standard_init_linux.go`：`prepareRootfs`≈91 行 → `finalizeNamespace`≈204 行） | 容器 netns 内、**仍是 root**（`finalizeNamespace` 才 `w.ApplyBoundingSet()` → `setupUser()` → `w.ApplyCaps()`，`libcontainer/init_linux.go:300-366`） |
| `startContainer` | 同一个子进程，**在 `finalizeNamespace` 之后**（≈293 行） | **容器的 uid + 容器被裁剪过的能力集** ⇒ 在 `#16`「容器无 `CAP_NET_ADMIN`」的前提下，**装不了 nftables 规则**，不是可用注入点 |

**与 `#16` 的相容性**：相容。要装 nftables 的是 **hook**（跑在容器进程之外、降权之前，拿宿主 root 的 `CAP_NET_ADMIN`），**容器进程一个能力都不用加**——容器保持默认能力集、无 `CAP_NET_ADMIN`。反过来说，`#3` 决议里那句「容器路由不构成边界——有 `CAP_NET_ADMIN` 就能改写」正是同一条事实的另一面：**能力放在 hook 上，不放在容器里**。

**rootless podman**：【源码读出】**没有**官方文档说明 hook 以什么身份跑。`pkg/hooks/exec/exec.go` 的 `osexec.Cmd` 同样不改 credential，所以 podman 扩展 stage 的 hook 跟着 podman 进程的身份（rootless 下 = 调用者用户）；OCI stage 的 hook 由 crun/runc 执行，按 runc 的读法同样是「不改 credential」——但**crun 的那条路径我没有逐行核对，属推断**。**rootless 下 hook 是否能真的把 nftables 装进容器 netns（取决于 netns 的 userns 归属）没有找到一手依据**——见 `# 附`。另外，rootless 与 hook 加载的关系只有一处源码事实：hooks 目录为空时 rootless 直接 `return nil, nil` 跳过；用非空 `hooks_dir`（即编译默认值）时 rootless 也会加载。

## 5. 安装与生效（Q5）

**`path` 的约定**（规范 + runc，【源码读出】）：

- 必须是**绝对路径**：`config.md` 对每个 stage 都写 "**`path`** (string, REQUIRED) ... This specification extends the IEEE standard in that **`path` MUST be absolute**"。
- **必须在 runtime namespace 里能解析**（即宿主路径，不是容器内路径）——runc 用 `exec.Cmd{Path: c.Path}` 直接 exec，**不做 PATH 查找**。
- ⇒ 二进制（`#19` 的 `stars-sandbox oci-hook` 第二入口）必须**以文件形式存在于宿主上且对 runtime 可执行**。规范**没有规定权限位**，可核实的只有「绝对路径 + 宿主可解析 + 被直接 exec」。
- `args`/`env`/`timeout` 都是规范字段，随 `hooks` 一起写进 `config.json`。

**调用约定（两端一致，【源码读出】runc + containers-common）**：

- **argv 就是配置里写的 `args`**，运行时**不会**追加容器 id（containers-common `pkg/hooks/exec/exec.go`：`Args: hook.Args`；runc：`Args: c.Args`）。容器 id 只从 **stdin 的 state JSON** 的 `id` 字段来。
- **stdin = state JSON**；stdout/stderr 被运行时收集（runc 把它们一起塞进错误信息）。
- `env` 未设时，**runc 与 crun 都是「继承 runtime 进程自己的环境」**（runc 留 `exec.Cmd.Env` 为 nil；Go 的 `os/exec` 文档："If Env is nil, the new process uses the current process's environment."；crun 显式 `char **env = environ;`）。**唯一例外**是 containers-common（podman 扩展 stage）显式设 `cmd.Env = []string{}`。⇒ hook **要么自己声明 `env`，要么不要依赖环境变量**。
- podman 注入的 OCI hook 由 crun/runc 执行，`Dir` = 容器 bundle 目录。

**改完配置什么时候生效**：

- **podman：不需要重启任何东西。** podman 是每次创建容器时读这些目录（【源码读出】`setupOCIHooks` 只在 `generateSpec` 路径上被调用，没有常驻监听），所以改完对**下一个**容器生效。`oci-hooks(5)` 提到的 "Tools consuming this format **may also opt to** monitor the hook directories for changes" 是一般性许可，podman 不需要它。
- **docker：通用面没有 hook 配置可改**，最接近的旋钮（`runtimes`）在 dockerd 参考页里写死了生效方式：**"After changing the runtimes configuration in the configuration file, you must reload or restart the daemon for changes to take effect:"** → `sudo systemctl reload dockerd`；同页 "Configuration reload behavior" 节："Some options can be reconfigured when the daemon is running without requiring to restart the process. The daemon uses the SIGHUP signal in Linux to reload"。**所以首发后端上「装通用 hook」等价于「注册/替换一个 runtime 并 reload dockerd」。**
- **docker 走 CDI 那条窄路时相反：放文件就生效、不用 reload。**【源码读出】CDI 的 `cdi.NewCache` 默认 `autoRefresh: true`（"By default, auto-refresh is enabled, the list of Spec directories are monitored and the Cache is automatically refreshed whenever a change is detected."），而 moby 用的是 `cdi.NewCache(cdi.WithSpecDirs(...))`、没有关掉它（`daemon/cdi.go`）⇒ 往 `/etc/cdi` 放/改 CDI spec 会被自动拾取。
- **别让 hook 对每个容器都触发**：podman 侧靠 `when` 条件收窄（`annotations` / `commands` / `hasBindMounts` / `always`，§2.2）；docker 的 CDI 路线天然只在**请求了对应设备**的容器上触发。

## 附：未能证实 / 未覆盖

- **docker 侧包装 runtime 的具体写法**未查：只确认了 `--add-runtime` / `default-runtime` / `daemon.json.runtimes` 是文档化的三个入口，以及 moby 自己注入 NVIDIA `prestart` hook（§1）的写法可当模板。
- **docker 会不会真的应用 CDI spec 里的 hooks**：链条是源码读出来的（moby `daemon/cdi.go` → CDI `InjectDevices` → `spec.Hooks.*`），**Docker 文档里没有一个字提到 hook**，也没有官方测试；上限未验证。鉴于 `#3` 定的加速器表达正是 CDI 设备名，这条值得实测。
- **rootless podman 下 `createRuntime` hook 能否真的装 nftables**：netns 的 userns 归属没有一个一手来源能回答。
- **podman 默认 hook 目录的三种说法互相矛盾**（§2.1：代码 `/usr/share/...`、`podman(1)` 两个目录、`containers.conf(5)` `/etc/...`）——没有官方结论，**落地前必须在目标机器上实测**。
- **hook 与 netavark（podman）/ libnetwork（docker）防火墙规则的相互作用**（谁先装、会不会互相覆盖）——没有一手来源。
- **docker `poststart` 时网卡是否已就绪**：只有上游用户报告 moby#51077（≥28 表现与 ≤27 不同），与 `daemon/start.go` 的时序读法冲突，**未经确认**。
- **crun 在 userns/rootless 下把 hook 放到哪个 namespace 执行**未核对（crun 有 `check_running_in_user_namespace()` 分支与 deferred-pivot 优化，我只读了调用点，没读进程创建路径）。
- **`$HOME/.config/containers/oci/hooks.d`**：任务书假设里提到过，一手来源里**不存在**这个目录。

## 附：docker 侧的替代注入路线——CDI containerEdits.hooks 是否被执行

本节回答 `# 附` 里那条「**docker 会不会真的应用 CDI spec 里的 hooks**」。取证方式：把 moby 从 `--device` 到 bundle `config.json` 的整条链逐环读源（moby `df9adbfaf592ada44e91e2a3ed06542b4e693cc0`、`tags.cncf.io/container-device-interface` `aa1e2a10408a6c422706eb0150ee66d889858b4f`、docker/cli `dfc520dcc28994c4c699b9b219fd2f5a6bcec0ff`），加一手文档与上游 issue 对照。凡属「文档没写、只能从源码读出来」的结论一律标【源码读出】；没法证实的一律标**未证实**。

### 0. crux 答案

**会。** docker 会执行 CDI spec 里 `containerEdits.hooks` 声明的 hook——**【源码读出】，链上六环全部核对过**（§1）。它和 `env`/`deviceNodes`/`mounts` 走的是**同一个 `Apply()`**，`hooks` 不是被容忍但忽略的字段。

**但这条路在 docker 文档里一个字都没有，也没有任何第一方测试覆盖**（本节 §2.2、§4）：`dockerd` 参考页全文 "hook" 出现 0 次；moby 自己的 CDI 集成测试用的 spec 只有 `env`。所以「文档级确认」这一档是**缺的**，结论建立在源码上，落地上线前建议先跑 §5 那个最小实验。

**这不影响 `# 1`/`# 2` 的结论**：hook 不是 dockerd 执行的，是 **runc/crun** 执行的。docker 只是把 CDI 给的 hook **转发进 `config.json`**；「docker 没有通用 hook 配置面」依然成立——它仍然没有让用户配置任意 hook 的入口，只是 CDI 这条窄路恰好能通到 `spec.Hooks`。

### 1. 证据链：`--device <cdi-name>` → `spec.Hooks.CreateRuntime`

| # | 环节 | 位置 | 内容 |
|---|---|---|---|
| 1 | CLI 认出 CDI 设备名 | docker/cli `cli/command/container/opts.go:493-497` | `if cdi.IsQualifiedName(device) { cdiDeviceNames = append(cdiDeviceNames, device); continue }` |
| 2 | 变成 DeviceRequest | 同上 `:609-614` | `container.DeviceRequest{Driver: "cdi", DeviceIDs: cdiDeviceNames}` 追加进 `deviceRequests`（最终落进 `HostConfig.DeviceRequests`） |
| 3 | daemon 消费 | moby `daemon/oci_linux.go:916-920` | `WithDevices`（`:853`）里 `for _, req := range c.HostConfig.DeviceRequests { daemon.handleDevice(req, s) }` |
| 4 | 选到 cdi driver | moby `daemon/devices.go:69-80` | `else if dd := deviceDrivers[req.Driver]; dd != nil { ... return dd.updateSpec(spec, ...) }` |
| 5 | 调 CDI 库 | moby `daemon/cdi.go:99-112` | `injectCDIDevices(s *specs.Spec, dev)` → `c.registry.InjectDevices(s, cdiDeviceNames...)` |
| 6 | CDI 库写 hooks | CDI `pkg/cdi/cache.go:238-271` → `pkg/cdi/container-edits.go:75-163` → `internal/ociedit/spec_editor.go:214-217` | `e.spec.Hooks.CreateRuntime = append(e.spec.Hooks.CreateRuntime, hook)` |

三处细节值得单独钉住：

- **就地改写、不是拷贝。** `daemon/oci_linux.go:1025` 的 `createSpec` 建 `s := oci.DefaultSpec()`，把 `WithDevices` 列进 `opts`，最后 `coci.ApplyOpts(..., &s, opts...)` 并 `return &s`。`InjectDevices` 拿到的就是这份 `s` 的指针（CDI `pkg/cdi/container-edits.go:75` 注释原文：*"Apply edits to the given OCI Spec. **Updates the OCI Spec in place.**"*）。
- **spec 级与 device 级 edits 都会应用。** `pkg/cdi/cache.go:260-262`：先 `edits.Append(d.GetSpec().edits())`（spec 级，任一设备被请求即应用），再 `edits.Append(d.edits())`（device 级）。⇒ 把 hook 写在 **spec 级** 的 `containerEdits` 里，只要容器请求了该 `kind` 下**任一**设备，hook 就挂上。
- **append 而不是覆盖。** `spec_editor.go:214-217` 是 `append`，CDI 的 hook 排在已有 hook **之后**；`initHooks()`（`:87-90`）只在 `spec.Hooks == nil` 时才新建。普通容器上 docker 不会预先占用 `Hooks.CreateRuntime`（它自己那两处硬编码 hook 是 `Prestart`，见 `# 1`）。

**hook 字段的映射**（CDI `pkg/cdi/oci.go:23-31`，【源码读出】）：`Path`/`Args`/`Env`/`Timeout` 四个字段原样搬进 OCI 的 `spec.Hook`；`hookName` 只用于选 stage，不落进 `config.json`。

**stage 白名单**（CDI `pkg/cdi/container-edits.go:33-58`，【源码读出】）：`prestart`、`createRuntime`、`createContainer`、`startContainer`、`poststart`、`poststop`。注意 CDI 实现**接受 `prestart`**，而 `SPEC.md` 的推荐列表 `{createRuntime, createContainer, startContainer, poststart, poststop}` **不含它**。写别的名字直接 `error`（`container-edits.go:161-162`）。

**spec 的去向**：`daemon/start.go:167` 取 `createSpec` 的返回值，原样交给 `libcontainerd.ReplaceContainer(..., spec, ...)` → containerd `WithSpec(ociSpec)`（moby `daemon/internal/libcontainerd/remote/client.go:119-124`）→ 写成 bundle 的 `config.json`。**之后由 runc/crun 在 create 阶段执行**，权限、时序、失败语义与 `# 3`、`# 4` 完全一致（`createRuntime` = 宿主 root，`createContainer` = 容器 netns 内降权前）。

### 2. 条件、目录与版本

#### 2.1 spec 文件放哪、怎么改、怎么关

| 项 | 值 | 出处 |
|---|---|---|
| 默认目录 | `/etc/cdi`（静态）、`/var/run/cdi`（生成） | CDI `pkg/cdi/spec-dirs.go:29-38` 的 `DefaultStaticDir`/`DefaultDynamicDir`/`DefaultSpecDirs`；moby `daemon/command/daemon.go:751-753` 用 `cdi.DefaultSpecDirs` 填默认值 |
| rootless 追加 | `$XDG_CONFIG_HOME/cdi`、`$XDG_RUNTIME_DIR/cdi` | moby `daemon/command/daemon.go:755-764`（PR moby#51624） |
| 改目录 | `daemon.json` 的 `cdi-spec-dirs`（键名见 `daemon/config/config.go:307-308`）或 `dockerd --cdi-spec-dir` | `daemon/command/daemon.go:751-781` |
| 关闭 | `"features": {"cdi": false}` ⇒ 清空 `CDISpecDirs`；把 `cdi-spec-dirs` 设成 `[""]` 也等于清空 | `daemon/command/daemon.go:782-789` |
| 怎么看生效值 | `docker info` 的 `CDISpecDirs` | moby `integration/container/cdi_test.go:TestCDISpecDirsAreInSystemInfo` |
| 放文件何时生效 | **不用 reload dockerd**：CDI 缓存默认 auto-refresh，moby 没关掉它 | 见上文「5. 安装与生效」末条（【源码读出】） |

#### 2.2 版本要求——**两处一手来源差一个小版本，未判定**

- dockerd 参考页（<https://docs.docker.com/reference/cli/dockerd/>，2026-10-15 取）：*"CDI is currently only supported for Linux containers and **is enabled by default since Docker Engine 28.3.0**."*
- CDI 项目 README（<https://github.com/cncf-tags/container-device-interface/blob/main/README.md>，"Docker and Podman Configuration" 节）：*"Both Docker Daemon and podman support CDI and process specified `--device` flags directly. ... Docker has CDI enabled by default beginning with version **28.2.0**."*（同节：*"Docker supports CDI since version **25.0.0**"*，25.0.0–28.1.1 需 `{"features":{"cdi":true}}`）

**两处一手来源对「默认开启的版本」说法不一致（28.2.0 vs 28.3.0），我没有找到判定依据——落地前用 `docker info` 在目标机上核。** 两者一致的是「自 25.0.0 起支持、25.0.0–28.1.x 需显式开启」。另 moby PR **#49963「daemon: Enable CDI by default」**（merged）是「默认开」的落地提交。

**`hooks` 不是后加的字段**：CDI `SPEC.md` 的 released-versions 表里**没有** `hooks` 的条目（只有 `hostPath`、`annotations`、`intelRdt`、`additionalGids`、`netDevices` 等），`specs-go/version.go` 的 `MinimumRequiredVersion` 也只对这些后加字段做版本门控 ⇒ 【源码读出】`hooks` **自初始 spec 起就在**，不存在「spec 版本太低所以 hook 被忽略」这条路径。moby master 依赖 `tags.cncf.io/container-device-interface v1.1.1`（moby `go.mod:126`）。

### 3. 哪一级、哪个 stage、错了会怎样

- **两级 `containerEdits` 都生效**：spec 级（任一设备被请求）+ device 级（该设备被请求），见 §1。想「放一份 spec 就给所有用某类加速器的容器挂 hook」，写成 **spec 级**。
- **stage 由 `hookName` 决定**，取值见 §1 白名单；**跑 stage 的语义与上文「3. stage 与时序」「4. 身份与权限」一字不差**——hook 由 runc/crun 执行、`path` 必须是宿主绝对路径、`createRuntime` 时 netns 已存在但 docker 的网卡要等 `initializeCreatedTask()` 之后才插（上文 §3.3）。
- **校验在 spec 加载期，不在注入期**：`Hook.Validate`（`container-edits.go:363-373`）只查 `hookName` 在白名单、`path` 非空、`env` 格式合法，**不检查 `path` 在宿主上是否存在**；`ContainerEdits.Validate` 由 `pkg/cdi/spec.go:219-232` 的 `s.validate()` 在 **spec 加载时**调用。⇒ ① 一份坏 spec 会整体加载失败，dockerd 记 `CDI setup error` 警告（moby `daemon/cdi.go:85-93`）；② `path` 指向不存在的文件时，错误**推迟到 runtime 执行期**，表现为容器创建失败（crun 的实际报错形态见下方 CDI issue #239），**不是 hook 静默不跑**——这对 `#16`「绝不静默降级」是好事。

### 4. podman 一侧（Q3）——同一个库，且有实测旁证

- podman vendored 的是**同一个 CDI 库、同一个版本**（`vendor/tags.cncf.io/container-device-interface`，go.mod 也是 `v1.1.1`），并且同样调 `registry.InjectDevices(...)`：
  - `pkg/specgen/generate/config_linux.go:28-40`（`--device` 的 CDI 分支）；
  - `libpod/container_internal_common.go:775-793`（`c.config.CDIDevices` / `GPUs` 路径，注释原文 *"Warning: CDI may alter g.Config in place."*）。
- **一手实测证据（发生在 podman/crun 上）**：CDI 仓库 issue **#239**（*"Error: OCI runtime error: --device nvidia.com/gpu=all podman nvidia container"*，<https://github.com/cncf-tags/container-device-interface/issues/239>）里用户贴出的报错是 crun 抛的：

  > `Error: OCI runtime error: /usr/local/bin/crun: {"msg":"error executing hook \`/usr/bin/nvidia-cdi-hook\` (exit code: 1)","level":"error",...}`

  这直接证明「CDI spec 里声明的 `nvidia-cdi-hook` 被 runtime 执行了」。NVIDIA 的 CDI 生成器确实往 `containerEdits.hooks` 里写 hook（【源码读出】<https://github.com/NVIDIA/nvidia-container-toolkit/blob/main/internal/discover/hooks.go>：`create-symlinks`/`update-ldcache`/`enable-cuda-compat` 等，默认路径常量 `defaultNvidiaCDIHookPath = "/usr/bin/nvidia-cdi-hook"`）。
- **⚠️ 这条旁证在 podman 上，不在 docker 上**：docker 侧我没有找到同等的一手实测报告（即有人在 docker 上看到 CDI 声明的 hook 被 runc 执行，或看到对应报错）——**未证实**，这正是本节 §5 那个实验要补的。

### 5. 最小实测：判定 Q1（给人在目标机上跑，本调研未执行）

**不创建容器、不改宿主配置的替代方案不存在**——Q1 只能实测。以下步骤留给人工执行，**本调研一个字都没跑**。

1. **hook 脚本**（宿主绝对路径、可执行位、`args` 里不写它也要能跑）：

   `/usr/local/libexec/stars-cdi-hook-probe`：
   ```sh
   #!/bin/sh
   # 把 OCI state JSON 原样存到宿主，用来证明 hook 真被 runtime 调用了
   cat > /tmp/stars-cdi-hook-probe.json
   exit 0
   ```
   `chmod +x /usr/local/libexec/stars-cdi-hook-probe`

2. **CDI spec**：`/etc/cdi/stars-sandbox-probe.yaml`
   ```yaml
   cdiVersion: "0.6.0"
   kind: stars.sandbox/probe
   devices:
     - name: hookprobe
       containerEdits:
         env:
           - STARS_PROBE=cdi-works
         hooks:
           - hookName: createRuntime
             path: /usr/local/libexec/stars-cdi-hook-probe
   ```
   （`env` 那条是**对照组**：它对应 moby 自己集成测试断言过的形状，用来区分「CDI 管线没通」和「只有 hook 没被执行」。）

3. **daemon 设置**：**不需要改**（≥28.3 默认开；25.0.0–28.1.x 需在 `daemon.json` 加 `{"features":{"cdi":true}}` 再 `systemctl reload dockerd`）。先确认：`docker info | grep -A3 CDISpecDirs`（应含 `/etc/cdi`）。

4. **单条命令**（不加 `--rm`，留下容器便于复看）：
   ```
   docker run --name stars-cdi-probe --device stars.sandbox/probe=hookprobe \
     --annotation stars.sandbox/probe=1 busybox env
   ```

5. **看什么**：

   | 观察 | 判定 |
   |---|---|
   | `/tmp/stars-cdi-hook-probe.json` **存在** | **Q1 = 会**：docker 执行了 CDI 声明的 hook。文件内容即 stdin 收到的 state JSON（本节 §6） |
   | 文件不存在、但 `STARS_PROBE=cdi-works` 出现在 `env` 输出里 | **Q1 = 不会**：CDI 管线通、但 `containerEdits.hooks` 被忽略 ⇒ 本节 §0 要改口，`# 22` 的候选 1 作废 |
   | 文件不存在、`STARS_PROBE` 也没出现 | CDI 管线本身没通（目录/开关/版本问题），先修这个再判 Q1 |
   | 容器起不来、报 `error executing hook ... /usr/local/libexec/stars-cdi-hook-probe` | hook **被应用、被调用**了，只是脚本失败（查权限位/绝对路径/shell 解释器）——仍算 **Q1 = 会** |

6. **顺带值得看的两件事**：
   - 把 `hookName` 换成 `createContainer`、`startContainer` 各跑一次 ⇒ 验证 stage 分派与「hook 在哪个 namespace 里跑」（对齐上文 §3.2/§4）。
   - `--annotation stars.sandbox/probe=1` 的键值**是否出现在 state JSON 的 `annotations` 里** ⇒ 一次同时验证 `# 19` 那句「per-task 数据经 annotation 进 hook」在 docker 上通不通（上文 §1 末尾那条：`daemon/oci_linux.go:1047` 的 `coci.WithAnnotations`）。

7. **清理**：`docker rm stars-cdi-probe`、删 `/etc/cdi/stars-sandbox-probe.yaml`、删 `/usr/local/libexec/stars-cdi-hook-probe`、删 `/tmp/stars-cdi-hook-probe.json`。

### 6. Q4 复述：`createRuntime` hook 的 stdin 到底有什么（按 runtime-spec 原文）

规范原文（`runtime.md` §State，main `b6f46de8f84eaa7a4d1e747f281a35f5e1b2bfe9`，<https://raw.githubusercontent.com/opencontainers/runtime-spec/main/runtime.md>）：

> * **`ociVersion`** (string, REQUIRED) ...
> * **`id`** (string, REQUIRED) is the container's ID. This MUST be unique across all containers on this host.
> * **`status`** (string, REQUIRED) ...
> * **`pid`** (int, **REQUIRED when `status` is `created` or `running`** on Linux, OPTIONAL on other platforms) ... For hooks executed in the runtime namespace, it is the pid as seen by the runtime. For hooks executed in the container namespace, it is the pid as seen by the container.
> * **`bundle`** (string, REQUIRED) is the absolute path to the container's bundle directory. This is provided so that consumers can find the container's configuration and root filesystem on the host.
> * **`annotations`** (map, **OPTIONAL**) contains the list of annotations associated with the container. If no annotations were provided then this property MAY either be absent or an empty map.

以及 `config.md` 的硬约定：*"The [state](runtime.md#state) of the container MUST be passed to hooks over stdin"*。

逐条落到「per-container 的出网策略怎么送到 hook 里」：

- **`id`（REQUIRED）在。** 容器 id 一定在，是 hook 可用的**唯一必然在场的 per-container 标识**。
- **`pid`**：**规范只保证 `status` 为 `created`/`running` 时必有**，而 create 阶段各家传的 `status` 不一（runc `creating`、**crun `created`**，上文 §3.2/§3.5）——runc/crun **实际上**都填了 `pid`，但**不能当成契约**。
- **`bundle`（REQUIRED）在**，绝对路径，指向含 `config.json` 的目录。
- **`annotations` 是 OPTIONAL**，没传就**可能整个字段缺席**。docker 侧要它出现，得显式带 `docker run --annotation`（上文 §1 末尾）——**「不传就是空/缺」是规范允许的**，hook 必须处理 `annotations` 缺失。
- ⇒ **「per-container 策略怎么到达 hook」目前只有两条规范内的通道**：① `id` → hook 自己去宿主上查（worker 侧存一份 id→策略的表）；② `--annotation` → 直接进 state。两条都不需要容器内配合。哪条作为落地设计**不在本节范围**。

### 7. 本节未能证实 / 需要实测的

- **docker 上没有任何一手实测**证明 CDI hook 被执行（源码链条完整、但无文档、无测试、无上游报告）。**「源码读出」是本节的最高取证强度**，本节 §5 是补这一档的唯一办法。
- **CDI 默认开启的版本号两处一手来源矛盾**（dockerd 文档 28.3.0 vs CDI README 28.2.0），未判定。
- **CDI 的 hook 是否受 `features.cdi` 之外的第二道门控**（例如某些 API 版本下限）未查；只确认了 `--device <限定名>` 这条 CLI 路径。
- **`docker create` / compose / API 直接传 `DeviceRequests`** 三种入口是否与 `docker run --device` 等价：`HostConfig.DeviceRequests` 是同一份数据（moby 集成测试断言的就是它），但 `docker compose` 的 `devices:` 字段走哪条**未查**。
- **CDI hook 与 dockerd 自己那两处硬编码 `Prestart` hook 的实际共存形态**（同一容器上会不会同时出现）未实测；源码上 `Hooks.CreateRuntime` 与 `Hooks.Prestart` 是不同列表，不冲突。
- **`cdi_spec_dirs` 的 podman 默认值**：`containers.conf(5)` 写 `["/etc/cdi", "/var/run/cdi", ...]`（<https://github.com/containers/container-libs/blob/main/common/docs/containers.conf.5.md>），未与 podman 代码逐行核对。
