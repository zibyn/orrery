//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/zibyn/orrery/internal/generation"
)

// TestStopCommitsTheFilesystem is #275's commit-on-stop scenario on the
// default Node: what a Sandbox writes to its root filesystem is in the head
// Generation once the stop ends (runtime.md §16). TestGvisorParity runs the
// same check under runsc.
func TestStopCommitsTheFilesystem(t *testing.T) {
	admin := adminToken(t)
	id := createReadySandbox(t, admin, fmt.Sprintf("commit-%d", time.Now().UnixNano()), 128<<20)
	t.Cleanup(func() { deleteSandbox(t, admin, id) })
	assertStopCommits(t, admin, id)
}

// assertStopCommits writes a package-like tree, edits and deletes base image
// files and leaves a Git lock behind, stops the Sandbox and reads the head
// Generation back from the object store.
// shortcut: /workspace is still a Workspace bind mount until #279, so the
// repository lives under /root; #276's e2e covers the second, incremental stop.
func assertStopCommits(t *testing.T, admin, id string) {
	t.Helper()
	e := createExec(t, admin, id, map[string]any{"argv": []string{"sh", "-c", `set -e
mkdir -p /opt/pkg/bin && printf 'tool' > /opt/pkg/bin/tool && chmod 4755 /opt/pkg/bin/tool
ln -s /opt/pkg/bin/tool /usr/local/bin/tool
printf 'edited' > /etc/orrery-e2e
rm -f /etc/issue
mkdir -p /root/repo/.git/objects && printf 'ref: refs/heads/main\n' > /root/repo/.git/HEAD
printf 'x' > /root/repo/.git/index.lock && printf 'tracked' > /root/repo/file`}})
	assertExitedWith(t, waitExecFinished(t, admin, e.ID, 30*time.Second), "exited")

	if status, data := call(t, http.MethodPost, "/api/v1/sandboxes/"+id+"/stop", admin, nil); status != http.StatusAccepted {
		t.Fatalf("stop: %d %s", status, data)
	}
	waitForSandboxState(t, admin, id, "stopped", 2*time.Minute)

	head := psqlCount(t, fmt.Sprintf("SELECT head_generation_id FROM sandboxes WHERE id = '%s'", id))
	owner := psqlCount(t, fmt.Sprintf("SELECT owner_id FROM sandboxes WHERE id = '%s'", id))
	if head == "" {
		t.Fatal("the stop ended without a head Generation")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	data, err := e2eObjectStore(t).GetGeneration(ctx, owner, head)
	if err != nil {
		t.Fatalf("read Generation %s: %v", head, err)
	}
	m, err := generation.Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	if m.Kind != generation.KindDiff {
		t.Fatalf("kind = %q", m.Kind)
	}
	entries := map[string]generation.Entry{}
	for _, entry := range m.Entries {
		entries[entry.Path] = entry
	}
	if tool := entries["opt/pkg/bin/tool"]; tool.Type != "file" || tool.Mode != 0o4755 {
		t.Fatalf("tool = %+v", tool)
	}
	if link := entries["usr/local/bin/tool"]; link.Target != "/opt/pkg/bin/tool" {
		t.Fatalf("symlink = %+v", link)
	}
	if entries["etc/orrery-e2e"].Type != "file" || entries["root/repo/file"].Type != "file" {
		t.Fatalf("written files missing: %v", m.Entries)
	}
	if issue, ok := entries["etc/issue"]; ok && issue.Type != generation.TypeWhiteout {
		t.Fatalf("deleted /etc/issue = %+v, want a whiteout", issue)
	}
	for _, skipped := range []string{"root/repo/.git/index.lock", ".orrery/run", "etc/resolv.conf"} {
		if _, ok := entries[skipped]; ok {
			t.Fatalf("%s was committed", skipped)
		}
	}
}
