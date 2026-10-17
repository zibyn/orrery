# PostgreSQL 表清单

PostgreSQL 是权威状态。本文列出表、关键列和约束；迁移 SQL 由实现编写，执行方式见 golang-migrate（ADR-0014）。

## 约定

- 状态列用 `text` 加 `CHECK`，不用 PG enum。
- Exec 的状态是从时间戳派生的 generated column（ADR-0015）。
- 逻辑删除就是 `deleting`/`deleted` 状态加 `deleted_at`；物理清理由 Reconciler 或 purge 完成。
- 外键一律 `ON DELETE RESTRICT`：只要还有引用，行就删不掉。
- Idempotency Key 不建通用表。每张可创建资源的表各有一个可空列 `idempotency_key`，对 (owner_id, key) 建部分唯一索引，purge 在 24 小时后把它置空。
- 凭据只存哈希（token）或密文（Secret、Git token）。密文旁边带 `key_id`（ADR-0017）。
- 执行日志相关的表不建成 `UNLOGGED`。

## 表

| 表 | 关键列 | 约束与说明 |
|---|---|---|
| `users` | `id`, `name`, `is_admin`, `gpu_quota`, `sandbox_quota`, `deleted_at` | `name` 唯一；不是 admin 的 User 签发不了 `admin` scope 的 token |
| `api_tokens` | `id`, `user_id`, `name`, `secret_hash`, `scope`, `expires_at`, `last_used_at` | (`user_id`, `name`) 唯一 |
| `enrollment_packets` | `id`, `secret_hash`, `expires_at`, `used_at`, `created_by` | 兑换就是 `UPDATE … WHERE used_at IS NULL AND expires_at > now()` |
| `nodes` | `id`, `name`, `token_hash`, `paused`, `lost_at`, `agent_version`, `proto_major`, `proto_minor`, `last_seen_at`, `runtime_health`, `capabilities`, `hardware`, `reserved_cpu`, `reserved_memory`, `cpu_per_gpu`, `memory_per_gpu`, `disk_watermark` | Unreachable 在读取时由 `last_seen_at` 派生，不落库 |
| `gpus` | `uuid` PK, `node_id`, `model` | |
| `workspaces` | `id`, `owner_id`, `kind`, `node_id`, `state`, `cause`, `storage_handle`, `conditions`, `deleted_at` | 临时 Workspace 带 `sandbox_id`，删除时随它一起删 |
| `sandboxes` | `id`, `owner_id`, `name`, `labels`, `desired_state`, `state`, `cause`, `node_id`, `snapshot_digest`, `cpu`, `memory`, `gpu_count`, `idle_stop_seconds`, `last_activity_at`, `token_hash`, `secret_env_ct`, `generation`, `deleted_at` | 未删除行按 (`owner_id`, `name`) 唯一；`secret_env_ct` 是 start 时拷贝的 Secret 密文（见下） |
| `attachments` | `sandbox_id` PK, `workspace_id`, `mode`, `generation` | 部分唯一索引 (`workspace_id`) `WHERE mode = 'rw'`，即单写者规则；Sandbox 停止或删除时删掉这一行，就是释放挂载 |
| `sandbox_resources` | `sandbox_id`, `resource_version_id`, `mount` | 引用即保留 |
| `admission_requests` | `id`, `sandbox_id`, `priority`, `enqueued_at`, `deadline_at`, `ended_at`, `outcome` | FIFO 按 (`priority`, `enqueued_at`) 排序 |
| `allocations` | `sandbox_id` PK, `node_id`, `cpu`, `memory`, `disk`, `generation` | |
| `gpu_allocations` | `gpu_uuid` PK, `sandbox_id` | 主键本身保证整卡独占 |
| `execs` | `id`, `sandbox_id`, `argv`, `env`, `cwd`, `pty`, `timeout_seconds`, `accepted_at`, `started_at`, `cancel_requested_at`, `deadline_at`, `finished_at`, `cause`, `exit_code`, `signal`, `log_truncated`, `stored_seq`, `state` generated | `env` 里不放 Secret |
| `exec_log_chunks` | `exec_id`, `first_seq`, `last_seq`, `frames` | PK (`exec_id`, `first_seq`) |
| `secrets` | `id`, `owner_id`, `name`, `ciphertext`, `key_id`, `size` | (`owner_id`, `name`) 唯一；每个 User 最多 50 个 |
| `git_links` | `user_id`, `provider_id`, `access_token_ct`, `refresh_token_ct`, `key_id`, `expires_at` | PK (`user_id`, `provider_id`)；刷新时对该行加 `FOR UPDATE`，以此串行化 |
| `resources` | `id`, `owner_id`, `name`, `current_version_id`, `deleted_at` | (`owner_id`, `name`) 唯一 |
| `resource_versions` | `id`, `resource_id`, `address`, `manifest_key`, `size`, `provenance`, `state`, `expires_at` | |
| `cas_objects` | `sha256` PK, `size`, `created_at` | |
| `uploads` | `id`, `resource_id`, `manifest`, `expires_at` | |
| `snapshots` | `id`, `owner_id`, `name`, `deleted_at` | (`owner_id`, `name`) 唯一 |
| `snapshot_versions` | `id`, `snapshot_id`, `digest`, `state`, `source`, `build_exec_id`, `provenance` | 被 Sandbox（含 stopped）引用时不能删除 |
| `audit_records` | `id` bigserial, `at`, `actor_user_id`, `actor_token_id`, `action`, `target_type`, `target_id`, `correlation_id`, `detail` | 在提交后插入（ADR-0016） |

**Secret 快照**。Sandbox 每次 start 时，按名字把 Secret 的密文拷进 `sandboxes.secret_env_ct`。每个 Exec 的 `StartExec` 都由 Control Plane 解密后下发，这样一来：

- Node Agent 重启不会丢失这些值；
- 运行中改了 Secret，只影响下一次 start（ADR-0013、ADR-0017）。

**没有命令表**。下发给 Node 的命令由 Reconciler 从期望状态派生，期望状态的表本身就是队列（ADR-0014）。
