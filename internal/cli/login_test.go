package cli

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// Login finishes in the browser: the CLI prints the console URL, takes the
// pasted token, validates it and stores it (ADR-0029).
func TestLoginUsesTheConsoleToken(t *testing.T) {
	const token = "orr_0123456789abcdef_secret"
	var authorization string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/users/me" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		authorization = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"u1","name":"alice","is_admin":false}`)
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "config.json")
	var stdout, stderr bytes.Buffer
	if err := runLogin(srv.URL, path, nil, strings.NewReader(token+"\n"), &stdout, &stderr); err != nil {
		t.Fatalf("login: %v", err)
	}
	if authorization != "Bearer "+token {
		t.Fatalf("Authorization = %q, want Bearer %s", authorization, token)
	}
	if !strings.Contains(stderr.String(), "/login?return=%2Fcli-auth") {
		t.Fatalf("stderr = %q, want the console URL", stderr.String())
	}
	fc, err := loadConfig(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if fc.Token != token || fc.APIURL != srv.URL {
		t.Fatalf("config = %+v", fc)
	}
}

// There is no password login any more (ADR-0029): --username is not a flag.
func TestLoginHasNoPasswordFlags(t *testing.T) {
	err := runLogin("http://127.0.0.1:1", filepath.Join(t.TempDir(), "config.json"),
		[]string{"--username", "bob"}, strings.NewReader("\n"), io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "username") {
		t.Fatalf("error = %v, want --username rejected", err)
	}
}

// An empty paste is rejected before any request is made.
func TestLoginRejectsAnEmptyToken(t *testing.T) {
	err := runLogin("http://127.0.0.1:1", filepath.Join(t.TempDir(), "config.json"), nil,
		strings.NewReader("\n"), io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "no token pasted") {
		t.Fatalf("error = %v", err)
	}
}
