//go:build e2e

package e2e

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestCacheEviction is the "cache-eviction" acceptance scenario (#90, ADR-0030):
// a Node below its disk watermark evicts LRU across Resource blocks, local
// images and BuildKit cache. Blocks of a mounted version are evictable too and
// are fetched again when read; an image a Sandbox references survives.
func TestCacheEviction(t *testing.T) {
	admin := adminToken(t)
	raiseSandboxQuota(t, admin, 20)
	suffix := time.Now().UnixNano()
	nodeName := fmt.Sprintf("e2e-evict-node-%d", suffix)
	state, dataRoot := enrollAgent(t, admin, nodeName)
	first := startAgent(t, state, dataRoot)
	waitForNodeReady(t, admin, nodeName)

	cli := newCLIRunner(t, admin)
	name := fmt.Sprintf("evict-%d", suffix)
	content := fmt.Sprintf("read, evicted, read again %d\n", suffix)
	src := t.TempDir()
	writeTestFile(t, filepath.Join(src, "payload.txt"), content, 0o644)
	var v1 resourceVersionView
	mustJSON(t, cli.run(t, "", "resource", "push", src, name), &v1)
	if v1.State != "ready" || v1.Address == nil {
		t.Fatalf("v1 = %+v", v1)
	}

	// A running Sandbox mounts v1; reading the file caches its block.
	a := createResourceSandboxVersion(t, admin, fmt.Sprintf("evict-live-%d", suffix), name, v1.ID)
	defer deleteSandbox(t, admin, a.ID)
	waitForSandboxState(t, admin, a.ID, "ready", 120*time.Second)
	if got, _, code := execInSandbox(t, admin, a.ID, "cat", "/resources/data/payload.txt"); code != 0 || got != content {
		t.Fatalf("first read = %q code %d", got, code)
	}
	block := filepath.Join(dataRoot, "cache", "blocks", digestHex([]byte(content)))
	if !pathExists(block) {
		t.Fatalf("the read did not cache block %s", block)
	}

	// A local image from the platform registry that no Sandbox references, and
	// BuildKit cache from building it. Both are the Node's to reclaim.
	cacheBefore := buildCacheIDs(t)
	unreferencedImage := buildUnreferencedImage(t, suffix)
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
	pinnedImage := sandboxImage(t, a.ID)

	// Restart with a watermark no disk can satisfy: eviction keeps running and
	// the free-space check cannot stop it mid-pass. Image pins live in the data
	// root, so the running Sandbox stays protected across the restart.
	first.stop()
	startAgentWith(t, state, dataRoot, "--disk-watermark", "0.99")

	deadline := time.Now().Add(180 * time.Second)
	for {
		if !pathExists(block) && !imageExists(unreferencedImage) && !anyCacheEntry(t, newCache) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("eviction did not finish: block=%v image=%v cache=%v",
				pathExists(block), imageExists(unreferencedImage), anyCacheEntry(t, newCache))
		}
		time.Sleep(time.Second)
	}
	// The mounted version still reads (from the page cache or a re-fetch).
	if got, _, code := execInSandbox(t, admin, a.ID, "cat", "/resources/data/payload.txt"); code != 0 || got != content {
		t.Fatalf("read after eviction = %q code %d", got, code)
	}
	// Images: the Node's unreferenced platform image is gone, the Sandbox
	// image stays, and a host-owned (Docker Hub) image, the probe image on a
	// containerd Node, is not the Node's to evict.
	if !imageExists(pinnedImage) {
		t.Fatal("an image pinned by a running Sandbox was evicted")
	}
	hostImage := "docker.io/library/alpine:3.22"
	if !imageExists(hostImage) {
		t.Fatalf("a host-owned image %s was evicted", hostImage)
	}

	// The suite shares one User quota; a Sandbox this test leaves behind would
	// starve later scenarios. The deferred delete runs at return, but assert
	// it completes while the eviction Agent is still up.
	deleteSandbox(t, admin, a.ID)
	if status, data := call(t, http.MethodGet, "/api/v1/sandboxes/"+a.ID, admin, nil); status != http.StatusNotFound {
		t.Fatalf("Sandbox %s survived cleanup: %d %s", a.ID, status, data)
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

// pathExists looks on the Node host, where the Agent's data root is.
func pathExists(path string) bool {
	_, err := nodeShell("test -e " + shellQuote(path))
	return err == nil
}

// buildUnreferencedImage builds an image named in the platform registry into
// the Node's local image store, through the Node's BuildKit so the build also
// leaves BuildKit cache. It returns the key imageExists takes.
func buildUnreferencedImage(t *testing.T, suffix int64) string {
	t.Helper()
	tag := fmt.Sprintf("127.0.0.1:15000/e2e-evict:%d", suffix)
	// The Agent's containerd namespace gets the image by import: buildkitd
	// has no containerd image store of its own here. The import goes through
	// ctrCommand so it lands in the containerd the Agent uses.
	var ctr []string
	for _, w := range ctrCommand() {
		ctr = append(ctr, shellQuote(w))
	}
	script := fmt.Sprintf(`set -e
d=$(mktemp -d); trap 'rm -rf "$d"' EXIT
echo evict-%d > "$d/f"; printf 'FROM scratch\nCOPY f /f\n' > "$d/Dockerfile"
buildctl --addr %s build --frontend dockerfile.v0 --local context="$d" --local dockerfile="$d" \
  --output type=oci,name=%s,dest="$d/image.tar"
%s -n orrery images import "$d/image.tar"`, suffix, shellQuote(e2eBuildkit(t)), shellQuote(tag), strings.Join(ctr, " "))
	if out, err := nodeShell(script); err != nil {
		t.Fatalf("build eviction image: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = sandboxCommand("images", "rm", tag).Run() })
	// Without this the eviction assertion passes vacuously if the import
	// went to some other containerd.
	if !imageExists(tag) {
		t.Fatalf("imported %s is not in the Agent's containerd", tag)
	}
	return tag
}

func e2eBuildkit(t *testing.T) string {
	t.Helper()
	addr := os.Getenv("ORRERY_E2E_BUILDKIT")
	if addr == "" {
		t.Fatal("ORRERY_E2E_BUILDKIT is not set; a containerd Node builds only against a standalone buildkitd")
	}
	return addr
}

// imageExists takes a containerd image name.
func imageExists(id string) bool {
	out, err := sandboxCommand("images", "ls", "-q").Output()
	return err == nil && slices.Contains(strings.Fields(string(out)), id)
}

// sandboxImage is the image key of a Sandbox's Runtime, as imageExists takes.
func sandboxImage(t *testing.T, id string) string {
	t.Helper()
	out, err := sandboxCommand("containers", "info", "orrery-"+id).Output()
	var info struct {
		Image string `json:"Image"`
	}
	if err != nil || json.Unmarshal(out, &info) != nil || info.Image == "" {
		t.Fatalf("ctr containers info orrery-%s: %v\n%s", id, err, out)
	}
	return info.Image
}

func digestHex(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// buildCacheIDs is the set of BuildKit cache record ids. The prune is
// all-or-nothing, so the test compares sets to isolate its entries.
func buildCacheIDs(t *testing.T) map[string]bool {
	t.Helper()
	out, err := nodeShell("buildctl --addr " + shellQuote(e2eBuildkit(t)) + " du --format '{{json .}}'")
	var records []struct {
		ID string `json:"id"`
	}
	if err != nil || json.Unmarshal(out, &records) != nil {
		t.Fatalf("buildctl du: %v\n%s", err, out)
	}
	ids := map[string]bool{}
	for _, record := range records {
		ids[record.ID] = true
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
