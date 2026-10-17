package api

import (
	"context"
	"errors"
	"fmt"
	"net/mail"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/zibyn/stars-sandbox/internal/store"
)

type BootstrapOptions struct {
	Username string
	Email    string
	Password string
}

// Bootstrap creates the first admin while no User exists. It is the only path
// that creates a User without a caller (ADR-0020).
func (s *Server) Bootstrap(ctx context.Context, opts BootstrapOptions) (store.User, error) {
	if !validDisplayName(opts.Username) {
		return store.User{}, fmt.Errorf("username must be non-empty, trimmed, and at most 255 characters")
	}
	if !validPassword(opts.Password) {
		return store.User{}, fmt.Errorf("password must be at least 12 characters")
	}
	var email *string
	if opts.Email != "" {
		addr, err := mail.ParseAddress(opts.Email)
		if err != nil || addr.Address != opts.Email {
			return store.User{}, fmt.Errorf("invalid email address")
		}
		email = &opts.Email
	}
	hash, err := HashPassword(opts.Password)
	if err != nil {
		return store.User{}, fmt.Errorf("hash password: %w", err)
	}
	id, err := uuid.NewV7()
	if err != nil {
		return store.User{}, fmt.Errorf("generate user id: %w", err)
	}
	user, err := s.q.BootstrapAdmin(ctx, store.BootstrapAdminParams{
		ID:           id,
		Name:         opts.Username,
		Email:        email,
		PasswordHash: &hash,
		GpuQuota:     s.cfg.DefaultGPUQuota,
		SandboxQuota: s.cfg.DefaultSandboxQuota,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return store.User{}, fmt.Errorf("bootstrap: a User already exists")
	}
	if err != nil {
		return store.User{}, fmt.Errorf("create first admin: %w", err)
	}
	s.auditAs(ctx, &user.ID, nil, "user.create", "user", user.ID.String(), map[string]any{
		"name": user.Name, "is_admin": true, "bootstrap": true,
	})
	return user, nil
}
