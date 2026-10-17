//go:build e2e

package e2e

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"net/http"
	"os/exec"
	"testing"
	"time"

	"github.com/zibyn/stars-sandbox/internal/supervisor"
)

// readySandboxWithAgent is readyExecSandbox with the Agent process in hand.
func readySandboxWithAgent(t *testing.T, admin string) (sandboxID string, agent *agentProcess, state, dataRoot string) {
	t.Helper()
	suffix := time.Now().UnixNano()
	nodeName := fmt.Sprintf("e2e-restart-node-%d", suffix)
	state, dataRoot = enrollAgent(t, admin, nodeName)
	agent = startAgent(t, state, dataRoot)
	waitForNodeReady(t, admin, nodeName)

	sandboxID = createReadySandbox(t, admin, fmt.Sprintf("restart-%d", suffix), 128<<20)
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", "stars-sandbox-"+sandboxID).Run()
	})
	return sandboxID, agent, state, dataRoot
}

// numberedLines is an Exec that takes long enough to straddle a restart and
// makes a gap or a duplicate in the log visible.
func numberedLines(count int) map[string]any {
	return map[string]any{
		"argv": []string{"sh", "-c", fmt.Sprintf(
			"for i in $(seq 1 %d); do echo line-$i; sleep 0.1; done", count)},
	}
}

func expectedLines(count int) string {
	var want bytes.Buffer
	for i := 1; i <= count; i++ {
		fmt.Fprintf(&want, "line-%d\n", i)
	}
	return want.String()
}

func waitExecFinished(t *testing.T, admin, id string, timeout time.Duration) execView {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		e := getExec(t, admin, id)
		if e.State == "finished" {
			return e
		}
		if time.Now().After(deadline) {
			t.Fatalf("Exec %s never finished (state %q)", id, e.State)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// execStdout pages the stored output from offset 0. Frames are deduplicated by
// seq on the Control Plane, so an exact match proves no gap and no repeat.
func execStdout(t *testing.T, admin, id string) string {
	t.Helper()
	after := int64(0)
	var out bytes.Buffer
	for {
		status, data := call(t, http.MethodGet,
			fmt.Sprintf("/api/v1/execs/%s/output?after=%d", id, after), admin, nil)
		if status != http.StatusOK {
			t.Fatalf("exec output: %d %s", status, data)
		}
		var frames []execFrame
		mustJSON(t, data, &frames)
		if len(frames) == 0 {
			return out.String()
		}
		for _, f := range frames {
			raw := []byte(f.Data)
			if f.Encoding == "base64" {
				decoded, err := base64.StdEncoding.DecodeString(f.Data)
				if err != nil {
					t.Fatalf("decode base64 frame: %v", err)
				}
				raw = decoded
			}
			if f.Stream == "stdout" {
				out.Write(raw)
			}
			after = f.Seq + supervisor.HeaderSize + int64(len(raw))
		}
	}
}

func assertExitedWith(t *testing.T, e execView, cause string) {
	t.Helper()
	if e.Result == nil || e.Result.Cause != cause {
		t.Fatalf("Exec result = %+v, want cause %q", e.Result, cause)
	}
	if cause == "exited" && (e.Result.ExitCode == nil || *e.Result.ExitCode != 0) {
		t.Fatalf("Exec exit code = %v, want 0", e.Result.ExitCode)
	}
}

// TestAgentRestartSurvives is the non-PTY part of the "agent-restart-survives"
// acceptance scenario (#39 M1 item 8): the Agent is killed mid-Exec, comes back
// on the same data root, and the log is complete and free of repeats.
func TestAgentRestartSurvives(t *testing.T) {
	admin := adminToken(t)
	raiseSandboxQuota(t, admin, 10)
	sandboxID, agent, state, dataRoot := readySandboxWithAgent(t, admin)

	const count = 60
	e := createExec(t, admin, sandboxID, numberedLines(count))
	waitExecRunning(t, admin, e.ID, 30*time.Second)
	// Let the first frames reach the Control Plane so the restart exercises a
	// resume from stored_seq, not a replay from zero.
	time.Sleep(500 * time.Millisecond)

	agent.stop()
	startAgent(t, state, dataRoot)

	assertExitedWith(t, waitExecFinished(t, admin, e.ID, 60*time.Second), "exited")
	if got, want := execStdout(t, admin, e.ID), expectedLines(count); got != want {
		t.Fatalf("output after an Agent restart has gaps or repeats:\ngot  %q\nwant %q", got, want)
	}
}

// TestRuntimeLostStopsSandbox is the "runtime-lost" acceptance scenario (#39 M1
// item 9): a Runtime that dies ends its Execs and releases the Sandbox.
func TestRuntimeLostStopsSandbox(t *testing.T) {
	admin := adminToken(t)
	raiseSandboxQuota(t, admin, 10)
	sandboxID := readyExecSandbox(t, admin, 128<<20)

	e := createExec(t, admin, sandboxID, map[string]any{"argv": []string{"sleep", "300"}})
	waitExecRunning(t, admin, e.ID, 30*time.Second)

	if out, err := exec.Command("docker", "kill", "stars-sandbox-"+sandboxID).CombinedOutput(); err != nil {
		t.Fatalf("kill Sandbox container: %v\n%s", err, out)
	}
	assertExitedWith(t, waitExecFinished(t, admin, e.ID, 60*time.Second), "runtime_lost")

	deadline := time.Now().Add(60 * time.Second)
	for {
		status, data := call(t, http.MethodGet, "/api/v1/sandboxes/"+sandboxID, admin, nil)
		if status != http.StatusOK {
			t.Fatalf("get Sandbox: %d %s", status, data)
		}
		var sb sandboxView
		mustJSON(t, data, &sb)
		if sb.State == "stopped" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Sandbox state = %q, want stopped", sb.State)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if n := psqlCount(t, fmt.Sprintf(
		"SELECT count(*) FROM allocations WHERE sandbox_id = '%s'", sandboxID)); n != "0" {
		t.Fatalf("Allocation rows left after runtime loss: %s", n)
	}
}

// TestControlPlaneRestart is the "control-plane-restart" acceptance scenario
// (#39 M1 item 18): the Reconciler rebuilds everything from the tables and the
// Agent resumes the Exec from stored_seq.
func TestControlPlaneRestart(t *testing.T) {
	admin := adminToken(t)
	raiseSandboxQuota(t, admin, 10)
	sandboxID := readyExecSandbox(t, admin, 128<<20)

	const count = 60
	e := createExec(t, admin, sandboxID, numberedLines(count))
	waitExecRunning(t, admin, e.ID, 30*time.Second)
	time.Sleep(500 * time.Millisecond)

	args := append(composeArgs(), "restart", "server")
	if out, err := exec.Command("docker", args...).CombinedOutput(); err != nil {
		t.Fatalf("restart the Control Plane: %v\n%s", err, out)
	}
	deadline := time.Now().Add(120 * time.Second)
	for {
		// The connection is refused or reset until the new process listens.
		status, _, err := postJSON(http.MethodGet, "/api/v1/users", "", nil)
		if err == nil && status == http.StatusUnauthorized {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the Control Plane did not come back (last status %d, err %v)", status, err)
		}
		time.Sleep(200 * time.Millisecond)
	}

	assertExitedWith(t, waitExecFinished(t, admin, e.ID, 120*time.Second), "exited")
	if got, want := execStdout(t, admin, e.ID), expectedLines(count); got != want {
		t.Fatalf("output across a Control Plane restart has gaps or repeats:\ngot  %q\nwant %q", got, want)
	}
}
