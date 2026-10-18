package mcpserver_test

import (
	"context"
	"encoding/json"
	"regexp"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zibyn/stars-sandbox/client"
	"github.com/zibyn/stars-sandbox/internal/mcpserver"
)

var toolName = regexp.MustCompile(`^[a-z_]+$`)

// connect starts the MCP server in-process and returns a connected client
// session. The seam is the MCP protocol, not the handler internals.
func connect(t *testing.T, api *client.Client) *sdk.ClientSession {
	t.Helper()
	ctx := context.Background()
	serverTransport, clientTransport := sdk.NewInMemoryTransports()
	srv := mcpserver.New(api, nil)
	if _, err := srv.Connect(ctx, serverTransport, nil); err != nil {
		t.Fatalf("connect server: %v", err)
	}
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "0.0.0"}, nil).
		Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("connect client: %v", err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func TestToolListIsFixedAndComplete(t *testing.T) {
	cs := connect(t, client.New("http://127.0.0.1:1", "test"))

	// The SDK returns tools sorted by name, which is what makes the order
	// stable; the registration order is fixed too.
	want := []string{
		"exec_cancel", "exec_output", "exec_run", "file_delete", "file_list",
		"file_mkdir", "file_move", "file_read", "file_stat", "file_write",
		"port_url", "sandbox_create", "sandbox_delete", "sandbox_get",
		"sandbox_list", "sandbox_start", "sandbox_stop",
	}
	result, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(result.Tools) != len(want) {
		t.Fatalf("got %d tools, want %d", len(result.Tools), len(want))
	}
	for i, tool := range result.Tools {
		if tool.Name != want[i] {
			t.Fatalf("tool %d = %q, want %q", i, tool.Name, want[i])
		}
		if !toolName.MatchString(tool.Name) {
			t.Errorf("tool name %q is not [a-z_]+", tool.Name)
		}
		if tool.Title == "" {
			t.Errorf("%s has no title", tool.Name)
		}
		if tool.Description == "" {
			t.Errorf("%s has no description", tool.Name)
		}
		if tool.OutputSchema == nil {
			t.Errorf("%s has no outputSchema", tool.Name)
		}
		schema, ok := tool.InputSchema.(map[string]any)
		if !ok || schema["type"] != "object" {
			t.Errorf("%s inputSchema is not an object schema: %#v", tool.Name, tool.InputSchema)
		}
	}
	if result.Cacheable.GetTTLMs() <= 0 {
		t.Errorf("tools/list is not cacheable: %+v", result.Cacheable)
	}

	// A second independent session sees the same fixed order.
	second, err := connect(t, client.New("http://127.0.0.1:1", "test")).ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	for i := range second.Tools {
		if second.Tools[i].Name != result.Tools[i].Name {
			t.Fatalf("second session order differs at %d: %q != %q", i, second.Tools[i].Name, result.Tools[i].Name)
		}
	}

	// Every declared schema must resolve as JSON Schema 2020-12.
	for _, tool := range result.Tools {
		for label, raw := range map[string]any{"input": tool.InputSchema, "output": tool.OutputSchema} {
			data, err := json.Marshal(raw)
			if err != nil {
				t.Fatalf("%s %s schema: %v", tool.Name, label, err)
			}
			var schema jsonschema.Schema
			if err := json.Unmarshal(data, &schema); err != nil {
				t.Fatalf("%s %s schema: %v", tool.Name, label, err)
			}
			if _, err := schema.Resolve(nil); err != nil {
				t.Fatalf("%s %s schema does not resolve: %v", tool.Name, label, err)
			}
		}
	}
}

func TestCapabilitiesDeclareOnlyTools(t *testing.T) {
	cs := connect(t, client.New("http://127.0.0.1:1", "test"))
	init := cs.InitializeResult()
	if init == nil {
		t.Fatal("no initialize result")
	}
	if init.Instructions == "" {
		t.Error("server has no instructions")
	}
	caps := init.Capabilities
	if caps.Tools == nil {
		t.Error("tools capability is missing")
	}
	if caps.Logging != nil || caps.Resources != nil || caps.Prompts != nil ||
		caps.Completions != nil || len(caps.Extensions) != 0 {
		t.Errorf("unexpected capabilities: %+v", caps)
	}
	if init.ProtocolVersion != "2026-07-28" {
		t.Errorf("protocol version = %q, want 2026-07-28", init.ProtocolVersion)
	}
}

// TestStructuredResultMatchesSchema checks the text/structured pairing every
// successful tool call must have.
func TestStructuredResultMatchesSchema(t *testing.T) {
	api := fakeAPI(t)
	cs := connect(t, client.New(api.URL, "test"))
	result, err := cs.CallTool(context.Background(), &sdk.CallToolParams{
		Name:      "sandbox_list",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if result.IsError {
		t.Fatalf("sandbox_list failed: %+v", result.Content)
	}
	text := contentText(t, result)
	structured, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	var want, got any
	if err := json.Unmarshal([]byte(text), &want); err != nil {
		t.Fatalf("text content is not JSON: %v", err)
	}
	if err := json.Unmarshal(structured, &got); err != nil {
		t.Fatalf("structured content is not JSON: %v", err)
	}
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("text content %s != structured content %s", wantJSON, gotJSON)
	}
}

func contentText(t *testing.T, result *sdk.CallToolResult) string {
	t.Helper()
	for _, c := range result.Content {
		if text, ok := c.(*sdk.TextContent); ok {
			return text.Text
		}
	}
	t.Fatalf("result has no text content: %+v", result.Content)
	return ""
}
