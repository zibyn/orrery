package api

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/zibyn/orrery/internal/store"
)

// The migration that drops passwords refuses while a password User is not yet
// linked to Polaris, so a deployment cannot switch before
// orrery-polaris-migrate has run (ADR-0029).
func TestMigrateRefusesUnlinkedPasswordUsers(t *testing.T) {
	dsn := os.Getenv("ORRERY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ORRERY_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	admin, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	name := "migrate_guard_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if a, err := Open(context.Background(), dsn); err == nil {
			a.Exec(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)") //nolint:errcheck
			a.Close()
		}
	})
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Database = name
	dbDSN := fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable",
		cfg.ConnConfig.User, cfg.ConnConfig.Password, cfg.ConnConfig.Host, cfg.ConnConfig.Port, name)

	// The schema as the release before the switch left it.
	src, err := iofs.New(store.Migrations, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, migrateDSN(dbDSN))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Migrate(dropPasswordsVersion - 2); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatal(err)
	}
	m.Close()
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	id := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, name, login_type, password_hash) VALUES ($1, 'alice', 'password', 'x')`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, name, login_type) VALUES ($1, 'ci', 'none')`, uuid.New()); err != nil {
		t.Fatal(err)
	}

	err = Migrate(ctx, pool, dbDSN)
	if err == nil || !strings.Contains(err.Error(), "orrery-polaris-migrate") || !strings.Contains(err.Error(), "alice") {
		t.Fatalf("Migrate with an unlinked password User = %v, want a refusal naming alice and the script", err)
	}

	if _, err := pool.Exec(ctx, `UPDATE users SET oidc_issuer = 'https://polaris.test', oidc_subject = 'sub-alice' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, pool, dbDSN); err != nil {
		t.Fatalf("Migrate once every password User is linked: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, name) VALUES ($1, 'bot')`, uuid.New()); err != nil {
		t.Fatalf("a machine User without a login type: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, name, oidc_issuer) VALUES ($1, 'half', 'https://polaris.test')`, uuid.New()); err == nil {
		t.Fatal("a User with an issuer but no subject was accepted")
	}
}
