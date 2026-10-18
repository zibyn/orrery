//go:build e2e

package e2e

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

type resourceVersionView struct {
	ID         string  `json:"id"`
	ResourceID string  `json:"resource_id"`
	Address    *string `json:"address"`
	State      string  `json:"state"`
}

// TestResourceUploadMount is the "resource-upload-mount" acceptance scenario
// (#86): a pushed directory is mounted read-only at /resources/<mount>, the
// content is byte-identical, writes fail, a later publish is invisible to the
// running Sandbox, and a restart still mounts the version pinned at create
// time. Two versions that share a file share one cached object.
func TestResourceUploadMount(t *testing.T) {
	admin := adminToken(t)
	raiseSandboxQuota(t, admin, 100)
	suffix := time.Now().UnixNano()
	nodeName := fmt.Sprintf("e2e-resource-node-%d", suffix)
	state, dataRoot := enrollAgent(t, admin, nodeName)
	startAgent(t, state, dataRoot)
	waitForNodeReady(t, admin, nodeName)
	// The version trees are read-only by design; let TempDir's cleanup remove
	// them.
	t.Cleanup(func() { makeTreeRemovable(dataRoot) })

	cli := newCLIRunner(t, admin)
	name := fmt.Sprintf("mounted-%d", suffix)
	src := t.TempDir()
	helloV1 := "pinned v1\n"
	writeTestFile(t, filepath.Join(src, "hello.txt"), helloV1, 0o644)
	nested := []byte("shared across versions\n")
	writeTestFile(t, filepath.Join(src, "nested", "data.bin"), string(nested), 0o644)
	writeTestFile(t, filepath.Join(src, "bin", "run.sh"), "#!/bin/sh\necho run\n", 0o755)

	var v1 resourceVersionView
	mustJSON(t, cli.run(t, "", "resource", "push", src, name), &v1)
	if v1.State != "ready" || v1.Address == nil {
		t.Fatalf("version v1 = %+v, want ready with an address", v1)
	}

	status, data := call(t, http.MethodPost, "/api/v1/sandboxes", admin, map[string]any{
		"name":      fmt.Sprintf("mount-%d", suffix),
		"cpu":       0.5,
		"memory":    128 << 20,
		"idle_stop": 0,
		"resources": []map[string]any{{"resource": name, "mount": "data"}},
	})
	if status != http.StatusCreated {
		t.Fatalf("create Sandbox: %d %s", status, data)
	}
	var sb sandboxView
	mustJSON(t, data, &sb)
	defer deleteSandbox(t, admin, sb.ID)

	sb = waitForSandboxState(t, admin, sb.ID, "ready", 120*time.Second)
	if len(sb.Conditions) != 0 {
		t.Fatalf("ready Sandbox conditions = %v, want none", sb.Conditions)
	}
	if got, _, code := execInSandbox(t, admin, sb.ID, "cat", "/resources/data/hello.txt"); code != 0 || got != helloV1 {
		t.Fatalf("cat mounted file = %q code %d, want %q", got, code, helloV1)
	}
	if got, _, code := execInSandbox(t, admin, sb.ID, "cat", "/resources/data/nested/data.bin"); code != 0 || got != string(nested) {
		t.Fatalf("cat nested file = %q code %d, want %q", got, code, nested)
	}

	// The mount is read-only: a write must fail.
	if _, stderr, code := execInSandbox(t, admin, sb.ID, "sh", "-c", "echo nope > /resources/data/new.txt"); code == 0 {
		t.Fatalf("write into the Resource mount succeeded (stderr %q)", stderr)
	}

	// Publishing a new version does not touch the running Sandbox: it pinned
	// v1 at create time.
	helloV2 := "pinned v2\n"
	writeTestFile(t, filepath.Join(src, "hello.txt"), helloV2, 0o644)
	var v2 resourceVersionView
	mustJSON(t, cli.run(t, "", "resource", "push", src, name), &v2)
	if v2.ID == v1.ID || v2.State != "ready" {
		t.Fatalf("version v2 = %+v, want a new ready version", v2)
	}
	if got, _, code := execInSandbox(t, admin, sb.ID, "cat", "/resources/data/hello.txt"); code != 0 || got != helloV1 {
		t.Fatalf("running Sandbox saw %q code %d, want the pinned %q", got, code, helloV1)
	}

	// Stop and start: the version pinned at create time is mounted again.
	if status, data := call(t, http.MethodPost, "/api/v1/sandboxes/"+sb.ID+"/stop", admin, nil); status != http.StatusAccepted {
		t.Fatalf("stop Sandbox: %d %s", status, data)
	}
	waitForSandboxState(t, admin, sb.ID, "stopped", 60*time.Second)
	if status, data := call(t, http.MethodPost, "/api/v1/sandboxes/"+sb.ID+"/start", admin, nil); status != http.StatusAccepted {
		t.Fatalf("start Sandbox: %d %s", status, data)
	}
	waitForSandboxState(t, admin, sb.ID, "ready", 120*time.Second)
	if got, _, code := execInSandbox(t, admin, sb.ID, "cat", "/resources/data/hello.txt"); code != 0 || got != helloV1 {
		t.Fatalf("restarted Sandbox saw %q code %d, want the pinned %q", got, code, helloV1)
	}

	// A second Sandbox pinned to v2 makes the Node cache both trees. The file
	// both versions share is one stored object, hard-linked into both trees.
	status, data = call(t, http.MethodPost, "/api/v1/sandboxes", admin, map[string]any{
		"name":      fmt.Sprintf("mount-v2-%d", suffix),
		"cpu":       0.5,
		"memory":    128 << 20,
		"idle_stop": 0,
		"resources": []map[string]any{{"resource": name, "version": v2.ID, "mount": "data"}},
	})
	if status != http.StatusCreated {
		t.Fatalf("create v2 Sandbox: %d %s", status, data)
	}
	var sb2 sandboxView
	mustJSON(t, data, &sb2)
	defer deleteSandbox(t, admin, sb2.ID)
	waitForSandboxState(t, admin, sb2.ID, "ready", 120*time.Second)
	if got, _, code := execInSandbox(t, admin, sb2.ID, "cat", "/resources/data/hello.txt"); code != 0 || got != helloV2 {
		t.Fatalf("v2 Sandbox saw %q code %d, want %q", got, code, helloV2)
	}

	sum := sha256.Sum256(nested)
	digest := hex.EncodeToString(sum[:])
	cacheObject := filepath.Join(dataRoot, "cache", "objects", digest)
	entries, err := os.ReadDir(cacheObject)
	if err != nil {
		t.Fatalf("read cache object directory %s: %v", cacheObject, err)
	}
	if len(entries) != 1 || entries[0].Name() != "data" {
		t.Fatalf("cache object directory entries = %v, want one plain object", entries)
	}
	firstCopy := filepath.Join(dataRoot, "cache", "versions", *v1.Address, "nested", "data.bin")
	secondCopy := filepath.Join(dataRoot, "cache", "versions", *v2.Address, "nested", "data.bin")
	firstInfo, err := os.Stat(firstCopy)
	if err != nil {
		t.Fatalf("stat %s: %v", firstCopy, err)
	}
	secondInfo, err := os.Stat(secondCopy)
	if err != nil {
		t.Fatalf("stat %s: %v", secondCopy, err)
	}
	if !os.SameFile(firstInfo, secondInfo) {
		t.Fatal("the file shared by both versions is not hard-linked")
	}
	if mode := firstInfo.Mode().Perm(); mode != 0o444 {
		t.Fatalf("cached file mode = %o, want 444", mode)
	}
}

// TestObjectStoreOutage is the "object-store-outage" acceptance scenario
// (#86): while RustFS is down a Sandbox with Resources reports
// fetching_resources; if the store recovers within the retry window the
// Sandbox becomes ready, otherwise it ends failed/resource_fetch_failed.
func TestObjectStoreOutage(t *testing.T) {
	admin := adminToken(t)
	raiseSandboxQuota(t, admin, 100)
	suffix := time.Now().UnixNano()
	nodeName := fmt.Sprintf("e2e-outage-node-%d", suffix)
	state, dataRoot := enrollAgent(t, admin, nodeName)
	startAgent(t, state, dataRoot)
	waitForNodeReady(t, admin, nodeName)
	t.Cleanup(func() { makeTreeRemovable(dataRoot) })

	// Warm the Node image with a plain Sandbox, then remove it. The outage
	// must not turn into an image pull failure.
	warmID := createReadySandbox(t, admin, fmt.Sprintf("warm-%d", suffix), 128<<20)
	deleteSandbox(t, admin, warmID)

	cli := newCLIRunner(t, admin)
	name := fmt.Sprintf("outage-%d", suffix)
	src := t.TempDir()
	writeTestFile(t, filepath.Join(src, "payload.txt"), "survives an outage\n", 0o644)
	var version resourceVersionView
	mustJSON(t, cli.run(t, "", "resource", "push", src, name), &version)

	composeService(t, "stop", "rustfs")
	t.Cleanup(func() { composeService(t, "start", "rustfs") })

	// The Sandbox reports the condition while the Agent retries.
	first := createResourceSandbox(t, admin, fmt.Sprintf("outage-recover-%d", suffix), name)
	defer deleteSandbox(t, admin, first.ID)
	waitForSandboxCondition(t, admin, first.ID, "fetching_resources", 60*time.Second)
	composeService(t, "start", "rustfs")
	waitForSandboxState(t, admin, first.ID, "ready", 180*time.Second)

	// Kept down past the backoff, the Sandbox ends failed with the fetch
	// cause (defaults.md: 1, 2, 4, 8, 16 seconds). The failure needs a version
	// the Node has not cached yet, so push a second Resource while the store
	// is still up.
	uncached := fmt.Sprintf("outage-uncached-%d", suffix)
	uncachedSrc := t.TempDir()
	writeTestFile(t, filepath.Join(uncachedSrc, "payload.txt"), "never fetched\n", 0o644)
	mustJSON(t, cli.run(t, "", "resource", "push", uncachedSrc, uncached), &version)
	composeService(t, "stop", "rustfs")
	second := createResourceSandbox(t, admin, fmt.Sprintf("outage-fail-%d", suffix), uncached)
	defer deleteSandbox(t, admin, second.ID)
	waitForFailed(t, admin, second.ID, "resource_fetch_failed")
}

func createResourceSandbox(t *testing.T, admin, name, resource string) sandboxView {
	t.Helper()
	status, data := call(t, http.MethodPost, "/api/v1/sandboxes", admin, map[string]any{
		"name":      name,
		"cpu":       0.5,
		"memory":    128 << 20,
		"idle_stop": 0,
		"resources": []map[string]any{{"resource": resource}},
	})
	if status != http.StatusCreated {
		t.Fatalf("create Sandbox with %s: %d %s", resource, status, data)
	}
	var sb sandboxView
	mustJSON(t, data, &sb)
	return sb
}

func waitForSandboxCondition(t *testing.T, admin, id, condition string, timeout time.Duration) sandboxView {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		sb := getSandbox(t, admin, id)
		for _, name := range sb.Conditions {
			if name == condition {
				return sb
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("Sandbox %s never reported %q (state %q, conditions %v)", id, condition, sb.State, sb.Conditions)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func composeService(t *testing.T, action, service string) {
	t.Helper()
	if len(composeArgs()) == 0 {
		t.Fatal("STARS_E2E_COMPOSE_FILE/PROJECT are not set; run make e2e")
	}
	args := append(composeArgs(), action, service)
	if out, err := exec.Command("docker", args...).CombinedOutput(); err != nil {
		t.Fatalf("docker compose %s %s: %v\n%s", action, service, err, out)
	}
}

func writeTestFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

// makeTreeRemovable restores write permission on the Agent's read-only cache
// trees so the test's TempDir cleanup can delete them.
func makeTreeRemovable(root string) {
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err == nil && entry.IsDir() {
			_ = os.Chmod(path, 0o755)
		}
		return nil
	})
}
