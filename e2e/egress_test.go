//go:build e2e

package e2e

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// recommendedDeny is defaults.md's recommended Egress Rules deny list.
var recommendedDeny = []string{"169.254.169.254/32", "10.213.0.0/16", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"}

// TestEgressRules is #164's scenario on a containerd Node: the registration
// probe proves egress_policy, Rules reach Sandboxes created or started after a
// Control Plane restart, deny drops, allow overrides deny, and two Sandboxes
// on one Node cannot reach each other under the recommended Rules.
func TestEgressRules(t *testing.T) {
	if composeArgs() == nil || os.Getenv("ORRERY_CONFIG_DIR") == "" {
		t.Skip("needs the Compose stack and its config.yaml (scripts/e2e.sh)")
	}
	admin := adminToken(t)
	raiseSandboxQuota(t, admin, 10)
	suffix := time.Now().UnixNano()
	node := startAgentNode(t, admin, fmt.Sprintf("e2e-egress-%d", suffix))
	if got := capabilityValues(node)["egress_policy"]; got != "enforced" {
		t.Fatalf("egress_policy = %q, want enforced (capabilities %v)", got, node.Capabilities)
	}

	a := createReadySandbox(t, admin, fmt.Sprintf("egress-a-%d", suffix), 64<<20)
	b := createReadySandbox(t, admin, fmt.Sprintf("egress-b-%d", suffix), 64<<20)
	t.Cleanup(func() { // the later scenarios share the admin's Sandbox quota
		for _, id := range []string{a, b} {
			_, _ = call(t, http.MethodDelete, "/api/v1/sandboxes/"+id, admin, nil)
		}
	})
	listen(t, admin, a)
	listen(t, admin, b)
	// No Rules: the e2e deployment is fully open.
	assertReach(t, admin, b, sandboxAddress(t, admin, a), 28001, true)

	setEgressRules(t, nil, recommendedDeny)
	// A running Sandbox keeps the Policy it was created with.
	assertReach(t, admin, b, sandboxAddress(t, admin, a), 28001, true)
	restartSandbox(t, admin, a)
	restartSandbox(t, admin, b)
	listen(t, admin, a)
	listen(t, admin, b)
	addrA, addrB := sandboxAddress(t, admin, a), sandboxAddress(t, admin, b)
	for _, port := range []int{28001, 28002} {
		assertReach(t, admin, b, addrA, port, false)
		assertReach(t, admin, a, addrB, port, false)
	}
	// The listener itself is up: loopback never leaves the Sandbox.
	assertReach(t, admin, a, "127.0.0.1", 28001, true)

	// allow wins over deny, for just the port it names.
	setEgressRules(t, []string{"10.213.0.0/16:28001"}, recommendedDeny)
	restartSandbox(t, admin, b)
	assertReach(t, admin, b, addrA, 28001, true)
	assertReach(t, admin, b, addrA, 28002, false)
}

// setEgressRules rewrites the e2e config.yaml's egress section and restarts
// the Control Plane: Rules change only with a restart (node-protocol.md
// HelloAck). Cleanup restores the original file.
func setEgressRules(t *testing.T, allow, deny []string) {
	t.Helper()
	path := filepath.Join(os.Getenv("ORRERY_CONFIG_DIR"), "config.yaml")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	base, _, _ := strings.Cut(string(original), "\n# egress (TestEgressRules)\n")
	if base == string(original) { // the first call: restore the file as it was
		t.Cleanup(func() {
			if err := os.WriteFile(path, original, 0o644); err != nil {
				t.Errorf("restore config.yaml: %v", err)
			}
			restartControlPlane(t)
		})
	}
	quote := func(entries []string) string {
		quoted := make([]string, len(entries))
		for n, entry := range entries {
			quoted[n] = fmt.Sprintf("%q", entry)
		}
		return "[" + strings.Join(quoted, ", ") + "]"
	}
	section := fmt.Sprintf("\n# egress (TestEgressRules)\negress:\n  allow: %s\n  deny: %s\n", quote(allow), quote(deny))
	if err := os.WriteFile(path, []byte(base+section), 0o644); err != nil {
		t.Fatal(err)
	}
	restartControlPlane(t)
}

func restartControlPlane(t *testing.T) {
	t.Helper()
	args := append(composeArgs(), "restart", "server")
	if out, err := exec.Command("docker", args...).CombinedOutput(); err != nil {
		t.Fatalf("restart the Control Plane: %v\n%s", err, out)
	}
	deadline := time.Now().Add(120 * time.Second)
	for {
		status, _, err := postJSON(http.MethodGet, "/api/v1/users", "", nil)
		if err == nil && status == http.StatusUnauthorized {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the Control Plane did not come back (last status %d, err %v)", status, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// restartSandbox stops and starts a Sandbox; the start builds its network
// again with the Rules the Node holds now (runtime.md §13).
func restartSandbox(t *testing.T, admin, id string) {
	t.Helper()
	if status, data := call(t, http.MethodPost, "/api/v1/sandboxes/"+id+"/stop", admin, nil); status != http.StatusAccepted {
		t.Fatalf("stop: %d %s", status, data)
	}
	if status, data := call(t, http.MethodGet, "/api/v1/sandboxes/"+id+"?wait_for=stopped", admin, nil); status != http.StatusOK {
		t.Fatalf("wait_for=stopped: %d %s", status, data)
	}
	if status, data := call(t, http.MethodPost, "/api/v1/sandboxes/"+id+"/start", admin, nil); status != http.StatusAccepted {
		t.Fatalf("start: %d %s", status, data)
	}
	status, data := call(t, http.MethodGet, "/api/v1/sandboxes/"+id+"?wait_for=ready", admin, nil)
	var sb sandboxView
	mustJSON(t, data, &sb)
	if status != http.StatusOK || sb.State != "ready" {
		t.Fatalf("wait_for=ready after start: %d %s", status, data)
	}
}

// listen serves TCP ports 28001 and 28002 in the Sandbox for the rest of the
// test, and returns once both accept on loopback.
func listen(t *testing.T, admin, id string) {
	t.Helper()
	for _, port := range []int{28001, 28002} {
		createExec(t, admin, id, map[string]any{"argv": []string{"sh", "-c",
			fmt.Sprintf("while true; do nc -l -p %d </dev/null; done", port)}})
	}
	runExecScript(t, admin, id,
		"for i in $(seq 1 50); do nc -z 127.0.0.1 28001 && nc -z 127.0.0.1 28002 && exit 0; sleep 0.1; done; exit 1")
}

func sandboxAddress(t *testing.T, admin, id string) string {
	t.Helper()
	e := createExec(t, admin, id, map[string]any{"argv": []string{"sh", "-c",
		"ip -4 -o addr show eth0 | awk '{print $4}' | cut -d/ -f1"}})
	assertExitedWith(t, waitExecFinished(t, admin, e.ID, 30*time.Second), "exited")
	addr := strings.TrimSpace(execStdout(t, admin, e.ID))
	if addr == "" {
		t.Fatalf("Sandbox %s has no eth0 address", id)
	}
	return addr
}

// assertReach dials addr:port from inside the Sandbox.
func assertReach(t *testing.T, admin, from, addr string, port int, want bool) {
	t.Helper()
	e := createExec(t, admin, from, map[string]any{"argv": []string{"nc", "-z", "-w", "3", addr, fmt.Sprint(port)}})
	result := waitExecFinished(t, admin, e.ID, 30*time.Second).Result
	got := result != nil && result.ExitCode != nil && *result.ExitCode == 0
	if got != want {
		t.Fatalf("Sandbox %s reaching %s:%d = %v, want %v (result %+v)", from, addr, port, got, want, result)
	}
}
