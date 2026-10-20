package api

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
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
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migrate: %w", err)
	}
	return tx.Commit(ctx)
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
