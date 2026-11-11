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
	// The schema as the release before the switch left it.
	pool, dbDSN := scratchDatabaseAt(t, ctx, dsn, dropPasswordsVersion-2)
	id := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, name, login_type, password_hash) VALUES ($1, 'alice', 'password', 'x')`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, name, login_type) VALUES ($1, 'ci', 'none')`, uuid.New()); err != nil {
		t.Fatal(err)
	}

	err := Migrate(ctx, pool, dbDSN, "https://polaris.test")
	if err == nil || !strings.Contains(err.Error(), "orrery-polaris-migrate") || !strings.Contains(err.Error(), "alice") {
		t.Fatalf("Migrate with an unlinked password User = %v, want a refusal naming alice and the script", err)
	}

	if _, err := pool.Exec(ctx, `UPDATE users SET oidc_issuer = 'https://polaris.test', oidc_subject = 'sub-alice' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, pool, dbDSN, "https://polaris.test"); err != nil {
		t.Fatalf("Migrate once every password User is linked: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, name) VALUES ($1, 'bot')`, uuid.New()); err != nil {
		t.Fatalf("a machine User without a login type: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, name, oidc_issuer) VALUES ($1, 'half', 'https://polaris.test')`, uuid.New()); err == nil {
		t.Fatal("a User with an issuer but no subject was accepted")
	}
}

// A User still linked to a previous issuer would log in to Polaris as a new,
// empty User, so the password drop refuses them too (#255).
func TestMigrateRefusesUsersOnAnotherIssuer(t *testing.T) {
	dsn := os.Getenv("ORRERY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ORRERY_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, dbDSN := scratchDatabaseAt(t, ctx, dsn, dropPasswordsVersion-2)
	id := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, name, login_type, oidc_issuer, oidc_subject) VALUES ($1, 'bob', 'oidc', 'https://old', 'sub-bob')`, id); err != nil {
		t.Fatal(err)
	}
	// Neither a machine User nor a deleted one ever logs in.
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, name, login_type, oidc_issuer, oidc_subject) VALUES ($1, 'ci', 'none', 'https://old', 'sub-ci')`, uuid.New()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, name, login_type, oidc_issuer, oidc_subject, deleted_at) VALUES ($1, 'gone', 'oidc', 'https://old', 'sub-gone', now())`, uuid.New()); err != nil {
		t.Fatal(err)
	}

	err := Migrate(ctx, pool, dbDSN, "https://polaris.test")
	if err == nil || !strings.Contains(err.Error(), "(bob)") {
		t.Fatalf("Migrate with a User on another issuer = %v, want a refusal naming only bob", err)
	}
	// Login compares the issuer byte for byte, so a trailing slash differs.
	if err := Migrate(ctx, pool, dbDSN, "https://old/"); err == nil {
		t.Fatal("Migrate accepted an issuer differing only by a trailing slash")
	}
	if err := Migrate(ctx, pool, dbDSN, "https://old"); err != nil {
		t.Fatalf("Migrate with every User on the configured issuer: %v", err)
	}
}

// scratchDatabaseAt creates a throwaway database migrated to version, so a
// migration test can seed the schema a release before left and then Migrate.
func scratchDatabaseAt(t *testing.T, ctx context.Context, dsn string, version uint) (*pgxpool.Pool, string) {
	t.Helper()
	admin, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	name := "migrate_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	src, err := iofs.New(store.Migrations, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, migrateDSN(dbDSN))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Migrate(version); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatal(err)
	}
	m.Close()
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool, dbDSN
}
