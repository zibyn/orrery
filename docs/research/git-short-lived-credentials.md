# Git 短时凭证机制：按 Provider 对照

- 调研日期：2026-10-15
- 问题（`zibyn/stars-sandbox` issue #2）：Credential Broker 要按"谁授权了哪个仓库"换发短期、限定范围的凭据。要回答：各 git 供应商分别有哪些短时凭证机制，各自的最大寿命、能否限定到单个仓库、能否只授予权限子集、以及轮换与吊销方式；并回答底线问题——**能否做到"一次用户授权 → 换发一个限定到单个仓库、寿命几十分钟、可随时吊销的凭据"**；做不到时回退方案是什么。
- 对照对象：GitHub（云端）、GitLab（含自建与旧版本）、Gitea / Forgejo、裸 SSH。
- 用法：本文只服务上面这一个问题。每条非显然事实附一手来源 URL 与读取日期（均为 2026-10-15）。**我自己的判断标【推断】**；没取到一手来源的标 **未验证**，没有用记忆补。

## 0. 怎么读这份文档

- **§1 是结论**（事实 + 底线问题的回答）；**§2 是对照表**；§3–§6 按供应商逐项取证；**§7 是对设计的硬约束与回退**；**§8 是未验证清单**；附录是取证记录。
- 取证方式：GitHub 官方文档与 REST API 文档；GitLab 官方文档（`docs.gitlab.com` 的 Markdown 源）**加上 GitLab 你自己仓库的源码**（`gitlab.org/gitlab` 的 `master` 分支——文档没写死的地方以源码为准）；Gitea / Forgejo 官方文档、官方仓库源码与 `/swagger.v1.json`。
- 读的是各仓库的 **`master` / `forgejo` 主干**，不是发行 tag，发行版可能有细微出入（在相关条目里标注）。
- GitLab 的文档当前对应 **19.x**（文档里已出现"introduced in GitLab 19.1"），所以"旧版本"的差异在 §4.8 单独列。

## 1. 结论速览

### 1.1 已验证的事实

1. **只有 GitHub 同时满足"单仓库 + 权限子集 + 短时 + 可吊销"四个条件。** GitHub App installation token 寿命**固定 1 小时、不可延长也不可缩短**（REST 文档原话："Installation tokens expire one hour from the time you create them."；指南页原话："The installation access token will expire after 1 hour."），且换发时可以**再收窄一次**：`repositories` / `repository_ids` 指定仓库（上限 500 个），`permissions` 指定权限子集，两者都只能向下、不能超过 App 被授予的范围（"The installation access token cannot be granted permissions that the app was not granted."）。吊销有专门的端点：`DELETE /installation/token`。
2. **GitLab 没有任何机制能给出"几十分钟"的凭据。** 仓库级 + 短时两个条件**互斥**：能做到单仓库的是 project access token / deploy key / deploy token，它们的 `expires_at` 要么是**日历日期粒度（当晚 00:00 UTC 过期）**，要么默认永不过期；能到小时级的是 OAuth access token（7200 秒，2 小时），但它的 scope 是 `read_repository` / `write_repository`，作用于**该用户名下所有可达项目**，不能限定到单仓库。GitLab 自己的 scope 文档把三种 token 的触达范围写死了："A personal access token can access all groups and projects available to the user... A project access token can access only its project."
3. **GitLab 的 project access token 是"1 天粒度、可按需吊销"，不是"短时"。** `expires_at` 在 API 里类型是 `date`（`YYYY-MM-DD`），文档写明"tokens expire at midnight UTC on the expiry date"；默认与上限都是 365 天（17.6 起有 400 天的 feature flag）。**源码层面只校验了上限、没有校验下限**（`PersonalAccessToken#expires_at_before_instance_max_expiry_date` 只判 `expires_at > max_expiry_date`），所以理论上可以填"明天"——但那给出的是 **0～24 小时之间的不确定寿命**（取决于换发时刻离 00:00 UTC 多远），无法用来做确定性的 30 分钟租约。
4. **GitLab 唯一能真正做到"分钟级 + 单仓库"的是 deploy key。** deploy key 是 SSH 公钥，按项目授予，有 `can_push` 开关，而且它的 `expires_at` 在 API 里类型是 **DateTime**（ISO 8601，例 `2024-12-31T08:00:00Z`），文档也确认过期后 key 会被拒绝（"A deploy key is rejected when... The key has expired."）。代价是：它是 SSH 通道、要先生成密钥对再把公钥传上去。
5. **GitLab 的 deploy token 不能写代码。** 它的仓库 scope 只有 `read_repository`（"Read-only access to the repository using `git clone`"）；写权限只覆盖容器镜像与包仓库，不覆盖 git push。而且 deploy token **默认不过期**，GitLab 文档自己警告它"long-lived, making them attractive for attackers"。
6. **GitHub 的 fine-grained PAT 满足"单仓库 + 权限子集"，但不能用来做自动换发。** 它确实可以限定到具体仓库、可以给细粒度权限，但**没有创建它的 API**——`/orgs/{org}/personal-access-tokens` 这一组端点只有"列出 / 审核 / 吊销组织资源上的 token"，没有 create。也就是说 Broker 无法程序化地为某个用户换发一个 fine-grained PAT。Classic PAT 更差："Your personal access token (classic) can access every repository that you can access."
7. **GitHub OAuth app token 不能限定仓库。** scope 列表里最窄的与仓库相关的只有 `public_repo`（按可见性而不是按仓库选择），`repo` 是 "full access to public and private repositories"；整页没有任何"选择仓库"的机制。
8. **Gitea 的 PAT 既不限定仓库、也不过期。** 官方 swagger 的 `CreateAccessTokenOption` 只有 `name` 与 `scopes` 两个字段，没有 `repositories`、没有 `expires_at`；源码 `models/auth/access_token.go` 的 `AccessToken` 结构体也没有任何过期字段。它的 scopes 是**路由级**（`read:repository` 对应 `/repos/*`），不是仓库级；粒度 scope 本身也是 **v1.23 起**才有的。
9. **Forgejo 的 PAT 能限定仓库、但不过期。** Forgejo **v15.0（2026-04）** 起支持 "Specific repositories" 选项，swagger 的 `CreateAccessTokenOption` 有 `repositories` 字段（`RepoTargetOption`），源码 `models/auth/access_token.go` 里有 `ResourceAllRepos` 标志与 `AccessTokenResourceRepo` 关联表；但同样**没有 `expires_at`**。限定到具体仓库时可用 scope 只有 `read/write:repository` 与 `read/write:issue`，且失去一切管理员能力。
10. **Forgejo 的 OAuth2 是最差的一档。** 官方文档原话："OAuth2 scopes are not yet implemented."，并且"Third-party applications obtaining a token for a user via such an application will have administrative rights."——通过实例级 OAuth 应用拿到的 token 等于该用户的完全权限。Forgejo 文档自己给出建议："Scoped application tokens or SSH keys limited to interactions with the repository should be preferred in environments where security is a concern."
11. **Gitea 的 OAuth2 token 是短的（`expires_in: 3600`）但不是仓库级的**，scope 是路由级。Gitea 的 deploy key 有仓库级（含 2026 年新增的 HTTP deploy token，前缀 `gdt_`，`models/deploykey/deploykey_token.go`），但结构体里**没有过期字段**。
12. **裸 SSH 没有短时机制。** git over SSH 的认证就是公钥白名单，协议层不携带作用域也不携带有效期；仓库级只能靠各家的 per-repo deploy key，短时只能靠各家的过期字段。OpenSSH 自身的 user certificate（`ssh-keygen -s` 签发、带 validity interval）有有效期，但【推断】GitHub / GitLab / Gitea / Forgejo 都不接受用户证书（它们只做授权公钥的比对）。

### 1.2 底线问题的回答

**"一次用户授权 → 换发一个限定到单个仓库、寿命几十分钟、可随时吊销的凭据"：**

| Provider | 能否做到 | 推荐机制 | 差在哪 |
|---|---|---|---|
| **GitHub** | **能，且完全吻合** | **GitHub App installation token** | 无。寿命固定 60 分钟；换发时用 `repositories` + `permissions` 收窄到单仓库、`contents:write`；`DELETE /installation/token` 即时吊销 |
| **GitLab** | **不能**（差在寿命） | **project access token**（单项目、最小 scope、按需吊销）；若可走 SSH，改用 **deploy key + ISO8601 `expires_at`** | project access token 的过期是日历日期粒度、当天 00:00 UTC 生效，拿不到确定的几十分钟；deploy key 能做到分钟级但只走 SSH |
| **Gitea** | **不能**（差在寿命，且仓库级也弱） | **repo deploy key / 2026 新增的 HTTP deploy token**（仓库级、不过期）+ Broker 按需删除 | 仓库级 ✓ 短时 ✗；OAuth token 短时 ✓ 但只是路由级 scope ✗ |
| **Forgejo** | **半能**（仓库级 ✓ 短时 ✗） | **"Specific repositories" 的 PAT（v15.0+）** + Broker 按需吊销 | token 本身永不过期，租约长度完全由 Broker 的吊销时延决定 |
| **裸 SSH** | **不能** | 无。退化为 per-repo deploy key（不过期） | 协议层没有作用域与有效期这两个概念 |

**一句话**：只有 GitHub 提供了"短时 + 单仓库 + 可吊销"三者齐备的机制；其余供应商都必须把"短时"重新定义为**Broker 侧的租约**（换发 + 记账 + 到点用 API 吊销），而不是依赖凭据自身的寿命。

## 2. 对照表

| 供应商 | 机制 | 最大寿命 | 可否限定到单仓库 | 可否只给权限子集 | 轮换 | 吊销 |
|---|---|---|---|---|---|---|
| GitHub | **App installation token** | **1 小时（固定，不可调）** | ✅ 换发时 `repositories`/`repository_ids`（≤500），安装时也可选 "Only select repositories" | ✅ 换发时 `permissions`，只能向下 | 重新换发即可（1 小时一次） | ✅ `DELETE /installation/token`；卸载 App 亦可 |
| GitHub | Fine-grained PAT | 可选永不过期；上限受组织/企业策略约束（预填参数 1–366 天） | ✅ 可选具体仓库 | ✅ 细粒度权限 | ❌ 无创建/轮换 API | ⚠️ 仅组织管理员端点可吊销组织资源上的 token |
| GitHub | OAuth app token | 默认不过期（除非启用 expiring tokens / `offline_access`） | ❌ 无任何按仓库选择的 scope | ⚠️ 只有粗粒度 scope（`repo` 等） | refresh token（若启用） | 用户撤销授权 |
| GitHub | Deploy key | **永不过期**（官方原话："Deploy keys are credentials that don't have an expiry date."） | ✅ 仅一个仓库 | ⚠️ 只有 write 开关（默认只读） | 手工换 key | ✅ 删除 key |
| GitLab | **Project access token** | 365 天（默认=上限；17.6 起有 400 天 flag） | ✅ 只能访问本项目 | ✅ scope 列表（`read/write_repository`、`api`…） | ✅ API rotate | ✅ API 吊销，立即失效 |
| GitLab | Group access token | 同上 365 天 | ❌ 只能到组，不能收到组内单个项目 | ✅ scope 列表 | ✅ | ✅ |
| GitLab | Deploy token | **默认不过期**（可选设日期） | ✅ 项目级（组级则覆盖全组） | ⚠️ 仓库只有 `read_repository`，**不能 push** | 手工换 | ✅ |
| GitLab | **Deploy key** | **`expires_at` 是 ISO8601 datetime → 可到分钟级** | ✅ 项目级 | ⚠️ 只有 `can_push` 开关 | 换密钥对 + 重新上传公钥 | ✅ |
| GitLab | OAuth token | **7200 秒（2 小时）**（自建可配，19.1 起） | ❌ scope 面向用户可达项目，不能收窄 | ⚠️ scope 粗（`read/write_repository`） | ✅ refresh token | 撤销授权/删应用 |
| GitLab | Personal access token | 默认 + 上限 365 天（日期粒度） | ❌ 用户全部可达项目 | ✅ scope 列表 | ✅ | ✅ |
| GitLab | Fine-grained PAT | 同 PAT（日期粒度、上限 365 天） | ✅ 可选具体项目（资源边界） | ✅ 细粒度权限 | ❌（未提供 API 创建，**未验证**） | ⚠️ 同 PAT |
| GitLab | CI/CD job token | **仅作业期间**（单项目） | ✅ | ⚠️ 端点白名单 | n/a | n/a（只在流水线内可用） |
| Gitea | OAuth2 token | `expires_in: 3600` + refresh token | ❌ scope 是路由级 | ⚠️ 路由级（v1.23+） | ✅ refresh token | 撤销授权 |
| Gitea | PAT | **不过期**（swagger 无 `expires_at`） | ❌ 无 `repositories` 字段 | ✅ 路由级 scopes | API 删除+重建 | ✅ API 删除 |
| Gitea | Deploy key / deploy token | **不过期**（结构体无过期字段） | ✅ 单仓库 | ⚠️ 访问模式（read/write） | 重新生成 | ✅ 删除 |
| Forgejo | OAuth2 token | 未说明 | ❌ **scopes 未实现**，第三方应用拿到管理员权限 | ❌ | — | — |
| Forgejo | **PAT（Specific repositories）** | **不过期** | ✅ v15.0+ 可选具体仓库 | ✅ 限 `read/write:repository`、`read/write:issue` | API 删除+重建 | ✅ API 删除 |
| 裸 SSH | 公钥 | 永不过期 | ❌（协议层无作用域） | ❌ | 手工 | 从 `authorized_keys` 删除 |

## 3. GitHub

### 3.1 GitHub App installation token —— 唯一完全满足的方案

- **寿命固定 1 小时，且没有 `expires_at` 请求参数**：REST 文档 "Installation tokens expire one hour from the time you create them."；指南页 "The installation access token will expire after 1 hour."。响应体里的 `expires_at` 是只读字段，不是入参。（[Create an installation access token](https://docs.github.com/en/rest/apps/apps#create-an-installation-access-token-for-an-app)，[指南页](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-an-installation-access-token-for-a-github-app)）
- **可以在换发时再收窄一层**（这正是"一次用户授权 → 每次任务换发一个更窄的凭据"）：`repositories`（仓库名数组）/ `repository_ids`（ID 数组，上限 500）；`permissions`（权限对象，取值是固定枚举，多数是 `read`/`write`，`contents` 属于这一类）。两条硬边界：不能给出 App 没被授予的权限，也不能给出 installation 没被授予的仓库。
- **权限子集可行**：`contents: write` 可以单独给出，不需要 `admin`。
- **吊销**：`DELETE /installation/token` —— "Once an installation token is revoked, the token is invalidated and cannot be used."（[Revoke an installation access token](https://docs.github.com/en/rest/apps/installations#revoke-an-installation-access-token)）
- **单仓库授权的入口**：安装 App 时可选 "All repositories" 或 "Only select repositories"，后者在下拉里挑具体仓库。（[Installing your own GitHub App](https://docs.github.com/en/apps/using-github-apps/installing-your-own-github-app)）
- **设计上要注意的**：installation 的 `repository_selection` 字段只有 `all` / `selected` 两种；把 installation 限定到单仓库意味着"每个仓库一次安装"，对"很多仓库、每个任务只碰一个"的场景更合适的做法是**一次安装覆盖一批仓库、每次换发时用 `repositories` 收窄到一个**。这是【推断】，但两个参数的行为是文档写明的。
- **一个未定的坑**：GitHub 从 2026-04-27 起在灰度新的无状态 token 格式 `ghs_APPID_JWT`，文档明确警告假定 40 字符 installation token 的工具有可能会坏。Broker 不应解析 token 内容，只当不透明字符串用。

### 3.2 Fine-grained PAT —— 条件符合，但不可自动化

- 仓库限定与权限子集都有："Each token can be further limited to only access specific repositories for that user or organization." / "Each token is granted specific, fine-grained permissions."
- 过期是**可选**的："Infinite lifetimes are allowed but may be blocked by a maximum lifetime policy set by your organization or enterprise owner."；UI 预填参数 `expires_in` 是 "Integer between 1 and 366, or `none`"，默认 30 天。
- **致命点**：REST API 里没有创建 fine-grained PAT 的端点。`/orgs/{org}/personal-access-tokens` 一组只有列表 / 审核请求 / 吊销组织资源上的 token。所以 Broker 不能为某个用户程序化地预置一个仓库级 PAT。
- Personalized 的补充事实：组织可以阻止 fine-grained PAT，或要求审批；未审批前 "Your token will only be able to read public resources until it is approved."
- （[Managing your personal access tokens](https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/managing-your-personal-access-tokens)、[REST API endpoints for personal access tokens](https://docs.github.com/en/rest/orgs/personal-access-tokens)）

### 3.3 OAuth app token

- 无仓库级 scope。最窄的仓库相关 scope 是 `public_repo`（按可见性限制），`repo` 是 "Grants full access to public and private repositories including read and write access to code"。
- `offline_access` "Requests an expiring access token and a refresh token"——暗示默认 OAuth token 不设置过期。
- 结论：不满足。

### 3.4 Deploy key

- 单仓库、`write` 开关、**无过期**："Deploy keys are credentials that don't have an expiry date."；"Deploy keys only grant access to a single repository."
- 满足"单仓库"，不满足"短时"。适合作为最后的兜底（仓库级 + 按需删除），但它是长期密钥，和 Credential Broker 的目标直接冲突。
- （[Managing deploy keys](https://docs.github.com/en/authentication/connecting-to-github-with-ssh/managing-deploy-keys)）

## 4. GitLab

### 4.1 三种 token 的触达范围（官方一句话）

GitLab 的 scope 文档写得最清楚：

> A personal access token can access all groups and projects available to the user.
> A group access token can access the subgroups and projects in its group.
> A project access token can access only its project.

（[Access token scopes](https://docs.gitlab.com/security/tokens/access_token_scopes/)）

**要在 GitLab 做到"单仓库"，选择只剩三种：project access token、project deploy key、project deploy token。**

### 4.2 Project access token

- 只作用于本项目；scope 可选（`read_repository` / `write_repository` / `api` / …）。
- **过期是必需的、日期粒度、默认即上限 365 天**：`expires_at` 在 API 里类型是 `date`，格式 `YYYY-MM-DD`；文档："tokens expire at midnight UTC on the expiry date"；"By default, the expiry date cannot be more than 365 days from today."；17.6 起 `buffered_token_expiration_limit` flag 可把上限提到 400 天。不填时源码回落到 `MAX_PERSONAL_ACCESS_TOKEN_LIFETIME_IN_DAYS.days.from_now`（=365）。
- **源码确认没有下限校验**：`app/models/personal_access_token.rb` 的校验只有 `expires_at_before_instance_max_expiry_date`（判 `expires_at > max_expiry_date`）。所以"能不能填明天"取决于 API 的 `Date` 解析与 UI，**模型层不拦**——但那给出的是"到明天 00:00 UTC 为止"的 0～24 小时不确定寿命，不能用作确定性租约。
- 创建需要 **Maintainer 或 Owner**（自建实例上还得考虑 bot user / 许可证差异；GitLab.com 上 project access token 需 Premium/Ultimate，自建任意 license 都有）。
- 吊销走 API，立即失效，且"this action cannot be undone"。
- （[Project access tokens](https://docs.gitlab.com/user/project/settings/project_access_tokens/)、[Project access tokens API](https://docs.gitlab.com/api/project_access_tokens/)）

### 4.3 Group access token

- 覆盖"组及其下所有项目"，**没有任何机制把它收到组内单个项目**。
- 创建需组 Owner；GitLab.com 上需 Premium/Ultimate，且试用期内不可用。
- 适合"一个组 = 一个工作域"的场景，不适合"一个仓库一个租约"。

### 4.4 Deploy token

- 项目级或组级；scope 里与仓库相关的**只有 `read_repository`**（只读 clone）；写权限只覆盖容器镜像/包仓库，**不能 push 代码**。
- **默认不过期**，可选日期；文档自评："long-lived, making them attractive for attackers."
- 额外限制：不支持 SSH 认证，也"can't be used with the GitLab public API"。
- 结论：不适合让 agent 改代码。
- （[Deploy tokens](https://docs.gitlab.com/user/project/deploy_tokens/)）

### 4.5 Deploy key —— GitLab 上唯一能做分钟级的机制

- 项目级 SSH 公钥；`can_push` 控制读写。
- **`expires_at` 是 DateTime**：API 文档 "Expiration date for the deploy key. Does not expire if no value is provided. Expected in ISO 8601 format (`2024-12-31T08:00:00Z`)"；源码 `lib/api/deploy_keys.rb` 里参数类型是 `optional :expires_at, type: DateTime`。
- 过期会被拒绝：文档 "A deploy key is rejected when... The key has expired."，且"cannot be used for any repository operations, including pulls and pushes"。
- 形态上需要"生成密钥对 → 上传公钥 → 把私钥交给沙箱"，吊销就是删除 key。比 token 重，但这是 GitLab 侧唯一能真正给"几十分钟"的。
- （[Deploy keys API](https://docs.gitlab.com/api/deploy_keys/)、[Deploy keys](https://docs.gitlab.com/user/project/deploy_keys/)）

### 4.6 OAuth —— 短，但不限仓库

- access token **`expires_in: 7200`（2 小时）**，带 refresh token（refresh 会使旧 access/refresh token 立即失效）；自建实例可调存活时间（"introduced in GitLab 19.1"，自建与 Dedicated）。
- scope：`read_repository` / `write_repository` 描述为"private projects"上的仓库读写——**面向该用户可达的项目**，文档里**没有任何**把 OAuth 应用限制到特定项目的机制。所以 2 小时 ✓、单仓库 ✗。
- （[OAuth 2.0 identity provider API](https://docs.gitlab.com/api/oauth2/)、[Configure GitLab as an OAuth 2.0 authentication identity provider](https://docs.gitlab.com/integration/oauth_provider/)）

### 4.7 CI/CD job token —— 短且单项目，但只在流水线内

- 文档称其为 "short-lived token valid only for the duration of a job"，作用范围由**触发作业的那个用户**的授权决定，并有 allowlist 控制可访问端点。
- 它不是给外部沙箱用的机制（只能在运行中的 job 里存在），但值得记录：**GitLab 自己承认"短时"这一档只有 job token 属于**。

### 4.8 依赖版本的部分

| 能力 | 版本门槛 |
|---|---|
| 非过期 access token | **15.4 宣布弃用，16.0 移除**（[deprecations](https://docs.gitlab.com/update/deprecations/)）——16.0 之前的实例可以造出永不过期的 PAT/project token |
| token 最大寿命可配、400 天上限 flag | **17.6** 起（`buffered_token_expiration_limit`，默认关） |
| deploy key 过期 | 文档有"Expiration date"字段与过期拒绝逻辑（具体引入版本 **未验证**） |
| fine-grained PAT | **18.10 beta、19.2 GA** |
| fine-grained PAT 强制启用 | 18.11 起（flag），19.2 自建 GA；**不适用于 group/project access token 与 service account** |
| OAuth token 寿命可配 | **19.1**（自建 / Dedicated） |
| Group access token | GitLab.com 需 Premium/Ultimate；自建任意 license |

**对自建旧实例的含义**：16.0 之前的 GitLab 上"永不过期"是默认选项，Broker 不能假设 token 一定会自己到期；14.x/15.x 没有 deploy key 过期字段的话，"分钟级"在 GitLab 上就彻底没有实现路径。

## 5. Gitea / Forgejo

### 5.1 Gitea

- **OAuth2**：access token `expires_in: 3600`（1 小时）+ refresh token，**短时 ✓**；但 scopes 是**路由级**（`read:repository` ↔ `/repos/*`），**没有任何按仓库选择的机制** ✗。粒度 scope 是 **v1.23** 起才有；在此之前第三方应用拿的是完全权限（文档原话："the application gains full access to all of the user's public and private resources"）。
- **PAT**：官方 swagger 的 `CreateAccessTokenOption` 只有 `name` 与 `scopes`；**没有 `repositories`、没有 `expires_at`**。源码 `models/auth/access_token.go` 的 `AccessToken` 结构体字段只有 ID/UID/Name/Token 系列/Scope/时间戳——**没有过期字段**。→ 既不限仓库、也不过期。
- **Deploy key / deploy token**：仓库级 ✓。源码 `models/deploykey/deploykey.go` 的 `DeployKey` 有 `KeyType`（`KeyTypeSSH` / `KeyTypeToken`）、`RepoID`、`Mode`，**没有过期字段**；2026 年新增的 HTTP deploy token 见 `models/deploykey/deploykey_token.go`（前缀 `gdt_`，256 bit 熵，"authenticates git HTTP requests for one repository"）。→ 单仓库 ✓ 短时 ✗。
- 结论：Gitea 上**不存在**同时满足单仓库 + 短时的机制。回退是"仓库级 key/token（不过期）+ Broker 按需删除"。
- （[OAuth2 provider](https://docs.gitea.com/development/oauth2-provider)、[Gitea swagger](https://gitea.com/swagger.v1.json)）

### 5.2 Forgejo

- **OAuth2**：文档原话 "OAuth2 scopes are not yet implemented."，并且实例级 OAuth 应用签发的 token 对用户而言"will have administrative rights"。→ 不可用。
- **PAT**：**v15.0（2026-04）起支持 "Specific repositories"**。文档描述三种选项（All / Public only / Specific repositories）；选 Specific 时只能配 `read:repository`、`write:repository`、`read:issue`、`write:issue` 四个权限，且**明确剥夺一切管理员能力**（不能转移仓库、不能加协作者、不能改可见性）。swagger 的 `CreateAccessTokenOption` 有 `repositories`（`RepoTargetOption`）→ **可程序化创建**。源码 `AccessTokenResourceRepo` 是 token↔repo 的关联表，`AccessToken.ResourceAllRepos=false` 时生效。→ 单仓库 ✓。
- **但 token 不过期**：swagger 里没有 `expires_at`，结构体里也没有过期字段。→ 短时 ✗，租约长度完全由 Broker 的吊销时延决定。
- Forgejo 文档自己给出的排序："Scoped application tokens or SSH keys limited to interactions with the repository should be preferred in environments where security is a concern."
- （[Access Token Scope](https://forgejo.org/docs/latest/user/token-scope/)、[OAuth2 provider](https://forgejo.org/docs/latest/user/oauth2-provider/)、[Codeberg swagger](https://codeberg.org/swagger.v1.json)）

## 6. 裸 SSH

- git over SSH 的认证就是"公钥在白名单里"，**协议层没有作用域也没有有效期**这两个概念。仓库级只能靠服务端的 per-repo deploy key；短时只能靠服务端自己的过期字段（GitLab deploy key 是四家里唯一支持 datetime 过期的）。
- OpenSSH 的 user certificate（`ssh-keygen -s ca_key -V +30m`）确实带 validity interval，但它要求服务端把 CA 配成信任锚。**四家托管产品都不吃用户证书**（【推断】：它们的 `authorized_keys` 只做授权公钥比对；未找到任何一家文档提到 SSH CA）——所以这条路径只在"自己写 git+ssh 网关"时成立，不属于任何一家 provider 的机制。
- 结论：裸 SSH 上没有任何短时机制，是长期密钥。

## 7. 对设计的硬约束与回退

**A. "租约"必须是 Broker 的概念，不能是 provider 的概念。**
只有 GitHub 的凭据自身是短时的。其余供应商上，Broker 必须自己维护"换发时刻 + 期望 TTL + 到期吊销"的记账——因为 provider 侧的到期时间可能比期望值长两三个数量级（GitLab 365 天 vs 期望 30 分钟）。这意味着**Broker 的崩溃/失联会直接表现为租约泄漏**，所以"到期吊销"不能只挂在内存定时器上，必须有可重放的持久化记录（【推断】，属于设计建议）。

**B. 所有方案里，Broker 都持有一个长期的高权限换发凭证。**

| Provider | Broker 需要持有什么 | 备注 |
|---|---|---|
| GitHub | GitHub App 的私钥 | 只能换出 ≤1 小时、范围只能是 App 已获授权的子集 |
| GitLab | 一个 Maintainer/Owner 的 PAT（用于调 `POST /projects/:id/access_tokens`）或能建 deploy key 的凭证 | Broker 自己的这把密钥就是最高价值目标 |
| Gitea | 用户 PAT 或管理员 token（用于建 deploy key / 删 token） | |
| Forgejo | 同上（v15+ 才能建仓库级 PAT） | |

所以"agent 与代码接触不到长期密钥"这个目标**在 GitHub 上是 provider 保证的，在其余三家上只由"Broker 是唯一持有者"这个边界保证**。这把整个系统的安全性押在 Broker 自身的凭证保护上。

**C. "一次用户授权"在所有 provider 上都有额外前提。**
GitHub App 的安装需要用户在目标仓库上有 admin（GitLab 侧对应的是 Maintainer/Owner）。也就是说"用户授权"不只是"用户同意"，还要求用户在目标仓库上有足够高的权限。设计上要把"用户不具备该仓库的授权能力"当成一等错误，而不是异常。

**D. 会迫使设计改变的三个点：**
1. **如果产品要对 GitLab 也承诺"确定的几十分钟"**，就必须接受 SSH 形态（deploy key + ISO8601 过期），或者把租约语义降级成"吊销时延有界"。二选一，没有第三条路。
2. **如果要对 Gitea 承诺"仓库级"**，只能用 SSH deploy key 或 2026 新增的 HTTP deploy token——它们都不过期，租约完全靠 Broker 吊销。
3. **如果目标里有 GitLab 自建旧版本（<16.0）**，"永不过期"是默认，Broker 不能依赖 token 自然到期；<18.10 没有 fine-grained PAT。

**E. 回退方案（按 provider）：**
- **GitHub**：无回退，直接满足。
- **GitLab**：主选 project access token（单项目、最小 scope、按需 API 吊销），把"短时"实现为"Broker 记账 + 到点吊销"；若 SSH 可接受，改用 deploy key + datetime 过期拿到真正的分钟级。
- **Gitea**：仓库级 SSH deploy key 或 HTTP deploy token（不过期）+ Broker 按需删除；不要用 OAuth token（1 小时但作用域是全用户的仓库）。
- **Forgejo**：v15.0+ 的 "Specific repositories" PAT + Broker 按需吊销；不要用 OAuth2 token（scopes 未实现，等于管理员权限）。
- **裸 SSH**：只能退化为长期 deploy key。

## 8. 未验证 / 待确认

- **GitLab**：API/UI 是否对 `expires_at` 施加"必须是明天以后"的下限。（源码层面只看到上限校验；文档也没写下限。这句"at least one day in the future"只在二手资料里出现，未在一手来源里找到。）
- **GitLab**：deploy key 过期字段是哪个版本引入的（文档没有 history 条目）。
- **GitLab**：fine-grained PAT 是否能通过 API 创建（文档只写了 UI 步骤，未确认有 create 端点）。
- **GitLab**：`read_repository` / `write_repository` 的 OAuth token 是否严格覆盖"该用户所有可达项目"——文档只写了 private projects，未写"所有"；结论是从"没有任何按项目收窄的机制"反推的。
- **Gitea**：`ACCESS_TOKEN_EXPIRATION_TIME` 的默认值与是否可配，未从源码确认（文档示例只给了 `expires_in: 3600`）。
- **Gitea / Forgejo**：deploy key（含 HTTP deploy token）是否支持任何形式的过期——swagger 与结构体都没看到，但未穷尽检查所有相关端点。
- **Forgejo**：PAT 是否真的完全没有过期机制（结构体无字段、swagger 无参数，但未查 release notes 的完整说明）。
- **GitHub**：installation token 除 `DELETE /installation/token` 之外是否还有别的吊销粒度（如"吊销单个 token 而不影响 installation"）——文档只看到这一个端点。
- **GitHub**：`ghs_APPID_JWT` 新格式对 `repositories` / `permissions` 收窄行为是否有变化（灰度中）。
- 四家是否接受 SSH user certificate（本文按【推断】处理为"不接受"）。

## 附录：取证记录

| 事实 | 一手来源 | 读法 |
|---|---|---|
| installation token 1 小时、无 `expires_at` 入参 | [docs.github.com/en/rest/apps/apps](https://docs.github.com/en/rest/apps/apps#create-an-installation-access-token-for-an-app) | REST 文档正文 |
| `repositories` / `repository_ids` / `permissions` 只能向下收窄 | 同上 | REST 文档正文 |
| `DELETE /installation/token` 吊销 | [docs.github.com/en/rest/apps/installations](https://docs.github.com/en/rest/apps/installations#revoke-an-installation-access-token) | REST 文档正文 |
| 安装时 "Only select repositories" | [Installing your own GitHub App](https://docs.github.com/en/apps/using-github-apps/installing-your-own-github-app) | 官方指南 |
| fine-grained PAT 仓库限定 / 过期可选 / 组织策略 | [Managing your personal access tokens](https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/managing-your-personal-access-tokens) | 官方文档 |
| 无 fine-grained PAT 创建端点 | [REST API endpoints for personal access tokens](https://docs.github.com/en/rest/orgs/personal-access-tokens) | 端点清单全文 |
| OAuth scope 列表与 `repo` 语义 | [Scopes for OAuth apps](https://docs.github.com/en/apps/oauth-apps/building-oauth-apps/scopes-for-oauth-apps) | 官方文档 |
| deploy key 单仓库、无过期 | [Managing deploy keys](https://docs.github.com/en/authentication/connecting-to-github-with-ssh/managing-deploy-keys) | 官方文档 |
| GitLab 三种 token 的触达范围 | [Access token scopes](https://docs.gitlab.com/security/tokens/access_token_scopes/) | 文档 Markdown 源 |
| project access token 单项目、365 天、日期粒度 | [Project access tokens](https://docs.gitlab.com/user/project/settings/project_access_tokens/)、[API](https://docs.gitlab.com/api/project_access_tokens/) | 文档 |
| GitLab 只校验过期上限、无下限 | [`app/models/personal_access_token.rb` @master](https://gitlab.com/gitlab-org/gitlab/-/blob/master/app/models/personal_access_token.rb) | 官方仓库源码 |
| deploy key `expires_at` 是 DateTime | [`lib/api/deploy_keys.rb` @master](https://gitlab.com/gitlab-org/gitlab/-/blob/master/lib/api/deploy_keys.rb)、[Deploy keys API](https://docs.gitlab.com/api/deploy_keys/) | 源码 + 文档 |
| deploy token 只能只读 clone、默认不过期 | [Deploy tokens](https://docs.gitlab.com/user/project/deploy_tokens/) | 文档 |
| OAuth `expires_in: 7200`、无按项目收窄 | [OAuth2 API](https://docs.gitlab.com/api/oauth2/)、[OAuth provider](https://docs.gitlab.com/integration/oauth_provider/) | 文档 |
| job token 短时且单项目 | [CI/CD job token](https://docs.gitlab.com/ci/jobs/ci_job_token/) | 文档 |
| fine-grained PAT beta 18.10 / GA 19.2 | [Fine-grained personal access tokens](https://docs.gitlab.com/auth/tokens/fine_grained_access_tokens/) | 文档 Markdown |
| 非过期 token 15.4 弃用 / 16.0 移除 | [GitLab deprecations](https://docs.gitlab.com/update/deprecations/) | 文档 |
| Gitea OAuth `expires_in: 3600`、路由级 scope、v1.23 起粒度 | [Gitea OAuth2 provider](https://docs.gitea.com/development/oauth2-provider) | 文档 |
| Gitea PAT 无 `repositories` / 无 `expires_at` | [gitea.com/swagger.v1.json](https://gitea.com/swagger.v1.json) | 官方 swagger |
| Gitea `AccessToken` / `DeployKey` 无过期字段 | [models/auth/access_token.go](https://github.com/go-gitea/gitea/blob/main/models/auth/access_token.go)、[models/deploykey/deploykey.go](https://github.com/go-gitea/gitea/blob/main/models/deploykey/deploykey.go)、[deploykey_token.go](https://github.com/go-gitea/gitea/blob/main/models/deploykey/deploykey_token.go) | 官方仓库源码 |
| Forgejo OAuth scopes 未实现、第三方应用有管理员权限 | [Forgejo OAuth2 provider](https://forgejo.org/docs/latest/user/oauth2-provider/) | 文档 |
| Forgejo "Specific repositories" 及其权限限制 | [Forgejo Access Token Scope](https://forgejo.org/docs/latest/user/token-scope/) | 文档 |
| Forgejo PAT 可指定 `repositories`、无 `expires_at` | [codeberg.org/swagger.v1.json](https://codeberg.org/swagger.v1.json)、[models/auth/access_token.go](https://codeberg.org/forgejo/forgejo/src/branch/forgejo/models/auth/access_token.go)、[access_token_resource.go](https://codeberg.org/forgejo/forgejo/src/branch/forgejo/models/auth/access_token_resource.go) | swagger + 源码 |
