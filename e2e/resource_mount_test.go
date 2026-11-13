//go:build e2e

package e2e

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
// time. Two versions that share a file share one cached block.
func TestResourceUploadMount(t *testing.T) {
	admin := adminToken(t)
	raiseSandboxQuota(t, admin, 100)
	suffix := time.Now().UnixNano()
	nodeName := fmt.Sprintf("e2e-resource-node-%d", suffix)
	state, dataRoot := enrollAgent(t, admin, nodeName)
	startAgent(t, state, dataRoot)
	waitForNodeReady(t, admin, nodeName)

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

	// A second Sandbox pinned to v2 mounts the other version. The file both
	// versions share is one cached block (runtime.md §11).
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

	if got, _, code := execInSandbox(t, admin, sb2.ID, "cat", "/resources/data/nested/data.bin"); code != 0 || got != string(nested) {
		t.Fatalf("v2 nested file = %q code %d", got, code)
	}
	if mode, _, code := execInSandbox(t, admin, sb2.ID, "stat", "-c", "%a", "/resources/data/bin/run.sh"); code != 0 || strings.TrimSpace(mode) != "555" {
		t.Fatalf("run.sh mode = %q code %d, want 555", mode, code)
	}
	sum := sha256.Sum256(nested)
	block := filepath.Join(dataRoot, "cache", "blocks", hex.EncodeToString(sum[:]))
	// The cache lives on the Node host (nodeShell).
	if out, err := nodeShell("test -f " + shellQuote(block)); err != nil {
		t.Fatalf("the shared file's block %s is not cached: %v\n%s", block, err, out)
	}
}

// TestObjectStoreOutage is the "object-store-outage" acceptance scenario
// (#86, ADR-0030): while RustFS is down a Sandbox with Resources cannot read
// its manifest and stays provisioning; if the store recovers within the retry
// window the Sandbox becomes ready, otherwise it ends
// failed/resource_fetch_failed.
func TestObjectStoreOutage(t *testing.T) {
	admin := adminToken(t)
	raiseSandboxQuota(t, admin, 100)
	suffix := time.Now().UnixNano()
	nodeName := fmt.Sprintf("e2e-outage-node-%d", suffix)
	state, dataRoot := enrollAgent(t, admin, nodeName)
	startAgent(t, state, dataRoot)
	waitForNodeReady(t, admin, nodeName)

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

	// The Sandbox waits while the Agent retries the mount.
	first := createResourceSandbox(t, admin, fmt.Sprintf("outage-recover-%d", suffix), name)
	defer deleteSandbox(t, admin, first.ID)
	time.Sleep(5 * time.Second)
	if sb := getSandbox(t, admin, first.ID); sb.State == "ready" || len(sb.Conditions) != 0 {
		t.Fatalf("during the outage: state %q conditions %v, want not ready and no conditions", sb.State, sb.Conditions)
	}
	composeService(t, "start", "rustfs")
	waitForSandboxState(t, admin, first.ID, "ready", 180*time.Second)

	// Kept down past the backoff, the Sandbox ends failed with the fetch
	// cause (defaults.md: 1, 2, 4, 8, 16 seconds). The failure needs a version
	// whose manifest the Node has not cached yet, so push a second Resource
	// while the store is still up.
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

func composeService(t *testing.T, action, service string) {
	t.Helper()
	if len(composeArgs()) == 0 {
		t.Fatal("ORRERY_E2E_COMPOSE_FILE/PROJECT are not set; run make e2e")
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

// TestUnavailableVersionRecovers is the api.md §3 recovery half of #86: an
// object that 404s when read marks its ResourceVersion unavailable; re-publishing the
// same content re-uploads the object, the publish validation re-checks every
// object of the affected version, and a complete version returns to ready
// without moving the current pointer.
func TestUnavailableVersionRecovers(t *testing.T) {
	admin := adminToken(t)
	raiseSandboxQuota(t, admin, 100)
	suffix := time.Now().UnixNano()
	nodeName := fmt.Sprintf("e2e-recover-node-%d", suffix)
	state, dataRoot := enrollAgent(t, admin, nodeName)
	startAgent(t, state, dataRoot)
	waitForNodeReady(t, admin, nodeName)

	status, data := call(t, http.MethodGet, "/api/v1/users/me", admin, nil)
	if status != http.StatusOK {
		t.Fatalf("GET users/me: %d %s", status, data)
	}
	var me struct {
		ID string `json:"id"`
	}
	mustJSON(t, data, &me)

	cli := newCLIRunner(t, admin)
	name := fmt.Sprintf("recover-%d", suffix)
	src := t.TempDir()
	content := "recover this version\n"
	writeTestFile(t, filepath.Join(src, "hello.txt"), content, 0o644)
	var v1 resourceVersionView
	mustJSON(t, cli.run(t, "", "resource", "push", src, name), &v1)
	if v1.State != "ready" {
		t.Fatalf("v1 = %+v, want ready", v1)
	}

	// Delete the CAS object behind the tree. Objects are fetched when read
	// (ADR-0030), so the Sandbox starts; the read's presign finds the 404,
	// the Control Plane marks the referenced version unavailable and the read
	// fails.
	sum := sha256.Sum256([]byte(content))
	digest := hex.EncodeToString(sum[:])
	store := e2eObjectStore(t)
	if err := store.DeleteCASObject(context.Background(), me.ID, digest); err != nil {
		t.Fatalf("delete CAS object: %v", err)
	}

	broken := createResourceSandbox(t, admin, fmt.Sprintf("recover-broken-%d", suffix), name)
	defer deleteSandbox(t, admin, broken.ID)
	waitForSandboxState(t, admin, broken.ID, "ready", 120*time.Second)
	got, stderr, code := execInSandbox(t, admin, broken.ID, "cat", "/resources/"+name+"/hello.txt")
	if code == 0 {
		t.Fatalf("cat of a deleted object = %q, want a failed read", got)
	}
	t.Logf("cat of a deleted object: exit %d, %s", code, stderr)
	waitForVersionState(t, admin, v1, "unavailable", 60*time.Second)

	// Re-pushing identical content uploads the missing object again. Its
	// successful validation revives the older version.
	var v2 resourceVersionView
	mustJSON(t, cli.run(t, "", "resource", "push", src, name), &v2)
	if v2.ID == v1.ID || v2.State != "ready" {
		t.Fatalf("v2 = %+v, want a new ready version", v2)
	}
	waitForVersionState(t, admin, v1, "ready", 60*time.Second)

	// The recovered version is mountable again.
	recovered := createVersionSandbox(t, admin, fmt.Sprintf("recover-ok-%d", suffix), name, v1.ID)
	defer deleteSandbox(t, admin, recovered.ID)
	waitForSandboxState(t, admin, recovered.ID, "ready", 120*time.Second)
	if got, _, code := execInSandbox(t, admin, recovered.ID, "cat", "/resources/data/hello.txt"); code != 0 || got != content {
		t.Fatalf("cat recovered mount = %q code %d, want %q", got, code, content)
	}
}

func createVersionSandbox(t *testing.T, admin, name, resource, version string) sandboxView {
	t.Helper()
	status, data := call(t, http.MethodPost, "/api/v1/sandboxes", admin, map[string]any{
		"name":      name,
		"cpu":       0.5,
		"memory":    128 << 20,
		"idle_stop": 0,
		"resources": []map[string]any{{"resource": resource, "version": version, "mount": "data"}},
	})
	if status != http.StatusCreated {
		t.Fatalf("create Sandbox with %s@%s: %d %s", resource, version, status, data)
	}
	var sb sandboxView
	mustJSON(t, data, &sb)
	return sb
}

func waitForVersionState(t *testing.T, admin string, version resourceVersionView, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		status, data := call(t, http.MethodGet,
			"/api/v1/resources/"+version.ResourceID+"/versions/"+version.ID, admin, nil)
		if status != http.StatusOK {
			t.Fatalf("get ResourceVersion: %d %s", status, data)
		}
		var current resourceVersionView
		mustJSON(t, data, &current)
		if current.State == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("ResourceVersion %s stayed %q, want %q", version.ID, current.State, want)
		}
		time.Sleep(250 * time.Millisecond)
	}
}
