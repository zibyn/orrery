//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	adminUsername = "admin"
	adminEmail    = "admin@example.com"
	adminPassword = "admin-password-123"
)

var (
	adminOnce sync.Once
	adminTok  string
	adminErr  error
)

func TestBootstrapAndToken(t *testing.T) {
	admin := adminToken(t)
	adminCLI := newRunner(t, admin)
	suffix := time.Now().UnixNano()
	username := fmt.Sprintf("alice-%d", suffix)

	created := adminCLI.run(t, "alice-password-123\n", "user", "create",
		"--name", username, "--login-type", "password", "--password-stdin")
	var user struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		IsAdmin bool   `json:"is_admin"`
	}
	mustJSON(t, created, &user)
	if user.Name != username || user.IsAdmin {
		t.Fatalf("created user = %+v", user)
	}

	alice, status, data := login(t, username, "alice-password-123", fmt.Sprintf("first-%d", suffix), "user")
	if status != http.StatusCreated {
		t.Fatalf("login as %s: %d %s", username, status, data)
	}
	aliceCLI := newRunner(t, alice)

	first := aliceCLI.run(t, "", "token", "create", "--name", fmt.Sprintf("ci-%d", suffix), "--scope", "read_only")
	var tokenA struct {
		ID    string `json:"id"`
		Token string `json:"token"`
		Scope string `json:"scope"`
	}
	mustJSON(t, first, &tokenA)
	if tokenA.Token == "" || tokenA.Scope != "read_only" {
		t.Fatalf("created token = %+v", tokenA)
	}

	second := aliceCLI.run(t, "", "token", "create", "--name", fmt.Sprintf("ops-%d", suffix), "--scope", "user")
	var tokenB struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	mustJSON(t, second, &tokenB)

	aliceCLI.run(t, "", "token", "revoke", tokenA.ID)

	status, data = call(t, http.MethodGet, "/api/v1/users/me", tokenA.Token, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("revoked token: got %d, want 401 (%s)", status, data)
	}
	var problem struct {
		Code string `json:"code"`
	}
	mustJSON(t, data, &problem)
	if problem.Code != "unauthenticated" {
		t.Fatalf("revoked token code = %q, want unauthenticated", problem.Code)
	}

	status, data = call(t, http.MethodGet, "/api/v1/users/me", tokenB.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("surviving token: got %d, want 200 (%s)", status, data)
	}
	var me struct {
		Name string `json:"name"`
	}
	mustJSON(t, data, &me)
	if me.Name != username {
		t.Fatalf("GET /users/me name = %q, want %q", me.Name, username)
	}
}

func TestAuditQuery(t *testing.T) {
	admin := adminToken(t)
	adminCLI := newRunner(t, admin)
	suffix := time.Now().UnixNano()
	username := fmt.Sprintf("bob-%d", suffix)

	created := adminCLI.run(t, "bob-password-123\n", "user", "create",
		"--name", username, "--login-type", "password", "--password-stdin")
	var user struct {
		ID string `json:"id"`
	}
	mustJSON(t, created, &user)

	bob, status, data := login(t, username, "bob-password-123", fmt.Sprintf("bob-first-%d", suffix), "user")
	if status != http.StatusCreated {
		t.Fatalf("login as %s: %d %s", username, status, data)
	}
	bobCLI := newRunner(t, bob)
	made := bobCLI.run(t, "", "token", "create", "--name", fmt.Sprintf("bob-ci-%d", suffix), "--scope", "user")
	var token struct {
		ID string `json:"id"`
	}
	mustJSON(t, made, &token)
	bobCLI.run(t, "", "token", "revoke", token.ID)

	revoked := adminCLI.run(t, "", "audit", "list", "--actor", username, "--action", "token.revoke", "--target", token.ID)
	var revokedList auditList
	mustJSON(t, revoked, &revokedList)
	if len(revokedList.Items) == 0 {
		t.Fatalf("no token.revoke record for %s", username)
	}
	if revokedList.Items[0].Action != "token.revoke" || revokedList.Items[0].TargetID != token.ID {
		t.Fatalf("audit record = %+v", revokedList.Items[0])
	}
	if revokedList.Items[0].ActorName == nil || *revokedList.Items[0].ActorName != username {
		t.Fatalf("actor name = %v, want %q", revokedList.Items[0].ActorName, username)
	}

	createdList := adminCLI.run(t, "", "audit", "list", "--action", "user.create", "--target", user.ID)
	var userCreates auditList
	mustJSON(t, createdList, &userCreates)
	if len(userCreates.Items) == 0 {
		t.Fatalf("no user.create record for %s", user.ID)
	}

	issuedList := adminCLI.run(t, "", "audit", "list", "--actor", username, "--action", "token.create")
	var issued auditList
	mustJSON(t, issuedList, &issued)
	if len(issued.Items) == 0 {
		t.Fatalf("no token.create record for %s", username)
	}
}

// TestPasswordSession is the "password-session" acceptance scenario (#48).
func TestPasswordSession(t *testing.T) {
	admin := adminToken(t)
	adminCLI := newRunner(t, admin)
	suffix := time.Now().UnixNano()
	username := fmt.Sprintf("carol-%d", suffix)
	password := "carol-password-123"
	var userID string
	adminCLI.run(t, password+"\n", "user", "create",
		"--name", username, "--login-type", "password", "--password-stdin")

	// A password login sets the Session cookie, and the cookie authenticates.
	resp, data := request(t, http.MethodPost, "/api/v1/sessions", nil, nil, map[string]any{
		"username": username, "password": password,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("password login: %d %s", resp.StatusCode, data)
	}
	cookies := resp.Cookies()
	if len(cookies) != 1 || cookies[0].Name != "stars_session" {
		t.Fatalf("Set-Cookie = %v, want one stars_session", resp.Header.Values("Set-Cookie"))
	}
	session := cookies[0]
	if !session.HttpOnly || !session.Secure || session.SameSite != http.SameSiteLaxMode || session.Path != "/" || session.Domain != "" {
		t.Fatalf("session cookie attributes = %+v", session)
	}
	if status, body := callWithCookie(t, http.MethodGet, "/api/v1/users/me", session, nil, nil); status != http.StatusOK {
		t.Fatalf("GET /users/me with cookie: %d %s", status, body)
	} else {
		var me struct {
			ID string `json:"id"`
		}
		mustJSON(t, body, &me)
		userID = me.ID
	}

	// A cross-origin unsafe request carrying the cookie is rejected; the same
	// request from the API's own origin is not.
	crossOrigin := map[string]string{"Origin": "http://evil.example", "Sec-Fetch-Site": "cross-site"}
	status, body := callWithCookie(t, http.MethodPost, "/api/v1/users/me/tokens", session, map[string]any{
		"name": fmt.Sprintf("cross-%d", suffix), "scope": "user",
	}, crossOrigin)
	if status != http.StatusForbidden {
		t.Fatalf("cross-origin POST: %d %s, want 403", status, body)
	}
	requireProblemCode(t, body, "permission_denied")

	sameOrigin := map[string]string{"Origin": apiOrigin(), "Sec-Fetch-Site": "same-origin"}
	status, body = callWithCookie(t, http.MethodPost, "/api/v1/users/me/tokens", session, map[string]any{
		"name": fmt.Sprintf("same-%d", suffix), "scope": "user",
	}, sameOrigin)
	if status != http.StatusCreated {
		t.Fatalf("same-origin POST: %d %s, want 201", status, body)
	}

	// A cookie-authenticated WebSocket upgrade must carry the API's origin.
	upgrade := map[string]string{"Connection": "Upgrade", "Upgrade": "websocket", "Origin": "http://evil.example"}
	status, body = callWithCookie(t, http.MethodGet, "/api/v1/users/me", session, nil, upgrade)
	if status != http.StatusForbidden {
		t.Fatalf("WebSocket upgrade with a foreign Origin: %d %s, want 403", status, body)
	}
	requireProblemCode(t, body, "permission_denied")
	upgrade["Origin"] = apiOrigin()
	if status, body = callWithCookie(t, http.MethodGet, "/api/v1/users/me", session, nil, upgrade); status != http.StatusOK {
		t.Fatalf("WebSocket upgrade with the API Origin: %d %s, want 200", status, body)
	}

	// Changing the password revokes every Session of that User.
	newPassword := "carol-new-password-456"
	status, body = callWithCookie(t, http.MethodPut, "/api/v1/users/me/password", session, map[string]any{
		"old": password, "new": newPassword,
	}, sameOrigin)
	if status != http.StatusOK {
		t.Fatalf("change password: %d %s", status, body)
	}
	if status, body = callWithCookie(t, http.MethodGet, "/api/v1/users/me", session, nil, nil); status != http.StatusUnauthorized {
		t.Fatalf("old Session after password change: %d %s, want 401", status, body)
	}

	// The new password logs in; DELETE /sessions/current ends that Session.
	newSession := loginSession(t, username, newPassword)
	status, body = callWithCookie(t, http.MethodDelete, "/api/v1/sessions/current", newSession, nil, sameOrigin)
	if status != http.StatusAccepted {
		t.Fatalf("logout: %d %s, want 202", status, body)
	}
	if status, body = callWithCookie(t, http.MethodGet, "/api/v1/users/me", newSession, nil, nil); status != http.StatusUnauthorized {
		t.Fatalf("Session after logout: %d %s, want 401", status, body)
	}

	// An admin reset revokes Sessions too.
	session3 := loginSession(t, username, newPassword)
	status, body = call(t, http.MethodPut, "/api/v1/users/"+userID+"/password", admin, map[string]any{
		"password": "carol-reset-password-789",
	})
	if status != http.StatusOK {
		t.Fatalf("admin reset password: %d %s", status, body)
	}
	if status, body = callWithCookie(t, http.MethodGet, "/api/v1/users/me", session3, nil, nil); status != http.StatusUnauthorized {
		t.Fatalf("Session after admin reset: %d %s, want 401", status, body)
	}
}

func TestLoginRateLimit(t *testing.T) {
	admin := adminToken(t)
	adminCLI := newRunner(t, admin)
	suffix := time.Now().UnixNano()
	username := fmt.Sprintf("dave-%d", suffix)
	password := "dave-password-123"
	adminCLI.run(t, password+"\n", "user", "create",
		"--name", username, "--login-type", "password", "--password-stdin")

	for i := 0; i < 10; i++ {
		status, body := postSession(t, username, "wrong-password")
		if status != http.StatusUnauthorized {
			t.Fatalf("failed login %d: %d %s, want 401", i+1, status, body)
		}
	}
	resp, body := request(t, http.MethodPost, "/api/v1/sessions", nil, nil, map[string]any{
		"username": username, "password": "wrong-password",
	})
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("over-limit login: %d %s, want 429", resp.StatusCode, body)
	}
	requireProblemCode(t, body, "rate_limited")
	if retry := resp.Header.Get("Retry-After"); retry == "" {
		t.Fatal("rate-limited response has no Retry-After")
	}
	// The limit is checked before the password, so even the right one waits.
	if status, body := postSession(t, username, password); status != http.StatusTooManyRequests {
		t.Fatalf("correct password during the block: %d %s, want 429", status, body)
	}
}

func TestCLILogin(t *testing.T) {
	admin := adminToken(t)
	adminCLI := newRunner(t, admin)
	suffix := time.Now().UnixNano()
	username := fmt.Sprintf("erin-%d", suffix)
	password := "erin-password-123"
	adminCLI.run(t, password+"\n", "user", "create",
		"--name", username, "--login-type", "password", "--password-stdin")

	config := filepath.Join(t.TempDir(), "config.json")
	cmd := exec.Command(cliPath(t), "--config", config, "--api-url", apiURL(), "login",
		"--username", username, "--name", fmt.Sprintf("cli-%d", suffix), "--password-stdin")
	cmd.Stdin = strings.NewReader(password + "\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("stars-sandbox login: %v\n%s", err, out)
	}

	out, err := exec.Command(cliPath(t), "--config", config, "user", "get", "me").CombinedOutput()
	if err != nil {
		t.Fatalf("user get me with the stored token: %v\n%s", err, out)
	}
	var me struct {
		Name string `json:"name"`
	}
	mustJSON(t, out, &me)
	if me.Name != username {
		t.Fatalf("logged-in User = %q, want %q", me.Name, username)
	}
}

func loginSession(t *testing.T, username, password string) *http.Cookie {
	t.Helper()
	resp, data := request(t, http.MethodPost, "/api/v1/sessions", nil, nil, map[string]any{
		"username": username, "password": password,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("login as %s: %d %s", username, resp.StatusCode, data)
	}
	cookies := resp.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("login Set-Cookie = %v", resp.Header.Values("Set-Cookie"))
	}
	return cookies[0]
}

func postSession(t *testing.T, username, password string) (int, []byte) {
	resp, data := request(t, http.MethodPost, "/api/v1/sessions", nil, nil, map[string]any{
		"username": username, "password": password,
	})
	return resp.StatusCode, data
}

// callWithCookie sends body with the Session cookie and extra headers.
func callWithCookie(t *testing.T, method, path string, cookie *http.Cookie, body any, headers map[string]string) (int, []byte) {
	t.Helper()
	resp, data := request(t, method, path, headers, cookie, body)
	return resp.StatusCode, data
}

func request(t *testing.T, method, path string, headers map[string]string, cookie *http.Cookie, body any) (*http.Response, []byte) {
	t.Helper()
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode body: %v", err)
		}
		payload = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, apiURL()+path, payload)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s %s: %v", method, path, err)
	}
	return resp, data
}

func requireProblemCode(t *testing.T, data []byte, want string) {
	t.Helper()
	var problem struct {
		Code string `json:"code"`
	}
	mustJSON(t, data, &problem)
	if problem.Code != want {
		t.Fatalf("problem code = %q, want %q (%s)", problem.Code, want, data)
	}
}

func apiOrigin() string {
	u, err := url.Parse(apiURL())
	if err != nil {
		return apiURL()
	}
	return u.Scheme + "://" + u.Host
}

type auditList struct {
	Items []struct {
		Action    string  `json:"action"`
		TargetID  string  `json:"target_id"`
		ActorName *string `json:"actor_name"`
	} `json:"items"`
}

func adminToken(t *testing.T) string {
	t.Helper()
	adminOnce.Do(func() { adminTok, adminErr = ensureAdmin() })
	if adminErr != nil {
		t.Fatal(adminErr)
	}
	return adminTok
}

func ensureAdmin() (string, error) {
	for attempt := 0; attempt < 2; attempt++ {
		tok, status, data, err := loginRaw(adminUsername, adminPassword, "e2e-admin", "admin")
		if err != nil {
			return "", err
		}
		if status == http.StatusCreated {
			return tok, nil
		}
		if status != http.StatusUnauthorized {
			return "", fmt.Errorf("admin login: %d %s", status, data)
		}
		if attempt == 0 {
			if err := bootstrap(); err != nil {
				return "", err
			}
		}
	}
	return "", fmt.Errorf("admin login failed after bootstrap")
}

// bootstrap runs the server-side CLI inside the server container. Running it
// twice against an already bootstrapped database is harmless for the test.
func bootstrap() error {
	args := append(composeArgs(), "exec", "-T", "server",
		"stars-sandbox-server", "bootstrap",
		"--username", adminUsername, "--email", adminEmail, "--password-stdin")
	cmd := exec.Command("docker", args...)
	cmd.Stdin = strings.NewReader(adminPassword + "\n")
	out, err := cmd.CombinedOutput()
	if err != nil && !strings.Contains(string(out), "a User already exists") {
		return fmt.Errorf("bootstrap: %v\n%s", err, out)
	}
	return nil
}

func login(t *testing.T, username, password, tokenName, scope string) (string, int, []byte) {
	t.Helper()
	tok, status, data, err := loginRaw(username, password, tokenName, scope)
	if err != nil {
		t.Fatal(err)
	}
	return tok, status, data
}

func loginRaw(username, password, tokenName, scope string) (string, int, []byte, error) {
	status, data, err := postJSON(http.MethodPost, "/api/v1/sessions", "", map[string]any{
		"username": username,
		"password": password,
		"token":    map[string]any{"name": tokenName, "scope": scope},
	})
	if err != nil || status != http.StatusCreated {
		return "", status, data, err
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", status, data, err
	}
	return out.Token, status, data, nil
}

type runner struct {
	path   string
	config string
}

func newRunner(t *testing.T, token string) *runner {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	cfg := fmt.Sprintf(`{"api_url":%q,"token":%q}`, apiURL(), token)
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return &runner{path: cliPath(t), config: path}
}

func (r *runner) run(t *testing.T, stdin string, args ...string) []byte {
	t.Helper()
	full := append([]string{"--config", r.config}, args...)
	cmd := exec.Command(r.path, full...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("stars-sandbox %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func call(t *testing.T, method, path, token string, body any) (int, []byte) {
	t.Helper()
	status, data, err := postJSON(method, path, token, body)
	if err != nil {
		t.Fatal(err)
	}
	return status, data
}

func postJSON(method, path, token string, body any) (int, []byte, error) {
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		payload = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, apiURL()+path, payload)
	if err != nil {
		return 0, nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	return resp.StatusCode, data, err
}

func mustJSON(t *testing.T, data []byte, dst any) {
	t.Helper()
	if err := json.Unmarshal(data, dst); err != nil {
		t.Fatalf("decode %s: %v", data, err)
	}
}

func apiURL() string {
	if v := os.Getenv("STARS_E2E_API_URL"); v != "" {
		return v
	}
	return "http://127.0.0.1:18080"
}

func cliPath(t *testing.T) string {
	t.Helper()
	v := os.Getenv("STARS_E2E_CLI")
	if v == "" {
		t.Fatal("STARS_E2E_CLI is not set; run make e2e")
	}
	return v
}

func composeArgs() []string {
	file := os.Getenv("STARS_E2E_COMPOSE_FILE")
	project := os.Getenv("STARS_E2E_COMPOSE_PROJECT")
	if file == "" || project == "" {
		return nil
	}
	return []string{"compose", "-f", file, "-p", project}
}
