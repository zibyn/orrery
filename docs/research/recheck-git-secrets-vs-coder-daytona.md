# 重查 #35 / ADR-0013（Git 账户与 caller Secret）：对照 Coder 与 Daytona

- 调研日期：**2026-10-17**。对象：已关票据 #35（并入 #41）的裁决，即 `docs/adr/0013-git-accounts-and-caller-secrets.md`；连带 ADR-0011 的 `git.credential` 动词、ADR-0010 的「caller secrets」段。
- 一手来源与钉死版本：
  - **Coder**：`coder/coder` tag `v2.38.0`，commit `c3c6a672821e8ac2472c45f3be7a3c600aa8dae1`。文档读的是同一 commit 下的 `docs/*.md` 源文件，源码同 commit。下文 `[C:path#Lx]` 都指 `https://github.com/coder/coder/blob/c3c6a672821e8ac2472c45f3be7a3c600aa8dae1/path#Lx`。
  - **Daytona**：`daytonaio/daytona` tag `v0.190.0`（2026-06 起公开仓库冻结，见 `daytona-sandbox-runtime-semantics.md` 第 1 条）。下文 `[D:path#Lx]` 都指 `https://github.com/daytonaio/daytona/blob/v0.190.0/path#Lx`。**Secrets 功能和 `/git/credentials` 端点不在 v0.190.0 树里**，只出现在线上文档，下文标【线上】，URL 是 `https://www.daytona.io/docs/en/secrets.md`、`https://www.daytona.io/docs/en/git-operations.md`（无版本号，2026-10-17 抓取）。
  - GitHub 的行为以 docs.github.com 为准，单独给 URL。
- 复用的已有调研：`sandbox-git-integration.md` §B.3/§B.5（Coder askpass、Daytona git API）、`prior-art-sandbox-orchestration.md` §1.2/§1.10（Daytona 占位符、Coder `/me/external-auth` 带 `listen`）、`git-short-lived-credentials.md`（只有 GitHub 能发短时凭据）。本文只写增量，并把它们的 Coder 引用改钉到 v2.38.0。
- 【推断】表示我自己的推理，没有一手来源。

## 先读这几条

1. **主体形状和 Coder 一致，可以留着。** 部署配置里配 provider、callback 路径 `/external-auth/<id>/callback`、token 加密存数据库、沙箱里的 git 按 host 回连控制面取 token、多个 provider 同时匹配就报错不猜、没关联账户就把授权 URL 交给 Agent、另给一个命令供 `gh` 用，这些 Coder v2.38.0 全都有，连「Agent 模式下不等待，直接让 Agent 通知用户」都有（`CODER_CHAT_AGENT`，[C:cli/gitaskpass.go#L101-L104]）。Daytona 的沙箱产品没有「账户关联」这个概念，凭据由调用方每次传入。
2. **最大的缺口在 token 刷新，Coder 在这里踩过坑并留下了代码注释。** GitHub 的 refresh token 用一次就作废，**旧 access token 也随之作废**；并发刷新会互相把对方弄失效；刷新拿到新 token 后如果没有先落库，新 token 就丢了；把 403/429 当成永久失败会把 link 毁掉。ADR-0013 只写了一句「token 过期就先刷新」。**应修改**（§2 第 6 行）。
3. **Coder 每次下发都会校验 token，我们不校验。** 用户在 GitHub 上撤销授权以后，我们会一直下发一个没过期但已经失效的 token，git 拿到 401，Agent 也拿不到授权 URL。**应修改**：跟 Coder 一样，下发前校验，校验失败就返回授权 URL（§2 第 7 行）。
4. **沙箱里 git 的接入方式需要写到可以直接实现的程度。** 「先清空镜像的 helper 列表」是对的（Daytona 的源码注释给了一手理由：*prevent any inherited helper from persisting the token*），但 ADR 没说在哪一层清、怎么清。另外，host 不匹配任何 provider 时应该**放行给 git 的下一个 helper**，Coder 就是这样回落的；现在写的是报错。各 provider 的用户名也不一样（GitLab OAuth token 的用户名必须是 `oauth2`）。**应修改**（§2 第 9–11 行）。
5. **caller Secret 有两处 Coder 已经防住而我们没防。** 一是保留变量名：Coder 拒绝 `GIT_*`、`LD_*`、`BASH_ENV`、`PATH`、`CODER_*` 等。我们自己的 helper 配置（`GIT_CONFIG_*`）和 Channel socket 路径变量都可能被一个 Secret 覆盖。二是大小上限。另外「改动只影响新 Sandbox」在 stop→start 时含义不清：Runtime 是新建的，那 Secret 的值重新解析吗？**应修改**（§3）。
6. **可以简化的地方**：自建实例不必让管理员填一组端点 URL 再加 host 正则，只填一个 `base_url`，其余按 type 推导（Coder 对 GitLab/Gitea 就是从 AuthURL 的 host 推导默认正则的，[C:coderd/externalauth/externalauth.go#L1350]，[#L1467]）。PKCE 不应该写成必选，Coder 默认 S256，但每个 provider 都能关掉（[C:docs/admin/external-auth/index.md#L171-L181]）。

## 1. 两家各自怎么做（事实底稿）

### 1.1 Coder v2.38.0

- **Provider 配置**：用环境变量 `CODER_EXTERNAL_AUTH_<N>_{ID,TYPE,CLIENT_ID,CLIENT_SECRET,AUTH_URL,TOKEN_URL,VALIDATE_URL,REVOKE_URL,REGEX,SCOPES,PKCE_METHODS,…}`；`ID` 用作回调路径 `https://example.com/external-auth/primary-github/callback`（[C:docs/admin/external-auth/index.md#L26-L50]）。*"The `REGEX` variable must be set if using a custom Git domain."*（[#L323]）。**多个 provider 是付费功能**：*"Configuring more than one external authentication provider requires a Premium license"*（[#L380-L384]）。
- **按 type 给默认值**：GitHub 默认 `Regex: ^(https?://)?github\.com(/.*)?$`、scopes `repo workflow`、`ValidateURL https://api.github.com/user`、`RevokeURL .../applications/<client_id>/grant`、PKCE S256（[C:coderd/externalauth/externalauth.go#L1304-L1320]）；GitLab 默认 scopes `write_repository read_api`、revoke `https://gitlab.com/oauth/revoke`（[#L1368-L1380]）；自建实例的默认正则从 AuthURL 的 host 推导（[#L1350]，[#L1467]）。
- **GitHub App 推荐**：「Configure a GitHub OAuth app」一节的第 1 步就是 *"Create a GitHub App"*，要求 *"Enable fine-grained access to specific repositories"*，并提醒 app 不设为 public 时，*"anyone outside the app's owning account or owning organization gets a GitHub 404"*（[C:docs/admin/external-auth/index.md#L335-L378]）。
- **PKCE**：*"Coder will usually assume PKCE support is available with "S256""*，可以用 `PKCE_METHODS="none"` 关掉（[#L166-L181]）；Bitbucket Server、JFrog 等 type 在源码里默认是 `none`，并带着 `TODO: Investigate if 'S256' is accepted`（[C:coderd/externalauth/externalauth.go#L1331-L1332]）。
- **沙箱内注入方式**：agent 给自己派生的每个进程设置 `GIT_ASKPASS=<coder 可执行文件>`（[C:cli/agent.go#L263-L265]）和 `GIT_SSH_COMMAND="<coder> gitssh --"`，同时把 `CODER_AGENT_TOKEN` 也放进进程环境（*"Specific Coder subcommands require the agent token exposed!"*，[C:agent/agent.go#L1704-L1709]）。也就是说，**沙箱里任何进程都能拿 agent token 去换 owner 的 Git token**，和我们 ADR-0013 Consequences 第 1 条承认的情况一样，只是 Coder 用的是 bearer token，我们用的是 socket 可达性。
- **askpass 流程**（[C:cli/gitaskpass.go#L47-L140]）：从 git 的提示字符串里解析出 host，调用 `GET /api/v2/workspaceagents/me/external-auth?match=<host>`；返回 404（没有匹配，或者有歧义）时打印警告并 `ErrCanceled`，**git 回落到它自己默认的凭据行为**；返回里带 URL（未关联）时，在 `CODER_CHAT_AGENT=true` 下输出 *"You must notify the user to authenticate with Git"* 后退出，人类用户则打开浏览器，并用 `listen=true` 轮询等待。
- **provider 解析**（[C:coderd/workspaceagents.go#L2186-L2225]）：只在模板声明过的 provider 里匹配；多个声明匹配时返回 404 *"Multiple external auth providers declared by this workspace's template match …"*；声明过但部署里已删除的 provider 也拒绝，*"Coder will not substitute a different provider's token"*；只有当所有声明都不匹配时，才在全部 provider 里匹配。
- **刷新**（[C:coderd/externalauth/externalauth.go#L255-L532]）：
  - 用 singleflight 按 `provider:user` 去重：*"Prevent parallel refreshes … Otherwise, the parallel calls will fail with a bad refresh token error as they can only be used once."*（#L265-L267），多副本之间再加一个 DB lease 列（#L310-L373）。
  - 先落库，再校验：*"GitHub rotates refresh tokens on every use, so the old refresh token is already invalid on the IDP side. If we validated first and the validation endpoint was unavailable (e.g. rate-limited 403), the new token would be silently lost"*（#L480-L484）。
  - 区分永久失败和暂时失败：`bad_refresh_token`、`invalid_grant`、`unauthorized_client`、400/401/200 算永久失败，此时清空 refresh token 并记下原因；5xx/429 算暂时失败，重试；*"403 is intentionally excluded … the previous 403 case caused token destruction on rate-limited refresh attempts."*（#L1690-L1740）。
  - **每次取 token 都校验**，即使不需要刷新（#L289-L295），校验失败就当作未关联，返回授权 URL（[C:coderd/workspaceagents.go#L2304-L2318]）；GitHub 刚刷新完立刻校验偶尔会 401，所以带退避重试（[C:coderd/externalauth/externalauth.go#L597-L607]）。
- **各 provider 的用户名不同**：GitLab 是 `Username: "oauth2", Password: token`，Bitbucket 是 `x-token-auth`，其余（GitHub 等）是 `Username: token`（[C:coderd/workspaceagents.go#L2637-L2657]）。
- **给 `gh` 的路径**：`coder external-auth access-token <id>` 把 token 打到 stdout，退出码 0；未关联时打印 URL，退出码 1（[C:cli/externalauth.go#L34-L41]）。文档同时提醒：通过模板注入的 `GITHUB_TOKEN` *"will not auto-refresh"*（[C:docs/admin/templates/extending-templates/external-auth.md#L72-L76]），推荐用 askpass。
- **解除关联**：先删除 link，再调用 provider 的 revoke；revoke 失败不影响删除，只在响应里返回 `TokenRevoked=false` 和错误信息（[C:coderd/externalauth.go#L113-L135]）。
- **存储加密**：*"By default, Coder stores external user tokens and other sensitive values in plaintext"*；启用后用 AES-256-GCM 加密，每个加密列都配一个 `$C_key_id` 列，以支持轮换（[C:docs/admin/security/database-encryption.md#L5-L44]）。
- **审计**：`enterprise/audit/table.go` 里有 `UserSecret`、`GitSSHKey`，**没有 `ExternalAuthLink`**（[C:enterprise/audit/table.go]）。也就是说 Coder 不审计 token 的下发。
- **SSH**：给每个用户生成密钥对，`coder gitssh` 每次调用时取私钥，写临时文件，用完删除（[C:cli/gitssh.go#L46-L60]，[C:docs/admin/external-auth/index.md#L138-L150]）。
- **User Secrets**（[C:docs/user-guides/user-secrets.md]）：
  - 字段：名字、值、env 目标或 file 目标、enabled。*"User secrets apply to all workspaces that you own."*（#L30）。值只写不读（#L32-L33）。
  - 风险提示：*"Anyone with shell or file access to a workspace can read secrets injected into that workspace."*（#L35-L38）。
  - 生效时机：在工作区启动或 agent 重连时生效，*"To pick up a change … restart the workspace"*（#L49-L53）；*"does not revoke a credential that a workspace has already received"*（#L61）。
  - 上限：每用户 50 个，单个 24 KiB（#L113-L127）。
  - 保留变量名：`PATH HOME SHELL … IFS CDPATH ENV BASH_ENV TMPDIR … SSH_AUTH_SOCK …`，加上前缀 `GIT_`、`LC_`、`LD_`、`DYLD_` 和 `CODER_*`（[C:codersdk/usersecretvalidation.go#L114-L212]，`GIT_` 的理由在 #L192-L196）。
  - 注入方式：agent 从 manifest 收到 Secret 值，放进它派生的每个进程的环境变量里，**不写进容器配置**；优先级是 Secret 覆盖模板 env，agent 自己的变量（`GIT_ASKPASS`、`CODER_AGENT_TOKEN`）覆盖一切（[C:agent/agent.go#L1743-L1760]）。

### 1.2 Daytona v0.190.0（加【线上】文档）

- **没有 OAuth 账户关联**：*"Private repositories authenticate with personal access tokens passed per operation."*【线上 git-operations.md 第 5 行】。toolbox 的 clone/push/pull 每次请求都带 `username/password`（[D:apps/daemon/pkg/toolbox/git/clone_repository.go#L55-L62]）。
- **git CLI 模式的凭据通路**（[D:apps/daemon/pkg/git/clone.go#L78-L160]）：
  - 用一个 askpass 脚本从 `GIT_USERNAME`/`GIT_PASSWORD` 读凭据，注释写的是 *"reads creds from env so they never hit argv, URL, or .git/config"*。
  - 每条命令都带 `-c credential.helper=`，注释是 *"prevent any inherited helper from persisting the token"*；同时带 `-c core.hooksPath=/dev/null`，注释是 *"so hooks can't read GIT_USERNAME / GIT_PASSWORD"*。
  - 做纯本地的 checkout 时不传凭据，注释是 *"so rogue checkout hooks cannot exfiltrate the token"*。
  - push/pull 同样处理（[D:apps/daemon/pkg/git/push.go#L85-L93]）。
- **遗留代码**：`SetGitConfig` 往 gitconfig 里写 `credential.helper = /usr/local/bin/daytona git-cred`（[D:apps/daemon/pkg/git/config.go#L30-L38]），但 v0.190.0 树里找不到 `git-cred` 子命令。【推断】这是旧 Workspace 产品的遗留。
- **【线上】`POST /toolbox/{id}/git/credentials`**：*"Persist Git credentials globally via the credential store … Credentials are stored in plaintext on disk."*（git-operations.md 第 1269–1271 行）。
- **环境变量**：创建时传 `env` map（[D:apps/api/src/sandbox/dto/create-sandbox.dto.ts#L54-L61]），**以明文 jsonb 存储**（[D:apps/api/src/sandbox/entities/sandbox.entity.ts#L118-L123]），**通过 Sandbox DTO 原样返回**（[D:apps/api/src/sandbox/dto/sandbox.dto.ts#L328]），由 runner 写进容器的 `Env`（[D:apps/runner/pkg/docker/container_configs.go#L87-L89]）。也就是说，env 可以读回，也落在 Docker 容器配置里。
- **【线上】Secrets**：
  - 组织级。值 *"Encrypted at rest and never returned by the API after creation"*（secrets.md 第 25 行）。
  - 创建 Sandbox 时传「环境变量名 → secret 名」，沙箱里拿到的是占位符 `dtn_secret_…`，由出口代理只在**请求头**里替换，并且只针对白名单 host。
  - 代理会擦洗响应，把回显的真值换回占位符。
  - *"set an allowlist for every secret"*（第 51 行）。
  - 轮换时占位符不变，*"within 15 seconds"* 生效（第 624 行）。
  - 可以用 `update_secrets` 原地增减（第 458–464 行）。
  - **Basic Auth 用不了**：*"These helpers Base64-encode the credential inside the sandbox … the proxy cannot substitute the real value"*（第 729 行）。git over HTTPS 用的就是 Basic Auth，所以**Daytona 的占位符方案不能直接拿来做 git 认证**。这条一手事实也支持我们否决「出口代理替换占位符」。

## 2. Git 部分逐条对照

| # | 裁决（ADR-0013） | Coder v2.38.0 | Daytona v0.190.0 | 判定 |
|---|---|---|---|---|
| 1 | 部署配置里列 Git Provider：id、type ∈ {github, gitlab, gitea, generic}、OAuth client id、client secret 用文件引用 | 用环境变量 `CODER_EXTERNAL_AUTH_<N>_*`，type 还包括 azure-devops/bitbucket/jfrog/slack 等，没识别的按通用 OAuth2 处理（[C:docs/admin/external-auth/index.md#L26-L40]，[C:codersdk/externalauth.go#L36-L47]） | 无此概念 | **保留**。和 Coder 同构；client secret 用文件引用是为了与 ADR-0010 一致，比 Coder 用环境变量更稳 |
| 2 | 自建实例必须填端点 URL 和 host pattern | 自建 GitLab 要列 `AUTH/TOKEN/VALIDATE/REVOKE_URL` 加 `REGEX`（[#L286-L299]）；但 GitLab/Gitea 的默认正则会从 AuthURL 的 host 推导（[C:coderd/externalauth/externalauth.go#L1350]，[#L1467]） | 无 | **应简化**：每个 provider 只填一个 `base_url`，auth/token/revoke/validate 的路径和 host pattern 都按 type 推导，个别项允许覆盖。理由：对单团队来说，配置项越少越不容易出错；Coder 自己也已经在推导正则 |
| 3 | 允许多个 provider 并存；多个匹配时报错 | 多 provider 是 Premium 功能（[#L380-L384]）；多个匹配时返回 404 拒绝猜（[C:coderd/workspaceagents.go#L2186-L2197]）；还会先按模板声明收窄 | 无 | **保留**。Coder 收费是商业原因，不是技术原因【推断】。我们没有模板，所以不需要「声明收窄」那一层。github.com + 内网 GitLab 两个 provider 的 host 本来就不重叠，这条报错只是兜底 |
| 4 | GitHub 同时接受 OAuth App 和 GitHub App，文档推荐 GitHub App | 文档第一步就是 *"Create a GitHub App"*，按仓库细粒度授权；提醒非 public 时会 404，并提供 `APP_INSTALL_URL`（[C:docs/admin/external-auth/index.md#L335-L378]） | 无 | **保留**。和 Coder 一致。文档要补 Coder 踩过的两点：app 需要安装到团队 org 上；GitHub App 的 user token 8 小时过期，refresh token 6 个月（[GitHub: refreshing user access tokens](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/refreshing-user-access-tokens)），这让第 6 行成为必做项 |
| 5 | 授权码流程强制 PKCE，回调地址 `/external-auth/<id>/callback` | 回调路径完全相同（[#L42-L50]）；PKCE 默认 S256，每个 provider 都能用 `PKCE_METHODS="none"` 关掉（[#L166-L181]）；部分 type 的源码默认是 none（[C:coderd/externalauth/externalauth.go#L1331-L1332]） | 无 | **应修改（小）**：PKCE 默认 S256，但每个 provider 允许关闭，写成「默认」而不是「必须」。理由：`generic` 和一些自建实例不一定支持 PKCE，Coder 已经因此留了开关 |
| 6 | 「Control Plane …… token 过期就先刷新」 | 按 `provider:user` 做 singleflight，多副本再加 DB lease；**先落库再校验**；区分永久和暂时失败，403/429 不毁 link；失败原因记在 link 上（[C:coderd/externalauth/externalauth.go#L255-L532]，[#L1690-L1740]） | 无 | **应修改**。ADR 要写明四点：①**同一 link 的刷新要串行**。CP 是单进程（ADR-0014），进程内按 link 加锁就够了，不需要 Coder 的 DB lease；②**新 token 要先提交，再做后续动作**；③**只有明确的 `invalid_grant`/`bad_refresh_token` 等才算永久失败**，这时清掉 refresh token 并返回授权 URL，网络错误/5xx/429/403 一律按暂时失败处理，返回 `unavailable`；④**留一个刷新余量**（例如剩余寿命不到几分钟就刷新）。一手理由：GitHub *"Once you use a refresh token, that refresh token and the old user access token will no longer work"*（同上 URL）；Coder 的注释记下了它踩过的坑（#L265-L267，#L480-L484，#L1728-L1731） |
| 7 | （ADR 没写）下发前不校验 token | 每次取 token 都调用 `ValidateURL`（GitHub 是 `/user`，GitLab 是 `/oauth/token/info`），校验失败就当作未关联，返回授权 URL（[C:coderd/externalauth/externalauth.go#L289-L295]，[C:coderd/workspaceagents.go#L2304-L2318]）；遇到限流按有效处理（[#L615-L620]（ValidateToken 注释）） | 无 | **应修改**：照 Coder 的做法，`git.credential` 在下发前校验一次，失败时返回授权 URL，而不是返回一个已经失效的 token。理由：用户在 provider 那边撤销授权、换了密码、或者 org 收回 app 安装以后，token 并没有「过期」，只靠过期时间判断，我们会无限期下发一个死 token，Agent 只能看到 401，拿不到恢复路径 |
| 8 | 解除关联：删除 token，并在有 revoke 端点时调用它 | 先删 link，再 revoke，revoke 失败不阻塞删除，响应里报告结果（[C:coderd/externalauth.go#L113-L135]）；GitHub 默认 revoke 的是 `/applications/<id>/grant`（[C:coderd/externalauth/externalauth.go#L1318-L1320]） | 无 | **保留**，补两句：revoke 尽力而为，失败不阻塞删除。另外 **ADR 的 Consequences 第 2 条要改**：GitHub 删除 grant 时 *"will also delete all OAuth tokens associated with the application for the user"*（[GitHub REST: Delete an app authorization](https://docs.github.com/en/rest/apps/oauth-applications?apiVersion=2022-11-28#delete-an-app-authorization)），所以对 GitHub 来说，已经被读走的 token 也会立刻失效，并不是「活到上游过期」。只有没有 revoke 端点的 provider 才是 ADR 现在写的那样 |
| 9 | Node Agent 先清空镜像的 `credential.helper` 列表，再设成平台 helper | 不用 credential.helper，给每个派生进程设 `GIT_ASKPASS` 环境变量（[C:cli/agent.go#L263-L265]）；文档承认 *"Some IDE's override the `GIT_ASKPASS` environment variable"*（[C:docs/admin/templates/extending-templates/external-auth.md#L58-L70]） | 自己发起的 git 命令都带 `-c credential.helper=`，理由是 *"prevent any inherited helper from persisting the token"*；同时带 `-c core.hooksPath=/dev/null`（[D:apps/daemon/pkg/git/clone.go#L137-L138]） | **应修改（写清机制）**。用 credential.helper 而不用 askpass 是合理的：helper 拿到的是结构化的 `protocol/host/path`，不必解析提示字符串，而且不会被 IDE 覆盖。清空列表的一手理由来自 Daytona：git 成功后会对每个 helper 调用 `store`，镜像里的 `store`/`osxkeychain` 会把 token 明文落盘。ADR 要写明在哪一层清：【推断】用 Exec 环境里的 `GIT_CONFIG_COUNT/KEY_n/VALUE_n`（命令级作用域，晚于 system/global/local 读取），依次设 `credential.helper=` 和平台 helper，不写任何文件，也和 Coder「靠进程环境注入」的做法一致。代价是用户自己在 gitconfig 里加的 helper 会被清掉，应在文档里说明 |
| 10 | host 没有已关联账户时报错并附授权 URL；多个匹配也报错 | 没有匹配的 provider，或者有歧义时返回 404，askpass **警告后回落到 git 自己的行为**（[C:docs/admin/external-auth/index.md#L416-L420]，[C:cli/gitaskpass.go#L83-L96]）；匹配上但未关联时返回 URL，Agent 模式下直接让 Agent 通知用户（#L99-L104） | 无 | **应修改**：要分三种情况。①**host 不匹配任何 provider** 时，helper 静默返回空，让 git 继续问下一个 helper，或者按 `GIT_TERMINAL_PROMPT=0` 直接失败；不要报「未关联」，否则访问一个和 provider 无关的 Git 服务器（例如公开 mirror，或者带 PAT Secret 的内部服务）就会被我们挡住；②匹配上但未关联时返回授权 URL（现状，保留）；③多个匹配时报错（现状，保留） |
| 11 | （ADR 没写）helper 返回的 username/password | 按 type 区分：GitLab 是 `oauth2:token`，Bitbucket 是 `x-token-auth:token`，GitHub 等是 token 作用户名（[C:coderd/workspaceagents.go#L2637-L2657]） | 调用方自己传 `username`，文档示例是 `"git"`（【线上】git-operations.md） | **应修改（小）**：`git.credential` 的返回按 type 固定用户名，GitHub 用 `x-access-token`【推断，GitHub 接受任意用户名配 token 作密码】，GitLab 用 `oauth2`（Coder 引了 GitLab 的说法），Gitea/generic 用 token 作用户名、token 作密码。否则 GitLab 会直接 401 |
| 12 | `store`/`erase` 什么都不做 | askpass 没有 store/erase 这一说，token 不在沙箱里留存（[C:docs/admin/external-auth/index.md#L126-L130]） | 【线上】`/git/credentials` **明文落盘**（git-operations.md 第 1271 行） | **保留**。比 Daytona 安全，和 Coder 等价。第 7 行做了以后，`erase` 也不需要承担「通知 token 失效」的语义 |
| 13 | `orrery token <provider>`，供 `GH_TOKEN=$(…) gh pr create` 使用 | `coder external-auth access-token <id>`：成功时退出码 0，未关联时打印 URL、退出码 1（[C:cli/externalauth.go#L34-L41]）；模板注入的 token *"will not auto-refresh"* | 无 | **保留**，在 Agent 文档里加一条 Coder 没写、但对 Agent 原生更要紧的提醒【推断】：只用内联形式 `GH_TOKEN=$(…) cmd`，不要单独运行这个命令。单独运行时 token 会进 Exec 的 stdout（平台会持久化），也会进 LLM 上下文；也不要 `gh auth login --with-token`，那样会把 token 写进 Workspace 里的 `~/.config/gh` |
| 14 | 不做 Git PAT 路径；PAT 是普通 Secret | external-auth 没有 PAT 路径；但 askpass 失败时回落到 git 默认行为，所以用户自己的凭据仍然能用 | 只有 PAT 路径 | **保留**，前提是第 10 行改成「不匹配就放行」。改了之后，一个以 Secret 注入的 PAT 配合用户自己的 helper 或 `-c` 就能访问非 provider 主机 |
| 15 | 只支持 HTTPS，不支持 SSH | 支持：每用户生成 SSH key，`coder gitssh` 每次调用时取 key，用临时文件（[C:cli/gitssh.go#L46-L60]）；*"Coder first attempts to use external auth provider tokens … If no tokens are available, it defaults to SSH"*（[C:docs/admin/external-auth/index.md#L99-L102]） | toolbox 只有 HTTPS 的用户名/密码 | **保留**。和 Daytona 一致，比 Coder 简单；SSH key 没有作用域，也没有过期时间（见 `git-short-lived-credentials.md` §1）。文档里给一条 Agent 自救办法：遇到 SSH 形式的 remote 或 submodule，用 `git config url."https://github.com/".insteadOf git@github.com:` 改写 |
| 16 | 每次 `git.credential` 下发都写 Audit Record（记 host 和 Sandbox） | **不审计**下发，audit 表里没有 `ExternalAuthLink`（[C:enterprise/audit/table.go]） | 无 | **保留**。这比 Coder 多一步，但 ADR-0013 把「审计」列为限制「沙箱内任何进程都能拿 token」的三件事之一，删掉它，这条 Consequence 就没有支撑了；对单团队来说，一次 git 操作一两行记录的写入量可以忽略 |
| 17 | token 加密存 PG | 默认**明文**，启用 dbcrypt 后用 AES-256-GCM，每列配 `*_key_id`，支持多把 key 轮换（[C:docs/admin/security/database-encryption.md#L5-L44]） | 【线上】Secrets 是 *"Encrypted at rest"*；`env` 是明文 | **应修改（小）**：写明加密 key 是部署时的文件引用（与 ADR-0010 一致），密文旁边存 key id（照 Coder 的做法），轮换 key 时不必一次重加密所有数据。否则「加密」只是一句话，没法落地 |
| 18 | 沙箱里的 git 直连远端，受 Egress Policy 约束；内网自建实例需要 Egress Opening | Coder 没有平台级出网策略 | Daytona 有 `insecure_skip_tls`，并建议 *"adding the server's CA to the sandbox base image's trust store"*（[D:apps/docs/src/content/docs/en/git-operations.mdx#L20]） | **保留**。文档补一句 Daytona 踩过的坑：内网 Git 如果用私有 CA，要把 CA 打进 Snapshot，平台不提供跳过 TLS 校验的开关 |
| 19 | 不托管 Repository，clone/push/PR 都是 Agent 自己的 `git`/`gh` | 不 clone（clone 由模板 `startup_script` 或模块完成），Agent 在工作区里自己跑 git | git 是 toolbox API，由平台在沙箱内执行，但仓库同样不是平台资源 | **保留**。两家都没有 Repository 资源 |

## 3. caller Secret 部分逐条对照

| # | 裁决 | Coder v2.38.0 | Daytona | 判定 |
|---|---|---|---|---|
| S1 | Secret 是 User 拥有的资源，值只写不读 | User 拥有；值 *"omitted from CLI output and REST API responses"*（[C:docs/user-guides/user-secrets.md#L32-L33]） | 【线上】组织拥有，值 *"never returned by the API after creation"*；v0.190.0 的普通 `env` **可以读回**（[D:apps/api/src/sandbox/dto/sandbox.dto.ts#L328]） | **保留**。两家对 Secret 都这么做；Daytona 普通 env 能读回，这正是单独做一个 Secret 资源的理由 |
| S2 | 加密存 PG | 只有启用 dbcrypt 时才加密 | 加密 | **保留**，加密 key 的写法同第 17 行 |
| S3 | 创建 Sandbox 时传「环境变量名 → Secret 名」并注入 | 不按工作区映射：*"User secrets apply to all workspaces that you own"*，变量名在 Secret 上配置（#L30） | 【线上】与我们完全相同，创建时传 `secrets: {ENV: name}`（secrets.md 第 92–96 行） | **保留**。照 Daytona，按 Sandbox 映射比 Coder「所有工作区都注入」更窄 |
| S4 | （没写）变量名限制 | 拒绝保留名和前缀：`GIT_`、`LD_`、`DYLD_`、`LC_`、`CODER_*`、`PATH`、`BASH_ENV`、`ENV`、`TMPDIR`、`SSH_AUTH_SOCK`……理由是 *"Blocking the entire GIT_* namespace avoids an arms race"*，`BASH_ENV` 那条的理由是 *"would inject arbitrary code into every shell"*（[C:codersdk/usersecretvalidation.go#L114-L212]） | 未查到 | **应修改**：映射时拒绝保留名，至少包括 `GIT_*`（第 9 行的 helper 配置走 `GIT_CONFIG_*`）、平台自己的前缀（Channel socket 路径变量）、`LD_*`、`BASH_ENV`/`ENV`、`PATH`、`HOME`。平台变量一律最后设置、覆盖用户值，这也是 Coder 的优先级（[C:agent/agent.go#L1755-L1760]） |
| S5 | （没写）大小和数量上限 | 每用户 50 个，单个 24 KiB；理由是 Windows 的 32 KiB 环境块（#L113-L127，[C:codersdk/usersecretvalidation.go#L40-L51]） | 未查到 | **应修改（小）**：写一个单值上限（Linux 单个环境字符串的上限是 `MAX_ARG_STRLEN` 128 KiB【推断】，取 64 KiB 就够了）和每用户数量上限。没有上限的话，过大的值会在 Exec 启动时以 `E2BIG` 失败，错误很难看懂 |
| S6 | 之后改动 Secret 只影响新建的 Sandbox | 工作区启动或 agent 重连时才拿到新值，*"restart the workspace"*（#L49-L53） | 【线上】占位符不变，代理换值，*"within 15 seconds"*；可以 `update_secrets` 原地增减 | **应修改（措辞）**。ADR-0009 说 restart 时 Runtime 由 `Create` 重建，那 stop→start 的 Sandbox 算不算「新」？建议照 Coder：**每次 Runtime Create（包括 restart）都按名字重新解析值**，不在 Sandbox 上快照值；如果引用的 Secret 已被删除，启动失败并指出是哪个名字【推断：fail-closed 比静默跳过更适合 Agent】。原地刷新不做，和 Coder 一致，Daytona 能做到是因为它有代理 |
| S7 | 不做文件交付 | 支持 file 目标，并且 *"Coder never deletes secret files it has written"*，路径冲突、残留文件都要专门写文档说明（#L83-L111） | 无文件交付 | **保留**。和 Daytona 一致；Coder 的那些文件语义说明了做文件交付的代价 |
| S8 | 平台从不把 Secret 写进 Workspace | 文件 secret 会写进工作区；env secret 只放在 agent 派生进程的环境里，不写进容器配置（[C:agent/agent.go#L1743-L1753]） | v0.190.0 的 env 写进 Docker 容器的 `Env`（[D:apps/runner/pkg/docker/container_configs.go#L87-L89]），在节点上 `docker inspect` 可见，也落在 Docker 元数据文件里 | **应修改（写清注入点）**【推断】：建议照 Coder，Node Agent 把值放进每个 Exec 的环境（ADR-0009 的 Exec 本来就接收 environment），而不是写进容器配置。这样 Secret 不进 Node 上的 Docker 元数据，也就不会在运行时元数据里活得比 Sandbox 还久 |
| S9 | 审计：Secret 的增删改，以及 Sandbox 创建时引用了哪些名字；任何地方都不记值 | `UserSecret` 在 audit 表里（[C:enterprise/audit/table.go]） | 未查到 | **保留** |
| S10 | 否决出口代理替换占位符 | 无此机制 | 【线上】有，但只替换请求头，**Basic Auth 不行**（secrets.md 第 729 行） | **保留否决**，并在否决理由里补上这条一手事实：git over HTTPS 用的是 Basic Auth，Daytona 自己的文档也说 Basic Auth helper 下占位符替换不了 |

## 4. 应简化 / 应修改清单

- **应简化**：
  - §2-2：Provider 只填 `base_url`，端点和 host pattern 按 type 推导，允许个别覆盖。
- **应修改**：
  - §2-5：PKCE 默认 S256，每个 provider 可以关闭。
  - §2-6：刷新要写清四点：同一 link 串行（进程内锁）、先落库、只有明确的永久错误才作废 link（403/429/5xx 都算暂时失败）、留刷新余量。
  - §2-7：下发前校验 token，校验失败返回授权 URL。
  - §2-8：改 Consequences 第 2 条，GitHub 删除 grant 会连带作废已经读走的 token；revoke 尽力而为。
  - §2-9：写清 helper 的接入机制（建议在 Exec 环境里用 `GIT_CONFIG_COUNT`：先清空，再设平台 helper），并把 Daytona 的「防止其他 helper `store` 落盘」作为理由写进去。
  - §2-10：host 不匹配任何 provider 时静默放行，不报错。
  - §2-11：按 type 固定返回的用户名（GitLab 用 `oauth2`）。
  - §2-17 / S2：加密 key 用文件引用，密文旁存 key id。
  - S4：Secret 的变量名要排除保留名和前缀。
  - S5：单值上限和数量上限。
  - S6：restart 时重新解析 Secret 值；引用的 Secret 缺失时启动失败。
  - S8：Secret 由 Node Agent 注入 Exec 环境，不写进容器配置。
- **文档补充（不改决定）**：§2-4 GitHub App 安装与过期时间；§2-13 只用内联 `GH_TOKEN=$(…)`；§2-15 用 `insteadOf` 改写 SSH remote；§2-18 私有 CA 打进 Snapshot；S10 否决理由补上 Basic Auth 事实。
