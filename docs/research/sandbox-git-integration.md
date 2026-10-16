# 沙箱与 git 之间的接缝：一手规范与先例对照

- 调研日期：**2026-10-16**。服务对象：`Workspace ↔ 仓库 = 1:1` 那条既定形状——远端动词在宿主侧代发、沙箱内零凭据、容器里一个 bind mount 进容器的人造通道（UDS 能力通道 + git 侧 shim）。
- 本文回答两个问题：**(线一)** git 自己有没有可用于这条接缝的扩展点，规范怎么写的；**(线二)** 别人（产品 / CI）把仓库送进沙箱、把改动送回去时，接口上交付的是什么。
- **取证方式（重要）**：
  - git 侧：`git-scm.com/docs/*` 页面用 WebFetch 逐页打开（`gitcredentials(7)`、`gitremote-helpers(7)`、`git-credential(1)`、`git-clone(1)`、`git-fetch(1)`、`git-push(1)`、`git-worktree(1)`、`git-bundle(1)`、`git-rev-list(1)`）；**协议细节另用 curl 取官方源文件逐字核对**——`git.git` 仓库 **tag `v2.53.0`** 的 `Documentation/*.adoc` 与 `Documentation/config/*.adoc`（URL 形如 `https://raw.githubusercontent.com/git/git/v2.53.0/Documentation/gitremote-helpers.adoc`）。两者的措辞以源文件为准（浏览器版是同一份源渲染出来的）。
  - 官方源码：同 tag 的 `transport-helper.c`、`builtin/clone.c`。源码读出的结论单独标注。
  - **实测**：本机 git **2.53.0**，`/tmp` 下的临时仓库与一份 85 行的临时 shim（未落地到本仓）。凡实测结论都标 「实测」，并给出实测命令的行为而非猜测。
  - 产品侧：厂商官方文档 / 官方仓库，逐页 WebFetch（其中 GitHub Copilot coding agent 一节由我本人逐字核对；其余产品页由本轮派出的调研代理用同一套 WebFetch 纪律打开，报告里带回原文引用）。**未证实**表示没找到一手来源，并写明查了哪里。
- ⚠️ 表示该先例**与我们的既定形状相抵触**。

## 先读这几条

1. **remote helper 就是我们要 shim 的那个协议，而且它比我们需要的更小。** 我们只需要 `capabilities` + `list` + `fetch` + `push` 四个命令；`import`/`export`/`connect`/`stateless-connect`/`bidi-import` 都是我们**不实现**的那一支（§A.2）。**实测一份 85 行（含日志脚手架）的 Python shim 就完成了 clone / fetch / push 三条路径**——「shim 进镜像」这件事在协议侧没有拦路石，唯一要解决的是分发（见 §E 的第一条【未证实】）。注意这份 shim **没有自己实现 pack 传输**，它把对象搬运交给一个本地裸仓（`git fetch <barcache> <ref>`）——**换句话说：协议侧是薄的，搬运侧复用 git 自己**。
2. **`fetch` 这条路上没有带内错误通道，`push` 有。** 规范里 `push` 明确有 `ok <dst>` / `error <dst> <why>` 两种行；`fetch` 只规定「批处理完成后输出一个空行」，没有任何错误行。宿主侧代发失败时，shim 唯一能做的表达是**往 stderr 写字然后非零退出**（实测：clone 退出码 128，stderr 原文透传到调用者终端）。**能力通道返回的 `{ok, exit_code, stderr_head}` 里，`exit_code` 与 `stderr_head` 在 fetch 方向必须由 shim 自己转写成「死掉」，否则丢信息。**
3. **最危险的一条：shim 的 `list` 返回空 = git 认为「远端是个空仓库」并成功退出。** `list` 只有一种「没有 ref」的表达，与「我连不上远端」不可区分。实测：shim 因内部错误返回空 ref 列表时，`git clone` **退出 0** 并打印「您似乎克隆了一个空仓库。」——一个失败被静默降级成成功。**shim 的唯一正确做法是：能力通道不可用时死掉，绝不返回空列表。**
4. **`--force-with-lease` 的强制语义被 git 委托给了 helper。** 源码里 `option cas <refname>:<sha>` 是 git 发给 helper 的唯一载体（`transport-helper.c` 的 `set_helper_option(transport, "cas", ...)`），而它的返回值**被忽略**。实测：shim 回答 `unsupported` 时 git 照常发 `push`，lease 静默失效——变成一次盲 force。**shim 要么实现 `cas`，要么在 `push` 里自己拒绝带 `+` 的 refspec。**
5. **非快进拒绝发生在客户端，不消耗能力通道。** 实测：客户端按 `list for-push` 报回的远端 tip 自行判定，`push` 命令根本没发给 helper，退出 1 并给出标准的 fetch/rebase 提示。这与 ADR-0002「push 失败要大声失败」相容，而且是**零成本**的大声失败。
6. **空仓库不是失败，但「指定 ref 的空仓库」是。** 官方文档对空仓库**沉默**（`git-clone(1)`/`git-fetch(1)` 全文没有 empty repository 这条）；行为只能从源码与实测取：clone 空仓库**退出 0**、只打一条 warning、不做 checkout（`builtin/clone.c`）；`git ls-remote` 空仓库退出 0 无输出；默认 refspec 的 fetch 退出 0。但 `git fetch origin HEAD` 与 `git clone -b main <空仓>` 都是 **fatal、退出 128**。**「ref 缺席取远端 HEAD」这一步在空仓库上无解，必须由平台自己处理成一种正常情形**——Tekton 的 git-init 给了现成做法（§B.11）。
7. **产品侧只有两条硬结论，但它们正好压在我们形状的两半上**：**`clone` 在平台侧做有先例且是最强的先例**（Codespaces 把 clone 出来的 `/workspaces` 目录 bind mount 进容器，与我们的工作区树逐字同构）；**凭据留在可信侧也有先例**（Coder 的 askpass 回连、Ona 的 runner 持凭据）。但"**零凭据 + 动词通道 + 平台代发**"三件**一起**没有先例（§D.4）。

---

## A. git 一侧的一手扩展点

### A.1 credential helper 协议：我们「凭据只进环境」那条纪律的规范依据

**两个 man 页，两套动词**（这点常在二手资料里被写混）：

- `gitcredentials(7)` 描述**外部 helper 的调用契约**：每个 helper 被追加一个操作参数，取值 `get` / `store` / `erase`——"Return a matching credential, if any exists." / "Store the credential, if applicable to the helper." / "Remove matching credentials, if any, from the helper's storage."。凭据详情走 helper 的 stdin；`get` 时 helper 在 stdout 打印属性列表；`store`/`erase` 时 "the helper's output is ignored"，不支持的操作用 "should silently ignore the request"。见 [gitcredentials(7)](https://git-scm.com/docs/gitcredentials)、[源文件 @v2.53.0](https://raw.githubusercontent.com/git/git/v2.53.0/Documentation/gitcredentials.adoc)。
- `git-credential(1)` 是同一套格式的 plumbing，**它自己的动词是 `fill` / `approve` / `reject` / `capability`**（不是 get/store/erase）。"git-credential takes an "action" option on the command-line (one of `fill`, `approve`, or `reject`) and reads a credential description on stdin"。见 [git-credential(1)](https://git-scm.com/docs/git-credential)。

**输入/输出格式（逐字，`git-credential(1)` → INPUT/OUTPUT FORMAT）**：

- 「The credential is split into a set of named attributes, with one attribute per line. Each attribute is specified by a key-value pair, separated by an `=` (equals) sign, followed by a newline.」
- 「The key may contain any bytes except `=`, newline, or NUL. The value may contain any bytes except newline or NUL. A line, including the trailing newline, may not exceed 65535 bytes...」
- 「In all cases, all bytes are treated as-is (i.e., there is no quoting, and one cannot transmit a value with newline or NUL in it). **The list of attributes is terminated by a blank line or end-of-file.**」
- 已定义属性：`protocol`、`host`（含端口）、`path`、`username`、`password`、`password_expiry_utc`（「When reading credentials from helpers, `git credential fill` ignores expired passwords.」——**这是把租约寿命交给 git 的唯一官方钩子**）、`oauth_refresh_token`、`url`、`authtype`、`credential`、`ephemeral`（「should not be saved by the credential helper because its usefulness is limited in time ... The default value is false.」）、`state[]`、`continue`、`wwwauth[]`、`capability[]`。
- `url=` 是唯一一个会**被展开成多个属性**的键：「the value is parsed as a URL and treated as if its constituent parts were read (e.g., `url=https://example.com` would behave as if `protocol=https` and `host=example.com` had been provided)」，且「specifying a protocol is mandatory」；URL 里没有的部分「will be left unset」。
- 未识别的属性「are silently discarded」——所以版本演进是非破坏性的。
- 能力协商：`capability[]` 必须先于依赖它的值声明；当前两个能力是 `authtype`（解锁 `authtype`/`credential`/`ephemeral`）与 `state`（解锁 `state[]`/`continue`）。`capability` 动作的应答是 `version 0` 开头，**非零退出或首行不是 `version ` 就按「无能力」处理**（向后兼容条款）。

**helper 的配置与"重置"**（这条正是 ADR-0001 宿主侧纪律的那一句）：

- 「If the helper string begins with "!", it is considered a shell snippet... Otherwise, if the helper string begins with an absolute path, the verbatim helper string becomes the command. Otherwise, the string "git credential-" is prepended」；「The resulting command then has an "operation" argument appended to it」。
- 「**If `credential.helper` is configured to the empty string, this resets the helper list to empty**」——空串是"清空"而不是"无操作"，所以宿主侧执行前重置成空串是有效且必要的动作。
- 多个 helper 依次尝试，「no more helpers will be consulted once both username and password had been provided」；helper 输出 `quit=true` 则「no further helpers will be consulted, nor will the user be prompted」。
- 匹配上下文是 URL：`credential.<url>.*`；默认**不**拿 path 当区分条件——「By default, Git does not consider the "path" component of an http URL to be worth matching via external helpers. This means that a credential stored for `https://example.com/foo.git` will also be used for `https://example.com/bar.git`. If you do want to distinguish these cases, set this option to `true`.」（`credential.useHttpPath`）
- **无 helper 时的兜底顺序**：`GIT_ASKPASS` → `core.askPass` → `SSH_ASKPASS` → 终端提示。`GIT_ASKPASS`「will call this program with a suitable prompt as command-line argument and read the password from its STDOUT」；`GIT_TERMINAL_PROMPT`「If this Boolean environment variable is set to false, git will not prompt on the terminal」。见 [git(1) ENVIRONMENT](https://git-scm.com/docs/git) 与 `gitcredentials(7)`。

**对我们的意义**：credential helper 的全部价值在于「**让 git 用我们的凭据去直连远端**」。我们的形状恰好相反——git 根本不直连。所以 credential helper 在本设计里**没有位置**（除了宿主侧代发进程内部那一条：重置 `credential.helper` 防止 `store` 把租约写进 `~/.git-credentials`，见 §D.1）。

### A.2 remote helper 协议：shim 的规格依据（`gitremote-helpers(7)`）

来源：<https://git-scm.com/docs/gitremote-helpers> 与 [源文件 @v2.53.0](https://raw.githubusercontent.com/git/git/v2.53.0/Documentation/gitremote-helpers.adoc)；行为细节另经 `transport-helper.c`（同 tag）与实测核对。

**调用方式**

- 「Remote helper programs are invoked with one or (optionally) two arguments.」`git remote-<transport> <repository> [<URL>]`。
- 「A URL of the form *<transport>::<address>* explicitly instructs Git to invoke *git remote-<transport>* with *<address>* as the second argument.」——`stars::ws-123` 这种 URL 就是我们让 sandbox 里的 git 找到 shim 的方式（不需要 `://`）。
- 「The `GIT_DIR` environment variable is set up for the remote helper」——helper 知道往哪个仓库写对象（实测：`GIT_DIR=/tmp/shimtest/client/.git`；`GIT_WORK_TREE` **未**设置）。
- 协议策略闸门：未知协议默认策略是 `user`（"protocol is only able to be used when `GIT_PROTOCOL_FROM_USER` is either unset or has a value of 1"）；**如果 shim 要被 submodule 递归这类无用户输入的路径用到，必须显式 `protocol.<name>.allow=always`**——`url.<base>.insteadOf` 的文档原文就是这么说的：「protocols you expect to use for submodules must be set to `always` rather than the default of `user`」。见 [protocol.allow](https://raw.githubusercontent.com/git/git/v2.53.0/Documentation/config/protocol.adoc)、[url.insteadOf](https://raw.githubusercontent.com/git/git/v2.53.0/Documentation/config/url.adoc)、[GIT_ALLOW_PROTOCOL](https://raw.githubusercontent.com/git/git/v2.53.0/Documentation/git.adoc)（「behave as if `protocol.allow` is set to `never`, and each of the listed protocols has `protocol.<name>.allow` set to `always`」）。

**命令流（INPUT FORMAT）**

- 「Git sends the remote helper a list of commands on standard input, one per line. **The first command is always the `capabilities` command**, in response to which the remote helper must print a list of the capabilities it supports (see below) followed by a blank line. The response to the capabilities command determines what commands Git uses in the remainder of the command stream.」
- 「The command stream is terminated by a blank line.」

**能力协商（CAPABILITIES）**：我们只关心四行——`fetch`（「Can discover remote refs and transfer objects reachable from them to the local object store. Supported commands: *list*, *fetch*.」）、`push`（「...Supported commands: *list for-push*, *push*.」）、`option`（「For specifying settings like `verbosity` ... and `depth` ... that affect how other commands are carried out.」），可选 `check-connectivity`。我们**不**声明 `connect`/`stateless-connect`（要求全双工、等于把真 git 协议透出去）、**不**声明 `import`/`export`/`bidi-import`（那是外部数据接口，走 fast-import 流）、**不**声明 `refspec`（私有命名空间映射，只有 import/export 需要）。同页还说：`connect` 优先、但 helper 可以 `fallback`；`push` 优先于 `export`；`fetch` 优先于 `import`。

**`capabilities` 命令本身**：「Lists the capabilities of the helper, one per line, ending with a blank line. Each capability may be preceded with `*`, which marks them mandatory for Git versions using the remote helper to understand. Any unknown mandatory capability is a fatal error. **Support for this command is mandatory.**」

**`list` / `list for-push`**

- 「Lists the refs, one per line, in the format "<value> <name> [<attr> ...]". The value may be a hex sha1 hash, **"@<dest>" for a symref**, ":<keyword> <value>" for a key-value pair, or **"?" to indicate that the helper could not get the value of the ref**. A space-separated list of attributes follows the name; unrecognized attributes are ignored. The list ends with a blank line.」
- `list for-push` 与 `list` 格式相同，**只在准备 push 时使用**——一个同时支持 fetch/push 的 helper 用它区分两种用途。实测：clone/fetch 收到的是 `list`，push 收到的是 `list for-push`。
- 已知的 ref 属性只有 `unchanged`；已知的 key-value 只有 `object-format`。
- ⚠️ **「空列表」与「连不上」不可区分**：`list` 结束于空行，没有错误形式。实测：列表为空时 clone 退出 0（判为「空仓库」）。

**`fetch <sha1> <name>`**

- 「Fetches the given object, writing the necessary objects to the database. **Fetch commands are sent in a batch, one per line, terminated with a blank line. Outputs a single blank line when all fetch commands in the same batch are complete.** Only objects which were reported in the output of `list` with a sha1 may be fetched this way.」
- 可选输出 `lock <file>`（`$GIT_DIR/objects/pack` 下、以 `.keep` 结尾的路径，防并发 repack）；若 git 打开过 `check-connectivity`，helper 必须输出 `connectivity-ok`。
- **没有任何错误行**。`transport-helper.c`（源码）里的读循环是：`recvline` 失败 → `exit(128)`；不认识的行 → `warning("%s unexpectedly said: '%s'")` 后继续。也就是说 fetch 阶段「多说一句」不会失败，「不说」才是失败。
- 实测命令序列（clone，缓存里有一个 commit）：`capabilities` → `option progress false` → `option verbosity 1` → `list` → `option cloning true` → `fetch <sha> refs/heads/main` + 空行 → 空行 → 命令流结束。增量 fetch 多一个 `option followtags true`，`fetch` 的 name 仍是远端 ref 名、sha 是 `list` 报的新 tip。

**`push [+]<src>:<dst>`**

- 「A batch sequence of one or more `push` commands is terminated with a blank line... Zero or more protocol options may be entered after the last `push` command, before the batch's terminating blank line.」
- 「When the push is complete, **outputs one or more `ok <dst>` or `error <dst> <why>?` lines** to indicate success or failure of each pushed ref. The status report output is terminated by a blank line. The option field `<why>` may be quoted in a C style string if it contains an LF.」
- 源码读出的形状（`transport-helper.c:1020-1090`）：`push ` + 可选 `+` + src（**删除时 src 为空，即 `push :refs/heads/x`**）+ `:` + dst；`+` 只在 `ref->force` 时出现。`--force-with-lease` **不**变成 `+`，而是批次前的一行 `option cas <refname>:<sha>`。实测这三条都出现：`option cas refs/heads/main:<sha>` 在 `push` 之前发出。
- 实测：helper 回 `error refs/heads/main <今天线>`,git 打印 `! [remote rejected] main -> main (<why>)` 并退出 1——**`<why>` 原样出现在用户面前**，这就是把宿主侧的 `stderr_head` 送达 agent 的正式通道。

**`option <name> <value>`**

- 「Outputs a single line containing one of `ok` (option successfully set), `unsupported` (option not recognized) or `error <msg>` (option `<name>` is supported but `<value>` is not valid for it). Options should be set before other commands...」
- 规范里定义的全部 option：`verbosity`、`progress`、`depth`、`deepen-since`、`deepen-not`、`deepen-relative`、`followtags`、`dry-run`、`servpath`、`check-connectivity`、`force`、`cloning`、`update-shallow`、`pushcert`、`push-option`、`from-promisor`、`no-dependents`、`atomic`、`object-format`（**没有 `filter`**，也没有 `cas`——`cas` 只存在于源码里，见下）。
- **哪些 option 不实现会死**（源码 `transport-helper.c:969-1001`）：`dry-run`、`--signed`（`pushcert`）、`--atomic`、`--force-if-includes`、`push-option`——不支持就 `die("helper %s does not support ...")`。而 `progress` / `verbosity` / `cloning` / `followtags` / `depth` 系列被忽略也无妨（返回码不检查）。
- **`cas` 是唯一「静默」的必需项**：`set_helper_option(transport, "cas", ...)` 的返回值在 `push_refs_with_push()` 里被丢弃，helper 回 `unsupported` 时 git 照推（实测复现）。规范页里查不到 `cas`，**它只存在于源码**。

**helper 进程的失败语义（源码 + 实测）**

- 源码：`die("remote helper '%s' aborted session", data->name)`（读到一半 helper 死了）、`die(_("expected ok/error, helper said '%s'"), buf->buf)`（push 状态行既不是 `ok` 也不是 `error`）。
- 实测：helper 在 fetch 阶段非零退出 → `git clone` 退出 **128**，helper 写到 stderr 的文字**原样出现在调用者的终端**（我们的模拟文案一字不差地打了出来）。**stderr 是唯一能把「能力通道为什么失败」讲给人听的地方。**

**实测结论汇总（这份 shim 只实现了 `fetch`/`push`/`option` 三个 capability；对象搬运委托给一个本地裸仓，即"宿主侧裸仓"的微缩版）**

| 场景 | 结果 |
|---|---|
| clone 一个有 commit 的仓库 | 退出 0，工作树正确检出（实测：`list` 未输出 `@` symref 行时，git 仍把 `HEAD` 指到了唯一的分支 `refs/heads/main`） |
| clone 一个空仓库 | 退出 0 + 「您似乎克隆了一个空仓库。」警告，无 checkout |
| 增量 fetch（远端有新 commit） | 退出 0，`origin/main` 前进 |
| push 快进 | 退出 0，git 打印 `d1f53bb..2dca659 main -> main` |
| push 非快进（无 lease） | **helper 未被调用**，`! [rejected] main -> main (non-fast-forward)`，退出 1 |
| helper 对 `option cas` 回 `unsupported` + push | lease 静默失效，照推（危险） |
| helper 返回空 ref 列表以掩盖内部错误 | clone 退出 **0**（把失败说成了成功，最危险） |
| helper 在 fetch 阶段 exit 1 | clone 退出 **128**，stderr 透传 |

### A.3 宿主侧代发要用到的环境与配置语义

| 东西 | 官方原话（或出处） | 对我们的用法 |
|---|---|---|
| `GIT_CONFIG_COUNT` / `GIT_CONFIG_KEY_<n>` / `GIT_CONFIG_VALUE_<n>` | 「If GIT_CONFIG_COUNT is set to a positive number, all environment pairs ... up to that number will be added to the process's runtime configuration. The config pairs are zero-indexed. Any missing key or value is treated as an error. ... These environment variables will override values in configuration files, but will be overridden by any explicit options passed via `git -c`.」——[git-config(1) @v2.53.0](https://raw.githubusercontent.com/git/git/v2.53.0/Documentation/git-config.adoc) | 宿主侧把**凭据塞进进程环境**的正式手段（例：`credential.helper` 清空 + 一个一次性 helper 路径） |
| `-c <name>=<value>` / `--config-env=<name>=<envvar>` | 「This is useful for cases where you want to pass transitory configuration options to git, but are doing so on operating systems where other processes might be able to read your command line」——[git(1) @v2.53.0](https://raw.githubusercontent.com/git/git/v2.53.0/Documentation/git.adoc) | **git 自己写下的"argv 会被同机其他进程读到"的理由**，ADR-0001 那条「绝不进 argv」的规范依据 |
| `GIT_ASKPASS` / `core.askPass` / `GIT_TERMINAL_PROMPT` | 见 §A.1 | 宿主侧若走 HTTP 代发，这三条必须显式堵死（`GIT_TERMINAL_PROMPT=false`） |
| `GIT_SSL_CAINFO` | `http.sslCAInfo`：「File containing the certificates to verify the peer with when fetching or pushing over HTTPS. Can be overridden by the `GIT_SSL_CAINFO` environment variable.」——[config/http.adoc](https://raw.githubusercontent.com/git/git/v2.53.0/Documentation/config/http.adoc) | 「Git 实例 = 地址 + 证书信任锚」那条的落地钩子 |
| `http.extraHeader` | 「Pass an additional HTTP header when communicating with a server. If more than one such entry exists, all of them are added as extra headers. To allow overriding the settings inherited from the system config, an empty value will reset the extra headers to the empty list.」 | 与 `GIT_CONFIG_*` 组合可以做到"凭据不进 argv"，但**它会进 `.git/config` 如果被写进配置**——只能靠环境注入 |
| `url.<base>.insteadOf` / `pushInsteadOf` | 「Any URL that starts with this value will be rewritten to start, instead of, with <base>. ... When more than one insteadOf strings match a given URL, the longest match is used.」 | 把真实远端 URL 在**沙箱内**改写成 `stars::<workspace>` 的标准做法；注意上面那条 submodule / `protocol.allow=always` 的附带要求 |
| `protocol.<name>.allow` / `GIT_ALLOW_PROTOCOL` | 见 §A.2 | 沙箱内可以收窄到只允许 `stars`（本仓的 `git-short-lived-credentials.md` 已覆盖供应商侧，不再重复） |
| `GIT_ALTERNATE_OBJECT_DIRECTORIES` | 「This variable specifies a ":" separated list of Git object directories which can be used to search for Git objects. **New objects will not be written to these directories.**」——[git(1)](https://raw.githubusercontent.com/git/git/v2.53.0/Documentation/git.adoc) | 宿主侧裸仓镜像的只读共享面 |
| `safe.directory` / `safe.bareRepository` | 「By default, Git will refuse to even parse a Git config of a repository owned by someone else, let alone run its hooks」；`safe.bareRepository=explicit` 「will protect you from attacks that involve cloning a repository that contains a bare repository and running a Git command within that directory」——[config/safe.adoc](https://raw.githubusercontent.com/git/git/v2.53.0/Documentation/config/safe.adoc) | 宿主上 Worker 以某个 uid 操作他人的 workspace 目录时会撞上；**裸仓镜像若落进 workspace 树内会被 `explicit` 挡** |

### A.4 搬运、缓存与浅历史：与「宿主侧代发 + 冷档留树」相关的一手语义

- **`--reference` / `--dissociate`（裸仓镜像的正式做法）**：「If the reference _<repository>_ is on the local machine, automatically setup `.git/objects/info/alternates` to obtain objects from the reference _<repository>_. Using an already existing repository as an alternate will require fewer objects to be copied from the repository being cloned, reducing network and local storage costs.」；「`--dissociate`: Borrow the objects from reference repositories specified with the `--reference` options **only to reduce network transfer, and stop borrowing from them after a clone is made** by making necessary local copies of borrowed objects.」——[git-clone(1)](https://git-scm.com/docs/git-clone)。
  - ⚠️ **同一页 `--shared` 的 NOTE 是这本账的代价，必须一起读**：「this is a possibly dangerous operation; do **not** use it unless you understand what it does. If you clone your repository using this option and then delete branches (or use any other Git command that makes any existing commit unreferenced) in the source repository, some objects may become unreferenced (or dangling). These objects may be removed by normal Git operations (such as `git commit`) which automatically call `git maintenance run --auto`. ... If these objects are removed and were referenced by the cloned repository, **then the cloned repository will become corrupt**.」
  - 推论见 §D.3：镜像要么**永不被 gc/repack 掉对象**，要么 clone 时带 `--dissociate` 把对象复制进 workspace。
- **partial clone / promisor**：`clone --filter=blob:none`「will filter out all blobs (file contents) until needed by Git」；filter 语法在 `git-rev-list(1)`（[页](https://git-scm.com/docs/git-rev-list)）：`blob:none`、`blob:limit=<n>`、`object:type=<t>`、`tree:<depth>`、`sparse:oid=<blob-ish>`、`combine:...`；`--filter=sparse:path=<path>`「has been dropped for security reasons」。缺对象时的行为由 `--missing=error|allow-any|allow-promisor|print` 决定（默认 `error`）。
  - **服务端侧的新语义（v2.53.0 有、多数二手资料没有）**：服务端可在 protocol v2 的 `promisor-remote` 能力里**广告**它认可的 promisor remote，而客户端默认 **`acceptFromServer=none`**（「no promisor remote advertised by a server will be accepted」）。见 [config/promisor.adoc](https://raw.githubusercontent.com/git/git/v2.53.0/Documentation/config/promisor.adoc)。
  - ⚠️ 对我们是**负向**的：promisor/partial clone 的收益来自「延迟拉 blob」，而 blob 的按需拉取意味着**运行期要能再连远端**。我们的形状里连远端这件事要过能力通道，一次操作一次租约、都可能失败——**partial clone 会让 agent 在 `git diff` 这种本地动词上撞到网络**。ADR-0003 已经写过这句（「每次沙箱内 git fetch / push 都要一次控制层往返」），这里补上它的技术根因。
- **浅历史**：`--depth=<n>`「Create a shallow clone with a history truncated to the specified number of commits. Implies `--single-branch` unless `--no-single-branch` is given」；`--shallow-since` / `--shallow-exclude`。浅克隆**没有**反向的诚实条款（`git fetch --unshallow` 才能补全）。
- **`git worktree`（共享对象库、多检出）**：「This new worktree is called a "linked worktree" ... linked to the current repository, sharing everything except per-worktree files such as `HEAD`, `index`, etc.」；同一分支不能在两处检出（「if it's not checked out anywhere else, otherwise the command will refuse to create the worktree」）；`$GIT_COMMON_DIR` 指回主仓。⚠️ 同页 BUGS：「Multiple checkout in general is still experimental, and the support for submodules is incomplete. It is NOT recommended to make multiple checkouts of a superproject.」——见 [git-worktree(1)](https://git-scm.com/docs/git-worktree)。
- **bundle**：「Git commands that fetch or otherwise "read" via protocols such as `ssh://` and `https://` can also operate on bundle files. It is possible git-clone a new repository from a bundle, to use git-fetch to fetch from one... **There's no corresponding "write" support, i.e. a `git push` into a bundle is not supported.**」；bundle 需要显式 ref 名（「Refusing to create empty bundle」）。见 [git-bundle(1)](https://git-scm.com/docs/git-bundle)。
- **bundle-uri 协议**（我按"是否比宿主裸仓更适合我们"读的）：它是**在真正 fetch 之前**用 bundle 预热对象库的加速机制——「Bundle URIs are locations where Git can download one or more bundles in order to bootstrap the object database **in advance of fetching the remaining objects from a remote**」；`git clone --bundle-uri=<uri>`「Before fetching from the remote, fetch a bundle from the given _<uri>_ and unbundle the data into the local repository. The refs in the bundle will be stored under the hidden `refs/bundle/*` namespace. This option is incompatible with `--depth`...」；默认关闭（`transfer.bundleURI`「Defaults to `false`」）。服务端广告走 protocol v2 能力，bundle 列表本身是 git config 格式的文本。见 [technical/bundle-uri.adoc](https://raw.githubusercontent.com/git/git/v2.53.0/Documentation/technical/bundle-uri.adoc)、[git-clone(1)](https://git-scm.com/docs/git-clone)、[config/transfer.adoc](https://raw.githubusercontent.com/git/git/v2.53.0/Documentation/config/transfer.adoc)。
  - **它替不掉宿主裸仓**：bundle-uri 的语义是「省一次网络传输」，最终仍要 fetch 真实远端；而我们的问题是「**远端不可达时也要有一个可 fetch 的对象源**」。结论见 §D.3。
- **`fetch --prune`**：「Before fetching, remove any remote-tracking references that no longer exist on the remote. Tags are not subject to pruning if they are fetched only because of the default tag auto-following or due to a `--tags` option.」；`--prune-tags` 才是「remove any local tags that no longer exist on the remote if `--prune` is enabled」，且「unlike `--prune` it will remove any local references (local tags) that have been created」——**代发路径上默认别带 `--prune-tags`**。见 [git-fetch(1)](https://git-scm.com/docs/git-fetch)。
- **push 的 refspec与幂等**：`+`=force；缺 `<src>`（`git push origin :dev`）＝删除；`<src>` 不带 `:dst` 「means to update the same ref as the <src>」；`*` glob 只有一个；`^` 负向 refspec。非快进：「Usually, `git push` will refuse to update a branch that is not an ancestor of the commit being pushed.」；「You can perform "git pull", resolve potential conflicts, and "git push" the result.」`push.default` 默认 `simple`（「push the current branch with the same name on the remote. ... This mode is the default since Git 2.0」）。见 [git-push(1)](https://git-scm.com/docs/git-push)。
- **`--force-with-lease`**：「`--force-with-lease` alone, without specifying the details, will protect all remote refs that are going to be updated by requiring their current value to be the same as the remote-tracking branch we have for them.」/「`--force-with-lease=<refname>:<expect>` will protect <refname> (alone) ... requiring its current value to be the same as the specified value _<expect>_ (which is allowed to be different from the remote-tracking branch we have for the refname, **or we do not even have to have such a remote-tracking branch when this form is used**)」；「**all forms other than `--force-with-lease=<refname>:<expect>` ... are still experimental**」；「supplying this option without an expected value ... interacts very badly with anything that implicitly runs `git fetch` ... in the background」。**实测**：无 remote-tracking ref 时裸 `--force-with-lease` → `! [rejected] ... (stale info)`、退出 1；`=main:<sha>` 形式不需要该 ref。
  - 对宿主侧「一次操作一次」的代发：**只有 `=refname:<expect>` 形式成立**（我们没有"上一次 fetch 的 remote-tracking ref"这种东西作为前提），而 `<expect>` 必须由**控制层**提供——git 自己的 remote-tracking 是客户端概念，跨"宿主代发"不存在。
- **空仓库 / 无 HEAD 的行为（文档沉默，源码 + 实测）**：
  - `builtin/clone.c:1559`（v2.53.0）：`warning(_("You appear to have cloned an empty repository.")); option_no_checkout = 1;` —— 是 `warning`，不是 `die`。
  - 实测（git 2.53.0）：clone 空仓 → 退出 **0**、无 checkout、`HEAD` 指向远端未出生的分支（`refs/heads/main`）、`git log` 报「尚无任何提交」；`git ls-remote` 空仓 → 退出 0、无输出；默认 refspec 的 `git fetch origin` → 退出 0；`git fetch origin HEAD` → `fatal: 无法找到远程引用 HEAD`、退出 **128**；`git clone -b main <空仓>` → `fatal: 远程分支 main 在上游 origin 未找到`、退出 **128**。
  - 结论：**「空仓库」这件事的失败与否，取决于我们有没有指定 ref**。平台动词必须把「远端没有任何 ref」当成一个正常的一等情形处理（ADR 里那句「空仓库不是失败」在 git 层面只对"不指定 ref"成立）。

---

## B. 产品怎么做这条接缝

> 每个小节：**怎么做的** / **该抄** / **不该抄**。⚠️ 标与既定形状相抵触的先例。所有页面均为逐页打开的一手文档。

### B.1 GitHub Codespaces（平台侧 clone + 目录 bind mount 进容器：与我们的形状最接近的先例）

**怎么做的**
- **clone 在平台侧、容器创建之前**：「your repository is **cloned onto the host VM before the container is created**.」「A shallow clone is then made of your repository」；落在 `/workspaces`，而「This is a persistent directory that is **mounted into the container**.」（[deep-dive](https://docs.github.com/en/codespaces/about-codespaces/deep-dive)）
- **凭据形态 = `GITHUB_TOKEN` 环境变量 + git credential helper**：「GitHub Codespaces uses the HTTPS protocol by default, and authenticates with a `GITHUB_TOKEN`」「configured with read and write access to the repository from which you created the codespace」；「Git's credential helper will try to use the existing `GITHUB_TOKEN` to authenticate」；「By default, the token does not have access to other repositories.」（[troubleshooting-auth](https://docs.github.com/en/codespaces/troubleshooting/troubleshooting-authentication-to-a-repository)）
- **token 寿命**：「Every time a codespace is created or restarted, it's assigned a new GitHub token with an automatic expiry period.」——只承诺「a typical working day」；写权限继承你对仓库的权限（只读用户拿到的是「will only allow the code to be cloned」（[security](https://docs.github.com/en/codespaces/reference/security-in-github-codespaces)）。附加仓库权限在 `devcontainer.json` 的 `customizations.codespaces.repositories` 里声明（[managing-repository-access](https://docs.github.com/en/codespaces/managing-your-codespaces/managing-repository-access-for-your-codespaces)）
- **生命周期钩子的分层回答了"哪一步能碰到用户的秘密"**：`initializeCommand`「to run on the host machine during initialization」「For cloud services, this is in the cloud」；`onCreateCommand`「Cloud services can use this command when caching or prebuilding a container.」「it will not typically have access to user-scoped assets or secrets.」；`postCreateCommand`「Cloud services can use this command to take advantage of user specific secrets and permissions.」（[devcontainer JSON reference](https://containers.dev/implementors/json_reference/)）；prebuild 里只跑到 `updateContentCommand`，「`postCreateCommand` commands are [not] run during the creation of a prebuild」（[prebuilds](https://docs.github.com/en/codespaces/prebuilding-your-codespaces/about-github-codespaces-prebuilds)）
- **持久化边界**：`/workspaces` 里的改动「are preserved over a rebuild」，之外「are cleared」；「A codespace has a maximum lifetime of 12 hours」、默认 30 分钟空闲停机、30 天删除（[lifecycle](https://docs.github.com/en/codespaces/about-codespaces/understanding-the-codespace-lifecycle)）
- **push 由工作区里的人做**：平台侧的动作只有"自动 fork + 把 token 升级成 fork 的读写"（[using-source-control](https://docs.github.com/en/codespaces/developing-in-a-codespace/using-source-control-in-your-codespace)）

**该抄**
- ⭐⭐ **`/workspaces` + 「cloned before the container is created」+「a persistent directory that is mounted into the container」= 我们的工作区树**（宿主上那一份、唯一活着的那份、bind mount 进沙箱的那份）。**"clone 是平台动作"在这条一手文档里是事实，不是我们的发明。**
- ⭐⭐ **"rebuild 只清 `/workspaces` 以外"**：这就是"冷档只保树"的同构表达——**树是资产，树以外是耗材**。
- ⭐ **钩子按"能不能碰用户秘密"分层**（prebuild 拿不到用户秘密，`postCreateCommand` 才能）：我们把凭据只给 Worker 时，用的是同一把尺子，值得抄它的**分层方式**而不是它的钩子名。

**不该抄**
- ⚠️ **`GITHUB_TOKEN` + credential helper 进容器**：凭据在不可信侧，且 helper 是一份"能打印消息"的程序。
- ⚠️ **默认浅克隆**（`--depth`）：与 `--bundle-uri` 不兼容，且历史相关的本地动词会受限。
- ⚠️ **12 小时 / 30 天这类写死的寿命**：保留期归控制层裁决，不该是沙箱实现层的常数。

### B.2 Gitpod / Ona（凭据留在 runner，环境里的 git 操作按环境所有者身份走）

**怎么做的**（`https://www.gitpod.io/docs` 308 → `https://ona.com/docs`）
- **clone 由 runner 做、用开发者自己的凭据**：「When a developer or agent starts an environment, **the runner uses the developer's credentials to clone the repository**.」「Ona clones the repository into a container and provisions a development environment.」（[source-control overview](https://ona.com/docs/ona/source-control/overview)、[quickstart](https://ona.com/docs/ona/quickstart)）
- **凭据不离开 runner**：「keeps source control credentials on runners」「They never reach the management plane.」；「All subsequent git operations (commits, pushes, fetches) use the same credentials within that environment.」（同上）
- **prebuild 发布前清凭据**：「the executor's managed SCM credential, environment token, and Git identity are cleared」（[prebuilds](https://ona.com/docs/ona/projects/prebuilds.md)）
- **scope 是全量 `repo`**：「The requested scopes are *repo*, *read:user*, *user:email*, and *workflow*.」；「Ona requests broad GitHub permissions to enable environment creation from any of your repositories.」「Scoped repository access (granting access to specific repos only) is on the roadmap.」（[source-control/github](https://ona.com/docs/ona/source-control/github.md)）
- **agent 与人同一身份**：「Agents inherit your permissions.」；关掉 SCM 工具后「agents can still clone, commit, and push code using git」（同 overview）
- 持久化：「Everything on disk persists across environment stops and starts.」（[persistent-storage](https://ona.com/docs/ona/environments/persistent-storage.md)）

**该抄**
- ⭐⭐ **"凭据留在 runner、环境按所有者身份动它"**是最接近"能力通道"的立场：凭据在可信侧，动作由执行侧发起。**它是继 Coder 之后第二个把凭据放在执行边界之外的先例。**
- ⭐ **prebuild 发布前清凭据**：任何"环境产物"在被发布/落盘前都必须清凭据——**我们的冷档与将来的镜像缓存同问**。
- ⭐ **agent 与人同身份**（不做二级身份）：与"沙箱就是信任单元"一致。

**不该抄**
- ⚠️ **`repo` 全量 scope**：我们要仓库级 × 操作级。
- ⚠️ **「All subsequent git operations ... use the same credentials within that environment」**：把一份凭据按环境寿命复用，正是"一次操作一次租约"要拆掉的。
- 【未证实】环境里的 git 操作**是否真的经 runner 中转**——文档只说凭据留在 runner。这一条决定了它是"我们的形状"还是"只借了凭据的位置"，见 §E。

### B.3 Coder（沙箱内 helper 回连控制面按需取凭据）

**怎么做的**
- **不 clone、不 push、不开 PR**：官方 git-clone 模块「allows you to automatically clone a repository by URL and skip if it exists in the base directory provided」，而它是在 **workspace 内部的 agent 启动脚本**里跑的（示例 `count = data.coder_workspace.me.start_count`、`agent_id = coder_agent.example.id`）。模板文档给的自动 clone 也是 HCL `startup_script` 里一句 `git clone`，外加 `if [ ! -d ... ]` 幂等守卫（[git-clone 模块 README](https://raw.githubusercontent.com/coder/modules/main/git-clone/README.md)、[open-in-coder](https://coder.com/docs/admin/templates/open-in-coder)）。
- **凭据形态：沙箱里放一个 askpass，按需回连控制层取 token**——「For Git operations over HTTPS, Coder automatically injects an external auth provider token」经「Git's `GIT_ASKPASS` mechanism, which Coder configures in each workspace」；「Sets up `GIT_ASKPASS` at `/tmp/coder.<random-string>/coder` in your workspaces」「Retrieves and injects the appropriate token when Git operations require authentication.」；SSH 侧是「Coder automatically generates an SSH key pair for each user」「The `coder gitssh` command wraps the standard `ssh` command and injects the SSH key during Git operations.」（[external-auth](https://coder.com/docs/admin/external-auth)）
- 模板必须先声明 provider（`data "coder_external_auth" ...`），Coder「considers only the providers that the workspace's template declares」。
- ⚠️ `coder gitssh` 只在 external-auth 页出现，**CLI reference 里没有这个条目**（[CLI reference](https://coder.com/docs/reference/cli) 只有 `publickey` 与 `external-auth`；`/docs/reference/cli/gitssh` 404）。

**该抄**
- ⭐⭐ **"凭据在可信侧、沙箱只拿到一个回连的钩子"这件事最成熟的公开实现**：控制层持凭据、沙箱里的 git 靠一条人造通道取它、凭据本身**不落在沙箱**（Coder 的 token 存在服务端数据库）。差别只在协议：Coder 借的是 git 自己的 askpass，我们借的是 remote helper（为什么必须换掉，见 §D.1）。
- ⭐ **`if [ ! -d repo ]` 这种"幂等守卫"**：冷档重建时"目录已经在"就是正常情形，不该重 clone。我们的平台 clone 动词同理（Workspace 树已在 → 走 fetch 而不是 clone）。
- ⭐ **注入点放在 workspace 的初始化脚本，而不是 agent 的每一次命令**：一次注册、长期有效。

**不该抄**
- ⚠️ **askpass 的形状**：askpass 只能交"用户名/口令"，交付的那一刻它就是一份可用的凭据，沙箱内任何进程都能读它并在**通道之外**用它（Coder 的 askpass 路径是 `/tmp/coder.<rand>/coder`，容器内谁都读得到）。我们要的是"凭据不进不可信侧"，所以必须换成一个**协议化的动词**（remote helper），而不是一个"能打印秘密的程序"。
- ⚠️ **SSH key 形态**（`coder gitssh`）：SSH 认证在协议层没有作用域也没有有效期（本仓 `git-short-lived-credentials.md` §1 已证），与"一次操作一次租约"直接冲突。

### B.4 DevPod（沙箱内 credential helper 回连你本机；clone 完就把 helper 从 PATH 拆掉）

**怎么做的**
- **clone 由 workspace 侧 agent 发起**：`pkg/agent/workspace.go` 的 `CloneRepositoryForWorkspace(...)`，来源是 git URL（`source.GitRepository` + branch/commit/PR/subpath）；平台缓存那条路走 runner 上的 unix socket（日志「Cloning repository %s in platform」），否则本地跑 git；**clone 完 `RemoveHelperFromPath(` 把它自己装上的 helper 从 PATH 里拆掉**（[workspace.go](https://raw.githubusercontent.com/loft-sh/devpod/main/pkg/agent/workspace.go)）
- **凭据形态**：往全局 git config 写一行 `helper = !'<devpod 二进制>' agent git-credentials`（[gitcredentials.go](https://raw.githubusercontent.com/loft-sh/devpod/main/pkg/gitcredentials/gitcredentials.go)）；helper 子命令**回连**取真值——先 unix socket 打 `http://runner-proxy/git-credentials`，失败回落到本机 `localhost:<port>`，真值最终来自**你本机**的 `git credential fill`（[git_credentials.go](https://raw.githubusercontent.com/loft-sh/devpod/main/cmd/agent/git_credentials.go)）。文档侧：「DevPod will make https credentials available inside the dev container through a **git credentials helper**」「ssh credentials are available through **agent-forwarding**」（[credentials](https://devpod.sh/docs/developing-in-workspaces/credentials)）
- **clone 策略是一个显式枚举**：full `{"clone"}`、blobless `--filter=blob:none`、treeless `--filter=tree:0`、shallow/bare `--depth=1`；LFS 用 `GIT_LFS_SKIP_SMUDGE=1` 跳过（[clone.go](https://raw.githubusercontent.com/loft-sh/devpod/main/pkg/git/clone.go)）

**该抄**
- ⭐⭐ **"clone 用完就把凭据 helper 从 PATH 拆掉"**：暴露窗口被压到一次 clone 的时长。我们的窗口是零，但**这条是"平台上零凭据"与"现实里的最小暴露"之间的距离的标尺**。
- ⭐ **clone 策略是一处枚举**（full / blobless / treeless / shallow）：我们的平台 clone 动词将来也要有一处这样的枚举，而不是让 `--depth` 之类散在代码里。
- ⭐ **LFS 有一个显式开关**（`GIT_LFS_SKIP_SMUDGE`）：这是本轮在"LFS 怎么办"上唯一拿到的一手答案。

**不该抄**
- ⚠️ **沙箱内 credential helper**：凭据进不可信侧，且 helper 本体是"打印秘密的程序"。
- ⚠️ **默认走 partial clone**（`blob:none` / `tree:0`）：他们有网可依赖，我们没有——**我们的本地动词不该触发网络**（§D.6.2）。

### B.5 Daytona（git 动词是平台 API，凭据按次传；明文落盘警告）

**怎么做的**
- **git 操作由平台 API 执行**：「Git operations are available through the `git` module of a sandbox.」「Operations run through the Daytona API, so your application works with repositories in a sandbox directly, **without installing Git clients or executing shell commands inside it**.」（[git-operations](https://www.daytona.io/docs/en/git-operations)）
- **凭据按次传**：「Private repositories authenticate with personal access tokens **passed per operation**.」push/pull 的形参就是 username/password；另有 credential store，文档自己警告「**Credentials are stored in plaintext on disk.**」
- **创建沙箱不接受仓库参数**（参数只有 snapshot / image / envVars / secrets / labels / autoStopInterval / autoDeleteInterval）（[sandboxes](https://www.daytona.io/docs/en/sandboxes)）；持久化：「Daytona sandboxes are persistent by default.」「memory is cleared on every stop」（[persistence](https://www.daytona.io/docs/en/persistence)）
- secrets 是占位符 + 出口代理，但**只替换 header**：「Request bodies (including JSON) are forwarded as-is. **Body substitution is not supported.**」「Plain HTTP requests are never substituted.」（[secrets](https://www.daytona.io/docs/en/secrets/)）

**该抄**
- ⭐⭐ **"git 动词是平台的操作"**：Daytona 从相反的方向（API 驱动沙箱、不在沙箱里跑 shell）走到了同一个结论——**clone/fetch/push 可以是平台动词**，不必是沙箱里的一条 shell 命令。这与我们的平台动词是同一类主张。
- ⭐ **凭据按次传**（不是配一次长期用）。

**不该抄**
- ⚠️ 凭据明文落盘；⚠️ 占位符只覆盖 header（git 的 HTTP 认证恰好是 header，**这是运气不是设计**）；⚠️ **创建时不带仓库**——开局变成"进沙箱第一件事是 clone"，把网络与凭据一起带进沙箱。

### B.6 OpenHands（token 递进沙箱，agent 自己 push + 自己开 PR）

**怎么做的**
- **沙箱内 `git clone`**：「The agent clones the repository, creates a feature branch, and implements the change」「Creates commits with descriptive messages and pushes to the feature branch」「Creates a PR titled `[#42] <issue title>` with a summary and `Closes #42` in the body」——PR 由 **agent 自己**通过「The source control API (to push branches and create PRs)」开出（[issue-to-pr](https://docs.openhands.dev/openhands/usage/agent-canvas/prebuilt/issue-to-pr.md)）。
- **凭据形态：平台签发的短时 token，以 conversation secret 递进沙箱**：「OpenHands requests short-lived tokens (**8-hour expiration**) with these permissions: Contents: Read and write, Pull requests: Read and write, ...」（[github-installation](https://docs.openhands.dev/openhands/usage/cloud/github-installation.md)）
- **实测到的一条硬事实：token 会被嵌进 `origin` 的 URL**：「if you attach a repository at conversation start, it is **cloned with the *service account's* credentials**, not the PAT」「the service-account token **embedded directly into the `origin` remote URL**」；默认托管 token 是一个 `LookupSecret`，回连 app-server 的 `/api/v1/webhooks/secrets`「fetch a freshly-minted token each time it's read」（[service-account-github-pat](https://docs.openhands.dev/cookbook/service-account-github-pat.md)）
- secret 注入只作用于 terminal tool：conversation secrets 在 SDK 的「secret registry」，由 agent 的终端工具「scanning the command text」后注入，所以要用 `GITHUB_TOKEN=$GITHUB_TOKEN gh pr create ...` 这种写法；且「Configure `git user.name` and `git user.email` separately because the push credential does not set commit authorship.」（[conversations-and-sandboxes](https://docs.openhands.dev/enterprise/conversations-and-sandboxes.md)）
- 自建沙箱路径同样在沙箱内 clone：「provisions a sandbox **yourself** — shallow-cloning a git repo and running its setup script」，时序图里是 `git clone --depth 1` 发到 sandbox 的 agent server；「Start the sandbox with the appropriate git credentials available (e.g. via sandbox secrets)」（[clone-and-attach](https://docs.openhands.dev/cookbook/clone-and-attach.md)）

**该抄**
- ⭐ **"PR 由谁开"这个问题在产品里是有答案的**：OpenHands 的选择是 **agent 开**（它自己调 source control API），Copilot 的选择是 **平台开**。我们必须把这个答案写死——本仓的答案是**不提供开 PR 的动词**，所以要在文档里明确说这是一条**设计选择**，而不是漏了。
- ⭐ **token 嵌进 `origin` URL 是反面教材里最值得记住的一条**：它同时违反我们两条纪律（凭据进沙箱、凭据进 argv/文件）。

**不该抄**
- ⚠️ **8 小时 token**（我们的租约是"一次操作一次"）。
- ⚠️ **把 token 写进 `origin` URL**：`.git/config` 是工作区树里的明文文件，冷档留在宿主上，等于凭据随树长期留存。
- ⚠️ **靠"扫描命令文本"注入 secret**：那是把授权判定挂在一层字符串匹配上。

### B.7 GitHub Copilot coding agent（唯一把「agent 不许 push、平台代做」写进产品约束的先例）

**怎么做的**
- 「Copilot cloud agent can only perform simple push operations.」**「It cannot directly run `git push` or other Git commands.」**；「Copilot cloud agent only has the ability to push to a single branch.」——已有 PR 时「Copilot has write access to the pull request's branch」，否则「a new `copilot/` branch is created for Copilot, and the agent can only push to that branch.」（[risks-and-mitigations](https://docs.github.com/en/copilot/concepts/agents/coding-agent/risks-and-mitigations)，本页由我本人 WebFetch 逐字核对）
- 「When implementing changes, Copilot handles branch creation, commit messages, and pushing changes.」「can open one pull request per task」「on one branch at a time」「can only make changes in the repository specified when you start a task.」PR 以「its own app identity」打开，且「Draft pull requests created by Copilot cloud agent must be reviewed and merged by a human.」（[about-coding-agent](https://docs.github.com/en/copilot/concepts/agents/coding-agent/about-coding-agent)）
- 运行环境是平台侧临时环境：「its own ephemeral development environment, powered by GitHub Actions.」checkout 由平台做——「**If you do not check out your code, Copilot will do this for you**」「Copilot itself gets its own token for its operations.」（[customize-the-agent-environment](https://docs.github.com/en/copilot/how-tos/use-copilot-agents/coding-agent/customize-the-agent-environment)）
- 出网另有防火墙，且只覆盖 agent 的 Bash 工具：「The firewall only applies to processes started by the agent via its Bash tool.」（[customize-the-agent-firewall](https://docs.github.com/en/copilot/how-tos/use-copilot-agents/coding-agent/customize-the-agent-firewall)）

**该抄**
- ⭐⭐ **"agent 不能 push，平台代 push"是一个被一线厂商写进产品约束、并且公开文档化的形状**——这正是我们的"远端动词由 Worker 代发"。
- ⭐⭐ **"一次任务一个分支、一台机器一次只开一个 PR、只碰启动时指定的那一个仓库"**：三条约束合起来就是我们的 `Workspace ↔ 仓库 1:1` + 平台动词只作用于本 Workspace。**别人从完全不同的起点（微软自己的安全审查）走到了同一个形状。**
- ⭐ **"平台不接手 = 由 agent 自己做"要被显式选边**：Copilot 给 agent 的 push 是"平台代做"，OpenHands 给的是"agent 自己做"。**没有第三种含糊状态**，我们必须选。

**不该抄**
- ⚠️ **它仍然是"环境里有 token"**（「Copilot itself gets its own token」）：`cannot run git push` 是靠**工具与权限面**挡住的策略，不是靠"凭据不在那儿"的结构。我们的主张更强（ADR-0001 的"零凭据"），文档里不要把两者说成同一件事。
- ⚠️ **push 到"单个分支"这个约束交给平台判断**：我们的对应物是 Grant 上的授权判定（"授权早在 Grant 上判过"），不要在下游再加一层分支白名单。

### B.8 OpenAI Codex cloud（平台侧 checkout，PR 归人）

**怎么做的**
- **平台侧 clone**：「Codex creates a container and checks out your repo at the selected branch or commit SHA.」；缓存容器时「Codex clones the repository and checks out the default branch.」；「Codex runs your setup script, plus an optional maintenance script when a cached container is resumed.」（[cloud-environment](https://learn.chatgpt.com/docs/environments/cloud-environment.md)）
- 凭据是**出口代理 + 占位符**：「Programs receive a placeholder; the proxy substitutes the real value for allowed destinations.」「Sharing the environment shares these requirements, not your personal credentials.」
- 出网默认关：「Agent internet access is off by default, but you can enable limited or unrestricted access if needed.」「All outbound internet traffic passes through this proxy.」
- **push/PR 明确归人**：「Inspect changed files and check results, request follow-up changes, and **commit or open a pull request when you're ready**.」（[cloud](https://learn.chatgpt.com/docs/cloud)）

**该抄**
- ⭐ **平台侧 checkout + `setup script` 的分离**：环境准备（含 checkout）与 agent 的工作是两个阶段。我们的 `clone` 是平台动作，正是这条。
- ⭐ **缓存容器（snapshot）刷新时重跑一次 setup/maintenance**：冷档重建的类比物——树回来了，但环境（依赖）要不要重跑是两件事，**分开表达**。

**不该抄**
- ⚠️ **占位符替换 + 出口代理**：那需要 TLS 终结/按 host 改写（本仓 ADR-0001 已论证过为什么不走这条）。
- ⚠️ **默认关网**：ADR-0003 已决定 `egress: public` 默认开。

### B.9 Devin / B.10 Google Jules（两个"agent 自己 push"的变体，压缩）

- **Devin**：agent 自己 push 分支、自己开 PR，身份是 org 级 GitHub App——「These permissions enable Devin to work in your repositories as a regular contributor—**pushing branches, opening pull requests**」；PR 作者可配（[gh.md](https://docs.devin.ai/integrations/gh.md)）。环境是「a Linux-based virtual machine with your repositories cloned」，**每次会话从快照启动**（[environment](https://docs.devin.ai/onboard-devin/environment)）。⭐ 该抄：**"PR 的作者身份"是一个要显式配置的轴**（Devin / 用户 / 只有用户）——我们将来若代开 PR，这一条会立刻回来问，现在就该在文档里留一句"作者身份归平台身份"；⭐ 快照预 clone 与"冷档留树"同构。⚠️ 不该抄：org 级权限 + agent 自己 push（授权粒度不是"一次操作"）、secrets 以环境变量注入 agent 会话。
- **Jules**：VM 内 clone（「Jules runs in a virtual machine where **it clones your code**」），**push / PR 由用户点按钮**（「Click **Publish branch** or **Publish PR** to push Jules' changes to GitHub」）（[docs](https://jules.google/docs/)、[code](https://jules.google/docs/code/)）。⭐⭐ 该抄：**"发布"是一个显式的、由人按下的动作，而"改文件"是自由发生的**——这正是把 push 做成平台动作（而不是"工作区里的一条 git 命令"）的形状。⚠️ 不该抄：VM 内 clone + 仓库级 OAuth 授权。

### B.11 CI 三件套：Tekton `git-clone` / Prow `clonerefs` / Argo git artifact

**怎么做的**
- **clone 是流水线里的一步，而且不是 `git clone`**：Tekton 的 `git-clone` task 是一个**普通 step**（`steps[0].name: clone`），跑 `/ko-app/git-init`；而 git-init 源码里是 `git init` → `git remote add origin <url>` → `git fetch origin --update-head-ok --force --` → `git checkout -f FETCH_HEAD`（[git-clone.yaml @0.9](https://raw.githubusercontent.com/tektoncd/catalog/main/task/git-clone/0.9/git-clone.yaml)、[git-init/git.go](https://raw.githubusercontent.com/tektoncd-catalog/git-clone/main/image/git-init/git/git.go)）。**「先 `git init` 造一个空的 master 分支，再用 `--update-head-ok` 让首次 fetch 覆盖这个空 HEAD」——注释原话：「git-init always creates and checks out an empty master branch」/「tells git-fetch that it is ok to update the current (empty) HEAD on initial fetch」。**
- **凭据是拷进 Pod 的文件**：`basic-auth` workspace 里的 `.git-credentials` 被 `cp` 到 `$HOME/.git-credentials`（`chmod 400`）、`.gitconfig` 里配 `credential.helper store`；`ssh-directory` 被 `cp -R` 成 `~/.ssh/`（`chmod 700`）。README 只说「Binding a Secret to this Workspace is **strongly** recommended」。脱敏只作用于**日志**：`credentialURLPattern = (https?://)([^@]+)@` → `${1}****@`（[README @0.9](https://raw.githubusercontent.com/tektoncd/catalog/main/task/git-clone/0.9/README.md)、git-init/git.go 同上）。**catalog 的 0.9/0.10 两个版本都带 `tekton.dev/deprecated: "true"`。**
- **Prow `clonerefs`：token 进 argv，不进 `.git/config`**。凭据由 clonerefs 进程自己持有（`oauth_token_file`、`-ssh-key`、`-cookiefile`，或 GitHub App）；注入方式是把凭据拼进 URL：`u.User = url.UserPassword(token, "x-oauth-basic")`，注释「GitHub requires that the personal access token is set as a username」，然后**把这个带凭据的 URI 直接作为 `git fetch` 的实参**——源码注释明确「this file contains **no `git remote add`/`git remote set-url`** command, so nothing is stored as a named remote」；git 序列是 `git init` → `config user.*` → `git fetch <credentialed-url> --tags --prune` → `git fetch <BaseSHA> --no-tags` → `checkout` → `branch --force` → `submodule update --init --recursive`。输出/命令/错误三处都过内建 `censorToken`（token 替换成 `CENSORED`）（[clonerefs/options.go](https://raw.githubusercontent.com/kubernetes-sigs/prow/main/pkg/clonerefs/options.go)、[clone.go](https://raw.githubusercontent.com/kubernetes-sigs/prow/main/pkg/pod-utils/clone/clone.go)、[run.go](https://raw.githubusercontent.com/kubernetes-sigs/prow/main/pkg/clonerefs/run.go)）
- **Argo 的 git artifact 是"每次 Workflow 上声明"，不是"artifact repository 配置"**：`ArtifactRepository` 的字段里**没有 Git**（只有 S3/Artifactory/HDFS/OSS/GCS/Azure/Plugin），输入 artifact 由 **init container**（`init` / Short = "Load artifacts"）下载；凭据是三个 `*Secret` selector 被控制器变成只读 Secret volume 挂到 `/argo/secret/<name>`，executor 从文件读；**实现是 go-git 而不是 git CLI**，所以 HTTP 认证是纯内存的 `&http.BasicAuth{...}`，SSH 私钥则写一个 0600 临时文件、cleanup 时 `os.Remove`（[workflow_types.go](https://raw.githubusercontent.com/argoproj/argo-workflows/main/pkg/apis/workflow/v1alpha1/workflow_types.go)、[git.go](https://raw.githubusercontent.com/argoproj/argo-workflows/main/workflow/artifacts/git/git.go)、[configure-artifact-repository](https://argo-workflows.readthedocs.io/en/latest/configure-artifact-repository/)）

**该抄**
- ⭐⭐ **Tekton 的 `init` + `fetch --update-head-ok` + `checkout FETCH_HEAD` 正是"平台动词不叫 clone"的现成形状**：`git clone` 是糖，CI 现场要的是"把远端对象取回本地、再把某个 commit 检出成工作树"这两件事——**与我们的"平台动词（fetch/checkout 到树）+ 工作区已有树"是同一种拆法**。它顺带给出了**空仓库/未出生 HEAD 的落地方式**（先造一个空分支，再 `--update-head-ok` 覆盖），比"判断远端有没有 ref"更省事。
- ⭐ **Prow 的「不写 named remote，凭据只出现在这一次 fetch 里」**：凭据不进 `.git/config`（进的是 argv，见下）。
- ⭐ **Argo 的"私钥写 0600 临时文件、用完 `os.Remove`"**：暴露窗口最小化的具体做法。
- ⭐ **脱敏是内建的一层**（Tekton 的正则、Prow 的 `censorToken`）：我们的 `stderr_head` 上界与脱敏规则应当照抄这个做法，而不是"相信上游不会回显"。Prow 连 cookie 内容都只记 md5。

**不该抄**
- ⚠️ **Tekton 把凭据 `cp` 成 `$HOME/.git-credentials` + `credential.helper store`**：凭据以**持久明文文件**留在执行侧磁盘上——这正是 ADR-0001 那条纪律（执行前重置 `credential.helper`）要挡的事，而这里是反着做的。
- ⚠️ **Prow 把 token 放进 `git fetch` 的 argv**：`/proc/<pid>/cmdline` 同机任何用户可读——**git 自己在 `--config-env` 的文档里写了这条理由**（§A.3）。Prow 止步于"不进 `.git/config`"，我们要的是不进 argv 也不进文件。
- ⚠️ **Argo 的 SSH 私钥临时文件存在窗口**（虽然删掉了）。

---

## C. 横向对照表

| 产品 / 系统 | clone 在哪一步 | 凭据形态 | push 发起方 | PR 由谁开 | 多仓库 / submodule / LFS |
|---|---|---|---|---|---|
| **GitHub Codespaces** | **平台侧，容器创建之前**（浅克隆到 `/workspaces`，该目录随后 mount 进容器） | 容器内 `GITHUB_TOKEN` env + **git credential helper**（读写本仓；每次创建/重启换一枚） | 工作区内的**人** | 工作区内（平台只自动 fork + 升级 token） | 多仓只到"授权"层，无第二次 clone；submodule / LFS **未证实** |
| **Gitpod / Ona** | **runner 侧，环境启动时**（用开发者自己的凭据） | **凭据留在 runner**，环境里的 git 操作按环境所有者身份执行 | 环境内（人与 agent 同一身份） | SCM 工具可代开 PR（另有 provider API） | Ona 命令式 clone 到 `/workspaces/<repo>`；Classic 声明式 `additionalRepositories`；submodule / LFS **未证实** |
| **Coder** | 沙箱内（模板 `startup_script` / 官方 git-clone 模块，带 `if [ ! -d ]` 守卫） | 沙箱内 **`GIT_ASKPASS`** 回连控制面按需取 token（或平台生成的 SSH key） | 沙箱内 | 无此概念 | 未证实 |
| **DevPod** | workspace 侧 agent（`CloneRepositoryForWorkspace`）；平台缓存那条走 runner 的 unix socket | 沙箱内 **git credential helper 回连本机** `git credential fill`；**clone 完从 PATH 拆掉** | workspace 内 | 无文档 | LFS 有显式开关 `GIT_LFS_SKIP_SMUDGE`；submodule 有开关；多仓未证实 |
| **Daytona** | **不在创建时**；沙箱内通过 **平台 API** 调 git | PAT **按次传**（username/password）；credential store **明文落盘** | 沙箱内（经平台 API） | 无文档 | 多仓 = 多次 clone；submodule / LFS **未证实** |
| **OpenHands** | 沙箱内（`git clone --depth 1` 发到 sandbox agent server） | 平台签发 **8 小时** token，经 conversation secret 下发，**嵌入 `origin` URL** | 沙箱内 agent（source control API） | **agent** | 未证实 |
| **GitHub Copilot coding agent** | **平台侧**（Actions 环境；"If you do not check out your code, Copilot will do this for you"） | 环境内有它自己的 token | **平台代做**（"cannot directly run `git push`"） | **平台**（agent 自己的 app identity） | 单仓库、单分支、单 PR |
| **Codex cloud** | **平台侧**（"creates a container and checks out your repo"） | 出口代理 + 占位符替换 | 用户 | **用户** | 未证实 |
| **Devin** | 环境快照里已 clone（VM 内，Git 集成授权） | Git 集成 + secrets 注入环境变量 | agent（GitHub App） | **agent**（作者可配） | 未证实 |
| **Google Jules** | VM 内 clone | GitHub App 仓库授权 | **用户点按钮** | **Jules**（发布后 creator 是 Jules） | 未证实 |
| **CI：Tekton / Prow / Argo** | 流水线里的一步（Tekton：普通 step 跑 git-init；Prow：clonerefs 在 Pod 内；Argo：init container "Load artifacts"） | Tekton：Secret volume → `cp` 成 `$HOME/.git-credentials`（`credential.helper store`）；Prow：token 拼进 `git fetch` 的 **argv**（不写 named remote）；Argo：Secret volume `/argo/secret` + go-git 内存/0600 临时文件 | 平台（流水线不 push 回业务仓） | 平台（CI 不涉及） | Tekton 有 `sparseCheckoutDirectories`/`subdirectory`；Prow 有 `submodule update --init --recursive`；Argo 有 `DisableSubmodules`/`SingleBranch`/`Depth` |
| **Stars Sandbox 既定形状** | **平台动作**（宿主侧，归属 Worker 代发） | **零凭据进沙箱**；能力通道只交动词 | **平台**（与 shim 共用同一代发器与同一种租约） | 不提供（显式选择） | 1:1 单仓库；不发 pull |

---

## D. 对 Stars Sandbox 的推论

### D.1 shim 该实现 remote helper，**不是** credential helper

两条协议的语义正好相反，这是选择的全部理由：

| | credential helper | remote helper |
|---|---|---|
| 契约是什么 | 「给我一份能用的 username/password，我去直连远端」 | 「git 把 `list`/`fetch`/`push` 动词交给你」 |
| 凭据在哪 | **在沙箱里**（helper 的 stdout 就是明文凭据；`credential.<url>.*` 还会把它写进 `.git/config` 或凭据库） | 不需要存在 |
| 通道形态 | 需要沙箱直连 Git 实例（网络路径 + 凭据两件事一起给） | 一条 UDS，协议是动词 |
| 与能力通道的关系 | 无关 | **一对一**：`fetch`/`push` 就是能力通道 v1 的那两个动词 |

- **取 remote helper 的直接后果**（都已实测）：shim 不需要凭据、不需要出网、不需要实现 pack 传输；能力通道的返回 `{ok, exit_code, stderr_head}` 与 shim 的回话之间有明确映射（§D.2）。
- **credential helper 协议仍然要用到，但在宿主侧，而且是"清空它"**：ADR-0001 那条纪律的规范依据就是 §A.1——执行任何代发前把 `credential.helper` 置成空串（「resets the helper list to empty」，原文），否则宿主上已有的 `store` helper 会把租约写进 `~/.git-credentials`。另外两条 git 自己给的锁值得一起加进宿主侧纪律：`credential.interactive=false`（「To avoid the possibility of user interactivity from Git」）与 `GIT_TERMINAL_PROMPT=false`；凭据只经 `GIT_CONFIG_COUNT`/`GIT_CONFIG_KEY_<n>`/`GIT_CONFIG_VALUE_<n>` 或 `--config-env` 注入（git 自己写了理由：argv「other processes might be able to read your command line」）。
- 一句话：**沙箱侧 = remote helper；宿主侧 = 把 credential helper 清空**。两侧都不把凭据交给沙箱。

### D.2 shim 的必答清单（每条代价都被规范或实测钉过）

| 命令 / 能力 | shim 必须做什么 | 不做会怎样 |
|---|---|---|
| `capabilities` | 必答，能力名 + 空行；`*` 前缀才算"必须被理解" | 「Support for this command is mandatory」——协议起不来 |
| `list` | 报真实远端 ref；**能力通道不可用时必须死** | 空表 = git 判"空仓库"并**退出 0**（实测）。**这是本设计最危险的静默失败** |
| `fetch <sha> <name>` | 批处理 + 完成时空行；出错 → **写 stderr + 非零退出** | 没有带内错误通道；「多说一句」只会是 warning（源码），说少了才是失败 |
| `push [+]<src>:<dst>` | 逐 ref 回 `ok <dst>` / `error <dst> <why>` | 回别的行会 `die("expected ok/error, helper said ...")`（源码）；`<why>` 是把宿主侧 `stderr_head` 送达 agent 的**唯一正式通道** |
| `option` | 至少实现 `cas`（把 lease 语义落到宿主侧那一次 push 上） | 回 `unsupported` 时 git **照推**（源码里返回值被丢弃 + 实测），`--force-with-lease` 静默变成盲 push |
| `option` 的其余项 | `verbosity`/`progress`/`cloning`/`followtags`/`depth*` 可回 `unsupported` | 无害；`dry-run`/`atomic`/`pushcert`/`push-option` 回 `unsupported` 会被 git `die`——**这是可以接受的大声失败**，别假装支持 |
| `connect`/`stateless-connect`/`import`/`export`/`refspec`/`bidi-import` | **不声明** | 声明了就等于多担一份语义；不声明时 git 侧该方向直接失败，正是我们要的白名单 |

补一条 shim 侧的形状建议（【推断】，本仓 map 上的 fog）：**把"能力通道握手的失败"与"远端动词被拒"分成两种退出**——前者绝不能被表达成空 `list`（实测会静默成功），后者可以（`error <dst> <why>` 或非零退出）。

### D.3 宿主侧裸仓镜像：值，但要带两条纪律（否则会把工作区搞坏）

- **值在哪**：官方原话——用已经存在的仓库当 alternate「will require fewer objects to be copied from the repository being cloned, **reducing network and local storage costs**」；`GIT_ALTERNATE_OBJECT_DIRECTORIES` 又是一条「New objects will not be written to these directories」的只读搜索面。对"同一仓库的第二个、第三个 Workspace"，这是唯一能把"再拉一遍"变成"只拉增量"的手段。
- **纪律一（来自 git 自己的 NOTE，必须原样读）**：alternates 是**借用**，源仓一旦把对象清掉，借用方「will become corrupt」。所以镜像必须是 **append-only**（永不 `gc`/`repack` 掉对象），或者
- **纪律二**：每次 clone 都带 `--dissociate`——「stop borrowing from them after a clone is made by making necessary local copies of borrowed objects」，代价是磁盘、换来的是工作区不再依赖镜像的存活。
- **bundle-uri 替不掉它**：bundle-uri 的定位是「bootstrap the object database in advance of **fetching the remaining objects from a remote**」——它省的是流量，不是"远端不可达时的对象源"；而且 `--bundle-uri` 不兼容 `--depth`，默认还关着（`transfer.bundleURI=false`）。**在"沙箱里的 git 只能通过能力通道触达远端"这个前提下，bundle-uri 的收益面基本被吃掉。**
- 【推断】镜像的粒度是**仓库引用**（Git 实例 + 路径），键就是那个坐标；它是冻在宿主上的缓存，与 Workspace 的保留期无关——这与 ADR-0007 的"寿命只由保留期决定"不冲突，因为镜像不属任何 Workspace。

### D.4 「`clone` 是平台动作」有没有先例：**有，而且是最强的那一类先例**

- **平台侧 clone 的先例**：**GitHub Codespaces**（「cloned onto the host VM **before the container is created**」+ 目录随后 mount 进容器——与我们的工作区树逐字同构）、**Gitpod / Ona**（「the runner uses the developer's credentials to clone the repository」）、Codex cloud（平台创建容器并 checkout）、Copilot coding agent（平台侧 Actions 环境，没 checkout 它会替你 checkout）。
- **沙箱内 clone 的先例（多数派）**：Coder、DevPod、Daytona、OpenHands、Jules、Devin。
- ⚠️ **绝大多数"沙箱内 clone"的先例同时把凭据放进了沙箱**（askpass / credential helper / secret env / `origin` URL / App 权限）——不抄的正是这一半。
- **"平台代 push、agent 不许 push"的先例**：Copilot coding agent 是唯一把它写成产品约束并公开文档化的（「It cannot directly run `git push` or other Git commands.」）；但它挡 push 靠的是工具与权限面，环境里仍然有 token。
- **"凭据在可信侧、动作由执行侧发起"的先例**：Coder（token 存在控制面，沙箱只拿到一个回连的 askpass）、Ona（「keeps source control credentials on runners」「They never reach the management plane.」）。
- **结论**：三件事各有先例——**平台侧 clone**（Codespaces / Ona）、**凭据留在可信侧**（Coder / Ona）、**平台代 push**（Copilot）；**但"零凭据进沙箱" + "远端动词经一条人造动词通道" + "平台代发"这三件一起没有先例**。最接近的是 **Codespaces 的目录模型 × Coder 的凭据位置 × Copilot 的 push 约束**。这句话值得写进设计文档与 Skill 文档：日后有人拿"别人都在沙箱里 clone、都往容器里塞 token"来推翻既定形状时，这里是逐条对照的一手证据。

### D.5 一条由此得到的、值得立刻验证的推论：对象换手可以**不走** UDS

【推断，未实测】Workspace 树是 bind mount 进沙箱的，`.git` 就在树里；因此**宿主侧那次 fetch 写进去的对象库，沙箱里的 git 看得见**。于是 shim 的 `fetch` 可以退化成一次"请求 + 等待"：Worker 在宿主侧把对象拉进同一个对象库，shim 收到 `{ok}` 后回一个空行即可——**能力通道上不需要承载 pack**。

- 好处：shim 侧没有搬运代码，协议侧依然是四个命令；也解释了为什么我实测的那份 shim（对象搬运交给本地裸仓）在结构上是对的形态。
- 待验证的坑：宿主侧写、沙箱内读**同一个对象库**的并发语义（只能落在 git 自己的锁纪律上）；以及 **ref 的归属**（宿主侧 fetch 会不会顺手改了沙箱里那份 `refs/remotes/origin/*`）。这两条都还没测。
- 反面：如果将来对象库不再落进工作区树（例如为了冷档更小），通道就得承载 pack，或者 shim 自己去 fetch 一个宿主可见的裸仓——**这个二选一应当在设计文档里显式写下来**。

### D.6 顺带被证实/纠正的四条

1. **不发 `pull` 是对的，而且理由比"动词少一个"更硬**：`pull` = `fetch` + 本地 merge，一次命令里混了需要凭据的那一半和不需要凭据的那一半，失败的归因不可分。实测：`git pull` 确实经通道走了一次 `fetch`，随后在本地 merge 处失败（`fatal: 需要指定如何调和偏离的分支。`）。**Skill 文档里要写：能力通道下的 `git pull` 需要 `--ff-only` 或 `--rebase`，而且是一次控制层往返。**
2. **不要用 partial clone / promisor remote**：它把 blob 的获取推迟到"用的时候"，而在我们的形状里"用的时候"是本地动词（`git diff`/`git log -p`），那时要么撞上网络要么撞上失败。`promisor.acceptFromServer` 默认 `none` 也说明服务端广告的 promisor 默认不被接受。
3. **"空仓库不是失败"要写窄一点**：只对"不指定 ref"成立（`clone` / `ls-remote` / 默认 refspec 的 `fetch`，退出 0）；`fetch <ref>` 与 `clone -b <name>` 在空仓库上是 **fatal、128**。平台的 clone 动作必须自己判"远端一个 ref 都没有"，而不是把 `ref` 直接传给 `git clone -b`。
4. **`--force-with-lease` 在宿主代发下缺前提**：git 的答案依赖"我们最后一次 fetch 时的 remote-tracking ref"，而宿主侧代发没有这个概念的对应物（`=refname:<expect>` 形式是唯一不依赖它的，且官方说其余形式仍是 experimental）。**`<expect>` 只能由控制层提供**；给不出来时，force 的正当性本来就该在 Grant 层面判定（ADR-0002 已经这么写了：refspec 里带 `+` 就是 force）。


---

## E. 未能证实 / 未覆盖

**git 侧**

- **官方文档对"空仓库"沉默**：`git-clone(1)`、`git-fetch(1)` 全文没有 empty repository 的行为说明；本节的行为断言全部来自 `builtin/clone.c` 源码与实测，**不是文档**。
- **规范里 `fetch` 没有错误形式**——这是协议设计（`list` 只有空表、`fetch` 只有空行），不是文档缺口。所以"不能把失败表达成空表"是我给出的**设计约束**，而不是规范条款。
- **`option cas` 只在源码里**：`gitremote-helpers(7)` 没有定义它（§A.2）。将来 git 改它，不会有文档层面的提醒——**这条依赖是实现细节，写进 shim 时要留注释**。
- 【未证实】**remote helper 的查找路径**：规范只说「it automatically invokes *git remote-<transport>*」，**没有写**它按 `PATH` 还是 `GIT_EXEC_PATH` 找（`gitcredentials(7)` 那句 "put it in the `$PATH` or `$GIT_EXEC_PATH`" 讲的是 credential helper 的命名惯例）。**map 上那条 fog「shim 怎么进镜像」因此还差一步：本轮没能从一手文档确认这两条里哪一条是保证。**
- 【未实测】**宿主侧写、沙箱内读同一个对象库**的并发语义与 ref 归属（§D.5）。
- 【未实测】把 pack 经 UDS 搬运、以及把 `git bundle` 当搬运格式（bundle 只支持读，**不支持 push 进去**，且创建时需要显式 ref 名）。
- 【纠错】`gitprotocol-bundle-uri` **不是 2.53.0 的 man 页**（`Documentation/gitprotocol-bundle-uri.adoc` 404）；bundle-uri 的规范在 `Documentation/technical/bundle-uri.adoc`。
- **未覆盖**：`git submodule` / `git LFS` 在"零凭据沙箱"里的完整处理（本轮只拿到 DevPod 的 `GIT_LFS_SKIP_SMUDGE`、Prow/Tekton 的 `--recursive` 开关与 `sparseCheckoutDirectories`）；多仓库（我们 1:1，本轮不做）。
- **未覆盖**：`actions/checkout` 自身的凭据形态（本轮没读，不凭记忆补）。

**产品侧**

- **Codespaces / Gitpod-Ona / DevPod / Daytona 的文档页都不带版本号或更新日期**，因此只能记为"2026-10-16 抓取到的当前版本"。
- **Codespaces**：token 的具体过期时长（只说 "automatic expiry period" + "a typical working day"）；空仓库建 codespace 的行为；submodule / LFS —— 均未证实。
- **Gitpod / Ona**：环境里的 git 操作**是否真的经 runner 中转**（文档只说凭据留在 runner）——这一条决定它是"我们的形状"还是"只借了凭据的位置"；git 凭据注入的具体形态、token 有效期均未证实。
- **DevPod / Daytona**：平台代 push 或代开 PR 的行为**没有文档**——只能记"未证实"，不能记"没有"。
- **CI**：`docs.prow.k8s.io` 没有 clonerefs 专页（两个路径都 404），clonerefs 的行为断言全部来自 `kubernetes-sigs/prow` 源码；Argo 的 GitHub App 认证只在 `examples/input-artifact-git.yaml` 的注释里，无正式文档；Tekton 的 catalog task 0.9/0.10 都已标 deprecated（用作"形状"的参考没问题，用作"当前最佳实践"有问题）。
