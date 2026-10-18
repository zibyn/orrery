package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"golang.org/x/oauth2"

	"github.com/zibyn/stars-sandbox/internal/store"
)

const (
	oidcStateCookieName = "stars_oidc_state"
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
}

// handleOIDCLogin sends the browser to the IdP with an authorization code, PKCE
// and a nonce. The state cookie binds the callback to this request.
func (s *Server) handleOIDCLogin(w http.ResponseWriter, r *http.Request, _ *principal) {
	if !s.cfg.OIDC.Enabled() {
		writeProblem(w, "not_found", "OIDC is not configured")
		return
	}
	provider, err := s.oidcProviderFor(r.Context())
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

// handleOIDCCallback verifies the IdP's response, matches or creates the User,
// and issues a Session (ADR-0020).
func (s *Server) handleOIDCCallback(w http.ResponseWriter, r *http.Request, _ *principal) {
	if !s.cfg.OIDC.Enabled() {
		writeProblem(w, "not_found", "OIDC is not configured")
		return
	}
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

	provider, err := s.oidcProviderFor(r.Context())
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
	// An unverified email never logs in, and email_domains restricts every
	// login, not just signups (defaults.md).
	if !claims.EmailVerified {
		writeProblem(w, "permission_denied", "the email address is not verified")
		return
	}
	if !emailDomainAllowed(claims.Email, s.cfg.OIDC.EmailDomains) {
		writeProblem(w, "permission_denied", "the email address is outside the allowed domains")
		return
	}

	user, err := s.oidcUser(r.Context(), idToken.Issuer, idToken.Subject, claims)
	var denial oidcDenial
	if errors.As(err, &denial) {
		writeProblem(w, "permission_denied", string(denial))
		return
	}
	if err != nil {
		s.internal(w, "resolve OIDC User", err)
		return
	}
	if _, err := s.startBrowserSession(w, r.Context(), user, nil, map[string]any{"via": "oidc"}); err != nil {
		s.internal(w, "create OIDC Session", err)
		return
	}
	http.Redirect(w, r, safeReturnPath(flow.Return), http.StatusFound)
}

// oidcDenial is a policy decision answered with 403, as opposed to a failure.
type oidcDenial string

func (d oidcDenial) Error() string { return string(d) }

// oidcUser matches by (issuer, subject), then by verified email, and otherwise
// signs up when allowed.
func (s *Server) oidcUser(ctx context.Context, issuer, subject string, claims oidcClaims) (store.User, error) {
	user, err := s.q.GetUserByOIDCIdentity(ctx, store.GetUserByOIDCIdentityParams{
		OidcIssuer: &issuer, OidcSubject: &subject,
	})
	switch {
	case err == nil:
		if user.LoginType != "oidc" {
			return store.User{}, oidcDenial("this User does not log in through OIDC")
		}
		return user, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return store.User{}, err
	}

	if claims.Email != "" {
		user, err = s.q.GetUserByEmail(ctx, &claims.Email)
		switch {
		case err == nil:
			// Matching by email links an OIDC User an admin already created;
			// it never converts another login type (ADR-0020).
			if user.LoginType != "oidc" {
				return store.User{}, oidcDenial("this email belongs to a User that does not log in through OIDC")
			}
			if user.OidcIssuer != nil || user.OidcSubject != nil {
				return store.User{}, oidcDenial("this User is already linked to another OIDC identity")
			}
			bound, err := s.q.BindUserOIDCIdentity(ctx, store.BindUserOIDCIdentityParams{
				ID: user.ID, OidcIssuer: &issuer, OidcSubject: &subject,
			})
			if err != nil {
				return store.User{}, err
			}
			return bound, nil
		case !errors.Is(err, pgx.ErrNoRows):
			return store.User{}, err
		}
	}

	count, err := s.q.CountUsers(ctx)
	if err != nil {
		return store.User{}, err
	}
	if count > 0 && !s.cfg.OIDC.AllowSignups {
		return store.User{}, oidcDenial("signups are disabled")
	}
	user, err = s.createOIDCUser(ctx, issuer, subject, claims)
	switch uniqueViolation(err) {
	case "users_email_unique":
		// The email belongs to a soft-deleted User; a new account would be a
		// merge, so deny it.
		return store.User{}, oidcDenial("this email belongs to another User")
	case "users_oidc_unique":
		// A concurrent login with the same identity won the insert.
		user, err = s.q.GetUserByOIDCIdentity(ctx, store.GetUserByOIDCIdentityParams{
			OidcIssuer: &issuer, OidcSubject: &subject,
		})
	}
	return user, err
}

func (s *Server) createOIDCUser(ctx context.Context, issuer, subject string, claims oidcClaims) (store.User, error) {
	var email *string
	if claims.Email != "" {
		email = &claims.Email
	}
	name := oidcUserName(subject, claims)
	id, err := uuid.NewV7()
	if err != nil {
		return store.User{}, err
	}
	params := store.CreateOIDCUserParams{
		ID:           id,
		Name:         name,
		Email:        email,
		OidcIssuer:   &issuer,
		OidcSubject:  &subject,
		GpuQuota:     s.cfg.DefaultGPUQuota,
		SandboxQuota: s.cfg.DefaultSandboxQuota,
	}
	user, err := s.q.CreateOIDCUser(ctx, params)
	if uniqueViolation(err) == "users_name_key" {
		// A name collision is not a denial: disambiguate with the subject.
		params.Name = uniqueOIDCName(name, subject)
		user, err = s.q.CreateOIDCUser(ctx, params)
	}
	if err != nil {
		return store.User{}, err
	}
	s.auditAs(ctx, &user.ID, nil, "user.create", "user", user.ID.String(), map[string]any{
		"name": user.Name, "login_type": user.LoginType, "is_admin": user.IsAdmin, "oidc": true,
	})
	return user, nil
}

// oidcProviderFor discovers the issuer once and remembers it. Discovery runs
// on the first login rather than at startup, so a slow IdP cannot delay
// serving.
func (s *Server) oidcProviderFor(ctx context.Context) (*oidc.Provider, error) {
	s.oidcMu.Lock()
	defer s.oidcMu.Unlock()
	if s.oidcProvider != nil {
		return s.oidcProvider, nil
	}
	provider, err := oidc.NewProvider(ctx, s.cfg.OIDC.IssuerURL)
	if err != nil {
		return nil, err
	}
	s.oidcProvider = provider
	return provider, nil
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

// emailDomainAllowed treats an empty configuration as unrestricted.
func emailDomainAllowed(email string, domains []string) bool {
	if len(domains) == 0 {
		return true
	}
	if !validEmail(email) {
		return false
	}
	domain := strings.ToLower(email[strings.LastIndexByte(email, '@')+1:])
	for _, allowed := range domains {
		if domain == allowed {
			return true
		}
	}
	return false
}

// oidcUserName picks a stable display name from the claims, falling back to
// the subject. The first User has no admin to choose one for them.
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
