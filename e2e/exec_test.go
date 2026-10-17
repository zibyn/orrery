//go:build e2e

package e2e

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

type execView struct {
	ID           string `json:"id"`
	SandboxID    string `json:"sandbox_id"`
	State        string `json:"state"`
	LogTruncated bool   `json:"log_truncated"`
	Result       *struct {
		Cause    string `json:"cause"`
		ExitCode *int32 `json:"exit_code"`
		Signal   *int32 `json:"signal"`
	} `json:"result"`
}

type execFrame struct {
	Type         string `json:"type"`
	Seq          int64  `json:"seq"`
	Stream       string `json:"stream"`
	Data         string `json:"data"`
	Encoding     string `json:"encoding"`
	Cause        string `json:"cause"`
	ExitCode     *int32 `json:"exit_code"`
	Signal       *int32 `json:"signal"`
	LogTruncated bool   `json:"log_truncated"`
}

var execSandboxSeq atomic.Int64

// readyExecSandbox is shared setup: an enrolled Node and a ready Sandbox.
func readyExecSandbox(t *testing.T, admin string, memory int64) string {
	t.Helper()
	suffix := time.Now().UnixNano()
	nodeName := fmt.Sprintf("e2e-exec-node-%d-%d", suffix, execSandboxSeq.Add(1))
	state, dataRoot := enrollAgent(t, admin, nodeName)
	startAgent(t, state, dataRoot)
	waitForNodeReady(t, admin, nodeName)

	status, data := call(t, http.MethodPost, "/api/v1/sandboxes", admin, map[string]any{
		"name":      fmt.Sprintf("exec-%d-%d", suffix, execSandboxSeq.Add(1)),
		"cpu":       0.5,
		"memory":    memory,
		"idle_stop": 0,
	})
	if status != http.StatusCreated {
		t.Fatalf("create Sandbox: %d %s", status, data)
	}
	var sb sandboxView
	mustJSON(t, data, &sb)
	status, data = call(t, http.MethodGet, "/api/v1/sandboxes/"+sb.ID+"?wait_for=ready", admin, nil)
	var ready sandboxView
	mustJSON(t, data, &ready)
	if status != http.StatusOK || ready.State != "ready" {
		t.Fatalf("wait_for=ready: %d %s", status, data)
	}
	return sb.ID
}

func createExec(t *testing.T, admin, sandboxID string, body map[string]any) execView {
	t.Helper()
	status, data := call(t, http.MethodPost, "/api/v1/sandboxes/"+sandboxID+"/execs", admin, body)
	if status != http.StatusCreated {
		t.Fatalf("create Exec: %d %s", status, data)
	}
	var e execView
	mustJSON(t, data, &e)
	return e
}

func getExec(t *testing.T, admin, id string) execView {
	t.Helper()
	status, data := call(t, http.MethodGet, "/api/v1/execs/"+id, admin, nil)
	if status != http.StatusOK {
		t.Fatalf("get Exec: %d %s", status, data)
	}
	var e execView
	mustJSON(t, data, &e)
	return e
}

func waitExecRunning(t *testing.T, admin, id string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if getExec(t, admin, id).State == "running" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("Exec %s never became running", id)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func wsURL() string {
	return strings.Replace(strings.Replace(apiURL(), "https://", "wss://", 1), "http://", "ws://", 1)
}

// followExec opens the follow WebSocket and collects output until the result
// frame.
func followExec(t *testing.T, admin, id string, timeout time.Duration) (stdout, stderr []byte, frames []execFrame, result execFrame) {
	t.Helper()
	cfg, err := websocket.NewConfig(wsURL()+"/api/v1/execs/"+id+"/output?follow=true", apiOrigin())
	if err != nil {
		t.Fatalf("websocket config: %v", err)
	}
	cfg.Header.Set("Authorization", "Bearer "+admin)
	conn, err := websocket.DialConfig(cfg)
	if err != nil {
		t.Fatalf("dial follow WebSocket: %v", err)
	}
	defer conn.Close()
	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	var out, errOut bytes.Buffer
	for {
		var f execFrame
		if err := websocket.JSON.Receive(conn, &f); err != nil {
			t.Fatalf("receive output frame: %v", err)
		}
		frames = append(frames, f)
		if f.Type == "result" {
			return out.Bytes(), errOut.Bytes(), frames, f
		}
		data := []byte(f.Data)
		if f.Encoding == "base64" {
			decoded, err := base64.StdEncoding.DecodeString(f.Data)
			if err != nil {
				t.Fatalf("decode base64 frame: %v", err)
			}
			data = decoded
		}
		switch f.Stream {
		case "stdout":
			out.Write(data)
		case "stderr":
			errOut.Write(data)
		}
	}
}

// TestExecLogStream is the "exec-log-stream" acceptance scenario (#51).
func TestExecLogStream(t *testing.T) {
	admin := adminToken(t)
	raiseSandboxQuota(t, admin, 10)
	sandboxID := readyExecSandbox(t, admin, 128<<20)

	e := createExec(t, admin, sandboxID, map[string]any{
		"argv": []string{"sh", "-c", "printf 'hello '; printf 'oops' >&2; printf '\\377'; exit 7"},
	})
	stdout, stderr, frames, result := followExec(t, admin, e.ID, 30*time.Second)
	// The invalid byte is stdout too; only its encoding differs.
	if want := []byte{'h', 'e', 'l', 'l', 'o', ' ', 0xff}; !bytes.Equal(stdout, want) {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
	if string(stderr) != "oops" {
		t.Fatalf("stderr = %q, want %q", stderr, "oops")
	}
	var sawInvalid bool
	for _, f := range frames {
		if f.Encoding == "base64" {
			raw, err := base64.StdEncoding.DecodeString(f.Data)
			if err != nil {
				t.Fatalf("decode base64 frame: %v", err)
			}
			if len(raw) == 1 && raw[0] == 0xff {
				sawInvalid = true
			}
		}
	}
	if !sawInvalid {
		t.Fatalf("no base64 frame carrying 0xff in %+v", frames)
	}
	if result.Cause != "exited" || result.ExitCode == nil || *result.ExitCode != 7 {
		t.Fatalf("result = %+v, want exited with code 7", result)
	}

	finished := getExec(t, admin, e.ID)
	if finished.State != "finished" || finished.Result == nil || finished.Result.Cause != "exited" {
		t.Fatalf("finished Exec = %+v", finished)
	}

	// The same frames are readable as a page after the Exec finished.
	status, data := call(t, http.MethodGet, "/api/v1/execs/"+e.ID+"/output?after=0", admin, nil)
	if status != http.StatusOK {
		t.Fatalf("output page: %d %s", status, data)
	}
	var page []execFrame
	mustJSON(t, data, &page)
	if len(page) != len(frames)-1 {
		t.Fatalf("output page has %d frames, the followed stream had %d before the result", len(page), len(frames)-1)
	}
	for i := range page {
		if page[i] != frames[i] {
			t.Fatalf("page frame %d = %+v, followed frame %+v", i, page[i], frames[i])
		}
	}

	// stdin is written once, then closed.
	cat := createExec(t, admin, sandboxID, map[string]any{
		"argv": []string{"cat"}, "stdin": "ping\n",
	})
	out, _, _, result := followExec(t, admin, cat.ID, 30*time.Second)
	if string(out) != "ping\n" || result.Cause != "exited" {
		t.Fatalf("cat output = %q, result = %+v", out, result)
	}
}

// TestExecCancelTimeout is the "exec-cancel-timeout" acceptance scenario (#51).
func TestExecCancelTimeout(t *testing.T) {
	admin := adminToken(t)
	raiseSandboxQuota(t, admin, 10)
	sandboxID := readyExecSandbox(t, admin, 128<<20)

	timeout := int64(1)
	timed := createExec(t, admin, sandboxID, map[string]any{
		"argv": []string{"sleep", "60"}, "timeout": timeout,
	})
	start := time.Now()
	_, _, _, result := followExec(t, admin, timed.ID, 30*time.Second)
	if result.Cause != "timed_out" {
		t.Fatalf("timed Exec result = %+v, want timed_out", result)
	}
	if time.Since(start) > 20*time.Second {
		t.Fatalf("timeout took %s; SIGTERM should end sleep immediately", time.Since(start))
	}

	canceled := createExec(t, admin, sandboxID, map[string]any{
		"argv": []string{"sleep", "60"},
	})
	waitExecRunning(t, admin, canceled.ID, 15*time.Second)
	status, data := call(t, http.MethodPost, "/api/v1/execs/"+canceled.ID+"/cancel", admin, nil)
	if status != http.StatusAccepted {
		t.Fatalf("cancel: %d %s", status, data)
	}
	_, _, _, result = followExec(t, admin, canceled.ID, 30*time.Second)
	if result.Cause != "canceled" {
		t.Fatalf("canceled Exec result = %+v, want canceled", result)
	}
}

// TestExecLogCap is the "log-cap" acceptance scenario (#51).
func TestExecLogCap(t *testing.T) {
	admin := adminToken(t)
	raiseSandboxQuota(t, admin, 10)
	sandboxID := readyExecSandbox(t, admin, 128<<20)

	capBytes := int64(1 << 20)
	if v := os.Getenv("STARS_E2E_EXEC_LOG_CAP"); v != "" {
		parsed, err := strconv.ParseInt(v, 10, 64)
		if err != nil || parsed <= 0 {
			t.Fatalf("STARS_E2E_EXEC_LOG_CAP = %q", v)
		}
		capBytes = parsed
	}

	e := createExec(t, admin, sandboxID, map[string]any{
		"argv": []string{"sh", "-c", fmt.Sprintf("yes y | head -c %d", capBytes*2)},
	})
	_, _, frames, result := followExec(t, admin, e.ID, 60*time.Second)
	if result.Cause != "exited" || result.ExitCode == nil || *result.ExitCode != 0 {
		t.Fatalf("result = %+v, want exited with code 0", result)
	}
	if !result.LogTruncated || !getExec(t, admin, e.ID).LogTruncated {
		t.Fatal("log_truncated is false after writing past the cap")
	}
	var sawTruncated bool
	for _, f := range frames {
		if f.Stream == "truncated" {
			sawTruncated = true
		}
	}
	if !sawTruncated {
		t.Fatalf("no truncated frame in %d frames", len(frames))
	}
}

// TestExecOOMKilled is the "oom-killed" acceptance scenario (#51).
func TestExecOOMKilled(t *testing.T) {
	admin := adminToken(t)
	raiseSandboxQuota(t, admin, 10)
	sandboxID := readyExecSandbox(t, admin, 128<<20)

	e := createExec(t, admin, sandboxID, map[string]any{
		"argv": []string{"sh", "-c", `a=xxxxxxxxxxxxxxxx; while :; do a="$a$a"; done`},
	})
	_, _, _, result := followExec(t, admin, e.ID, 60*time.Second)
	if result.Cause != "oom_killed" {
		t.Fatalf("result = %+v, want oom_killed", result)
	}
}

// TestExecDeleteCascade covers the running-Exec half of "delete-cascade".
func TestExecDeleteCascade(t *testing.T) {
	admin := adminToken(t)
	raiseSandboxQuota(t, admin, 10)
	sandboxID := readyExecSandbox(t, admin, 128<<20)

	e := createExec(t, admin, sandboxID, map[string]any{"argv": []string{"sleep", "60"}})
	waitExecRunning(t, admin, e.ID, 15*time.Second)

	status, data := call(t, http.MethodDelete, "/api/v1/sandboxes/"+sandboxID, admin, nil)
	if status != http.StatusAccepted {
		t.Fatalf("delete: %d %s", status, data)
	}

	deadline := time.Now().Add(60 * time.Second)
	for {
		finished := getExec(t, admin, e.ID)
		if finished.State == "finished" {
			if finished.Result == nil {
				t.Fatalf("finished Exec has no result: %+v", finished)
			}
			if finished.Result.Cause != "canceled" && finished.Result.Cause != "runtime_lost" {
				t.Fatalf("Exec cause = %q, want canceled or runtime_lost", finished.Result.Cause)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Exec %s still %s after the Sandbox was deleted", e.ID, finished.State)
		}
		time.Sleep(200 * time.Millisecond)
	}
	waitForSandboxDeleted(t, sandboxID, 60*time.Second)
}
