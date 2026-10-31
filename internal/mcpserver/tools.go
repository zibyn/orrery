package mcpserver

import (
	"encoding/json"
	"fmt"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func boolPtr(b bool) *bool { return &b }

const sandboxSchema = `{
  "type": "object",
  "description": "A Sandbox.",
  "properties": {
    "id": {"type": "string"},
    "name": {"type": "string"},
    "labels": {"type": ["object", "null"], "additionalProperties": {"type": "string"}},
    "runtime": {"type": "string"},
    "egress": {"type": ["object", "null"], "properties": {
      "mode": {"type": "string"},
      "allow": {"type": ["array", "null"], "items": {"type": "string"}}
    }},
    "state": {"type": "string"},
    "cause": {"type": ["string", "null"]},
    "node_id": {"type": ["string", "null"]},
    "snapshot": {"type": "string"},
    "cpu": {"type": "number"},
    "memory": {"type": "integer"},
    "gpu": {"type": ["object", "null"], "properties": {
      "count": {"type": "integer"},
      "model": {"type": ["string", "null"]}
    }},
    "idle_stop": {"type": "integer"},
    "workspace": {"type": ["object", "null"], "properties": {
      "id": {"type": "string"},
      "mode": {"type": "string"}
    }},
    "conditions": {"type": ["array", "null"], "items": {"type": "string"}},
    "queue_position": {"type": ["integer", "null"]},
    "blocked_reason": {"type": ["string", "null"]},
    "created_at": {"type": "string"}
  },
  "required": ["id", "name", "state", "runtime"]
}`

const execResultSchema = `{
  "type": ["object", "null"],
  "properties": {
    "cause": {"type": "string"},
    "exit_code": {"type": ["integer", "null"]},
    "signal": {"type": ["integer", "null"]}
  },
  "required": ["cause"]
}`

const frameSchema = `{
  "type": "object",
  "description": "One Execution Log frame. data is UTF-8 unless encoding is base64.",
  "properties": {
    "seq": {"type": "integer"},
    "stream": {"type": "string", "enum": ["stdout", "stderr", "truncated"]},
    "data": {"type": "string"},
    "encoding": {"type": "string"}
  },
  "required": ["seq", "stream", "data"]
}`

var sandboxListSchema = json.RawMessage(fmt.Sprintf(`{
  "type": "object",
  "properties": {
    "items": {"type": "array", "items": %s},
    "next_cursor": {"type": ["string", "null"]}
  },
  "required": ["items"]
}`, sandboxSchema))

var fileEntrySchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {"type": "string"},
    "name": {"type": "string"},
    "type": {"type": "string"},
    "size": {"type": "integer"},
    "mode": {"type": "integer"},
    "mod_time": {"type": "string"},
    "link_target": {"type": ["string", "null"]}
  },
  "required": ["path", "name", "type"]
}`)

var fileListSchema = json.RawMessage(fmt.Sprintf(`{
  "type": "object",
  "properties": {
    "items": {"type": "array", "items": %s}
  },
  "required": ["items"]
}`, fileEntrySchema))

var execSchema = json.RawMessage(fmt.Sprintf(`{
  "type": "object",
  "properties": {
    "exec_id": {"type": "string"},
    "state": {"type": "string"},
    "result": %s,
    "output": {"type": "array", "items": %s},
    "log_truncated": {"type": "boolean"},
    "after": {"type": "integer", "minimum": 0}
  },
  "required": ["exec_id", "state", "output"]
}`, execResultSchema, frameSchema))

var execOutputSchema = json.RawMessage(fmt.Sprintf(`{
  "type": "object",
  "properties": {
    "exec_id": {"type": "string"},
    "state": {"type": "string"},
    "result": %s,
    "output": {"type": "array", "items": %s},
    "log_truncated": {"type": "boolean"},
    "next_after": {"type": "integer", "minimum": 0}
  },
  "required": ["exec_id", "state", "output", "next_after"]
}`, execResultSchema, frameSchema))

var execCancelSchema = json.RawMessage(fmt.Sprintf(`{
  "type": "object",
  "properties": {
    "exec_id": {"type": "string"},
    "state": {"type": "string"},
    "result": %s,
    "log_truncated": {"type": "boolean"}
  },
  "required": ["exec_id", "state"]
}`, execResultSchema))

const sandboxID = `"sandbox_id": {"type": "string", "description": "The Sandbox id."}`
const pathProp = `"path": {"type": "string", "description": "Path relative to /workspace."}`

func objectSchema(properties, required string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {%s},
  "required": [%s]
}`, properties, required))
}

func (s *server) register(srv *sdk.Server) {
	// The order of these calls is the fixed tools/list order.
	addTool(srv, &sdk.Tool{
		Name:        "sandbox_create",
		Title:       "Create Sandbox",
		Description: "Create a Sandbox and wait up to 60 seconds for it to become ready. If it is still pending, the result carries queue_position and blocked_reason; call sandbox_get with wait_for to keep waiting.",
		InputSchema: objectSchema(`
    "name": {"type": "string", "description": "Name, unique per User."},
    "labels": {"type": "object", "additionalProperties": {"type": "string"}},
    "runtime": {"type": "string", "enum": ["container", "local"]},
    "workspace": {"type": "object", "additionalProperties": false, "properties": {
      "id": {"type": "string"},
      "mode": {"type": "string", "enum": ["rw", "ro"]}
    }, "required": ["id"]},
    "snapshot": {"type": "string", "description": "SnapshotVersion id or Snapshot name."},
    "resources": {"type": "array", "items": {"type": "object", "additionalProperties": false, "properties": {
      "resource": {"type": "string"},
      "version": {"type": "string"},
      "mount": {"type": "string", "description": "Single-segment mount name, default the Resource name."}
    }, "required": ["resource"]}},
    "secrets": {"type": "object", "additionalProperties": {"type": "string"}, "description": "Environment variable name to Secret name."},
    "cpu": {"type": "number"},
    "memory": {"type": "integer", "description": "Memory limit in bytes."},
    "gpu": {"type": "object", "additionalProperties": false, "properties": {
      "count": {"type": "integer", "minimum": 1},
      "model": {"type": "string"}
    }, "required": ["count"]},
    "idle_stop": {"type": "integer", "minimum": 0, "description": "Idle Stop in seconds; 0 disables it."},
    "egress": {"type": "object", "additionalProperties": false, "description": "Egress Mode, fixed for the Sandbox's life; omitted means open. A mode other than open needs a Node that enforces egress_policy.", "properties": {
      "mode": {"type": "string", "enum": ["open", "none", "allowlist"]},
      "allow": {"type": "array", "minItems": 1, "items": {"type": "string"}, "description": "allowlist only: IPv4 CIDR[:port] entries."}
    }, "required": ["mode"]}`, ""),
		OutputSchema: json.RawMessage(sandboxSchema),
		Annotations:  &sdk.ToolAnnotations{},
	}, s.sandboxCreate)

	addTool(srv, &sdk.Tool{
		Name:        "sandbox_list",
		Title:       "List Sandboxes",
		Description: "List the caller's Sandboxes, with optional filters.",
		InputSchema: objectSchema(`
    "owner": {"type": "string", "description": "admin only"},
    "state": {"type": "string"},
    "label": {"type": "string"},
    "node": {"type": "string"},
    "has_gpu": {"type": "boolean"},
    "limit": {"type": "integer", "minimum": 1, "maximum": 200},
    "cursor": {"type": "string"}`, ""),
		OutputSchema: sandboxListSchema,
		Annotations:  &sdk.ToolAnnotations{ReadOnlyHint: true},
	}, s.sandboxList)

	addTool(srv, &sdk.Tool{
		Name:        "sandbox_get",
		Title:       "Get Sandbox",
		Description: "Read one Sandbox. With wait_for, wait up to 60 seconds for that state.",
		InputSchema: objectSchema(sandboxID+`,
    "wait_for": {"type": "string", "description": "State to wait for, e.g. ready or stopped."}`, `"sandbox_id"`),
		OutputSchema: json.RawMessage(sandboxSchema),
		Annotations:  &sdk.ToolAnnotations{ReadOnlyHint: true},
	}, s.sandboxGet)

	addTool(srv, &sdk.Tool{
		Name:         "sandbox_start",
		Title:        "Start Sandbox",
		Description:  "Start a stopped Sandbox. It re-enters admission and eventually becomes ready.",
		InputSchema:  objectSchema(sandboxID, `"sandbox_id"`),
		OutputSchema: json.RawMessage(sandboxSchema),
		Annotations:  &sdk.ToolAnnotations{IdempotentHint: true, DestructiveHint: boolPtr(false)},
	}, s.sandboxStart)

	addTool(srv, &sdk.Tool{
		Name:         "sandbox_stop",
		Title:        "Stop Sandbox",
		Description:  "Stop a Sandbox. Its Workspace contents are kept.",
		InputSchema:  objectSchema(sandboxID, `"sandbox_id"`),
		OutputSchema: json.RawMessage(sandboxSchema),
		Annotations:  &sdk.ToolAnnotations{IdempotentHint: true},
	}, s.sandboxStop)

	addTool(srv, &sdk.Tool{
		Name:         "sandbox_delete",
		Title:        "Delete Sandbox",
		Description:  "Delete a Sandbox. Running Execs are canceled and an ephemeral Workspace is removed.",
		InputSchema:  objectSchema(sandboxID, `"sandbox_id"`),
		OutputSchema: json.RawMessage(sandboxSchema),
		Annotations:  &sdk.ToolAnnotations{IdempotentHint: true, DestructiveHint: boolPtr(true)},
	}, s.sandboxDelete)

	addTool(srv, &sdk.Tool{
		Name:  "exec_run",
		Title: "Run Command",
		Description: "Run a command in a ready Sandbox as argv [\"sh\",\"-c\",command] and wait for it. " +
			"timeout is the Exec's own timeout: the process is killed when it expires. wait_seconds only bounds this call (default 30, max 300); when it runs out the Exec keeps running and the result is state=running with the output so far. " +
			"Reattach with exec_output using the returned after. The exec_id is valid for the life of the Exec.",
		InputSchema: objectSchema(fmt.Sprintf(`%s,
    "command": {"type": "string", "description": "Command line executed with sh -c."},
    "cwd": {"type": "string", "description": "Working directory relative to /workspace."},
    "env": {"type": "object", "additionalProperties": {"type": "string"}},
    "stdin": {"type": "string", "description": "Written once and then closed."},
    "timeout": {"type": "integer", "minimum": 1, "description": "The Exec's own timeout in seconds; it kills the process."},
    "wait_seconds": {"type": "integer", "minimum": 0, "maximum": 300, "default": 30, "description": "How long this call waits; it does not limit the process."}`, sandboxID), `"sandbox_id", "command"`),
		OutputSchema: execSchema,
		Annotations:  &sdk.ToolAnnotations{DestructiveHint: boolPtr(true)},
	}, s.execRun)

	addTool(srv, &sdk.Tool{
		Name:  "exec_output",
		Title: "Read Exec Output",
		Description: "Read an Exec's output and state. With after, resume from that byte offset and return at most 32 KiB, with next_after for the following read; without after, return the last 32 KiB. " +
			"wait_seconds waits until new output arrives or the Exec finishes. An unknown exec_id is an error result with code not_found.",
		InputSchema: objectSchema(`
    "exec_id": {"type": "string"},
    "after": {"type": "integer", "minimum": 0, "description": "Resume offset, normally after or next_after from a previous result. Omit for the tail."},
    "wait_seconds": {"type": "integer", "minimum": 0, "maximum": 300, "description": "Wait until new output arrives or the Exec finishes."}`, `"exec_id"`),
		OutputSchema: execOutputSchema,
		Annotations:  &sdk.ToolAnnotations{ReadOnlyHint: true},
	}, s.execOutput)

	addTool(srv, &sdk.Tool{
		Name:         "exec_cancel",
		Title:        "Cancel Exec",
		Description:  "Cancel a running Exec. The process gets the grace period before it is killed.",
		InputSchema:  objectSchema(`"exec_id": {"type": "string"}`, `"exec_id"`),
		OutputSchema: execCancelSchema,
		Annotations:  &sdk.ToolAnnotations{IdempotentHint: true, DestructiveHint: boolPtr(true)},
	}, s.execCancel)

	addTool(srv, &sdk.Tool{
		Name:         "file_list",
		Title:        "List Files",
		Description:  "List a directory in the Sandbox Workspace.",
		InputSchema:  objectSchema(sandboxID+","+pathProp, `"sandbox_id", "path"`),
		OutputSchema: fileListSchema,
		Annotations:  &sdk.ToolAnnotations{ReadOnlyHint: true},
	}, s.fileList)

	addTool(srv, &sdk.Tool{
		Name:         "file_stat",
		Title:        "Stat File",
		Description:  "Read a Workspace file's metadata.",
		InputSchema:  objectSchema(sandboxID+","+pathProp, `"sandbox_id", "path"`),
		OutputSchema: fileEntrySchema,
		Annotations:  &sdk.ToolAnnotations{ReadOnlyHint: true},
	}, s.fileStat)

	addTool(srv, &sdk.Tool{
		Name:  "file_read",
		Title: "Read File",
		Description: "Read up to 1 MiB of a Workspace file. UTF-8 content is returned as text, an image/* file as image content, " +
			"and any other binary content as content_base64 with encoding base64. Use Exec with tar for archives and larger files.",
		InputSchema: objectSchema(sandboxID+","+pathProp+`,
    "offset": {"type": "integer", "minimum": 0, "default": 0},
    "limit": {"type": "integer", "minimum": 1, "maximum": 1048576, "default": 1048576}`, `"sandbox_id", "path"`),
		OutputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {"type": "string"},
    "offset": {"type": "integer", "minimum": 0},
    "size": {"type": "integer", "minimum": 0},
    "content_type": {"type": "string"},
    "encoding": {"type": "string", "enum": ["text", "base64", "image"]},
    "content": {"type": "string"},
    "content_base64": {"type": "string"}
  },
  "required": ["path", "offset", "size", "encoding"]
}`),
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
	}, s.fileRead)

	addTool(srv, &sdk.Tool{
		Name:  "file_write",
		Title: "Write File",
		Description: "Write a Workspace file, at most 1 MiB. encoding text (the default) writes content as UTF-8; " +
			"encoding base64 decodes content first.",
		InputSchema: objectSchema(sandboxID+","+pathProp+`,
    "content": {"type": "string"},
    "encoding": {"type": "string", "enum": ["text", "base64"], "default": "text"}`, `"sandbox_id", "path", "content"`),
		OutputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {"type": "string"},
    "size": {"type": "integer"}
  },
  "required": ["path", "size"]
}`),
		Annotations: &sdk.ToolAnnotations{IdempotentHint: true, DestructiveHint: boolPtr(true)},
	}, s.fileWrite)

	addTool(srv, &sdk.Tool{
		Name:         "file_mkdir",
		Title:        "Make Directory",
		Description:  "Create a directory in the Workspace.",
		InputSchema:  objectSchema(sandboxID+","+pathProp, `"sandbox_id", "path"`),
		OutputSchema: json.RawMessage(`{"type": "object", "properties": {"ok": {"type": "boolean"}}, "required": ["ok"]}`),
		Annotations:  &sdk.ToolAnnotations{IdempotentHint: true, DestructiveHint: boolPtr(false)},
	}, s.fileMkdir)

	addTool(srv, &sdk.Tool{
		Name:         "file_move",
		Title:        "Move File",
		Description:  "Move or rename a Workspace path.",
		InputSchema:  objectSchema(sandboxID+`,"from": {"type": "string"}, "to": {"type": "string"}`, `"sandbox_id", "from", "to"`),
		OutputSchema: json.RawMessage(`{"type": "object", "properties": {"ok": {"type": "boolean"}}, "required": ["ok"]}`),
		Annotations:  &sdk.ToolAnnotations{DestructiveHint: boolPtr(true)},
	}, s.fileMove)

	addTool(srv, &sdk.Tool{
		Name:         "file_delete",
		Title:        "Delete File",
		Description:  "Delete a Workspace path.",
		InputSchema:  objectSchema(sandboxID+","+pathProp, `"sandbox_id", "path"`),
		OutputSchema: json.RawMessage(`{"type": "object", "properties": {"ok": {"type": "boolean"}}, "required": ["ok"]}`),
		Annotations:  &sdk.ToolAnnotations{IdempotentHint: true, DestructiveHint: boolPtr(true)},
	}, s.fileDelete)

	addTool(srv, &sdk.Tool{
		Name:  "port_url",
		Title: "Get Port URL",
		Description: "Get a URL for a port inside the Sandbox, for a human to open in a browser. " +
			"The ticket in the URL is valid for 1 minute.",
		InputSchema: objectSchema(sandboxID+`,
    "port": {"type": "integer", "minimum": 1, "maximum": 65535}`, `"sandbox_id", "port"`),
		OutputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "url": {"type": "string"}
  },
  "required": ["url"]
}`),
		Annotations: &sdk.ToolAnnotations{DestructiveHint: boolPtr(false)},
	}, s.portURL)
}
