# Stars Sandbox

Python SDK: [`sdk/python`](sdk/python/README.md)，用 `pip install "git+https://github.com/zibyn/stars-sandbox#subdirectory=sdk/python"` 安装。

## MCP Tool API

`stars-sandbox mcp` 通过 stdio 提供 MCP 工具（ADR-0022；工具清单与语义见 `docs/spec/api.md` §7）。凭据解析与其它子命令一致：`--token` > `STARS_SANDBOX_TOKEN` > 配置文件；不接受 Sandbox Token。诊断信息写 stderr，stdout 只有协议帧。

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


