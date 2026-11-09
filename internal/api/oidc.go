package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	polaris "github.com/zibyn/polaris/sdk/go"
	"golang.org/x/oauth2"

	"github.com/zibyn/orrery/internal/store"
)

const (
	oidcStateCookieName = "orrery_oidc_state"
	oidcStateTTL        = 10 * time.Minute
)

// oidcState is the flow's server-side half, kept in an HttpOnly cookie so the
// IdP's redirect carries it back (api.md §3).
type oidcState struct {
	State    string `json:"state"`
	Nonce    string `json:"nonce"`
	Verifier string `json:"verifier"`
	Return   string `json:"return"`
}

type oidcClaims struct {
	Email             string `json:"email"`
	EmailVerified     bool   `json:"email_verified"`
	Name              string `json:"name"`
	PreferredUsername string `json:"preferred_username"`
	Sid               string `json:"sid"`
}

// adminEntitlement is the Polaris Permission that makes a User an Orrery admin.
const adminEntitlement = "orrery:admin"

// handleOIDCLogin sends the browser to the IdP with an authorization code, PKCE
// and a nonce. The state cookie binds the callback to this request.
func (s *Server) handleOIDCLogin(w http.ResponseWriter, r *http.Request, _ *principal) {
	provider, _, err := s.oidcProviderFor(r.Context())
	if err != nil {
		s.log.Error("OIDC discovery failed", "error", err)
		writeProblem(w, "unavailable", "the OIDC provider is not reachable")
		return
	}
	state := oauth2.GenerateVerifier()
	nonce := oauth2.GenerateVerifier()
	verifier := oauth2.GenerateVerifier()
	payload, err := json.Marshal(oidcState{
		State:    state,
		Nonce:    nonce,
		Verifier: verifier,
		Return:   r.URL.Query().Get("return"),
	})
	if err != nil {
		s.internal(w, "encode OIDC state", err)
		return
	}
	http.SetCookie(w, oidcStateCookie(base64.RawURLEncoding.EncodeToString(payload), int(oidcStateTTL.Seconds())))
	conf := s.oauth2Config(provider, r)
	http.Redirect(w, r, conf.AuthCodeURL(state,
		oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier)), http.StatusFound)
}

// handleOIDCAccount sends the browser to the Polaris account center, where
// profile edits live (ADR-0029); client_id brands it for Orrery.
func (s *Server) handleOIDCAccount(w http.ResponseWriter, r *http.Request, _ *principal) {
	target := strings.TrimSuffix(s.cfg.OIDC.IssuerURL, "/") + "/account?" +
		url.Values{"client_id": {s.cfg.OIDC.ClientID}}.Encode()
	http.Redirect(w, r, target, http.StatusFound)
}

// handleOIDCCallback verifies Polaris's response, matches or creates the
// User, and issues a Session (ADR-0029).
func (s *Server) handleOIDCCallback(w http.ResponseWriter, r *http.Request, _ *principal) {
	flow, ok := takeOIDCState(w, r)
	if !ok {
		writeProblem(w, "invalid_request", "the OIDC state cookie is missing or malformed")
		return
	}
	q := r.URL.Query()
	if q.Get("error") != "" {
		writeProblem(w, "permission_denied", "the OIDC provider rejected the login")
		return
	}
	if q.Get("state") != flow.State {
		writeProblem(w, "invalid_request", "the OIDC state does not match")
		return
	}
	code := q.Get("code")
	if code == "" {
		writeProblem(w, "invalid_request", "the OIDC callback has no code")
		return
	}

	provider, accessVerifier, err := s.oidcProviderFor(r.Context())
	if err != nil {
		s.log.Error("OIDC discovery failed", "error", err)
		writeProblem(w, "unavailable", "the OIDC provider is not reachable")
		return
	}
	conf := s.oauth2Config(provider, r)
	token, err := conf.Exchange(r.Context(), code, oauth2.VerifierOption(flow.Verifier))
	if err != nil {
		s.log.Warn("OIDC code exchange failed", "error", err)
		writeProblem(w, "invalid_request", "the OIDC code exchange failed")
		return
	}
	rawIDToken, _ := token.Extra("id_token").(string)
	if rawIDToken == "" {
		writeProblem(w, "invalid_request", "the OIDC response has no ID token")
		return
	}
	verifier := provider.Verifier(&oidc.Config{ClientID: s.cfg.OIDC.ClientID})
	idToken, err := verifier.Verify(r.Context(), rawIDToken)
	if err != nil {
		s.log.Warn("OIDC ID token rejected", "error", err)
		writeProblem(w, "invalid_request", "the ID token is invalid")
		return
	}
	if idToken.Nonce != flow.Nonce {
		writeProblem(w, "invalid_request", "the ID token nonce does not match")
		return
	}
	var claims oidcClaims
	if err := idToken.Claims(&claims); err != nil {
		writeProblem(w, "permission_denied", "the ID token has no usable claims")
		return
	}
	// Admin authority comes only from the access token for the Orrery API
	// (ADR-0029); one for any other audience fails the login.
	access, err := accessVerifier.VerifyAccessToken(r.Context(), token.AccessToken)
	if err != nil || access.Sub != idToken.Subject {
		s.log.Warn("Polaris access token rejected", "error", err)
		writeProblem(w, "invalid_request", "the access token is invalid")
		return
	}

	user, err := s.oidcUser(r.Context(), idToken.Issuer, idToken.Subject, claims,
		slices.Contains(access.Entitlements, adminEntitlement))
	if err != nil {
		s.internal(w, "resolve OIDC User", err)
		return
	}
	var sid *string
	if claims.Sid != "" {
		sid = &claims.Sid
	}
	if _, err := s.startBrowserSession(w, r.Context(), user, nil, sid, map[string]any{"via": "oidc"}); err != nil {
		s.internal(w, "create OIDC Session", err)
		return
	}
	http.Redirect(w, r, safeReturnPath(flow.Return), http.StatusFound)
}

// oidcUser matches by (issuer, subject) only, creating the User on a first
// login, and copies the claims and admin status onto it (ADR-0029).
func (s *Server) oidcUser(ctx context.Context, issuer, subject string, claims oidcClaims, isAdmin bool) (store.User, error) {
	var email *string
	if claims.Email != "" && claims.EmailVerified {
		email = &claims.Email
	}
	name := oidcUserName(subject, claims)
	identity := store.GetUserByOIDCIdentityParams{OidcIssuer: &issuer, OidcSubject: &subject}
	user, err := s.q.GetUserByOIDCIdentity(ctx, identity)
	if errors.Is(err, pgx.ErrNoRows) {
		user, err = s.createOIDCUser(ctx, issuer, subject, name, email, isAdmin)
		if uniqueViolation(err) != "users_oidc_unique" {
			return user, err
		}
		// A concurrent first login won the insert; update that User instead.
		user, err = s.q.GetUserByOIDCIdentity(ctx, identity)
	}
	if err != nil {
		return store.User{}, err
	}
	return s.syncOIDCUser(ctx, user, subject, name, email, isAdmin)
}

// createOIDCUser inserts a User with the default quota. A taken name is
// disambiguated with the subject and a taken email is left unset, so a
// collision never fails a login.
func (s *Server) createOIDCUser(ctx context.Context, issuer, subject, name string, email *string, isAdmin bool) (store.User, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return store.User{}, err
	}
	params := store.CreateOIDCUserParams{
		ID:           id,
		Name:         name,
		Email:        email,
		IsAdmin:      isAdmin,
		OidcIssuer:   &issuer,
		OidcSubject:  &subject,
		GpuQuota:     s.cfg.DefaultGPUQuota,
		SandboxQuota: s.cfg.DefaultSandboxQuota,
	}
	for {
		user, err := s.q.CreateOIDCUser(ctx, params)
		switch violated := uniqueViolation(err); {
		case violated == "users_name_key" && params.Name == name:
			params.Name = uniqueOIDCName(name, subject)
		case violated == "users_email_unique" && params.Email != nil:
			params.Email = nil
		default:
			if err == nil {
				s.auditAs(ctx, &user.ID, nil, "user.create", "user", user.ID.String(), map[string]any{
					"name": user.Name, "is_admin": user.IsAdmin, "oidc": true,
				})
			}
			return user, err
		}
	}
}

// syncOIDCUser copies a returning login's name, email and admin status. The
// same collision rules as createOIDCUser apply; an absent email claim keeps
// the stored one.
func (s *Server) syncOIDCUser(ctx context.Context, user store.User, subject, name string, email *string, isAdmin bool) (store.User, error) {
	want := store.SyncOIDCUserParams{ID: user.ID, Name: name, Email: email, IsAdmin: isAdmin}
	if email == nil {
		want.Email = user.Email
	}
	for {
		if want.Name == user.Name && sameString(want.Email, user.Email) && want.IsAdmin == user.IsAdmin {
			return user, nil
		}
		updated, err := s.q.SyncOIDCUser(ctx, want)
		switch violated := uniqueViolation(err); {
		case violated == "users_name_key" && want.Name == name:
			want.Name = uniqueOIDCName(name, subject)
		case violated == "users_email_unique" && !sameString(want.Email, user.Email):
			want.Email = user.Email
		default:
			if err == nil {
				s.auditAs(ctx, &user.ID, nil, "user.update", "user", user.ID.String(), map[string]any{
					"name": updated.Name, "is_admin": updated.IsAdmin, "via": "oidc",
				})
			}
			return updated, err
		}
	}
}

func sameString(a, b *string) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

// oidcProviderFor discovers Polaris once and remembers it, with the access
// token verifier for the Orrery API. Discovery runs on the first login rather
// than at startup, so a slow IdP cannot delay serving.
func (s *Server) oidcProviderFor(ctx context.Context) (*oidc.Provider, *polaris.Verifier, error) {
	s.oidcMu.Lock()
	defer s.oidcMu.Unlock()
	if s.oidcProvider != nil {
		return s.oidcProvider, s.polaris, nil
	}
	provider, err := oidc.NewProvider(ctx, s.cfg.OIDC.IssuerURL)
	if err != nil {
		return nil, nil, err
	}
	verifier, err := polaris.NewVerifier(ctx, s.cfg.OIDC.IssuerURL, s.cfg.OIDC.Audience)
	if err != nil {
		return nil, nil, err
	}
	s.oidcProvider, s.polaris = provider, verifier
	return provider, verifier, nil
}

func (s *Server) oauth2Config(provider *oidc.Provider, r *http.Request) oauth2.Config {
	return oauth2.Config{
		ClientID:     s.cfg.OIDC.ClientID,
		ClientSecret: s.cfg.OIDC.ClientSecret,
		Endpoint:     provider.Endpoint(),
		RedirectURL:  s.oidcRedirectURL(r),
		Scopes:       []string{oidc.ScopeOpenID, "profile", "email"},
	}
}

// oidcRedirectURL is the deployment's access URL when configured, as with
// every other externally visible URL (defaults.md); otherwise the request's
// own origin.
func (s *Server) oidcRedirectURL(r *http.Request) string {
	base := s.cfg.AccessURL
	if base == "" {
		scheme := "http"
		if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
			scheme = "https"
		}
		base = scheme + "://" + r.Host
	}
	return strings.TrimSuffix(base, "/") + "/api/v1/oidc/callback"
}

// takeOIDCState reads and clears the flow cookie. A missing cookie is not an
// error worth logging: it is a stale or forged callback.
func takeOIDCState(w http.ResponseWriter, r *http.Request) (oidcState, bool) {
	c, err := r.Cookie(oidcStateCookieName)
	http.SetCookie(w, oidcStateCookie("", -1))
	if err != nil || c.Value == "" {
		return oidcState{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		return oidcState{}, false
	}
	var flow oidcState
	if err := json.Unmarshal(raw, &flow); err != nil || flow.State == "" || flow.Nonce == "" || flow.Verifier == "" {
		return oidcState{}, false
	}
	return flow, true
}

func oidcStateCookie(value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     oidcStateCookieName,
		Value:    value,
		Path:     "/api/v1/oidc",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	}
}

// safeReturnPath mirrors the console's rule: only a same-origin absolute path,
// so an IdP callback cannot be turned into an open redirect. Control
// characters are rejected because browsers strip tabs and newlines from URLs,
// turning "/\t/evil" into the protocol-relative "//evil".
func safeReturnPath(value string) string {
	if value == "" || value[0] != '/' || strings.HasPrefix(value, "//") || strings.HasPrefix(value, `/\`) {
		return "/"
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return "/"
		}
	}
	return value
}

// oidcUserName picks a stable display name from the claims, falling back to
// the subject.
func oidcUserName(subject string, claims oidcClaims) string {
	candidates := []string{claims.PreferredUsername, claims.Name}
	if at := strings.LastIndexByte(claims.Email, '@'); at > 0 {
		candidates = append(candidates, claims.Email[:at])
	}
	candidates = append(candidates, subject)
	for _, name := range candidates {
		name = strings.TrimSpace(name)
		if name != "" && utf8.RuneCountInString(name) <= maxNameLen {
			return name
		}
	}
	return "oidc-user"
}

func uniqueOIDCName(name, subject string) string {
	suffix := "-" + strings.ReplaceAll(subject, " ", "")
	if r := []rune(suffix); len(r) > 9 {
		suffix = string(r[:9])
	}
	runes := []rune(name)
	if len(runes) > maxNameLen-len([]rune(suffix)) {
		runes = runes[:maxNameLen-len([]rune(suffix))]
	}
	return string(runes) + suffix
}
