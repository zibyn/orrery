package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"golang.org/x/net/websocket"

	"github.com/zibyn/stars-sandbox/internal/store"
	"github.com/zibyn/stars-sandbox/internal/supervisor"
)

// outputPageBytes bounds one REST output page; follow pages are the same size.
const outputPageBytes = 256 << 10

type createExecRequest struct {
	Argv    []string          `json:"argv"`
	Env     map[string]string `json:"env"`
	Cwd     string            `json:"cwd"`
	Timeout *int64            `json:"timeout"`
	Stdin   *string           `json:"stdin"`
	Pty     json.RawMessage   `json:"pty"`
}

// validExecCwd reports whether cwd names a path inside /workspace (api.md §4).
// It accepts a relative path that cleans back inside the root and refuses an
// absolute path or traversal.
func validExecCwd(cwd string) bool {
	if cwd == "" {
		return true
	}
	clean := path.Clean(cwd)
	return !path.IsAbs(clean) && clean != ".." && !strings.HasPrefix(clean, "../")
}

type execResultDTO struct {
	Cause    string `json:"cause"`
	ExitCode *int32 `json:"exit_code,omitempty"`
	Signal   *int32 `json:"signal,omitempty"`
}

type execDTO struct {
	ID           string         `json:"id"`
	SandboxID    string         `json:"sandbox_id"`
	Argv         []string       `json:"argv"`
	Cwd          string         `json:"cwd"`
	Timeout      *int32         `json:"timeout"`
	State        string         `json:"state"`
	Result       *execResultDTO `json:"result"`
	LogTruncated bool           `json:"log_truncated"`
	AcceptedAt   time.Time      `json:"accepted_at"`
	StartedAt    *time.Time     `json:"started_at"`
	FinishedAt   *time.Time     `json:"finished_at"`
	DeadlineAt   *time.Time     `json:"deadline_at"`
}

type execList struct {
	Items      []execDTO `json:"items"`
	NextCursor *string   `json:"next_cursor"`
}

// logFrame is one stored output frame. Data is UTF-8 unless Encoding is
// base64 (api.md §4).
type logFrame struct {
	Seq      int64  `json:"seq"`
	Stream   string `json:"stream"`
	Data     string `json:"data"`
	Encoding string `json:"encoding,omitempty"`
}

// end is the frame's end offset in the Node record: seq + header + data.
func (f logFrame) end() int64 {
	n := len(f.Data)
	if f.Encoding == "base64" {
		if raw, err := base64.StdEncoding.DecodeString(f.Data); err == nil {
			n = len(raw)
		}
	}
	return f.Seq + supervisor.HeaderSize + int64(n)
}

type execResultFrame struct {
	Type         string `json:"type"`
	Cause        string `json:"cause"`
	ExitCode     *int32 `json:"exit_code,omitempty"`
	Signal       *int32 `json:"signal,omitempty"`
	LogTruncated bool   `json:"log_truncated"`
}

// hasPTY reports whether a requested or stored pty field asks for a PTY.
func hasPTY(raw json.RawMessage) bool {
	return len(raw) > 0 && string(raw) != "null"
}

// ptySize is the requested terminal size. Zero means "not known yet": the
// Supervisor then leaves the pty at its default and waits for `resize`.
type ptySize struct {
	Rows uint16 `json:"rows"`
	Cols uint16 `json:"cols"`
}

func parsePTY(w http.ResponseWriter, raw json.RawMessage) (ptySize, bool) {
	var size ptySize
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&size); err != nil {
		writeProblem(w, "invalid_request", "pty must be {rows, cols} with values at most 65535")
		return ptySize{}, false
	}
	return size, true
}

// rejectPTY writes the unsupported problem for Exec surfaces that do not exist
// in PTY mode, and reports whether it wrote one.
func rejectPTY(w http.ResponseWriter, raw json.RawMessage, detail string) bool {
	if !hasPTY(raw) {
		return false
	}
	writeProblem(w, "unsupported", detail)
	return true
}

func (s *Server) handleCreateExec(w http.ResponseWriter, r *http.Request, p *principal) {
	sandboxID, ok := pathUUID(r, "id")
	if !ok {
		writeProblem(w, "invalid_request", "id must be a UUID")
		return
	}
	ctx := r.Context()
	sb, ok := s.getOwnedSandbox(w, ctx, p, sandboxID)
	if !ok {
		return
	}
	key, ok := idempotencyKey(r)
	if !ok {
		writeProblem(w, "invalid_request", "Idempotency-Key is too long")
		return
	}
	lookup := func() (store.Exec, error) {
		return s.q.GetExecByIdempotencyKey(ctx, store.GetExecByIdempotencyKeyParams{
			SandboxID: sandboxID, IdempotencyKey: key,
		})
	}
	if replayOr(s, w, "look up idempotent Exec", key, lookup, func(e store.Exec) {
		writeJSON(w, http.StatusOK, execToDTO(e))
	}) {
		return
	}
	if SandboxState(sb.State) != SandboxReady || SandboxDesiredState(sb.DesiredState) != SandboxDesiredRunning {
		writeProblem(w, "conflict", "the Sandbox is not ready")
		return
	}

	var req createExecRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	var pty json.RawMessage
	if hasPTY(req.Pty) {
		size, ok := parsePTY(w, req.Pty)
		if !ok {
			return
		}
		pty, _ = json.Marshal(size) // validated: marshal cannot fail
	}
	if len(req.Argv) == 0 || req.Argv[0] == "" {
		writeProblem(w, "invalid_request", "argv must be a non-empty array whose first element is a program")
		return
	}
	for _, arg := range req.Argv {
		if strings.ContainsRune(arg, 0) {
			writeProblem(w, "invalid_request", "argv must not contain NUL bytes")
			return
		}
	}
	if !validExecCwd(req.Cwd) {
		writeProblem(w, "invalid_request", "cwd must be a path inside /workspace")
		return
	}
	for k, v := range req.Env {
		if k == "" || strings.ContainsAny(k, "=\x00") || strings.ContainsRune(v, 0) {
			writeProblem(w, "invalid_request", "env keys must be non-empty without '=' or NUL; values must not contain NUL")
			return
		}
	}
	var timeout *int32
	if req.Timeout != nil {
		if *req.Timeout < 1 || *req.Timeout > math.MaxInt32 {
			writeProblem(w, "invalid_request", "timeout must be a positive number of seconds")
			return
		}
		t := int32(*req.Timeout)
		timeout = &t
	}

	id, err := uuid.NewV7()
	if err != nil {
		s.internal(w, "generate Exec id", err)
		return
	}
	argv, err := json.Marshal(req.Argv)
	if err != nil {
		s.internal(w, "encode argv", err)
		return
	}
	env := []byte("{}")
	if req.Env != nil {
		if env, err = json.Marshal(req.Env); err != nil {
			s.internal(w, "encode env", err)
			return
		}
	}
	var stdin []byte
	if req.Stdin != nil && pty == nil { // PTY mode ignores stdin (api.md §4)
		stdin = []byte(*req.Stdin)
	}
	exec, replayed, err := createIdempotent(key, lookup,
		func() (store.Exec, error) {
			return s.q.CreateExec(ctx, store.CreateExecParams{
				ID: id, SandboxID: sandboxID, Argv: argv, Env: env, Cwd: req.Cwd,
				Stdin: stdin, TimeoutSeconds: timeout, Generation: sb.Generation,
				IdempotencyKey: key, Pty: pty,
			})
		})
	if err != nil {
		s.internal(w, "create Exec", err)
		return
	}
	if replayed {
		writeJSON(w, http.StatusOK, execToDTO(exec))
		return
	}
	s.activity.touch(sandboxID)
	s.changed(ctx)
	writeJSON(w, http.StatusCreated, execToDTO(exec))
}

func (s *Server) handleListExecs(w http.ResponseWriter, r *http.Request, p *principal) {
	sandboxID, ok := pathUUID(r, "id")
	if !ok {
		writeProblem(w, "invalid_request", "id must be a UUID")
		return
	}
	if _, ok := s.getOwnedSandbox(w, r.Context(), p, sandboxID); !ok {
		return
	}
	rows, next, ok := readPage(s, w, r, "list Execs", parseCursor,
		func(ctx context.Context, limit int32, after *uuid.UUID) ([]store.Exec, error) {
			return s.q.ListExecs(ctx, store.ListExecsParams{
				SandboxID: sandboxID, After: after, Limit: limit,
			})
		},
		func(e store.Exec) string { return EncodeCursor(e.ID) })
	if !ok {
		return
	}
	list := execList{Items: make([]execDTO, 0, len(rows)), NextCursor: next}
	for _, row := range rows {
		list.Items = append(list.Items, execToDTO(row))
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleGetExec(w http.ResponseWriter, r *http.Request, p *principal) {
	e, ok := s.getOwnedExec(w, r, p)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, execToDTO(e))
}

func (s *Server) handleCancelExec(w http.ResponseWriter, r *http.Request, p *principal) {
	e, ok := s.getOwnedExec(w, r, p)
	if !ok {
		return
	}
	ctx := r.Context()
	canceled, err := s.q.CancelExec(ctx, e.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		current, getErr := s.q.GetExec(ctx, e.ID)
		if getErr != nil {
			s.internal(w, "read finished Exec", getErr)
			return
		}
		writeJSON(w, http.StatusAccepted, execToDTO(current))
		return
	}
	if err != nil {
		s.internal(w, "cancel Exec", err)
		return
	}
	s.changed(ctx)
	writeJSON(w, http.StatusAccepted, execToDTO(canceled))
}

func (s *Server) handleExecOutput(w http.ResponseWriter, r *http.Request, p *principal) {
	e, ok := s.getOwnedExec(w, r, p)
	if !ok {
		return
	}
	if rejectPTY(w, e.Pty, "PTY Execs have no output page") {
		return
	}
	after, ok := parseAfter(r)
	if !ok {
		writeProblem(w, "invalid_request", "after must be a non-negative byte offset")
		return
	}
	follow := false
	if raw := r.URL.Query().Get("follow"); raw != "" {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			writeProblem(w, "invalid_request", "follow must be true or false")
			return
		}
		follow = parsed
	}
	if !follow {
		frames, err := s.execFrames(r.Context(), e.ID, after)
		if err != nil {
			s.internal(w, "read Exec output", err)
			return
		}
		writeJSON(w, http.StatusOK, frames)
		return
	}
	s.followExecOutput(w, r, e.ID, after)
}

// followExecOutput upgrades to a WebSocket that replays from after and ends
// with the result frame once the Exec is finished and every frame is sent.
func (s *Server) followExecOutput(w http.ResponseWriter, r *http.Request, execID uuid.UUID, after int64) {
	server := websocket.Server{
		// Origin was already enforced for cookie-auth in authenticate.
		Handshake: func(*websocket.Config, *http.Request) error { return nil },
		Handler: func(conn *websocket.Conn) {
			ctx := r.Context()
			closed := make(chan struct{})
			go func() {
				var discard []byte
				_ = websocket.Message.Receive(conn, &discard)
				close(closed)
			}()
			for {
				frames, err := s.execFrames(ctx, execID, after)
				if err != nil {
					return
				}
				for _, f := range frames {
					if websocket.JSON.Send(conn, f) != nil {
						return
					}
					after = f.end()
				}
				e, err := s.q.GetExec(ctx, execID)
				if err != nil {
					return
				}
				if e.FinishedAt.Valid && after >= e.StoredSeq {
					cause := string(ExecExited)
					if e.Cause != nil {
						cause = *e.Cause
					}
					_ = websocket.JSON.Send(conn, execResultFrame{
						Type: "result", Cause: cause, ExitCode: e.ExitCode,
						Signal: e.Signal, LogTruncated: e.LogTruncated,
					})
					return
				}
				select {
				case <-ctx.Done():
					return
				case <-closed:
					return
				case <-time.After(100 * time.Millisecond):
				}
			}
		},
	}
	server.ServeHTTP(w, r)
}

// execFrames reads at most one page of stored frames at or after after.
// ponytail: a page is bounded by outputPageBytes plus the chunk query's
// LIMIT 32; raise both together if 256 KiB pages prove too small.
func (s *Server) execFrames(ctx context.Context, execID uuid.UUID, after int64) ([]logFrame, error) {
	rows, err := s.q.ListExecOutputChunks(ctx, store.ListExecOutputChunksParams{
		ExecID: execID, After: after,
	})
	if err != nil {
		return nil, err
	}
	frames := []logFrame{}
	size := 0
	for _, row := range rows {
		var chunk []logFrame
		if err := json.Unmarshal(row.Frames, &chunk); err != nil {
			return nil, fmt.Errorf("decode Exec output chunk %d: %w", row.FirstSeq, err)
		}
		for _, f := range chunk {
			if f.Seq < after {
				continue
			}
			frames = append(frames, f)
			size += len(f.Data)
			if size >= outputPageBytes {
				return frames, nil
			}
		}
	}
	return frames, nil
}

// getOwnedExec resolves the Exec named by the path and checks that the caller
// owns its Sandbox. A missing Sandbox also reads as not found.
func (s *Server) getOwnedExec(w http.ResponseWriter, r *http.Request, p *principal) (store.Exec, bool) {
	id, ok := pathUUID(r, "id")
	if !ok {
		writeProblem(w, "invalid_request", "id must be a UUID")
		return store.Exec{}, false
	}
	ctx := r.Context()
	return loadVisible(s, w, ctx, p, id, "Exec",
		func(ctx context.Context, id uuid.UUID) (store.Exec, uuid.UUID, error) {
			e, err := s.q.GetExec(ctx, id)
			if err != nil {
				return store.Exec{}, uuid.Nil, err
			}
			// An Exec outlives its Sandbox: the log is durable, so ownership is
			// read from the row even after the Sandbox is deleted.
			sb, err := s.q.GetSandboxIncludingDeleted(ctx, e.SandboxID)
			if err != nil {
				return store.Exec{}, uuid.Nil, err
			}
			return e, sb.OwnerID, nil
		})
}

func parseAfter(r *http.Request) (int64, bool) {
	raw := r.URL.Query().Get("after")
	if raw == "" {
		return 0, true
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

func execToDTO(e store.Exec) execDTO {
	state := ""
	if e.State != nil {
		state = *e.State
	}
	argv := []string{}
	_ = json.Unmarshal(e.Argv, &argv)
	dto := execDTO{
		ID: e.ID.String(), SandboxID: e.SandboxID.String(), Argv: argv,
		Cwd: e.Cwd, Timeout: e.TimeoutSeconds, State: state,
		LogTruncated: e.LogTruncated, AcceptedAt: e.AcceptedAt.Time.UTC(),
		StartedAt: timePtr(e.StartedAt), FinishedAt: timePtr(e.FinishedAt),
		DeadlineAt: timePtr(e.DeadlineAt),
	}
	if e.FinishedAt.Valid && e.Cause != nil {
		dto.Result = &execResultDTO{Cause: *e.Cause, ExitCode: e.ExitCode, Signal: e.Signal}
	}
	return dto
}

// logFrameFromProto stores one Agent frame; invalid UTF-8 becomes base64
// (api.md §4).
func logFrameFromProto(seq int64, stream string, data []byte) logFrame {
	if utf8.Valid(data) {
		return logFrame{Seq: seq, Stream: stream, Data: string(data)}
	}
	return logFrame{
		Seq: seq, Stream: stream,
		Data:     base64.StdEncoding.EncodeToString(data),
		Encoding: "base64",
	}
}
