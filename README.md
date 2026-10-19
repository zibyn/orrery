# Stars Sandbox

面向 agent 的远程开发与 GPU 执行平台。Control Plane 统一管理身份、Workspace、Sandbox、Exec 和资源；Node Agent 在受信任的主机上通过 Docker、Podman 或 Local Runtime 实际执行命令。

- **Workspace**：可变工作目录，永久绑定一个 Node，可以是持久的，也可以随 Sandbox 一起删除（临时 Workspace）。
- **Sandbox**：短期隔离执行实例，挂载一个 Workspace 和若干只读 Resource，按 CPU、内存、GPU 分配。
- **Exec**：持久执行意图。输出按序落盘，Node Agent 重启后可重新接上；另有可重连的 PTY。
- **Snapshot**：可复用的环境模板，支持按 digest 导入和 BuildKit 构建。
- **Resource**：内容寻址的稳定产物，由 Workspace `publish` 或 CLI 上传产生。
- **多用户与凭据**：API Token、Session、OIDC、User Quota、Secret、Git Account Link 和审计记录。

完整术语以 [`GLOSSARY.md`](GLOSSARY.md) 为准；架构决策见 [`docs/adr/`](docs/adr/)。

## 架构

```
                 ┌──────────────── Control Plane ────────────────┐
  Web / CLI /    │  api：REST + Gateway + Web console            │
  SDK / MCP ───▶ │  node：Node Agent 流 + Enrollment Packet      │
                 │  PostgreSQL · 对象存储 · registry · nginx     │
                 └───────────────────┬───────────────────────────┘
                                     │ Node Agent 出站连接
                          ┌──────────▼──────────┐
                          │      Node Agent     │
                          │ Docker / Podman /   │
                          │ Local Runtime       │
                          └─────────────────────┘
```

- **Control Plane**（`stars-sandbox-server`）：身份、调度、生命周期、配额、审计，以及 API、Gateway 和 Web 控制台。它只决策和执行状态，不直接进入 Sandbox。
- **Node Agent**（`stars-sandbox-agent`）：在 Node 上落地 Sandbox、Exec、文件、Git、端口转发和构建；用 Enrollment Packet 注册后长期出站连接。
- **Gateway**：文件、Git、Exec 输出、PTY 和端口隧道都走已授权的 Data Plane，不暴露 Sandbox 的网络地址。
- 部署只有**一个公开端口**：nginx 终止 TLS，按路径分流到 registry（`/v2/`）、Node gRPC（`/stars.node.v1.Node/`）和其余 API/控制台。

## 部署

部署是 pull 式的：服务器从 release tag 取 `deploy/install.sh` 执行，GitHub 不持有服务器凭据。详见 [`docs/deploy.md`](docs/deploy.md)（前置条件、TLS、Node 注册、升级与回滚），所有配置项见 [`docs/spec/defaults.md`](docs/spec/defaults.md)。

首次安装后还需要：配置平台 Snapshot、用 `stars-sandbox-server bootstrap` 创建第一个 admin、签发 Enrollment Packet 注册 Node。

## 客户端

### Web 控制台

与 API 同源，由 `stars-sandbox-server` 在 `/` 下提供；Sandbox 创建、终端、文件浏览、Snapshot、用户与节点管理都在这里。

### CLI

`stars-sandbox` 只调用公开 REST API 和 Gateway。凭据解析顺序：`--token` > `STARS_SANDBOX_TOKEN` > 配置文件（`--config`、`STARS_SANDBOX_CONFIG` 或 `$XDG_CONFIG_HOME/stars-sandbox/config.json`）；API 地址同理用 `--api-url` / `STARS_SANDBOX_API_URL`。

Sandbox 的创建走控制台、REST API、SDK 或 MCP；CLI 负责创建之后的操作：

```sh
stars-sandbox login --username alice          # 或 --password-stdin
stars-sandbox exec run <sandbox-id> -- uname -a
stars-sandbox shell <sandbox-id> -- /bin/bash
stars-sandbox files ls <sandbox-id> src
stars-sandbox files cp ./config.toml <sandbox-id>:src/config.toml
stars-sandbox files cp <sandbox-id>:out/report.txt .
stars-sandbox port-forward <sandbox-id> 8080:3000
```

`stars-sandbox` 还覆盖 Snapshot 导入/构建、Resource 上传/发布、Secret、Git Account、token、用户和节点管理；`stars-sandbox` 无参数时打印全部命令。

### REST API

```sh
curl -fsS -X POST https://cp.example.com/api/v1/sandboxes \
  -H "Authorization: Bearer $STARS_SANDBOX_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"name":"demo","cpu":1,"memory":1073741824}'
```

约定、资源路径、错误码和 Gateway 语义见 [`docs/spec/api.md`](docs/spec/api.md)。`stars-sandbox-server openapi` 输出由代码生成的 OpenAPI 文档。

### MCP Tool API

`stars-sandbox mcp` 通过 stdio 提供 MCP 工具（ADR-0022；工具清单与语义见 `docs/spec/api.md` §7）。凭据解析与其它子命令一致，但不接受 Sandbox Token。诊断信息写 stderr，stdout 只有协议帧。

Claude Code：

```sh
claude mcp add stars-sandbox -- stars-sandbox mcp
```

通用 JSON 配置（Claude Desktop 等）：

```json
{
  "mcpServers": {
    "stars-sandbox": {
      "command": "stars-sandbox",
      "args": ["mcp"],
      "env": {
        "STARS_SANDBOX_API_URL": "https://stars.example",
        "STARS_SANDBOX_TOKEN": "ssk_<id>_<secret>"
      }
    }
  }
}
```

典型流程：`sandbox_create` → `exec_run` → `exec_output`（长命令超时后重新接上）→ `sandbox_delete`。

### SDK

Python SDK 见 [`sdk/python`](sdk/python/README.md)，用 `pip install "git+https://github.com/zibyn/stars-sandbox#subdirectory=sdk/python"` 安装。

Go SDK 在 [`client`](client) 包，CLI 和 MCP 都构建在它之上。

## 开发

需要 Go 1.26+；Web 控制台需要 pnpm。

```sh
make build     # go build ./...
make test      # go test ./...
make vet

make web       # 构建 Web 控制台
make console   # 构建并嵌入控制台后再编译
make e2e       # 起 PostgreSQL + Control Plane，跑黑盒 e2e，再清理
make e2e-console  # 同一部署上跑 Playwright
```

`make e2e` 需要 Docker 和用到的镜像；它自带 compose 项目，不会碰现有部署。修改 proto 后 `make proto`，修改 SQL 后 `make sqlc`。

仓库布局：

| 路径 | 内容 |
|---|---|
| `cmd/` | `stars-sandbox`（CLI）、`stars-sandbox-server`、`stars-sandbox-agent` |
| `internal/` | Control Plane、Node Agent、API、调度、Exec、CLI、MCP |
| `client/` | Go SDK |
| `sdk/python/` | Python SDK |
| `web/` | Web 控制台（React + Vite） |
| `proto/` | Node Agent gRPC 协议 |
| `deploy/` | compose、安装脚本、release 配置 |
| `e2e/` | 黑盒场景测试 |
| `docs/` | 规格、ADR、调研与部署指南 |

## 文档

- [`GLOSSARY.md`](GLOSSARY.md)：领域词汇，代码和文档都以此为准。
- [`docs/adr/`](docs/adr/)：架构决策记录（ADR-0009 起）。
- [`docs/spec/`](docs/spec/)：`api.md`（REST 语义权威）、`schema.md`、`runtime.md`、`node-protocol.md`、`defaults.md`。
- [`docs/research/`](docs/research/)：与 Coder、Daytona 等厂商的一手资料对比。
- [`docs/deploy.md`](docs/deploy.md)：部署、TLS、升级与回滚。
- [`AGENTS.md`](AGENTS.md)、[`docs/agents/`](docs/agents/)：agent 协作约定。

`GLOSSARY.md`、`docs/adr/` 和 `docs/spec/` 是锁定的规格：规格是事实来源，代码跟随规格。规格本身有问题时先开 issue，再改文档，最后改代码。
