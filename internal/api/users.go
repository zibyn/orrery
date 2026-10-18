package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/mail"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/zibyn/stars-sandbox/internal/store"
)

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request, p *principal) {
	key, ok := idempotencyKey(r)
	if !ok {
		writeProblem(w, "invalid_request", "Idempotency-Key is too long")
		return
	}
	ctx := r.Context()
	lookup := func() (store.User, error) {
		return s.q.GetUserByIdempotencyKey(ctx, store.GetUserByIdempotencyKeyParams{
			CreatedBy: &p.user.ID, IdempotencyKey: key,
		})
	}
	if replayOr(s, w, "look up idempotent user", key, lookup, func(u store.User) {
		writeJSON(w, http.StatusOK, userToDTO(u))
	}) {
		return
	}

	var req createUserRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	email, ok := normalizeEmail(w, req.Email)
	if !ok {
		return
	}
	if !validDisplayName(req.Name) {
		writeProblem(w, "invalid_request", "name must be non-empty, trimmed, and at most 255 characters")
		return
	}
	if !validLoginType(req.LoginType) {
		writeProblem(w, "invalid_request", "login_type must be password, oidc, or none")
		return
	}
	var passwordHash *string
	if req.Password != nil {
		if req.LoginType != "password" {
			writeProblem(w, "invalid_request", "password requires login_type=password")
			return
		}
		if !validPassword(*req.Password) {
			writeProblem(w, "invalid_request", "password must be at least 12 characters")
			return
		}
		h, err := HashPassword(*req.Password)
		if err != nil {
			s.internal(w, "hash password", err)
			return
		}
		passwordHash = &h
	}
	id, err := uuid.NewV7()
	if err != nil {
		s.internal(w, "generate user id", err)
		return
	}
	user, replayed, err := createIdempotent(key, "users_idempotency_key_unique", lookup,
		func() (store.User, error) {
			return s.q.CreateUser(ctx, store.CreateUserParams{
				ID:             id,
				Name:           req.Name,
				Email:          email,
				IsAdmin:        req.IsAdmin,
				LoginType:      req.LoginType,
				PasswordHash:   passwordHash,
				GpuQuota:       s.cfg.DefaultGPUQuota,
				SandboxQuota:   s.cfg.DefaultSandboxQuota,
				IdempotencyKey: key,
				CreatedBy:      &p.user.ID,
			})
		})
	if err != nil {
		if writeConstraintProblem(w, err) {
			return
		}
		s.internal(w, "create user", err)
		return
	}
	if replayed {
		writeJSON(w, http.StatusOK, userToDTO(user))
		return
	}
	s.audit(r.Context(), p, "user.create", "user", user.ID.String(), map[string]any{
		"name":       user.Name,
		"login_type": user.LoginType,
		"is_admin":   user.IsAdmin,
	})
	writeJSON(w, http.StatusCreated, userToDTO(user))
}

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request, p *principal) {
	rows, next, ok := readPage(s, w, r, "list users", parseCursor,
		func(ctx context.Context, limit int32, after *uuid.UUID) ([]store.User, error) {
			return s.q.ListUsers(ctx, store.ListUsersParams{After: after, Limit: limit})
		},
		func(u store.User) string { return EncodeCursor(u.ID) })
	if !ok {
		return
	}
	out := userList{Items: make([]userDTO, 0, len(rows)), NextCursor: next}
	for _, row := range rows {
		out.Items = append(out.Items, userToDTO(row))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetUser(w http.ResponseWriter, r *http.Request, p *principal) {
	id, ok := pathUUID(r, "id")
	if !ok {
		writeProblem(w, "not_found", "")
		return
	}
	user, err := s.q.GetUser(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeProblem(w, "not_found", "")
		return
	}
	if err != nil {
		s.internal(w, "get user", err)
		return
	}
	writeJSON(w, http.StatusOK, userToDTO(user))
}

func (s *Server) handleUpdateUser(w http.ResponseWriter, r *http.Request, p *principal) {
	id, ok := pathUUID(r, "id")
	if !ok {
		writeProblem(w, "not_found", "")
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
	var req updateUserRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	var changed []string
	passwordChanged := false
	if req.Name != nil {
		if !validDisplayName(*req.Name) {
			writeProblem(w, "invalid_request", "name must be non-empty, trimmed, and at most 255 characters")
			return
		}
		user.Name = *req.Name
		changed = append(changed, "name")
	}
	if req.Email != nil {
		email, ok := normalizeEmail(w, req.Email)
		if !ok {
			return
		}
		user.Email = email
		changed = append(changed, "email")
	}
	if req.LoginType != nil {
		if !validLoginType(*req.LoginType) {
			writeProblem(w, "invalid_request", "login_type must be password, oidc, or none")
			return
		}
		user.LoginType = *req.LoginType
		changed = append(changed, "login_type")
	}
	if req.Password != nil {
		if !validPassword(*req.Password) {
			writeProblem(w, "invalid_request", "password must be at least 12 characters")
			return
		}
		if user.LoginType != "password" {
			writeProblem(w, "invalid_request", "password requires login_type=password")
			return
		}
		hash, err := HashPassword(*req.Password)
		if err != nil {
			s.internal(w, "hash password", err)
			return
		}
		user.PasswordHash = &hash
		passwordChanged = true
		changed = append(changed, "password")
	}
	if req.IsAdmin != nil {
		user.IsAdmin = *req.IsAdmin
		changed = append(changed, "is_admin")
	}
	// A User that no longer logs in with a password keeps no password hash.
	if user.LoginType != "password" {
		user.PasswordHash = nil
	}

	params := store.UpdateUserParams{
		ID:           user.ID,
		Name:         user.Name,
		Email:        user.Email,
		IsAdmin:      user.IsAdmin,
		LoginType:    user.LoginType,
		PasswordHash: user.PasswordHash,
		GpuQuota:     user.GpuQuota,
		SandboxQuota: user.SandboxQuota,
	}
	var updated store.User
	if passwordChanged {
		// ADR-0020: every password change revokes the User's Sessions.
		updated, err = s.updateUserAndRevokeSessions(ctx, params)
	} else {
		updated, err = s.q.UpdateUser(ctx, params)
	}
	if err != nil {
		if writeConstraintProblem(w, err) {
			return
		}
		s.internal(w, "update user", err)
		return
	}
	s.audit(ctx, p, "user.update", "user", updated.ID.String(), map[string]any{"fields": changed})
	writeJSON(w, http.StatusOK, userToDTO(updated))
}

// updateUserAndRevokeSessions commits the update and the Session revocation
// together, so a password change cannot leave old Sessions valid.
func (s *Server) updateUserAndRevokeSessions(ctx context.Context, params store.UpdateUserParams) (store.User, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return store.User{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	q := s.q.WithTx(tx)
	user, err := q.UpdateUser(ctx, params)
	if err != nil {
		return store.User{}, err
	}
	if _, err := q.DeleteUserSessions(ctx, params.ID); err != nil {
		return store.User{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return store.User{}, err
	}
	return user, nil
}

func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request, p *principal) {
	id, ok := pathUUID(r, "id")
	if !ok {
		writeProblem(w, "not_found", "")
		return
	}
	ctx := r.Context()
	user, err := s.q.SoftDeleteUser(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeProblem(w, "not_found", "")
		return
	}
	if err != nil {
		s.internal(w, "delete user", err)
		return
	}
	s.audit(ctx, p, "user.delete", "user", user.ID.String(), nil)
	writeJSON(w, http.StatusAccepted, userToDTO(user))
}

func (s *Server) handleSetUserQuota(w http.ResponseWriter, r *http.Request, p *principal) {
	id, ok := pathUUID(r, "id")
	if !ok {
		writeProblem(w, "not_found", "")
		return
	}
	var req setQuotaRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.GPUs < 0 || req.Sandboxes < 0 {
		writeProblem(w, "invalid_request", "quota values must be non-negative")
		return
	}
	ctx := r.Context()
	user, err := s.q.SetUserQuota(ctx, store.SetUserQuotaParams{
		ID: id, GpuQuota: req.GPUs, SandboxQuota: req.Sandboxes,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeProblem(w, "not_found", "")
		return
	}
	if err != nil {
		s.internal(w, "set quota", err)
		return
	}
	s.audit(ctx, p, "user.quota_update", "user", user.ID.String(), map[string]any{
		"gpus": req.GPUs, "sandboxes": req.Sandboxes,
	})
	writeJSON(w, http.StatusOK, userToDTO(user))
}

func (s *Server) handleGetMe(w http.ResponseWriter, r *http.Request, p *principal) {
	writeJSON(w, http.StatusOK, userToDTO(p.user))
}

func (s *Server) handleGetMyQuota(w http.ResponseWriter, r *http.Request, p *principal) {
	usage, err := s.q.CountActiveSandboxResources(r.Context(), p.user.ID)
	if err != nil {
		s.internal(w, "count active Sandboxes", err)
		return
	}
	writeJSON(w, http.StatusOK, userQuotaUsage{
		GPUs:      quotaUsageItem{Limit: p.user.GpuQuota, Used: int32(usage.Gpus)},
		Sandboxes: quotaUsageItem{Limit: p.user.SandboxQuota, Used: int32(usage.Sandboxes)},
	})
}

func pathUUID(r *http.Request, name string) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue(name))
	if err != nil {
		return uuid.UUID{}, false
	}
	return id, true
}

func validEmail(email string) bool {
	addr, err := mail.ParseAddress(email)
	return err == nil && addr.Address == email
}

func normalizeEmail(w http.ResponseWriter, email *string) (*string, bool) {
	if email == nil || *email == "" {
		return nil, true
	}
	if !validEmail(*email) {
		writeProblem(w, "invalid_request", "invalid email address")
		return nil, false
	}
	return email, true
}

// uniqueViolation returns the violated unique constraint or index name.
func uniqueViolation(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return pgErr.ConstraintName
	}
	return ""
}

func (s *Server) audit(ctx context.Context, p *principal, action, targetType, targetID string, detail any) {
	var actorUser *uuid.UUID
	var actorToken *uuid.UUID
	if p != nil {
		actorUser = &p.user.ID
		actorToken = &p.token.ID
	}
	s.auditAs(ctx, actorUser, actorToken, action, targetType, targetID, detail)
}

// auditAs inserts after the mutation commits. A failed insert is logged and
// never fails the request (ADR-0016).
func (s *Server) auditAs(ctx context.Context, actorUserID, actorTokenID *uuid.UUID, action, targetType, targetID string, detail any) {
	raw, err := json.Marshal(detail)
	if err != nil || string(raw) == "null" {
		raw = []byte("{}")
	}
	err = s.q.InsertAudit(ctx, store.InsertAuditParams{
		ActorUserID:   actorUserID,
		ActorTokenID:  actorTokenID,
		Action:        action,
		TargetType:    targetType,
		TargetID:      targetID,
		CorrelationID: correlationID(ctx),
		Detail:        raw,
	})
	if err != nil {
		s.log.Error("audit insert failed", "action", action, "target_id", targetID, "error", err)
	}
}
