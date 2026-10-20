package api

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/zibyn/orrery/internal/keyring"
)

// writeGitProviders writes a secret file and a providers file, returning the
// providers file path.
func writeGitProviders(t *testing.T, jsonBody string) string {
	t.Helper()
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret")
	if err := os.WriteFile(secret, []byte("client-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "providers.json")
	body := strings.ReplaceAll(jsonBody, "__SECRET__", secret)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadGitProvidersGitHubDefaults(t *testing.T) {
	path := writeGitProviders(t, `[{
		"id": "github", "type": "github",
		"client_id": "cid", "client_secret_file": "__SECRET__",
		"scopes": ["repo", "workflow"]
	}]`)
	providers, err := loadGitProviders(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(providers) != 1 {
		t.Fatalf("providers = %d, want 1", len(providers))
	}
	p := providers[0]
	if p.AuthURL != "https://github.com/login/oauth/authorize" ||
		p.TokenURL != "https://github.com/login/oauth/access_token" ||
		p.ValidateURL != "https://api.github.com/user" ||
		p.RevokeURL != "https://api.github.com/applications/cid/grant" {
		t.Fatalf("GitHub endpoints = %+v", p)
	}
	if !p.PKCE {
		t.Fatal("PKCE must default to true")
	}
	if got := strings.Join(p.Scopes, " "); got != "repo workflow" {
		t.Fatalf("scopes = %q, want repo workflow", got)
	}
	if !p.HostPattern.MatchString("github.com") || p.HostPattern.MatchString("gitlab.com") {
		t.Fatalf("host pattern %q does not match github.com only", p.HostPattern)
	}
	if p.ClientSecret != "client-secret" {
		t.Fatalf("client secret = %q", p.ClientSecret)
	}
}

func TestLoadGitProvidersGiteaAndGeneric(t *testing.T) {
	path := writeGitProviders(t, `[
		{"id": "gitea", "type": "gitea", "client_id": "cid", "client_secret_file": "__SECRET__",
		 "base_url": "https://git.example.test/", "host": "^git\\.example\\.test$", "scopes": ["read:repository"]},
		{"id": "custom", "type": "generic", "client_id": "cid", "client_secret_file": "__SECRET__",
		 "auth_url": "https://idp.test/authorize", "token_url": "https://idp.test/token", "pkce": false, "scopes": []}
	]`)
	providers, err := loadGitProviders(path)
	if err != nil {
		t.Fatal(err)
	}
	gitea, custom := providers[0], providers[1]
	if gitea.AuthURL != "https://git.example.test/login/oauth/authorize" ||
		gitea.TokenURL != "https://git.example.test/login/oauth/access_token" ||
		gitea.ValidateURL != "https://git.example.test/api/v1/user" {
		t.Fatalf("Gitea endpoints = %+v", gitea)
	}
	if gitea.RevokeURL != "" {
		t.Fatalf("Gitea revoke URL = %q, want empty", gitea.RevokeURL)
	}
	if custom.PKCE {
		t.Fatal("pkce=false must be honored")
	}
	if custom.ValidateURL != "" {
		t.Fatalf("generic validate URL = %q, want empty", custom.ValidateURL)
	}
	if !custom.HostPattern.MatchString("idp.test") {
		t.Fatalf("generic host pattern %q does not match the auth host", custom.HostPattern)
	}
}

func TestLoadGitProvidersRejectsInvalidConfig(t *testing.T) {
	for name, body := range map[string]string{
		"gitea without base_url": `[{"id":"g","type":"gitea","client_id":"c","client_secret_file":"__SECRET__","scopes":[]}]`,
		"generic without auth":   `[{"id":"g","type":"generic","client_id":"c","client_secret_file":"__SECRET__","token_url":"https://x.test/t","scopes":[]}]`,
		"unknown type":           `[{"id":"g","type":"svn","client_id":"c","client_secret_file":"__SECRET__","scopes":[]}]`,
		"missing scopes":         `[{"id":"g","type":"github","client_id":"c","client_secret_file":"__SECRET__"}]`,
		"duplicate id": `[{"id":"g","type":"github","client_id":"c","client_secret_file":"__SECRET__","scopes":[]},
		                 {"id":"g","type":"github","client_id":"c","client_secret_file":"__SECRET__","scopes":[]}]`,
		"bad host pattern": `[{"id":"g","type":"github","client_id":"c","client_secret_file":"__SECRET__","host":"(","scopes":[]}]`,
		"endpoints on a typed provider": `[{"id":"g","type":"gitea","client_id":"c","client_secret_file":"__SECRET__",
			"base_url":"https://gitea.test","auth_url":"https://gitea.test/authorize","scopes":[]}]`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := loadGitProviders(writeGitProviders(t, body)); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

// GET /git-providers/{id}/authorize sends a browser without a Session to the
// console login page, keeping the authorize URL as the return path.
func TestGitAuthorizeWithoutSessionRedirectsToLogin(t *testing.T) {
	s := &Server{cfg: Config{
		AccessURL:    "https://orrery.example",
		GitProviders: []GitProvider{{ID: "gitea", Type: "gitea"}},
	}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/git-providers/gitea/authorize", nil)
	req.SetPathValue("id", "gitea")
	s.handleGitAuthorize(rec, req, nil)
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	location := rec.Header().Get("Location")
	want := "/login?return=%2Fapi%2Fv1%2Fgit-providers%2Fgitea%2Fauthorize"
	if location != want {
		t.Fatalf("Location = %q, want %q", location, want)
	}
}

// Linking is off without ORRERY_ACCESS_URL (defaults.md).
func TestGitAuthorizeWithoutAccessURL(t *testing.T) {
	s := &Server{cfg: Config{GitProviders: []GitProvider{{ID: "gitea"}}}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/git-providers/gitea/authorize", nil)
	req.SetPathValue("id", "gitea")
	s.handleGitAuthorize(rec, req, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// Without ORRERY_ACCESS_URL Git linking is off, so the console sees no
// providers at all (defaults.md).
func TestGitProvidersClosedWithoutAccessURL(t *testing.T) {
	s := &Server{cfg: Config{GitProviders: []GitProvider{{ID: "gitea", Type: "gitea", HostPattern: regexp.MustCompile(".")}}}}
	rec := httptest.NewRecorder()
	s.handleListGitProviders(rec, httptest.NewRequest(http.MethodGet, "/api/v1/git-providers", nil), nil)
	var out struct {
		Items []any `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Items) != 0 {
		t.Fatalf("providers listed while linking is off: %s", rec.Body)
	}
}

func TestGitStateEncryptionRoundTrip(t *testing.T) {
	s := &Server{cfg: Config{EncryptionKeys: gitTestKeyring(t)}}
	flow := gitAuthState{
		Verifier: "v", ProviderID: "gitea", UserID: "user-1",
		ExpiresAt: time.Now().Add(time.Minute).Unix(),
	}
	raw, err := s.encodeGitState(flow)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := s.decodeGitState(raw)
	if !ok || got != flow {
		t.Fatalf("decodeGitState = (%+v, %v), want %+v", got, ok, flow)
	}
	if _, ok := s.decodeGitState(raw + "x"); ok {
		t.Fatal("a tampered state must be rejected")
	}
}

func TestGitStateExpires(t *testing.T) {
	s := &Server{cfg: Config{EncryptionKeys: gitTestKeyring(t)}}
	raw, err := s.encodeGitState(gitAuthState{
		Verifier: "v", ProviderID: "gitea", UserID: "user-1",
		ExpiresAt: time.Now().Add(-time.Second).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.decodeGitState(raw); ok {
		t.Fatal("an expired state must be rejected")
	}
}

func gitTestKeyring(t *testing.T) *keyring.Keyring {
	t.Helper()
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	k, err := keyring.Parse([]byte("test " + base64.StdEncoding.EncodeToString(raw)))
	if err != nil {
		t.Fatal(err)
	}
	return k
}
