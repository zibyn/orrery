# PostgreSQL 表清单

PostgreSQL 是权威状态。本文列出表、关键列和约束；迁移 SQL 由实现编写，执行方式见 golang-migrate（ADR-0014）。

## 约定

- 状态列用 `text` 加 `CHECK`，不用 PG enum。
- Exec 的状态是从时间戳派生的 generated column（ADR-0015）。
- 逻辑删除就是 `deleting`/`deleted` 状态加 `deleted_at`；物理清理由 Reconciler 或 purge 完成。
- 外键一律 `ON DELETE RESTRICT`：只要还有引用，行就删不掉。
- Idempotency Key 不建通用表。每张可创建资源的表各有一个可空列 `idempotency_key`，对 (owner_id, key) 建部分唯一索引，purge 在 24 小时后把它置空。`PUT /secrets/{name}` 由调用方命名、天然幂等（新建 201、覆盖 200），不参与该规则。
- 凭据只存哈希（API Token、Session、Node Token、Enrollment Packet）或密文（Secret、Git token、Sandbox Token）。密文旁边带 `key_id`（ADR-0017），加密方式见 `defaults.md` 的 `encryption_keys`。Sandbox Token 存密文的原因见 ADR-0016。
- 执行日志相关的表不建成 `UNLOGGED`。

## 表

| 表 | 关键列 | 约束与说明 |
|---|---|---|
| `users` | `id`, `name`, `email`, `is_admin`, `login_type`, `password_hash`, `oidc_issuer`, `oidc_subject`, `gpu_quota`, `sandbox_quota`, `deleted_at` | `name` 唯一；`email` 可空，非空时唯一；(`oidc_issuer`, `oidc_subject`) 唯一；`login_type` 取 `password`/`oidc`/`none`；`password_hash` 是 argon2id，只在 `password` 时非空；不是 admin 的 User 签发不了 `admin` scope 的 token |
| `api_tokens` | `id`, `user_id`, `kind`, `name`, `secret_hash`, `scope`, `expires_at`, `last_used_at` | `kind` 取 `token`/`session`；(`user_id`, `name`) 在 `kind=token` 上部分唯一；Session 每次使用把 `expires_at` 顺延到 now + 闲置期限 |
| `enrollment_packets` | `id`, `secret_hash`, `expires_at`, `used_at`, `created_by` | 兑换就是 `UPDATE … WHERE used_at IS NULL AND expires_at > now()` |
| `nodes` | `id`, `name`, `token_hash`, `paused`, `lost_at`, `agent_version`, `proto_major`, `proto_minor`, `last_seen_at`, `runtime_health`, `capabilities`, `hardware`, `mesh_public_key`, `mesh_endpoint`, `sandbox_subnet`, `reserved_cpu`, `reserved_memory`, `cpu_per_gpu`, `memory_per_gpu`, `created_at`, `deleted_at` | Unreachable 在读取时由 `last_seen_at` 派生，不落库；宣告 lost 和删除都把 `token_hash` 置空；删除只写 `deleted_at`，行留作墓碑，不清理，`workspaces`、`sandboxes` 仍引用它；放置同等条件下按 `created_at` 排序（ADR-0015）；`mesh_*` 和 `sandbox_subnet` 取自最近一次 `Hello`，连通矩阵不落库（ADR-0027） |
| `gpus` | `uuid` PK, `node_id`, `model` | Node 删除时删掉对应的行 |
| `workspaces` | `id`, `owner_id`, `kind`, `node_id`, `state`, `cause`, `storage_handle`, `conditions`, `idempotency_key`, `deleted_at` | 临时 Workspace 带 `sandbox_id`，删除时随它一起删 |
| `sandboxes` | `id`, `owner_id`, `kind`, `name`, `labels`, `runtime`, `desired_state`, `state`, `cause`, `node_id`, `snapshot_digest`, `conditions`, `cpu`, `memory`, `gpu_count`, `gpu_model`, `idle_stop_seconds`, `egress_mode`, `egress_allow`, `links_from`, `links_ports`, `address`, `last_activity_at`, `workspace_id`, `workspace_mode`, `token_ct`, `token_key_id`, `secret_env_ct`, `generation`, `deleted_at` | 未删除行按 (`owner_id`, `name`) 唯一；`kind` 取 `user`/`build`，`build` 行是 Snapshot 构建的内部行，没有 Runtime 容器，只承载 Allocation 和构建 Exec，不出现在 API 里，不计入 User Quota（ADR-0021）；`conditions` 如 `fetching_resources`；`token_ct` 是 Sandbox Token 的密文，创建时生成，整个生命周期不变，Sandbox 删除即失效；认证时按 id 取行、解密，再做常数时间比较；`secret_env_ct` 是 start 时拷贝的 Secret 密文（见下）；`gpu_model` 是创建时请求的型号，`runtime` 取 `container`/`local`，两者重启后仍作为准入约束；`egress_mode` 取 `open`/`none`/`allowlist`，`egress_allow` 是 `allowlist` 的条目（其余为空），创建后不变，每次 start 原样下发（ADR-0026）；`links_from`（uuid 数组）和 `links_ports`（整数数组，空表示所有端口）是 Sandbox Link，创建后不变；`address` 是 Agent 上报的当前地址，不在运行时为空（ADR-0027）；`workspace_id`/`workspace_mode` 是挂载关系，Attachment 行只在运行时存在 |
| `attachments` | `sandbox_id` PK, `workspace_id`, `mode`, `generation` | 部分唯一索引 (`workspace_id`) `WHERE mode = 'rw'`，即单写者规则；Sandbox 停止或删除时删掉这一行，就是释放挂载 |
| `sandbox_resources` | `sandbox_id`, `resource_version_id`, `mount` | 引用即保留；(`sandbox_id`, `mount`) 唯一；构建行的 `contexts` 也记在这里，`mount` 取 context 名 |
| `admission_requests` | `id`, `sandbox_id`, `priority`, `enqueued_at`, `deadline_at`, `ended_at`, `outcome` | FIFO 按 (`priority`, `enqueued_at`) 排序 |
| `allocations` | `sandbox_id` PK, `node_id`, `cpu`, `memory`, `disk`, `generation` | |
| `gpu_allocations` | `gpu_uuid` PK, `sandbox_id`, `created_at` | 主键本身保证整卡独占；随 `allocations` 一起释放 |
| `execs` | `id`, `sandbox_id`, `argv`, `env`, `cwd`, `pty`, `timeout_seconds`, `accepted_at`, `started_at`, `cancel_requested_at`, `deadline_at`, `finished_at`, `cause`, `exit_code`, `signal`, `log_truncated`, `stored_seq`, `state` generated | `env` 里不放 Secret |
| `exec_log_chunks` | `exec_id`, `first_seq`, `last_seq`, `frames` | PK (`exec_id`, `first_seq`) |
| `secrets` | `id`, `owner_id`, `name`, `ciphertext`, `key_id`, `size` | (`owner_id`, `name`) 唯一；每个 User 最多 50 个 |
| `git_links` | `user_id`, `provider_id`, `access_token_ct`, `refresh_token_ct`, `key_id`, `expires_at` | PK (`user_id`, `provider_id`)；刷新时对该行加 `FOR UPDATE`，以此串行化 |
| `resources` | `id`, `owner_id`, `name`, `current_version_id`, `deleted_at` | (`owner_id`, `name`) 在未删除行上部分唯一 |
| `resource_versions` | `id`, `resource_id`, `address`, `manifest_key`, `size`, `provenance`, `state`, `cause`, `idempotency_key`, `expires_at` | `state` 取 `pending`/`ready`/`failed`/`unavailable`；`address` 在 manifest 提交前为空；`manifest_key` 为 `manifests/<owner_id>/<address>`；`provenance.source` 取 `upload` 或 `workspace{id, path}`；变成 `ready` 时移动 Resource 的 current 指针 |
| `resource_version_objects` | `version_id`, `sha256` | PK (`version_id`, `sha256`)；CAS 清扫用 `NOT EXISTS` 判断对象是否还被同一 owner 的版本或未过期的 upload 会话引用 |
| `cas_objects` | `owner_id`, `sha256`, `size`, `created_at` | PK (`owner_id`, `sha256`)，对应对象 `cas/<owner_id>/sha256/<hex>`（ADR-0021）；版本标为 `unavailable` 时删掉缺失对象的行，下一次上传就会重新签发 PUT |
| `uploads` | `id`, `resource_id`, `address`, `manifest`, `expires_at` | 同一 Resource 下未过期、`address` 相同的会话被复用，即续传 |
| `snapshots` | `id`, `owner_id`, `name`, `current_version_id`, `deleted_at` | `owner_id` 为空即平台 Snapshot；(`owner_id`, `name`) 在未删除行上部分唯一，`NULLS NOT DISTINCT`；版本变成 `ready` 时移动 current 指针 |
| `snapshot_versions` | `id`, `snapshot_id`, `digest`, `state`, `cause`, `source`, `build_sandbox_id`, `build_exec_id`, `provenance`, `idempotency_key`, `deleted_at` | `state` 取 `building`/`ready`/`failed`/`cancelled`；`source` 取 `import`/`build`；`build_sandbox_id` 指向 `kind=build` 的 Sandbox 行；被 Sandbox（含 stopped）引用或是 current 时不能删除；显式删除只写 `deleted_at`，由 purge 删 registry 里的 manifest 后再删行；Snapshot 删除后，被引用的版本保留到最后一个引用消失，再删 registry 里的 manifest |
| `audit_records` | `id` bigserial, `at`, `actor_user_id`, `actor_token_id`, `action`, `target_type`, `target_id`, `correlation_id`, `detail` | 在提交后插入（ADR-0016） |

**Secret 快照**。Sandbox 每次 start 时，按名字把 Secret 的密文拷进 `sandboxes.secret_env_ct`。每个 Exec 的 `StartExec` 都由 Control Plane 解密后下发，这样一来：

- Node Agent 重启不会丢失这些值；
- 运行中改了 Secret，只影响下一次 start（ADR-0013、ADR-0017）。

**没有命令表**。下发给 Node 的命令由 Reconciler 从期望状态派生，期望状态的表本身就是队列（ADR-0014）。
