package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// polarisTestServer is a DB-backed Control Plane whose identity provider is
// an in-process fake Polaris (ADR-0029).
func polarisTestServer(t *testing.T) (http.Handler, *fakePolaris, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("ORRERY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ORRERY_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := Migrate(ctx, pool, dsn); err != nil {
		t.Fatal(err)
	}
	op := newFakePolaris(t)
	cfg := Config{
		AccessURL:           "https://orrery.test",
		EncryptionKeys:      testKeyring(t),
		DefaultGPUQuota:     3,
		DefaultSandboxQuota: 4,
		OIDC: OIDCConfig{
			IssuerURL: op.URL, ClientID: op.ClientID, ClientSecret: "secret", Audience: op.Audience,
			WebhookSecret: op.WebhookSecret,
		},
	}
	return New(cfg, pool, slog.New(slog.DiscardHandler)).Handler(), op, pool
}

// asSession calls the API with a Session cookie and decodes a 2xx body into out.
func asSession(t *testing.T, h http.Handler, c *http.Cookie, path string, out any) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if out != nil && rec.Code/100 == 2 {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatal(err)
		}
	}
	return rec.Code
}

func me(t *testing.T, h http.Handler, c *http.Cookie) userDTO {
	t.Helper()
	var u userDTO
	if code := asSession(t, h, c, "/api/v1/users/me", &u); code != http.StatusOK {
		t.Fatalf("GET /users/me = %d", code)
	}
	return u
}

func profile(name, email string) map[string]any {
	return map[string]any{"preferred_username": name, "email": email, "email_verified": true}
}

func TestPolarisLoginCreatesThenMatchesBySubject(t *testing.T) {
	h, op, _ := polarisTestServer(t)
	name := "p-" + uuid.NewString()[:8]
	first := me(t, h, sessionCookie(t, op.Login(t, h, polarisLogin{
		Sub: "sub-" + name, Sid: "sid-1", Profile: profile(name, name+"@example.com"),
	})))
	if first.Name != name || first.Email == nil || *first.Email != name+"@example.com" {
		t.Fatalf("new User = %+v, want the claims' name and email", first)
	}
	if first.Quota.GPUs != 3 || first.Quota.Sandboxes != 4 {
		t.Fatalf("new User quota = %+v, want the default 3 GPUs and 4 Sandboxes", first.Quota)
	}

	// The same subject with changed claims lands on the same User, updated.
	renamed := name + "-new"
	again := me(t, h, sessionCookie(t, op.Login(t, h, polarisLogin{
		Sub: "sub-" + name, Sid: "sid-2", Profile: profile(renamed, renamed+"@example.com"),
	})))
	if again.ID != first.ID {
		t.Fatalf("second login landed on User %s, want %s", again.ID, first.ID)
	}
	if again.Name != renamed || again.Email == nil || *again.Email != renamed+"@example.com" {
		t.Fatalf("returning User = %+v, want the changed claims copied", again)
	}
}

func TestPolarisLoginNameAndEmailCollisions(t *testing.T) {
	h, op, _ := polarisTestServer(t)
	name := "c-" + uuid.NewString()[:8]
	email := name + "@example.com"
	taken := me(t, h, sessionCookie(t, op.Login(t, h, polarisLogin{Sub: "a-" + name, Profile: profile(name, email)})))

	// A second person with the same username and email still gets in: the
	// name is disambiguated with the subject and the email is left unset.
	other := me(t, h, sessionCookie(t, op.Login(t, h, polarisLogin{Sub: "b-" + name, Profile: profile(name, email)})))
	if other.ID == taken.ID {
		t.Fatal("a different subject matched the existing User")
	}
	if other.Name == name || !strings.HasPrefix(other.Name, name+"-") {
		t.Fatalf("colliding name = %q, want %q disambiguated with the subject", other.Name, name)
	}
	if other.Email != nil {
		t.Fatalf("colliding email stored as %q, want none", *other.Email)
	}

	// A returning User whose new email is taken keeps the stored one.
	own := name + "-own@example.com"
	third := me(t, h, sessionCookie(t, op.Login(t, h, polarisLogin{Sub: "c-" + name, Profile: profile("c3-"+name, own)})))
	third = me(t, h, sessionCookie(t, op.Login(t, h, polarisLogin{Sub: "c-" + name, Profile: profile("c3-"+name, email)})))
	if third.Email == nil || *third.Email != own {
		t.Fatalf("returning User's email = %v, want the stored %q", third.Email, own)
	}
}

func TestPolarisLoginAdminFromEntitlements(t *testing.T) {
	h, op, _ := polarisTestServer(t)
	sub := "admin-" + uuid.NewString()
	admin := sessionCookie(t, op.Login(t, h, polarisLogin{Sub: sub, Entitlements: []string{"orrery:read", "orrery:admin"}}))
	if !me(t, h, admin).IsAdmin {
		t.Fatal("orrery:admin entitlement did not grant admin")
	}
	if code := asSession(t, h, admin, "/api/v1/users", nil); code != http.StatusOK {
		t.Fatalf("admin GET /users = %d, want 200", code)
	}

	// The next login without the entitlement removes it, and the earlier
	// admin Session loses admin authority with it.
	plain := sessionCookie(t, op.Login(t, h, polarisLogin{Sub: sub, Entitlements: []string{"orrery:read"}}))
	if me(t, h, plain).IsAdmin {
		t.Fatal("login without orrery:admin kept admin")
	}
	for _, c := range []*http.Cookie{plain, admin} {
		if code := asSession(t, h, c, "/api/v1/users", nil); code != http.StatusForbidden {
			t.Fatalf("GET /users after admin removed = %d, want 403", code)
		}
	}
}

func TestPolarisLoginRejectsAccessTokenForAnotherAudience(t *testing.T) {
	h, op, _ := polarisTestServer(t)
	resp := op.Login(t, h, polarisLogin{Sub: "aud-" + uuid.NewString(), Audience: "api://other", Entitlements: []string{"orrery:admin"}})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("login with a foreign-audience access token = %d, want 400", resp.StatusCode)
	}
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookieName && c.Value != "" {
			t.Fatal("login with a foreign-audience access token set a Session cookie")
		}
	}
}

// The Session records the Polaris sid so Back-Channel Logout can end it.
func TestPolarisLoginSessionCarriesSid(t *testing.T) {
	h, op, pool := polarisTestServer(t)
	sid := "sid-" + uuid.NewString()
	u := me(t, h, sessionCookie(t, op.Login(t, h, polarisLogin{Sub: "sid-" + uuid.NewString(), Sid: sid})))
	var got string
	if err := pool.QueryRow(context.Background(),
		`SELECT sid FROM api_tokens WHERE user_id = $1 AND kind = 'session'`, u.ID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != sid {
		t.Fatalf("Session sid = %q, want %q", got, sid)
	}
}
