# OCI hook 的配置面：往容器 netns 注入 nftables 需要的事实

面向 `#16`（注入点定为 OCI hook、v1 必须实现）与 `#19`（hook 与 Worker 是同一个二进制的两个入口）。`#3`/`#16` 定了「用 OCI hook 注入 nftables」，但 `docs/research/sandbox-isolation-and-gpu.md` 与 `#3` 的决议里**没有一个字**讲 hook 怎么配。这份只回答事实与出处，不回答「选哪个」。

> **取证强度**：逐条打开一手页面与一手源码核对（OCI Runtime Spec 的 `config.md`/`runtime.md`、runc 源码、crun 源码、podman 源码、containers-common 的 `oci-hooks(5)`、`dockerd` 参考页、moby 源码），引文保留原文。文档里没写、只能从源码读出来的结论标「**源码读出**」。未能证实的一律留在 `# 附`，不填。

## 0. 先读这三条

1. **OCI 规范里没有「hook 配置放在哪里」这回事。** 规范只定义 `config.json` 里的 `hooks` 对象——一个容器一份，由 runtime caller 生成。「把某个 JSON 文件放进某个目录，于是所有容器都挂上这个 hook」是 **podman/containers-common 在规范之上加的一层**，不是规范的一部分。
2. **docker 没有 OCI hook 配置面。** `daemon.json` 没有对应键，`dockerd` 没有对应 flag（`# 1`：dockerd 参考页全文 "hook" 出现 **0** 次；moby 自己写 hook 只有两处硬编码的 `prestart`，一个是 NVIDIA、一个是 buildkit setkey）。而首发后端正是 docker rootful（`#3`）——所以 `#16` 的注入点在首发后端上**不存在「放一个 hook 配置文件」这种落地方式**，只剩「换 OCI runtime」这一个明写在文档里的旋钮。
3. **hook 的权限来自运行时，不来自容器。** `createRuntime`/`createContainer` 都跑在容器进程**降权之前**（runc 顺序：`prepareRootfs` → `finalizeNamespace`），拿到的是宿主 root 的 `CAP_NET_ADMIN`；容器进程本身**不需要任何额外能力**——与 `#16`「容器保持默认能力集、无 `CAP_NET_ADMIN`」相容（`# 4`）。

## 1. docker：配置面不存在（Q1）

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

**唯一的例外，正好也是我们关心的那一类问题**：moby 自己会注入**一个** OCI hook——NVIDIA 的（`daemon/devices_nvidia_linux.go`，`injectNVIDIARuntimeHook`）：

```go
// This implementation uses prestart hooks, which are deprecated.
// CreateRuntime is the closest equivalent, and executed in the same
// locations as prestart-hooks, but depending on what these hooks do,
// possibly one of the other hooks could be used instead (such as
// CreateContainer or StartContainer).
s.Hooks.Prestart = append(s.Hooks.Prestart, specs.Hook{ //nolint:staticcheck // FIXME(thaJeztah); replace prestart hook with a non-deprecated one.
    Path: path, ...
```

触发条件是**硬编码**的：宿主 `$PATH` 上存在 `nvidia-container-runtime-hook`、且容器用了 `--gpus`（`exec.LookPath(nvidiaContainerRuntimeHookExecutableName)`）。**没有对应的配置键**——这既是「docker 侧 hook 面不存在」的反证，也解释了 nvidia-container-runtime 为什么要以「替换 runtime」的形态存在。另外 NRI（改 OCI spec 的扩展机制）在 moby 里**明确拒绝**加 hook：`daemon/internal/nri/nri.go` 里 `if adj.Hooks != nil { ... unsupported = append(unsupported, "hooks") }`。

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

**结论**：docker 侧没有「hook 配置文件放在哪」这个问题——dockerd 自己生成 `config.json` 并直接调 runc，用户没有往 `hooks` 里塞东西的入口；dockerd 自己写过的 hook 只有两处硬编码的 `prestart` hook（NVIDIA、buildkit setkey）。

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
- **docker：没有 hook 配置可改。** 最接近的旋钮（`runtimes`）在 dockerd 参考页里写死了生效方式：**"After changing the runtimes configuration in the configuration file, you must reload or restart the daemon for changes to take effect:"** → `sudo systemctl reload dockerd`；同页 "Configuration reload behavior" 节："Some options can be reconfigured when the daemon is running without requiring to restart the process. The daemon uses the SIGHUP signal in Linux to reload"。**所以首发后端上「装 hook」等价于「注册/替换一个 runtime 并 reload dockerd」**，不是「放个 JSON 文件」。
- **别让 hook 对每个容器都触发**：podman 侧靠 `when` 条件收窄（`annotations` / `commands` / `hasBindMounts` / `always`，§2.2），否则该目录下的 hook 会挂到这台机器上创建的所有容器上。

## 附：未能证实 / 未覆盖

- **docker 侧包装 runtime 的具体写法**未查：只确认了 `--add-runtime` / `default-runtime` / `daemon.json.runtimes` 是文档化的三个入口，以及 moby 自己注入 NVIDIA `prestart` hook（§1）的写法可当模板。
- **rootless podman 下 `createRuntime` hook 能否真的装 nftables**：netns 的 userns 归属没有一个一手来源能回答。
- **podman 默认 hook 目录的三种说法互相矛盾**（§2.1：代码 `/usr/share/...`、`podman(1)` 两个目录、`containers.conf(5)` `/etc/...`）——没有官方结论，**落地前必须在目标机器上实测**。
- **hook 与 netavark（podman）/ libnetwork（docker）防火墙规则的相互作用**（谁先装、会不会互相覆盖）——没有一手来源。
- **docker `poststart` 时网卡是否已就绪**：只有上游用户报告 moby#51077（≥28 表现与 ≤27 不同），与 `daemon/start.go` 的时序读法冲突，**未经确认**。
- **crun 在 userns/rootless 下把 hook 放到哪个 namespace 执行**未核对（crun 有 `check_running_in_user_namespace()` 分支与 deferred-pivot 优化，我只读了调用点，没读进程创建路径）。
- **`$HOME/.config/containers/oci/hooks.d`**：任务书假设里提到过，一手来源里**不存在**这个目录。
