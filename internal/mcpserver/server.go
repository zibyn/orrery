// Package mcpserver serves the MCP Tool API (api.md §7, ADR-0022) over the
// public Go SDK client.
package mcpserver

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zibyn/stars-sandbox/client"
)

const (
	// outputLimit is the most Execution Log bytes a tool result carries.
	outputLimit = 32 << 10
	// fileLimit is the most file bytes file_read and file_write move.
	fileLimit = 1 << 20
	// waitLimit bounds wait_seconds.
	waitLimit = 300
	// sandboxWait is how long sandbox_create and sandbox_get wait for a state.
	sandboxWait = 60 * time.Second
	// pollInterval is how often a tool polls the REST API while waiting.
	pollInterval = 200 * time.Millisecond
)

// server holds the shared state of every tool handler.
type server struct {
	c *client.Client
}

// New builds the MCP server. Every tool is registered in a fixed order so
// tools/list is stable, and the server declares only the tools capability.
func New(c *client.Client, logger *slog.Logger) *sdk.Server {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	s := &server{c: c}
	srv := sdk.NewServer(
		&sdk.Implementation{Name: "stars-sandbox", Title: "stars-sandbox", Version: "0.1.0"},
		&sdk.ServerOptions{
			Instructions: "Manage Sandboxes and run commands in them. The usual flow is " +
				"sandbox_create, exec_run, exec_output when a command is still running, and " +
				"sandbox_delete. Commands run as sh -c and may use the network.",
			Logger: logger,
			// Only tools: a non-nil Capabilities drops the SDK's default
			// logging capability.
			Capabilities: &sdk.ServerCapabilities{Tools: &sdk.ToolCapabilities{}},
			SetCacheable: func(_ context.Context, req sdk.Request, c *sdk.Cacheable) {
				// tools/list cannot change during a session; the order is fixed.
				if _, ok := req.GetParams().(*sdk.ListToolsParams); ok {
					c.TTLMs = 60_000
					c.CacheScope = "public"
				}
			},
		})
	s.register(srv)
	return srv
}

// addTool decodes and validates the arguments before calling h. Validation
// failures become isError results; they never become JSON-RPC errors.
func addTool[In any](srv *sdk.Server, tool *sdk.Tool, h func(context.Context, *sdk.CallToolRequest, In) (*sdk.CallToolResult, error)) {
	srv.AddTool(tool, func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		in, err := decode[In](req.Params.Arguments)
		if err != nil {
			return errorResult("invalid_request", err.Error()), nil
		}
		return h(ctx, req, in)
	})
}

func decode[In any](raw json.RawMessage) (In, error) {
	var in In
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return in, fmt.Errorf("invalid arguments: %w", err)
	}
	if dec.More() {
		return in, errors.New("invalid arguments: trailing data")
	}
	if v, ok := any(&in).(interface{ validate() error }); ok {
		if err := v.validate(); err != nil {
			return in, err
		}
	}
	return in, nil
}

// okResult pairs structured content with the same JSON as text, as api.md §7
// requires. The SDK does not validate output schemas on the low-level API, so
// the caller keeps the two in lockstep here.
func okResult(v any) (*sdk.CallToolResult, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshal tool result: %w", err)
	}
	return &sdk.CallToolResult{
		Content:           []sdk.Content{&sdk.TextContent{Text: string(data)}},
		StructuredContent: json.RawMessage(data),
	}, nil
}

// errorResult reports a business error the agent can read and correct.
func errorResult(code, detail string) *sdk.CallToolResult {
	data, _ := json.Marshal(map[string]string{"code": code, "detail": detail})
	return &sdk.CallToolResult{
		Content:           []sdk.Content{&sdk.TextContent{Text: string(data)}},
		StructuredContent: json.RawMessage(data),
		IsError:           true,
	}
}

// fail maps a client error onto the api.md §2 catalog. Transport failures keep
// the retryable "unavailable" meaning instead of escaping as JSON-RPC errors.
func fail(err error) (*sdk.CallToolResult, error) {
	var ce *client.Error
	if errors.As(err, &ce) {
		code := ce.Code
		if code == "" {
			code = "internal"
		}
		return errorResult(code, ce.Detail), nil
	}
	return errorResult("unavailable", err.Error()), nil
}

// waitSandbox polls until the Sandbox reaches state or a terminal state, for
// at most sandboxWait. Polling instead of `?wait_for=` keeps every request
// under the client's 30-second HTTP timeout.
func (s *server) waitSandbox(ctx context.Context, id, state string) (*client.Sandbox, error) {
	deadline := time.Now().Add(sandboxWait)
	for {
		sb, err := s.c.GetSandbox(ctx, id, "")
		if err != nil {
			return nil, err
		}
		if state == "" || sb.State == state || sb.State == "failed" || sb.State == "deleted" {
			return sb, nil
		}
		if time.Now().After(deadline) {
			return sb, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// Frame helpers.

// frameEnd is the byte offset just past a frame, matching the Supervisor's
// record framing (api.md §4).
func frameEnd(f client.Frame) int64 {
	data := []byte(f.Data)
	if f.Encoding == "base64" {
		if raw, err := base64.StdEncoding.DecodeString(f.Data); err == nil {
			data = raw
		}
	}
	return f.Seq + 5 + int64(len(data))
}

// frameSize is the number of output bytes a frame carries.
func frameSize(f client.Frame) int64 {
	return frameEnd(f) - f.Seq - 5
}

// readFrames reads every page starting at after and returns the frames and the
// offset just past the last one. Empty frames means the log is exhausted.
func readFrames(ctx context.Context, c *client.Client, execID string, after int64) ([]client.Frame, int64, error) {
	var all []client.Frame
	offset := after
	for {
		page, err := c.ExecOutput(ctx, execID, offset)
		if err != nil {
			return nil, offset, err
		}
		if len(page) == 0 {
			return all, offset, nil
		}
		all = append(all, page...)
		offset = frameEnd(page[len(page)-1])
	}
}

// tailFrames keeps the last at most outputLimit bytes. Frames are never split:
// `after` must stay on a frame boundary (api.md §4).
func tailFrames(frames []client.Frame) []client.Frame {
	total := 0
	start := len(frames)
	for i := len(frames) - 1; i >= 0; i-- {
		n := int(frameSize(frames[i]))
		if total+n > outputLimit {
			if start == len(frames) {
				start = i // one oversized frame is still returned whole
			}
			break
		}
		total += n
		start = i
	}
	return frames[start:]
}

// headFrames keeps whole frames from the front up to outputLimit bytes.
func headFrames(frames []client.Frame) []client.Frame {
	total := 0
	for i, f := range frames {
		n := int(frameSize(f))
		if total+n > outputLimit && i > 0 {
			return frames[:i]
		}
		total += n
	}
	return frames
}

// progress sends one notification per poll when the request carries a token.
func progress(ctx context.Context, req *sdk.CallToolRequest, token any, bytes int64, state string) {
	if token == nil {
		return
	}
	_ = req.Session.NotifyProgress(ctx, &sdk.ProgressNotificationParams{
		ProgressToken: token,
		Progress:      float64(bytes),
		Message:       fmt.Sprintf("%d bytes; %s", bytes, state),
	})
}
