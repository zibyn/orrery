//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestGitLinkStateBinding is the git-link-state-binding scenario (#79): a Git
// authorization started by User A cannot be finished with User B's Session,
// and the failed callback stores no link. A then links and unlinks the same
// provider through the API and the external CLI.
func TestGitLinkStateBinding(t *testing.T) {
	providerID := envOr("ORRERY_E2E_GIT_PROVIDER", "gitea")
	giteaUser := envOr("ORRERY_E2E_GITEA_USER", "gitea-user")
	giteaPassword := envOr("ORRERY_E2E_GITEA_PASSWORD", "gitea-user-password-123")

	admin := adminToken(t)
	adminCLI := newCLIRunner(t, admin)
	suffix := time.Now().UnixNano()
	userA := fmt.Sprintf("git-a-%d", suffix)
	userB := fmt.Sprintf("git-b-%d", suffix)
	const passwordA = "git-a-password-123"
	const passwordB = "git-b-password-123"
	for _, user := range []struct{ name, password string }{{userA, passwordA}, {userB, passwordB}} {
		adminCLI.run(t, user.password+"\n", "user", "create",
			"--name", user.name, "--login-type", "password", "--password-stdin")
	}
	sessionA := loginSession(t, userA, passwordA)
	sessionB := loginSession(t, userB, passwordB)

	// A browser with no Session is sent to the console login page and comes
	// back here afterwards.
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	req, err := http.NewRequest(http.MethodGet, apiURL()+"/api/v1/git-providers/"+providerID+"/authorize", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || !strings.HasPrefix(resp.Header.Get("Location"), "/login?return=") {
		t.Fatalf("authorize without a Session: %d %q, want 302 to /login", resp.StatusCode, resp.Header.Get("Location"))
	}

	// User A starts the flow, producing a provider URL whose `state` is bound
	// to A.
	authorizeURL := startGitAuthorize(t, providerID, sessionA)

	// The browser authenticates at Gitea and produces the callback URL. In
	// this scenario that browser belongs to User B: it carries no state of
	// its own, only A's encrypted `state` from the link.
	browser := newGiteaBrowser(t)
	callbackURL := browser.loginAndAuthorize(t, authorizeURL, giteaUser, giteaPassword)

	status, location, body := finishGitAuthorize(t, callbackURL, sessionB)
	if status != http.StatusForbidden {
		t.Fatalf("callback with another User's Session: %d %q %s, want 403", status, location, body)
	}
	requireProblemCode(t, body, "permission_denied")

	if links := listGitLinks(t, sessionA); len(links) != 0 {
		t.Fatalf("User A has links after the rejected callback: %v", links)
	}
	if links := listGitLinks(t, sessionB); len(links) != 0 {
		t.Fatalf("User B has links after the rejected callback: %v", links)
	}

	// A completes the same flow with their own Session.
	authorizeURL = startGitAuthorize(t, providerID, sessionA)
	browser = newGiteaBrowser(t)
	callbackURL = browser.loginAndAuthorize(t, authorizeURL, giteaUser, giteaPassword)
	status, location, body = finishGitAuthorize(t, callbackURL, sessionA)
	if status != http.StatusFound {
		t.Fatalf("successful callback: %d %s, want 302", status, body)
	}
	if location != "/git" {
		t.Fatalf("callback Location = %q, want /git", location)
	}
	if links := listGitLinks(t, sessionA); len(links) != 1 || links[0] != providerID {
		t.Fatalf("User A links = %v, want [%s]", links, providerID)
	}

	// The external CLI sees the link and removes it.
	tokenA, statusCode, data := login(t, userA, passwordA, fmt.Sprintf("git-cli-%d", suffix), "user")
	if statusCode != http.StatusCreated {
		t.Fatalf("login for CLI: %d %s", statusCode, data)
	}
	cli := newCLIRunner(t, tokenA)
	out := cli.run(t, "", "git", "links")
	if !strings.Contains(string(out), `"provider_id": "`+providerID+`"`) {
		t.Fatalf("git links output = %s", out)
	}
	cli.run(t, "", "git", "unlink", providerID)
	out = cli.run(t, "", "git", "links")
	if strings.Contains(string(out), `"provider_id": "`+providerID+`"`) {
		t.Fatalf("git unlink left the link: %s", out)
	}
}

// startGitAuthorize runs GET /git-providers/{id}/authorize and returns the
// provider redirect.
func startGitAuthorize(t *testing.T, providerID string, session *http.Cookie) string {
	t.Helper()
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	req, err := http.NewRequest(http.MethodGet,
		apiURL()+"/api/v1/git-providers/"+url.PathEscape(providerID)+"/authorize", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(session)
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatalf("GET authorize: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		data, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET authorize = %d, want 302: %s", resp.StatusCode, data)
	}
	return resp.Header.Get("Location")
}

// finishGitAuthorize requests the provider callback with the Session cookie.
func finishGitAuthorize(t *testing.T, callbackURL string, session *http.Cookie) (int, string, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, callbackURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(session)
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatalf("GET callback: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("Location"), body
}

func listGitLinks(t *testing.T, session *http.Cookie) []string {
	t.Helper()
	status, data := callWithCookie(t, http.MethodGet, "/api/v1/users/me/git-links", session, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /users/me/git-links: %d %s", status, data)
	}
	var out struct {
		Items []struct {
			ProviderID string `json:"provider_id"`
		} `json:"items"`
	}
	mustJSON(t, data, &out)
	ids := make([]string, 0, len(out.Items))
	for _, item := range out.Items {
		ids = append(ids, item.ProviderID)
	}
	return ids
}

var (
	giteaCSRF        = regexp.MustCompile(`name="_csrf" value="([^"]+)"`)
	giteaHiddenInput = regexp.MustCompile(`<input type="hidden" name="([^"]+)" value="([^"]*)"`)
	giteaGrantForm   = regexp.MustCompile(`<form method="post" action="(/login/oauth/grant)">`)
)

// giteaRedirectLimit bounds the login/grant/authorize redirect chain.
const giteaRedirectLimit = 6

// giteaBrowser is the browser half of Gitea's OAuth flow. Redirects are
// followed by hand so the flow stops at the Control Plane's callback.
type giteaBrowser struct {
	base   string
	client *http.Client
}

func newGiteaBrowser(t *testing.T) *giteaBrowser {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &giteaBrowser{
		base: envOr("ORRERY_E2E_GITEA_URL", "http://127.0.0.1:13000"),
		client: &http.Client{
			Jar: jar,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

func (b *giteaBrowser) get(t *testing.T, rawURL string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	return b.do(t, req)
}

func (b *giteaBrowser) login(t *testing.T, username, password string) {
	t.Helper()
	resp, page := b.get(t, b.base+"/user/login")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Gitea login page: %d %s", resp.StatusCode, page)
	}
	match := giteaCSRF.FindSubmatch(page)
	if match == nil {
		t.Fatalf("Gitea login page has no _csrf token: %s", page)
	}
	form := url.Values{
		"user_name": {username},
		"password":  {password},
		"_csrf":     {string(match[1])},
	}
	req, err := http.NewRequest(http.MethodPost, b.base+"/user/login", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err = b.client.Do(req)
	if err != nil {
		t.Fatalf("POST Gitea login: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound && resp.StatusCode != http.StatusSeeOther {
		data, _ := io.ReadAll(resp.Body)
		t.Fatalf("Gitea login: %d %s", resp.StatusCode, data)
	}
}

// loginAndAuthorize signs in and follows Gitea's side of the OAuth flow until
// it points at the Control Plane, returning that callback URL.
func (b *giteaBrowser) loginAndAuthorize(t *testing.T, authorizeURL, username, password string) string {
	t.Helper()
	b.login(t, username, password)
	baseURL, err := url.Parse(b.base)
	if err != nil {
		t.Fatal(err)
	}
	next := authorizeURL
	for range giteaRedirectLimit {
		resp, page := b.get(t, next)
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			next, external := b.redirect(t, baseURL, resp)
			if external {
				return next
			}
			continue
		}
		// Gitea still asks the user to authorize the application. Submit its
		// form; this deployment's skip_secondary_authorization only applies to
		// already-granted applications.
		if form := giteaGrantForm.FindSubmatch(page); form != nil {
			grantURL := resp.Request.URL.ResolveReference(&url.URL{Path: string(form[1])}).String()
			resp, page = b.postForm(t, grantURL, giteaGrantFields(t, page))
			if resp.StatusCode < 300 || resp.StatusCode >= 400 {
				t.Fatalf("Gitea grant: %d %s", resp.StatusCode, page)
			}
			next, external := b.redirect(t, baseURL, resp)
			if external {
				return next
			}
			continue
		}
		t.Fatalf("Gitea authorization stopped at %d: %s", resp.StatusCode, page)
	}
	t.Fatal("Gitea authorization did not reach the callback")
	return ""
}

// redirect resolves a Gitea response's Location, reporting whether it left
// Gitea for the Control Plane's callback.
func (b *giteaBrowser) redirect(t *testing.T, baseURL *url.URL, resp *http.Response) (string, bool) {
	t.Helper()
	parsed, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("Gitea redirect %q: %v", resp.Header.Get("Location"), err)
	}
	target := resp.Request.URL.ResolveReference(parsed)
	return target.String(), target.Host != baseURL.Host
}

// giteaGrantFields copies the authorization form's hidden inputs and grants
// the application.
func giteaGrantFields(t *testing.T, page []byte) url.Values {
	t.Helper()
	fields := url.Values{"granted": {"true"}}
	for _, match := range giteaHiddenInput.FindAllSubmatch(page, -1) {
		fields.Set(string(match[1]), string(match[2]))
	}
	if fields.Get("client_id") == "" {
		t.Fatalf("Gitea grant page has no client_id: %s", page)
	}
	return fields
}

func (b *giteaBrowser) postForm(t *testing.T, rawURL string, form url.Values) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, rawURL, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := b.client.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", rawURL, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp, data
}

// gitFixture is a User-owned ready Sandbox whose owner may or may not have
// linked the e2e Gitea provider.
type gitFixture struct {
	userID       string
	userToken    string
	sandboxID    string
	sandboxToken string
}

func gitProviderID() string    { return envOr("ORRERY_E2E_GIT_PROVIDER", "gitea") }
func gitGiteaUser() string     { return envOr("ORRERY_E2E_GITEA_USER", "gitea-user") }
func gitGiteaPassword() string { return envOr("ORRERY_E2E_GITEA_PASSWORD", "gitea-user-password-123") }

func giteaBase() string { return envOr("ORRERY_E2E_GITEA_URL", "http://127.0.0.1:13000") }

// newGitFixture creates a User with sandbox quota and a ready Sandbox they
// own, optionally linking the e2e Gitea provider first.
func newGitFixture(t *testing.T, admin string, link bool) *gitFixture {
	t.Helper()
	suffix := time.Now().UnixNano()
	username := fmt.Sprintf("git-cred-%d", suffix)
	const password = "git-cred-password-123"
	adminCLI := newCLIRunner(t, admin)
	adminCLI.run(t, password+"\n", "user", "create",
		"--name", username, "--login-type", "password", "--password-stdin")

	userToken, status, data := login(t, username, password, fmt.Sprintf("git-cred-%d", suffix), "user")
	if status != http.StatusCreated {
		t.Fatalf("login as %s: %d %s", username, status, data)
	}
	status, data = call(t, http.MethodGet, "/api/v1/users/me", userToken, nil)
	if status != http.StatusOK {
		t.Fatalf("get me: %d %s", status, data)
	}
	var me struct {
		ID string `json:"id"`
	}
	mustJSON(t, data, &me)
	setUserSandboxQuota(t, admin, me.ID, 5)

	if link {
		linkGitea(t, loginSession(t, username, password))
	}

	nodeName := fmt.Sprintf("git-cred-node-%d", suffix)
	state, dataRoot := enrollAgent(t, admin, nodeName)
	// Git inside a Sandbox runs as root and leaves root-owned files under the
	// Workspace; hand them back before t.TempDir removes the tree.
	t.Cleanup(func() {
		_ = exec.Command("docker", "run", "--rm", "-v", dataRoot+":/w",
			"alpine:3.22", "chmod", "-R", "a+rwX", "/w").Run()
	})
	startAgent(t, state, dataRoot)
	waitForNodeReady(t, admin, nodeName)
	sandboxID := createReadySandbox(t, userToken, fmt.Sprintf("git-cred-%d", suffix), 256<<20)

	stdout, stderr, code := execInSandbox(t, admin, sandboxID, "sh", "-c", `printf %s "$ORRERY_SANDBOX_TOKEN"`)
	if code != 0 || !strings.HasPrefix(stdout, "ors_") {
		t.Fatalf("Sandbox Token: exit %d, stdout %q, stderr %s", code, stdout, stderr)
	}
	return &gitFixture{
		userID:       me.ID,
		userToken:    userToken,
		sandboxID:    sandboxID,
		sandboxToken: stdout,
	}
}

// linkGitea completes the provider's OAuth flow for a Control Plane Session.
func linkGitea(t *testing.T, session *http.Cookie) *giteaBrowser {
	t.Helper()
	authorizeURL := startGitAuthorize(t, gitProviderID(), session)
	browser := newGiteaBrowser(t)
	callbackURL := browser.loginAndAuthorize(t, authorizeURL, gitGiteaUser(), gitGiteaPassword())
	status, location, body := finishGitAuthorize(t, callbackURL, session)
	if status != http.StatusFound {
		t.Fatalf("Git callback: %d %q %s, want 302", status, location, body)
	}
	return browser
}

func setUserSandboxQuota(t *testing.T, admin, userID string, sandboxes int) {
	t.Helper()
	status, data := call(t, http.MethodPut, "/api/v1/users/"+userID+"/quota", admin, map[string]any{
		"gpus": 0, "sandboxes": sandboxes,
	})
	if status != http.StatusOK {
		t.Fatalf("set quota for %s: %d %s", userID, status, data)
	}
}

// installGit adds git to an Alpine Sandbox from the package mirror. The Git
// scenarios also reach Gitea and the Control Plane from inside the Sandbox, so
// they skip on a Node whose Sandboxes have no such route, which the harness
// declares with ORRERY_E2E_SANDBOX_NO_EGRESS (the shared containerd host).
func installGit(t *testing.T, admin, sandboxID string) {
	t.Helper()
	if noSandboxEgress() {
		t.Skip("ORRERY_E2E_SANDBOX_NO_EGRESS: this Node's Sandboxes reach neither the Alpine mirror nor Gitea")
	}
	if out, errOut, code := execInSandbox(t, admin, sandboxID, "apk", "add", "--no-cache", "git"); code != 0 {
		t.Fatalf("install git: exit %d\n%s%s", code, out, errOut)
	}
}

func noSandboxEgress() bool { return os.Getenv("ORRERY_E2E_SANDBOX_NO_EGRESS") != "" }

// execInSandbox runs one Exec and returns its streams and exit code.
func execInSandbox(t *testing.T, admin, sandboxID string, argv ...string) (string, string, int) {
	t.Helper()
	e := createExec(t, admin, sandboxID, map[string]any{"argv": argv})
	stdout, stderr, _, result := followExec(t, admin, e.ID, 180*time.Second)
	if result.Cause != "exited" {
		t.Fatalf("%v: cause = %q, stderr %s", argv, result.Cause, stderr)
	}
	code := -1
	if result.ExitCode != nil {
		code = int(*result.ExitCode)
	}
	return string(stdout), string(stderr), code
}

// createGiteaRepo creates a private repository owned by owner through the
// Gitea admin API, with an initial commit so it can be cloned.
func createGiteaRepo(t *testing.T, owner, name string) {
	t.Helper()
	token := os.Getenv("ORRERY_E2E_GITEA_TOKEN")
	if token == "" {
		t.Fatal("ORRERY_E2E_GITEA_TOKEN is not set; run make e2e")
	}
	body, err := json.Marshal(map[string]any{
		"name": name, "private": true, "auto_init": true, "default_branch": "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost,
		giteaBase()+"/api/v1/admin/users/"+url.PathEscape(owner)+"/repos", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "token "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create Gitea repository: %v", err)
	}
	defer resp.Body.Close()
	page, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create Gitea repository: %d %s", resp.StatusCode, page)
	}
}

func giteaRepoFileExists(t *testing.T, owner, repo, path string) bool {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet,
		giteaBase()+"/api/v1/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(repo)+"/contents/"+url.PathEscape(path), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "token "+os.Getenv("ORRERY_E2E_GITEA_TOKEN"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("read Gitea repository: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode == http.StatusOK
}

// credentialPassword calls POST /git/credential and returns the password.
func credentialPassword(t *testing.T, sandboxToken string, body map[string]string) string {
	t.Helper()
	status, data := call(t, http.MethodPost, "/api/v1/git/credential", sandboxToken, body)
	if status != http.StatusOK {
		t.Fatalf("POST /git/credential %v: %d %s", body, status, data)
	}
	var cred struct {
		Password string `json:"password"`
	}
	mustJSON(t, data, &cred)
	return cred.Password
}

// TestGitLinkClonePush is the git-link-clone-push scenario (#80): a linked
// User clones and pushes a private repository with plain git, and neither the
// Workspace nor the container configuration retains the token.
func TestGitLinkClonePush(t *testing.T) {
	admin := adminToken(t)
	fixture := newGitFixture(t, admin, true)
	suffix := time.Now().UnixNano()
	repo := fmt.Sprintf("clone-push-%d", suffix)
	createGiteaRepo(t, gitGiteaUser(), repo)
	cloneURL := giteaBase() + "/" + gitGiteaUser() + "/" + repo + ".git"

	installGit(t, admin, fixture.sandboxID)
	clone := fmt.Sprintf(
		"export GIT_TERMINAL_PROMPT=0; git clone %s /workspace/repo && "+
			"cd /workspace/repo && echo e2e > pushed.txt && "+
			"git -c user.email=e2e@example.com -c user.name=e2e add pushed.txt && "+
			"git -c user.email=e2e@example.com -c user.name=e2e commit -m e2e >/dev/null && "+
			"git push", cloneURL)
	if out, errOut, code := execInSandbox(t, admin, fixture.sandboxID, "sh", "-c", clone); code != 0 {
		t.Fatalf("clone and push: exit %d\nstdout %s\nstderr %s", code, out, errOut)
	}
	if !giteaRepoFileExists(t, gitGiteaUser(), repo, "pushed.txt") {
		t.Fatal("the pushed file is not in the Gitea repository")
	}

	// The credential is issued per use and never persisted: compare what the
	// endpoint returns now against the Workspace and the container config.
	token := credentialPassword(t, fixture.sandboxToken, map[string]string{"provider": gitProviderID()})
	if token == "" {
		t.Fatal("the endpoint returned an empty token")
	}
	grep := fmt.Sprintf(`grep -rlF '%s' /workspace 2>/dev/null || true`, token)
	if out, _, _ := execInSandbox(t, admin, fixture.sandboxID, "sh", "-c", grep); out != "" {
		t.Fatalf("the Git token is written under /workspace:\n%s", out)
	}
	configs, _, _ := execInSandbox(t, admin, fixture.sandboxID, "sh", "-c",
		`git -C /workspace/repo config --list`)
	if strings.Contains(configs, token) {
		t.Fatalf("git config holds the Git token:\n%s", configs)
	}
	if out, err := inspectSandboxRuntime(fixture.sandboxID); err != nil {
		t.Fatalf("inspect the Sandbox container: %v\n%s", err, out)
	} else if strings.Contains(string(out), token) {
		t.Fatalf("the container record exposes the Git token:\n%s", out)
	}

	// Every issue is audited with the host, provider and Sandbox, never the
	// token (ADR-0013).
	status, data := call(t, http.MethodGet,
		"/api/v1/audit?action=git.token_issue&target="+fixture.sandboxID+"&limit=200", admin, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /audit: %d %s", status, data)
	}
	var audit auditList
	mustJSON(t, data, &audit)
	if len(audit.Items) == 0 {
		t.Fatal("no git.token_issue Audit Record")
	}
	hostSeen := false
	for _, item := range audit.Items {
		detail := string(item.Detail)
		if strings.Contains(detail, token) {
			t.Fatal("the Audit Record contains the token")
		}
		var fields struct {
			Host     string `json:"host"`
			Provider string `json:"provider"`
		}
		if json.Unmarshal(item.Detail, &fields) == nil &&
			fields.Provider == gitProviderID() && fields.Host != "" {
			hostSeen = true
		}
	}
	if !hostSeen {
		t.Fatalf("no Audit Record carries the repository host: %+v", audit.Items)
	}
}

// TestGitUnlinked is the git-unlinked scenario (#80): with no link the helper
// names the console's authorize URL on stderr and git fails.
func TestGitUnlinked(t *testing.T) {
	admin := adminToken(t)
	fixture := newGitFixture(t, admin, false)
	repo := fmt.Sprintf("unlinked-%d", time.Now().UnixNano())
	createGiteaRepo(t, gitGiteaUser(), repo)
	cloneURL := giteaBase() + "/" + gitGiteaUser() + "/" + repo + ".git"

	installGit(t, admin, fixture.sandboxID)
	stdout, stderr, code := execInSandbox(t, admin, fixture.sandboxID, "sh", "-c",
		fmt.Sprintf("GIT_TERMINAL_PROMPT=0 git clone %s /workspace/repo", cloneURL))
	if code == 0 {
		t.Fatal("cloning a private repository without a link succeeded")
	}
	want := strings.TrimSuffix(envURL("ORRERY_E2E_ACCESS_URL", apiURL()), "/") + "/git"
	if !strings.Contains(stdout+stderr, want) {
		t.Fatalf("clone output does not name %s:\nstdout %s\nstderr %s", want, stdout, stderr)
	}
}

// TestGitRefresh is the git-refresh scenario (#80): an expired link is
// refreshed once even when many requests race, because the row lock
// serializes them (ADR-0017). Gitea rotates refresh tokens, so a second
// concurrent refresh would be rejected upstream.
func TestGitRefresh(t *testing.T) {
	admin := adminToken(t)
	fixture := newGitFixture(t, admin, true)
	// Fetch once so the link holds a token with a refresh token.
	credentialPassword(t, fixture.sandboxToken, map[string]string{"provider": gitProviderID()})

	updated := psqlCount(t, fmt.Sprintf(
		"UPDATE git_links SET expires_at = now() - interval '1 hour' "+
			"WHERE user_id = '%s' AND provider_id = '%s' RETURNING user_id",
		fixture.userID, gitProviderID()))
	if updated == "" {
		t.Fatal("the Git link was not expired")
	}

	const racers = 8
	type result struct {
		status int
		data   []byte
		err    error
	}
	results := make([]result, racers)
	var wg sync.WaitGroup
	for n := range racers {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			status, data, err := postJSON(http.MethodPost, "/api/v1/git/credential",
				fixture.sandboxToken, map[string]string{"provider": gitProviderID()})
			results[n] = result{status: status, data: data, err: err}
		}(n)
	}
	wg.Wait()

	passwords := make([]string, racers)
	for n, res := range results {
		if res.err != nil {
			t.Fatalf("racer %d: %v", n, res.err)
		}
		if res.status != http.StatusOK {
			t.Fatalf("racer %d: %d %s", n, res.status, res.data)
		}
		var cred struct {
			Password string `json:"password"`
		}
		mustJSON(t, res.data, &cred)
		passwords[n] = cred.Password
	}
	for n := 1; n < racers; n++ {
		if passwords[n] != passwords[0] {
			t.Fatalf("racers received different tokens: %d refreshed more than once", n)
		}
	}
}

// TestGitRevokedUpstream is the git-revoked-upstream scenario (#80): after the
// owner revokes the provider grant, the endpoint returns the authorization URL
// instead of a dead token.
func TestGitRevokedUpstream(t *testing.T) {
	admin := adminToken(t)
	fixture := newGitFixture(t, admin, true)
	credentialPassword(t, fixture.sandboxToken, map[string]string{"provider": gitProviderID()})

	browser := newGiteaBrowser(t)
	browser.login(t, gitGiteaUser(), gitGiteaPassword())
	revokeGiteaGrant(t, browser)

	status, data := call(t, http.MethodPost, "/api/v1/git/credential", fixture.sandboxToken,
		map[string]string{"provider": gitProviderID()})
	if status != http.StatusNotFound {
		t.Fatalf("after upstream revocation: %d %s, want 404", status, data)
	}
	requireProblemCode(t, data, "not_found")
	var problem struct {
		AuthorizeURL string `json:"authorize_url"`
	}
	mustJSON(t, data, &problem)
	if problem.AuthorizeURL == "" {
		t.Fatalf("problem = %s, want an authorize_url", data)
	}
	if bytes.Contains(data, []byte(`"password"`)) {
		t.Fatalf("a rejected token was issued: %s", data)
	}
}

var (
	giteaRevokeForm = regexp.MustCompile(`data-url="(/user/settings/applications/oauth2/\d+/revoke/\d+)"`)
	giteaCSRFToken  = regexp.MustCompile(`csrfToken: '([^']+)'`)
)

// revokeGiteaGrant removes the user's authorization of the OAuth application
// through Gitea's own settings page, the only place it offers.
func revokeGiteaGrant(t *testing.T, browser *giteaBrowser) {
	t.Helper()
	resp, page := browser.get(t, browser.base+"/user/settings/applications")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Gitea applications page: %d %s", resp.StatusCode, page)
	}
	form := giteaRevokeForm.FindSubmatch(page)
	if form == nil {
		t.Fatalf("no OAuth2 grant to revoke on the page: %s", page)
	}
	csrf := giteaCSRFToken.FindSubmatch(page)
	if csrf == nil {
		t.Fatal("the Gitea settings page has no CSRF token")
	}
	req, err := http.NewRequest(http.MethodPost, browser.base+string(form[1]), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("x-csrf-token", string(csrf[1]))
	resp, page = browser.do(t, req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("revoke OAuth2 grant: %d %s", resp.StatusCode, page)
	}
}

// do performs one browser request and drains its body.
func (b *giteaBrowser) do(t *testing.T, req *http.Request) (*http.Response, []byte) {
	t.Helper()
	resp, err := b.client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp, data
}
