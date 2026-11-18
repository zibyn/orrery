//go:build e2e

package e2e

import (
	"fmt"
	"net/http"
	"testing"
	"time"
)

// TestVolumeLifecycleWithoutNode covers what a Volume does with no Node
// involved (ADR-0031): create, the single-holder rule, the empty file view of
// a never-committed Volume, a publish from it, and the refused delete.
func TestVolumeLifecycleWithoutNode(t *testing.T) {
	admin := adminToken(t)
	raiseSandboxQuota(t, admin, 10)
	name := fmt.Sprintf("e2e-volume-%d", time.Now().UnixNano())

	status, data := call(t, http.MethodPost, "/api/v1/volumes", admin, map[string]any{"name": name})
	if status != http.StatusCreated {
		t.Fatalf("create Volume: %d %s", status, data)
	}
	var volume struct {
		ID     string  `json:"id"`
		State  string  `json:"state"`
		HeldBy *string `json:"held_by"`
	}
	mustJSON(t, data, &volume)
	if volume.State != "ready" {
		t.Fatalf("created Volume state = %s, want ready", volume.State)
	}

	status, data = call(t, http.MethodGet, "/api/v1/volumes/"+volume.ID+"/files", admin, nil)
	var files struct {
		Items []any `json:"items"`
	}
	if mustJSON(t, data, &files); status != http.StatusOK || files.Items == nil || len(files.Items) != 0 {
		t.Fatalf("files of a never-committed Volume: %d %s, want an empty directory", status, data)
	}

	create := func() (int, []byte) {
		return call(t, http.MethodPost, "/api/v1/sandboxes", admin, map[string]any{
			"cpu": 0.1, "memory": 64 << 20,
			"volumes": []map[string]string{{"volume": name, "path": "/data"}},
		})
	}
	status, data = create()
	if status != http.StatusCreated {
		t.Fatalf("create holder: %d %s", status, data)
	}
	var holder struct {
		ID string `json:"id"`
	}
	mustJSON(t, data, &holder)
	t.Cleanup(func() { call(t, http.MethodDelete, "/api/v1/sandboxes/"+holder.ID, admin, nil) })

	status, data = create()
	if status != http.StatusConflict {
		t.Fatalf("second holder: %d %s, want 409", status, data)
	}
	requireProblemCode(t, data, "conflict")
	status, data = call(t, http.MethodDelete, "/api/v1/volumes/"+volume.ID, admin, nil)
	if status != http.StatusConflict {
		t.Fatalf("delete a held Volume: %d %s, want 409", status, data)
	}

	status, data = call(t, http.MethodPost, "/api/v1/volumes/"+volume.ID+"/publish", admin,
		map[string]string{"path": ".", "resource": name})
	if status != http.StatusAccepted {
		t.Fatalf("publish Volume: %d %s", status, data)
	}
	var version struct {
		ID         string `json:"id"`
		ResourceID string `json:"resource_id"`
		State      string `json:"state"`
	}
	mustJSON(t, data, &version)
	deadline := time.Now().Add(30 * time.Second)
	for version.State == "pending" && time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
		_, data = call(t, http.MethodGet, "/api/v1/resources/"+version.ResourceID+"/versions/"+version.ID, admin, nil)
		mustJSON(t, data, &version)
	}
	if version.State != "ready" {
		t.Fatalf("Volume publish ended %s, want ready", version.State)
	}
}
