package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"golang.org/x/oauth2"
)

func gitMatchServer(providers ...GitProvider) *Server {
	return &Server{cfg: Config{AccessURL: "https://orrery.example", GitProviders: providers}}
}

// A host resolves to its provider; no match passes through and several
// matches fail rather than guess (ADR-0013, api.md §3).
func TestGitCredentialProviderMatching(t *testing.T) {
	gitea := GitProvider{ID: "gitea", HostPattern: regexp.MustCompile(`^git\.example$`)}
	other := GitProvider{ID: "other", HostPattern: regexp.MustCompile(`^git\.example$`)}
	plain := GitProvider{ID: "plain", HostPattern: regexp.MustCompile(`^plain\.example$`)}

	cases := []struct {
		name    string
		server  *Server
		host    string
		id      string
		wantID  string
		matches int
	}{
		{"host with one provider", gitMatchServer(gitea, plain), "git.example", "", "gitea", 1},
		{"host with no provider", gitMatchServer(gitea), "unknown.example", "", "", 0},
		{"host with two providers", gitMatchServer(gitea, other), "git.example", "", "", 2},
		{"provider by id", gitMatchServer(gitea, plain), "", "plain", "plain", 1},
		{"unknown provider id", gitMatchServer(gitea), "", "nope", "", 0},
		{"linking off without an access URL", &Server{cfg: Config{GitProviders: []GitProvider{gitea}}}, "git.example", "", "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider, matches := tc.server.gitCredentialProvider(tc.host, tc.id)
			if matches != tc.matches || provider.ID != tc.wantID {
				t.Fatalf("got (%q, %d), want (%q, %d)", provider.ID, matches, tc.wantID, tc.matches)
			}
		})
	}
}

// The username is fixed per provider type (ADR-0013).
func TestGitUsername(t *testing.T) {
	for providerType, want := range map[string]string{
		"github":  "x-access-token",
		"gitlab":  "oauth2",
		"gitea":   "oauth2",
		"generic": "oauth2",
	} {
		if got := gitUsername(providerType); got != want {
			t.Fatalf("gitUsername(%q) = %q, want %q", providerType, got, want)
		}
	}
}

// The provider's own user endpoint decides whether a token is still usable.
func TestValidateGitToken(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		switch r.URL.Path {
		case "/ok":
			w.WriteHeader(http.StatusOK)
		case "/rejected":
			w.WriteHeader(http.StatusUnauthorized)
		default:
			w.WriteHeader(http.StatusBadGateway)
		}
	}))
	t.Cleanup(srv.Close)

	provider := func(path string) GitProvider {
		return GitProvider{Type: "gitea", ValidateURL: srv.URL + path}
	}
	if err := validateGitToken(t.Context(), provider("/ok"), "tok"); err != nil {
		t.Fatalf("valid token: %v", err)
	}
	if gotAuth != "Bearer tok" {
		t.Fatalf("Authorization = %q, want Bearer tok", gotAuth)
	}
	if err := validateGitToken(t.Context(), provider("/rejected"), "tok"); !errors.Is(err, errGitTokenRejected) {
		t.Fatalf("rejected token: err = %v, want errGitTokenRejected", err)
	}
	if err := validateGitToken(t.Context(), provider("/broken"), "tok"); err == nil || errors.Is(err, errGitTokenRejected) {
		t.Fatalf("upstream failure: err = %v, want a transient error", err)
	}
	// A generic provider without validate_url skips validation (defaults.md).
	if err := validateGitToken(t.Context(), GitProvider{Type: "generic"}, "tok"); err != nil {
		t.Fatalf("no validate_url: %v", err)
	}
}

// Only invalid_grant makes a link permanently dead (ADR-0017).
func TestIsPermanentGitRefreshError(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"invalid_grant", &oauth2.RetrieveError{ErrorCode: "invalid_grant"}, true},
		{"server_error", &oauth2.RetrieveError{ErrorCode: "server_error"}, false},
		{"network", errors.New("connection reset"), false},
	} {
		if got := isPermanentGitRefreshError(tc.err); got != tc.want {
			t.Fatalf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The 404 body carries the console's Git page (api.md §3).
func TestWriteGitAuthorizationRequired(t *testing.T) {
	s := &Server{cfg: Config{AccessURL: "https://orrery.example/"}}
	rec := httptest.NewRecorder()
	s.writeGitAuthorizationRequired(rec, "no link")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Fatalf("Content-Type = %q", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"code":"not_found"`) {
		t.Fatalf("body = %s, want not_found", body)
	}
	if !strings.Contains(body, `"authorize_url":"https://orrery.example/git"`) {
		t.Fatalf("body = %s, want the console's /git URL", body)
	}
}
