package mcpserver

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zibyn/stars-sandbox/client"
)

// requireSandboxID applies the rule every Sandbox-scoped tool shares.
func requireSandboxID(id string) error {
	if id == "" {
		return fmt.Errorf("sandbox_id is required")
	}
	return nil
}

// Sandbox lifecycle.

type sandboxCreateInput struct {
	Name      *string                  `json:"name"`
	Labels    map[string]string        `json:"labels"`
	Runtime   *string                  `json:"runtime"`
	Workspace *client.SandboxWorkspace `json:"workspace"`
	Snapshot  *string                  `json:"snapshot"`
	Resources []client.SandboxResource `json:"resources"`
	Secrets   map[string]string        `json:"secrets"`
	CPU       *float64                 `json:"cpu"`
	Memory    *int64                   `json:"memory"`
	GPU       *client.SandboxGPU       `json:"gpu"`
	IdleStop  *int32                   `json:"idle_stop"`
}

func (in *sandboxCreateInput) validate() error {
	if in.Runtime != nil && *in.Runtime != "container" && *in.Runtime != "local" {
		return fmt.Errorf("runtime must be container or local")
	}
	if in.Workspace != nil && in.Workspace.Mode != "" && in.Workspace.Mode != "rw" && in.Workspace.Mode != "ro" {
		return fmt.Errorf("workspace.mode must be rw or ro")
	}
	return nil
}

func (s *server) sandboxCreate(ctx context.Context, _ *sdk.CallToolRequest, in sandboxCreateInput) (*sdk.CallToolResult, error) {
	created, err := s.c.CreateSandbox(ctx, client.CreateSandboxRequest{
		Name:      in.Name,
		Labels:    in.Labels,
		Runtime:   in.Runtime,
		Workspace: in.Workspace,
		Snapshot:  in.Snapshot,
		Resources: in.Resources,
		Secrets:   in.Secrets,
		CPU:       in.CPU,
		Memory:    in.Memory,
		GPU:       in.GPU,
		IdleStop:  in.IdleStop,
	}, "")
	if err != nil {
		return fail(err)
	}
	sb, err := s.c.GetSandbox(ctx, created.ID, "ready")
	if err != nil {
		return fail(err)
	}
	return okResult(sb)
}

type sandboxGetInput struct {
	SandboxID string `json:"sandbox_id"`
	WaitFor   string `json:"wait_for"`
}

func (in *sandboxGetInput) validate() error {
	if err := requireSandboxID(in.SandboxID); err != nil {
		return err
	}
	return nil
}

func (s *server) sandboxGet(ctx context.Context, _ *sdk.CallToolRequest, in sandboxGetInput) (*sdk.CallToolResult, error) {
	sb, err := s.c.GetSandbox(ctx, in.SandboxID, in.WaitFor)
	if err != nil {
		return fail(err)
	}
	return okResult(sb)
}

type sandboxListInput struct {
	Owner  string `json:"owner"`
	State  string `json:"state"`
	Label  string `json:"label"`
	Node   string `json:"node"`
	HasGPU bool   `json:"has_gpu"`
	Limit  int    `json:"limit"`
	Cursor string `json:"cursor"`
}

func (s *server) sandboxList(ctx context.Context, _ *sdk.CallToolRequest, in sandboxListInput) (*sdk.CallToolResult, error) {
	list, err := s.c.ListSandboxes(ctx, client.ListSandboxesParams{
		Owner: in.Owner, State: in.State, Label: in.Label, Node: in.Node,
		HasGPU: in.HasGPU, Limit: in.Limit, Cursor: in.Cursor,
	})
	if err != nil {
		return fail(err)
	}
	if list.Items == nil {
		list.Items = []client.Sandbox{}
	}
	return okResult(list)
}

type sandboxIDInput struct {
	SandboxID string `json:"sandbox_id"`
}

func (in *sandboxIDInput) validate() error {
	if err := requireSandboxID(in.SandboxID); err != nil {
		return err
	}
	return nil
}

func (s *server) sandboxStart(ctx context.Context, _ *sdk.CallToolRequest, in sandboxIDInput) (*sdk.CallToolResult, error) {
	sb, err := s.c.StartSandbox(ctx, in.SandboxID)
	if err != nil {
		return fail(err)
	}
	return okResult(sb)
}

func (s *server) sandboxStop(ctx context.Context, _ *sdk.CallToolRequest, in sandboxIDInput) (*sdk.CallToolResult, error) {
	sb, err := s.c.StopSandbox(ctx, in.SandboxID)
	if err != nil {
		return fail(err)
	}
	return okResult(sb)
}

func (s *server) sandboxDelete(ctx context.Context, _ *sdk.CallToolRequest, in sandboxIDInput) (*sdk.CallToolResult, error) {
	sb, err := s.c.DeleteSandbox(ctx, in.SandboxID)
	if err != nil {
		return fail(err)
	}
	return okResult(sb)
}

// Exec.

// execResult is the shared success body of exec_run, exec_output and
// exec_cancel. After is set only by exec_run and NextAfter only by
// exec_output; the outputSchema of each tool documents its own fields.
type execResult struct {
	ExecID       string             `json:"exec_id"`
	State        string             `json:"state"`
	Result       *client.ExecResult `json:"result,omitempty"`
	Output       []client.Frame     `json:"output"`
	LogTruncated bool               `json:"log_truncated"`
	After        *int64             `json:"after,omitempty"`
	NextAfter    *int64             `json:"next_after,omitempty"`
}

type execRunInput struct {
	SandboxID   string            `json:"sandbox_id"`
	Command     string            `json:"command"`
	Cwd         string            `json:"cwd"`
	Env         map[string]string `json:"env"`
	Stdin       *string           `json:"stdin"`
	Timeout     *int64            `json:"timeout"`
	WaitSeconds *int              `json:"wait_seconds"`
}

func (in *execRunInput) validate() error {
	if err := requireSandboxID(in.SandboxID); err != nil {
		return err
	}
	if in.Command == "" {
		return fmt.Errorf("command is required")
	}
	if in.WaitSeconds != nil && (*in.WaitSeconds < 0 || *in.WaitSeconds > waitLimit) {
		return fmt.Errorf("wait_seconds must be between 0 and %d", waitLimit)
	}
	if in.Timeout != nil && *in.Timeout <= 0 {
		return fmt.Errorf("timeout must be positive")
	}
	return nil
}

func (in *execRunInput) wait() time.Duration {
	if in.WaitSeconds == nil {
		return 30 * time.Second
	}
	return time.Duration(*in.WaitSeconds) * time.Second
}

func (s *server) execRun(ctx context.Context, req *sdk.CallToolRequest, in execRunInput) (*sdk.CallToolResult, error) {
	created, err := s.c.CreateExec(ctx, in.SandboxID, client.ExecRequest{
		Argv:    []string{"sh", "-c", in.Command},
		Cwd:     in.Cwd,
		Env:     in.Env,
		Stdin:   in.Stdin,
		Timeout: in.Timeout,
	}, "")
	if err != nil {
		return fail(err)
	}
	token := req.Params.GetProgressToken()
	deadline := time.Now().Add(in.wait())
	exec := created
	var frames []client.Frame
	var offset int64
	var sentBytes int64
	sentState := ""
	for {
		more, end, err := readFrames(ctx, s.c, exec.ID, offset)
		if err != nil {
			return fail(err)
		}
		frames = append(frames, more...)
		offset = end
		if token != nil && (offset > sentBytes || exec.State != sentState) {
			progress(ctx, req, token, offset, exec.State)
			sentBytes, sentState = offset, exec.State
		}
		if exec.State == "finished" || !time.Now().Before(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return fail(ctx.Err())
		case <-time.After(pollInterval):
		}
		if exec, err = s.c.GetExec(ctx, exec.ID); err != nil {
			return fail(err)
		}
	}
	state := exec.State
	if state != "finished" {
		// api.md §7: a call that stops waiting reports running.
		state = "running"
	}
	tail := tailFrames(frames)
	if tail == nil {
		tail = []client.Frame{}
	}
	body := execResult{
		ExecID:       exec.ID,
		State:        state,
		Result:       exec.Result,
		Output:       tail,
		LogTruncated: exec.LogTruncated,
		After:        &offset,
	}
	return okResult(body)
}

type execOutputInput struct {
	ExecID      string `json:"exec_id"`
	After       *int64 `json:"after"`
	WaitSeconds *int   `json:"wait_seconds"`
}

func (in *execOutputInput) validate() error {
	if in.ExecID == "" {
		return fmt.Errorf("exec_id is required")
	}
	if in.After != nil && *in.After < 0 {
		return fmt.Errorf("after must not be negative")
	}
	if in.WaitSeconds != nil && (*in.WaitSeconds < 0 || *in.WaitSeconds > waitLimit) {
		return fmt.Errorf("wait_seconds must be between 0 and %d", waitLimit)
	}
	return nil
}

func (s *server) execOutput(ctx context.Context, _ *sdk.CallToolRequest, in execOutputInput) (*sdk.CallToolResult, error) {
	tail := in.After == nil
	// offset is where this read starts; end is the offset just past what the
	// log currently holds.
	var offset int64
	if in.After != nil {
		offset = *in.After
	}
	frames, end, err := readFrames(ctx, s.c, in.ExecID, offset)
	if err != nil {
		return fail(err)
	}
	if tail {
		frames = tailFrames(frames)
	} else {
		frames = headFrames(frames)
	}
	next := offset
	if len(frames) > 0 {
		next = frames[len(frames)-1].End()
	}

	exec, err := s.c.GetExec(ctx, in.ExecID)
	if err != nil {
		return fail(err)
	}
	waitSeconds := 0
	if in.WaitSeconds != nil {
		waitSeconds = *in.WaitSeconds
	}
	// With after, existing frames already answer the call; only wait when the
	// page is empty. Without after, the tail may hold old output, so wait past
	// its end for new output or for the Exec to finish (api.md §7).
	if waitSeconds > 0 && ((!tail && len(frames) == 0) || (tail && exec.State != "finished")) {
		deadline := time.Now().Add(time.Duration(waitSeconds) * time.Second)
		for {
			// Read new output before deciding to sleep, so a finished Exec's
			// last frames are never missed.
			more, moreEnd, err := readFrames(ctx, s.c, in.ExecID, end)
			if err != nil {
				return fail(err)
			}
			if len(more) > 0 {
				if tail {
					frames = tailFrames(append(frames, more...))
				} else {
					frames = headFrames(more)
				}
				next = frames[len(frames)-1].End()
				// Output may have arrived together with the finish; report the
				// state that goes with the frames.
				if exec, err = s.c.GetExec(ctx, in.ExecID); err != nil {
					return fail(err)
				}
				break
			}
			end = moreEnd
			if exec.State == "finished" || !time.Now().Before(deadline) {
				break
			}
			select {
			case <-ctx.Done():
				return fail(ctx.Err())
			case <-time.After(pollInterval):
			}
			if exec, err = s.c.GetExec(ctx, in.ExecID); err != nil {
				return fail(err)
			}
		}
	}

	if frames == nil {
		frames = []client.Frame{}
	}
	return okResult(execResult{
		ExecID:       exec.ID,
		State:        exec.State,
		Result:       exec.Result,
		Output:       frames,
		LogTruncated: exec.LogTruncated,
		NextAfter:    &next,
	})
}

type execIDInput struct {
	ExecID string `json:"exec_id"`
}

func (in *execIDInput) validate() error {
	if in.ExecID == "" {
		return fmt.Errorf("exec_id is required")
	}
	return nil
}

func (s *server) execCancel(ctx context.Context, _ *sdk.CallToolRequest, in execIDInput) (*sdk.CallToolResult, error) {
	exec, err := s.c.CancelExec(ctx, in.ExecID)
	if err != nil {
		return fail(err)
	}
	return okResult(execResult{
		ExecID:       exec.ID,
		State:        exec.State,
		Result:       exec.Result,
		Output:       []client.Frame{},
		LogTruncated: exec.LogTruncated,
	})
}

// Files.

type fileInput struct {
	SandboxID string `json:"sandbox_id"`
	Path      string `json:"path"`
}

func (in *fileInput) validate() error {
	if err := requireSandboxID(in.SandboxID); err != nil {
		return err
	}
	if in.Path == "" {
		return fmt.Errorf("path is required")
	}
	return nil
}

func (s *server) fileList(ctx context.Context, _ *sdk.CallToolRequest, in fileInput) (*sdk.CallToolResult, error) {
	list, err := s.c.ListFiles(ctx, in.SandboxID, in.Path)
	if err != nil {
		return fail(err)
	}
	if list.Items == nil {
		list.Items = []client.FileEntry{}
	}
	return okResult(list)
}

func (s *server) fileStat(ctx context.Context, _ *sdk.CallToolRequest, in fileInput) (*sdk.CallToolResult, error) {
	entry, err := s.c.StatFile(ctx, in.SandboxID, in.Path)
	if err != nil {
		return fail(err)
	}
	return okResult(entry)
}

type fileReadInput struct {
	SandboxID string `json:"sandbox_id"`
	Path      string `json:"path"`
	Offset    int64  `json:"offset"`
	Limit     int64  `json:"limit"`
}

func (in *fileReadInput) validate() error {
	if err := requireSandboxID(in.SandboxID); err != nil {
		return err
	}
	if in.Path == "" {
		return fmt.Errorf("path is required")
	}
	if in.Offset < 0 {
		return fmt.Errorf("offset must not be negative")
	}
	if in.Limit < 0 || in.Limit > fileLimit {
		return fmt.Errorf("limit must be between 1 and %d", fileLimit)
	}
	return nil
}

func (s *server) fileRead(ctx context.Context, _ *sdk.CallToolRequest, in fileReadInput) (*sdk.CallToolResult, error) {
	limit := in.Limit
	if limit == 0 {
		limit = fileLimit
	}
	byteRange := fmt.Sprintf("bytes=%d-%d", in.Offset, in.Offset+limit-1)
	data, contentType, err := s.c.ReadFileRange(ctx, in.SandboxID, in.Path, byteRange)
	if err != nil {
		// Reading past the end is an empty result, not a failure.
		var ce *client.Error
		if errors.As(err, &ce) && ce.Status == http.StatusRequestedRangeNotSatisfiable {
			data, contentType, err = nil, "", nil
		}
		if err != nil {
			return fail(err)
		}
	}
	if len(data) > fileLimit {
		data = data[:fileLimit]
	}
	body := map[string]any{
		"path":         in.Path,
		"offset":       in.Offset,
		"size":         len(data),
		"content_type": contentType,
	}
	switch {
	case strings.HasPrefix(contentType, "image/"):
		body["encoding"] = "image"
		result, err := okResult(body)
		if err != nil {
			return nil, err
		}
		result.Content = append(result.Content, &sdk.ImageContent{Data: data, MIMEType: contentType})
		return result, nil
	case utf8.Valid(data):
		body["encoding"] = "text"
		body["content"] = string(data)
	default:
		body["encoding"] = "base64"
		body["content_base64"] = base64.StdEncoding.EncodeToString(data)
	}
	return okResult(body)
}

type fileWriteInput struct {
	SandboxID string `json:"sandbox_id"`
	Path      string `json:"path"`
	Content   string `json:"content"`
	Encoding  string `json:"encoding"`
}

func (in *fileWriteInput) validate() error {
	if err := requireSandboxID(in.SandboxID); err != nil {
		return err
	}
	if in.Path == "" {
		return fmt.Errorf("path is required")
	}
	if in.Encoding != "" && in.Encoding != "text" && in.Encoding != "base64" {
		return fmt.Errorf("encoding must be text or base64")
	}
	return nil
}

func (s *server) fileWrite(ctx context.Context, _ *sdk.CallToolRequest, in fileWriteInput) (*sdk.CallToolResult, error) {
	data := []byte(in.Content)
	if in.Encoding == "base64" {
		decoded, err := base64.StdEncoding.DecodeString(in.Content)
		if err != nil {
			return errorResult("invalid_request", "content is not valid base64: "+err.Error()), nil
		}
		data = decoded
	}
	if len(data) > fileLimit {
		return errorResult("invalid_request", fmt.Sprintf("content exceeds the %d-byte limit", fileLimit)), nil
	}
	if err := s.c.WriteFile(ctx, in.SandboxID, in.Path, bytes.NewReader(data), int64(len(data))); err != nil {
		return fail(err)
	}
	return okResult(map[string]any{"path": in.Path, "size": len(data)})
}

func (s *server) fileMkdir(ctx context.Context, _ *sdk.CallToolRequest, in fileInput) (*sdk.CallToolResult, error) {
	if err := s.c.Mkdir(ctx, in.SandboxID, in.Path); err != nil {
		return fail(err)
	}
	return okResult(map[string]any{"ok": true})
}

type fileMoveInput struct {
	SandboxID string `json:"sandbox_id"`
	From      string `json:"from"`
	To        string `json:"to"`
}

func (in *fileMoveInput) validate() error {
	if err := requireSandboxID(in.SandboxID); err != nil {
		return err
	}
	if in.From == "" || in.To == "" {
		return fmt.Errorf("from and to are required")
	}
	return nil
}

func (s *server) fileMove(ctx context.Context, _ *sdk.CallToolRequest, in fileMoveInput) (*sdk.CallToolResult, error) {
	if err := s.c.MoveFile(ctx, in.SandboxID, in.From, in.To); err != nil {
		return fail(err)
	}
	return okResult(map[string]any{"ok": true})
}

func (s *server) fileDelete(ctx context.Context, _ *sdk.CallToolRequest, in fileInput) (*sdk.CallToolResult, error) {
	if err := s.c.DeleteFile(ctx, in.SandboxID, in.Path); err != nil {
		return fail(err)
	}
	return okResult(map[string]any{"ok": true})
}

type portURLInput struct {
	SandboxID string `json:"sandbox_id"`
	Port      int    `json:"port"`
}

func (in *portURLInput) validate() error {
	if err := requireSandboxID(in.SandboxID); err != nil {
		return err
	}
	if in.Port < 1 || in.Port > 65535 {
		return fmt.Errorf("port must be between 1 and 65535")
	}
	return nil
}

func (s *server) portURL(ctx context.Context, _ *sdk.CallToolRequest, in portURLInput) (*sdk.CallToolResult, error) {
	ticket, err := s.c.CreatePortTicket(ctx, in.SandboxID, in.Port)
	if err != nil {
		return fail(err)
	}
	return okResult(map[string]any{"url": ticket.URL})
}
