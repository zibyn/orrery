//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/websocket"

	"github.com/zibyn/orrery/client"
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

// readyExecSandbox is shared setup: an enrolled Node and a ready Sandbox.
func readyExecSandbox(t *testing.T, admin string, memory int64) string {
	t.Helper()
	suffix := time.Now().UnixNano()
	nodeName := fmt.Sprintf("e2e-exec-node-%d", suffix)
	state, dataRoot := enrollAgent(t, admin, nodeName)
	startAgent(t, state, dataRoot)
	waitForNodeReady(t, admin, nodeName)
	return createReadySandbox(t, admin, fmt.Sprintf("exec-%d", suffix), memory)
}

// createReadySandbox creates a Sandbox on an already-ready Node and waits for
// it to reach ready.
func createReadySandbox(t *testing.T, admin, name string, memory int64) string {
	t.Helper()
	return createReadySandboxWith(t, admin, map[string]any{
		"name":      name,
		"cpu":       0.5,
		"memory":    memory,
		"idle_stop": 0,
	})
}

// createReadySandboxWith creates a Sandbox from a full request body and waits
// for it to be ready.
func createReadySandboxWith(t *testing.T, admin string, body map[string]any) string {
	t.Helper()
	status, data := call(t, http.MethodPost, "/api/v1/sandboxes", admin, body)
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

func setUserSandboxQuota(t *testing.T, admin, userID string, sandboxes int) {
	t.Helper()
	status, data := call(t, http.MethodPut, "/api/v1/users/"+userID+"/quota", admin, map[string]any{
		"gpus": 0, "sandboxes": sandboxes,
	})
	if status != http.StatusOK {
		t.Fatalf("set quota for %s: %d %s", userID, status, data)
	}
}

// execInSandbox runs one Exec and returns its streams and exit code.
func execInSandbox(t *testing.T, admin, sandboxID string, argv ...string) (string, string, int) {
	t.Helper()
	e := createExec(t, admin, sandboxID, map[string]any{"argv": argv})
	stdout, stderr, _, result := followExec(t, admin, e.ID, 180*time.Second)
	if result.Cause != "exited" {
		t.Fatalf("%v: cause = %q, stderr %s", argv, result.Cause, stderr)
	}
	code := -1
	if result.ExitCode != nil {
		code = int(*result.ExitCode)
	}
	return string(stdout), string(stderr), code
}

// installGit adds git to an Alpine Sandbox from the package mirror. It skips
// on a Node whose Sandboxes cannot reach the mirror, which the harness
// declares with ORRERY_E2E_SANDBOX_NO_EGRESS (the shared containerd host).
func installGit(t *testing.T, admin, sandboxID string) {
	t.Helper()
	if os.Getenv("ORRERY_E2E_SANDBOX_NO_EGRESS") != "" {
		t.Skip("ORRERY_E2E_SANDBOX_NO_EGRESS: this Node's Sandboxes do not reach the Alpine mirror")
	}
	if out, errOut, code := execInSandbox(t, admin, sandboxID, "apk", "add", "--no-cache", "git"); code != 0 {
		t.Fatalf("install git: exit %d\n%s%s", code, out, errOut)
	}
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

// TestExecLogStream is the "exec-log-stream" acceptance scenario (#51) and its
// Go SDK parity half (#108): the same command through the client package must
// match the REST run, so both share one Sandbox.
func TestExecLogStream(t *testing.T) {
	admin := adminToken(t)
	raiseSandboxQuota(t, admin, 10)
	sandboxID := readyExecSandbox(t, admin, 128<<20)

	command := []string{"sh", "-c", "printf 'hello '; printf 'oops' >&2; printf '\\377'; exit 7"}
	e := createExec(t, admin, sandboxID, map[string]any{"argv": command})
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
	out, _, _, catResult := followExec(t, admin, cat.ID, 30*time.Second)
	if string(out) != "ping\n" || catResult.Cause != "exited" {
		t.Fatalf("cat output = %q, result = %+v", out, catResult)
	}

	// SDK parity: the same command through the client package matches the REST run above.
	ctx := context.Background()
	c := sdkClient(t)
	created, err := c.CreateExec(ctx, sandboxID, client.ExecRequest{Argv: command}, "")
	if err != nil {
		t.Fatalf("CreateExec: %v", err)
	}
	var sdkOut, sdkErr bytes.Buffer
	final, err := c.FollowOutput(ctx, created.ID, &sdkOut, &sdkErr)
	if err != nil {
		t.Fatalf("FollowOutput: %v", err)
	}
	if !bytes.Equal(sdkOut.Bytes(), stdout) {
		t.Fatalf("sdk stdout = %q, REST stdout = %q", sdkOut.Bytes(), stdout)
	}
	if sdkErr.String() != "oops" || sdkErr.String() != string(stderr) {
		t.Fatalf("sdk stderr = %q, REST stderr = %q", sdkErr.String(), stderr)
	}
	if final.State != "finished" || final.Result == nil || final.Result.Cause != "exited" ||
		final.Result.ExitCode == nil || *final.Result.ExitCode != 7 {
		t.Fatalf("sdk result = %+v", final)
	}
	if result.Cause != final.Result.Cause {
		t.Fatalf("REST result = %+v, sdk result = %+v", result, final.Result)
	}

	// The SDK's Exec read and output page agree with REST.
	direct := getExec(t, admin, created.ID)
	if direct.State != final.State || direct.Result == nil || direct.Result.Cause != final.Result.Cause {
		t.Fatalf("sdk GetExec = %+v, REST = %+v", final, direct)
	}
	sdkPage, err := c.ExecOutput(ctx, created.ID, 0)
	if err != nil {
		t.Fatalf("ExecOutput: %v", err)
	}
	status, data = call(t, http.MethodGet, "/api/v1/execs/"+created.ID+"/output?after=0", admin, nil)
	if status != http.StatusOK {
		t.Fatalf("output page: %d %s", status, data)
	}
	var restPage []execFrame
	mustJSON(t, data, &restPage)
	if len(sdkPage) != len(restPage) {
		t.Fatalf("sdk page has %d frames, REST has %d", len(sdkPage), len(restPage))
	}
	for i := range sdkPage {
		f, r := sdkPage[i], restPage[i]
		if f.Seq != r.Seq || f.Stream != r.Stream || f.Data != r.Data || f.Encoding != r.Encoding {
			t.Fatalf("frame %d: sdk %+v, REST %+v", i, f, r)
		}
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
	if v := os.Getenv("ORRERY_E2E_EXEC_LOG_CAP"); v != "" {
		parsed, err := strconv.ParseInt(v, 10, 64)
		if err != nil || parsed <= 0 {
			t.Fatalf("ORRERY_E2E_EXEC_LOG_CAP = %q", v)
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

// TestExecOOMKilled is the "oom-killed" acceptance scenario (#51). Under
// runsc the host may kill the Exec or the whole Sandbox; either way the
// cause is oom_killed (runtime.md §9).
func TestExecOOMKilled(t *testing.T) {
	admin := adminToken(t)
	raiseSandboxQuota(t, admin, 10)
	sandboxID := readyExecSandbox(t, admin, 128<<20)
	since, task := hostNow(), runtimeTaskPID(sandboxID)

	e := createExec(t, admin, sandboxID, map[string]any{
		"argv": []string{"sh", "-c", `a=xxxxxxxxxxxxxxxx; while :; do a="$a$a"; done`},
	})
	_, _, _, result := followExec(t, admin, e.ID, 60*time.Second)
	logOOMVictims(t, since, task)
	if result.Cause != "oom_killed" {
		t.Fatalf("result = %+v, want oom_killed", result)
	}
}

// TestExecOOMSentryKilled forces the other runsc outcome (#173, #177): memory
// the Sentry copies into the guest (a pipe) counts against the Sentry, so the
// host OOM killer takes the whole Sandbox. The Exec still ends oom_killed and
// the Sandbox stops with runtime_lost (runtime.md §9).
func TestExecOOMSentryKilled(t *testing.T) {
	if os.Getenv("ORRERY_E2E_ISOLATION") != "runsc" {
		t.Skip("only runsc has a Sentry to kill (ORRERY_E2E_ISOLATION=runsc)")
	}
	admin := adminToken(t)
	raiseSandboxQuota(t, admin, 10)
	sandboxID := readyExecSandbox(t, admin, 128<<20)
	since, task := hostNow(), runtimeTaskPID(sandboxID)

	e := createExec(t, admin, sandboxID, map[string]any{
		"argv": []string{"sh", "-c", "head -c 600m /dev/zero | tail"},
	})
	_, _, _, result := followExec(t, admin, e.ID, 60*time.Second)
	victims := logOOMVictims(t, since, task)
	if result.Cause != "oom_killed" {
		t.Fatalf("result = %+v, want oom_killed (victims %v, Sentry %s)", result, victims, task)
	}
	sb := waitForSandboxState(t, admin, sandboxID, "stopped", 60*time.Second)
	if sb.Cause == nil || *sb.Cause != "runtime_lost" {
		t.Fatalf("Sandbox cause = %v, want runtime_lost", sb.Cause)
	}
}

// runtimeTaskPID is the host pid of the Sandbox's task: runsc-sandbox (the
// Sentry) under runsc. Empty when ctr cannot say.
func runtimeTaskPID(sandboxID string) string {
	out, _ := sandboxCommand("task", "ls").CombinedOutput()
	for _, line := range strings.Split(string(out), "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[0] == "orrery-"+sandboxID {
			return f[1]
		}
	}
	return ""
}

// hostNow is the Node host's Unix time; its clock need not match this one.
func hostNow() string {
	out, _ := nodeShell("date +%s")
	return strings.TrimSpace(string(out))
}

// logOOMVictims logs the host kernel's OOM kills since a host Unix time, next to
// the Sandbox's task pid, so a run shows whether the stub or the Sentry died.
// Best effort: the Node shell may not read the kernel log.
func logOOMVictims(t *testing.T, since, task string) []string {
	t.Helper()
	out, _ := nodeShell(fmt.Sprintf("journalctl -k --no-pager --since @%s | grep 'Killed process'", since))
	var pids []string
	for _, line := range strings.Split(string(out), "\n") {
		if _, rest, ok := strings.Cut(line, "Killed process "); ok {
			pids = append(pids, strings.Fields(rest)[0])
		}
	}
	t.Logf("OOM victims %v, Sandbox task pid %s:\n%s", pids, task, out)
	return pids
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

// TestExecEnvironment checks what the platform adds to an Exec's environment
// (node-protocol.md): ORRERY_URL and the staged CLI on PATH, but no Sandbox
// Token and no Git config, so the caller's own GIT_CONFIG_* reaches git as
// given (ADR-0032).
func TestExecEnvironment(t *testing.T) {
	admin := adminToken(t)
	raiseSandboxQuota(t, admin, 10)
	sandboxID := readyExecSandbox(t, admin, 256<<20)

	run := func(body map[string]any) string {
		t.Helper()
		e := createExec(t, admin, sandboxID, body)
		stdout, stderr, _, result := followExec(t, admin, e.ID, 180*time.Second)
		if result.Cause != "exited" || result.ExitCode == nil || *result.ExitCode != 0 {
			t.Fatalf("%v: result = %+v, stderr %s", body["argv"], result, stderr)
		}
		return string(stdout)
	}

	env := run(map[string]any{"argv": []string{"env"}})
	for _, name := range []string{"GIT_CONFIG_COUNT", "ORRERY_SANDBOX_TOKEN"} {
		if strings.Contains(env, name+"=") {
			t.Fatalf("the Exec environment has %s:\n%s", name, env)
		}
	}
	if orreryURL := run(map[string]any{"argv": []string{"sh", "-c", `printf %s "$ORRERY_URL"`}}); orreryURL == "" {
		t.Fatal("ORRERY_URL is empty; ORRERY_ACCESS_URL did not reach the Exec")
	}
	if out := run(map[string]any{"argv": []string{"sh", "-c", `test -x /.orrery/bin/orrery && echo ok`}}); strings.TrimSpace(out) != "ok" {
		t.Fatal("the external CLI is not staged in /.orrery/bin")
	}
	if path := run(map[string]any{"argv": []string{"sh", "-c", `printf %s "$PATH"`}}); !strings.HasSuffix(path, ":/.orrery/bin") && path != "/.orrery/bin" {
		t.Fatalf("PATH = %q, want /.orrery/bin appended", path)
	}

	installGit(t, admin, sandboxID)
	run(map[string]any{"argv": []string{"git", "--version"}})
	got := run(map[string]any{
		"argv": []string{"git", "config", "x.y"},
		"env":  map[string]string{"GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "x.y", "GIT_CONFIG_VALUE_0": "z"},
	})
	if strings.TrimSpace(got) != "z" {
		t.Fatalf("git config x.y = %q, want the caller's z", got)
	}
}
