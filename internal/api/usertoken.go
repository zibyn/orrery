package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/zibyn/orrery/internal/store"
)

// UserTokenOptions are the flags of orrery-server user-token. Exactly one of
// Name (a machine User) and Sub (a Polaris-linked User) is set. Session mints
// a Session instead of an API Token, for tests that drive the console; its
// scope follows the User's admin status, like a login's.
type UserTokenOptions struct {
	Name      string
	Sub       string
	Admin     bool
	Scope     string
	TokenName string
	Session   bool
}

// CreatedToken is the token-creation API response.
type CreatedToken = createdTokenDTO

const userTokenActor = "orrery-server user-token"

// UserToken mints an API Token from the server host, without the network API
// or Polaris (ADR-0029). It is the only path that creates a machine User and
// the break-glass path for a Polaris-linked one.
func (s *Server) UserToken(ctx context.Context, opts UserTokenOptions) (CreatedToken, error) {
	if (opts.Name == "") == (opts.Sub == "") {
		return CreatedToken{}, fmt.Errorf("exactly one of --name and --sub is required")
	}
	if opts.Sub != "" && opts.Admin {
		return CreatedToken{}, fmt.Errorf("--admin applies only to --name; a linked User's admin status comes from Polaris")
	}
	if opts.Session && opts.TokenName != "" {
		return CreatedToken{}, fmt.Errorf("--token-name does not apply to --session")
	}
	if opts.TokenName == "" {
		b := make([]byte, 4)
		if _, err := rand.Read(b); err != nil {
			return CreatedToken{}, err
		}
		opts.TokenName = "user-token-" + hex.EncodeToString(b)
	}
	var (
		user store.User
		err  error
	)
	if opts.Sub != "" {
		user, err = s.q.GetUserByOIDCIdentity(ctx, store.GetUserByOIDCIdentityParams{
			OidcIssuer: &s.cfg.OIDC.IssuerURL, OidcSubject: &opts.Sub,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return CreatedToken{}, fmt.Errorf("no User has sub %q under issuer %q", opts.Sub, s.cfg.OIDC.IssuerURL)
		}
	} else {
		user, err = s.machineUser(ctx, opts.Name, opts.Admin)
	}
	if err != nil {
		return CreatedToken{}, err
	}

	if opts.Session {
		token, raw, err := s.newSession(ctx, user, nil)
		if err != nil {
			return CreatedToken{}, err
		}
		s.auditAs(ctx, nil, nil, "session.create", "session", token.ID.String(), map[string]any{
			"user_id": user.ID.String(), "actor": userTokenActor,
		})
		return CreatedToken{tokenDTO: tokenToDTO(token), Token: raw}, nil
	}
	req := createTokenRequest{Name: opts.TokenName, Scope: opts.Scope}
	if _, detail, ok := s.validateTokenRequest(req, &principal{user: user}); !ok {
		return CreatedToken{}, errors.New(detail)
	}
	token, raw, _, err := s.issueNamedToken(ctx, user.ID, req, nil)
	if err != nil {
		return CreatedToken{}, err
	}
	s.auditAs(ctx, nil, nil, "token.create", "token", token.ID.String(), map[string]any{
		"name": token.Name, "scope": token.Scope, "user_id": user.ID.String(), "actor": userTokenActor,
	})
	return CreatedToken{tokenDTO: tokenToDTO(token), Token: raw}, nil
}

// machineUser finds or creates the machine User called name; admin makes it,
// or keeps it, an admin.
func (s *Server) machineUser(ctx context.Context, name string, admin bool) (store.User, error) {
	if !validDisplayName(name) {
		return store.User{}, fmt.Errorf("name must be non-empty, trimmed, and at most 255 characters")
	}
	user, err := s.q.GetUserByName(ctx, name)
	if errors.Is(err, pgx.ErrNoRows) {
		id, err := uuid.NewV7()
		if err != nil {
			return store.User{}, err
		}
		user, err = s.q.CreateMachineUser(ctx, store.CreateMachineUserParams{
			ID:           id,
			Name:         name,
			IsAdmin:      admin,
			GpuQuota:     s.cfg.DefaultGPUQuota,
			SandboxQuota: s.cfg.DefaultSandboxQuota,
		})
		if err != nil {
			return store.User{}, fmt.Errorf("create machine User: %w", err)
		}
		s.auditAs(ctx, nil, nil, "user.create", "user", user.ID.String(), map[string]any{
			"name": user.Name, "is_admin": user.IsAdmin, "machine": true, "actor": userTokenActor,
		})
		return user, nil
	}
	if err != nil {
		return store.User{}, err
	}
	if user.OidcIssuer != nil || user.OidcSubject != nil {
		return store.User{}, fmt.Errorf("%q is a Polaris-linked User; use --sub", name)
	}
	if admin && !user.IsAdmin {
		user, err = s.q.UpdateUser(ctx, store.UpdateUserParams{
			ID:           user.ID,
			Name:         user.Name,
			Email:        user.Email,
			IsAdmin:      true,
			GpuQuota:     user.GpuQuota,
			SandboxQuota: user.SandboxQuota,
		})
		if err != nil {
			return store.User{}, fmt.Errorf("make %q an admin: %w", name, err)
		}
		s.auditAs(ctx, nil, nil, "user.update", "user", user.ID.String(), map[string]any{
			"is_admin": true, "actor": userTokenActor,
		})
	}
	return user, nil
}
