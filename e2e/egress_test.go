//go:build e2e

package e2e

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// recommendedDeny is defaults.md's recommended Egress Rules deny list.
var recommendedDeny = []string{"169.254.169.254/32", "10.213.0.0/16", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"}

type egressView struct {
	Mode  string   `json:"mode"`
	Allow []string `json:"allow"`
}

// TestEgressRules is #164's and #165's scenarios on one containerd Node: the
// registration probe proves egress_policy; with no Rules, a caller's none and
// allowlist Modes narrow the open deployment and are reapplied on restart;
// then Rules reach Sandboxes created or started after a Control Plane restart,
// deny drops, allow overrides deny and applies to Mode none too, and two
// Sandboxes on one Node cannot reach each other under the recommended Rules.
func TestEgressRules(t *testing.T) {
	if composeArgs() == nil || os.Getenv("ORRERY_CONFIG_DIR") == "" {
		t.Skip("needs the Compose stack and its config.yaml (scripts/e2e.sh)")
	}
	admin := adminToken(t)
	raiseSandboxQuota(t, admin, 10)
	suffix := time.Now().UnixNano()
	node, agent := startAgentNode(t, admin, fmt.Sprintf("e2e-egress-%d", suffix))
	if got := capabilityValues(node)["egress_policy"]; got != "enforced" {
		t.Fatalf("egress_policy = %q, want enforced (capabilities %v)", got, node.Capabilities)
	}
	sandbox := func(name string, egress map[string]any) string {
		body := map[string]any{"name": fmt.Sprintf("%s-%d", name, suffix), "cpu": 0.5, "memory": 64 << 20, "idle_stop": 0}
		if egress != nil {
			body["egress"] = egress
		}
		id := createReadySandboxWith(t, admin, body)
		t.Cleanup(func() { _, _ = call(t, http.MethodDelete, "/api/v1/sandboxes/"+id, admin, nil) })
		return id
	}

	a := sandbox("egress-a", nil)
	b := sandbox("egress-b", nil)
	assertEgress(t, admin, a, egressView{Mode: "open"})
	listen(t, admin, a)
	listen(t, admin, b)
	addr := sandboxAddress(t, admin, a)
	// No Rules: the e2e deployment is fully open, so only a Mode narrows.
	assertReach(t, admin, b, addr, 28001, true)

	none := sandbox("egress-none", map[string]any{"mode": "none"})
	assertEgress(t, admin, none, egressView{Mode: "none"})
	assertReach(t, admin, none, addr, 28001, false)

	allow := []string{addr + ":28001"}
	listed := sandbox("egress-allowlist", map[string]any{"mode": "allowlist", "allow": allow})
	assertEgress(t, admin, listed, egressView{Mode: "allowlist", Allow: allow})
	assertReach(t, admin, listed, addr, 28001, true)
	assertReach(t, admin, listed, addr, 28002, false)

	// The Mode is stored and sent again on every start.
	restartSandbox(t, admin, listed)
	assertEgress(t, admin, listed, egressView{Mode: "allowlist", Allow: allow})
	assertReach(t, admin, listed, addr, 28001, true)
	assertReach(t, admin, listed, addr, 28002, false)

	setEgressRules(t, agent, nil, recommendedDeny)
	// A running Sandbox keeps the Policy it was created with.
	assertReach(t, admin, b, addr, 28001, true)
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
	setEgressRules(t, agent, []string{"10.213.0.0/16:28001"}, recommendedDeny)
	restartSandbox(t, admin, b)
	assertReach(t, admin, b, addrA, 28001, true)
	assertReach(t, admin, b, addrA, 28002, false)
	// Rules allow applies to none too (runtime.md §13).
	restartSandbox(t, admin, none)
	assertEgress(t, admin, none, egressView{Mode: "none"})
	assertReach(t, admin, none, addrA, 28001, true)
	assertReach(t, admin, none, addrA, 28002, false)
}

// setEgressRules rewrites the e2e config.yaml's egress section and restarts
// the Control Plane: Rules change only with a restart (node-protocol.md
// HelloAck). It returns once agent has reported Runtime Capabilities on its new
// stream, so admission places on its Node again. Cleanup restores the file.
func setEgressRules(t *testing.T, agent *agentProcess, allow, deny []string) {
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
	before := agent.capabilityReports()
	restartControlPlane(t)
	agent.waitForCapabilityReport(t, before)
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
	// Denied traffic is dropped, so a refusal waits out the whole timeout.
	timeout := "3"
	if !want {
		timeout = "1"
	}
	e := createExec(t, admin, from, map[string]any{"argv": []string{"nc", "-z", "-w", timeout, addr, fmt.Sprint(port)}})
	result := waitExecFinished(t, admin, e.ID, 30*time.Second).Result
	got := result != nil && result.ExitCode != nil && *result.ExitCode == 0
	if got != want {
		t.Fatalf("Sandbox %s reaching %s:%d = %v, want %v (result %+v)", from, addr, port, got, want, result)
	}
}

func assertEgress(t *testing.T, admin, id string, want egressView) {
	t.Helper()
	status, data := call(t, http.MethodGet, "/api/v1/sandboxes/"+id, admin, nil)
	var sb struct {
		Egress egressView `json:"egress"`
	}
	mustJSON(t, data, &sb)
	if status != http.StatusOK || sb.Egress.Mode != want.Mode || !slices.Equal(sb.Egress.Allow, want.Allow) {
		t.Fatalf("Sandbox %s egress = %d %s, want %+v", id, status, data, want)
	}
}
