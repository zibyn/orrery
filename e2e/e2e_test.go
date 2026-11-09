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

const adminUsername = "admin"

var (
	adminOnce sync.Once
	adminTok  string
	adminErr  error
)

func TestMachineUserToken(t *testing.T) {
	suffix := time.Now().UnixNano()
	username := fmt.Sprintf("alice-%d", suffix)

	alice := userToken(t, "--name", username)
	user := getMe(t, alice)
	if user.Name != username || user.IsAdmin || !user.Machine {
		t.Fatalf("machine User = %+v", user)
	}
	// Rerunning mints another token for the same User.
	if again := getMe(t, userToken(t, "--name", username)); again.ID != user.ID {
		t.Fatalf("rerun User = %s, want %s", again.ID, user.ID)
	}
	// An admin-scoped token needs an admin User, as through the API.
	if out, err := userTokenRaw("--name", username, "--scope", "admin"); err == nil {
		t.Fatalf("admin scope for a non-admin: %s, want an error", out)
	}
	aliceCLI := newCLIRunner(t, alice)

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

	status, data := call(t, http.MethodGet, "/api/v1/users/me", tokenA.Token, nil)
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
	adminCLI := newCLIRunner(t, admin)
	suffix := time.Now().UnixNano()
	username := fmt.Sprintf("bob-%d", suffix)

	bob := userToken(t, "--name", username)
	user := getMe(t, bob)
	bobCLI := newCLIRunner(t, bob)
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

// shortcut: the Git link tests still need a password User for a browser
// Session; e2e has no other Session source after ADR-0029. Replace before #231
// removes POST /sessions.
func login(t *testing.T, username, password, tokenName, scope string) (string, int, []byte) {
	t.Helper()
	status, data, err := postJSON(http.MethodPost, "/api/v1/sessions", "", map[string]any{
		"username": username,
		"password": password,
		"token":    map[string]any{"name": tokenName, "scope": scope},
	})
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusCreated {
		return "", status, data
	}
	var out struct {
		Token string `json:"token"`
	}
	mustJSON(t, data, &out)
	return out.Token, status, data
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
		Action    string          `json:"action"`
		TargetID  string          `json:"target_id"`
		ActorName *string         `json:"actor_name"`
		Detail    json.RawMessage `json:"detail"`
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
	out, err := userTokenRaw("--name", adminUsername, "--admin", "--scope", "admin")
	if err != nil {
		return "", err
	}
	var created struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(out, &created); err != nil {
		return "", fmt.Errorf("user-token output: %v\n%s", err, out)
	}
	return created.Token, nil
}

// userToken mints an API Token with orrery-server user-token inside the
// server container, the only way e2e creates Users (ADR-0029).
func userToken(t *testing.T, args ...string) string {
	t.Helper()
	out, err := userTokenRaw(args...)
	if err != nil {
		t.Fatal(err)
	}
	var created struct {
		Token string `json:"token"`
	}
	mustJSON(t, out, &created)
	return created.Token
}

func userTokenRaw(args ...string) ([]byte, error) {
	full := append(composeArgs(), append([]string{"exec", "-T", "server", "orrery-server", "user-token"}, args...)...)
	cmd := exec.Command("docker", full...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("orrery-server user-token %s: %v\n%s", strings.Join(args, " "), err, stderr.Bytes())
	}
	return out, nil
}

type meUser struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	IsAdmin bool   `json:"is_admin"`
	Machine bool   `json:"machine"`
}

func getMe(t *testing.T, token string) meUser {
	t.Helper()
	status, data := call(t, http.MethodGet, "/api/v1/users/me", token, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /users/me: %d %s", status, data)
	}
	var me meUser
	mustJSON(t, data, &me)
	return me
}

type cliRunner struct {
	path   string
	config string
}

func newCLIRunner(t *testing.T, token string) *cliRunner {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	cfg := fmt.Sprintf(`{"api_url":%q,"token":%q}`, apiURL(), token)
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return &cliRunner{path: cliPath(t), config: path}
}

func (r *cliRunner) run(t *testing.T, stdin string, args ...string) []byte {
	t.Helper()
	full := append([]string{"--config", r.config}, args...)
	cmd := exec.Command(r.path, full...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("orrery %s: %v\n%s", strings.Join(args, " "), err, out)
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
	if v := os.Getenv("ORRERY_E2E_API_URL"); v != "" {
		return v
	}
	return "http://127.0.0.1:18080"
}

func cliPath(t *testing.T) string {
	t.Helper()
	v := os.Getenv("ORRERY_E2E_CLI")
	if v == "" {
		t.Fatal("ORRERY_E2E_CLI is not set; run make e2e")
	}
	return v
}

func composeArgs() []string {
	file := os.Getenv("ORRERY_E2E_COMPOSE_FILE")
	project := os.Getenv("ORRERY_E2E_COMPOSE_PROJECT")
	if file == "" || project == "" {
		return nil
	}
	return []string{"compose", "-f", file, "-p", project}
}
