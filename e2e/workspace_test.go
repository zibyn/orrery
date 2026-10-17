//go:build e2e

package e2e

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type workspaceView struct {
	ID          string   `json:"id"`
	Kind        string   `json:"kind"`
	NodeID      *string  `json:"node_id"`
	State       string   `json:"state"`
	Cause       *string  `json:"cause"`
	Unavailable bool     `json:"unavailable"`
	Conditions  []string `json:"conditions"`
	SandboxID   *string  `json:"sandbox_id"`
}

func createWorkspace(t *testing.T, admin string) workspaceView {
	t.Helper()
	status, data := call(t, http.MethodPost, "/api/v1/workspaces", admin, nil)
	if status != http.StatusCreated {
		t.Fatalf("create Workspace: %d %s", status, data)
	}
	var ws workspaceView
	mustJSON(t, data, &ws)
	if ws.Kind != "persistent" || ws.State != "provisioning" {
		t.Fatalf("created Workspace = %+v", ws)
	}
	return ws
}

func getWorkspace(t *testing.T, admin, id string) workspaceView {
	t.Helper()
	status, data := call(t, http.MethodGet, "/api/v1/workspaces/"+id, admin, nil)
	if status != http.StatusOK {
		t.Fatalf("get Workspace: %d %s", status, data)
	}
	var ws workspaceView
	mustJSON(t, data, &ws)
	return ws
}

func waitForWorkspaceState(t *testing.T, admin, id, want string, timeout time.Duration) workspaceView {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		ws := getWorkspace(t, admin, id)
		if ws.State == want {
			return ws
		}
		if ws.State == "failed" {
			t.Fatalf("Workspace %s failed: cause %v", id, ws.Cause)
		}
		if time.Now().After(deadline) {
			t.Fatalf("Workspace %s never became %s (state %q)", id, want, ws.State)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

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

func createSandboxWithWorkspace(t *testing.T, admin, name, workspaceID, mode string, idleStop int32) (int, []byte) {
	t.Helper()
	return call(t, http.MethodPost, "/api/v1/sandboxes", admin, map[string]any{
		"name":      name,
		"cpu":       0.5,
		"memory":    128 << 20,
		"idle_stop": idleStop,
		"workspace": map[string]string{"id": workspaceID, "mode": mode},
	})
}

// TestIdleStopRestart is the "idle-stop-restart" acceptance scenario (#39 M1
// item 13, #59): an idle Sandbox stops by itself, and a persistent Workspace
// keeps its content across the restart.
func TestIdleStopRestart(t *testing.T) {
	admin := adminToken(t)
	raiseSandboxQuota(t, admin, 100)
	suffix := time.Now().UnixNano()
	nodeName := fmt.Sprintf("e2e-idle-node-%d", suffix)
	state, dataRoot := enrollAgent(t, admin, nodeName)
	startAgent(t, state, dataRoot)
	waitForNodeReady(t, admin, nodeName)

	ws := waitForWorkspaceState(t, admin, createWorkspace(t, admin).ID, "ready", 60*time.Second)
	if ws.NodeID == nil {
		t.Fatalf("ready Workspace has no node_id: %+v", ws)
	}

	status, data := createSandboxWithWorkspace(t, admin, fmt.Sprintf("idle-%d", suffix), ws.ID, "rw", 2)
	if status != http.StatusCreated {
		t.Fatalf("create Sandbox: %d %s", status, data)
	}
	var sb sandboxView
	mustJSON(t, data, &sb)
	if status, data = call(t, http.MethodGet, "/api/v1/sandboxes/"+sb.ID+"?wait_for=ready", admin, nil); status != http.StatusOK {
		t.Fatalf("wait_for=ready: %d %s", status, data)
	}
	mustJSON(t, data, &sb)
	if sb.State != "ready" {
		t.Fatalf("Sandbox state = %q", sb.State)
	}
	if sb.Workspace == nil || sb.Workspace.ID != ws.ID || sb.Workspace.Mode != "rw" {
		t.Fatalf("Sandbox workspace = %+v", sb.Workspace)
	}

	// The content that must survive the Idle Stop.
	content := []byte("persistent across idle stop\n")
	files := "/api/v1/sandboxes/" + sb.ID + "/files/content?path=" + url.QueryEscape("keep.txt")
	if status, data := fileCall(t, admin, http.MethodPut, files, content); status != http.StatusNoContent {
		t.Fatalf("upload: %d %s", status, data)
	}

	stopped := waitForSandboxState(t, admin, sb.ID, "stopped", 60*time.Second)
	if stopped.Cause != nil {
		t.Fatalf("Idle Stop cause = %v, want none", *stopped.Cause)
	}
	if containerRunning(sb.ID) {
		t.Fatal("the container still runs after the Idle Stop")
	}

	// Start again: the same Workspace, same content.
	if status, data := call(t, http.MethodPost, "/api/v1/sandboxes/"+sb.ID+"/start", admin, nil); status != http.StatusAccepted {
		t.Fatalf("start: %d %s", status, data)
	}
	restarted := waitForSandboxReady(t, admin, sb.ID)
	if restarted.Workspace == nil || restarted.Workspace.ID != ws.ID {
		t.Fatalf("restarted workspace = %+v", restarted.Workspace)
	}
	status, got := fileCall(t, admin, http.MethodGet, files, nil)
	if status != http.StatusOK || string(got) != string(content) {
		t.Fatalf("download after restart = %d %q", status, got)
	}

	// Deleting the Sandbox keeps a persistent Workspace.
	if status, data := call(t, http.MethodDelete, "/api/v1/sandboxes/"+sb.ID, admin, nil); status != http.StatusAccepted {
		t.Fatalf("delete: %d %s", status, data)
	}
	deadline := time.Now().Add(60 * time.Second)
	for psqlCount(t, fmt.Sprintf("SELECT count(*) FROM sandboxes WHERE id = '%s' AND state = 'deleted'", sb.ID)) != "1" {
		if time.Now().After(deadline) {
			t.Fatal("Sandbox was not deleted")
		}
		time.Sleep(500 * time.Millisecond)
	}
	kept := getWorkspace(t, admin, ws.ID)
	if kept.State != "ready" || kept.SandboxID != nil {
		t.Fatalf("persistent Workspace after Sandbox delete = %+v", kept)
	}
	if _, err := os.Stat(filepath.Join(dataRoot, "workspaces", ws.ID)); err != nil {
		t.Fatalf("persistent Workspace directory: %v", err)
	}
}

// TestWriteAttachmentConflict is the "write-attachment-conflict" acceptance
// scenario (#39 M1 item 14, #59): one read-write Attachment at a time, and
// stopping the holder releases it.
func TestWriteAttachmentConflict(t *testing.T) {
	admin := adminToken(t)
	raiseSandboxQuota(t, admin, 100)
	suffix := time.Now().UnixNano()
	nodeName := fmt.Sprintf("e2e-attach-node-%d", suffix)
	state, dataRoot := enrollAgent(t, admin, nodeName)
	startAgent(t, state, dataRoot)
	waitForNodeReady(t, admin, nodeName)

	ws := waitForWorkspaceState(t, admin, createWorkspace(t, admin).ID, "ready", 60*time.Second)

	status, data := createSandboxWithWorkspace(t, admin, fmt.Sprintf("writer-%d", suffix), ws.ID, "rw", 0)
	if status != http.StatusCreated {
		t.Fatalf("first Sandbox: %d %s", status, data)
	}
	var first sandboxView
	mustJSON(t, data, &first)
	waitForSandboxReady(t, admin, first.ID)

	// A second read-write Attachment is refused.
	status, data = createSandboxWithWorkspace(t, admin, fmt.Sprintf("second-%d", suffix), ws.ID, "rw", 0)
	if status != http.StatusConflict {
		t.Fatalf("second rw Sandbox: %d %s, want 409", status, data)
	}
	requireProblemCode(t, data, "conflict")

	// Read-only attachments are allowed alongside the writer.
	status, data = createSandboxWithWorkspace(t, admin, fmt.Sprintf("reader-%d", suffix), ws.ID, "ro", 0)
	if status != http.StatusCreated {
		t.Fatalf("ro Sandbox: %d %s", status, data)
	}
	var reader sandboxView
	mustJSON(t, data, &reader)
	waitForSandboxReady(t, admin, reader.ID)

	// Stopping the writer releases the single-writer slot.
	if status, data := call(t, http.MethodPost, "/api/v1/sandboxes/"+first.ID+"/stop", admin, nil); status != http.StatusAccepted {
		t.Fatalf("stop first: %d %s", status, data)
	}
	waitForSandboxState(t, admin, first.ID, "stopped", 60*time.Second)

	status, data = createSandboxWithWorkspace(t, admin, fmt.Sprintf("second-%d", suffix), ws.ID, "rw", 0)
	if status != http.StatusCreated {
		t.Fatalf("second rw Sandbox after stop: %d %s, want 201", status, data)
	}
	var second sandboxView
	mustJSON(t, data, &second)
	waitForSandboxReady(t, admin, second.ID)

	for _, id := range []string{first.ID, reader.ID, second.ID} {
		if status, data := call(t, http.MethodDelete, "/api/v1/sandboxes/"+id, admin, nil); status != http.StatusAccepted {
			t.Fatalf("cleanup delete %s: %d %s", id, status, data)
		}
	}
}
