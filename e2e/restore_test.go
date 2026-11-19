//go:build e2e

package e2e

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestStoppedSandboxStartsOnAnotherNode is #276's two-Node scenario
// (ADR-0031, runtime.md §16): a Sandbox installs a package and commits a Git
// repository under /workspace on Node A and stops, and that stop commits the
// root filesystem as well (#275's assertStopCommits); with A paused, start puts
// it on Node B, restored from its head Generation, with the package, the
// files and a clean `git status`. Back on B it stops again and its next
// commit is based on what B restored.
func TestStoppedSandboxStartsOnAnotherNode(t *testing.T) {
	admin := adminToken(t)
	raiseSandboxQuota(t, admin, 10)
	suffix := time.Now().UnixNano()
	nameA := fmt.Sprintf("e2e-restore-a-%d", suffix)
	nameB := fmt.Sprintf("e2e-restore-b-%d", suffix)

	// A is enrolled first, so placement prefers it while both are ready.
	nodeA, _ := startAgentNode(t, admin, nameA)
	nodeB, _ := startAgentNode(t, admin, nameB)
	t.Cleanup(func() { call(t, http.MethodPost, "/api/v1/nodes/"+nodeA.ID+"/resume", admin, nil) })

	id := createReadySandbox(t, admin, fmt.Sprintf("restore-%d", suffix), 256<<20)
	t.Cleanup(func() { deleteSandbox(t, admin, id) })
	if sb := getSandbox(t, admin, id); sb.NodeID == nil || *sb.NodeID != nodeA.ID {
		t.Fatalf("Sandbox node_id = %v, want the older Node %s", sb.NodeID, nodeA.ID)
	}
	installGit(t, admin, id)
	runExecScript(t, admin, id, `set -e
mkdir -p /workspace/repo && cd /workspace/repo
git init -q -b main && git config user.email e2e@orrery && git config user.name e2e
printf 'hello\n' > README && mkdir -p bin && printf '#!/bin/sh\necho tool\n' > bin/tool && chmod 755 bin/tool
git add . && git commit -q -m first
rm -f /etc/motd`)
	assertStopCommits(t, admin, id)
	stop := func() {
		t.Helper()
		if status, data := call(t, http.MethodPost, "/api/v1/sandboxes/"+id+"/stop", admin, nil); status != http.StatusAccepted {
			t.Fatalf("stop: %d %s", status, data)
		}
		waitForSandboxState(t, admin, id, "stopped", 3*time.Minute)
	}
	headA := psqlCount(t, fmt.Sprintf("SELECT head_generation_id FROM sandboxes WHERE id = '%s'", id))

	if status, data := call(t, http.MethodPost, "/api/v1/nodes/"+nodeA.ID+"/pause", admin, nil); status != http.StatusAccepted {
		t.Fatalf("pause A: %d %s", status, data)
	}
	if status, data := call(t, http.MethodPost, "/api/v1/sandboxes/"+id+"/start", admin, nil); status != http.StatusAccepted {
		t.Fatalf("start: %d %s", status, data)
	}
	sb := waitForSandboxReady(t, admin, id)
	if sb.NodeID == nil || *sb.NodeID != nodeB.ID {
		t.Fatalf("restarted on %v, want Node B %s", sb.NodeID, nodeB.ID)
	}
	out, errOut, code := execInSandbox(t, admin, id, "sh", "-c", `set -e
git --version >/dev/null
cd /workspace/repo
test "$(cat README)" = hello && test -x bin/tool && test ! -e /etc/motd
git status --porcelain
git log --oneline | wc -l`)
	if code != 0 {
		t.Fatalf("check on B: exit %d\n%s%s", code, out, errOut)
	}
	if got := strings.TrimSpace(out); got != "1" {
		t.Fatalf("git status/log on B = %q, want a clean tree with one commit", got)
	}

	// B's commit is based on the head it restored, not refused as stale.
	runExecScript(t, admin, id, `printf 'more\n' > /workspace/repo/NOTES`)
	stop()
	headB := psqlCount(t, fmt.Sprintf("SELECT head_generation_id FROM sandboxes WHERE id = '%s'", id))
	if headB == "" || headB == headA {
		t.Fatalf("head after the stop on B = %q, want a new Generation after %q", headB, headA)
	}
}
