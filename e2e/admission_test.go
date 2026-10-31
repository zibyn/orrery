//go:build e2e

package e2e

import (
	"fmt"
	"net/http"
	"testing"
	"time"
)

// TestQuotaExceeded is the "quota-exceeded" acceptance scenario (#39 M1 item
// 5): a GPU request is refused immediately while the GPU quota is zero.
func TestQuotaExceeded(t *testing.T) {
	admin := adminToken(t)
	setSandboxQuota(t, admin, 10) // gpus: 0

	status, data := call(t, http.MethodPost, "/api/v1/sandboxes", admin, map[string]any{
		"cpu": 0.1, "memory": 1 << 20, "gpu": map[string]any{"count": 1},
	})
	if status != http.StatusTooManyRequests {
		t.Fatalf("create with a GPU over quota: %d %s, want 429", status, data)
	}
	requireProblemCode(t, data, "quota_exceeded")
}

// TestAdmissionQueueAndDeadline is the "queue-and-deadline" acceptance
// scenario (#39 M1 item 5): a CPU-saturated Node leaves a request pending with
// a queue position and a reason; its deadline ends it admission_timeout and
// releases the quota hold.
func TestAdmissionQueueAndDeadline(t *testing.T) {
	admin := adminToken(t)
	suffix := time.Now().UnixNano()
	nodeName := fmt.Sprintf("e2e-admission-node-%d", suffix)

	state, dataRoot := enrollAgent(t, admin, nodeName)
	// Zero CPU reservation lets one Sandbox hold every core the Agent reports.
	startAgentWith(t, state, dataRoot, "--reserve-cpu", "0")
	waitForNodeReady(t, admin, nodeName)

	node := findNode(t, admin, nodeName)
	cores, ok := node.Hardware["cpu_cores"].(float64)
	if !ok || cores <= 0 {
		t.Fatalf("Node hardware has no cpu_cores: %v", node.Hardware)
	}

	// The free slots left after whatever other tests left running: one for the
	// Sandbox that fills the Node, one for the request that has to wait.
	before := myQuota(t, admin)
	setSandboxQuota(t, admin, before.Sandboxes.Used+2)

	// On a GPU Node, GPU-less Sandboxes share only the quarter of the CPU the
	// per-GPU shares leave (ADR-0015).
	fill := cores
	if len(node.GPUs) > 0 {
		fill = cores / 4
	}
	holder := createSandbox(t, admin, map[string]any{
		"cpu": fill, "memory": 64 << 20, "idle_stop": 0,
	})
	t.Cleanup(func() { deleteSandbox(t, admin, holder.ID) })
	waitForAdmitted(t, admin, holder.ID, node.ID)
	// An admitted Sandbox is no longer queued, so the queue fields are absent.
	if admitted := getSandbox(t, admin, holder.ID); admitted.QueuePosition != nil || admitted.BlockedBy != nil {
		t.Fatalf("admitted Sandbox still reports queue_position=%v blocked_reason=%v",
			admitted.QueuePosition, admitted.BlockedBy)
	}

	waiting := createSandbox(t, admin, map[string]any{
		"cpu": 0.5, "memory": 64 << 20, "idle_stop": 0,
	})
	t.Cleanup(func() { deleteSandbox(t, admin, waiting.ID) })
	if waiting.State != "pending" {
		t.Fatalf("queued Sandbox state = %q, want pending", waiting.State)
	}
	blocked := getSandbox(t, admin, waiting.ID)
	if blocked.QueuePosition == nil || *blocked.QueuePosition != 1 {
		t.Fatalf("queue_position = %v, want 1", blocked.QueuePosition)
	}
	if blocked.BlockedBy == nil || *blocked.BlockedBy == "" {
		t.Fatal("a queued Sandbox has no blocked_reason")
	}

	// The queued Sandbox holds its quota slot: both slots are taken now, so a
	// third create is refused rather than queued.
	status, data := call(t, http.MethodPost, "/api/v1/sandboxes", admin, map[string]any{
		"cpu": 0.1, "memory": 1 << 20,
	})
	if status != http.StatusTooManyRequests {
		t.Fatalf("create over the held quota: %d %s, want 429", status, data)
	}
	requireProblemCode(t, data, "quota_exceeded")

	// The deployment's short admission deadline ends the request.
	waitForFailed(t, admin, waiting.ID, "admission_timeout")

	// The hold is released with the failure.
	if after := myQuota(t, admin); after.Sandboxes.Used != before.Sandboxes.Used+1 {
		t.Fatalf("active Sandboxes after the deadline = %d, want %d", after.Sandboxes.Used, before.Sandboxes.Used+1)
	}
	third := createSandbox(t, admin, map[string]any{"cpu": 0.1, "memory": 1 << 20})
	t.Cleanup(func() { deleteSandbox(t, admin, third.ID) })

	// A request no Node could ever fit ends unsatisfiable without waiting for
	// the deadline.
	setSandboxQuota(t, admin, before.Sandboxes.Used+3)
	oversized := createSandbox(t, admin, map[string]any{"cpu": cores + 1, "memory": 64 << 20})
	t.Cleanup(func() { deleteSandbox(t, admin, oversized.ID) })
	waitForFailed(t, admin, oversized.ID, "unsatisfiable")

	// The capacity view reports the Node and its status to every scope.
	status, data = call(t, http.MethodGet, "/api/v1/capacity", admin, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /capacity: %d %s", status, data)
	}
	var capacity struct {
		Items []struct {
			NodeID string `json:"node_id"`
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"items"`
	}
	mustJSON(t, data, &capacity)
	found := false
	for _, item := range capacity.Items {
		if item.NodeID == node.ID {
			found = true
			if item.Status != "ready" {
				t.Fatalf("capacity status = %q, want ready", item.Status)
			}
		}
	}
	if !found {
		t.Fatalf("capacity view has no Node %s", node.ID)
	}
}

// TestStartReAdmissionQuota covers api.md §2: starting a stopped Sandbox is a
// new admission, and exceeding the quota ends it failed/quota_exceeded.
func TestStartReAdmissionQuota(t *testing.T) {
	admin := adminToken(t)
	suffix := time.Now().UnixNano()
	nodeName := fmt.Sprintf("e2e-admission-restart-%d", suffix)
	state, dataRoot := enrollAgent(t, admin, nodeName)
	startAgent(t, state, dataRoot)
	waitForNodeReady(t, admin, nodeName)
	node := findNode(t, admin, nodeName)

	before := myQuota(t, admin)
	setSandboxQuota(t, admin, before.Sandboxes.Used+1)

	first := createSandbox(t, admin, map[string]any{"cpu": 0.1, "memory": 64 << 20, "idle_stop": 0})
	t.Cleanup(func() { deleteSandbox(t, admin, first.ID) })
	waitForAdmitted(t, admin, first.ID, node.ID)

	// Stopping releases the slot; another Sandbox takes it.
	if status, data := call(t, http.MethodPost, "/api/v1/sandboxes/"+first.ID+"/stop", admin, nil); status != http.StatusAccepted {
		t.Fatalf("stop: %d %s", status, data)
	}
	status, data := call(t, http.MethodGet, "/api/v1/sandboxes/"+first.ID+"?wait_for=stopped", admin, nil)
	if status != http.StatusOK {
		t.Fatalf("wait_for=stopped: %d %s", status, data)
	}
	second := createSandbox(t, admin, map[string]any{"cpu": 0.1, "memory": 64 << 20, "idle_stop": 0})
	t.Cleanup(func() { deleteSandbox(t, admin, second.ID) })

	// Start re-admits under the now-full quota.
	status, data = call(t, http.MethodPost, "/api/v1/sandboxes/"+first.ID+"/start", admin, nil)
	if status != http.StatusAccepted {
		t.Fatalf("start over quota: %d %s, want 202", status, data)
	}
	var restarted sandboxView
	mustJSON(t, data, &restarted)
	if restarted.State != "failed" || restarted.Cause == nil || *restarted.Cause != "quota_exceeded" {
		t.Fatalf("start over quota = state %q cause %v, want failed/quota_exceeded", restarted.State, restarted.Cause)
	}
}

type quotaUsage struct {
	GPUs struct {
		Limit int32 `json:"limit"`
		Used  int32 `json:"used"`
	} `json:"gpus"`
	Sandboxes struct {
		Limit int32 `json:"limit"`
		Used  int32 `json:"used"`
	} `json:"sandboxes"`
}

func myQuota(t *testing.T, admin string) quotaUsage {
	t.Helper()
	status, data := call(t, http.MethodGet, "/api/v1/users/me/quota", admin, nil)
	if status != http.StatusOK {
		t.Fatalf("GET quota: %d %s", status, data)
	}
	var usage quotaUsage
	mustJSON(t, data, &usage)
	return usage
}

// setSandboxQuota sets both quotas for the calling User; the admission tests
// keep the GPU quota at zero.
func setSandboxQuota(t *testing.T, admin string, sandboxes int32) {
	t.Helper()
	status, data := call(t, http.MethodGet, "/api/v1/users/me", admin, nil)
	if status != http.StatusOK {
		t.Fatalf("get me: %d %s", status, data)
	}
	var me struct {
		ID string `json:"id"`
	}
	mustJSON(t, data, &me)
	status, data = call(t, http.MethodPut, "/api/v1/users/"+me.ID+"/quota", admin, map[string]any{
		"gpus": 0, "sandboxes": sandboxes,
	})
	if status != http.StatusOK {
		t.Fatalf("set quota: %d %s", status, data)
	}
}

func createSandbox(t *testing.T, admin string, body map[string]any) sandboxView {
	t.Helper()
	status, data := call(t, http.MethodPost, "/api/v1/sandboxes", admin, body)
	if status != http.StatusCreated {
		t.Fatalf("create Sandbox %v: %d %s", body, status, data)
	}
	var sb sandboxView
	mustJSON(t, data, &sb)
	return sb
}

func getSandbox(t *testing.T, admin, id string) sandboxView {
	t.Helper()
	status, data := call(t, http.MethodGet, "/api/v1/sandboxes/"+id, admin, nil)
	if status != http.StatusOK {
		t.Fatalf("get Sandbox: %d %s", status, data)
	}
	var sb sandboxView
	mustJSON(t, data, &sb)
	return sb
}

// waitForAdmitted waits for the admission sweep to bind the Sandbox to nodeID.
func waitForAdmitted(t *testing.T, admin, id, nodeID string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		sb := getSandbox(t, admin, id)
		if sb.NodeID != nil {
			if *sb.NodeID != nodeID {
				t.Fatalf("Sandbox was admitted to Node %s, want %s", *sb.NodeID, nodeID)
			}
			return
		}
		if sb.State == "failed" {
			t.Fatalf("Sandbox failed during admission (cause %v)", sb.Cause)
		}
		if time.Now().After(deadline) {
			t.Fatalf("Sandbox was never admitted (state %q)", sb.State)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// waitForFailed polls until the Sandbox reaches failed with the given cause.
func waitForFailed(t *testing.T, admin, id, cause string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		current := getSandbox(t, admin, id)
		if current.State == "failed" {
			if current.Cause == nil || *current.Cause != cause {
				t.Fatalf("failed cause = %v, want %s", current.Cause, cause)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("Sandbox stayed %q instead of failing with %s", current.State, cause)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// deleteSandbox is best-effort cleanup: a failure must not mask the scenario's
// result. It waits briefly so the Agent removes the Runtime before the test
// stops it.
func deleteSandbox(t *testing.T, admin, id string) {
	t.Helper()
	_, _ = call(t, http.MethodDelete, "/api/v1/sandboxes/"+id, admin, nil)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		status, _ := call(t, http.MethodGet, "/api/v1/sandboxes/"+id, admin, nil)
		if status == http.StatusNotFound {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
}
