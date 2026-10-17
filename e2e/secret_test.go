//go:build e2e

package e2e

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestSecrets walks a Secret through the CLI and checks that the value is
// write-only and validated (ADR-0013, ADR-0017, ticket #74).
func TestSecrets(t *testing.T) {
	admin := adminToken(t)
	cli := newCLIRunner(t, admin)
	name := fmt.Sprintf("API_TOKEN_%d", time.Now().UnixNano())

	cli.run(t, "first-value\n", "secret", "set", name)

	file := filepath.Join(t.TempDir(), "value")
	if err := os.WriteFile(file, []byte("second-value"), 0o600); err != nil {
		t.Fatal(err)
	}
	cli.run(t, "", "secret", "set", "--file", file, name)

	listed := cli.run(t, "", "secret", "list")
	var list struct {
		Items []struct {
			Name      string    `json:"name"`
			UpdatedAt time.Time `json:"updated_at"`
		} `json:"items"`
	}
	mustJSON(t, listed, &list)
	found := false
	for _, item := range list.Items {
		if item.Name == name {
			found = true
			if item.UpdatedAt.IsZero() {
				t.Fatalf("Secret %s has no updated_at", name)
			}
		}
	}
	if !found {
		t.Fatalf("secret list does not contain %s: %s", name, listed)
	}
	// The value is write-only.
	for _, secret := range []string{"first-value", "second-value"} {
		if strings.Contains(string(listed), secret) {
			t.Fatalf("secret list leaked %q: %s", secret, listed)
		}
	}

	// Reserved names and the 24 KiB cap are rejected (ADR-0017).
	status, body := call(t, http.MethodPut, "/api/v1/secrets/GIT_TOKEN", admin, map[string]string{"value": "x"})
	if status != http.StatusBadRequest {
		t.Fatalf("reserved Secret name: %d %s, want 400", status, body)
	}
	requireProblemCode(t, body, "invalid_request")
	status, body = call(t, http.MethodPut, "/api/v1/secrets/BIG", admin, map[string]string{
		"value": strings.Repeat("x", 24<<10+1),
	})
	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized Secret: %d %s, want 413", status, body)
	}
	requireProblemCode(t, body, "payload_too_large")

	// Create, update and delete are audited without the value.
	for _, action := range []string{"secret.create", "secret.update"} {
		records := cli.run(t, "", "audit", "list", "--action", action, "--target", name)
		var audit auditList
		mustJSON(t, records, &audit)
		if len(audit.Items) == 0 {
			t.Fatalf("no %s record for %s", action, name)
		}
		if strings.Contains(string(audit.Items[0].Detail), "second-value") {
			t.Fatalf("%s record leaked the value", action)
		}
	}

	cli.run(t, "", "secret", "rm", name)
	listed = cli.run(t, "", "secret", "list")
	if strings.Contains(string(listed), name) {
		t.Fatalf("secret list still contains the deleted %s: %s", name, listed)
	}
	deleted := cli.run(t, "", "audit", "list", "--action", "secret.delete", "--target", name)
	var audit auditList
	mustJSON(t, deleted, &audit)
	if len(audit.Items) == 0 {
		t.Fatalf("no secret.delete record for %s", name)
	}
}

// TestSecretQuota drives the 50-Secret cap through the handler, not just the
// predicate (ADR-0017).
func TestSecretQuota(t *testing.T) {
	admin := adminToken(t)
	suffix := strconv.FormatInt(time.Now().UnixNano(), 10)

	status, body := call(t, http.MethodGet, "/api/v1/secrets?limit=200", admin, nil)
	if status != http.StatusOK {
		t.Fatalf("list Secrets: %d %s", status, body)
	}
	var list struct {
		Items []struct {
			Name string `json:"name"`
		} `json:"items"`
	}
	mustJSON(t, body, &list)

	var created []string
	for i := len(list.Items); i < 50; i++ {
		name := fmt.Sprintf("QUOTA_%s_%d", suffix, i)
		status, body := call(t, http.MethodPut, "/api/v1/secrets/"+name, admin, map[string]string{"value": "x"})
		if status != http.StatusCreated {
			t.Fatalf("create Secret %d: %d %s", i, status, body)
		}
		created = append(created, name)
	}
	status, body = call(t, http.MethodPut, "/api/v1/secrets/OVER_"+suffix, admin, map[string]string{"value": "x"})
	if status != http.StatusTooManyRequests {
		t.Fatalf("51st Secret: %d %s, want 429", status, body)
	}
	requireProblemCode(t, body, "quota_exceeded")

	for _, name := range created {
		if status, body := call(t, http.MethodDelete, "/api/v1/secrets/"+name, admin, nil); status != http.StatusAccepted {
			t.Fatalf("delete %s: %d %s", name, status, body)
		}
	}
}

// TestSecretInject is the "secret-inject" acceptance scenario (#39 M3 item 6):
// a Secret reaches the Exec environment but no Runtime metadata, a restart sees
// the updated value, and an Agent restart does not lose the values.
func TestSecretInject(t *testing.T) {
	admin := adminToken(t)
	raiseSandboxQuota(t, admin, 10)
	suffix := time.Now().UnixNano()
	secretName := fmt.Sprintf("INJECT_%d", suffix)
	envName := fmt.Sprintf("INJECTED_%d", suffix)

	status, body := call(t, http.MethodPut, "/api/v1/secrets/"+secretName, admin,
		map[string]string{"value": "first-value"})
	if status != http.StatusCreated {
		t.Fatalf("create Secret: %d %s", status, body)
	}
	t.Cleanup(func() {
		_, _ = call(t, http.MethodDelete, "/api/v1/secrets/"+secretName, admin, nil)
	})

	// A reserved environment name and an unknown Secret name are both 400.
	status, body = call(t, http.MethodPost, "/api/v1/sandboxes", admin, map[string]any{
		"cpu": 0.5, "memory": 128 << 20, "idle_stop": 0,
		"secrets": map[string]string{"GIT_TOKEN": secretName},
	})
	if status != http.StatusBadRequest {
		t.Fatalf("reserved secrets key: %d %s, want 400", status, body)
	}
	requireProblemCode(t, body, "invalid_request")
	status, body = call(t, http.MethodPost, "/api/v1/sandboxes", admin, map[string]any{
		"cpu": 0.5, "memory": 128 << 20, "idle_stop": 0,
		"secrets": map[string]string{envName: "no-such-secret"},
	})
	if status != http.StatusBadRequest {
		t.Fatalf("unknown Secret: %d %s, want 400", status, body)
	}
	requireProblemCode(t, body, "invalid_request")

	nodeName := fmt.Sprintf("e2e-secret-node-%d", suffix)
	state, dataRoot := enrollAgent(t, admin, nodeName)
	agent := startAgent(t, state, dataRoot)
	waitForNodeReady(t, admin, nodeName)

	status, body = call(t, http.MethodPost, "/api/v1/sandboxes", admin, map[string]any{
		"name": fmt.Sprintf("secret-%d", suffix), "cpu": 0.5, "memory": 128 << 20,
		"idle_stop": 0, "secrets": map[string]string{envName: secretName},
	})
	if status != http.StatusCreated {
		t.Fatalf("create Sandbox with a Secret: %d %s", status, body)
	}
	var sb sandboxView
	mustJSON(t, body, &sb)
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", "stars-sandbox-"+sb.ID).Run()
	})
	status, body = call(t, http.MethodGet, "/api/v1/sandboxes/"+sb.ID+"?wait_for=ready", admin, nil)
	var ready sandboxView
	mustJSON(t, body, &ready)
	if status != http.StatusOK || ready.State != "ready" {
		t.Fatalf("wait_for=ready: %d %s", status, body)
	}

	// The create Audit Record names the Secrets, never a value (ADR-0013).
	status, body = call(t, http.MethodGet,
		"/api/v1/audit?action=sandbox.create&target="+sb.ID, admin, nil)
	if status != http.StatusOK {
		t.Fatalf("list audit: %d %s", status, body)
	}
	var audit auditList
	mustJSON(t, body, &audit)
	if len(audit.Items) == 0 {
		t.Fatalf("no sandbox.create Audit Record for %s", sb.ID)
	}
	detail := string(audit.Items[0].Detail)
	if !strings.Contains(detail, secretName) {
		t.Fatalf("sandbox.create detail does not name %s: %s", secretName, detail)
	}
	if strings.Contains(detail, "first-value") {
		t.Fatalf("sandbox.create detail leaked the value: %s", detail)
	}

	readSecret := func() string {
		t.Helper()
		e := createExec(t, admin, sb.ID, map[string]any{
			"argv": []string{"sh", "-c", fmt.Sprintf("printf %%s \"$%s\"", envName)},
		})
		stdout, _, _, result := followExec(t, admin, e.ID, 30*time.Second)
		if result.Cause != "exited" || result.ExitCode == nil || *result.ExitCode != 0 {
			t.Fatalf("read Secret result = %+v", result)
		}
		return string(stdout)
	}
	if got := readSecret(); got != "first-value" {
		t.Fatalf("Exec saw %q, want first-value", got)
	}

	// The value is not part of the Runtime's configuration on the Node.
	out, err := exec.Command("docker", "inspect", "stars-sandbox-"+sb.ID).CombinedOutput()
	if err != nil {
		t.Fatalf("docker inspect: %v\n%s", err, out)
	}
	if strings.Contains(string(out), "first-value") {
		t.Fatalf("docker inspect exposes the Secret value:\n%s", out)
	}
	// Nor is it stored with the Exec's caller-supplied env.
	if n := psqlCount(t, fmt.Sprintf(
		"SELECT count(*) FROM execs WHERE sandbox_id = '%s' AND env::text LIKE '%%first-value%%'", sb.ID)); n != "0" {
		t.Fatalf("%s Exec rows carry the Secret value", n)
	}

	// An update is visible only after the next start.
	if status, body := call(t, http.MethodPut, "/api/v1/secrets/"+secretName, admin,
		map[string]string{"value": "second-value"}); status != http.StatusOK {
		t.Fatalf("update Secret: %d %s", status, body)
	}
	if got := readSecret(); got != "first-value" {
		t.Fatalf("a running Sandbox saw the updated Secret: %q", got)
	}
	if status, body := call(t, http.MethodPost, "/api/v1/sandboxes/"+sb.ID+"/stop", admin, nil); status != http.StatusAccepted {
		t.Fatalf("stop: %d %s", status, body)
	}
	call(t, http.MethodGet, "/api/v1/sandboxes/"+sb.ID+"?wait_for=stopped", admin, nil)
	if status, body := call(t, http.MethodPost, "/api/v1/sandboxes/"+sb.ID+"/start", admin, nil); status != http.StatusAccepted {
		t.Fatalf("start: %d %s", status, body)
	}
	status, body = call(t, http.MethodGet, "/api/v1/sandboxes/"+sb.ID+"?wait_for=ready", admin, nil)
	mustJSON(t, body, &ready)
	if status != http.StatusOK || ready.State != "ready" {
		t.Fatalf("wait_for=ready after start: %d %s", status, body)
	}
	if got := readSecret(); got != "second-value" {
		t.Fatalf("after restart the Exec saw %q, want second-value", got)
	}

	// PTY Execs take the same StartExec environment path (issue #75).
	pty := createExec(t, admin, sb.ID, map[string]any{
		"argv": []string{"sh"},
		"pty":  map[string]any{"rows": 24, "cols": 80},
	})
	waitExecRunning(t, admin, pty.ID, 30*time.Second)
	terminal := dialPTY(t, admin, pty.ID)
	terminal.send(fmt.Sprintf("printf '%%s\\n' \"$%s\"\nexit\n", envName))
	terminal.waitContains("second-value", 20*time.Second)
	terminal.close()

	// The snapshot lives in PostgreSQL, so an Agent restart does not lose it.
	agent.stop()
	agent = startAgent(t, state, dataRoot)
	waitForNodeReady(t, admin, nodeName)
	if got := readSecret(); got != "second-value" {
		t.Fatalf("after an Agent restart the Exec saw %q, want second-value", got)
	}
}
