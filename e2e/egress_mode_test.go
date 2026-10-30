//go:build e2e

package e2e

import (
	"fmt"
	"net/http"
	"os"
	"slices"
	"testing"
	"time"
)

type egressView struct {
	Mode  string   `json:"mode"`
	Allow []string `json:"allow"`
}

// TestEgressMode is #165's scenario on a containerd Node: a caller's none and
// allowlist Modes narrow an otherwise open deployment, are stored on the
// Sandbox and reapplied on restart, and Rules allow still applies to none.
func TestEgressMode(t *testing.T) {
	if e2eRuntime() != "containerd" {
		t.Skip("Egress Modes are enforced only on containerd Nodes (ORRERY_E2E_RUNTIME=containerd)")
	}
	if composeArgs() == nil || os.Getenv("ORRERY_CONFIG_DIR") == "" {
		t.Skip("needs the Compose stack and its config.yaml (scripts/e2e.sh)")
	}
	admin := adminToken(t)
	raiseSandboxQuota(t, admin, 10)
	suffix := time.Now().UnixNano()
	node := startAgentNode(t, admin, fmt.Sprintf("e2e-egress-mode-%d", suffix))
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

	// No Rules: the e2e deployment is fully open, so only the Mode narrows.
	target := sandbox("egress-target", nil)
	assertEgress(t, admin, target, egressView{Mode: "open"})
	listen(t, admin, target)
	addr := sandboxAddress(t, admin, target)

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

	// Rules allow applies to none too (runtime.md §13).
	setEgressRules(t, []string{addr + ":28002"}, nil)
	restartSandbox(t, admin, none)
	assertEgress(t, admin, none, egressView{Mode: "none"})
	assertReach(t, admin, none, addr, 28002, true)
	assertReach(t, admin, none, addr, 28001, false)
}

// TestEgressModeUnsupported: without a Node that enforces egress_policy, a
// Mode other than open is refused at create, while open is accepted (api.md
// §3). It runs only where no such Node is enrolled, e.g. the Docker job.
func TestEgressModeUnsupported(t *testing.T) {
	admin := adminToken(t)
	status, data := call(t, http.MethodGet, "/api/v1/nodes?limit=100", admin, nil)
	if status != http.StatusOK {
		t.Fatalf("list Nodes: %d %s", status, data)
	}
	var list struct {
		Items []nodeView `json:"items"`
	}
	mustJSON(t, data, &list)
	if slices.ContainsFunc(list.Items, func(n nodeView) bool { return capabilityValues(n)["egress_policy"] == "enforced" }) {
		t.Skip("a Node enforces egress_policy")
	}
	for _, egress := range []map[string]any{
		{"mode": "none"},
		{"mode": "allowlist", "allow": []string{"10.0.0.1"}},
	} {
		status, data := call(t, http.MethodPost, "/api/v1/sandboxes", admin, map[string]any{
			"cpu": 0.5, "memory": 64 << 20, "egress": egress,
		})
		if status != http.StatusUnprocessableEntity {
			t.Fatalf("create with egress %v: %d %s, want 422", egress, status, data)
		}
		requireProblemCode(t, data, "unsupported")
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
