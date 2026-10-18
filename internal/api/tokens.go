package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/zibyn/stars-sandbox/internal/store"
)

var errTokenNameTaken = errors.New("a token with that name already exists")

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
	token, raw, replayed, err := s.issueNamedToken(r.Context(), p.user.ID, req, key)
	if errors.Is(err, errTokenNameTaken) {
		writeProblem(w, "conflict", "a token with that name already exists")
		return
	}
	if err != nil {
		s.internal(w, "create token", err)
		return
	}
	if replayed {
		// The plaintext secret exists only in the first response.
		writeJSON(w, http.StatusOK, tokenToDTO(token))
		return
	}
	s.audit(r.Context(), p, "token.create", "token", token.ID.String(), map[string]any{
		"name": token.Name, "scope": token.Scope,
	})
	writeJSON(w, http.StatusCreated, createdTokenDTO{tokenDTO: tokenToDTO(token), Token: raw})
}

// issueNamedToken creates one named API Token. replayed means the idempotency
// key already produced a token: its row is returned and raw is empty.
func (s *Server) issueNamedToken(ctx context.Context, userID uuid.UUID, req createTokenRequest, key *string) (store.ApiToken, string, bool, error) {
	lookup := func() (store.ApiToken, error) {
		return s.q.GetTokenByIdempotencyKey(ctx, store.GetTokenByIdempotencyKeyParams{
			UserID: userID, IdempotencyKey: key,
		})
	}
	if existing, replayed, err := replayExisting(key, lookup); err != nil {
		return store.ApiToken{}, "", false, err
	} else if replayed {
		return existing, "", true, nil
	}
	raw, id, hash, err := NewToken()
	if err != nil {
		return store.ApiToken{}, "", false, err
	}
	token, replayed, err := createIdempotent(key, "api_tokens_idempotency_key_unique", lookup,
		func() (store.ApiToken, error) {
			return s.q.CreateToken(ctx, store.CreateTokenParams{
				ID:             id,
				UserID:         userID,
				Kind:           "token",
				Name:           req.Name,
				SecretHash:     hash,
				Scope:          req.Scope,
				ExpiresAt:      timestamptzPtr(req.ExpiresAt),
				IdempotencyKey: key,
			})
		})
	if err != nil {
		if uniqueViolation(err) == "api_tokens_name_unique" {
			return store.ApiToken{}, "", false, errTokenNameTaken
		}
		return store.ApiToken{}, "", false, err
	}
	if replayed {
		return token, "", true, nil
	}
	return token, raw, false, nil
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
	rows, next := trimPage(rows, limit, func(t store.ApiToken) string { return EncodeCursor(t.ID) })
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
