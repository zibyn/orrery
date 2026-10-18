//go:build e2e

package e2e

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// TestNodeUnreachableLost is the "node-unreachable-lost" scenario (#39, #95):
// an unreachable Node is only unavailable and keeps its Sandbox and compute; an
// admin's declaration then fails the Sandbox with node_lost and releases the
// compute. A reachable Node is refused, and the declared-lost Agent's old
// identity is rejected and cleared.
func TestNodeUnreachableLost(t *testing.T) {
	admin := adminToken(t)
	raiseSandboxQuota(t, admin, 10)
	suffix := time.Now().UnixNano()
	nodeName := fmt.Sprintf("e2e-lost-node-%d", suffix)

	state, dataRoot := enrollAgent(t, admin, nodeName)
	agent := startAgentWith(t, state, dataRoot, "--runtime", "docker")
	waitForNodeReady(t, admin, nodeName)
	node := findNode(t, admin, nodeName)

	sb := createSandbox(t, admin, map[string]any{
		"name": fmt.Sprintf("lost-%d", suffix), "cpu": 0.5, "memory": 128 << 20,
	})
	sb = waitForSandboxReady(t, admin, sb.ID)
	if sb.NodeID == nil || *sb.NodeID != node.ID {
		t.Fatalf("Sandbox node_id = %v, want %s", sb.NodeID, node.ID)
	}
	if n := psqlCount(t, fmt.Sprintf(
		"SELECT count(*) FROM allocations WHERE sandbox_id = '%s'", sb.ID)); n != "1" {
		t.Fatalf("allocations before loss = %s, want 1", n)
	}

	// A reachable Node cannot be declared lost, even though the Agent is well.
	if status, data := call(t, http.MethodPost,
		"/api/v1/nodes/"+node.ID+"/declare-lost", admin, nil); status != http.StatusConflict {
		t.Fatalf("declare a reachable Node lost: %d %s, want 409", status, data)
	}

	// Simulating an unreachable Node: the Agent stops. Reachability is
	// liveness, so the Node first becomes unavailable and nothing is released.
	agent.stop()
	deadline := time.Now().Add(60 * time.Second)
	for {
		current := getNode(t, admin, node.ID)
		if current.Status == "unavailable" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Node %s stayed %q after its Agent stopped", node.ID, current.Status)
		}
		time.Sleep(500 * time.Millisecond)
	}
	if got := getSandbox(t, admin, sb.ID); got.State != "ready" {
		t.Fatalf("Sandbox state while the Node is unreachable = %q, want ready", got.State)
	}
	if n := psqlCount(t, fmt.Sprintf(
		"SELECT count(*) FROM allocations WHERE sandbox_id = '%s'", sb.ID)); n != "1" {
		t.Fatalf("allocations while the Node is unreachable = %s, want 1", n)
	}

	// The declaration is one irreversible cascade.
	if status, data := call(t, http.MethodPost,
		"/api/v1/nodes/"+node.ID+"/declare-lost", admin, nil); status != http.StatusOK {
		t.Fatalf("declare Node lost: %d %s", status, data)
	}
	if got := getNode(t, admin, node.ID); got.Status != "lost" {
		t.Fatalf("declared Node status = %q, want lost", got.Status)
	}
	lost := getSandbox(t, admin, sb.ID)
	if lost.State != "failed" || lost.Cause == nil || *lost.Cause != "node_lost" {
		t.Fatalf("Sandbox after Node loss = %s/%v, want failed/node_lost", lost.State, lost.Cause)
	}
	if n := psqlCount(t, fmt.Sprintf(
		"SELECT count(*) FROM allocations WHERE sandbox_id = '%s'", sb.ID)); n != "0" {
		t.Fatalf("allocations after Node loss = %s, want 0", n)
	}
	if n := psqlCount(t, fmt.Sprintf(
		"SELECT count(*) FROM attachments WHERE sandbox_id = '%s'", sb.ID)); n != "0" {
		t.Fatalf("Attachments after Node loss = %s, want 0", n)
	}

	// The revoked identity is rejected and the Agent clears it.
	restarted := runAgentCommand(t, "--ca", nodeCA(), "--state", state,
		"--data-root", dataRoot, "--runtime", "docker")
	out, err := restarted.CombinedOutput()
	if err == nil {
		t.Fatalf("the declared-lost Agent reconnected with its old identity:\n%s", out)
	}
	if !strings.Contains(string(out), "identity rejected") {
		t.Fatalf("Agent exit does not report a rejected identity:\n%s", out)
	}
	if _, statErr := os.Stat(state); !os.IsNotExist(statErr) {
		t.Fatalf("the rejected identity was not cleared (stat err = %v)", statErr)
	}
}
