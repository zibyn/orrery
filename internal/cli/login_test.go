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

// A User who has no password finishes login in the browser: the CLI prints the
// console URL, takes the pasted token, validates it and stores it (ADR-0020).
func TestLoginWithoutUsernameUsesTheConsoleToken(t *testing.T) {
	const token = "ssk_0123456789abcdef_secret"
	var authorization string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/users/me" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		authorization = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"u1","name":"alice","is_admin":false,"login_type":"oidc"}`)
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

// A password attempt that fails points the User at the console, where an OIDC
// User can get a token instead.
func TestLoginPasswordFailureMentionsTheConsole(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"code":"unauthenticated","detail":"invalid username or password"}`)
	}))
	defer srv.Close()

	err := runLogin(srv.URL, filepath.Join(t.TempDir(), "config.json"),
		[]string{"--username", "bob", "--password-stdin"}, strings.NewReader("secret-password\n"), io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "without --username") {
		t.Fatalf("error = %v, want a hint to leave out --username", err)
	}
}

// An empty paste is rejected before any request is made.
func TestLoginWithoutUsernameRejectsAnEmptyToken(t *testing.T) {
	err := runLogin("http://127.0.0.1:1", filepath.Join(t.TempDir(), "config.json"), nil,
		strings.NewReader("\n"), io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "no token pasted") {
		t.Fatalf("error = %v", err)
	}
}
