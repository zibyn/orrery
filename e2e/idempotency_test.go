//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// withIdempotencyKey sends a request carrying an Idempotency Key.
func withIdempotencyKey(t *testing.T, method, path, token, key string, body any) (int, []byte) {
	t.Helper()
	resp, data := request(t, method, path, map[string]string{
		"Authorization":   "Bearer " + token,
		"Idempotency-Key": key,
	}, nil, body)
	return resp.StatusCode, data
}

func waitForSandboxGone(t *testing.T, admin, id string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		status, _ := call(t, http.MethodGet, "/api/v1/sandboxes/"+id, admin, nil)
		if status == http.StatusNotFound {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("Sandbox %s was not deleted", id)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// concurrentCreates sends one create body with one Idempotency-Key from
// several callers. They must converge on one row: exactly one 201 and the rest
// 200 with the same id.
func concurrentCreates(t *testing.T, path, token, key string, body any) string {
	t.Helper()
	const callers = 4
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		status int
		body   []byte
		err    error
	}
	outcomes := make(chan outcome, callers)
	for range callers {
		go func() {
			req, err := http.NewRequest(http.MethodPost, apiURL()+path, bytes.NewReader(payload))
			if err != nil {
				outcomes <- outcome{err: err}
				return
			}
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Idempotency-Key", key)
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				outcomes <- outcome{err: err}
				return
			}
			defer resp.Body.Close()
			data, err := io.ReadAll(resp.Body)
			outcomes <- outcome{status: resp.StatusCode, body: data, err: err}
		}()
	}
	ids := map[string]bool{}
	created := 0
	for range callers {
		result := <-outcomes
		if result.err != nil {
			t.Fatal(result.err)
		}
		var row struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(result.body, &row); err != nil {
			t.Fatalf("decode concurrent create %d: %v", result.status, err)
		}
		switch result.status {
		case http.StatusCreated:
			created++
		case http.StatusOK:
		default:
			t.Fatalf("concurrent create: %d %s", result.status, result.body)
		}
		if row.ID == "" {
			t.Fatalf("concurrent create returned no id: %d %s", result.status, result.body)
		}
		ids[row.ID] = true
	}
	if created != 1 || len(ids) != 1 {
		t.Fatalf("concurrent creates: %d created, ids %v; want one 201 and one id", created, ids)
	}
	for id := range ids {
		return id
	}
	return ""
}

// TestIdempotencyKeyAfterDelete covers the Idempotency Key rules shared by the
// create endpoints: the replay lookup ignores deleted rows (#122), concurrent
// calls with one key converge even when the name index answers first (#127),
// and Snapshots participate like every other resource (#130).
func TestIdempotencyKeyAfterDelete(t *testing.T) {
	admin := adminToken(t)
	raiseSandboxQuota(t, admin, 10)

	suffix := time.Now().UnixNano()
	nodeName := fmt.Sprintf("e2e-idempotency-node-%d", suffix)
	state, dataRoot := enrollAgent(t, admin, nodeName)
	startAgent(t, state, dataRoot)
	waitForNodeReady(t, admin, nodeName)

	// Resource: delete is a soft delete, so the replay is immediate.
	resourceName := fmt.Sprintf("replay-resource-%d", suffix)
	resourceKey := fmt.Sprintf("replay-resource-key-%d", suffix)
	status, data := withIdempotencyKey(t, http.MethodPost, "/api/v1/resources", admin, resourceKey,
		map[string]string{"name": resourceName})
	if status != http.StatusCreated {
		t.Fatalf("create Resource: %d %s", status, data)
	}
	var firstResource publishedResource
	mustJSON(t, data, &firstResource)
	if status, data := call(t, http.MethodDelete, "/api/v1/resources/"+firstResource.ID, admin, nil); status != http.StatusAccepted {
		t.Fatalf("delete Resource: %d %s", status, data)
	}
	status, data = withIdempotencyKey(t, http.MethodPost, "/api/v1/resources", admin, resourceKey,
		map[string]string{"name": resourceName})
	if status != http.StatusCreated {
		t.Fatalf("replay Resource key after delete: %d %s, want 201", status, data)
	}
	var secondResource publishedResource
	mustJSON(t, data, &secondResource)
	if secondResource.ID == firstResource.ID {
		t.Fatalf("replayed Resource reused the tombstone %s", firstResource.ID)
	}
	t.Cleanup(func() { _, _ = call(t, http.MethodDelete, "/api/v1/resources/"+secondResource.ID, admin, nil) })

	// Concurrent creates with one Idempotency Key converge on one row. The
	// shared name makes the losers race the name index too; they must replay
	// the winner whichever index answers first (#127).
	concurrentKey := fmt.Sprintf("concurrent-resource-key-%d", suffix)
	concurrentID := concurrentCreates(t, "/api/v1/resources", admin, concurrentKey,
		map[string]string{"name": fmt.Sprintf("concurrent-resource-%d", suffix)})
	t.Cleanup(func() { _, _ = call(t, http.MethodDelete, "/api/v1/resources/"+concurrentID, admin, nil) })

	// Snapshot (#130): a repeat before deletion replays the row; after it, the
	// same key creates a new Snapshot.
	snapshotName := fmt.Sprintf("replay-snapshot-%d", suffix)
	snapshotKey := fmt.Sprintf("replay-snapshot-key-%d", suffix)
	status, data = withIdempotencyKey(t, http.MethodPost, "/api/v1/snapshots", admin, snapshotKey,
		map[string]string{"name": snapshotName})
	if status != http.StatusCreated {
		t.Fatalf("create Snapshot: %d %s", status, data)
	}
	var firstSnapshot snapshotView
	mustJSON(t, data, &firstSnapshot)
	status, data = withIdempotencyKey(t, http.MethodPost, "/api/v1/snapshots", admin, snapshotKey,
		map[string]string{"name": snapshotName})
	if status != http.StatusOK {
		t.Fatalf("replay Snapshot key: %d %s, want 200", status, data)
	}
	var replayedSnapshot snapshotView
	mustJSON(t, data, &replayedSnapshot)
	if replayedSnapshot.ID != firstSnapshot.ID {
		t.Fatalf("replayed Snapshot %s, want %s", replayedSnapshot.ID, firstSnapshot.ID)
	}
	if status, data := call(t, http.MethodDelete, "/api/v1/snapshots/"+firstSnapshot.ID, admin, nil); status != http.StatusAccepted {
		t.Fatalf("delete Snapshot: %d %s", status, data)
	}
	status, data = withIdempotencyKey(t, http.MethodPost, "/api/v1/snapshots", admin, snapshotKey,
		map[string]string{"name": snapshotName})
	if status != http.StatusCreated {
		t.Fatalf("replay Snapshot key after delete: %d %s, want 201", status, data)
	}
	var secondSnapshot snapshotView
	mustJSON(t, data, &secondSnapshot)
	if secondSnapshot.ID == firstSnapshot.ID {
		t.Fatalf("replayed Snapshot reused the tombstone %s", firstSnapshot.ID)
	}
	t.Cleanup(func() { _, _ = call(t, http.MethodDelete, "/api/v1/snapshots/"+secondSnapshot.ID, admin, nil) })

	// Concurrent same-key, same-name Snapshot creates converge on one row.
	concurrentSnapshotID := concurrentCreates(t, "/api/v1/snapshots", admin,
		fmt.Sprintf("concurrent-snapshot-key-%d", suffix),
		map[string]string{"name": fmt.Sprintf("concurrent-snapshot-%d", suffix)})
	t.Cleanup(func() {
		_, _ = call(t, http.MethodDelete, "/api/v1/snapshots/"+concurrentSnapshotID, admin, nil)
	})

	// An oversized key is rejected before the body is read.
	status, data = withIdempotencyKey(t, http.MethodPost, "/api/v1/snapshots", admin,
		strings.Repeat("k", 256), map[string]string{"name": fmt.Sprintf("oversized-snapshot-%d", suffix)})
	if status != http.StatusBadRequest {
		t.Fatalf("oversized Idempotency-Key: %d %s, want 400", status, data)
	}

	// Workspace: the tombstone reaches deleted once the Node removes storage.
	workspaceKey := fmt.Sprintf("replay-workspace-key-%d", suffix)
	status, data = withIdempotencyKey(t, http.MethodPost, "/api/v1/workspaces", admin, workspaceKey, nil)
	if status != http.StatusCreated {
		t.Fatalf("create Workspace: %d %s", status, data)
	}
	var firstWorkspace workspaceView
	mustJSON(t, data, &firstWorkspace)
	waitForWorkspaceState(t, admin, firstWorkspace.ID, "ready", 60*time.Second)
	if status, data := call(t, http.MethodDelete, "/api/v1/workspaces/"+firstWorkspace.ID, admin, nil); status != http.StatusAccepted {
		t.Fatalf("delete Workspace: %d %s", status, data)
	}
	waitForWorkspaceState(t, admin, firstWorkspace.ID, "deleted", 60*time.Second)
	status, data = withIdempotencyKey(t, http.MethodPost, "/api/v1/workspaces", admin, workspaceKey, nil)
	if status != http.StatusCreated {
		t.Fatalf("replay Workspace key after delete: %d %s, want 201", status, data)
	}
	var secondWorkspace workspaceView
	mustJSON(t, data, &secondWorkspace)
	if secondWorkspace.ID == firstWorkspace.ID {
		t.Fatalf("replayed Workspace reused the tombstone %s", firstWorkspace.ID)
	}
	t.Cleanup(func() { _, _ = call(t, http.MethodDelete, "/api/v1/workspaces/"+secondWorkspace.ID, admin, nil) })

	// Sandbox: the tombstone is visible to the lookup until the Agent reports
	// the Runtime gone, so the replay waits for that deletion.
	sandboxName := fmt.Sprintf("replay-sandbox-%d", suffix)
	sandboxKey := fmt.Sprintf("replay-sandbox-key-%d", suffix)
	createSandbox := map[string]any{
		"name":      sandboxName,
		"cpu":       0.5,
		"memory":    128 << 20,
		"idle_stop": 0,
	}
	status, data = withIdempotencyKey(t, http.MethodPost, "/api/v1/sandboxes", admin, sandboxKey, createSandbox)
	if status != http.StatusCreated {
		t.Fatalf("create Sandbox: %d %s", status, data)
	}
	var firstSandbox sandboxView
	mustJSON(t, data, &firstSandbox)
	waitForSandboxState(t, admin, firstSandbox.ID, "ready", 120*time.Second)
	if status, data := call(t, http.MethodDelete, "/api/v1/sandboxes/"+firstSandbox.ID, admin, nil); status != http.StatusAccepted {
		t.Fatalf("delete Sandbox: %d %s", status, data)
	}
	waitForSandboxGone(t, admin, firstSandbox.ID, 60*time.Second)
	status, data = withIdempotencyKey(t, http.MethodPost, "/api/v1/sandboxes", admin, sandboxKey, createSandbox)
	if status != http.StatusCreated {
		t.Fatalf("replay Sandbox key after delete: %d %s, want 201", status, data)
	}
	var secondSandbox sandboxView
	mustJSON(t, data, &secondSandbox)
	if secondSandbox.ID == firstSandbox.ID {
		t.Fatalf("replayed Sandbox reused the tombstone %s", firstSandbox.ID)
	}
	t.Cleanup(func() { _, _ = call(t, http.MethodDelete, "/api/v1/sandboxes/"+secondSandbox.ID, admin, nil) })
}
