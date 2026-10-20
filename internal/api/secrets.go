package api

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/zibyn/orrery/internal/store"
)

const (
	// defaults.md: at most 50 Secrets per User, each at most 24 KiB.
	maxSecretsPerUser = 50
	maxSecretBytes    = 24 << 10
)

var reservedSecretPrefixes = []string{"GIT_", "LD_", "ORRERY_"}

// PUT /secrets/{name} creates or overwrites a write-only Secret. The value is
// encrypted with the deployment keyring, bound to its owner, and never read
// back (ADR-0013, ADR-0017).
func (s *Server) handlePutSecret(w http.ResponseWriter, r *http.Request, p *principal) {
	if !s.requireSecrets(w) {
		return
	}
	name := r.PathValue("name")
	var req putSecretRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	value := req.Value
	if code, detail := secretUploadProblem(name, value); code != "" {
		writeProblem(w, code, detail)
		return
	}
	ciphertext, keyID, err := s.cfg.EncryptionKeys.Encrypt([]byte(value), []byte(p.user.ID.String()))
	if err != nil {
		s.internal(w, "encrypt Secret", err)
		return
	}
	id, err := uuid.NewV7()
	if err != nil {
		s.internal(w, "generate Secret id", err)
		return
	}

	ctx := r.Context()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		s.internal(w, "begin transaction", err)
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	q := s.q.WithTx(tx)
	// The User row lock serializes concurrent creates so the count cannot be
	// exceeded by two requests that both saw room (ADR-0015).
	if _, err := q.LockUser(ctx, p.user.ID); err != nil {
		s.internal(w, "lock User for Secret quota", err)
		return
	}
	_, err = q.GetSecretByName(ctx, store.GetSecretByNameParams{OwnerID: p.user.ID, Name: name})
	existing := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		s.internal(w, "look up Secret", err)
		return
	}
	if !existing {
		count, err := q.CountSecrets(ctx, p.user.ID)
		if err != nil {
			s.internal(w, "count Secrets", err)
			return
		}
		if secretQuotaExceeded(count) {
			writeProblem(w, "quota_exceeded", "a User may hold at most 50 Secrets")
			return
		}
	}
	secret, err := q.UpsertSecret(ctx, store.UpsertSecretParams{
		ID: id, OwnerID: p.user.ID, Name: name,
		Ciphertext: ciphertext, KeyID: keyID, Size: int32(len(value)),
	})
	if err != nil {
		s.internal(w, "upsert Secret", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		s.internal(w, "commit Secret", err)
		return
	}
	action := "secret.create"
	if existing {
		action = "secret.update"
	}
	// Never the value (ADR-0013).
	s.audit(ctx, p, action, "secret", name, map[string]any{"size": len(value)})
	status := http.StatusOK
	if !existing {
		status = http.StatusCreated
	}
	writeJSON(w, status, secretToDTO(secret))
}

// GET /secrets returns names and update times only.
func (s *Server) handleListSecrets(w http.ResponseWriter, r *http.Request, p *principal) {
	if !s.requireSecrets(w) {
		return
	}
	rows, next, ok := readPage(s, w, r, "list Secrets", parseSecretCursor,
		func(ctx context.Context, limit int32, after *string) ([]store.Secret, error) {
			return s.q.ListSecrets(ctx, store.ListSecretsParams{
				OwnerID: p.user.ID, After: after, Limit: limit,
			})
		},
		func(secret store.Secret) string { return encodeSecretCursor(secret.Name) })
	if !ok {
		return
	}
	out := secretList{Items: make([]secretDTO, 0, len(rows)), NextCursor: next}
	for _, row := range rows {
		out.Items = append(out.Items, secretToDTO(row))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleDeleteSecret(w http.ResponseWriter, r *http.Request, p *principal) {
	if !s.requireSecrets(w) {
		return
	}
	secret, err := s.q.DeleteSecret(r.Context(), store.DeleteSecretParams{
		OwnerID: p.user.ID, Name: r.PathValue("name"),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeProblem(w, "not_found", "")
		return
	}
	if err != nil {
		s.internal(w, "delete Secret", err)
		return
	}
	s.audit(r.Context(), p, "secret.delete", "secret", secret.Name, map[string]any{"size": secret.Size})
	writeJSON(w, http.StatusAccepted, secretToDTO(secret))
}

func (s *Server) requireSecrets(w http.ResponseWriter) bool {
	if s.cfg.EncryptionKeys == nil {
		writeProblem(w, "unavailable", "Secret storage is not configured: set encryption_keys in the deployment configuration")
		return false
	}
	return true
}

// Secret cursors are the last name, base64-encoded like the API's other
// opaque cursors (api.md §1).
func encodeSecretCursor(name string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(name))
}

func parseSecretCursor(r *http.Request) (*string, bool) {
	raw := r.URL.Query().Get("cursor")
	if raw == "" {
		return nil, true
	}
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(data) == 0 {
		return nil, false
	}
	name := string(data)
	return &name, true
}

func secretUploadProblem(name, value string) (code, detail string) {
	if !validDisplayName(name) {
		return "invalid_request", "name must be non-empty, trimmed, and at most 255 characters"
	}
	if reservedSecretName(name) {
		return "invalid_request", "name is reserved: GIT_*, LD_*, ORRERY_*, PATH and HOME are not allowed"
	}
	if len(value) > maxSecretBytes {
		return "payload_too_large", "a Secret value is at most 24 KiB"
	}
	return "", ""
}

func reservedSecretName(name string) bool {
	if name == "PATH" || name == "HOME" {
		return true
	}
	for _, prefix := range reservedSecretPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func secretQuotaExceeded(count int64) bool {
	return count >= maxSecretsPerUser
}
