package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// credentialAPI stands in for POST /git/credential. It records the request
// body and answers with respond.
func credentialAPI(t *testing.T, respond func(http.ResponseWriter, map[string]string)) (*httptest.Server, *map[string]string) {
	t.Helper()
	var seen map[string]string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/git/credential", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		seen = body
		respond(w, body)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &seen
}

// gitCredentialStdin writes the credential description git passes on stdin.
func gitCredentialStdin(t *testing.T, body string) *os.File {
	t.Helper()
	path := filepath.Join(t.TempDir(), "credential")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

// The helper answers `get` with the username/password lines git expects and
// forwards the repository host (gitcredentials(7)).
func TestGitCredentialGet(t *testing.T) {
	srv, seen := credentialAPI(t, func(w http.ResponseWriter, body map[string]string) {
		_, _ = w.Write([]byte(`{"username":"x-access-token","password":"ghs_value"}`))
	})
	global := []string{"--api-url", srv.URL, "--token", "sst_test"}
	stdin := gitCredentialStdin(t, "protocol=https\nhost=github.com\nusername=\n\n")

	code, stdout, stderr := runCLI(t, stdin, append(global, "git-credential", "get")...)
	if code != 0 {
		t.Fatalf("git-credential get: exit %d, %s", code, stderr)
	}
	if got := (*seen)["host"]; got != "github.com" {
		t.Fatalf("request host = %q, want github.com", got)
	}
	want := "username=x-access-token\npassword=ghs_value\n"
	if stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
}

// A 404 with authorize_url is not a helper failure: stderr names the console
// page, stdout stays empty and git carries on (api.md §3).
func TestGitCredentialNotFound(t *testing.T) {
	srv, _ := credentialAPI(t, func(w http.ResponseWriter, body map[string]string) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"type":"about:blank","title":"Not Found","status":404,` +
			`"code":"not_found","authorize_url":"https://stars.example/git"}`))
	})
	global := []string{"--api-url", srv.URL, "--token", "sst_test"}
	stdin := gitCredentialStdin(t, "protocol=https\nhost=github.com\n\n")

	code, stdout, stderr := runCLI(t, stdin, append(global, "git-credential", "get")...)
	if code != 0 {
		t.Fatalf("a 404 must exit 0 for git, got %d", code)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "https://stars.example/git") {
		t.Fatalf("stderr = %q, want the authorize_url", stderr)
	}
}

// store and erase are no-ops; git calls them after a successful auth or a
// rejection.
func TestGitCredentialStoreErase(t *testing.T) {
	for _, action := range []string{"store", "erase"} {
		t.Run(action, func(t *testing.T) {
			stdin := gitCredentialStdin(t, "protocol=https\nhost=github.com\n\n")
			code, stdout, stderr := runCLI(t, stdin, "--api-url", "http://127.0.0.1:1", "--token", "sst_test",
				"git-credential", action)
			if code != 0 || stdout != "" {
				t.Fatalf("%s: exit %d, stdout %q, stderr %q", action, code, stdout, stderr)
			}
		})
	}
}

// `token <provider>` asks for a credential by provider id; the placeholder
// answer is an error here, but the request shape is the contract.
func TestTokenProvider(t *testing.T) {
	srv, seen := credentialAPI(t, func(w http.ResponseWriter, body map[string]string) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":"not_found","status":404}`))
	})
	code, _, _ := runCLI(t, nil, "--api-url", srv.URL, "--token", "sst_test", "token", "github")
	if code == 0 {
		t.Fatal("the placeholder's 404 should surface as a failure")
	}
	if got := (*seen)["provider"]; got != "github" {
		t.Fatalf("request provider = %q, want github", got)
	}
}
