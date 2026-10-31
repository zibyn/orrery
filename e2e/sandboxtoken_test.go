//go:build e2e

package e2e

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestSandboxTokenScope is the "sandbox-token-scope" acceptance scenario
// (#39 M3 item 5, ADR-0016): a Sandbox Token reaches the Exec environment,
// may call only POST /git/credential, and stops working when the Sandbox is
// deleted. It also checks the in-Sandbox Git helper is configured through the
// environment only, so an image's own git config is never touched.
func TestSandboxTokenScope(t *testing.T) {
	admin := adminToken(t)
	raiseSandboxQuota(t, admin, 10)

	suffix := time.Now().UnixNano()
	nodeName := fmt.Sprintf("e2e-token-node-%d", suffix)
	state, dataRoot := enrollAgent(t, admin, nodeName)
	startAgent(t, state, dataRoot)
	waitForNodeReady(t, admin, nodeName)

	sandboxID := createReadySandbox(t, admin, fmt.Sprintf("token-%d", suffix), 256<<20)

	run := func(argv ...string) (string, string) {
		t.Helper()
		e := createExec(t, admin, sandboxID, map[string]any{"argv": argv})
		stdout, stderr, _, result := followExec(t, admin, e.ID, 60*time.Second)
		if result.Cause != "exited" || result.ExitCode == nil || *result.ExitCode != 0 {
			t.Fatalf("%v: result = %+v, stderr %s", argv, result, stderr)
		}
		return string(stdout), string(stderr)
	}

	token, _ := run("sh", "-c", `printf %s "$ORRERY_SANDBOX_TOKEN"`)
	if !strings.HasPrefix(token, "ors_") {
		t.Fatalf("ORRERY_SANDBOX_TOKEN = %q, want an ors_ token", token)
	}
	if !strings.Contains(token, sandboxID) {
		t.Fatalf("ORRERY_SANDBOX_TOKEN %q does not name Sandbox %s", token, sandboxID)
	}
	orreryURL, _ := run("sh", "-c", `printf %s "$ORRERY_URL"`)
	if orreryURL == "" {
		t.Fatal("ORRERY_URL is empty; ORRERY_ACCESS_URL did not reach the Exec")
	}

	// A Sandbox Token may only call POST /git/credential (api.md §1).
	for _, probe := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/users/me"},
		{http.MethodGet, "/api/v1/sandboxes/" + sandboxID},
		{http.MethodGet, "/api/v1/secrets"},
		{http.MethodPost, "/api/v1/sandboxes/" + sandboxID + "/execs"},
	} {
		status, body := call(t, probe.method, probe.path, token, nil)
		if status != http.StatusForbidden {
			t.Fatalf("%s %s with a Sandbox Token: %d %s, want 403", probe.method, probe.path, status, body)
		}
		requireProblemCode(t, body, "permission_denied")
	}

	// The endpoint itself takes a Sandbox Token and nothing else.
	status, body := call(t, http.MethodPost, "/api/v1/git/credential", admin,
		map[string]string{"host": "github.com"})
	if status != http.StatusForbidden {
		t.Fatalf("POST /git/credential with an API Token: %d %s, want 403", status, body)
	}
	requireProblemCode(t, body, "permission_denied")
	status, body = call(t, http.MethodPost, "/api/v1/git/credential", token,
		map[string]string{"host": "github.com"})
	if status != http.StatusNotFound {
		t.Fatalf("POST /git/credential for an unknown host: %d %s, want 404", status, body)
	}
	requireProblemCode(t, body, "not_found")

	// The external CLI is staged and is the only credential helper. Installing
	// git in the image proves the helper config travels in the environment,
	// not in a file (node-protocol.md, runtime.md §2).
	if out, _ := run("sh", "-c", `test -x /.orrery/bin/orrery && echo ok`); strings.TrimSpace(out) != "ok" {
		t.Fatal("the external CLI is not staged in /.orrery/bin")
	}
	path, _ := run("sh", "-c", `printf %s "$PATH"`)
	if !strings.HasSuffix(path, ":/.orrery/bin") && path != "/.orrery/bin" {
		t.Fatalf("PATH = %q, want /.orrery/bin appended", path)
	}
	if noSandboxEgress() {
		t.Log("ORRERY_E2E_SANDBOX_NO_EGRESS: no git and no route to ORRERY_URL; the helper checks are skipped")
	} else {
		installGit(t, admin, sandboxID)
		helpers, _ := run("sh", "-c", `git config --get-all credential.helper | sed '/^$/d'`)
		if got := strings.TrimSpace(helpers); got != "/.orrery/bin/orrery git-credential" {
			t.Fatalf("credential.helper = %q, want only the platform helper", got)
		}

		// An unmatched host passes through: no output, and a not_found round
		// trip through ORRERY_URL still exits 0 so git can try the next helper.
		stdout, stderr := run("sh", "-c",
			`printf 'protocol=https\nhost=github.com\n\n' | /.orrery/bin/orrery git-credential get`)
		if stdout != "" {
			t.Fatalf("pass-through helper stdout = %q, want empty", stdout)
		}
		if stderr != "" {
			t.Fatalf("pass-through helper stderr = %q, want empty", stderr)
		}
	}

	// The token is never persisted: not in the Workspace, not in the
	// container configuration.
	workspace, _ := run("sh", "-c", `grep -rl "$ORRERY_SANDBOX_TOKEN" /workspace 2>/dev/null || true`)
	if workspace != "" {
		t.Fatalf("the Sandbox Token is written under /workspace: %s", workspace)
	}
	home, _ := run("sh", "-c", `ls "$HOME/.gitconfig" "$HOME/.git-credentials" 2>/dev/null || true`)
	if home != "" {
		t.Fatalf("git credential files exist: %s", home)
	}
	out, err := inspectSandboxRuntime(sandboxID)
	if err != nil {
		t.Fatalf("inspect the Sandbox container: %v\n%s", err, out)
	}
	if strings.Contains(string(out), token) {
		t.Fatalf("the container record exposes the Sandbox Token:\n%s", out)
	}

	// Deleting the Sandbox revokes the token at once, before teardown finishes.
	if status, body := call(t, http.MethodDelete, "/api/v1/sandboxes/"+sandboxID, admin, nil); status != http.StatusAccepted {
		t.Fatalf("delete Sandbox: %d %s", status, body)
	}
	status, body = call(t, http.MethodPost, "/api/v1/git/credential", token, map[string]string{"host": "github.com"})
	if status != http.StatusUnauthorized {
		t.Fatalf("deleted Sandbox's token: %d %s, want 401", status, body)
	}
	requireProblemCode(t, body, "unauthenticated")

	waitForSandboxDeleted(t, sandboxID, 90*time.Second)
	status, body = call(t, http.MethodPost, "/api/v1/git/credential", token, map[string]string{"host": "github.com"})
	if status != http.StatusUnauthorized {
		t.Fatalf("deleted Sandbox's token after teardown: %d %s, want 401", status, body)
	}
}
