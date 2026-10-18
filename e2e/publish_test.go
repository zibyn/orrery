//go:build e2e

package e2e

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// runExecScript runs a shell command in a Sandbox and waits for it to exit
// successfully. The M4 publish scenarios use it to populate the Workspace the
// way a Sandbox would.
func runExecScript(t *testing.T, admin, sandboxID, script string) {
	t.Helper()
	e := createExec(t, admin, sandboxID, map[string]any{"argv": []string{"sh", "-c", script}})
	deadline := time.Now().Add(60 * time.Second)
	for {
		view := getExec(t, admin, e.ID)
		if view.State == "finished" {
			if view.Result == nil || view.Result.ExitCode == nil || *view.Result.ExitCode != 0 {
				t.Fatalf("Exec %s did not exit 0: %+v", e.ID, view.Result)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("Exec %s never finished (state %q)", e.ID, view.State)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

type publishedVersion struct {
	ID         string  `json:"id"`
	ResourceID string  `json:"resource_id"`
	Address    *string `json:"address"`
	Size       int64   `json:"size"`
	State      string  `json:"state"`
	Cause      *string `json:"cause"`
}

type publishedResource struct {
	ID               string  `json:"id"`
	CurrentVersionID *string `json:"current_version_id"`
}

func getVersion(t *testing.T, admin, resourceID, versionID string) publishedVersion {
	t.Helper()
	status, data := call(t, http.MethodGet, "/api/v1/resources/"+resourceID+"/versions/"+versionID, admin, nil)
	if status != http.StatusOK {
		t.Fatalf("get ResourceVersion: %d %s", status, data)
	}
	var version publishedVersion
	mustJSON(t, data, &version)
	return version
}

func getResource(t *testing.T, admin, resourceID string) publishedResource {
	t.Helper()
	status, data := call(t, http.MethodGet, "/api/v1/resources/"+resourceID, admin, nil)
	if status != http.StatusOK {
		t.Fatalf("get Resource: %d %s", status, data)
	}
	var resource publishedResource
	mustJSON(t, data, &resource)
	return resource
}

// writeExpectedTree materializes what the Sandbox wrote under /workspace/src,
// rooted the way a published subtree is: paths relative to the published path.
func writeExpectedTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "hello.txt"), []byte("hello stars\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "run.sh"), []byte("#!/bin/sh\necho run\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("hello.txt", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	return root
}

// TestPublishFromWorkspace is the "publish-from-workspace" acceptance scenario
// (#87): a persistent Workspace with a running Sandbox publishes a path; the
// version becomes ready, moves the current pointer, and pulls back byte for
// byte (including the executable bit, a symlink and an empty directory).
func TestPublishFromWorkspace(t *testing.T) {
	admin := adminToken(t)
	raiseSandboxQuota(t, admin, 10)
	suffix := time.Now().UnixNano()
	nodeName := fmt.Sprintf("e2e-publish-node-%d", suffix)
	state, dataRoot := enrollAgent(t, admin, nodeName)
	startAgent(t, state, dataRoot)
	waitForNodeReady(t, admin, nodeName)
	// The pinning Sandbox caches a read-only version tree.
	t.Cleanup(func() { makeTreeRemovable(dataRoot) })

	ws := waitForWorkspaceState(t, admin, createWorkspace(t, admin).ID, "ready", 60*time.Second)
	status, data := createSandboxWithWorkspace(t, admin, fmt.Sprintf("publish-%d", suffix), ws.ID, "rw", 0)
	if status != http.StatusCreated {
		t.Fatalf("create Sandbox: %d %s", status, data)
	}
	var sb sandboxView
	mustJSON(t, data, &sb)
	status, data = call(t, http.MethodGet, "/api/v1/sandboxes/"+sb.ID+"?wait_for=ready", admin, nil)
	if status != http.StatusOK {
		t.Fatalf("wait_for=ready: %d %s", status, data)
	}

	runExecScript(t, admin, sb.ID, `set -eu
mkdir -p /workspace/src/empty /workspace/src/bin
printf 'hello stars\n' > /workspace/src/hello.txt
printf '#!/bin/sh\necho run\n' > /workspace/src/bin/run.sh
chmod +x /workspace/src/bin/run.sh
ln -s hello.txt /workspace/src/link
# Exec runs as root; make the tree removable by the test user's TempDir
# cleanup. The executable bit recorded by the manifest is unaffected.
chmod -R a+rwX /workspace/src
`)

	name := fmt.Sprintf("ws-publish-%d", suffix)
	cli := newCLIRunner(t, admin)
	var version publishedVersion
	mustJSON(t, cli.run(t, "", "workspace", "publish", ws.ID, "/workspace/src", name), &version)
	if version.State != "ready" || version.Address == nil {
		t.Fatalf("published version = %+v, want a ready version with an address", version)
	}
	resource := getResource(t, admin, version.ResourceID)
	if resource.CurrentVersionID == nil || *resource.CurrentVersionID != version.ID {
		t.Fatalf("Resource current = %v, want %s", resource.CurrentVersionID, version.ID)
	}

	dst := filepath.Join(t.TempDir(), "pulled")
	cli.run(t, "", "resource", "pull", name+"@"+version.ID, dst)
	assertTreesEqual(t, writeExpectedTree(t), dst)

	// #90 retention: STARS_RESOURCE_VERSION_TTL=1s and
	// STARS_PURGE_INTERVAL=1s (scripts/e2e.sh). A Sandbox pins the first
	// version; two more publishes make it non-current, and the middle one
	// becomes a non-current version with no reference.
	status, data = call(t, http.MethodPost, "/api/v1/sandboxes", admin, map[string]any{
		"name":      fmt.Sprintf("ttl-pin-%d", suffix),
		"cpu":       0.5,
		"memory":    128 << 20,
		"idle_stop": 0,
		"resources": []map[string]any{{"resource": name, "version": version.ID, "mount": "data"}},
	})
	if status != http.StatusCreated {
		t.Fatalf("create pinning Sandbox: %d %s", status, data)
	}
	var pinned sandboxView
	mustJSON(t, data, &pinned)
	defer deleteSandbox(t, admin, pinned.ID)
	waitForSandboxState(t, admin, pinned.ID, "ready", 120*time.Second)

	// Change the tree so the middle version has its own address, then publish
	// the same tree twice: v2 is stale once v3 is current, but v3 still uses
	// its content-addressed manifest.
	runExecScript(t, admin, sb.ID, `printf 'second generation\n' > /workspace/src/hello.txt`)
	var stale publishedVersion
	mustJSON(t, cli.run(t, "", "workspace", "publish", ws.ID, "/workspace/src", name), &stale)
	if stale.ID == version.ID || stale.State != "ready" || stale.Address == nil {
		t.Fatalf("second published version = %+v", stale)
	}
	var current publishedVersion
	mustJSON(t, cli.run(t, "", "workspace", "publish", ws.ID, "/workspace/src", name), &current)
	if current.ID == stale.ID || current.Address == nil || *current.Address != *stale.Address {
		t.Fatalf("third published version = %+v, want the middle version's content", current)
	}
	if resource := getResource(t, admin, version.ResourceID); resource.CurrentVersionID == nil || *resource.CurrentVersionID != current.ID {
		t.Fatalf("current pointer = %v, want %s", resource.CurrentVersionID, current.ID)
	}

	// The stale version is physically gone (row included), while the version a
	// stopped-able Sandbox references and the current version survive.
	deadline := time.Now().Add(60 * time.Second)
	for psqlCount(t, fmt.Sprintf("SELECT count(*) FROM resource_versions WHERE id = '%s'", stale.ID)) != "0" {
		if time.Now().After(deadline) {
			t.Fatalf("non-current version %s survived the TTL sweep", stale.ID)
		}
		time.Sleep(250 * time.Millisecond)
	}
	if status, data := call(t, http.MethodGet,
		"/api/v1/resources/"+version.ResourceID+"/versions/"+version.ID, admin, nil); status != http.StatusOK {
		t.Fatalf("referenced version was swept: %d %s", status, data)
	}
	// The stale row shared its manifest with the current version; the object
	// must outlive the sweep.
	if status, data := call(t, http.MethodGet,
		"/api/v1/resources/"+version.ResourceID+"/versions/"+current.ID+"/manifest", admin, nil); status != http.StatusOK {
		t.Fatalf("shared manifest was swept with the stale version: %d %s", status, data)
	}
}

// TestPublishAgentRestart is the "publish-agent-restart" acceptance scenario
// (#87): the Agent is frozen when the publish starts, so the version is
// observably pending and the current pointer unchanged; after a restart the
// redelivered command converges to ready or failed, never stuck and never
// half-made.
func TestPublishAgentRestart(t *testing.T) {
	admin := adminToken(t)
	raiseSandboxQuota(t, admin, 10)
	suffix := time.Now().UnixNano()
	nodeName := fmt.Sprintf("e2e-publish-restart-%d", suffix)
	state, dataRoot := enrollAgent(t, admin, nodeName)
	agent := startAgent(t, state, dataRoot)
	waitForNodeReady(t, admin, nodeName)

	ws := waitForWorkspaceState(t, admin, createWorkspace(t, admin).ID, "ready", 60*time.Second)

	// A Resource that already exists makes the unchanged current pointer
	// observable across the publish.
	resourceName := fmt.Sprintf("ws-restart-%d", suffix)
	status, data := call(t, http.MethodPost, "/api/v1/resources", admin, map[string]string{"name": resourceName})
	if status != http.StatusCreated {
		t.Fatalf("create Resource: %d %s", status, data)
	}
	var resource publishedResource
	mustJSON(t, data, &resource)
	if resource.CurrentVersionID != nil {
		t.Fatalf("new Resource already has a current version: %v", resource.CurrentVersionID)
	}

	// Populate the host Workspace directly: the Agent publishes the directory,
	// no Sandbox is required. Enough data keeps a real publish busy for a
	// moment even though the freeze below is what makes the test deterministic.
	src := filepath.Join(dataRoot, "workspaces", ws.ID, "tree")
	if err := os.MkdirAll(filepath.Join(src, "many"), 0o755); err != nil {
		t.Fatal(err)
	}
	blob := bytes.Repeat([]byte("stars-sandbox\n"), 1<<19) // 8 MiB
	if err := os.WriteFile(filepath.Join(src, "blob.bin"), blob, 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		name := fmt.Sprintf("file-%02d.txt", i)
		if err := os.WriteFile(filepath.Join(src, "many", name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Freeze the Agent so the command cannot run before we observe the pending
	// version. Its Control stream stays connected, so the publish is accepted.
	if err := agent.cmd.Process.Signal(syscall.SIGSTOP); err != nil {
		t.Fatalf("freeze Agent: %v", err)
	}
	status, data = call(t, http.MethodPost, "/api/v1/workspaces/"+ws.ID+"/publish", admin, map[string]string{
		"path": "tree", "resource": resourceName,
	})
	if status != http.StatusAccepted {
		t.Fatalf("publish: %d %s", status, data)
	}
	var version publishedVersion
	mustJSON(t, data, &version)
	if version.State != "pending" {
		t.Fatalf("started version state = %q, want pending", version.State)
	}
	if resource := getResource(t, admin, resource.ID); resource.CurrentVersionID != nil {
		t.Fatalf("current pointer moved while the version is pending: %v", resource.CurrentVersionID)
	}

	// Restart the Agent with the same identity and data root.
	agent.stop()
	startAgent(t, state, dataRoot)

	deadline := time.Now().Add(120 * time.Second)
	for {
		version = getVersion(t, admin, resource.ID, version.ID)
		if version.State != "pending" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("version %s stayed pending after the Agent restart", version.ID)
		}
		time.Sleep(250 * time.Millisecond)
	}
	switch version.State {
	case "ready":
		if version.Address == nil {
			t.Fatalf("ready version has no address: %+v", version)
		}
		if resource := getResource(t, admin, resource.ID); resource.CurrentVersionID == nil || *resource.CurrentVersionID != version.ID {
			t.Fatalf("ready version did not move the current pointer: %v", resource.CurrentVersionID)
		}
		status, data := call(t, http.MethodGet,
			"/api/v1/resources/"+resource.ID+"/versions/"+version.ID+"/manifest", admin, nil)
		if status != http.StatusOK {
			t.Fatalf("ready version manifest is unreadable: %d %s", status, data)
		}
	case "failed":
		if version.Cause == nil || *version.Cause != "publish_failed" {
			t.Fatalf("failed version cause = %v", version.Cause)
		}
		if resource := getResource(t, admin, resource.ID); resource.CurrentVersionID != nil {
			t.Fatalf("failed publish moved the current pointer: %v", resource.CurrentVersionID)
		}
	default:
		t.Fatalf("version ended in unexpected state %q", version.State)
	}
}
