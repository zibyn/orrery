package api

import (
	"context"
	"log/slog"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/zibyn/orrery/internal/store"
)

// The mutation has committed by the time auditAs runs, so a client that
// disconnected (canceling the request context) must not lose the record.
func TestAuditSurvivesCanceledRequest(t *testing.T) {
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

	reqCtx, cancel := context.WithCancel(context.WithValue(ctx, correlationKey{}, "corr-1"))
	cancel()
	target := uuid.NewString()
	s.auditAs(reqCtx, nil, nil, "test.audit", "test", target, nil)

	var correlation string
	if err := pool.QueryRow(ctx,
		`SELECT correlation_id FROM audit_records WHERE target_type = 'test' AND target_id = $1`, target,
	).Scan(&correlation); err != nil {
		t.Fatalf("audit record missing after canceled request: %v", err)
	}
	if correlation != "corr-1" {
		t.Fatalf("correlation_id = %q, want corr-1", correlation)
	}
}
