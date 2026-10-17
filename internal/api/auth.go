package api

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zibyn/stars-sandbox/internal/store"
)

type principal struct {
	user      store.User
	token     store.GetTokenWithUserRow
	viaCookie bool
	// sandbox is set only for a Sandbox Token. Such a principal can call
	// exactly one endpoint (ADR-0016).
	sandbox *store.Sandbox
}

// isAdmin is the effective authority: an admin-scoped token only acts as admin
// while its User is still an admin.
func (p *principal) isAdmin() bool {
	return p.sandbox == nil && p.token.UserIsAdmin && p.token.Scope == "admin"
}

func (s *Server) wrapAuth(level string, next handlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := s.authenticate(w, r)
		if !ok {
			return
		}
		if p.sandbox != nil && level != "sandbox" {
			writeProblem(w, "permission_denied", "a Sandbox Token may only call POST /git/credential")
			return
		}
		if level == "sandbox" && p.sandbox == nil {
			writeProblem(w, "permission_denied", "this endpoint requires a Sandbox Token")
			return
		}
		if level == "admin" && !p.isAdmin() {
			writeProblem(w, "permission_denied", "this endpoint requires an admin-scoped token")
			return
		}
		next(w, r, p)
	}
}

func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) (*principal, bool) {
	raw, viaCookie := credential(r)
	if raw == "" {
		writeUnauthenticated(w, "a Bearer API token or a Session cookie is required")
		return nil, false
	}
	if parts, ok := ParseSandboxToken(raw); ok {
		return s.authenticateSandbox(w, r, parts)
	}
	parts, ok := ParseToken(raw)
	if !ok {
		writeUnauthenticated(w, "malformed API token")
		return nil, false
	}
	ctx := r.Context()
	row, err := s.q.GetTokenWithUser(ctx, parts.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeUnauthenticated(w, "unknown, expired, or revoked token")
		return nil, false
	}
	if err != nil {
		s.internal(w, "look up token", err)
		return nil, false
	}
	if subtle.ConstantTimeCompare(row.SecretHash, HashSecret(parts.Secret)) != 1 {
		writeUnauthenticated(w, "unknown, expired, or revoked token")
		return nil, false
	}
	if row.ExpiresAt.Valid && !row.ExpiresAt.Time.After(time.Now()) {
		writeUnauthenticated(w, "unknown, expired, or revoked token")
		return nil, false
	}
	if row.Scope == "read_only" && r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeProblem(w, "permission_denied", "a read_only token may only make GET requests")
		return nil, false
	}
	user, err := s.q.GetUser(ctx, row.UserID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeUnauthenticated(w, "unknown, expired, or revoked token")
		return nil, false
	}
	if err != nil {
		s.internal(w, "look up token owner", err)
		return nil, false
	}
	// Only cookie-authenticated requests carry ambient browser credentials, so
	// only they need the browser-facing checks (ADR-0020).
	if viaCookie {
		if isWebSocketUpgrade(r) {
			if !sameOrigin(r, r.Header.Get("Origin")) {
				writeProblem(w, "permission_denied", "WebSocket Origin must match the API origin")
				return nil, false
			}
		} else if !isSafeMethod(r.Method) {
			if err := s.crossOrigin.Check(r); err != nil {
				writeProblem(w, "permission_denied", "cross-origin request rejected")
				return nil, false
			}
		}
	}
	// last_used_at is observability, not authorization; a failed write must not
	// fail the request. Session use slides its idle expiry.
	sessionExpires := time.Now().Add(sessionIdleTTL)
	if err := s.q.TouchToken(ctx, store.TouchTokenParams{ID: row.ID, SessionExpiresAt: timestamptzPtr(&sessionExpires)}); err != nil {
		s.log.Warn("touch token failed", "token_id", row.ID, "error", err)
	}
	return &principal{user: user, token: row, viaCookie: viaCookie}, true
}

// authenticateSandbox resolves a Sandbox Token by the Sandbox id embedded in
// it, decrypts the stored secret and compares in constant time (api.md §1,
// schema.md).
func (s *Server) authenticateSandbox(w http.ResponseWriter, r *http.Request, parts TokenParts) (*principal, bool) {
	ctx := r.Context()
	sb, err := s.q.GetSandboxIncludingDeleted(ctx, parts.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeUnauthenticated(w, "unknown or revoked Sandbox Token")
		return nil, false
	}
	if err != nil {
		s.internal(w, "look up Sandbox Token", err)
		return nil, false
	}
	if !sandboxTokenLive(sb) {
		writeUnauthenticated(w, "unknown or revoked Sandbox Token")
		return nil, false
	}
	secret, err := sandboxTokenSecret(s.cfg.EncryptionKeys, sb.OwnerID, sb.TokenCt, sb.TokenKeyID)
	if err != nil {
		// An unopenable row is as good as unknown; the reason is for the
		// operator's log, not the caller.
		s.log.Warn("open Sandbox Token", "sandbox_id", sb.ID, "error", err)
		writeUnauthenticated(w, "unknown or revoked Sandbox Token")
		return nil, false
	}
	if subtle.ConstantTimeCompare([]byte(secret), []byte(parts.Secret)) != 1 {
		writeUnauthenticated(w, "unknown or revoked Sandbox Token")
		return nil, false
	}
	return &principal{sandbox: &sb}, true
}

// credential accepts a Bearer token or the Session cookie. A Bearer header
// wins, so a client that sends one is never treated as a browser.
func credential(r *http.Request) (raw string, viaCookie bool) {
	if raw := bearerToken(r); raw != "" {
		return raw, false
	}
	if c, err := r.Cookie(sessionCookieName); err == nil && c.Value != "" {
		return c.Value, true
	}
	return "", false
}

func bearerToken(r *http.Request) string {
	scheme, rest, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(rest)
}

func writeUnauthenticated(w http.ResponseWriter, detail string) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	writeProblem(w, "unauthenticated", detail)
}
