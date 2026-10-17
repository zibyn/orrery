# Node Agent 协议

Protobuf 是内部 gRPC 契约，只在 `node` 监听面上使用，并且全部走 TLS。连接由 Node Agent 主动拨出，Control Plane 是 gRPC 服务端。本文锁定 RPC 和消息的轮廓，字段编号与 `.proto` 由实现决定。

## 服务

```proto
service Node {
  // 唯一不需认证的入口：凭 Enrollment Packet 换取 Node Token
  rpc Redeem(RedeemRequest) returns (RedeemResponse);
  // 以下三个都带 Node Token
  rpc Control(stream AgentMessage) returns (stream ControlMessage);
  rpc ExecOutput(stream OutputBatch) returns (stream OutputAck);
  rpc Tunnel(stream TunnelFrame) returns (stream TunnelFrame);
}
```

## Control：控制流，一个 Agent 一条

Agent 发的第一条消息必须是 `Hello`；Control Plane 回 `HelloAck`，或用 `incompatible` 拒绝（ADR-0014）。

`AgentMessage`（oneof）：
- `Hello{node_id, agent_version, proto_major, proto_minor}`
- `Heartbeat{runtime_health, disk_free, gpu_health[]}`
- `Capabilities{runtime, entries[{name, value: enforced|best_effort|unsupported}], gpus[{uuid, model}]}`
- `SandboxObserved{sandbox_id, generation, phase: running|gone|create_failed, cause?}`
- `WorkspaceObserved{workspace_id, phase, cause?}`
- `ExecObserved{exec_id, started_at}`（结果不走这里，见 ExecOutput）
- `CommandResult{command_id, error?}`

`ControlMessage`（oneof）：
- `HelloAck{accepted, registry_credential}`
- `ProvisionWorkspace` · `DeleteWorkspace`
- `CreateSandbox{sandbox_id, generation, image_digest, limits, gpu_uuids[], workspace{id, mode}, resources[{address, presigned_url, mount}]}`
- `StopSandbox{sandbox_id, generation}`
- `StartExec{exec_id, sandbox_id, generation, argv, env, secret_env, cwd, timeout, log_cap, stdin?, pty?}`
- `CancelExec{exec_id}`
- `PublishWorkspace{workspace_id, path, upload_targets}`
- `BuildSnapshot{…}`
- `OpenTunnel{tunnel_id, target: agent_http | sandbox_port{sandbox_id, port}}`

每条命令都带 `command_id`，并且是幂等的。`generation` 就是 fencing token：对于比当前代数旧的命令，Agent 一律拒绝（ADR-0015）。命令由 Reconciler 根据期望状态反复下发，所以单条命令丢了也没关系。

### Exec 的环境变量

Sandbox Token 不在 `CreateSandbox` 里，而是随每个 `StartExec` 下发，所以 Agent 重启、Reconciler 重复下发都拿得到它。前例是 Coder 的 `workspace_agents.auth_token`：明文存储，放进每个进程的环境（`docs/research/recheck-identity-network-channel-vs-coder-daytona.md` I3、C1）。

- **Control Plane** 往 `secret_env` 放 Secret 快照（`schema.md`）和 `STARS_SANDBOX_TOKEN`，往 `env` 放 `STARS_URL`（取 `STARS_ACCESS_URL`，未配置时不放）。这几项都不写入 `execs.env`。
- **Agent** 再追加两组：
  - `GIT_CONFIG_COUNT`/`GIT_CONFIG_KEY_n`/`GIT_CONFIG_VALUE_n`，先写一条 `credential.helper=` 清空镜像的 helper 列表，再写 `credential.helper=/.stars/bin/stars-sandbox git-credential`；
  - `PATH`：取调用方 `env` 里的 `PATH`，没有就取镜像配置里的 `PATH`（Local Runtime 取 Agent 自身的 `PATH`），在末尾追加 `/.stars/bin`，不遮挡镜像里的同名命令。
- **合并顺序**：调用方的 Exec `env` 在前，平台写入的值最后合并，同名时覆盖调用方；唯一例外是 `PATH`，按上面的规则追加而不覆盖。Exec `env` 不做保留名校验，保留名只约束 Secret（ADR-0017）。PTY Exec 走同一条路径。

## ExecOutput：每个 Exec 一条

Agent 推送 `OutputBatch{exec_id, frames[{seq, stream, data}]}`。Control Plane 写入 `exec_log_chunks` 后回 `OutputAck{exec_id, stored_seq}`。重连后，Agent 从 `stored_seq` 之后接着推。`seq` 是帧在 Node 记录文件里的字节偏移（`runtime.md`）。最后一帧的 `stream` 是 `exit`，携带 `{cause, exit_code?, signal?}`；它的 `OutputAck` 到达后，Agent 删除该 Exec 在 Node 上的记录（ADR-0019）。PTY 的输出不走这条流（ADR-0017），PTY Exec 的这条流只传 exit 帧。

## Tunnel：反向拨号的字节流

Gateway 需要访问某个 Node 时，Control Plane 先在 `Control` 上下发 `OpenTunnel`，Agent 再用同一个 `tunnel_id` 发起一条 `Tunnel` 流作为回拨。之后的 `TunnelFrame{tunnel_id?, data}` 都只承载原始字节。

- `agent_http`：隧道另一端是 Agent 的内部 HTTP 接口。文件 API 和 PTY WebSocket 由 Gateway 原样反向代理过去。这个接口不对外，只能通过隧道访问。
- `sandbox_port`：Agent 进入 Sandbox 的网络命名空间，拨 `127.0.0.1:PORT`，用于端口转发。

这样文件、PTY 和端口共用一种机制，不必在 Protobuf 里各自定义消息（ADR-0018）。

## 错误映射

gRPC status 与 REST 错误码一一对应，`google.rpc.ErrorInfo.reason` 填同一个 `code`：

| gRPC | `code` |
|---|---|
| `INVALID_ARGUMENT` | `invalid_request` |
| `UNAUTHENTICATED` | `unauthenticated` |
| `NOT_FOUND` | `not_found` |
| `FAILED_PRECONDITION` | `conflict`，或握手时的 `incompatible` |
| `RESOURCE_EXHAUSTED` | `resource_exhausted` |
| `UNIMPLEMENTED` | `unsupported` |
| `UNAVAILABLE` | `unavailable` |
| `INTERNAL` | `runtime_failed`, `internal` |

`resource_exhausted` 和 `runtime_failed` 是 Runtime Provider 的错误（ADR-0009），只在 Node 内部出现。对外它们会变成 Sandbox 的 `cause`，例如 `runtime_create_failed`。ADR-0010 的非对称失败规则不变：`NOT_FOUND`/`UNAUTHENTICATED` 会让 Agent 清掉本地身份，传输层失败则保留身份并退避重试。
