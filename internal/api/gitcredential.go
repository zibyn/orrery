package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"golang.org/x/oauth2"

	"github.com/zibyn/orrery/internal/keyring"
	"github.com/zibyn/orrery/internal/store"
)

// gitCredentialRequest is api.md §3: exactly one of host or provider.
type gitCredentialRequest struct {
	Host     string `json:"host"`
	Provider string `json:"provider"`
}

type gitCredentialResponse struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// errGitTokenRejected means the provider's own validation endpoint answered
// that the token is not usable, so the owner must authorize again.
var errGitTokenRejected = errors.New("Git token rejected")

// errGitUpstreamTransient means refresh failed without proving the link dead;
// the caller may retry (ADR-0017).
var errGitUpstreamTransient = errors.New("Git provider unreachable")

// gitUpstreamClient bounds token validation, refresh and revocation calls.
var gitUpstreamClient = &http.Client{Timeout: 15 * time.Second}

// POST /git/credential returns the Git credential a Sandbox Token may use for
// a host or provider (ADR-0013, ADR-0017). A host matching no provider is a
// silent pass-through for git's helper chain; a missing or rejected link is a
// 404 carrying the console's authorize_url.
func (s *Server) handleGitCredential(w http.ResponseWriter, r *http.Request, p *principal) {
	var req gitCredentialRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if (req.Host == "") == (req.Provider == "") {
		writeProblem(w, "invalid_request", "exactly one of host or provider is required")
		return
	}
	provider, matches := s.gitCredentialProvider(req.Host, req.Provider)
	switch {
	case matches == 0:
		detail := ""
		if req.Provider != "" {
			detail = fmt.Sprintf("no Git Provider named %q", req.Provider)
		}
		writeProblem(w, "not_found", detail)
		return
	case matches > 1:
		writeProblem(w, "conflict", fmt.Sprintf("%d Git Providers match host %q", matches, req.Host))
		return
	}
	if s.cfg.EncryptionKeys == nil {
		writeProblem(w, "unavailable", "Git account linking is not configured: set encryption_keys in the deployment configuration")
		return
	}

	owner := p.sandbox.OwnerID
	token, linked, err := s.gitLinkToken(r.Context(), owner, provider)
	if errors.Is(err, errGitUpstreamTransient) {
		s.log.Warn("refresh Git token", "provider", provider.ID, "error", err)
		writeProblem(w, "unavailable", "the Git provider could not be reached")
		return
	}
	if err != nil {
		s.internal(w, "issue Git credential", err)
		return
	}
	if !linked {
		s.writeGitAuthorizationRequired(w, "no Git account is linked for "+provider.ID)
		return
	}
	if err := validateGitToken(r.Context(), provider, token); err != nil {
		if errors.Is(err, errGitTokenRejected) {
			s.writeGitAuthorizationRequired(w, "the Git provider rejected the token")
			return
		}
		// A network or upstream failure proves nothing about the link.
		s.log.Warn("validate Git token", "provider", provider.ID, "error", err)
		writeProblem(w, "unavailable", "the Git provider could not be reached")
		return
	}

	// Every issue is audited with its host, provider and Sandbox, never the
	// token (ADR-0013). A provider-addressed request names no host, so record
	// the configured pattern that stands for it.
	host := req.Host
	if host == "" && provider.HostPattern != nil {
		host = provider.HostPattern.String()
	}
	s.auditAs(r.Context(), &owner, nil, "git.token_issue", "sandbox", p.sandbox.ID.String(),
		map[string]any{"host": host, "provider": provider.ID})
	writeJSON(w, http.StatusOK, gitCredentialResponse{
		Username: gitUsername(provider.Type),
		Password: token,
	})
}

// gitCredentialProvider resolves a host or provider id to exactly one
// configured provider. The match count lets the caller tell "no provider"
// (pass through) from "ambiguous" (fail rather than guess).
func (s *Server) gitCredentialProvider(host, id string) (GitProvider, int) {
	// An unset ORRERY_ACCESS_URL turns Git account linking off, so there is no
	// provider to match either way (defaults.md).
	if s.cfg.AccessURL == "" {
		return GitProvider{}, 0
	}
	if id != "" {
		provider, ok := s.gitProvider(id)
		if !ok {
			return GitProvider{}, 0
		}
		return provider, 1
	}
	matches := make([]GitProvider, 0, 1)
	for _, provider := range s.cfg.GitProviders {
		if provider.HostPattern != nil && provider.HostPattern.MatchString(host) {
			matches = append(matches, provider)
		}
	}
	if len(matches) == 1 {
		return matches[0], 1
	}
	return GitProvider{}, len(matches)
}

// gitUsername is fixed per provider type: GitLab and Gitea accept "oauth2",
// GitHub's app tokens conventionally use "x-access-token" (ADR-0013).
func gitUsername(providerType string) string {
	if providerType == "github" {
		return "x-access-token"
	}
	return "oauth2"
}

// writeGitAuthorizationRequired answers 404 with the console's Git page, which
// the helper prints to stderr before git tries its next helper (api.md §3).
func (s *Server) writeGitAuthorizationRequired(w http.ResponseWriter, detail string) {
	writeProblemWithExtras(w, "not_found", detail, map[string]any{
		"authorize_url": s.gitAuthorizeURL(),
	})
}

// gitLinkToken returns a usable access token for the owner's link. linked is
// false when there is no link or it can never work again; err is reserved for
// transient failures, which must not invalidate the link (ADR-0017).
func (s *Server) gitLinkToken(ctx context.Context, owner uuid.UUID, provider GitProvider) (token string, linked bool, err error) {
	row, err := s.q.GetGitLink(ctx, store.GetGitLinkParams{UserID: owner, ProviderID: provider.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	tok, err := openGitLinkToken(s.cfg.EncryptionKeys, owner, row)
	if err != nil {
		return "", false, err
	}
	if tok.Valid() {
		return tok.AccessToken, true, nil
	}
	return s.refreshGitLink(ctx, owner, provider)
}

// refreshGitLink serializes refreshing one link: the row lock means concurrent
// callers refresh once and the rest reuse the stored token (ADR-0017).
func (s *Server) refreshGitLink(ctx context.Context, owner uuid.UUID, provider GitProvider) (string, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	q := s.q.WithTx(tx)
	row, err := q.GetGitLinkForUpdate(ctx, store.GetGitLinkForUpdateParams{UserID: owner, ProviderID: provider.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	tok, err := openGitLinkToken(s.cfg.EncryptionKeys, owner, row)
	if err != nil {
		return "", false, err
	}
	// Another request may have refreshed while this one waited for the lock.
	if tok.Valid() {
		if err := tx.Commit(ctx); err != nil {
			return "", false, err
		}
		return tok.AccessToken, true, nil
	}
	if tok.RefreshToken == "" {
		if err := s.dropGitLink(ctx, tx, q, owner, provider); err != nil {
			return "", false, err
		}
		return "", false, nil
	}

	conf := s.gitOAuthConfig(provider)
	fresh, err := conf.
		TokenSource(context.WithValue(ctx, oauth2.HTTPClient, gitUpstreamClient), tok).Token()
	if err != nil {
		if !isPermanentGitRefreshError(err) {
			// Everything but an explicit permanent error is transient; the
			// link stays so a later call can retry (ADR-0017).
			return "", true, fmt.Errorf("%w: %v", errGitUpstreamTransient, err)
		}
		if err := s.dropGitLink(ctx, tx, q, owner, provider); err != nil {
			return "", false, err
		}
		return "", false, nil
	}

	// Store the new token before anything else uses it (ADR-0017).
	if err := s.storeGitLink(ctx, q, owner, provider.ID, fresh); err != nil {
		return "", false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", false, err
	}
	return fresh.AccessToken, true, nil
}

// dropGitLink removes a link that can never be refreshed again. It is the only
// path that invalidates a link, and only for an explicit permanent error.
func (s *Server) dropGitLink(ctx context.Context, tx pgx.Tx, q *store.Queries, owner uuid.UUID, provider GitProvider) error {
	if _, err := q.DeleteGitLink(ctx, store.DeleteGitLinkParams{UserID: owner, ProviderID: provider.ID}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// openGitLinkToken decrypts a link's tokens into the shape oauth2.Token.Valid
// expects: an expiry within ten seconds of now already counts as expired
// (defaults.md).
func openGitLinkToken(keys *keyring.Keyring, owner uuid.UUID, row store.GitLink) (*oauth2.Token, error) {
	aad := []byte(owner.String())
	access, err := keys.Decrypt(row.AccessTokenCt, row.KeyID, aad)
	if err != nil {
		return nil, fmt.Errorf("decrypt Git access token: %w", err)
	}
	tok := &oauth2.Token{AccessToken: string(access)}
	if row.ExpiresAt.Valid {
		tok.Expiry = row.ExpiresAt.Time
	}
	if len(row.RefreshTokenCt) > 0 {
		refresh, err := keys.Decrypt(row.RefreshTokenCt, row.KeyID, aad)
		if err != nil {
			return nil, fmt.Errorf("decrypt Git refresh token: %w", err)
		}
		tok.RefreshToken = string(refresh)
	}
	return tok, nil
}

// isPermanentGitRefreshError reports whether a refresh failure proves the link
// is dead. Only invalid_grant is that: network, 5xx and server errors are
// transient (ADR-0017).
func isPermanentGitRefreshError(err error) bool {
	var retrieve *oauth2.RetrieveError
	return errors.As(err, &retrieve) && retrieve.ErrorCode == "invalid_grant"
}

// validateGitToken asks the provider's own user endpoint whether the token is
// usable. A rejected token is a linking problem; anything else is transient.
func validateGitToken(ctx context.Context, provider GitProvider, token string) error {
	if provider.ValidateURL == "" {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, provider.ValidateURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if provider.Type == "github" {
		req.Header.Set("Accept", "application/vnd.github+json")
	}
	resp, err := gitUpstreamClient.Do(req)
	if err != nil {
		return fmt.Errorf("validate Git token: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return errGitTokenRejected
	}
	return fmt.Errorf("validate Git token: %s", resp.Status)
}

// gitAuthorizeURL points at the console's Git linking page (api.md §8).
func (s *Server) gitAuthorizeURL() string {
	return strings.TrimSuffix(s.cfg.AccessURL, "/") + "/git"
}
