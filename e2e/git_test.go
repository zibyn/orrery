//go:build e2e

package e2e

import (
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

// TestGitLinkStateBinding is the git-link-state-binding scenario (#79): a Git
// authorization started by User A cannot be finished with User B's Session,
// and the failed callback stores no link. A then links and unlinks the same
// provider through the API and the external CLI.
func TestGitLinkStateBinding(t *testing.T) {
	providerID := envOr("STARS_E2E_GIT_PROVIDER", "gitea")
	giteaUser := envOr("STARS_E2E_GITEA_USER", "gitea-user")
	giteaPassword := envOr("STARS_E2E_GITEA_PASSWORD", "gitea-user-password-123")

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
		base: envOr("STARS_E2E_GITEA_URL", "http://127.0.0.1:13000"),
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
	resp, err := b.client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", rawURL, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp, data
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
