//go:build e2e

package e2e

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"
)

func waitForSandboxState(t *testing.T, admin, id, want string, timeout time.Duration) sandboxView {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		status, data := call(t, http.MethodGet, "/api/v1/sandboxes/"+id, admin, nil)
		if status == http.StatusOK {
			var sb sandboxView
			mustJSON(t, data, &sb)
			if sb.State == want {
				return sb
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("Sandbox %s never became %s (%d %s)", id, want, status, data)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// createVolume creates a Volume and returns its id.
func createVolume(t *testing.T, admin, name string) string {
	t.Helper()
	status, data := call(t, http.MethodPost, "/api/v1/volumes", admin, map[string]any{"name": name})
	if status != http.StatusCreated {
		t.Fatalf("create Volume: %d %s", status, data)
	}
	var volume struct {
		ID string `json:"id"`
	}
	mustJSON(t, data, &volume)
	return volume.ID
}

// TestIdleStopRestart is the "idle-stop-restart" acceptance scenario (#39 M1
// item 13, #59): an idle Sandbox stops by itself, and its filesystem keeps
// its content across the restart (ADR-0031).
func TestIdleStopRestart(t *testing.T) {
	admin := adminToken(t)
	raiseSandboxQuota(t, admin, 100)
	suffix := time.Now().UnixNano()
	nodeName := fmt.Sprintf("e2e-idle-node-%d", suffix)
	state, dataRoot := enrollAgent(t, admin, nodeName)
	startAgent(t, state, dataRoot)
	waitForNodeReady(t, admin, nodeName)

	id := createReadySandboxWith(t, admin, map[string]any{
		"name": fmt.Sprintf("idle-%d", suffix), "cpu": 0.5, "memory": 128 << 20, "idle_stop": 2,
	})

	// The content that must survive the Idle Stop.
	content := []byte("persistent across idle stop\n")
	files := "/api/v1/sandboxes/" + id + "/files/content?path=" + url.QueryEscape("keep.txt")
	if status, data := fileCall(t, admin, http.MethodPut, files, content); status != http.StatusNoContent {
		t.Fatalf("upload: %d %s", status, data)
	}

	stopped := waitForSandboxState(t, admin, id, "stopped", 60*time.Second)
	if stopped.Cause != nil {
		t.Fatalf("Idle Stop cause = %v, want none", *stopped.Cause)
	}
	if containerRunning(id) {
		t.Fatal("the container still runs after the Idle Stop")
	}

	if status, data := call(t, http.MethodPost, "/api/v1/sandboxes/"+id+"/start", admin, nil); status != http.StatusAccepted {
		t.Fatalf("start: %d %s", status, data)
	}
	waitForSandboxReady(t, admin, id)
	status, got := fileCall(t, admin, http.MethodGet, files, nil)
	if status != http.StatusOK || string(got) != string(content) {
		t.Fatalf("download after restart = %d %q", status, got)
	}
	deleteSandbox(t, admin, id)
}
