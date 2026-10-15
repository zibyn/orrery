# S3 短时授权调研

对应 issue：zibyn/stars-sandbox#5（`wayfinder:research`）。问题：Resource Manager 要让任务读改 S3 资源而不持有长期密钥，**Broker 该下发什么形态的 S3 凭据？**

调研日期：2026-10-15。所有结论都标注了来源；凡是 rustFS 侧无法从官方文档或可达源码确认的，明确写成「未确认」。

---

## 结论

**Broker 下发 STS 短期凭证三元组：`AccessKeyId` + `SecretAccessKey` + `SessionToken`（+ `Expiration`），由 Broker 在控制层用长期密钥调 `AssumeRole` 时附上一个 inline session policy，把范围限死在 `tasks/<id>/`。**

理由：

1. 它是唯一能同时表达「限 prefix」和「一次会话」的形态。预签名 URL 只能表达单个 object 的单个操作，不能表达「这个凭证只能读写 `tasks/<id>/`」。
2. 它对客户端是标准 S3 语义：沙箱侧只需要标准 S3 SDK 的 `session_token` 字段，协议在 rustFS / AWS S3 / 阿里云 OSS 之间不变。STS endpoint 只有控制层知道，沙箱从头到尾只拿到三元组。
3. rustFS 今天（1.0.x 稳定版）实现了 AWS 兼容的 `AssumeRole`，并支持 inline session policy 作为**交集**限制（源码层面已确认，见下）。

配套的硬性要求：

- Broker 用的长期身份必须是**专用的、最小权限的 IAM 用户**，不是 root。因为 rustFS 忽略 `RoleArn`，临时凭证继承的是**调用者自己的 policy**（见「rustFS 的 AssumeRole 模型」）。session policy 是第二道墙，不是唯一一道。
- 沙箱拿到的凭证不得再能调 `AssumeRole`。rustFS 明确拒绝临时凭证/服务账户再签发凭证；AWS 侧则用「不给 `sts:AssumeRole` 权限」保证。两边一致。
- 预签名 URL 只作为**次要**手段：把一个具体 object 交给浏览器或不可信进程时用。不作为任务的主凭证形态。
- 退路（仅当部署的 rustFS 版本没有可用的 STS 时）：控制层保留长期密钥并做代理——见「退路」。

---

## 1. 短时授权手段（AWS 语义，作为协议的参考模型）

### 1.1 STS AssumeRole

- `DurationSeconds`：默认 3600s，取值 900–43200s（15 分钟–12 小时），且受角色的 **MaxSessionDuration** 设置（1–12 小时）封顶；超过会直接失败。**角色链（role chaining）最长 1 小时**。
- 返回三元组：access key ID、secret access key、session token（另有 `Expiration`）。
- 可用 `Policy`（单条 inline JSON session policy）和 `PolicyArns`（最多 10 条托管 policy）作为会话策略；inline + 托管 policy 的明文合计 **≤ 2048 字符**。
- 会话权限 = **角色 identity-based policy 与 session policy 的交集**；session policy 不能授予超过角色的权限。

来源：
- <https://docs.aws.amazon.com/STS/latest/APIReference/API_AssumeRole.html>
- <https://docs.aws.amazon.com/IAM/latest/UserGuide/access_policies.html#policies_session>
- <https://docs.aws.amazon.com/IAM/latest/UserGuide/id_credentials_temp_request.html#stsapi_comparison>

### 1.2 AssumeRoleWithWebIdentity

给 OIDC 联合身份用，不需要在控制层放长期密钥：沙箱/工作负载持 IdP 签发的 JWT，直接换取临时凭证。会话时长与 session policy 语义同 `AssumeRole`。这是「连 Broker 都不持有长期 S3 密钥」的进阶方案，依赖有一个可用的 OIDC issuer；rustFS 也实现了它（见 2.2）。

来源：<https://docs.aws.amazon.com/STS/latest/APIReference/API_AssumeRoleWithWebIdentity.html>

### 1.3 预签名 URL

- SigV4 的 query-string 形式，有有效期上限，但**上限由签名时用的凭证决定**：
  - IAM 用户长期凭证：最长 **7 天**（AWS CLI / SDK 上限 604800s）。
  - S3 控制台生成：1 分钟–12 小时。
  - **临时凭证（STS）：不能长于凭证本身**，角色会话结束即失效。
- 一个预签名 URL 只对一个 object + 一个 HTTP 方法有效（`GET`/`PUT`/`HEAD` 等），可重复使用到过期为止。
- 因此：预签名 URL 是「把某个 object 的某次操作租出去」，**不是权限范围的载体**，无法表达 prefix 级授权。

来源：<https://docs.aws.amazon.com/AmazonS3/latest/userguide/using-presigned-url.html>

### 1.4 bucket policy 条件（`s3:prefix`）

`ListBucket` 的返回范围靠条件键约束，而不是靠 resource ARN：

- `s3:prefix` 把 `ListObjectsV2` 的结果限制在某个前缀；`s3:max-keys` 限制页大小。
- AWS 官方示例是 **Allow + 显式 Deny 成对**：一条 `Allow s3:ListBucket` 带 `StringEquals: {s3:prefix: "projects"}`，再加一条 `Deny s3:ListBucket` 带 `StringNotEquals: {s3:prefix: "projects"}`。因为显式 Deny 永远压过 Allow，这样「列出别的前缀」才真的不可能，而不会因为别处有更宽泛的 Allow 而漏出去。
- 对象级读写则靠 resource ARN 前缀（`arn:aws:s3:::bucket/tasks/<id>/*`）。
- 版本化 bucket 需要 `s3:ListBucketVersions`，它也支持 `s3:prefix`。

来源：<https://docs.aws.amazon.com/AmazonS3/latest/userguide/amazon-s3-policy-keys.html>（bucket operations 的 Example 2）
另见：<https://docs.aws.amazon.com/AmazonS3/latest/userguide/walkthrough1.html>

### 1.5 session policy

就是 1.1 里的 `Policy` 参数：作用在**会话**上的额外限制，取交集、不能放大。这是我们下发 prefix 范围的主要载体——把它塞进 `AssumeRole` 的 `Policy`，就得到「这次会话只能碰这个前缀」的凭证。

来源：<https://docs.aws.amazon.com/IAM/latest/UserGuide/access_policies.html#policies_session>

---

## 2. rustFS 的支持面

### 2.1 S3 兼容到哪一层

官方 S3 协议页与兼容性矩阵的记录：

- **支持**：bucket CRUD/listing、object put/get/delete/copy/head、`ListObjects`/`V2`（prefix/delimiter/marker/max-keys）、multipart、bucket 与 object tagging、**bucket policy（put/get/delete）**、public access block、**presigned GET/PUT URLs**、range/条件读、SSE-C、部分 versioning/object-lock/CORS。认证走 SigV4，官方明确说「supports AWS Signature Version 4 (SigV4)」。
- **不支持或未完成**：**ACL 授权被有意不支持**（建议「Prefer IAM and bucket policies」）、bucket access logging 与 bucket ownership controls 是 planned、POST Object 表单上传的 checksum 处理不完整、S3 Select 只接受未压缩输入。
- 官方自己提醒它不是「每个 AWS S3 / MinIO API 都逐字节一致」，建议显式设置 endpoint 并实测用到的那几个操作。

来源：
- <https://docs.rustfs.com/administration/protocols/s3>
- <https://github.com/rustfs/rustfs/blob/main/docs/architecture/s3-compatibility-matrix.md>
- 测试门禁：<https://github.com/rustfs/rustfs/blob/main/scripts/s3-tests/implemented_tests.txt>（`test_cors_presigned_get_object`、`test_cors_presigned_put_object*`、`test_object_presigned_put_object_with_acl*` 等 8 条 presign 用例）

### 2.2 有 STS，且是 AWS 兼容的

官方文档「Service Accounts and STS」：

- **只有两个 action**：`AssumeRole`（既有 IAM 身份，SigV4 签名）与 `AssumeRoleWithWebIdentity`（OIDC 身份，JWT 认证）。文档没有 `GetSessionToken` / `AssumeRoleWithSAML` 的支持声明。
- **endpoint 是 S3 服务端根路径**：`POST /`，form-encoded 参数。AWS SDK 需要把 `sts` 客户端的 `endpoint_url` 指到 rustFS 地址（官方 discussion 的例子用了和 S3 同一个端口）。
- `Version` 必须是 `2011-06-15`。
- `DurationSeconds`：默认 3600，**clamp 到 900–43200**。
- `Policy`：可选 inline session policy（JSON）。
- 返回 XML `AssumeRoleResponse`，含 `AccessKeyId` / `SecretAccessKey` / `SessionToken` / `Expiration`；session token 走 `X-Amz-Security-Token`。
- **`AssumeRole` 必须由长期 IAM 凭证签名**：文档写明「temporary credentials and service accounts cannot call `AssumeRole`」。→ 沙箱拿到的凭证无法再签发凭证。

来源：<https://docs.rustfs.com/security-compliance/iam/sts>

**rustFS 的 AssumeRole 模型（重要，和 AWS 不同）**：维护者在 issue #4832 里明确说明——「RustFS does not create AWS IAM Role resources, account IDs, or role trust policies for this API」，`RoleArn` / `RoleSessionName` 只是 **AWS SDK 兼容占位符**，**不选择任何 policy**；「The returned temporary credentials inherit the policies of the built-in user」。另外调用者必须显式拥有 `sts:AssumeRole` 权限。

来源：<https://github.com/rustfs/rustfs/issues/4832>

**session policy 确实是交集限制，且 fail-closed（源码确认）**：

- `rustfs/src/admin/handlers/sts.rs`：`AssumeRole` 把 `body.policy` 交给 `populate_session_policy`，并显式拒绝 `cred.is_temp() || cred.is_service_account()`。
- `rustfs/src/admin/service/session_policy.rs`：解析并校验 session policy（要求有 version，明文 **≤ 2048 bytes**），base64url 后存进 session claim（`SESSION_POLICY_NAME`）。
- `crates/iam/src/sys.rs`：`try_eval_prepared` 对 STS 分支返回 `Ok(ok && (*is_owner || combined_policy.is_allowed(args).await))`；`evaluate_prepared_session_policy` 对解析失败返回 `DenyAll`。即「父 policy 与 session policy 都放行才算放行」，解析失败一律拒绝。

来源：
- <https://docs.rs/rustfs/latest/src/rustfs/admin/handlers/sts.rs.html>
- <https://docs.rs/rustfs/latest/src/rustfs/admin/service/session_policy.rs.html>
- <https://github.com/rustfs/rustfs/blob/main/crates/iam/src/sys.rs>

### 2.3 预签名 URL

- 兼容性矩阵把 **「Presigned GET and PUT URLs」列为 Supported**，有 s3tests 用例背书（见 2.1）。
- SigV4 的 **query-string 形式是实现了的**：`crates/signer/src/request_signature_v4.rs` 生成 `X-Amz-Algorithm` / `X-Amz-Credential` / `X-Amz-Date` / `X-Amz-Expires` / `X-Amz-SignedHeaders`，并可选带 `X-Amz-Security-Token`。
- 验证侧 `rustfs/src/auth.rs` 会**优先取 header 的 `x-amz-security-token`，否则回落到 query 里的同名参数**，说明带 STS 会话凭证的预签名 URL 是被识别并校验的。`determine_auth_type_and_version_with_query` 对 presigned 请求产出 `REST-QUERY-STRING` / `AWS4-HMAC-SHA256`。
- PR #2046「fix: support query-only presigned URL access」修过「纯 query 的预签名链接被当成匿名请求」的问题——说明这条路径现在是走通的，但历史上踩过坑。

来源：
- <https://github.com/rustfs/rustfs/blob/main/crates/signer/src/request_signature_v4.rs>
- <https://docs.rs/rustfs/latest/src/rustfs/auth.rs.html>
- <https://github.com/rustfs/rustfs/pull/2046>

**未确认**：rustFS 是否对 `X-Amz-Expires` 施加任何上限（例如 AWS 的 604800s）。我在签名侧（`request_signature_v4.rs`）只看到 `query.push(("X-Amz-Expires", format!("{expires:010}")))`，**没有任何上限常量或越界检查**；过期校验应在服务端 auth 中间件，而这一段我没能读到（`auth.rs` 只覆盖了前 100k 字符）。**结论：不要把「有效期不能超过 7 天」这件事交给 rustFS 保证——Broker 自己设上限。**

### 2.4 版本底线与已知坑

rustFS 迭代很快（alpha → beta → rc → 1.0.x），**授权相关的 bug 集中在 1.0.0 之前**。当前稳定版是 **1.0.1（2026-10-03）**，另有 1.0.2-preview.1（2026-10-14）。

和本方案直接相关的修复：

- **CVE-2026-22043 / GHSA-xgr5-qc6w-vcg9**：IAM `deny_only` 短路导致提权——受限的 service account 或 **STS 凭证**可以自签一个不受限的 service account，继承父身份（可达 root）的权限。**影响 alpha.13–alpha.78，alpha.79 修复。** 我们下发的正是 STS 凭证，务必跑 ≥ alpha.79（即当前 1.0.x）。来源：<https://github.com/rustfs/rustfs/security/advisories/GHSA-xgr5-qc6w-vcg9>
- **issue #1337**：session policy 的 `Resource` 写成字符串（非数组）会让 `AssumeRole` 500。**alpha.79 修复。** → 我们下发的策略里 `Resource` 一律写成数组。来源：<https://github.com/rustfs/rustfs/issues/1337>
- **issue #2699**：split-statement 策略（bucket ARN 管 `s3:ListBucket` + object 前缀 ARN 管读写）+ `s3:prefix` 条件下 `ListObjects` 返回 403，而同一策略下 `PutObject` 正常。**PR #2707 修复。** 这正好是我们想要的策略形状（见第 3 节），所以要用包含该修复的版本，并对目标版本实测。来源：<https://github.com/rustfs/rustfs/issues/2699>、<https://github.com/rustfs/rustfs/pull/2707>
- **PR #1804**：临时凭证评估父用户**组**策略时的 fallback 与 session policy 调试日志。说明 STS 与组的交互也修过。来源：<https://github.com/rustfs/rustfs/pull/1804>

### 2.5 退路（如果部署的 rustFS 没有可用 STS）

两个方向，都不改变沙箱看到的接口：

1. **自建签名代理（推荐退路）**：控制层保留长期密钥，用一个「签名代理」把长期密钥换成短时的访问入口——沙箱请求带 Broker 签发的短期 token（不是 S3 凭证，就是我们自己的会话令牌）打到代理，代理校验后用自己的长期密钥做 S3 读写。对沙箱暴露的是受控的 HTTP 语义（如 `/objects/tasks/<id>/<key>`），而不是 S3 API。
2. **控制层代理全部对象读写**：等价于把上一条做成唯一的对象访问通道，沙箱完全不接触任何 S3 凭证。

代价：沙箱侧不能直接用标准 S3 SDK（要面对我们的代理协议），且控制层成为对象 I/O 的数据面瓶颈。这违背了「迁移到 OSS 只是改配置」的初衷（stars-trail ADR 0003 要求后端只用 S3 API），所以只作为版本不支持时的临时退路，不是目标形态。

---

## 3. 按 prefix 限权

**能表达，而且三个平台用的是同一个形状**：object 前缀写在 resource ARN 里，`ListBucket` 的范围用 `s3:prefix` 条件收住。

rustFS 官方 policies 文档给的「Read/Write Restricted to a Prefix」就是这个写法（bucket ARN 配 `StringLike: {s3:prefix: [...]}`，object 前缀 ARN 配 `GetObject`/`PutObject`），ARN 形式为 `arn:aws:s3:::bucket/prefix/*`。issue #1399 里维护者给出的可工作版本同样是两语句：`s3:ListBucket` on `arn:aws:s3:::rustfs` + `StringLike s3:prefix`，加 `s3:GetObject` on `arn:aws:s3:::rustfs/rustfs/*`。

来源：
- <https://docs.rustfs.com/security-compliance/iam/policies>
- <https://github.com/rustfs/rustfs/issues/1399>

建议的 session policy 模板（`<id>` 替换为任务 id；**`Resource` 用数组**，规避 issue #1337）：

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "ListOwnPrefix",
      "Effect": "Allow",
      "Action": "s3:ListBucket",
      "Resource": ["arn:aws:s3:::stars-tasks"],
      "Condition": { "StringLike": { "s3:prefix": ["tasks/<id>/*"] } }
    },
    {
      "Sid": "DenyListOutsideOwnPrefix",
      "Effect": "Deny",
      "Action": "s3:ListBucket",
      "Resource": ["arn:aws:s3:::stars-tasks"],
      "Condition": { "StringNotLike": { "s3:prefix": ["tasks/<id>/*"] } }
    },
    {
      "Sid": "ObjectAccessOwnPrefix",
      "Effect": "Allow",
      "Action": [
        "s3:GetObject",
        "s3:PutObject",
        "s3:DeleteObject",
        "s3:AbortMultipartUpload"
      ],
      "Resource": ["arn:aws:s3:::stars-tasks/tasks/<id>/*"]
    }
  ]
}
```

第 2 条显式 Deny 是照抄 AWS 官方示例的防御姿势（1.4）：只靠 Allow + `s3:prefix` 条件，一旦别处有更宽泛的 Allow 就会漏。

**未确认**：（a）rustFS 没有专门文档或测试证明「把这条 prefix 策略作为 **AssumeRole 的 session policy** 传入时，交集评估按预期生效」——语义（交集 + fail-closed）在源码里成立，prefix 策略作为普通 IAM policy 也被文档承认，但两者的**组合**没有现成用例。部署版本上必须实测。（b）为什么 rustFS 忽略 `RoleArn`，意味着我们**不能**用「一条 per-task role」替代 session policy；`tasks/<id>/` 的收口只能靠 session policy 的 `Resource` + `s3:prefix`。

---

## 4. 云对应机制

### 4.1 AWS S3

- 凭证：`AssumeRole`（+ session policy）或 `AssumeRoleWithWebIdentity`。语义与第 1 节相同，是其他两家对齐的基准。
- 预签名：SigV4，IAM 用户凭证最长 7 天；控制台 1 分钟–12 小时；用临时凭证时以凭证过期为准。
- prefix：object 前缀 ARN + `s3:prefix` 条件（见 3）。

### 4.2 阿里云 OSS

- **STS AssumeRole**：调用方须是 RAM 用户/RAM 角色并带 `AliyunSTSAssumeRoleAccess`，且被加进角色的 trust policy。返回 `AccessKeyId` / `AccessKeySecret` / `SecurityToken` / `Expiration`——**和我们从 rustFS 拿到的三元组形状一致**（OSS 的 STS AccessKeyId 以 `STS.` 开头）。
- **时长**：`DurationSeconds` 默认 3600，最小 900；上限 = 角色的 `MaxSessionDuration`（可配 3600–43200）。
- **scope**：角色自身的 policy 给出上限，`AssumeRole` 的 `policy` 参数（≤2048 字符）作为 session policy，**最终权限 = 角色权限 ∩ 传入 policy**；不传就继承角色的全部权限。官方示例把上传限制到 `acs:oss:*:*:examplebucket/src/*`——**同样是前缀 ARN 的形状**，只是 ARN 语法不同。
- **预签名**：ossutil `presign` 的有效期范围是 **1 秒–1 周**（默认 15 分钟）；OSS 同时支持 V1 与 V4 签名（`OSS4-HMAC-SHA256`，较新 SDK 默认 V4）。用 STS 凭证签 URL 时必须带 `security-token`（query）或 `x-oss-security-token`（header），且 **URL 有效期取「URL 自身有效期」与「凭证有效期」中较短者**。
- 请求带 STS 凭证时三个字段缺一不可（常见报错 0002-00000003 就是漏了 security token）。

来源：
- <https://help.aliyun.com/en/oss/developer-reference/use-temporary-access-credentials-provided-by-sts-to-access-oss>
- <https://www.alibabacloud.com/help/en/ram/developer-reference/api-sts-2015-04-01-assumerole>
- <https://www.alibabacloud.com/help/en/oss/developer-reference/presign-generate-presigned-url>
- <https://help.aliyun.com/en/oss/user-guide/0002-00000003>
- <https://www.alibabacloud.com/help/en/oss/developer-reference/guidelines-for-upgrading-v1-signatures-to-v4-signatures>

### 4.3 协议不变性

从 rustFS 换到 AWS S3 / OSS，沙箱侧**不需要改协议**：

- 沙箱只看三元组 + endpoint + region，走标准 S3 SDK 的 `session_token` 字段；这三家都接受。
- 差异只在配置与策略文本：ARN 拼写（`arn:aws:s3:::b/p/*` vs `acs:oss:*:*:b/p/*`）、STS endpoint（AWS 是独立主机，rustFS 是 S3 根路径——但 STS 调用只在控制层发生，沙箱无感）、寻址方式（stars-trail ADR 0003 已记：rustFS 默认 path-style，OSS 不接受 path-style）。
- 因此 Broker 应当把「ARN 语法 / endpoint / region / addressing style」做成配置，把 session policy 模板按后端渲染。

---

## 5. 建议的设计落点（回答 issue 的产出要求）

「Broker 该下发什么形态的 S3 凭据」→ **STS 三元组 + inline session policy 限 prefix，短时长，按需重签。**

- **形态**：`{access_key_id, secret_access_key, session_token, expiration}`。沙箱只拿到它，永远不接触 Broker 的长期密钥，也不需要知道 STS endpoint。
- **范围**：Broker 调 `AssumeRole` 时带第 3 节的 session policy，ARN/前缀按后端渲染。
- **时长**：取 900–3600s 一档（比如 900 或 1800），在任务运行期间按需重签；不要为了省事申请 12 小时。RustFS 的 clamp 上限是 43200，AWS 还受 role MaxSessionDuration 与 role chaining 1 小时限制。
- **Broker 的长期身份**：专用 IAM 用户，自身 policy 就只覆盖 `tasks/*`（rustFS 下这是第一道且唯一「身份自带」的边界），并显式赋予 `sts:AssumeRole`。绝不用 root。
- **幂等/审计**：`RoleSessionName` 带任务 id（rustFS 里它是占位符，但 AWS/OSS 里它进 ARN 与审计日志）。
- **预签名 URL**：只作为「把某个 object 交给浏览器/不可信进程」的补充手段；它的有效期天然被会话凭证封顶，rustFS 侧不要指望有 7 天上限。
- **退路**：若目标 rustFS 版本 STS 不可用或实测不通过，改为控制层的签名代理（2.5），并把代理协议当作临时形态，等版本跟上再切回 STS。

---

## 6. 未确认清单（明确标注）

1. **rustFS 是否对预签名 URL 的 `X-Amz-Expires` 设上限**（如 AWS 的 604800s）。签名侧无任何上限常量；服务端过期校验代码未读到。→ 自行设上限。
2. **rustFS 下「prefix session policy ∩ 父 policy」这一组合没有专门文档或测试**。交集与 fail-closed 语义在 `crates/iam/src/sys.rs` 里成立，prefix 策略作为普通 IAM policy 也在文档里有例，但组合未验证。→ 部署版本上实测。
3. **rustFS 的 `AssumeRoleWithWebIdentity` 是否在生产可依赖**（文档有、参数有，但只在 #4832 一类讨论里被提及，未找到成熟用例）。→ 本次方案不依赖它，用 `AssumeRole`。
4. rustFS 官方英文站点是 `docs.rustfs.com`，中文镜像 `docs.rustfs.com.cn`；两者内容基本一致，本文件引用前者。
5. rustFS 没有 `GetSessionToken` / `AssumeRoleWithSAML` 的支持声明——不是「确认不支持」，是**文档未表态**。

---

## 参考来源（官方一手）

- AWS STS `AssumeRole` API 参考：<https://docs.aws.amazon.com/STS/latest/APIReference/API_AssumeRole.html>
- AWS STS `AssumeRoleWithWebIdentity`：<https://docs.aws.amazon.com/STS/latest/APIReference/API_AssumeRoleWithWebIdentity.html>
- AWS IAM session policy 与策略求值：<https://docs.aws.amazon.com/IAM/latest/UserGuide/access_policies.html>
- AWS 临时凭证对比（含 role chaining）：<https://docs.aws.amazon.com/IAM/latest/UserGuide/id_credentials_temp_request.html>
- AWS S3 预签名 URL：<https://docs.aws.amazon.com/AmazonS3/latest/userguide/using-presigned-url.html>
- AWS S3 条件键示例（`s3:prefix`）：<https://docs.aws.amazon.com/AmazonS3/latest/userguide/amazon-s3-policy-keys.html>
- AWS S3 按前缀控制访问：<https://docs.aws.amazon.com/AmazonS3/latest/userguide/walkthrough1.html>
- rustFS「Service Accounts and STS」：<https://docs.rustfs.com/security-compliance/iam/sts>
- rustFS「Users, Groups, and Policies」：<https://docs.rustfs.com/security-compliance/iam/policies>
- rustFS「S3 protocol」：<https://docs.rustfs.com/administration/protocols/s3>
- rustFS S3 兼容性矩阵：<https://github.com/rustfs/rustfs/blob/main/docs/architecture/s3-compatibility-matrix.md>
- rustFS STS 处理源码：<https://docs.rs/rustfs/latest/src/rustfs/admin/handlers/sts.rs.html>
- rustFS session policy 源码：<https://docs.rs/rustfs/latest/src/rustfs/admin/service/session_policy.rs.html>
- rustFS IAM 求值源码：<https://github.com/rustfs/rustfs/blob/main/crates/iam/src/sys.rs>
- rustFS issue #1399（prefix 限权写法）：<https://github.com/rustfs/rustfs/issues/1399>
- rustFS issue #2699（split-statement + s3:prefix 403，已修）：<https://github.com/rustfs/rustfs/issues/2699>
- rustFS issue #4832（AssumeRole 模型与常见报错）：<https://github.com/rustfs/rustfs/issues/4832>
- rustFS issue #1337（session policy Resource 必须为数组）：<https://github.com/rustfs/rustfs/issues/1337>
- rustFS 安全公告 GHSA-xgr5-qc6w-vcg9 / CVE-2026-22043：<https://github.com/rustfs/rustfs/security/advisories/GHSA-xgr5-qc6w-vcg9>
- 阿里云 OSS × STS：<https://help.aliyun.com/en/oss/developer-reference/use-temporary-access-credentials-provided-by-sts-to-access-oss>
- 阿里云 STS `AssumeRole` API：<https://www.alibabacloud.com/help/en/ram/developer-reference/api-sts-2015-04-01-assumerole>
- 阿里云 OSS 预签名（ossutil presign）：<https://www.alibabacloud.com/help/en/oss/developer-reference/presign-generate-presigned-url>
- 阿里云 OSS V1→V4 签名升级：<https://www.alibabacloud.com/help/en/oss/developer-reference/guidelines-for-upgrading-v1-signatures-to-v4-signatures>
- 相关 ADR：`/home/zibyn/Projects/stars-trail/docs/adr/0003-lan-server-and-rustfs-before-launch.md`
