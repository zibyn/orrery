//go:build e2e

package e2e

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

var mcpToolName = regexp.MustCompile(`^[a-z_]+$`)

// newMCPSession launches the real `orrery mcp` over stdio. Each call is
// a fresh process, so two calls also check that tools/list order is stable
// across sessions (the client caches it within one session).
func newMCPSession(t *testing.T, token string) *sdk.ClientSession {
	t.Helper()
	cmd := exec.Command(cliPath(t), "mcp")
	cmd.Env = append(os.Environ(),
		"ORRERY_SANDBOX_API_URL="+apiURL(),
		"ORRERY_SANDBOX_TOKEN="+token,
	)
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "e2e", Version: "0.0.0"}, nil).
		Connect(context.Background(), &sdk.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("connect to orrery mcp: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func TestMCPToolListStable(t *testing.T) {
	admin := adminToken(t)
	firstSession := newMCPSession(t, admin)
	due := firstSession.InitializeResult()
	if due == nil {
		t.Fatal("no initialize result")
	}
	if due.Instructions == "" {
		t.Error("server sends no instructions")
	}
	if due.Capabilities.Tools == nil {
		t.Error("tools capability is missing")
	}
	if due.Capabilities.Logging != nil {
		t.Error("server must not declare the logging capability")
	}

	first, err := firstSession.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	if len(first.Tools) != 17 {
		t.Fatalf("got %d tools, want 17", len(first.Tools))
	}
	for _, tool := range first.Tools {
		if !mcpToolName.MatchString(tool.Name) {
			t.Errorf("tool name %q does not match [a-z_]+", tool.Name)
		}
		if tool.Title == "" || tool.OutputSchema == nil {
			t.Errorf("%s is missing its title or outputSchema", tool.Name)
		}
	}

	second, err := newMCPSession(t, admin).ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("second tools/list: %v", err)
	}
	if len(second.Tools) != len(first.Tools) {
		t.Fatalf("second session has %d tools, first has %d", len(second.Tools), len(first.Tools))
	}
	for i := range first.Tools {
		if first.Tools[i].Name != second.Tools[i].Name {
			t.Fatalf("tool %d: %q != %q across sessions", i, first.Tools[i].Name, second.Tools[i].Name)
		}
	}
}

func TestMCPTools(t *testing.T) {
	admin := adminToken(t)
	raiseSandboxQuota(t, admin, 10)

	suffix := time.Now().UnixNano()
	nodeName := fmt.Sprintf("e2e-mcp-node-%d", suffix)
	state, dataRoot := enrollAgent(t, admin, nodeName)
	startAgent(t, state, dataRoot)
	waitForNodeReady(t, admin, nodeName)

	cs := newMCPSession(t, admin)
	schemas := mcpSchemas(t, cs)
	call := func(name string, args map[string]any) *sdk.CallToolResult {
		t.Helper()
		result, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s: JSON-RPC error: %v", name, err)
		}
		if !result.IsError {
			if err := schemas[name].Validate(result.StructuredContent); err != nil {
				t.Fatalf("%s: structuredContent does not validate: %v", name, err)
			}
		}
		return result
	}

	// A missing Sandbox is a tool error with a code and no structuredContent,
	// not a JSON-RPC error.
	missing := call("sandbox_get", map[string]any{"sandbox_id": "d3b07384-d9a1-4c0f-8a3e-3f7e6e1a2b3c"})
	if !missing.IsError {
		t.Fatalf("missing Sandbox was not an error result: %#v", missing.Content)
	}
	if missing.StructuredContent != nil {
		t.Fatalf("error result carries structuredContent: %#v", missing.StructuredContent)
	}
	problem := mcpErrorBody(t, missing)
	if problem.Code != "not_found" || problem.Detail == "" {
		t.Fatalf("missing Sandbox error = %#v", problem)
	}

	// Create, run a command longer than wait_seconds, reattach, read files,
	// then delete.
	created := mcpStructured(t, call("sandbox_create", map[string]any{
		"name": fmt.Sprintf("mcp-%d", suffix), "cpu": 0.5, "memory": 128 << 20, "idle_stop": 0,
	}))
	sandboxID, _ := created["id"].(string)
	if sandboxID == "" || created["state"] != "ready" {
		t.Fatalf("created Sandbox = %#v", created)
	}
	t.Cleanup(func() { _, _, _ = postJSON(http.MethodDelete, "/api/v1/sandboxes/"+sandboxID, admin, nil) })

	const command = `printf first; sleep 3; printf second`
	run := mcpStructured(t, call("exec_run", map[string]any{
		"sandbox_id": sandboxID, "command": command, "timeout": 30, "wait_seconds": 1,
	}))
	if run["state"] != "running" {
		t.Fatalf("exec_run state = %v, want running", run["state"])
	}
	execID, _ := run["exec_id"].(string)
	if execID == "" {
		t.Fatalf("exec_run = %#v", run)
	}
	output := mcpFrames(t, run)
	after, _ := run["after"].(float64)
	deadline := time.Now().Add(30 * time.Second)
	for run["state"] != "finished" {
		if time.Now().After(deadline) {
			t.Fatalf("Exec %s did not finish", execID)
		}
		run = mcpStructured(t, call("exec_output", map[string]any{
			"exec_id": execID, "after": after, "wait_seconds": 10,
		}))
		output = append(output, mcpFrames(t, run)...)
		after, _ = run["next_after"].(float64)
	}
	if got := string(output); got != "firstsecond" {
		t.Fatalf("reattached output = %q, want %q (no gaps or duplicates)", got, "firstsecond")
	}
	result, _ := run["result"].(map[string]any)
	if result["cause"] != "exited" || result["exit_code"] != float64(0) {
		t.Fatalf("result = %#v", result)
	}
	if run["log_truncated"] != false {
		t.Fatalf("log_truncated = %v", run["log_truncated"])
	}

	// Text file round trip.
	call("file_write", map[string]any{
		"sandbox_id": sandboxID, "path": "mcp.txt", "content": "hello mcp\n",
	})
	text := mcpStructured(t, call("file_read", map[string]any{
		"sandbox_id": sandboxID, "path": "mcp.txt",
	}))
	if text["encoding"] != "text" || text["content"] != "hello mcp\n" {
		t.Fatalf("text read = %#v", text)
	}

	// Binary file round trip: invalid UTF-8 forces content_base64.
	binary := []byte{0xff, 0x00, 0x01, 0x80}
	call("file_write", map[string]any{
		"sandbox_id": sandboxID, "path": "mcp.bin",
		"content": base64.StdEncoding.EncodeToString(binary), "encoding": "base64",
	})
	blob := mcpStructured(t, call("file_read", map[string]any{
		"sandbox_id": sandboxID, "path": "mcp.bin",
	}))
	if blob["encoding"] != "base64" {
		t.Fatalf("binary read encoding = %v", blob["encoding"])
	}
	decoded, err := base64.StdEncoding.DecodeString(blob["content_base64"].(string))
	if err != nil || string(decoded) != string(binary) {
		t.Fatalf("binary read = %x, want %x", decoded, binary)
	}

	deleted := mcpStructured(t, call("sandbox_delete", map[string]any{"sandbox_id": sandboxID}))
	if state, _ := deleted["state"].(string); state != "deleting" && state != "deleted" {
		t.Fatalf("deleted Sandbox state = %v", deleted["state"])
	}
}

// mcpSchemas resolves every tool's output schema for result validation.
func mcpSchemas(t *testing.T, cs *sdk.ClientSession) map[string]*jsonschema.Resolved {
	t.Helper()
	list, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	out := make(map[string]*jsonschema.Resolved, len(list.Tools))
	for _, tool := range list.Tools {
		raw, err := json.Marshal(tool.OutputSchema)
		if err != nil {
			t.Fatalf("%s output schema: %v", tool.Name, err)
		}
		var schema jsonschema.Schema
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatalf("%s output schema: %v", tool.Name, err)
		}
		resolved, err := schema.Resolve(nil)
		if err != nil {
			t.Fatalf("%s output schema: %v", tool.Name, err)
		}
		out[tool.Name] = resolved
	}
	return out
}

func mcpStructured(t *testing.T, result *sdk.CallToolResult) map[string]any {
	t.Helper()
	if result.IsError {
		t.Fatalf("tool error: %s", mcpText(t, result))
	}
	body, ok := result.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("structuredContent = %#v", result.StructuredContent)
	}
	return body
}

func mcpText(t *testing.T, result *sdk.CallToolResult) string {
	t.Helper()
	for _, content := range result.Content {
		if text, ok := content.(*sdk.TextContent); ok {
			return text.Text
		}
	}
	return ""
}

type mcpError struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

func mcpErrorBody(t *testing.T, result *sdk.CallToolResult) mcpError {
	t.Helper()
	if !result.IsError {
		t.Fatalf("result is not an error: %#v", result.Content)
	}
	if result.StructuredContent != nil {
		t.Fatalf("error result carries structuredContent: %#v", result.StructuredContent)
	}
	var body mcpError
	if err := json.Unmarshal([]byte(mcpText(t, result)), &body); err != nil {
		t.Fatalf("error result is not JSON: %v", err)
	}
	return body
}

// mcpFrames concatenates stdout and stderr frames in order.
func mcpFrames(t *testing.T, body map[string]any) []byte {
	t.Helper()
	raw, _ := body["output"].([]any)
	var out []byte
	for _, item := range raw {
		frame, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("frame = %#v", item)
		}
		data, _ := frame["data"].(string)
		if frame["encoding"] == "base64" {
			decoded, err := base64.StdEncoding.DecodeString(data)
			if err != nil {
				t.Fatalf("base64 frame: %v", err)
			}
			data = string(decoded)
		}
		out = append(out, data...)
	}
	return out
}
