package api

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/zibyn/orrery/internal/store"
)

func oidcTestServer(t *testing.T) (*Server, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("ORRERY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ORRERY_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := Migrate(ctx, pool, dsn); err != nil {
		t.Fatal(err)
	}
	s := &Server{pool: pool, q: store.New(pool), log: slog.New(slog.DiscardHandler)}
	s.cfg.OIDC.AllowSignups = true
	return s, pool
}

// Two first logins match the same unlinked User by email. The one that binds
// second must not overwrite the first: it is let through only for the same
// identity and denied for a different one.
func TestOIDCEmailBindRace(t *testing.T) {
	s, pool := oidcTestServer(t)
	ctx := context.Background()
	for _, tt := range []struct {
		name     string
		second   string
		wantDeny bool
	}{
		{"same identity", "sub-a", false},
		{"different identity", "sub-b", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			id := uuid.New()
			email := id.String() + "@example.com"
			issuer := "https://idp-" + id.String()
			if _, err := pool.Exec(ctx, `INSERT INTO users (id, name, email, login_type) VALUES ($1, $2, $3, 'oidc')`,
				id, "u-"+id.String(), email); err != nil {
				t.Fatal(err)
			}
			// The first login holds the row with its bind uncommitted.
			first, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer first.Rollback(ctx) //nolint:errcheck
			if _, err := first.Exec(ctx, `UPDATE users SET oidc_issuer = $2, oidc_subject = 'sub-a' WHERE id = $1`,
				id, issuer); err != nil {
				t.Fatal(err)
			}

			type result struct {
				user store.User
				err  error
			}
			done := make(chan result)
			go func() {
				u, err := s.oidcUser(ctx, issuer, tt.second, oidcClaims{Email: email})
				done <- result{u, err}
			}()
			// Give the second login time to block on the held row.
			time.Sleep(300 * time.Millisecond)
			if err := first.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			got := <-done

			var denial oidcDenial
			if tt.wantDeny {
				if !errors.As(got.err, &denial) {
					t.Fatalf("second login: err = %v, want an oidcDenial", got.err)
				}
			} else if got.err != nil || got.user.ID != id {
				t.Fatalf("second login: user %v, err %v; want the bound User", got.user.ID, got.err)
			}
			var subject string
			if err := pool.QueryRow(ctx, `SELECT oidc_subject FROM users WHERE id = $1`, id).Scan(&subject); err != nil {
				t.Fatal(err)
			}
			if subject != "sub-a" {
				t.Fatalf("oidc_subject = %q, want the first login's sub-a", subject)
			}
		})
	}
}

// A signup waits for the signup lock, so it cannot read the User count while
// another signup is still deciding first-admin.
func TestOIDCSignupTakesSignupLock(t *testing.T) {
	s, pool := oidcTestServer(t)
	ctx := context.Background()
	held, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Rollback(ctx) //nolint:errcheck
	if _, err := held.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", oidcSignupLockID); err != nil {
		t.Fatal(err)
	}

	issuer, subject := "https://idp-"+uuid.NewString(), "sub-lock"
	done := make(chan error)
	go func() {
		_, err := s.createOIDCUser(ctx, issuer, subject, oidcClaims{PreferredUsername: "lock-" + uuid.NewString()})
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("signup finished while the lock was held (err %v)", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := held.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE oidc_issuer = $1`, issuer).Scan(&n); err != nil || n != 1 {
		t.Fatalf("users for issuer = %d (err %v), want 1", n, err)
	}
}
