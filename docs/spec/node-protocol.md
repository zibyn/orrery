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
  rpc SubmitGeneration(stream GenerationChunk) returns (GenerationAck);
  rpc CommitGeneration(CommitGenerationRequest) returns (CommitGenerationResponse);
}
```

## Control：控制流，一个 Agent 一条

Agent 发的第一条消息必须是 `Hello`；Control Plane 回 `HelloAck`，或用 `incompatible` 拒绝（ADR-0014）。

`AgentMessage`（oneof）：
- `Hello{node_id, agent_version, proto_major, proto_minor, mesh?{public_key, sandbox_subnet, endpoint?}}`；`mesh` 只在 containerd Node 且内核有 WireGuard 时带，`endpoint` 是 `--mesh-endpoint`（`runtime.md` §15，ADR-0027）
- `Heartbeat{runtime_health, disk_free, gpu_health[]}`
- `Capabilities{runtime_provider, entries[{name, value: enforced|best_effort|unsupported}], gpus[{uuid, model}]}`；`runtime_provider` 是这台 Node 唯一的 Runtime Provider（`containerd`、`local`，由 Agent 的 `--runtime` 选定）；`entries` 含 `resource_mount`、`snapshot_build`、`kernel_isolation`、`egress_policy` 和 `sandbox_links`；`sandbox_links` 由注册时的 Link 自检决定，Local 上为 `unsupported`；`kernel_isolation` 在 runsc Node 上为 `enforced`，runc 和 Local 上为 `unsupported`；`egress_policy` 由注册时的出网自检决定，Local 上为 `unsupported`（`runtime.md` §13，ADR-0026）；`resource_mount` 由 Agent 对 `orrery-resourcefs` 的自检决定，containerd 和 Local 都一样（`runtime.md` §10、§11）；`snapshot_build` 由 BuildKit 端点的自检决定，与 Runtime 无关（ADR-0021）
- `SandboxObserved{sandbox_id, generation, phase: running|committing|gone|create_failed|commit_failed, cause?, address?, commit_retrying?}`；`address` 随 `running` 上报，是 CNI 分给这次 Create 的地址，Control Plane 存为 Sandbox 的 `address`，`gone` 时清空；`create_failed` 的 cause 可以是 `resource_fetch_failed`，即 manifest 读不到（`runtime.md` §11）。ADR-0030 去掉了 `fetching_resources`。Runtime 结束后（`StopSandbox` 或 Runtime 死亡），Agent 先报 `committing`，文件系统和所挂卷的 Generation 都提交成功后才报 `gone`；`commit_retrying` 为真时 Control Plane 挂上 `commit_stalled`；upperdir 读不出时报 `commit_failed`（`runtime.md` §16，ADR-0031）
- `ExecObserved{exec_id, started_at}`（结果不走这里，见 ExecOutput）
- `BuildObserved{version_id, digest?, from_digests[], cause?}`：构建结束时上报，见下文「构建」
- `MeshReport{peers[{node_id, last_handshake_at}]}`：每个上报间隔一次，Control Plane 据此维护内存里的连通矩阵（`runtime.md` §15）
- `CommandResult{command_id, error?}`

`ControlMessage`（oneof）：
- `HelloAck{accepted, registry_credential, egress_rules{allow[], deny[]}}`；`egress_rules` 是部署配置里的 Egress Rules，原样下发，条目格式 `CIDR[:port]`（`defaults.md`）。部署配置没有热加载，所以 Rules 只在 Control Plane 重启后变化，而那时每个 Agent 都会重连、重新收到 `HelloAck`。Agent 只把它放在内存里，用于此后的每个 Create（`runtime.md` §13）
- `CreateSandbox{sandbox_id, generation, image_digest, limits, gpu_uuids[], restore?{generation_id}, volumes[{volume_id, path, head_generation_id?}], resources[{address, mount}], egress{mode: open|none|allowlist, allow[]}, link_target}`；manifest 在 Create 时经 `Presign` 读取，对象在读到时才经 `Presign` 拉取（`runtime.md` §11）；`restore` 是 Sandbox 的 head Generation，没有提交过时省略，Agent 本地缓存的差异等于它时直接复用，否则先恢复再起 Runtime（`runtime.md` §16）；`volumes` 交给 `orrery-resourcefs` 以可写方式挂载（`runtime.md` §17）；`egress` 是 Sandbox 存下的 Egress Mode，每次（重新）下发都带同一份，Agent 拿它和当前的 Egress Rules 在 Create 时算出 Egress Policy。Mode 不是 `open` 时 Control Plane 只会把它下发给 `egress_policy` 为 `enforced` 的 Node；Agent 收到时自己的 `egress_policy` 不是 `enforced`，就以 `create_failed`、cause `runtime_create_failed` 上报，不降级。`link_target` 在 Sandbox 带 `links` 时为真，每次（重新）下发都一样；Agent 随 veth 记下它，在 Create 里、Sandbox 运行之前就渲染这个目标的入站链（此时没有任何来源规则，只以丢弃结尾），之后的 `SandboxLinks` 只往里加放行规则；Agent 重启时按记下的值重建
- `StopSandbox{sandbox_id, generation}`
- `DropSandboxDisk{sandbox_id}`：Sandbox 删除后下发给它上次运行的 Node，删掉缓存的差异和容器快照；那台 Node 不可达或已删除时不下发，缓存由磁盘水位淘汰
- `MeshPeers{peers[{node_id, public_key, sandbox_subnet, endpoint?}]}`：除本机外所有带 `mesh` 的 Node，Sandbox 子网与本机重叠的除外；整份下发，由 Reconciler 的 sweep 在内容变化时发出（Node 注册、重连、删除或被宣告失联都会唤醒一次 sweep），命令 id 由内容派生，所以内容不变不重发，发送队列满或连接断开后下一轮 sweep 会重发
- `SandboxLinks{links[{target_id, target_address, source_id, source_address, ports[]}]}`：一端在本机的全部 Sandbox Link，只含目标有地址的条目；来源不在运行或已删除时 `source_address` 为空，Agent 不为它渲染放行规则，目标的入站链照样以丢弃结尾；整份下发，任一端地址变化时重发。Agent 收到后重新渲染 `orrery` 和 `orrery_mesh` 两张表（`runtime.md` §13、§15）。这两种消息只发一次，Agent 应用失败时自己按退避重新应用最新一份，直到成功、被新的一份取代或流断开（`defaults.md`）
- `StartExec{exec_id, sandbox_id, generation, argv, env, secret_env, cwd, timeout, log_cap, stdin?, pty?}`
- `CancelExec{exec_id}`
- `PublishSandbox{version_id, sandbox_id, path}`：只对运行中的 Sandbox，见下文「Sandbox publish」
- `BuildSnapshot{version_id, sandbox_id, exec_id, dockerfile, base_digest?, contexts[{name, address}], secrets[{id, value}], limits, timeout, push_repository}`
- `OpenTunnel{tunnel_id, target: agent_http | sandbox_port{sandbox_id, port}}`

每条命令都带 `command_id`，并且是幂等的。`generation` 就是 fencing token：对于比当前代数旧的命令，Agent 一律拒绝（ADR-0015）。命令由 Reconciler 根据期望状态反复下发，所以单条命令丢了也没关系。

### Exec 的环境变量

- **Control Plane** 往 `secret_env` 先放 Sandbox 的 Secret 快照，再放这个 Exec 自己的 Secret（`schema.md`），同名时 Exec 的覆盖 Sandbox 的；往 `env` 放 `ORRERY_URL`（取部署配置的 `access.url`，未配置时不放）。这几项都不写入 `execs.env`。
- **Agent** 再追加 `PATH`：取调用方 `env` 里的 `PATH`，没有就取镜像配置里的 `PATH`（Local Runtime 取 Agent 自身的 `PATH`），在末尾追加 `<bin>`，不遮挡镜像里的同名命令。`<bin>` 是 `/.orrery/bin`；Local Runtime 没有 `/.orrery`，`<bin>` 是 Agent 自己的 bin 目录，即 Agent 可执行文件所在、旁边放着 `orrery` 的目录（`runtime.md` §2、§10）。
- **Local Runtime** 另设 `ORRERY_WORKSPACE`，值为 Sandbox 工作目录在宿主上的路径，以及 `ORRERY_VOLUMES`，值为每个卷一个子目录的挂载目录（`runtime.md` §10）。
- **合并顺序**：调用方的 Exec `env`，然后平台写入 `env` 的值，然后 Sandbox 的 Secret，最后 Exec 的 Secret（后两者由 Control Plane 合成一个 `secret_env`），同名时后者覆盖前者；唯一例外是 `PATH`，按上面的规则追加而不覆盖。平台不写 Git 配置，调用方在 Exec `env` 里写的 `GIT_CONFIG_*` 原样生效（ADR-0032）。Exec `env` 不做保留名校验，保留名只约束 Secret（ADR-0017）。PTY Exec 走同一条路径。

## Presign：Node 取放对象

`Presign{sha256[], manifests: address[], generations: generation_id[], method: GET|PUT} → urls[]` 是一元 RPC，用 Node Token 认证。一次最多签 1,000 个（`defaults.md`），超出整批返回 `INVALID_ARGUMENT`，这样响应留在 gRPC 默认的 4 MiB 以内；Agent 分批调用。`manifests` 只签 GET，指向 `manifests/<owner_id>/<address>`；`generations` 指向 `generations/<owner_id>/<id>`，GET 和 PUT 都可以。Control Plane 只为下面几类对象签名，其余整批返回 `INVALID_ARGUMENT`（不用 `NOT_FOUND`，它会让 Agent 清掉身份）：

- 分配给本 Node 的 Sandbox（含构建行）在 `sandbox_resources` 里引用的版本，及其中的对象（不超过 4 MiB 的文件和大文件的块），只签 GET；
- 分配给本 Node 的 Sandbox 的 head Generation 和它持有的卷的 head Generation，及其中的对象，只签 GET；
- 本 Node 进行中的 publish，或本 Node 上 Sandbox 和它持有的卷进行中的 Generation 提交，缺失的对象和 Generation manifest，只签 PUT，带 `x-amz-checksum-sha256`。

URL 有效期见 `defaults.md`。Node 读写对象存储只走这条路（ADR-0017）。

## Generation 提交

ADR-0031。Sandbox 的文件系统和卷都按 Generation 提交，流程相同：

1. Agent 扫描出变化的条目（`runtime.md` §16、§17），经 `SubmitGeneration` 分批上送；首批带 `{sandbox_id | volume_id, generation, base_generation_id?}`，`generation` 是持有者 Sandbox 的 fencing token，`base_generation_id` 是 Agent 据以做增量的 head。Control Plane 在 `GenerationAck{missing sha256[]}` 里返回缺失的对象；`generation` 已过期、Sandbox 不在本 Node 或不持有该卷时返回 `FAILED_PRECONDITION`。
2. Agent 用 `Presign` 取 PUT，上传缺失的对象和完整 manifest（增量条目合入 base 之后的结果，格式同 Resource manifest，另带 mode、属主、mtime、xattr、硬链接、whiteout 和 opaque 目录），然后调 `CommitGeneration{upload_id}`。
3. Control Plane 在一个事务里：对每个对象做 `HeadObject`（`ChecksumMode`），核对 fencing token 仍是当前的、`head_generation_id` 仍等于 `base_generation_id`，写 `generations` 和 `generation_objects`，推进 head。任何一条不满足就整体失败，head 不动；Agent 按 `defaults.md` 退避重试。

## Sandbox publish

1. `POST /sandboxes/{id}/publish` 建一个 `pending` 的 ResourceVersion，Reconciler 向 Sandbox 所在 Node 下发 `PublishSandbox`。只对 `ready` 的 Sandbox：Agent 读宿主上该 Sandbox 的 rootfs 合并视图（Local 读工作目录），路径规则同 ADR-0012，不提交 Generation。卷的 publish 不经 Node：Control Plane 从卷的 head Generation 取出 `path` 下的条目写成 manifest，对象已在同一 owner 的 CAS 里。
2. Agent 遍历目录，按 4 MiB 切块计算哈希：不超过 4 MiB 的文件整个是一个对象，更大的文件条目带 `blocks`（`api.md` §3）。条目经 `SubmitManifest` 分批上送（首批带 `version_id`）。Control Plane 规范化后写入 `address`、`resource_version_objects`，在 `ManifestAck{missing sha256[]}` 里返回缺失的对象（文件或块）。`version_id` 不是本 Node 进行中的 publish 时返回 `INVALID_ARGUMENT`。
3. Agent 用 `Presign` 取 PUT，上传缺失对象，然后对这条命令回 `CommandResult`。
4. Control Plane 对每个对象（文件或块）做 `HeadObject`（`ChecksumMode`），并按块列表算出大文件的 sha256；全部存在且一致就把版本置为 `ready` 并移动 current 指针；否则或 `CommandResult` 带错误就置为 `failed`，指针不动。

## 构建

`BuildSnapshot` 由 Reconciler 在构建准入、拿到 Allocation 之后下发。`sandbox_id` 是承载 Allocation 的 `kind=build` 行，`exec_id` 是构建 Exec，构建输出按普通 Exec 写进 Log Spool，经 `ExecOutput` 上送。`contexts` 是 Node 上的 Resource 挂载（`runtime.md` §11）；`base_digest` 和 `push_repository` 都指向平台 registry，拉取和推送用 `HelloAck` 的 registry 凭据。`secrets` 的值与 `StartExec.secret_env` 一样，由 Control Plane 解密后下发。Agent 推送成功后发 `BuildObserved{digest, from_digests}`，失败发 `cause`。`CancelExec{exec_id}` 中止构建会话。执行细节见 `runtime.md` §12。

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
