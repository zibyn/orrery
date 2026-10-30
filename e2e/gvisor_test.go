//go:build e2e

package e2e

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// TestGvisorParity is #160's gVisor parity scenario (#168): a containerd Node under --isolation gvisor reports
// kernel_isolation enforced and GPU unsupported, and exec-log-stream,
// agent-restart-survives and an Egress Rules scenario pass on it. It needs a
// containerd host with runsc and its shim on PATH; the e2e harness runs every
// Agent there over SSH:
//
//	ORRERY_E2E_ISOLATION=gvisor make e2e -run GvisorParity
func TestGvisorParity(t *testing.T) {
	if os.Getenv("ORRERY_E2E_ISOLATION") != "gvisor" {
		t.Skip("needs a gvisor containerd Node (ORRERY_E2E_ISOLATION=gvisor)")
	}
	admin := adminToken(t)
	raiseSandboxQuota(t, admin, 10)
	suffix := time.Now().UnixNano()
	nodeName := fmt.Sprintf("e2e-gvisor-%d", suffix)
	state, dataRoot := enrollAgent(t, admin, nodeName)
	agent := startAgent(t, state, dataRoot)
	waitForNodeReady(t, admin, nodeName)
	node := findNode(t, admin, nodeName)
	parent := t

	values := capabilityValues(node)
	for name, want := range map[string]string{
		"kernel_isolation":      "enforced",
		"gpu_device_visibility": "unsupported",
		"egress_policy":         "enforced",
		"cpu_memory_pid_limits": "enforced",
		"exec_supervisor":       "enforced",
		"workspace_rw_mount":    "enforced",
	} {
		if values[name] != want {
			t.Fatalf("%s = %q, want %q (capabilities %v)", name, values[name], want, node.Capabilities)
		}
	}

	t.Run("tmpfs-data-root-refused", func(t *testing.T) {
		root := fmt.Sprintf("/dev/shm/orrery-e2e-gvisor-%d", suffix)
		t.Cleanup(func() { _, _ = nodeShell("rm -rf " + root) })
		out, err := runAgentCommand(t, "--ca", nodeCA(), "--state", state, "--data-root", root).CombinedOutput()
		if err == nil || !strings.Contains(string(out), "tmpfs") {
			t.Fatalf("an Agent with a tmpfs data root = %v, want a refusal naming tmpfs:\n%s", err, out)
		}
	})

	t.Run("runs-under-gvisor", func(t *testing.T) {
		id := createReadySandbox(t, admin, fmt.Sprintf("gvisor-kernel-%d", suffix), 128<<20)
		t.Cleanup(func() { deleteSandbox(t, admin, id) })
		e := createExec(t, admin, id, map[string]any{"argv": []string{"dmesg"}})
		assertExitedWith(t, waitExecFinished(t, admin, e.ID, 30*time.Second), "exited")
		if out := execStdout(t, admin, e.ID); !strings.Contains(out, "gVisor") {
			t.Fatalf("the Sandbox kernel log does not come from gVisor:\n%s", out)
		}
	})

	t.Run("exec-log-stream", func(t *testing.T) {
		id := createReadySandbox(t, admin, fmt.Sprintf("gvisor-exec-%d", suffix), 128<<20)
		t.Cleanup(func() { deleteSandbox(t, admin, id) })
		e := createExec(t, admin, id, map[string]any{
			"argv": []string{"sh", "-c", "printf 'hello '; printf 'oops' >&2; printf '\\377'; exit 7"},
		})
		stdout, stderr, _, result := followExec(t, admin, e.ID, 30*time.Second)
		if want := []byte{'h', 'e', 'l', 'l', 'o', ' ', 0xff}; !bytes.Equal(stdout, want) {
			t.Fatalf("stdout = %q, want %q", stdout, want)
		}
		if string(stderr) != "oops" {
			t.Fatalf("stderr = %q, want %q", stderr, "oops")
		}
		if result.Cause != "exited" || result.ExitCode == nil || *result.ExitCode != 7 {
			t.Fatalf("result = %+v, want exited with code 7", result)
		}
	})

	t.Run("agent-restart-survives", func(t *testing.T) {
		id := createReadySandbox(t, admin, fmt.Sprintf("gvisor-restart-%d", suffix), 128<<20)
		t.Cleanup(func() { deleteSandbox(t, admin, id) })
		const count = 60
		e := createExec(t, admin, id, numberedLines(count))
		waitExecRunning(t, admin, e.ID, 30*time.Second)
		// Let the first frames reach the Control Plane so the restart resumes
		// from stored_seq instead of replaying from zero.
		time.Sleep(500 * time.Millisecond)

		agent.stop()
		agent = startAgent(parent, state, dataRoot) // lives on for egress-rules

		assertExitedWith(t, waitExecFinished(t, admin, e.ID, 60*time.Second), "exited")
		if got, want := execStdout(t, admin, e.ID), expectedLines(count); got != want {
			t.Fatalf("output after an Agent restart has gaps or repeats:\ngot  %q\nwant %q", got, want)
		}
	})

	// Last: it restarts the Control Plane. The host-side filter holds for a
	// runsc Sandbox: deny drops, and allow wins over deny for its one port.
	t.Run("egress-rules", func(t *testing.T) {
		if composeArgs() == nil || os.Getenv("ORRERY_CONFIG_DIR") == "" {
			t.Skip("needs the Compose stack and its config.yaml (scripts/e2e.sh)")
		}
		setEgressRules(t, []string{"10.213.0.0/16:28001"}, recommendedDeny)
		waitForNodeReady(t, admin, nodeName)
		a := createReadySandbox(t, admin, fmt.Sprintf("gvisor-egress-a-%d", suffix), 64<<20)
		b := createReadySandbox(t, admin, fmt.Sprintf("gvisor-egress-b-%d", suffix), 64<<20)
		t.Cleanup(func() {
			for _, id := range []string{a, b} {
				_, _ = call(t, http.MethodDelete, "/api/v1/sandboxes/"+id, admin, nil)
			}
		})
		listen(t, admin, a)
		addrA := sandboxAddress(t, admin, a)
		assertReach(t, admin, b, addrA, 28001, true)
		assertReach(t, admin, b, addrA, 28002, false)
	})
}
