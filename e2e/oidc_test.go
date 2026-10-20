//go:build e2e

package e2e

import (
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strings"
	"testing"
)

const (
	dexEmail    = "oidc-user@example.com"
	dexPassword = "oidc-password-123"
)

func envURL(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// TestOIDCSignup is the oidc-signup scenario (#78, ADR-0020): an unknown OIDC
// User is denied while signups are off, and the first OIDC User on an empty
// Control Plane becomes the admin.
func TestOIDCSignup(t *testing.T) {
	// The main Control Plane has the bootstrapped admin, so signups are off.
	adminToken(t)

	status, _, _, body := oidcLogin(t, envURL("ORRERY_E2E_ACCESS_URL", apiURL()), "/cli-auth")
	if status != http.StatusForbidden {
		t.Fatalf("unknown OIDC User with allow_signups=false: %d %s, want 403", status, body)
	}
	requireProblemCode(t, body, "permission_denied")

	// The dedicated empty Control Plane signs the first OIDC User in as admin.
	base := envURL("ORRERY_E2E_OIDC_ACCESS_URL", "http://127.0.0.1:18082")
	status, location, session, body := oidcLogin(t, base, "/")
	if status != http.StatusFound || location != "/" {
		t.Fatalf("first OIDC login: %d %q %s, want 302 /", status, location, body)
	}
	if session == nil {
		t.Fatal("the OIDC callback set no Session cookie")
	}

	req, err := http.NewRequest(http.MethodGet, base+"/api/v1/users/me", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(session)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /users/me with the OIDC Session: %d %s", resp.StatusCode, data)
	}
	var me struct {
		Name      string `json:"name"`
		IsAdmin   bool   `json:"is_admin"`
		LoginType string `json:"login_type"`
	}
	mustJSON(t, data, &me)
	if !me.IsAdmin || me.LoginType != "oidc" {
		t.Fatalf("first OIDC User = %+v, want an oidc admin", me)
	}
}

// oidcLogin runs the browser half of the flow: GET /oidc/login, sign in at
// Dex, then follow the callback. It returns the callback's status, Location
// and Session cookie.
func oidcLogin(t *testing.T, base, returnTo string) (int, string, *http.Cookie, []byte) {
	t.Helper()
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	resp, err := noRedirect.Get(base + "/api/v1/oidc/login?return=" + url.QueryEscape(returnTo))
	if err != nil {
		t.Fatalf("GET /oidc/login: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		data, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /oidc/login = %d, want 302: %s", resp.StatusCode, data)
	}
	var state *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "orrery_oidc_state" {
			state = c
		}
	}
	if state == nil {
		t.Fatalf("GET /oidc/login set no state cookie: %v", resp.Header.Values("Set-Cookie"))
	}

	browser := newDexBrowser(t)
	page := browser.untilPage(t, resp.Header.Get("Location"))
	callback := browser.submit(t, page, url.Values{
		"login":    {dexEmail},
		"password": {dexPassword},
	})

	req, err := http.NewRequest(http.MethodGet, callback.String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(state)
	cb, err := noRedirect.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", callback, err)
	}
	defer cb.Body.Close()
	body, _ := io.ReadAll(cb.Body)
	var session *http.Cookie
	for _, c := range cb.Cookies() {
		if c.Name == "orrery_session" {
			session = c
		}
	}
	return cb.StatusCode, cb.Header.Get("Location"), session, body
}

// dexBrowser is a browser that talks to the published Dex port. The OIDC
// issuer is the Compose service name, which only the Control Plane can
// resolve, so requests are rewritten on the way out.
type dexBrowser struct {
	client  *http.Client
	issuer  *url.URL
	publish *url.URL
}

func newDexBrowser(t *testing.T) *dexBrowser {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := url.Parse(envURL("ORRERY_E2E_OIDC_ISSUER_URL", "http://dex:5556/dex"))
	if err != nil {
		t.Fatal(err)
	}
	publish, err := url.Parse(envURL("ORRERY_E2E_DEX_URL", "http://127.0.0.1:15556/dex"))
	if err != nil {
		t.Fatal(err)
	}
	return &dexBrowser{
		client: &http.Client{
			Jar: jar,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		issuer:  issuer,
		publish: publish,
	}
}

func (b *dexBrowser) rewrite(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(u.Host, b.issuer.Host) {
		return raw
	}
	u.Scheme = b.publish.Scheme
	u.Host = b.publish.Host
	return u.String()
}

// untilPage follows redirects until Dex renders a page, and returns its URL.
func (b *dexBrowser) untilPage(t *testing.T, raw string) *url.URL {
	t.Helper()
	for range 10 {
		resp, err := b.client.Get(b.rewrite(raw))
		if err != nil {
			t.Fatalf("GET %s: %v", raw, err)
		}
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			next, err := resp.Request.URL.Parse(resp.Header.Get("Location"))
			resp.Body.Close()
			if err != nil {
				t.Fatalf("parse Dex redirect %q: %v", resp.Header.Get("Location"), err)
			}
			raw = next.String()
			continue
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			data, _ := io.ReadAll(resp.Body)
			t.Fatalf("Dex page %s = %d: %s", raw, resp.StatusCode, data)
		}
		return resp.Request.URL
	}
	t.Fatalf("Dex redirect loop at %s", raw)
	return nil
}

// submit posts the login form and returns the URL Dex redirects to.
func (b *dexBrowser) submit(t *testing.T, page *url.URL, form url.Values) *url.URL {
	t.Helper()
	resp, err := b.client.PostForm(page.String(), form)
	if err != nil {
		t.Fatalf("POST Dex login: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 300 || resp.StatusCode >= 400 {
		data, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST Dex login = %d: %s", resp.StatusCode, data)
	}
	next, err := page.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("parse Dex callback %q: %v", resp.Header.Get("Location"), err)
	}
	return next
}
