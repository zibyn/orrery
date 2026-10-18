package api

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/zibyn/stars-sandbox/internal/store"
)

// handleCreateSession is the password login. Without a "token" field it sets
// the Session cookie; with one it issues a named API Token and sets no cookie,
// which is what `stars-sandbox login` uses.
func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request, _ *principal) {
	var req sessionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	ctx := r.Context()
	limitKey := req.Username + "\x00" + splitClientIP(r.RemoteAddr)
	now := time.Now()
	if retry, blocked := s.logins.retryAfter(limitKey, now); blocked {
		w.Header().Set("Retry-After", strconv.Itoa(max(1, int(math.Ceil(retry.Seconds())))))
		writeProblem(w, "rate_limited", "too many failed logins; retry later")
		return
	}

	user, err := s.q.GetUserByName(ctx, req.Username)
	if err != nil || user.LoginType != "password" || user.PasswordHash == nil {
		burnPasswordCheck(req.Password)
		s.logins.fail(limitKey, now)
		writeUnauthenticated(w, "invalid username or password")
		return
	}
	passwordHash := *user.PasswordHash
	if !VerifyPassword(passwordHash, req.Password) {
		s.logins.fail(limitKey, now)
		writeUnauthenticated(w, "invalid username or password")
		return
	}
	s.logins.succeed(limitKey)

	// Issuing the token is conditional on this hash still being current, so a
	// password change cannot be overtaken by a login that verified the old one
	// (#123).
	if req.Token != nil {
		s.createLoginToken(w, r, user, *req.Token, passwordHash)
		return
	}
	s.createBrowserSession(w, r, user, passwordHash)
}

func (s *Server) createLoginToken(w http.ResponseWriter, r *http.Request, user store.User, tokenReq createTokenRequest, passwordHash string) {
	ctx := r.Context()
	if code, detail, ok := s.validateTokenRequest(tokenReq, &principal{user: user}); !ok {
		writeProblem(w, code, detail)
		return
	}
	key, ok := idempotencyKey(r)
	if !ok {
		writeProblem(w, "invalid_request", "Idempotency-Key is too long")
		return
	}
	token, raw, replayed, err := s.issueNamedToken(ctx, user.ID, tokenReq, key, &passwordHash)
	if errors.Is(err, errTokenNameTaken) {
		writeProblem(w, "conflict", "a token with that name already exists")
		return
	}
	if errors.Is(err, pgx.ErrNoRows) {
		writeUnauthenticated(w, "invalid username or password")
		return
	}
	if err != nil {
		s.internal(w, "create login token", err)
		return
	}
	if replayed {
		writeJSON(w, http.StatusOK, tokenToDTO(token))
		return
	}
	s.auditAs(ctx, &user.ID, nil, "token.create", "token", token.ID.String(), map[string]any{
		"name": token.Name, "scope": token.Scope, "via": "session",
	})
	writeJSON(w, http.StatusCreated, createdTokenDTO{tokenDTO: tokenToDTO(token), Token: raw})
}

func (s *Server) createBrowserSession(w http.ResponseWriter, r *http.Request, user store.User, passwordHash string) {
	token, err := s.startBrowserSession(w, r.Context(), user, &passwordHash, nil)
	if errors.Is(err, pgx.ErrNoRows) {
		writeUnauthenticated(w, "invalid username or password")
		return
	}
	if err != nil {
		s.internal(w, "create session", err)
		return
	}
	writeJSON(w, http.StatusCreated, tokenToDTO(token))
}

// startBrowserSession mints a Session, sets its cookie and audits it. A nil
// passwordHash is an OIDC login; otherwise the insert is conditional on that
// hash still being current (#123). No idempotency key: the cookie is the only
// copy of the secret, so a retried login cannot return the old one (api.md §1).
func (s *Server) startBrowserSession(w http.ResponseWriter, ctx context.Context, user store.User, passwordHash *string, detail any) (store.ApiToken, error) {
	raw, id, hash, err := NewToken()
	if err != nil {
		return store.ApiToken{}, err
	}
	scope := "user"
	if user.IsAdmin {
		scope = "admin"
	}
	expires := time.Now().Add(sessionIdleTTL)
	token, err := s.insertToken(ctx, store.CreateTokenParams{
		ID:         id,
		UserID:     user.ID,
		Kind:       "session",
		Name:       "session",
		SecretHash: hash,
		Scope:      scope,
		ExpiresAt:  timestamptzPtr(&expires),
	}, passwordHash)
	if err != nil {
		return store.ApiToken{}, err
	}
	http.SetCookie(w, newSessionCookie(raw))
	s.auditAs(ctx, &user.ID, &token.ID, "session.create", "session", token.ID.String(), detail)
	return token, nil
}

func (s *Server) handleDeleteCurrentSession(w http.ResponseWriter, r *http.Request, p *principal) {
	if p.token.Kind != "session" {
		writeProblem(w, "invalid_request", "the current credential is not a Session")
		return
	}
	ctx := r.Context()
	rows, err := s.q.DeleteToken(ctx, store.DeleteTokenParams{ID: p.token.ID, UserID: p.user.ID})
	if err != nil {
		s.internal(w, "delete session", err)
		return
	}
	if rows == 0 {
		writeProblem(w, "not_found", "")
		return
	}
	http.SetCookie(w, clearSessionCookie())
	s.audit(ctx, p, "session.delete", "session", p.token.ID.String(), nil)
	writeJSON(w, http.StatusAccepted, tokenDTO{
		ID:         p.token.ID.String(),
		Kind:       p.token.Kind,
		Name:       p.token.Name,
		Scope:      p.token.Scope,
		ExpiresAt:  timePtr(p.token.ExpiresAt),
		LastUsedAt: timePtr(p.token.LastUsedAt),
		CreatedAt:  p.token.CreatedAt.Time.UTC(),
	})
}

func (s *Server) handleChangeMyPassword(w http.ResponseWriter, r *http.Request, p *principal) {
	if p.user.LoginType != "password" {
		writeProblem(w, "invalid_request", "only a password User can change their password")
		return
	}
	var req changePasswordRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !validPassword(req.New) {
		writeProblem(w, "invalid_request", "password must be at least 12 characters")
		return
	}
	if p.user.PasswordHash == nil || !VerifyPassword(*p.user.PasswordHash, req.Old) {
		writeProblem(w, "invalid_request", "old password is incorrect")
		return
	}
	ctx := r.Context()
	updated, err := s.updatePasswordAndRevokeSessions(ctx, p.user.ID, req.New)
	if errors.Is(err, pgx.ErrNoRows) {
		writeUnauthenticated(w, "unknown User")
		return
	}
	if err != nil {
		s.internal(w, "change password", err)
		return
	}
	if p.viaCookie {
		http.SetCookie(w, clearSessionCookie())
	}
	s.audit(ctx, p, "user.password_change", "user", updated.ID.String(), nil)
	writeJSON(w, http.StatusOK, userToDTO(updated))
}

func (s *Server) handleResetUserPassword(w http.ResponseWriter, r *http.Request, p *principal) {
	id, ok := pathUUID(r, "id")
	if !ok {
		writeProblem(w, "not_found", "")
		return
	}
	var req resetPasswordRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !validPassword(req.Password) {
		writeProblem(w, "invalid_request", "password must be at least 12 characters")
		return
	}
	ctx := r.Context()
	user, err := s.q.GetUser(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeProblem(w, "not_found", "")
		return
	}
	if err != nil {
		s.internal(w, "get user", err)
		return
	}
	if user.LoginType != "password" {
		writeProblem(w, "invalid_request", "only a password User has a password to reset")
		return
	}
	updated, err := s.updatePasswordAndRevokeSessions(ctx, id, req.Password)
	if errors.Is(err, pgx.ErrNoRows) {
		writeProblem(w, "not_found", "")
		return
	}
	if err != nil {
		s.internal(w, "reset password", err)
		return
	}
	s.audit(ctx, p, "user.password_reset", "user", updated.ID.String(), nil)
	writeJSON(w, http.StatusOK, userToDTO(updated))
}

// updatePasswordAndRevokeSessions commits the hash and the Session revocation
// together: a half-applied password change would leave old Sessions valid.
func (s *Server) updatePasswordAndRevokeSessions(ctx context.Context, id uuid.UUID, password string) (store.User, error) {
	hash, err := HashPassword(password)
	if err != nil {
		return store.User{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return store.User{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	q := s.q.WithTx(tx)
	user, err := q.UpdateUserPassword(ctx, store.UpdateUserPasswordParams{ID: id, PasswordHash: &hash})
	if err != nil {
		return store.User{}, err
	}
	if _, err := q.DeleteUserSessions(ctx, id); err != nil {
		return store.User{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return store.User{}, err
	}
	return user, nil
}
