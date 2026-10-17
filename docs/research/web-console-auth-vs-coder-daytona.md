# Web 控制台认证：对照 Coder 与 Daytona

调研日期 **2026-10-17**。只用一手来源：钉死版本的源码和仓库内文档。厂商原文保留英文。推断标【推断】。

| 项目 | tag | commit | 简写 |
|---|---|---|---|
| coder/coder | v2.38.0 | `c3c6a672821e8ac2472c45f3be7a3c600aa8dae1` | `C/` = `https://github.com/coder/coder/blob/c3c6a672821e8ac2472c45f3be7a3c600aa8dae1/` |
| daytonaio/daytona | v0.190.0 | `01c502bb1f1ff8f2885d0cd490e043736083dca8` | `DT/` = `https://github.com/daytonaio/daytona/blob/v0.190.0/` |

Daytona 的 pin 取自 `recheck-identity-network-channel-vs-coder-daytona.md` 的表头。

---

## 0. 总表

| 问题 | Coder | Daytona |
|---|---|---|
| 浏览器怎么登录 | coderd 自己做：密码、GitHub OAuth、OIDC，都由 coderd 回调 | 浏览器 SPA 直接对外部 OIDC（Auth0，自托管用 Dex）走授权码流程，API 不参与登录 |
| 登录后拿到的凭据 | 一行 `api_keys`，和 CLI token 同表同格式 `${ID}-${SECRET}`，按 `login_type` 区分 | IdP 签的 RS256 JWT access token。API key 是另一种东西 |
| 会话过期 | 默认 24h，**滑动续期**（`login_type=token` 的除外） | 由 IdP 决定；SPA 在到期前 290s 静默续期 |
| 浏览器怎么带凭据 | HttpOnly cookie `coder_session_token` | `Authorization: Bearer <JWT>`，token 只在内存里 |
| cookie 和 header 能互换吗 | 能：cookie、query、`Coder-Session-Token`、`Bearer` 依次查找 | 没有 cookie。同一个 Bearer 槽位，按 JWT 正则分流给 JWT 或 API key 策略 |
| CSRF | nosurf 双提交：HTML 里嵌 token，前端带 `X-CSRF-TOKEN`；只有 cookie 认证才检查 | 不需要（没有 cookie 认证）；CORS `credentials: false` |
| 控制台怎么部署 | `site/` 用 `go:embed` 编进 coderd，与 `/api/v2` 同源 | API 进程用 `ServeStatic` 托管 `/dashboard`，同源；可关掉另行部署，API 开 `origin: true` 的 CORS |
| 控制台用没用私有接口 | 都是公开 REST，少量 `/api/experimental` 也在生成的 API 文档里 | 都是公开 REST，但用户 / 组织 / API key 管理类 controller **只收 JWT**，API key 调不了 |
| CLI 登录 | 打开 `/cli-auth`，页面用 cookie 调 `POST /users/me/keys` 生成新 key，用户手动粘贴回终端 | CLI 自己起 `localhost` 回调，对 IdP 走授权码流程；或者 `--api-key` |
| Web 终端 | 同源时直接用 session cookie 连 `/pty` WebSocket；跨域（workspace proxy）时先换一张 signed token 放 query | 打开端口 22222 的 **signed preview URL**，token 在 URL 里 |
| 端口预览 | subdomain app 走 `auth-redirect`，另发一枚 `application_connect` 范围的 API key，加密后重定向过去，写进 app 域 cookie | proxy 依次认 Bearer、preview token header / query、每沙箱 cookie、signed token，都失败时自己对 IdP 走 OAuth，校验后写 1h cookie |
| 外部 OAuth 授权（git provider） | `/external-auth/{id}/callback` 要求已登录（session cookie，不登录就跳登录页），state / PKCE 放 HttpOnly cookie | 没有 git provider OAuth。只有「关联登录身份」，转调 IdP 的 management API |
| 第一个管理员 | 有 Web 设置页（`/setup`，`POST /users/first`）；也可以 `coder login` 交互创建；OIDC / GitHub 的第一个用户自动成为 owner | 没有设置页。API 启动时建 `daytona-admin` 用户和 API key，打印在日志里；普通用户第一次带 JWT 访问时自动建 |

---

## 1. Coder：浏览器登录、凭据、cookie、CSRF

**三种登录方式都由 coderd 处理，结果都是 `createAPIKey`。**

- 密码：`POST /api/v2/users/login` → `createAPIKey(LoginType: password, DefaultLifetime: Sessions.DefaultDuration)`，把 cookie 写回去，同时在 body 里返回 `session_token`（`C/coderd/userauth.go#L504-L556`）。
- GitHub / OIDC：路由 `/users/oauth2/github/callback`、`/users/oidc/callback`，前面挂 `httpmw.ExtractOAuth2`（`C/coderd/coderd.go#L1655-L1669`）。回调最后也调 `createAPIKey(LoginType: params.LoginType)`（`C/coderd/userauth.go#L2076-L2087`）。还有 GitHub device flow：`/github/device`（`C/coderd/coderd.go#L1656`）。

**浏览器会话和 CLI token 是同一张表、同一种格式，只是 `login_type` 和寿命不同。**

- `createAPIKey` 先 `apikey.Generate`，再 `InsertAPIKey`，然后返回 cookie（`C/coderd/apikey.go#L570-L606`）。token 格式是 `${ID}-${SECRET}`（`C/coderd/httpmw/apikey.go#L1052-L1056`）。
- `coder tokens create` 走 `LoginType: token`，寿命用 `DefaultTokenDuration`（`C/coderd/apikey.go#L115-L120`），默认 7 天（`C/codersdk/deployment.go#L3540-L3543`）。
- 浏览器会话的 `--session-duration` 默认 24h："Sessions may last longer if they are actively making requests"（`C/codersdk/deployment.go#L3868-L3876`）。
- 滑动续期：非 `token` 类型的 key，剩余寿命少于 `lifetime - 1h` 时，把 `ExpiresAt` 推到 `now + lifetime`。原文："Programmatic API tokens (LoginTypeToken ...) honor a fixed, finite lifetime and must not be silently extended"（`C/coderd/httpmw/apikey.go#L439-L448`）。可以用 `--disable-session-expiry-refresh` 关掉。

**cookie。**

- 名字：`coder_session_token`（`C/codersdk/client.go#L33-L35`）。
- `HttpOnly: true`，`Path: /`，`MaxAge` 固定 1 年。原因是 "the server refreshes the key's ExpiresAt on activity but does not re-set the cookie"，过期由服务端判断（`C/coderd/apikey.go#L585-L605`）。
- `Secure` 和 `SameSite` 由 `HTTPCookies.Apply` 统一设置（`C/codersdk/deployment.go#L1210-L1220`）。`Secure` 默认等于 "access URL 是不是 https"（`#L3647-L3657`）。`SameSite` 只能是 `lax` / `none`，默认 `lax`："Do not allow "strict" same-site cookies. That would potentially break workspace apps."（`#L3659-L3669`）。另外可以开 `__Host-` 前缀（`#L1213-L1217`）。

**cookie 和 header 可以互换。** `APITokenFromRequest` 依次查 cookie、`?coder_session_token=`、`Coder-Session-Token` header、`Authorization: Bearer`、`?access_token=`（`C/coderd/httpmw/apikey.go#L975-L1015`）。

**CSRF：nosurf 双提交，只对 cookie 认证生效。**

- 中间件挂在全局（`C/coderd/coderd.go#L1217`），实现在 `C/coderd/httpmw/csrf.go#L19-L129`。
- 只检查 `/api` 和 `/oauth2/authorize` 的请求（`#L74-L77`）。没有 session cookie 的请求放行（"If no cookie is present, then there is no risk of CSRF"，`#L79-L84`）。header 里的 token 和 cookie 相同的放行（`#L86-L91`），`Bearer` 也放行（`#L113-L117`）。否则要求 `X-CSRF-TOKEN` 通过 `nosurf.VerifyToken`（`#L119-L125`）。
- token 的来源：coderd 渲染 `index.html` 时写进 `<meta property="csrf-token">`（`C/site/index.html#L21`、`C/site/site.go#L170-L174`）。前端 axios 读出来放进每个请求的 `X-CSRF-TOKEN` header（`C/site/src/api/api.ts#L4045-L4078`）。渲染后的 HTML 带 `no-store`（`C/site/site.go#L350-L356`）。

## 2. Coder：`coder login` 和 `/cli-auth`

- CLI 打开 `<server>/cli-auth`，然后提示 "Paste your token here:"，用 `GET /users/me` 校验粘贴进来的值（`C/cli/login.go#L341-L366`）。没有回调，也没有设备码，靠用户复制粘贴。
- `/cli-auth` 页面加载时调 `apiKey()` 查询（`C/site/src/pages/CliAuthPage/CliAuthPage.tsx#L6-L14`），也就是 `POST /api/v2/users/me/keys`（`C/site/src/api/api.ts#L566-L571`）。请求带的是浏览器的 session cookie 和 CSRF header。
- 服务端 `postAPIKey` 新建一枚 key：`LoginType: password`，寿命 `DefaultTokenDuration`（`C/coderd/apikey.go#L199-L227`）。CLI 代码注释说它 "should not show on the `tokens` page"（`C/cli/login.go#L371-L381`）。【推断】因为 `login_type` 不是 `token`，这枚 CLI key 也会滑动续期。

## 3. Coder：控制台的构建与托管

- `site/` 是 React SPA。`go:build embed` 时用 `//go:embed all:out` 编进二进制（`C/site/site_embed.go#L1-L13`）。slim 构建不带它（`C/site/site_slim.go#L1-L2`）。
- coderd 用 `site.New(...)` 生成 `SiteHandler`（`C/coderd/coderd.go#L774-L780`），挂在根路由的 `NotFound` 上（`#L2172`）。所以控制台和 `/api/v2` **同源**。
- 默认 CORS 只允许 `GET` / `OPTIONS`，`AllowCredentials: false`（`C/coderd/httpmw/cors.go#L32-L50`）。浏览器跨域调 API 不在设计里。
- 私有接口：控制台用的都是 REST。有少量 `/api/experimental/*`（例如 `C/site/src/api/api.ts#L362`、`#L3483`），它们也出现在生成的 API 文档里（`C/docs/reference/api/chats.md#L17-L21`）。只有少数端点标了 `@x-apidocgen {"skip": true}`，不进文档，例如 `/api/experimental/users/email`（`C/coderd/users.go#L886`）、`/api/experimental/watch-all-workspacebuilds`（`C/coderd/workspaces.go#L2321`）。【推断】这些是给 dashboard 用的未公开端点，数量很少。

## 4. Daytona：控制台登录与托管

**登录完全交给外部 OIDC，API 只验 JWT。**

- 控制台先调公开的 `GET /api/config`（`@Public()`，`DT/apps/api/src/config/config.controller.ts#L12-L27`），拿到 `oidc.issuer` / `clientId` / `audience`。然后用 `react-oidc-context` 对 IdP 走授权码流程，`redirect_uri` 是 `<origin>/dashboard`（`DT/apps/dashboard/src/providers/ConfigProvider.tsx#L21-L56`）。
- token 只存内存：非 localhost 时用 `InMemoryWebStorage`（`#L34-L35`）；在到期前 290s 续期（`#L46`）。
- 自托管时 IdP 是 Dex：compose 里 `OIDC_ISSUER_BASE_URL=http://dex:5556/dex`（`DT/docker/docker-compose.yaml#L23-L26`）。Dex 是 public client，开了密码库，有一个静态用户 `dev@daytona.io`（`DT/docker/dex/config.yaml#L12-L25`、`DT/docker/README.md#L20`、`#L36-L37`）。README 后半是 Auth0 配置指南（`DT/docker/README.md#L64-L69`）。CLI 的配置项也叫 `Auth0Domain`（`DT/apps/cli/cmd/auth/login.go#L145-L159`）。【推断】云版用的是 Auth0。

**控制台发给 API 的是 JWT access token。**

- `ApiClient` 用 `user.access_token` 构造，以 `Authorization: Bearer` 发出（`DT/apps/dashboard/src/providers/ApiProvider.tsx#L26-L28`、`DT/apps/dashboard/src/api/apiClient.ts#L50-L54`、`#L80-L88`）。
- API 端 `JwtStrategy` 从 Bearer 取 token，用 IdP 的 JWKS 验 RS256，检查 `audience` / `issuer`（`DT/apps/api/src/auth/jwt.strategy.ts#L35-L47`）。用户第一次出现时按 `sub` 自动建（`#L70-L80`）。

**JWT 和 API key 共用 Bearer 槽位，按形状分流。**

- `GlobalAuthGuard` 同时挂 `API_KEY` 和 `JWT` 两个策略（`DT/apps/api/src/auth/global-auth.guard.ts#L25`）。`ApiKeyStrategy` 先比对几把配置里的系统 key，然后 "Tokens matching JWT structure are not API keys — skip DB lookups and delegate to the JWT strategy"（`DT/apps/api/src/auth/api-key.strategy.ts#L75-L104`）。正则是 `^eyJ...\.eyJ...\....$`（`DT/apps/api/src/auth/constants/jwt-regex.constant.ts`）。
- **没写 `@AuthStrategy` 的端点默认只收 JWT**（`DT/apps/api/src/auth/global-auth.guard.ts#L70-L84`）。用户、组织、API key 这几个 controller 整类标成 `@AuthStrategy(JWT)`（`DT/apps/api/src/user/user.controller.ts#L42`、`DT/apps/api/src/api-key/api-key.controller.ts#L31`、`DT/apps/api/src/organization/controllers/organization.controller.ts#L60`），只有个别方法单独放开 API key（例如 `api-key.controller.ts#L109`）。所以「创建 API key」只能在控制台里做。

**托管。**

- 生产模式下 API 进程用 `ServeStaticModule` 在 `/dashboard` 托管控制台静态文件（`DT/apps/api/src/app.module.ts#L117-L134`）。启动时把构建产物里的 `%DAYTONA_BASE_API_URL%` 替换成配置值（`DT/apps/api/src/main.ts#L127-L148`）。默认 `apiUrl = window.location.origin + '/api'`（`DT/apps/dashboard/src/providers/ConfigProvider.tsx#L15`）。所以默认**同源**。
- `DONT_SERVE_DASHBOARD=true` 时不托管（`DT/apps/api/src/config/configuration.ts#L358`），可以另行部署。API 的 CORS 是 `origin: true, credentials: false`（`DT/apps/api/src/cors-options.ts#L8-L12`、`DT/apps/api/src/main.ts#L60`）。凭据走 header 而不是 cookie，所以跨域也能用。

## 5. Web 终端、端口预览、外部 OAuth

**Coder。**

- Web 终端连 `/api/v2/workspaceagents/{id}/pty` WebSocket。同源时直接复用 session cookie："If the URL is just the primary API, we don't need a signed token"。走 workspace proxy（跨域）时，先调 `POST /api/v2/applications/reconnecting-pty-signed-token` 换一张 signed token，放进 query 参数 `coder_signed_app_token_23db1dde`（`C/site/src/utils/terminal.ts#L28-L53`）。这个 query 参数 "is only acceptable on reconnecting-pty requests, not apps"（`C/codersdk/client.go#L71-L75`）。
- 端口 / app 预览分两种：
  - path app：和 API 同源，会带上 session cookie。官方文档建议生产环境关掉："Path-based apps share the same origin as the Coder API ... A malicious workspace could reuse Coder cookies to call the API"（`C/docs/tutorials/best-practices/security-best-practices.md#L71-L96`）。
  - subdomain app：`GET /api/v2/applications/auth-redirect`，用 session 认证后**另发一枚** `Scope: application_connect` 的 API key（寿命不超过当前会话），用 `AppEncryptionKeyCache` 加密后重定向到 app 域（`C/coderd/workspaceapps.go#L53-L140`）。app 域把它存进 `coder_subdomain_app_session_token_<hash>` cookie。另外还有短期 JWT cookie `coder_signed_app_token`（`C/codersdk/client.go#L54-L70`）。
- 外部 OAuth（git provider）：`/external-auth/{id}/callback`（旧名 `/gitauth`）前面挂 `apiKeyMiddlewareRedirect`，未登录就跳登录页（`RedirectToLogin: true`），然后才是 `ExtractOAuth2`（`C/coderd/coderd.go#L1119-L1127`、`#L1244-L1261`）。`ExtractOAuth2` 把 `state`、redirect、PKCE verifier 写进 HttpOnly cookie，回调时比对 state（`C/coderd/httpmw/oauth2.go#L188-L250`）。所以浏览器靠的是 session cookie 加 state cookie。

**Daytona。**

- Web 终端就是一个端口预览：控制台对端口 `22222` 调 `getSignedPortPreviewUrl`（`DT/apps/dashboard/src/hooks/useSandboxSession.ts#L24`、`#L136-L145`），对应 `GET /sandbox/:id/ports/:port/signed-preview-url`（`DT/apps/api/src/sandbox/controllers/sandbox.controller.ts#L1190-L1221`）。signed token "embeds the authentication token directly in the URL"，和 header 用的 preview token 不能互换（`DT/apps/docs/src/content/docs/en/preview.mdx#L13`、`#L20`、`#L100-L115`）。
- preview proxy 的认证顺序是：Bearer（API key 或 JWT）→ `x-daytona-preview-token` header / query → 每沙箱 cookie `daytona-sandbox-auth-<id>` → signed token。都失败时，非 toolbox 请求重定向到 IdP 登录（`DT/apps/proxy/pkg/proxy/auth.go#L17-L104`）。回调用 PKCE 换出 access token，调 API 的 `HasSandboxAccess` 校验，然后写一个 1h 的 HttpOnly securecookie（`DT/apps/proxy/pkg/proxy/auth_callback.go#L74-L127`、`DT/apps/proxy/pkg/proxy/proxy.go#L40`、`#L46`）。控制台的 JWT 不在 cookie 里，所以 proxy 不复用它，而是在 proxy 域上单独再登录一次。
- 外部 OAuth：API 里没有 git provider 的 OAuth 授权流程（在 `apps/api/src` 里搜 `gitprovider` / `git-provider` 没有结果）。只有「关联登录账号」：`POST /users/linked-accounts` 转调 `${oidc.issuer}/api/v2/users/{id}/identities`（`DT/apps/api/src/user/user.controller.ts#L116-L190`），provider 只有 `google-oauth2` / `github`（`DT/apps/api/src/user/enums/account-provider.enum.ts`）。【推断】这条路径是 Auth0 Management API 的形状，自托管 Dex 用不了。

## 6. 第一个管理员

**Coder：有 Web 设置页，CLI 也行。**

- `POST /api/v2/users/first` 只在用户数为 0 时可用，否则返回 409（`C/coderd/users.go#L177-L204`）。它豁免 CSRF（`C/coderd/httpmw/csrf.go#L43`）。
- 控制台 `SetupPage` 调这个接口（`C/site/src/api/api.ts#L1734-L1737`），GitHub 启用时还提供 GitHub 按钮（`C/site/src/pages/SetupPage/SetupPageView.tsx#L120-L129`）。
- `coder login` 发现还没有用户时，交互式询问 "Would you like to create the first user?"，或者用 `--first-user-*` 标志（`C/cli/login.go#L229-L234`、`#L290`、`#L410-L434`）。
- OIDC / GitHub 的第一个用户不受 `AllowSignups` 限制，并且自动成为 owner（`C/coderd/userauth.go#L1782-L1784`、`#L1848-L1849`）。

**Daytona：没有设置页，靠启动引导和 IdP。**

- API 启动时 `initializeAdminUser` 建 `daytona-admin` 用户（`SystemRole.ADMIN`），给它建一枚 API key（值可以由 `admin.apiKey` 配置），并把 key 打印在日志里（`DT/apps/api/src/app.service.ts#L172-L210`）。
- 人类用户由 IdP 管理（自托管时是 Dex 的 static password），第一次带 JWT 访问时由 `JwtStrategy` 自动建（`DT/apps/api/src/auth/jwt.strategy.ts#L70-L80`）。

---

## 未覆盖

- Coder 的 OIDC 回调里怎么映射 claims 到用户和组，没有逐行读。
- Daytona 云版实际用的 IdP 只从配置名和 README 推断，没有找到明确声明。
