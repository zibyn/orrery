package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/zibyn/stars-sandbox/internal/store"
)

const (
	defaultPageSize = 50
	maxPageSize     = 200
	minPasswordLen  = 12
	maxNameLen      = 255
)

type quotaDTO struct {
	GPUs      int32 `json:"gpus"`
	Sandboxes int32 `json:"sandboxes"`
}

type userDTO struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Email     *string    `json:"email"`
	IsAdmin   bool       `json:"is_admin"`
	LoginType string     `json:"login_type"`
	Quota     quotaDTO   `json:"quota"`
	CreatedAt time.Time  `json:"created_at"`
	DeletedAt *time.Time `json:"deleted_at"`
}

type userList struct {
	Items      []userDTO `json:"items"`
	NextCursor *string   `json:"next_cursor"`
}

type tokenDTO struct {
	ID         string     `json:"id"`
	Kind       string     `json:"kind"`
	Name       string     `json:"name"`
	Scope      string     `json:"scope"`
	ExpiresAt  *time.Time `json:"expires_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	CreatedAt  time.Time  `json:"created_at"`
}

// createdTokenDTO carries the plaintext secret, which appears exactly once.
type createdTokenDTO struct {
	tokenDTO
	Token string `json:"token"`
}

type tokenList struct {
	Items      []tokenDTO `json:"items"`
	NextCursor *string    `json:"next_cursor"`
}

type quotaUsageItem struct {
	Limit int32 `json:"limit"`
	Used  int32 `json:"used"`
}

type userQuotaUsage struct {
	GPUs      quotaUsageItem `json:"gpus"`
	Sandboxes quotaUsageItem `json:"sandboxes"`
}

type auditDTO struct {
	ID            int64           `json:"id"`
	At            time.Time       `json:"at"`
	ActorUserID   *string         `json:"actor_user_id"`
	ActorName     *string         `json:"actor_name"`
	ActorTokenID  *string         `json:"actor_token_id"`
	Action        string          `json:"action"`
	TargetType    string          `json:"target_type"`
	TargetID      string          `json:"target_id"`
	CorrelationID *string         `json:"correlation_id"`
	Detail        json.RawMessage `json:"detail"`
}

type auditList struct {
	Items      []auditDTO `json:"items"`
	NextCursor *string    `json:"next_cursor"`
}

type createUserRequest struct {
	Name      string  `json:"name"`
	Email     *string `json:"email"`
	LoginType string  `json:"login_type"`
	Password  *string `json:"password"`
	IsAdmin   bool    `json:"is_admin"`
}

type updateUserRequest struct {
	Name      *string `json:"name"`
	Email     *string `json:"email"`
	LoginType *string `json:"login_type"`
	Password  *string `json:"password"`
	IsAdmin   *bool   `json:"is_admin"`
}

type setQuotaRequest struct {
	GPUs      int32 `json:"gpus"`
	Sandboxes int32 `json:"sandboxes"`
}

type createTokenRequest struct {
	Name      string     `json:"name"`
	Scope     string     `json:"scope"`
	ExpiresAt *time.Time `json:"expires_at"`
}

type sessionRequest struct {
	Username string              `json:"username"`
	Password string              `json:"password"`
	Token    *createTokenRequest `json:"token"`
}

func userToDTO(u store.User) userDTO {
	return userDTO{
		ID:        u.ID.String(),
		Name:      u.Name,
		Email:     u.Email,
		IsAdmin:   u.IsAdmin,
		LoginType: u.LoginType,
		Quota:     quotaDTO{GPUs: u.GpuQuota, Sandboxes: u.SandboxQuota},
		CreatedAt: u.CreatedAt.Time.UTC(),
		DeletedAt: timePtr(u.DeletedAt),
	}
}

func tokenToDTO(t store.ApiToken) tokenDTO {
	return tokenDTO{
		ID:         t.ID.String(),
		Kind:       t.Kind,
		Name:       t.Name,
		Scope:      t.Scope,
		ExpiresAt:  timePtr(t.ExpiresAt),
		LastUsedAt: timePtr(t.LastUsedAt),
		CreatedAt:  t.CreatedAt.Time.UTC(),
	}
}

func timePtr(ts pgtype.Timestamptz) *time.Time {
	if !ts.Valid {
		return nil
	}
	t := ts.Time.UTC()
	return &t
}

func uuidPtr(id *uuid.UUID) *string {
	if id == nil {
		return nil
	}
	s := id.String()
	return &s
}

func validLoginType(s string) bool {
	return s == "password" || s == "oidc" || s == "none"
}

func validScope(s string) bool {
	return s == "read_only" || s == "user" || s == "admin"
}

func validDisplayName(s string) bool {
	return s != "" && s == strings.TrimSpace(s) && utf8.RuneCountInString(s) <= maxNameLen
}

func validPassword(s string) bool {
	return utf8.RuneCountInString(s) >= minPasswordLen
}

// idempotencyKey returns nil when the header is absent. A present but oversized
// key is a request error.
func idempotencyKey(r *http.Request) (*string, bool) {
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		return nil, true
	}
	if len(key) > maxNameLen {
		return nil, false
	}
	return &key, true
}

func parseLimit(r *http.Request) (int32, bool) {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return defaultPageSize, true
	}
	n, err := strconv.ParseInt(raw, 10, 32)
	if err != nil || n < 1 || n > maxPageSize {
		return 0, false
	}
	return int32(n), true
}

// parseCursor returns nil for no cursor, and false for a malformed one.
func parseCursor(r *http.Request) (*uuid.UUID, bool) {
	raw := r.URL.Query().Get("cursor")
	if raw == "" {
		return nil, true
	}
	id, ok := DecodeCursor(raw)
	if !ok {
		return nil, false
	}
	return &id, true
}
