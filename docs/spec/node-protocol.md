# Node Agent 协议

Protobuf 是内部 gRPC 契约，只在 `node` 监听面上使用，并且全部走 TLS。连接由 Node Agent 主动拨出，Control Plane 是 gRPC 服务端。本文锁定 RPC 和消息的轮廓，字段编号与 `.proto` 由实现决定。

## 服务

```proto
service Node {
  // 唯一不需认证的入口：凭 Enrollment Packet 换取 Node Token
  rpc Redeem(RedeemRequest) returns (RedeemResponse);
  // 以下都带 Node Token
  rpc Control(stream AgentMessage) returns (stream ControlMessage);
  rpc ExecOutput(stream OutputBatch) returns (stream OutputAck);
  rpc Tunnel(stream TunnelFrame) returns (stream TunnelFrame);
  rpc Presign(PresignRequest) returns (PresignResponse);
  rpc SubmitManifest(stream ManifestChunk) returns (ManifestAck);
}
```

## Control：控制流，一个 Agent 一条

Agent 发的第一条消息必须是 `Hello`；Control Plane 回 `HelloAck`，或用 `incompatible` 拒绝（ADR-0014）。

`AgentMessage`（oneof）：
- `Hello{node_id, agent_version, proto_major, proto_minor, mesh?{public_key, sandbox_subnet, endpoint?}}`；`mesh` 只在 containerd Node 且内核有 WireGuard 时带，`endpoint` 是 `--mesh-endpoint`（`runtime.md` §15，ADR-0027）
- `Heartbeat{runtime_health, disk_free, gpu_health[]}`
- `Capabilities{runtime_provider, entries[{name, value: enforced|best_effort|unsupported}], gpus[{uuid, model}]}`；`runtime_provider` 是这台 Node 唯一的 Runtime Provider（`containerd`、`local`，由 Agent 的 `--runtime` 选定）；`entries` 含 `resource_mount`、`snapshot_build`、`kernel_isolation`、`egress_policy` 和 `sandbox_links`；`sandbox_links` 由注册时的 Link 自检决定，Local 上为 `unsupported`；`kernel_isolation` 在 runsc Node 上为 `enforced`，runc 和 Local 上为 `unsupported`；`egress_policy` 由注册时的出网自检决定，Local 上为 `unsupported`（`runtime.md` §13，ADR-0026）；Local Runtime 的 `resource_mount` 报 `unsupported`，`snapshot_build` 由 BuildKit 端点的自检决定，与 Runtime 无关（ADR-0021）
- `SandboxObserved{sandbox_id, generation, phase: fetching_resources|running|gone|create_failed, cause?, address?}`；`address` 随 `running` 上报，是 CNI 分给这次 Create 的地址，Control Plane 存为 Sandbox 的 `address`，`gone` 时清空；`fetching_resources` 让 Sandbox 挂上同名 condition，`create_failed` 的 cause 可以是 `resource_fetch_failed`（`runtime.md` §11）
- `WorkspaceObserved{workspace_id, phase, cause?}`
- `ExecObserved{exec_id, started_at}`（结果不走这里，见 ExecOutput）
- `BuildObserved{version_id, digest?, from_digests[], cause?}`：构建结束时上报，见下文「构建」
- `MeshReport{peers[{node_id, last_handshake_at}]}`：每个上报间隔一次，Control Plane 据此维护内存里的连通矩阵（`runtime.md` §15）
- `CommandResult{command_id, error?}`

`ControlMessage`（oneof）：
- `HelloAck{accepted, registry_credential, egress_rules{allow[], deny[]}}`；`egress_rules` 是部署配置里的 Egress Rules，原样下发，条目格式 `CIDR[:port]`（`defaults.md`）。部署配置没有热加载，所以 Rules 只在 Control Plane 重启后变化，而那时每个 Agent 都会重连、重新收到 `HelloAck`。Agent 只把它放在内存里，用于此后的每个 Create（`runtime.md` §13）
- `ProvisionWorkspace` · `DeleteWorkspace`
- `CreateSandbox{sandbox_id, generation, image_digest, limits, gpu_uuids[], workspace{id, mode}, resources[{address, mount}], egress{mode: open|none|allowlist, allow[]}}`；对象经 `Presign` 拉取；`egress` 是 Sandbox 存下的 Egress Mode，每次（重新）下发都带同一份，Agent 拿它和当前的 Egress Rules 在 Create 时算出 Egress Policy。Mode 不是 `open` 时 Control Plane 只会把它下发给 `egress_policy` 为 `enforced` 的 Node；Agent 收到时自己的 `egress_policy` 不是 `enforced`，就以 `create_failed`、cause `runtime_create_failed` 上报，不降级
- `StopSandbox{sandbox_id, generation}`
- `MeshPeers{peers[{node_id, public_key, sandbox_subnet, endpoint?}]}`：除本机外所有带 `mesh` 的 Node，Sandbox 子网与本机重叠的除外；整份下发，Node 注册、重连或删除时重发
- `SandboxLinks{links[{target_id, target_address, source_id, source_address, ports[]}]}`：一端在本机的全部 Sandbox Link，只含目标有地址的条目；来源不在运行或已删除时 `source_address` 为空，Agent 不为它渲染放行规则，目标的入站链照样以丢弃结尾；整份下发，任一端地址变化时重发。Agent 收到后重新渲染 `orrery` 和 `orrery_mesh` 两张表（`runtime.md` §13、§15）
- `StartExec{exec_id, sandbox_id, generation, argv, env, secret_env, cwd, timeout, log_cap, stdin?, pty?}`
- `CancelExec{exec_id}`
- `PublishWorkspace{version_id, workspace_id, path}`
- `BuildSnapshot{version_id, sandbox_id, exec_id, dockerfile, base_digest?, contexts[{name, address}], secrets[{id, value}], limits, timeout, push_repository}`
- `OpenTunnel{tunnel_id, target: agent_http | sandbox_port{sandbox_id, port}}`

每条命令都带 `command_id`，并且是幂等的。`generation` 就是 fencing token：对于比当前代数旧的命令，Agent 一律拒绝（ADR-0015）。命令由 Reconciler 根据期望状态反复下发，所以单条命令丢了也没关系。

### Exec 的环境变量

Sandbox Token 不在 `CreateSandbox` 里，而是随每个 `StartExec` 下发，所以 Agent 重启、Reconciler 重复下发都拿得到它。前例是 Coder 的 `workspace_agents.auth_token`：明文存储，放进每个进程的环境（`docs/research/recheck-identity-network-channel-vs-coder-daytona.md` I3、C1）。

- **Control Plane** 往 `secret_env` 放 Secret 快照（`schema.md`）和 `ORRERY_SANDBOX_TOKEN`，往 `env` 放 `ORRERY_URL`（取部署配置的 `access.url`，未配置时不放）。这几项都不写入 `execs.env`。
- **Agent** 再追加两组。下面的 `<bin>` 是 `/.orrery/bin`；Local Runtime 没有 `/.orrery`，`<bin>` 是 Agent 自己的 bin 目录，即 Agent 可执行文件所在、旁边放着 `orrery` 的目录（`runtime.md` §2、§10）：
  - `GIT_CONFIG_COUNT`/`GIT_CONFIG_KEY_n`/`GIT_CONFIG_VALUE_n`，先写一条 `credential.helper=` 清空镜像的 helper 列表，再写 `credential.helper=<bin>/orrery git-credential`；
  - `PATH`：取调用方 `env` 里的 `PATH`，没有就取镜像配置里的 `PATH`（Local Runtime 取 Agent 自身的 `PATH`），在末尾追加 `<bin>`，不遮挡镜像里的同名命令。
- **Local Runtime** 另设 `ORRERY_WORKSPACE`，值为 Workspace 在宿主上的路径。
- **合并顺序**：调用方的 Exec `env` 在前，平台写入的值最后合并，同名时覆盖调用方；唯一例外是 `PATH`，按上面的规则追加而不覆盖。Exec `env` 不做保留名校验，保留名只约束 Secret（ADR-0017）。PTY Exec 走同一条路径。

## Presign：Node 取放对象

`Presign{sha256[], manifests: address[], method: GET|PUT} → urls[]` 是一元 RPC，用 Node Token 认证。`manifests` 只签 GET，指向 `manifests/<owner_id>/<address>`。Control Plane 只为两类对象签名，其余整批返回 `INVALID_ARGUMENT`（不用 `NOT_FOUND`，它会让 Agent 清掉身份）：

- 分配给本 Node 的 Sandbox（含构建行）在 `sandbox_resources` 里引用的版本，及其中的对象，只签 GET；
- 本 Node 进行中的 publish 缺失的对象，只签 PUT，带 `x-amz-checksum-sha256`。

URL 有效期见 `defaults.md`。Node 读写对象存储只走这条路（ADR-0017）。

## Workspace publish

1. `POST /workspaces/{id}/publish` 建一个 `pending` 的 ResourceVersion，Reconciler 向 Workspace 所在 Node 下发 `PublishWorkspace`。不要求有运行中的 Sandbox，Agent 直接读宿主上的 Workspace 目录，路径规则同 ADR-0012。
2. Agent 遍历目录、计算哈希，经 `SubmitManifest` 分块上送 manifest 条目（首块带 `version_id`）。Control Plane 规范化后写入 `address`、`resource_version_objects`，在 `ManifestAck{missing sha256[]}` 里返回缺失的对象。`version_id` 不是本 Node 进行中的 publish 时返回 `INVALID_ARGUMENT`。
3. Agent 用 `Presign` 取 PUT，上传缺失对象，然后对这条命令回 `CommandResult`。
4. Control Plane 对每个对象做 `HeadObject`（`ChecksumMode`），全部存在且 sha256 一致就把版本置为 `ready` 并移动 current 指针；否则或 `CommandResult` 带错误就置为 `failed`，指针不动。

## 构建

`BuildSnapshot` 由 Reconciler 在构建准入、拿到 Allocation 之后下发。`sandbox_id` 是承载 Allocation 的 `kind=build` 行，`exec_id` 是构建 Exec，构建输出按普通 Exec 写进 Log Spool，经 `ExecOutput` 上送。`contexts` 的对象经 Node 缓存拉取；`base_digest` 和 `push_repository` 都指向平台 registry，拉取和推送用 `HelloAck` 的 registry 凭据。`secrets` 的值与 `StartExec.secret_env` 一样，由 Control Plane 解密后下发。Agent 推送成功后发 `BuildObserved{digest, from_digests}`，失败发 `cause`。`CancelExec{exec_id}` 中止构建会话。执行细节见 `runtime.md` §12。

## ExecOutput：每个 Exec 一条

Agent 推送 `OutputBatch{exec_id, frames[{seq, stream, data}]}`。Control Plane 写入 `exec_log_chunks` 后回 `OutputAck{exec_id, stored_seq}`。重连后，Agent 从 `stored_seq` 之后接着推。`seq` 是帧在 Node 记录文件里的字节偏移（`runtime.md`）。最后一帧的 `stream` 是 `exit`，携带 `{cause, exit_code?, signal?}`；它的 `OutputAck` 到达后，Agent 删除该 Exec 在 Node 上的记录（ADR-0019）。PTY 的输出不走这条流（ADR-0017），PTY Exec 的这条流只传 exit 帧。

## Tunnel：反向拨号的字节流

Gateway 需要访问某个 Node 时，Control Plane 先在 `Control` 上下发 `OpenTunnel`，Agent 再用同一个 `tunnel_id` 发起一条 `Tunnel` 流作为回拨。之后的 `TunnelFrame{tunnel_id?, data}` 都只承载原始字节。

- `agent_http`：隧道另一端是 Agent 的内部 HTTP 接口。文件 API 和 PTY WebSocket 由 Gateway 原样反向代理过去。这个接口不对外，只能通过隧道访问。
- `sandbox_port`：Sandbox 里的 Supervisor 经运行目录上的 unix socket 转到 `127.0.0.1:PORT`，Agent 连这个 socket，不进入 Sandbox 的网络命名空间（`runtime.md` §14，ADR-0026）。用于端口转发。

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
