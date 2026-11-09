package api

import (
	"context"
	"net/http"
	"time"

	"github.com/zibyn/orrery/internal/store"
)

// startBrowserSession mints a Session for a Polaris login, records the Polaris
// sid, sets its cookie and audits it. No idempotency key: the cookie is the
// only copy of the secret, so a retried login cannot return the old one
// (api.md §1).
func (s *Server) startBrowserSession(w http.ResponseWriter, ctx context.Context, user store.User, sid *string, detail any) (store.ApiToken, error) {
	token, raw, err := s.newSession(ctx, user, sid)
	if err != nil {
		return store.ApiToken{}, err
	}
	http.SetCookie(w, newSessionCookie(raw))
	s.auditAs(ctx, &user.ID, &token.ID, "session.create", "session", token.ID.String(), detail)
	return token, nil
}

// newSession inserts a Session row, scoped admin for an admin User, and
// returns it with its raw secret.
func (s *Server) newSession(ctx context.Context, user store.User, sid *string) (store.ApiToken, string, error) {
	raw, id, hash, err := NewToken()
	if err != nil {
		return store.ApiToken{}, "", err
	}
	scope := "user"
	if user.IsAdmin {
		scope = "admin"
	}
	expires := time.Now().Add(sessionIdleTTL)
	token, err := s.q.CreateToken(ctx, store.CreateTokenParams{
		ID:         id,
		UserID:     user.ID,
		Kind:       "session",
		Name:       "session",
		SecretHash: hash,
		Scope:      scope,
		ExpiresAt:  timestamptzPtr(&expires),
		Sid:        sid,
	})
	return token, raw, err
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
