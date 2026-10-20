//go:build e2e

package e2e

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestCacheEviction is the "cache-eviction" acceptance scenario (#90): a Node
// below its disk watermark evicts LRU across version trees, local images and
// BuildKit cache. Anything a Sandbox still references (including a stopped
// one) survives.
func TestCacheEviction(t *testing.T) {
	admin := adminToken(t)
	raiseSandboxQuota(t, admin, 20)
	suffix := time.Now().UnixNano()
	nodeName := fmt.Sprintf("e2e-evict-node-%d", suffix)
	state, dataRoot := enrollAgent(t, admin, nodeName)
	first := startAgent(t, state, dataRoot)
	waitForNodeReady(t, admin, nodeName)
	// Version trees are read-only by design; let TempDir cleanup remove them.
	t.Cleanup(func() { makeTreeRemovable(dataRoot) })

	cli := newCLIRunner(t, admin)
	name := fmt.Sprintf("evict-%d", suffix)
	shared := []byte("shared by two versions\n")

	// v1 and v2 share an object; v3 carries a unique one.
	v1Src := t.TempDir()
	writeTestFile(t, filepath.Join(v1Src, "shared.bin"), string(shared), 0o644)
	writeTestFile(t, filepath.Join(v1Src, "a.txt"), "version one\n", 0o644)
	var v1 resourceVersionView
	mustJSON(t, cli.run(t, "", "resource", "push", v1Src, name), &v1)
	if v1.State != "ready" || v1.Address == nil {
		t.Fatalf("v1 = %+v", v1)
	}

	// A live Sandbox pins v1.
	a := createResourceSandboxVersion(t, admin, fmt.Sprintf("evict-live-%d", suffix), name, v1.ID)
	defer deleteSandbox(t, admin, a.ID)
	waitForSandboxState(t, admin, a.ID, "ready", 120*time.Second)

	v2Src := t.TempDir()
	writeTestFile(t, filepath.Join(v2Src, "shared.bin"), string(shared), 0o644)
	writeTestFile(t, filepath.Join(v2Src, "b.txt"), "version two\n", 0o644)
	var v2 resourceVersionView
	mustJSON(t, cli.run(t, "", "resource", "push", v2Src, name), &v2)
	if v2.State != "ready" || v2.Address == nil {
		t.Fatalf("v2 = %+v", v2)
	}

	// A stopped Sandbox still pins v2 (runtime.md §11).
	b := createResourceSandboxVersion(t, admin, fmt.Sprintf("evict-stopped-%d", suffix), name, v2.ID)
	defer deleteSandbox(t, admin, b.ID)
	waitForSandboxState(t, admin, b.ID, "ready", 120*time.Second)
	if status, data := call(t, http.MethodPost, "/api/v1/sandboxes/"+b.ID+"/stop", admin, nil); status != http.StatusAccepted {
		t.Fatalf("stop pinned Sandbox: %d %s", status, data)
	}
	waitForSandboxState(t, admin, b.ID, "stopped", 60*time.Second)

	v3Src := t.TempDir()
	unique := []byte(fmt.Sprintf("unique to v3 %d\n", suffix))
	writeTestFile(t, filepath.Join(v3Src, "unique.bin"), string(unique), 0o644)
	var v3 resourceVersionView
	mustJSON(t, cli.run(t, "", "resource", "push", v3Src, name), &v3)
	if v3.State != "ready" || v3.Address == nil {
		t.Fatalf("v3 = %+v", v3)
	}

	// A deleted Sandbox leaves its cache unreferenced. The Agent keyed the pins
	// to the Sandbox's data-root record; with no "Sandbox deleted" command it
	// retains that record, so the test drops it the way an operator would.
	c := createResourceSandboxVersion(t, admin, fmt.Sprintf("evict-deleted-%d", suffix), name, v3.ID)
	waitForSandboxState(t, admin, c.ID, "ready", 120*time.Second)
	deleteSandbox(t, admin, c.ID)
	if err := os.Remove(filepath.Join(dataRoot, "sandboxes", c.ID, "pinned.json")); err != nil {
		t.Fatalf("drop the deleted Sandbox's pin record: %v", err)
	}

	// A local image from the platform registry that no Sandbox references, and
	// BuildKit cache from building it. Both are the Node's to reclaim.
	cacheBefore := buildCacheIDs(t)
	buildDir := t.TempDir()
	dockerfile := fmt.Sprintf("FROM alpine:3.22\nRUN echo evict-%d > /cache.txt\n", suffix)
	writeTestFile(t, filepath.Join(buildDir, "Dockerfile"), dockerfile, 0o644)
	tag := fmt.Sprintf("127.0.0.1:15000/e2e-evict:%d", suffix)
	if out, err := exec.Command("docker", "build", "--network=none", "-t", tag, buildDir).CombinedOutput(); err != nil {
		t.Fatalf("build eviction image: %v\n%s", err, out)
	}
	unreferencedImage := dockerInspect(t, tag, "{{.Id}}")
	t.Cleanup(func() { _ = exec.Command("docker", "rmi", "-f", tag).Run() })
	newCache := map[string]bool{}
	for id := range buildCacheIDs(t) {
		if !cacheBefore[id] {
			newCache[id] = true
		}
	}
	if len(newCache) == 0 {
		t.Fatal("the build added no BuildKit cache entries")
	}
	// The running Sandbox's image is pinned and must survive.
	pinnedImage := dockerInspect(t, "orrery-"+a.ID, "{{.Image}}")

	// Restart with a watermark no disk can satisfy: eviction keeps running and
	// the free-space check cannot stop it mid-pass. Pins live in the data root,
	// so the running and stopped Sandboxes stay protected across the restart.
	first.stop()
	startAgentWith(t, state, dataRoot, "--disk-watermark", "0.99")

	tree := func(version resourceVersionView) string {
		return filepath.Join(dataRoot, "cache", "versions", *version.Address)
	}
	sharedDigest := digestHex(shared)
	uniqueDigest := digestHex(unique)
	objectPath := func(digest string) string { return filepath.Join(dataRoot, "cache", "objects", digest) }

	deadline := time.Now().Add(180 * time.Second)
	for {
		if !pathExists(tree(v3)) && !imageExists(unreferencedImage) && !anyCacheEntry(t, newCache) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("eviction did not finish: tree3=%v image=%v cache=%v",
				pathExists(tree(v3)), imageExists(unreferencedImage), anyCacheEntry(t, newCache))
		}
		time.Sleep(time.Second)
	}

	// Unreferenced: v3's tree and its uniquely linked object are gone.
	if pathExists(tree(v3)) {
		t.Fatal("unreferenced version tree survived eviction")
	}
	if pathExists(objectPath(uniqueDigest)) {
		t.Fatal("unlinked object was not reclaimed with its last tree")
	}
	// Referenced: the running and stopped Sandboxes keep their trees and the
	// object both trees hard-link.
	for label, version := range map[string]resourceVersionView{"running": v1, "stopped": v2} {
		if !pathExists(tree(version)) {
			t.Fatalf("%s Sandbox's version tree was evicted", label)
		}
	}
	if !pathExists(objectPath(sharedDigest)) {
		t.Fatal("an object still linked by two trees was reclaimed")
	}
	// Images: the Node's unreferenced platform image is gone, the Sandbox
	// image stays, and a host-owned (Docker Hub) image is not the Node's to
	// evict.
	if imageExists(unreferencedImage) {
		t.Fatal("unreferenced local image survived eviction")
	}
	if !imageExists(pinnedImage) {
		t.Fatal("an image pinned by a running or stopped Sandbox was evicted")
	}
	if out, err := exec.Command("docker", "image", "inspect", "alpine:3.22").CombinedOutput(); err != nil {
		t.Fatalf("a host-owned image was evicted: %v\n%s", err, out)
	}

	// The suite shares one User quota; a Sandbox this test leaves behind would
	// starve later scenarios. The deferred deletes above run at return, but
	// assert they complete while the eviction Agent is still up.
	deleteSandbox(t, admin, a.ID)
	deleteSandbox(t, admin, b.ID)
	for _, id := range []string{a.ID, b.ID} {
		if status, data := call(t, http.MethodGet, "/api/v1/sandboxes/"+id, admin, nil); status != http.StatusNotFound {
			t.Fatalf("Sandbox %s survived cleanup: %d %s", id, status, data)
		}
	}
}

func createResourceSandboxVersion(t *testing.T, admin, name, resource, version string) sandboxView {
	t.Helper()
	status, data := call(t, http.MethodPost, "/api/v1/sandboxes", admin, map[string]any{
		"name":      name,
		"cpu":       0.5,
		"memory":    128 << 20,
		"idle_stop": 0,
		"resources": []map[string]any{{"resource": resource, "version": version, "mount": "data"}},
	})
	if status != http.StatusCreated {
		t.Fatalf("create Sandbox pinning %s: %d %s", version, status, data)
	}
	var sb sandboxView
	mustJSON(t, data, &sb)
	return sb
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func imageExists(id string) bool {
	return exec.Command("docker", "image", "inspect", id).Run() == nil
}

func dockerInspect(t *testing.T, ref, format string) string {
	t.Helper()
	out, err := exec.Command("docker", "inspect", "-f", format, ref).CombinedOutput()
	if err != nil {
		t.Fatalf("docker inspect %s: %v\n%s", ref, err, out)
	}
	return strings.TrimSpace(string(out))
}

func digestHex(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// buildCacheIDs is the set of BuildKit cache record ids. The Engine API's
// prune is all-or-nothing, so the test compares sets to isolate its entries.
func buildCacheIDs(t *testing.T) map[string]bool {
	t.Helper()
	out, err := exec.Command("docker", "buildx", "du", "--format", "{{json .}}").Output()
	if err != nil {
		t.Fatalf("docker buildx du: %v", err)
	}
	ids := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		var entry struct {
			ID string `json:"ID"`
		}
		if json.Unmarshal([]byte(line), &entry) == nil && entry.ID != "" {
			ids[entry.ID] = true
		}
	}
	return ids
}

func anyCacheEntry(t *testing.T, ids map[string]bool) bool {
	t.Helper()
	present := buildCacheIDs(t)
	for id := range ids {
		if present[id] {
			return true
		}
	}
	return false
}
