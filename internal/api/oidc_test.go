package api

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The console's safeReturn and this one must agree: only same-origin paths
// survive an OIDC callback.
func TestSafeReturnPath(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"/cli-auth", "/cli-auth"},
		{"/sandboxes/1?x=1", "/sandboxes/1?x=1"},
		{"", "/"},
		{"https://evil.example", "/"},
		{"//evil.example", "/"},
		{`/\evil.example`, "/"},
		{"/\tevil.example", "/"},
		{"/\x7fevil.example", "/"},
		{"relative", "/"},
	} {
		if got := safeReturnPath(tt.in); got != tt.want {
			t.Fatalf("safeReturnPath(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestEmailDomainAllowed(t *testing.T) {
	domains := []string{"example.com", "corp.example"}
	for _, tt := range []struct {
		name   string
		email  string
		limits []string
		want   bool
	}{
		{"empty config allows anyone", "a@anywhere.test", nil, true},
		{"allowed domain", "a@example.com", domains, true},
		{"case-insensitive domain", "a@EXAMPLE.COM", domains, true},
		{"another allowed domain", "a@corp.example", domains, true},
		{"outside the list", "a@evil.test", domains, false},
		{"subdomain is not the domain", "a@sub.example.com", domains, false},
		{"no email with a list", "", domains, false},
		{"malformed email", "not-an-email", domains, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := emailDomainAllowed(tt.email, tt.limits); got != tt.want {
				t.Fatalf("emailDomainAllowed(%q, %v) = %v, want %v", tt.email, tt.limits, got, tt.want)
			}
		})
	}
}

func TestOIDCUserName(t *testing.T) {
	for _, tt := range []struct {
		name    string
		subject string
		claims  oidcClaims
		want    string
	}{
		{"preferred username wins", "sub-1", oidcClaims{PreferredUsername: "alice", Name: "Alice", Email: "alice@example.com"}, "alice"},
		{"name is the fallback", "sub-1", oidcClaims{Name: "Alice", Email: "alice@example.com"}, "Alice"},
		{"email local part is next", "sub-1", oidcClaims{Email: "alice@example.com"}, "alice"},
		{"subject is last", "sub-1", oidcClaims{}, "sub-1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := oidcUserName(tt.subject, tt.claims); got != tt.want {
				t.Fatalf("oidcUserName = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestOIDCStateCookieRoundTrip(t *testing.T) {
	flow := oidcState{State: "s", Nonce: "n", Verifier: "v", Return: "/cli-auth"}
	raw, err := json.Marshal(flow)
	if err != nil {
		t.Fatal(err)
	}
	payload := base64.RawURLEncoding.EncodeToString(raw)
	r := httptest.NewRequest(http.MethodGet, "/api/v1/oidc/callback", nil)
	r.AddCookie(&http.Cookie{Name: oidcStateCookieName, Value: payload})
	w := httptest.NewRecorder()

	got, ok := takeOIDCState(w, r)
	if !ok || got != flow {
		t.Fatalf("takeOIDCState = (%+v, %v), want %+v", got, ok, flow)
	}
	if cookies := w.Result().Cookies(); len(cookies) != 1 || cookies[0].MaxAge != -1 {
		t.Fatalf("the state cookie was not cleared: %v", cookies)
	}
}

func TestOIDCStateWithoutCookieIsRejected(t *testing.T) {
	w := httptest.NewRecorder()
	if _, ok := takeOIDCState(w, httptest.NewRequest(http.MethodGet, "/api/v1/oidc/callback", nil)); ok {
		t.Fatal("a callback without the state cookie must be rejected")
	}
}
