package mcpserver_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// callTool invokes a tool and validates a successful result's
// structuredContent against that tool's outputSchema.
func callTool(t *testing.T, cs *sdk.ClientSession, name string, args map[string]any) *sdk.CallToolResult {
	t.Helper()
	result, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: JSON-RPC error: %v", name, err)
	}
	if !result.IsError {
		validateOutput(t, cs, name, result.StructuredContent)
	}
	return result
}

func validateOutput(t *testing.T, cs *sdk.ClientSession, name string, value any) {
	t.Helper()
	list, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	for _, tool := range list.Tools {
		if tool.Name != name {
			continue
		}
		raw, err := json.Marshal(tool.OutputSchema)
		if err != nil {
			t.Fatalf("%s output schema: %v", name, err)
		}
		var schema jsonschema.Schema
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatalf("%s output schema: %v", name, err)
		}
		resolved, err := schema.Resolve(nil)
		if err != nil {
			t.Fatalf("%s output schema: %v", name, err)
		}
		if err := resolved.Validate(value); err != nil {
			t.Fatalf("%s structuredContent does not validate: %v\nvalue: %#v", name, err, value)
		}
		return
	}
	t.Fatalf("tool %s not found", name)
}

func errorCode(t *testing.T, result *sdk.CallToolResult) string {
	t.Helper()
	if !result.IsError {
		t.Fatalf("result is not an error: %#v", result.Content)
	}
	var body struct {
		Code   string `json:"code"`
		Detail string `json:"detail"`
	}
	mustTextJSON(t, result, &body)
	return body.Code
}

func mustTextJSON(t *testing.T, result *sdk.CallToolResult, v any) {
	t.Helper()
	var text string
	for _, c := range result.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			text = tc.Text
			break
		}
	}
	if text == "" {
		t.Fatalf("result has no text content: %#v", result.Content)
	}
	if err := json.Unmarshal([]byte(text), v); err != nil {
		t.Fatalf("text content is not JSON: %v (%s)", err, text)
	}
}

func structured(t *testing.T, result *sdk.CallToolResult) map[string]any {
	t.Helper()
	body, ok := result.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("structuredContent = %#v", result.StructuredContent)
	}
	return body
}

func TestRESTProblemBecomesToolError(t *testing.T) {
	api := fakeAPI(t)
	api.handle("GET", "/api/v1/sandboxes/missing", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]any{"code": "not_found", "detail": "no such Sandbox"})
	})
	cs := connect(t, clientAPI(api))
	result := callTool(t, cs, "sandbox_get", map[string]any{"sandbox_id": "missing"})
	if code := errorCode(t, result); code != "not_found" {
		t.Fatalf("code = %q, want not_found", code)
	}
	if _, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "sandbox_get", Arguments: map[string]any{"sandbox_id": "missing"}}); err != nil {
		t.Fatalf("unknown Sandbox must not be a JSON-RPC error: %v", err)
	}
}

func TestInvalidArgumentsAreToolErrors(t *testing.T) {
	cs := connect(t, clientAPI(fakeAPI(t)))
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"exec_run", map[string]any{"sandbox_id": "sb", "command": "", "wait_seconds": 5}},
		{"exec_run", map[string]any{"sandbox_id": "sb", "command": "x", "wait_seconds": 301}},
		{"exec_output", map[string]any{"exec_id": "", "after": 0}},
		{"file_write", map[string]any{"sandbox_id": "sb", "path": "x", "content": "%%%", "encoding": "base64"}},
		{"port_url", map[string]any{"sandbox_id": "sb", "port": 0}},
		{"sandbox_get", map[string]any{"sandbox_id": ""}},
		{"exec_run", map[string]any{"sandbox_id": "sb", "command": "x", "unknown_field": 1}},
	} {
		result := callTool(t, cs, tc.name, tc.args)
		if code := errorCode(t, result); code != "invalid_request" {
			t.Errorf("%s %v: code = %q, want invalid_request", tc.name, tc.args, code)
		}
	}
}

func TestSandboxListPassesFilters(t *testing.T) {
	api := fakeAPI(t)
	api.handle("GET", "/api/v1/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("state") != "ready" || q.Get("limit") != "5" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"code": "invalid_request", "detail": "bad filters"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"items": []any{map[string]any{
				"id": "sb-1", "name": "one", "runtime": "container", "state": "ready",
				"labels": map[string]string{}, "snapshot": "default", "cpu": 0.5,
				"memory": 1024, "idle_stop": 0, "conditions": []string{}, "created_at": "2026-01-01T00:00:00Z",
			}},
			"next_cursor": nil,
		})
	})
	cs := connect(t, clientAPI(api))
	result := callTool(t, cs, "sandbox_list", map[string]any{"state": "ready", "limit": 5})
	body := structured(t, result)
	items, _ := body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items = %#v", body["items"])
	}
}

func TestSandboxCreateWaitsForReady(t *testing.T) {
	api := fakeAPI(t)
	var calls int
	var mu sync.Mutex
	api.handle("POST", "/api/v1/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["name"] != "one" || body["memory"] != float64(1<<20) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"code": "invalid_request", "detail": "bad body"})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"id": "sb-1", "name": "one", "runtime": "container", "state": "pending"})
	})
	api.handle("GET", "/api/v1/sandboxes/sb-1", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"id": "sb-1", "name": "one", "runtime": "container", "state": "ready"})
	})
	cs := connect(t, clientAPI(api))
	result := callTool(t, cs, "sandbox_create", map[string]any{"name": "one", "memory": 1 << 20})
	if state := structured(t, result)["state"]; state != "ready" {
		t.Fatalf("state = %v, want ready", state)
	}
	if calls == 0 {
		t.Fatal("sandbox_create did not wait for ready")
	}
}

func TestExecRunReturnsFinishedOutput(t *testing.T) {
	api := fakeAPI(t)
	frames := []map[string]any{
		{"seq": 0, "stream": "stdout", "data": "hello"},
		{"seq": 10, "stream": "stderr", "data": "oops"},
		{"seq": 19, "stream": "stdout", "data": "!"},
	}
	api.handle("POST", "/api/v1/sandboxes/sb-1/execs", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Argv []string `json:"argv"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if len(body.Argv) != 3 || body.Argv[0] != "sh" || body.Argv[1] != "-c" || body.Argv[2] != "echo hi" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"code": "invalid_request", "detail": "argv"})
			return
		}
		writeJSON(w, http.StatusCreated, execJSON("exec-1", "running", nil))
	})
	api.handle("GET", "/api/v1/execs/exec-1/output", frameHandler(frames))
	api.handle("GET", "/api/v1/execs/exec-1", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, execJSON("exec-1", "finished", map[string]any{"cause": "exited", "exit_code": 0}))
	})
	cs := connect(t, clientAPI(api))
	result := callTool(t, cs, "exec_run", map[string]any{
		"sandbox_id": "sb-1", "command": "echo hi", "wait_seconds": 10,
	})
	body := structured(t, result)
	if body["state"] != "finished" || body["after"] != float64(25) {
		t.Fatalf("body = %#v", body)
	}
	got := framesOf(t, body)
	if want := "stdout:hello|stderr:oops|stdout:!"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
	var resultBody map[string]any
	mustTextJSON(t, result, &resultBody)
	if resultBody["state"] != "finished" {
		t.Fatalf("text content disagrees: %#v", resultBody)
	}
}

func TestExecRunKeepsTailOfLongOutput(t *testing.T) {
	api := fakeAPI(t)
	var frames []map[string]any
	seq := int64(0)
	for i := 0; i < 40; i++ {
		data := strings.Repeat(string(rune('a'+i%26)), 1024)
		frames = append(frames, map[string]any{"seq": seq, "stream": "stdout", "data": data})
		seq += 5 + int64(len(data))
	}
	total := seq
	api.handle("POST", "/api/v1/sandboxes/sb-1/execs", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusCreated, execJSON("exec-1", "running", nil))
	})
	api.handle("GET", "/api/v1/execs/exec-1/output", frameHandler(frames))
	api.handle("GET", "/api/v1/execs/exec-1", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, execJSON("exec-1", "finished", map[string]any{"cause": "exited", "exit_code": 0}))
	})
	cs := connect(t, clientAPI(api))
	body := structured(t, callTool(t, cs, "exec_run", map[string]any{
		"sandbox_id": "sb-1", "command": "yes", "wait_seconds": 10,
	}))
	size := 0
	for _, f := range body["output"].([]any) {
		size += len(f.(map[string]any)["data"].(string))
	}
	if size > 32<<10 {
		t.Fatalf("returned %d bytes, want at most 32 KiB", size)
	}
	if body["after"] != float64(total) {
		t.Fatalf("after = %v, want %d", body["after"], total)
	}
}

func TestExecRunRunningThenReattach(t *testing.T) {
	api := fakeAPI(t)
	var mu sync.Mutex
	first := []map[string]any{{"seq": 0, "stream": "stdout", "data": "one"}}
	all := []map[string]any{
		{"seq": 0, "stream": "stdout", "data": "one"},
		{"seq": 8, "stream": "stdout", "data": "two"},
	}
	finished := false
	api.handle("POST", "/api/v1/sandboxes/sb-1/execs", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusCreated, execJSON("exec-1", "running", nil))
	})
	api.handle("GET", "/api/v1/execs/exec-1/output", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		page := first
		if finished {
			page = all
		}
		writeFrames(w, r, page)
	})
	api.handle("GET", "/api/v1/execs/exec-1", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		state := "running"
		var result any
		if finished {
			state = "finished"
			result = map[string]any{"cause": "exited", "exit_code": 0}
		}
		writeJSON(w, http.StatusOK, execJSON("exec-1", state, result))
	})
	cs := connect(t, clientAPI(api))

	run := structured(t, callTool(t, cs, "exec_run", map[string]any{
		"sandbox_id": "sb-1", "command": "slow", "wait_seconds": 0,
	}))
	if run["state"] != "running" {
		t.Fatalf("state = %v, want running", run["state"])
	}
	if got := framesOf(t, run); got != "stdout:one" {
		t.Fatalf("first output = %q", got)
	}
	after := run["after"].(float64)

	mu.Lock()
	finished = true
	mu.Unlock()
	out := structured(t, callTool(t, cs, "exec_output", map[string]any{
		"exec_id": "exec-1", "after": after, "wait_seconds": 5,
	}))
	if got := framesOf(t, out); got != "stdout:two" {
		t.Fatalf("reattached output = %q, want stdout:two (no gaps, no duplicates)", got)
	}
	if out["next_after"] != float64(16) {
		t.Fatalf("next_after = %v, want 16", out["next_after"])
	}
	if out["state"] != "finished" {
		t.Fatalf("state = %v, want finished", out["state"])
	}
}

func TestExecOutputTailWithoutAfter(t *testing.T) {
	api := fakeAPI(t)
	frames := []map[string]any{
		{"seq": 0, "stream": "stdout", "data": "one"},
		{"seq": 8, "stream": "stdout", "data": "two"},
	}
	api.handle("GET", "/api/v1/execs/exec-1/output", frameHandler(frames))
	api.handle("GET", "/api/v1/execs/exec-1", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, execJSON("exec-1", "finished", map[string]any{"cause": "exited", "exit_code": 3}))
	})
	cs := connect(t, clientAPI(api))
	body := structured(t, callTool(t, cs, "exec_output", map[string]any{"exec_id": "exec-1"}))
	if got := framesOf(t, body); got != "stdout:one|stdout:two" {
		t.Fatalf("tail = %q, want stdout:one|stdout:two", got)
	}
	if body["next_after"] != float64(16) {
		t.Fatalf("next_after = %v, want 16", body["next_after"])
	}
	if body["state"] != "finished" {
		t.Fatalf("state = %v", body["state"])
	}
	result, _ := body["result"].(map[string]any)
	if result["exit_code"] != float64(3) {
		t.Fatalf("exit_code = %v", result["exit_code"])
	}
}

func TestExecOutputUnknownIDIsNotFound(t *testing.T) {
	api := fakeAPI(t)
	api.handle("GET", "/api/v1/execs/nope/output", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]any{"code": "not_found", "detail": "no such Exec"})
	})
	cs := connect(t, clientAPI(api))
	result := callTool(t, cs, "exec_output", map[string]any{"exec_id": "nope"})
	if code := errorCode(t, result); code != "not_found" {
		t.Fatalf("code = %q, want not_found", code)
	}
}

func TestExecRunSendsProgress(t *testing.T) {
	api := fakeAPI(t)
	var mu sync.Mutex
	polls := 0
	api.handle("POST", "/api/v1/sandboxes/sb-1/execs", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusCreated, execJSON("exec-1", "running", nil))
	})
	api.handle("GET", "/api/v1/execs/exec-1/output", frameHandler([]map[string]any{
		{"seq": 0, "stream": "stdout", "data": "abc"},
	}))
	api.handle("GET", "/api/v1/execs/exec-1", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		polls++
		state := "running"
		if polls >= 3 {
			state = "finished"
		}
		mu.Unlock()
		writeJSON(w, http.StatusOK, execJSON("exec-1", state, map[string]any{"cause": "exited", "exit_code": 0}))
	})

	var notifications []*sdk.ProgressNotificationParams
	var muN sync.Mutex
	serverTransport, clientTransport := sdk.NewInMemoryTransports()
	if _, err := newServerFor(api).Connect(context.Background(), serverTransport, nil); err != nil {
		t.Fatalf("connect server: %v", err)
	}
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "0"}, &sdk.ClientOptions{
		ProgressNotificationHandler: func(_ context.Context, req *sdk.ProgressNotificationClientRequest) {
			muN.Lock()
			notifications = append(notifications, req.Params)
			muN.Unlock()
		},
	}).Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatalf("connect client: %v", err)
	}
	t.Cleanup(func() { cs.Close() })

	params := &sdk.CallToolParams{
		Name:      "exec_run",
		Arguments: map[string]any{"sandbox_id": "sb-1", "command": "x", "wait_seconds": 10},
	}
	params.SetProgressToken("p1")
	result, err := cs.CallTool(context.Background(), params)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if result.IsError {
		t.Fatalf("exec_run failed: %#v", result.Content)
	}
	muN.Lock()
	defer muN.Unlock()
	if len(notifications) == 0 {
		t.Fatal("no progress notifications")
	}
	if notifications[len(notifications)-1].Progress < 3 {
		t.Fatalf("progress = %v, want at least the output byte count", notifications[len(notifications)-1].Progress)
	}
}

// Helpers for the fake Exec API.

func execJSON(id, state string, result any) map[string]any {
	return map[string]any{
		"id": id, "sandbox_id": "sb-1", "argv": []string{"sh", "-c", "x"},
		"cwd": "", "state": state, "result": result, "log_truncated": false,
		"accepted_at": "2026-01-01T00:00:00Z",
	}
}

// frameHandler serves frames at or after the requested byte offset.
func frameHandler(frames []map[string]any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeFrames(w, r, frames)
	}
}

func writeFrames(w http.ResponseWriter, r *http.Request, frames []map[string]any) {
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	out := []map[string]any{}
	for _, f := range frames {
		if seqOf(f["seq"]) >= after {
			out = append(out, f)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func seqOf(v any) int64 {
	switch n := v.(type) {
	case int:
		return int64(n)
	case int64:
		return n
	case float64:
		return int64(n)
	}
	return 0
}

// framesOf renders the output frames as stream:data, in order.
func framesOf(t *testing.T, body map[string]any) string {
	t.Helper()
	var b strings.Builder
	for _, raw := range body["output"].([]any) {
		f := raw.(map[string]any)
		data := f["data"].(string)
		if f["encoding"] == "base64" {
			decoded, err := base64.StdEncoding.DecodeString(data)
			if err != nil {
				t.Fatalf("bad base64 frame: %v", err)
			}
			data = string(decoded)
		}
		fmt.Fprintf(&b, "%s:%s|", f["stream"], data)
	}
	return strings.TrimSuffix(b.String(), "|")
}
