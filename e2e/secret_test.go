//go:build e2e

package e2e

import (
	"fmt"
	"net/http"
	"os"
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
