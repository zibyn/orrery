package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func cliLoginCall(t *testing.T, h http.Handler, c *http.Cookie, path, body string, out any) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1"+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if c != nil {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if out != nil && rec.Code/100 == 2 {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatal(err)
		}
	}
	return rec.Code
}

// ADR-0033: start without a token, approve with the Session, and the poll
// hands over a working token exactly once.
func TestCLILoginDeviceFlow(t *testing.T) {
	h, op, _ := polarisTestServer(t)
	cookie := sessionCookie(t, op.Login(t, h, polarisLogin{Sub: "cli-" + uuid.NewString()}))
	user := me(t, h, cookie)

	var login cliLoginDTO
	if code := cliLoginCall(t, h, nil, "/cli-logins", "", &login); code != http.StatusCreated {
		t.Fatalf("start = %d", code)
	}
	if len(login.UserCode) != 9 || login.VerificationURI != "https://orrery.test/cli-auth" || login.Interval != 5 || login.ExpiresIn != 600 {
		t.Fatalf("login = %+v", login)
	}
	poll := func() (int, cliLoginTokenDTO) {
		var out cliLoginTokenDTO
		code := cliLoginCall(t, h, nil, "/cli-logins/token", `{"device_code":"`+login.DeviceCode+`"}`, &out)
		return code, out
	}
	if code, out := poll(); code != http.StatusOK || out.Status != "pending" || out.Token != "" {
		t.Fatalf("poll before approval = %d %+v", code, out)
	}

	// Approval needs a signed-in User.
	approve := `{"user_code":"` + strings.ToLower(strings.ReplaceAll(login.UserCode, "-", " ")) + `"}`
	if code := cliLoginCall(t, h, nil, "/cli-logins/approve", approve, nil); code != http.StatusUnauthorized {
		t.Fatalf("anonymous approve = %d, want 401", code)
	}
	if code := cliLoginCall(t, h, cookie, "/cli-logins/approve", `{"user_code":"AAAA-AAAA"}`, nil); code != http.StatusBadRequest {
		t.Fatalf("approve with vowels = %d, want 400", code)
	}
	if code := cliLoginCall(t, h, cookie, "/cli-logins/approve", approve, nil); code != http.StatusNoContent {
		t.Fatalf("approve = %d", code)
	}
	if code := cliLoginCall(t, h, cookie, "/cli-logins/approve", approve, nil); code != http.StatusNotFound {
		t.Fatalf("second approve = %d, want 404", code)
	}

	code, out := poll()
	if code != http.StatusOK || out.Status != "approved" || out.Token == "" {
		t.Fatalf("poll after approval = %d %+v", code, out)
	}
	var who userDTO
	req := httptest.NewRequest(http.MethodGet, "/api/v1/users/me", nil)
	req.Header.Set("Authorization", "Bearer "+out.Token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &who) != nil || who.ID != user.ID {
		t.Fatalf("token's User = %d %s, want %s", rec.Code, rec.Body, user.ID)
	}
	if code, _ := poll(); code != http.StatusNotFound {
		t.Fatalf("second collect = %d, want 404: the token is handed over once", code)
	}
	if code := cliLoginCall(t, h, nil, "/cli-logins/token", `{"device_code":"nope"}`, nil); code != http.StatusNotFound {
		t.Fatalf("unknown device code = %d, want 404", code)
	}
}

// An expired login can be neither approved nor collected.
func TestCLILoginExpired(t *testing.T) {
	h, op, pool := polarisTestServer(t)
	cookie := sessionCookie(t, op.Login(t, h, polarisLogin{Sub: "cli-" + uuid.NewString()}))
	var login cliLoginDTO
	if code := cliLoginCall(t, h, nil, "/cli-logins", "", &login); code != http.StatusCreated {
		t.Fatalf("start = %d", code)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE cli_logins SET expires_at = now() - interval '1 second' WHERE user_code = $1`, login.UserCode); err != nil {
		t.Fatal(err)
	}
	if code := cliLoginCall(t, h, cookie, "/cli-logins/approve", `{"user_code":"`+login.UserCode+`"}`, nil); code != http.StatusNotFound {
		t.Fatalf("approve expired = %d, want 404", code)
	}
	if code := cliLoginCall(t, h, nil, "/cli-logins/token", `{"device_code":"`+login.DeviceCode+`"}`, nil); code != http.StatusNotFound {
		t.Fatalf("poll expired = %d, want 404", code)
	}
}

func TestNormalizeUserCode(t *testing.T) {
	for in, want := range map[string]string{"bcdf-ghjk": "BCDF-GHJK", "BCDF GHJK": "BCDF-GHJK", "bcdfghjk": "BCDF-GHJK", "BCDF-GHJ": "", "BCDA-GHJK": "", "BCDF-GHJK-B": ""} {
		got, ok := normalizeUserCode(in)
		if ok != (want != "") || got != want {
			t.Errorf("normalizeUserCode(%q) = %q %v, want %q", in, got, ok, want)
		}
	}
	code, err := newUserCode()
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := normalizeUserCode(code); !ok || got != code {
		t.Fatalf("generated %q does not round-trip", code)
	}
}
