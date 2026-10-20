package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// secretAPI is a minimal stand-in for the Control Plane's Secret endpoints.
func secretAPI(t *testing.T) (*httptest.Server, map[string]string) {
	t.Helper()
	values := map[string]string{}
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/v1/secrets/{name}", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Value string `json:"value"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		values[r.PathValue("name")] = body.Value
		_, _ = w.Write([]byte(`{"name":"` + r.PathValue("name") + `","updated_at":"2026-10-17T00:00:00Z"}`))
	})
	mux.HandleFunc("GET /api/v1/secrets", func(w http.ResponseWriter, r *http.Request) {
		items := make([]map[string]string, 0, len(values))
		for name := range values {
			items = append(items, map[string]string{"name": name, "updated_at": "2026-10-17T00:00:00Z"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items, "next_cursor": nil})
	})
	mux.HandleFunc("DELETE /api/v1/secrets/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if _, ok := values[name]; !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":"not_found","status":404}`))
			return
		}
		delete(values, name)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"name":"` + name + `","updated_at":"2026-10-17T00:00:00Z"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, values
}

func runCLI(t *testing.T, stdin *os.File, args ...string) (int, string, string) {
	t.Helper()
	if stdin != nil {
		old := os.Stdin
		os.Stdin = stdin
		t.Cleanup(func() { os.Stdin = old })
	}
	t.Setenv("ORRERY_SANDBOX_CONFIG", filepath.Join(t.TempDir(), "missing.json"))
	var stdout, stderr bytes.Buffer
	code := Run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestSecretCLICreateUpdateListDelete(t *testing.T) {
	srv, values := secretAPI(t)
	global := []string{"--api-url", srv.URL, "--token", "orr_test"}
	valueFile := filepath.Join(t.TempDir(), "value")
	if err := os.WriteFile(valueFile, []byte("first-value"), 0o600); err != nil {
		t.Fatal(err)
	}

	if code, _, stderr := runCLI(t, nil, append(global, "secret", "set", "--file", valueFile, "API_TOKEN")...); code != 0 {
		t.Fatalf("secret set --file: exit %d, %s", code, stderr)
	}
	if values["API_TOKEN"] != "first-value" {
		t.Fatalf("stored value = %q, want first-value", values["API_TOKEN"])
	}

	stdin := filepath.Join(t.TempDir(), "stdin")
	if err := os.WriteFile(stdin, []byte("second-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(stdin)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if code, _, stderr := runCLI(t, f, append(global, "secret", "set", "API_TOKEN")...); code != 0 {
		t.Fatalf("secret set from stdin: exit %d, %s", code, stderr)
	}
	if values["API_TOKEN"] != "second-value" {
		t.Fatalf("overwritten value = %q, want second-value (trailing newline trimmed)", values["API_TOKEN"])
	}

	code, stdout, stderr := runCLI(t, nil, append(global, "secret", "list")...)
	if code != 0 {
		t.Fatalf("secret list: exit %d, %s", code, stderr)
	}
	if !strings.Contains(stdout, "API_TOKEN") {
		t.Fatalf("secret list output = %q, want API_TOKEN", stdout)
	}

	if code, _, stderr := runCLI(t, nil, append(global, "secret", "rm", "API_TOKEN")...); code != 0 {
		t.Fatalf("secret rm: exit %d, %s", code, stderr)
	}
	if _, ok := values["API_TOKEN"]; ok {
		t.Fatal("secret rm left the value behind")
	}
}
