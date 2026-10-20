# ADR-0022: MCP Tool API: a hand-picked tool set, handles instead of Tasks, no PTY tool

- Status: accepted; amends ADR-0017's MCP and SDK entries in place
- Date: 2026-10-18

## Context

ADR-0017 generated one MCP tool per `user`-scope operation, about 45, plus two composite tools: run an Exec and wait, and a PTY send-and-read. Grilling M2 (`#107`, primary sources read on 2026-10-18) found that no vendor does this and that the spec is behind the latest MCP revision:

- Daytona (`daytona/clients`; `daytonaio/daytona` was archived in 2026-06) serves `daytona mcp start` over stdio with hand-written tools, 12 core and 27 computer-use. `execute_command` blocks until the command ends and does not truncate. File tools take `encoding: text|base64`. There is no PTY tool.
- Coder's `coder exp mcp server` is built on the official go-sdk with 39 hand-written tools. `coder_workspace_execute` waits 10 seconds by default, at most 5 minutes; past that it returns a background process id that `coder_workspace_process_output` reattaches to. Output is cut to its first 32 KB. There is no PTY tool.
- E2B deprecated its MCP server; it had one tool, `run_code`.
- MCP 2026-07-28 (GA) is stateless: no session and no `initialize` handshake; state across calls travels as a server-issued handle passed back as a tool argument ("Stateful Tools"). Tasks moved to the extension `io.modelcontextprotocol/tasks`; Logging, Sampling and Roots are deprecated, and logs go to stderr. A server MUST keep its output within its `outputSchema`; `inputSchema` is a non-empty object in JSON Schema 2020-12 by default; business errors SHOULD use `isError`; `tools/list` order SHOULD be stable; tool names SHOULD use `[A-Za-z0-9_.-]`, 1–128 characters.
- go-sdk v1.8.0 (2026-09-14) supports 2026-07-28 and earlier revisions, accepts raw JSON Schema in `AddTool`, has `SetCacheable`, does not implement the Tasks extension, and declares the `logging` capability unless told not to.

Generated tools include Resource upload, manifest and presign operations that an agent never uses, and they crowd its context. `pty_send_and_read` would need the `mcp` process to hold a WebSocket and decide when output has "gone quiet", which is hard to test.

## Decision

**A hand-picked tool set.** The MCP Tool API is a fixed list of hand-written tools over the public REST API, covering Sandbox lifecycle, Exec, Workspace files and port URLs, not one tool per OpenAPI operation. The list, schemas and semantics are `docs/spec/api.md` §7. Each tool declares a title, annotations and both an input and an output schema, and its structured result conforms to the output schema.

**A long Exec is a handle plus polling.** `exec_run` waits a bounded time and, if the Exec has not finished, returns its `exec_id`; `exec_output` reattaches from an offset. The Exec id is the handle, as MCP 2026-07-28's Stateful Tools describes and as Coder does. The server is stateless and holds no connection between calls.

**No PTY tool.** Interactive programs are driven through the CLI or the Web console.

**stdio only, served by the CLI.** `orrery mcp` runs on the caller's machine with the caller's API Token, declares only the `tools` capability and writes diagnostics to stderr.

## Consequences

- An agent sees 17 tools whose descriptions it can afford to read.
- Every REST change that an agent needs is a deliberate tool change, not a free side effect of the OpenAPI document.
- An agent cannot drive an interactive program; it runs a non-interactive command instead.

## Rejected alternatives

- One tool per OpenAPI operation: no vendor ships it, and operations an agent never calls crowd its context.
- Hand-writing the Tasks extension: go-sdk does not implement it, and a handle plus polling covers long Execs without it.
- `pty_send_and_read`: it needs a WebSocket held across calls and a quiet-output heuristic, neither vendor has a PTY tool, and the stateless protocol has no session to keep it in.
