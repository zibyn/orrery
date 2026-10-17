package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/zibyn/stars-sandbox/internal/store"
)

func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request, p *principal) {
	key, ok := idempotencyKey(r)
	if !ok {
		writeProblem(w, "invalid_request", "Idempotency-Key is too long")
		return
	}
	var req createTokenRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if code, detail, ok := s.validateTokenRequest(req, p); !ok {
		writeProblem(w, code, detail)
		return
	}
	ctx := r.Context()
	if key != nil {
		if existing, err := s.q.GetTokenByIdempotencyKey(ctx, store.GetTokenByIdempotencyKeyParams{
			UserID: p.user.ID, IdempotencyKey: key,
		}); err == nil {
			// The plaintext secret exists only in the first response.
			writeJSON(w, http.StatusOK, tokenToDTO(existing))
			return
		} else if !errors.Is(err, pgx.ErrNoRows) {
			s.internal(w, "look up idempotent token", err)
			return
		}
	}

	raw, id, hash, err := NewToken()
	if err != nil {
		s.internal(w, "generate token", err)
		return
	}
	token, err := s.q.CreateToken(ctx, store.CreateTokenParams{
		ID:             id,
		UserID:         p.user.ID,
		Kind:           "token",
		Name:           req.Name,
		SecretHash:     hash,
		Scope:          req.Scope,
		ExpiresAt:      timestamptzPtr(req.ExpiresAt),
		IdempotencyKey: key,
	})
	if err != nil {
		switch uniqueViolation(err) {
		case "api_tokens_name_unique":
			writeProblem(w, "conflict", "a token with that name already exists")
			return
		case "api_tokens_idempotency_key_unique":
			if key != nil {
				if existing, lookupErr := s.q.GetTokenByIdempotencyKey(ctx, store.GetTokenByIdempotencyKeyParams{
					UserID: p.user.ID, IdempotencyKey: key,
				}); lookupErr == nil {
					writeJSON(w, http.StatusOK, tokenToDTO(existing))
					return
				}
			}
		}
		s.internal(w, "create token", err)
		return
	}
	s.audit(ctx, p, "token.create", "token", token.ID.String(), map[string]any{
		"name": token.Name, "scope": token.Scope,
	})
	writeJSON(w, http.StatusCreated, createdTokenDTO{tokenDTO: tokenToDTO(token), Token: raw})
}

func (s *Server) handleListTokens(w http.ResponseWriter, r *http.Request, p *principal) {
	limit, ok := parseLimit(r)
	if !ok {
		writeProblem(w, "invalid_request", "limit must be between 1 and 200")
		return
	}
	after, ok := parseCursor(r)
	if !ok {
		writeProblem(w, "invalid_request", "invalid cursor")
		return
	}
	rows, err := s.q.ListTokens(r.Context(), store.ListTokensParams{
		UserID: p.user.ID, After: after, Limit: limit + 1,
	})
	if err != nil {
		s.internal(w, "list tokens", err)
		return
	}
	var next *string
	if len(rows) > int(limit) {
		cursor := EncodeCursor(rows[limit-1].ID)
		next = &cursor
		rows = rows[:limit]
	}
	out := tokenList{Items: make([]tokenDTO, 0, len(rows)), NextCursor: next}
	for _, row := range rows {
		out.Items = append(out.Items, tokenToDTO(row))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleRevokeToken(w http.ResponseWriter, r *http.Request, p *principal) {
	id, ok := pathUUID(r, "id")
	if !ok {
		writeProblem(w, "not_found", "")
		return
	}
	ctx := r.Context()
	token, err := s.q.GetToken(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && token.UserID != p.user.ID) {
		writeProblem(w, "not_found", "")
		return
	}
	if err != nil {
		s.internal(w, "get token", err)
		return
	}
	rows, err := s.q.DeleteToken(ctx, store.DeleteTokenParams{ID: id, UserID: p.user.ID})
	if err != nil {
		s.internal(w, "revoke token", err)
		return
	}
	if rows == 0 {
		writeProblem(w, "not_found", "")
		return
	}
	s.audit(ctx, p, "token.revoke", "token", token.ID.String(), map[string]any{
		"name": token.Name, "scope": token.Scope, "kind": token.Kind,
	})
	writeJSON(w, http.StatusAccepted, tokenToDTO(token))
}

// handleCreateSession implements the token half of POST /sessions. Cookie
// Sessions arrive with the password-login ticket.
func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request, p *principal) {
	var req sessionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Token == nil {
		writeProblem(w, "invalid_request", "browser Sessions are not implemented yet; send token to receive a named API Token")
		return
	}
	ctx := r.Context()
	user, err := s.q.GetUserByName(ctx, req.Username)
	if err != nil || user.LoginType != "password" || user.PasswordHash == nil {
		burnPasswordCheck(req.Password)
		writeProblem(w, "unauthenticated", "invalid username or password")
		return
	}
	if !VerifyPassword(*user.PasswordHash, req.Password) {
		writeUnauthenticated(w, "invalid username or password")
		return
	}
	tokenReq := *req.Token
	if code, detail, ok := s.validateTokenRequest(tokenReq, &principal{user: user}); !ok {
		writeProblem(w, code, detail)
		return
	}
	key, ok := idempotencyKey(r)
	if !ok {
		writeProblem(w, "invalid_request", "Idempotency-Key is too long")
		return
	}
	if key != nil {
		if existing, err := s.q.GetTokenByIdempotencyKey(ctx, store.GetTokenByIdempotencyKeyParams{
			UserID: user.ID, IdempotencyKey: key,
		}); err == nil {
			writeJSON(w, http.StatusOK, tokenToDTO(existing))
			return
		} else if !errors.Is(err, pgx.ErrNoRows) {
			s.internal(w, "look up idempotent token", err)
			return
		}
	}
	raw, id, hash, err := NewToken()
	if err != nil {
		s.internal(w, "generate token", err)
		return
	}
	token, err := s.q.CreateToken(ctx, store.CreateTokenParams{
		ID:             id,
		UserID:         user.ID,
		Kind:           "token",
		Name:           tokenReq.Name,
		SecretHash:     hash,
		Scope:          tokenReq.Scope,
		ExpiresAt:      timestamptzPtr(tokenReq.ExpiresAt),
		IdempotencyKey: key,
	})
	if err != nil {
		switch uniqueViolation(err) {
		case "api_tokens_name_unique":
			writeProblem(w, "conflict", "a token with that name already exists")
			return
		case "api_tokens_idempotency_key_unique":
			if key != nil {
				if existing, lookupErr := s.q.GetTokenByIdempotencyKey(ctx, store.GetTokenByIdempotencyKeyParams{
					UserID: user.ID, IdempotencyKey: key,
				}); lookupErr == nil {
					writeJSON(w, http.StatusOK, tokenToDTO(existing))
					return
				}
			}
		}
		s.internal(w, "create login token", err)
		return
	}
	s.auditAs(ctx, &user.ID, nil, "token.create", "token", token.ID.String(), map[string]any{
		"name": token.Name, "scope": token.Scope, "via": "session",
	})
	writeJSON(w, http.StatusCreated, createdTokenDTO{tokenDTO: tokenToDTO(token), Token: raw})
}

// validateTokenRequest validates everything except the permission to ask for
// admin, which the caller checks with its own principal.
func (s *Server) validateTokenRequest(req createTokenRequest, p *principal) (code, detail string, ok bool) {
	if !validDisplayName(req.Name) {
		return "invalid_request", "name must be non-empty, trimmed, and at most 255 characters", false
	}
	if !validScope(req.Scope) {
		return "invalid_request", "scope must be read_only, user, or admin", false
	}
	if req.Scope == "admin" && !p.user.IsAdmin {
		return "permission_denied", "only an admin User may create an admin-scoped token", false
	}
	if req.ExpiresAt != nil && !req.ExpiresAt.After(time.Now()) {
		return "invalid_request", "expires_at must be in the future", false
	}
	return "", "", true
}
