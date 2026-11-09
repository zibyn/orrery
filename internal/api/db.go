package api

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/zibyn/orrery/internal/store"
)

// migrationLockID is an arbitrary fixed advisory lock id, so two Control
// Plane processes starting together serialize their migrations.
const migrationLockID int64 = 0x4F5252455259 // "ORRERY"

func Open(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return pool, nil
}

// Migrate runs forward-only migrations under pg_advisory_xact_lock. There are
// no down migrations: rollback is restoring a backup (ADR-0014).
func Migrate(ctx context.Context, pool *pgxpool.Pool, dsn string) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration connection: %w", err)
	}
	defer conn.Release()

	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin migration lock transaction: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // released with the lock either way
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", migrationLockID); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}

	src, err := iofs.New(store.Migrations, "migrations")
	if err != nil {
		return fmt.Errorf("read embedded migrations: %w", err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, migrateDSN(dsn))
	if err != nil {
		return fmt.Errorf("open migrations: %w", err)
	}
	defer m.Close()
	if err := refuseUnlinkedUsers(ctx, m, pool); err != nil {
		return err
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migrate: %w", err)
	}
	return tx.Commit(ctx)
}

// dropPasswordsVersion is the migration that drops password_hash and
// login_type (ADR-0029).
const dropPasswordsVersion = 29

// refuseUnlinkedUsers stops the password drop while a User who logs in is not
// linked to Polaris: it would come out of the migration as a machine User and
// its person would log in as a new, empty one. Checked here rather than in the
// SQL, because a failed migration leaves the schema dirty.
func refuseUnlinkedUsers(ctx context.Context, m *migrate.Migrate, pool *pgxpool.Pool) error {
	version, _, err := m.Version()
	if errors.Is(err, migrate.ErrNilVersion) || (err == nil && version >= dropPasswordsVersion) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read migration version: %w", err)
	}
	rows, err := pool.Query(ctx, `SELECT name FROM users
		WHERE (deleted_at IS NULL AND login_type <> 'none' AND (oidc_issuer IS NULL OR oidc_subject IS NULL))
		   OR ((oidc_issuer IS NULL) <> (oidc_subject IS NULL))
		ORDER BY name`)
	if err != nil {
		return fmt.Errorf("check unlinked Users: %w", err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("check unlinked Users: %w", err)
	}
	if len(names) > 0 {
		return fmt.Errorf("migrate: %d User(s) are not linked to Polaris (%s); run orrery-polaris-migrate with the previous release first (docs/deploy.md)",
			len(names), strings.Join(names, ", "))
	}
	return nil
}

func migrateDSN(dsn string) string {
	switch {
	case strings.HasPrefix(dsn, "postgres://"):
		return "pgx5://" + strings.TrimPrefix(dsn, "postgres://")
	case strings.HasPrefix(dsn, "postgresql://"):
		return "pgx5://" + strings.TrimPrefix(dsn, "postgresql://")
	default:
		return dsn
	}
}
