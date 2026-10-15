# 沙箱的文件进出与产物取回：先行形态调研

面向 `#14`（Resource Manager 的资源寻址与权限）。补 `prior-art-sandbox-orchestration.md` 缺的一节：那份回答了"任务怎么下发、状态怎么回流、凭据怎么注入"，**没有回答"文件怎么进、产物怎么出"**。

> **可信度声明**：本节结论来自搜索引擎对一手文档的摘要转述，URL 指向一手页面但**未逐页打开原文核对**。凡属推断我都标了。这与仓库其他四份调研的取证强度不同，取其"结构形状"够用，取"确切数字"请回到原文。
>
> ⚠️ **2026-10-15：本文 §1 已被 `vendor-file-apis.md` 取代**（那份逐页打开了一手文档页）。其中「E2B 把『忘了下载产物』列为结果丢失的头号原因」一条**已撤回**——E2B 一手页面里没有这句话，见那份的 §5.2。

## 0. 先读这三条

1. **每个平台都有一条"按路径寻址的文件 API"。** 没有平台需要你声明"资源引用"、申请"资源授权"才能把一个文件放进沙箱——你拿着沙箱的凭据，`write(path, bytes)` 就完了。**"资源 + 授权 + 物化"这一整套在先行形态里不存在。**
2. **产物取回 = 文件 API 或 对象存储 + 短时预签名 URL。** 没有第二种形状。TTL 从 **60 秒**（Buildkite REST 302）到 **10 分钟**（Buildkite GraphQL）到"只要产物还在"（GitHub Actions 认证下载）。
3. **先行形态默认有网，所以"文件怎么进来"通常不是平台功能。** E2B `allowInternetAccess` 默认 `true`；Modal 除非 `block_network` 否则通网。agent 要数据就自己 `git clone` / `curl` / `pip install`。**平台的文件 API 主要是给沙箱外面的调用方用的**，不是给沙箱里的 agent 用的。我们要禁出网（`#16` 未定）+ 禁凭据入沙箱（ADR-0001），等于把别人靠"有网"绕过去的那件事，逼成必须由平台提供的功能。

## 1. 进沙箱：清一色的"按路径寻址的文件 API"

| 平台 | 写入 | 读取 | 传输形态 | 鉴权 |
|---|---|---|---|---|
| **E2B** | `files.write(path, data)`（text/bytes/文件对象，`write_files` 批量） | `files.read(path, format="bytes")` | SDK → 平台 API，二进制常 base64 | 沙箱 API key |
| **Daytona** | `sandbox.fs.uploadFile(content, path)`；REST `POST /files/upload?path=`（multipart） | `GET /files/download?path=`（`application/octet-stream`） | **HTTP 字节流**，有流式版 | 沙箱凭据 |
| **Vercel Sandbox** | `sandbox.writeFiles([{path, content: Buffer, mode}])` | `readFile()` / `readFileToBuffer()` / `downloadFile()` | SDK → 网络边界内流式 | OIDC 或 `VERCEL_TOKEN` |
| **Runloop** | `devbox.file.write()`（小文本）/ `devbox.file.upload()`（大/二进制） | `devbox.file.read()` / `download()` | 两档，按体积分 | API key |
| **OpenHands** | `POST /api/conversations/{id}/upload-files`（multipart） | `select-file` / `zip-directory`（整目录打包） | HTTP multipart / zip | 会话凭据 |
| **Modal** | `sandbox.filesystem.write` | `sandbox.filesystem.read` | SDK | API token |
| **Cloudflare** | `Files` API（`writeFile`/`listFiles`） | 同 | Worker → 容器 | Worker 绑定 |

要点：

- **寻址就是文件系统路径**，不是资源 URI。没有 `s3://`、没有实例、没有路径不透明性这一说。
- **没有"操作集"概念**：能写就是能写，权限粒度在"谁持有沙箱凭据"这一层，不在资源这一层。
- E2B 反复强调的一条踩坑：**文件随沙箱上下文销毁**，必须在 `kill()` 之前下载——"忘了下载产物"被列为结果丢失的头号原因。这印证 `#7`"产物在过程中上传、不是拆除时"的取向。

## 2. 大文件与数据集：对象存储 + 挂载（有人做，也有代价）

- ⭐ **Cloudflare `mountBucket()`**：把 S3 兼容存储（R2 / S3 / GCS）用 **s3fs-fuse** 挂成沙箱内的一个路径。三种模式里有两种**不需要给容器凭据**：
  - **R2 binding mount**（生产）：走"无凭据出口拦截"，Worker 侧签名，容器里**零凭据**；
  - **Remote endpoint mount** + **`credentialProxy: true`**：在网络层**重新签名** S3 请求，凭据不进容器（文档说这将是未来的默认）。
  - 已文档化的代价：**FUSE 开销**（小文件/元数据操作要对 S3 打好几次往返）、**同一文件不可并发写**、**最终一致性**（刚写的文件可能看不见）。
- **Runloop Storage Objects**：跨 devbox 共享的对象，`storage_object.get_download_url(secondsToExpire)` 生成**限时预签名 URL**；`.gz/.tar/.tgz` 归档对象可挂载时**自动解包**到 devbox 路径。挂载在**创建时**声明（`mounts=[{type:"object_mount", object_id, object_path}]`）。
- **Modal Volumes**：挂在路径上的网络文件系统，可 `.readOnly()`；沙箱内外都能读写。
- **Blaxel** 的 drives（S3 后端）已在既有调研里被判为 **YAGNI**（`prior-art-sandbox-orchestration.md` §1.9：drives、FUSE 挂载、failover 区域"每个都是子系统"）。

**对我们**：ADR-0001 拒绝了 TLS 中间人路线，选了"函数调用版"。Cloudflare 的 `credentialProxy` 正是中间人路线的**现代实现**，而且它是**为了挂载**才需要中间人的。我们不做挂载，就不需要中间人——**ADR-0001 的选择在"不做挂载"这个前提下仍然是对的**，但代价是：数据必须由平台搬进搬出，而不是"挂上来看见"。

## 3. 出沙箱：产物怎么被远程取回

两种形状，没有第三种：

**(a) 直接文件下载 API**——Daytona `GET /files/download`、Vercel `downloadFile()`、OpenHands `zip-directory`（整目录打包成 zip 回传）。

**(b) 对象存储 + 预签名 URL**——产物先上传到对象存储，再换一个短时 URL：

- **Buildkite**：`GET /v2/organizations/{org}/jobs/{job}/artifacts/{id}/download` 返回 **302 重定向**到下载 URL，文档要求"**假设该 URL 只活 60 秒**"；GraphQL `Artifact.downloadURL` 是 **10 分钟**；用**自己的 S3 桶**则没有这个限制。上传侧返回的是**预签名 S3 POST 表单**（`PresignedUploadForm{url, data, file_input}`），表单字段必须排在文件字段之前；**S3 返回 403 就是预签名策略过期**，重新取一个再传。
- **GitHub Actions v4**：上传改成**客户端直连存储的签名 URL**（不经平台代理，官方称上传快 90%）；`artifact-url` 需要 GitHub 认证，**匿名下载**要另外调 REST API 换一个**短时签名 URL**；默认保留 **90 天**、`retention-days` 上限也是 90；产物**不可变**（`overwrite: true` 才能覆盖）。

**对 `#7` 的意义**：`#7` 已经定了"产物由 Worker 在过程中直传 `tasks/<task_id>/`"——**出沙箱那一半已经存在**。缺的只是**把 `tasks/<id>/` 换成一个能下载的 URL** 这一步，而先行形态给的答案是：**控制层签发一个短时预签名 GET**。

## 4. 预签名 URL 的硬限制（会咬到 `#5`/`#12`）

- AWS SDK/CLI 生成时**上限 7 天**（604800 秒）；控制台上限 **12 小时**；S3 on Outposts 1 秒–7 天。
- ⚠️ **真正的到期时间是"配置时长"与"签名所用凭据的寿命"两者取早。** 用 STS 临时凭据签的 URL，**活不过那个会话**（AssumeRole 默认 1 小时，最长 36 小时）。
- URL 可重复使用直到过期；下载**已经开始**的，过期不影响这次传输。

**这一条直接约束设计**：若产物下载 URL 由 STS 会话签名，它的寿命就被租约绑死（分钟级）。要"给调用方一个能贴给别人、活几小时的链接"，签名必须用**平台自己的长期凭据**，且**在平台侧签**——不能交给 Worker 用 STS 签。

## 5. ⚠️ 决定性事实：先行形态默认有网

- **E2B**：`allowInternetAccess` 默认 `true`；关掉它等价于 `denyOut: ["0.0.0.0/0"]`（是"默认拒绝"基线，不是硬开关）；域名白名单**必须**配合 `denyOut: ["0.0.0.0/0"]` 才生效；`updateNetwork()` 可在运行中整体替换规则（不是合并）。另有 `egressProxy`（SOCKS5）、按域名改写规则的 `rules`。
- **Modal**：`block_network` / 出网域名与 CIDR 白名单；`block_network=True` 与动态网络策略 API **不兼容**，要全禁就用空允许列表。
- **Cloudflare**：容器有出口，且提供"凭据不出的出口拦截"这一档。

**这是全部调研里对我们最重要的一条**：先行形态把"agent 要数据"这件事交给了网络（clone / curl / pip）。**只有我们**（禁凭据入沙箱 + 可能禁出网）必须由平台自己提供搬运。这不是我们设计复杂，是约束换来的——但**约束只该换来一个机制，不是四个**。

## 6. 小结：最简公共形态，与"我们多出来的那几层"

**最简公共形态**（各家收敛于此）：

```
进：调用方 →（文件 API 或预签名 PUT）→ 平台存储 →（Worker 搬运）→ 沙箱里的一个路径
出：沙箱里的一个路径 →（Worker 搬运）→ 平台存储 →（短时预签名 GET）→ 调用方
```

没有资源引用语法、没有实例注册表、没有操作集、没有生命周期授权、没有"物化"这个动词——**只有路径和字节**。

**我们模型里先行形态不存在的那几层**（逐条列出，供裁剪）：

| 我加的层 | 先行形态里有没有 | 它其实是为什么加的 |
|---|---|---|
| 统一引用语法 `<kind>://<实例>/<路径>` | ❌ 没有 | 为了把 git 与 S3 塞进同一个抽象 |
| 资源实例注册表（S3 实例） | ❌ 没有 | 为了吸收 rustFS / 云 S3 差异 |
| Grant（长期授权对象） | ❌ 没有 | 为了回答"谁允许这个 Task 碰这个桶" |
| OpSet `{read, write}` | ❌ 没有（Cloudflare 挂载只有 readOnly 一个开关） | 同 Git 那两档对齐 |
| 物化 / 收集 编舞 | ⚠️ 挂载是它的替代品，且有人做 | 因为禁凭据入沙箱，数据只能平台搬 |
| 产物预签名下载 URL | ✅ **有，且是唯一形状** | `#7` 缺的就是这一步 |

**一句话**：`#7` 的 `artifacts` + 一个**短时预签名下载 URL**，已经覆盖了"沙箱上传产物、调用方能远程下载"这件事；而"智能体把文件弄进来"在先行形态里是**文件 API**，不是"资源 + 授权"。

## 附：未能证实 / 未覆盖

- GitLab artifacts、CircleCI、Travis/Drone 的产物取回形状未查。
- Codex cloud / Claude Code 云端沙箱的"附件怎么进沙箱"没有找到公开一手资料。
- Modal 字节的线上协议（页面级）仍未文档化（既有调研同一条）。
- E2B `files.write` 的单文件体积上限、各家的上传体积上限未逐一核实。
- "沙箱内 agent 主动请求平台把某个对象取进来"这种形状**在先行形态里没找到任何先例**——各家都是"有网就自己下"。
