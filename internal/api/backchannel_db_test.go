package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// logoutClaims is a valid Back-Channel Logout token's claims for (sub, sid).
func (f *fakePolaris) logoutClaims(sub, sid string) map[string]any {
	now := time.Now().Unix()
	return map[string]any{
		"iss": f.URL, "aud": f.ClientID, "sub": sub, "sid": sid,
		"iat": now, "exp": now + 120, "jti": uuid.NewString(),
		"events": map[string]any{"http://schemas.openid.net/event/backchannel-logout": map[string]any{}},
	}
}

// backchannelLogout posts a logout_token the way Polaris delivers it.
func backchannelLogout(h http.Handler, token string) int {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/polaris/backchannel-logout",
		strings.NewReader(url.Values{"logout_token": {token}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

// apiToken creates an API Token through the Session cookie c.
func apiToken(t *testing.T, h http.Handler, c *http.Cookie) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/users/me/tokens", strings.NewReader(`{"name":"cli","scope":"user"}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out createdTokenDTO
	if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
		t.Fatalf("create token = %d: %s", rec.Code, rec.Body)
	}
	return out.Token
}

func asBearer(h http.Handler, token, path string) int {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

// Logging out in Polaris ends only the Orrery Session with that sid.
func TestBackchannelLogoutEndsOnlyTheMatchingSession(t *testing.T) {
	h, op, _ := polarisTestServer(t)
	sub, sid := "bcl-"+uuid.NewString(), "sid-"+uuid.NewString()
	ended := sessionCookie(t, op.Login(t, h, polarisLogin{Sub: sub, Sid: sid}))
	other := sessionCookie(t, op.Login(t, h, polarisLogin{Sub: sub, Sid: "sid-" + uuid.NewString()}))
	token := apiToken(t, h, ended)

	if code := backchannelLogout(h, op.Sign("logout+jwt", op.logoutClaims(sub, sid))); code != http.StatusOK {
		t.Fatalf("backchannel logout = %d, want 200", code)
	}
	if code := asSession(t, h, ended, "/api/v1/users/me", nil); code != http.StatusUnauthorized {
		t.Fatalf("ended Session = %d, want 401", code)
	}
	if code := asSession(t, h, other, "/api/v1/users/me", nil); code != http.StatusOK {
		t.Fatalf("other Session = %d, want 200", code)
	}
	if code := asBearer(h, token, "/api/v1/users/me"); code != http.StatusOK {
		t.Fatalf("API Token = %d, want 200", code)
	}
}

func TestBackchannelLogoutUnknownSidIsAcknowledged(t *testing.T) {
	h, op, _ := polarisTestServer(t)
	sub := "bcl-" + uuid.NewString()
	c := sessionCookie(t, op.Login(t, h, polarisLogin{Sub: sub, Sid: "sid-" + uuid.NewString()}))
	if code := backchannelLogout(h, op.Sign("logout+jwt", op.logoutClaims(sub, "sid-"+uuid.NewString()))); code != http.StatusOK {
		t.Fatalf("unknown sid = %d, want 200", code)
	}
	if code := asSession(t, h, c, "/api/v1/users/me", nil); code != http.StatusOK {
		t.Fatalf("Session after unknown-sid logout = %d, want 200", code)
	}
}

func TestBackchannelLogoutRejectsInvalidTokens(t *testing.T) {
	h, op, _ := polarisTestServer(t)
	sub, sid := "bcl-"+uuid.NewString(), "sid-"+uuid.NewString()
	c := sessionCookie(t, op.Login(t, h, polarisLogin{Sub: sub, Sid: sid}))
	with := func(k string, v any) map[string]any {
		claims := op.logoutClaims(sub, sid)
		claims[k] = v
		return claims
	}
	valid := op.Sign("logout+jwt", op.logoutClaims(sub, sid))
	unsigned := strings.Join(strings.Split(valid, ".")[:2], ".") + "."
	for name, token := range map[string]string{
		"garbage":        "not-a-jwt",
		"missing":        "",
		"unsigned":       unsigned,
		"expired":        op.Sign("logout+jwt", with("exp", time.Now().Add(-time.Minute).Unix())),
		"wrong audience": op.Sign("logout+jwt", with("aud", op.Audience)),
		"wrong typ":      op.Sign("JWT", op.logoutClaims(sub, sid)),
	} {
		if code := backchannelLogout(h, token); code != http.StatusBadRequest {
			t.Errorf("%s token = %d, want 400", name, code)
		}
	}
	if code := asSession(t, h, c, "/api/v1/users/me", nil); code != http.StatusOK {
		t.Fatalf("Session after rejected logouts = %d, want 200", code)
	}
}
