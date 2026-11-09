package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/mail"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/zibyn/orrery/internal/store"
)

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
	var req updateUserRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	// Checks that need no stored state run before the row lock is taken.
	if req.Name != nil && !validDisplayName(*req.Name) {
		writeProblem(w, "invalid_request", "name must be non-empty, trimmed, and at most 255 characters")
		return
	}
	var email *string
	if req.Email != nil {
		if email, ok = normalizeEmail(w, req.Email); !ok {
			return
		}
	}

	ctx := r.Context()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		s.internal(w, "begin update user", err)
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	q := s.q.WithTx(tx)
	// UpdateUser writes every column back, so the read must hold the row:
	// otherwise a concurrent PATCH or quota change is silently reverted.
	user, err := q.LockUser(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeProblem(w, "not_found", "")
		return
	}
	if err != nil {
		s.internal(w, "lock user", err)
		return
	}

	var changed []string
	if req.Name != nil {
		user.Name = *req.Name
		changed = append(changed, "name")
	}
	if req.Email != nil {
		user.Email = email
		changed = append(changed, "email")
	}

	updated, err := q.UpdateUser(ctx, store.UpdateUserParams{
		ID:           user.ID,
		Name:         user.Name,
		Email:        user.Email,
		IsAdmin:      user.IsAdmin,
		GpuQuota:     user.GpuQuota,
		SandboxQuota: user.SandboxQuota,
	})
	if err != nil {
		if writeConstraintProblem(w, err) {
			return
		}
		s.internal(w, "update user", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		s.internal(w, "commit update user", err)
		return
	}
	s.audit(ctx, p, "user.update", "user", updated.ID.String(), map[string]any{"fields": changed})
	writeJSON(w, http.StatusOK, userToDTO(updated))
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

// isUniqueViolation reports any SQLSTATE 23505, whichever index answered.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
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
	// The mutation already committed; a client that disconnected meanwhile
	// must not cancel its Audit Record. The timeout bounds a stuck database.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
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
