package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"golang.org/x/oauth2"

	"github.com/zibyn/stars-sandbox/internal/store"
)

const gitStateTTL = 10 * time.Minute

// gitStateAAD binds the encrypted state to this flow.
var gitStateAAD = []byte("stars-git-oauth-state")

// gitAuthState is the authorization-code flow's state, encrypted into the
// OAuth `state` parameter. UserID is what binds the callback to the User who
// started it, so another User opening the link gets 403 (api.md §3).
type gitAuthState struct {
	Verifier   string `json:"verifier"`
	ProviderID string `json:"provider_id"`
	UserID     string `json:"user_id"`
	ExpiresAt  int64  `json:"expires_at"`
}

type gitProviderDTO struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Host string `json:"host"`
}

type gitProviderList struct {
	Items      []gitProviderDTO `json:"items"`
	NextCursor *string          `json:"next_cursor"` // no pagination: always null
}

type gitLinkDTO struct {
	ProviderID string    `json:"provider_id"`
	LinkedAt   time.Time `json:"linked_at"`
}

type gitLinkList struct {
	Items      []gitLinkDTO `json:"items"`
	NextCursor *string      `json:"next_cursor"` // no pagination: always null
}

// GET /git-providers lists the deployment's providers without any credential
// material. An unset STARS_ACCESS_URL turns linking off, so there is nothing
// to offer (defaults.md).
func (s *Server) handleListGitProviders(w http.ResponseWriter, r *http.Request, _ *principal) {
	out := gitProviderList{Items: []gitProviderDTO{}}
	if s.cfg.AccessURL != "" {
		out.Items = make([]gitProviderDTO, 0, len(s.cfg.GitProviders))
		for _, p := range s.cfg.GitProviders {
			out.Items = append(out.Items, gitProviderDTO{ID: p.ID, Type: p.Type, Host: p.HostPattern.String()})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// GET /git-providers/{id}/authorize starts the OAuth authorization-code flow
// with PKCE. Only a browser Session can start it; without one the browser goes
// to the console login page and returns here afterwards (api.md §3).
func (s *Server) handleGitAuthorize(w http.ResponseWriter, r *http.Request, _ *principal) {
	provider, p, ok := s.gitBrowserEndpoint(w, r, true)
	if !ok {
		return
	}
	verifier := oauth2.GenerateVerifier()
	state, err := s.encodeGitState(gitAuthState{
		Verifier:   verifier,
		ProviderID: provider.ID,
		UserID:     p.user.ID.String(),
		ExpiresAt:  time.Now().Add(gitStateTTL).Unix(),
	})
	if err != nil {
		s.internal(w, "encode Git authorization state", err)
		return
	}
	http.Redirect(w, r, s.gitAuthCodeURL(provider, state, verifier), http.StatusFound)
}

// GET /external-auth/{id}/callback exchanges the code and stores the encrypted
// tokens. The state's User must be the Session's User (api.md §3).
func (s *Server) handleGitCallback(w http.ResponseWriter, r *http.Request, _ *principal) {
	provider, p, ok := s.gitBrowserEndpoint(w, r, false)
	if !ok {
		return
	}
	q := r.URL.Query()
	flow, ok := s.decodeGitState(q.Get("state"))
	if !ok {
		writeProblem(w, "invalid_request", "the Git authorization state is missing, malformed or expired")
		return
	}
	if flow.ProviderID != provider.ID {
		writeProblem(w, "invalid_request", "the Git authorization state does not match")
		return
	}
	// The state records who started the flow. Another User opening the
	// resulting link must never link their own account to it.
	if flow.UserID != p.user.ID.String() {
		writeProblem(w, "permission_denied", "this Git authorization belongs to another User")
		return
	}
	if q.Get("error") != "" {
		writeProblem(w, "permission_denied", "the Git provider rejected the authorization")
		return
	}
	code := q.Get("code")
	if code == "" {
		writeProblem(w, "invalid_request", "the Git callback has no code")
		return
	}

	conf := s.gitOAuthConfig(provider)
	opts := []oauth2.AuthCodeOption{}
	if provider.PKCE {
		opts = append(opts, oauth2.VerifierOption(flow.Verifier))
	}
	token, err := conf.Exchange(r.Context(), code, opts...)
	if err != nil {
		s.log.Warn("Git code exchange failed", "provider", provider.ID, "error", err)
		writeProblem(w, "invalid_request", "the Git code exchange failed")
		return
	}
	if token.AccessToken == "" {
		writeProblem(w, "invalid_request", "the Git provider returned no access token")
		return
	}
	if err := s.storeGitLink(r.Context(), s.q, p.user.ID, provider.ID, token); err != nil {
		s.internal(w, "store Git account link", err)
		return
	}
	http.Redirect(w, r, "/git", http.StatusFound)
}

// storeGitLink encrypts a link's tokens for its owner and writes the row. The
// query handle lets a refresh store inside its locking transaction.
func (s *Server) storeGitLink(ctx context.Context, q *store.Queries, owner uuid.UUID, providerID string, token *oauth2.Token) error {
	aad := []byte(owner.String())
	accessCt, keyID, err := s.cfg.EncryptionKeys.Encrypt([]byte(token.AccessToken), aad)
	if err != nil {
		return err
	}
	var refreshCt []byte
	if token.RefreshToken != "" {
		refreshCt, _, err = s.cfg.EncryptionKeys.Encrypt([]byte(token.RefreshToken), aad)
		if err != nil {
			return err
		}
	}
	_, err = q.UpsertGitLink(ctx, store.UpsertGitLinkParams{
		UserID:         owner,
		ProviderID:     providerID,
		AccessTokenCt:  accessCt,
		RefreshTokenCt: refreshCt,
		KeyID:          keyID,
		ExpiresAt:      timestamptzPtr(expiryPtr(token)),
	})
	return err
}

// GET /users/me/git-links lists the caller's linked providers. Tokens never
// leave the database.
func (s *Server) handleListGitLinks(w http.ResponseWriter, r *http.Request, p *principal) {
	rows, err := s.q.ListGitLinks(r.Context(), p.user.ID)
	if err != nil {
		s.internal(w, "list Git links", err)
		return
	}
	out := gitLinkList{Items: make([]gitLinkDTO, 0, len(rows))}
	for _, row := range rows {
		out.Items = append(out.Items, gitLinkToDTO(row))
	}
	writeJSON(w, http.StatusOK, out)
}

// DELETE /users/me/git-links/{provider_id} removes the link and, where the
// provider has a revoke endpoint, asks it to invalidate the token. Revocation
// is best effort and never blocks the unlink (ADR-0017).
func (s *Server) handleDeleteGitLink(w http.ResponseWriter, r *http.Request, p *principal) {
	row, err := s.q.DeleteGitLink(r.Context(), store.DeleteGitLinkParams{
		UserID: p.user.ID, ProviderID: r.PathValue("provider_id"),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeProblem(w, "not_found", "")
		return
	}
	if err != nil {
		s.internal(w, "delete Git link", err)
		return
	}
	if provider, ok := s.gitProvider(row.ProviderID); ok && provider.RevokeURL != "" && s.cfg.EncryptionKeys != nil {
		token, err := s.cfg.EncryptionKeys.Decrypt(row.AccessTokenCt, row.KeyID, []byte(p.user.ID.String()))
		if err != nil {
			s.log.Warn("open Git access token for revoke", "provider", provider.ID, "error", err)
		} else if err := revokeGitToken(r.Context(), provider, string(token)); err != nil {
			s.log.Warn("Git token revocation failed", "provider", provider.ID, "error", err)
		}
	}
	writeJSON(w, http.StatusAccepted, gitLinkToDTO(row))
}

func gitLinkToDTO(row store.GitLink) gitLinkDTO {
	return gitLinkDTO{ProviderID: row.ProviderID, LinkedAt: row.CreatedAt.Time.UTC()}
}

// gitOAuthConfig is the provider's OAuth2 client. The redirect target is the
// deployment's access URL, not the request's Host (defaults.md).
func (s *Server) gitOAuthConfig(p GitProvider) oauth2.Config {
	return oauth2.Config{
		ClientID:     p.ClientID,
		ClientSecret: p.ClientSecret,
		Endpoint:     oauth2.Endpoint{AuthURL: p.AuthURL, TokenURL: p.TokenURL},
		RedirectURL:  s.gitRedirectURL(p.ID),
		Scopes:       p.Scopes,
	}
}

func (s *Server) gitRedirectURL(id string) string {
	return strings.TrimSuffix(s.cfg.AccessURL, "/") + "/api/v1/external-auth/" + url.PathEscape(id) + "/callback"
}

func (s *Server) gitAuthCodeURL(provider GitProvider, state, verifier string) string {
	opts := []oauth2.AuthCodeOption{}
	if provider.PKCE {
		opts = append(opts, oauth2.S256ChallengeOption(verifier))
	}
	conf := s.gitOAuthConfig(provider)
	return conf.AuthCodeURL(state, opts...)
}

// gitBrowserEndpoint resolves the provider and the browser Session shared by
// the authorize and callback endpoints. Only a Session cookie is accepted.
// With redirectToLogin, any authentication failure (a missing, expired or
// revoked Session) goes to the login page and returns to the same URL
// afterwards (api.md §3).
func (s *Server) gitBrowserEndpoint(w http.ResponseWriter, r *http.Request, redirectToLogin bool) (GitProvider, *principal, bool) {
	if s.cfg.AccessURL == "" {
		writeProblem(w, "not_found", "Git account linking is not configured")
		return GitProvider{}, nil, false
	}
	provider, ok := s.gitProvider(r.PathValue("id"))
	if !ok {
		writeProblem(w, "not_found", "")
		return GitProvider{}, nil, false
	}
	var (
		p        *principal
		authOkay bool
	)
	if redirectToLogin {
		// Buffer authenticate's failure so a dead Session becomes a redirect
		// instead of problem JSON, while a real server error is passed through.
		buffered := newBufferedResponse()
		p, authOkay = s.authenticate(buffered, r)
		if !authOkay {
			if buffered.status == http.StatusUnauthorized {
				http.Redirect(w, r, "/login?return="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
			} else {
				buffered.copyTo(w)
			}
			return GitProvider{}, nil, false
		}
	} else {
		p, authOkay = s.authenticate(w, r)
		if !authOkay {
			return GitProvider{}, nil, false
		}
	}
	if !p.viaCookie || p.token.Kind != "session" {
		writeProblem(w, "permission_denied", "this endpoint requires a Session cookie")
		return GitProvider{}, nil, false
	}
	if s.cfg.EncryptionKeys == nil {
		writeProblem(w, "unavailable", "Git account linking is not configured: set STARS_ENCRYPTION_KEYS_FILE")
		return GitProvider{}, nil, false
	}
	return provider, p, true
}

// bufferedResponse captures what authenticate would send, so authorize can
// replace a 401 with the console's login redirect.
type bufferedResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func newBufferedResponse() *bufferedResponse {
	return &bufferedResponse{header: http.Header{}}
}

func (b *bufferedResponse) Header() http.Header { return b.header }

func (b *bufferedResponse) WriteHeader(status int) {
	if b.status == 0 {
		b.status = status
	}
}

func (b *bufferedResponse) Write(p []byte) (int, error) {
	if b.status == 0 {
		b.status = http.StatusOK
	}
	return b.body.Write(p)
}

func (b *bufferedResponse) copyTo(w http.ResponseWriter) {
	for name, values := range b.header {
		for _, value := range values {
			w.Header().Add(name, value)
		}
	}
	status := b.status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = w.Write(b.body.Bytes())
}

// encodeGitState encrypts the flow into the OAuth `state` parameter. It is
// stateless, so a restarted or another Control Plane can still finish the
// flow, and authenticated, so nobody can mint a state for another User.
func (s *Server) encodeGitState(flow gitAuthState) (string, error) {
	raw, err := json.Marshal(flow)
	if err != nil {
		return "", err
	}
	ciphertext, keyID, err := s.cfg.EncryptionKeys.Encrypt(raw, gitStateAAD)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString([]byte(keyID)) + "." +
		base64.RawURLEncoding.EncodeToString(ciphertext), nil
}

func (s *Server) decodeGitState(raw string) (gitAuthState, bool) {
	keyPart, cipherPart, ok := strings.Cut(raw, ".")
	if !ok {
		return gitAuthState{}, false
	}
	keyID, err := base64.RawURLEncoding.DecodeString(keyPart)
	if err != nil {
		return gitAuthState{}, false
	}
	ciphertext, err := base64.RawURLEncoding.DecodeString(cipherPart)
	if err != nil {
		return gitAuthState{}, false
	}
	plaintext, err := s.cfg.EncryptionKeys.Decrypt(ciphertext, string(keyID), gitStateAAD)
	if err != nil {
		return gitAuthState{}, false
	}
	var flow gitAuthState
	if err := json.Unmarshal(plaintext, &flow); err != nil {
		return gitAuthState{}, false
	}
	if flow.Verifier == "" || flow.ProviderID == "" || flow.UserID == "" ||
		flow.ExpiresAt <= time.Now().Unix() {
		return gitAuthState{}, false
	}
	return flow, true
}

func expiryPtr(token *oauth2.Token) *time.Time {
	if token.Expiry.IsZero() {
		return nil
	}
	return &token.Expiry
}

// revokeGitToken invalidates the token upstream where the provider has a
// revoke endpoint. Gitea has none, and generic providers are not configured
// with one.
func revokeGitToken(ctx context.Context, p GitProvider, token string) error {
	switch p.Type {
	case "github":
		body, err := json.Marshal(map[string]string{"access_token": token})
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodDelete, p.RevokeURL, strings.NewReader(string(body)))
		if err != nil {
			return err
		}
		req.SetBasicAuth(p.ClientID, p.ClientSecret)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		return doRevoke(req)
	case "gitlab":
		form := url.Values{"client_id": {p.ClientID}, "client_secret": {p.ClientSecret}, "token": {token}}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.RevokeURL, strings.NewReader(form.Encode()))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		return doRevoke(req)
	default:
		return nil
	}
}

func doRevoke(req *http.Request) error {
	resp, err := gitUpstreamClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("revoke answered %s", resp.Status)
	}
	return nil
}
