package api

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

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

// A quota change that commits while a PATCH is in flight must survive it:
// UpdateUser writes every column back, so the PATCH has to read under the
// row lock rather than from a stale snapshot.
func TestUpdateUserKeepsConcurrentQuotaChange(t *testing.T) {
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
	id := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, name) VALUES ($1, $2)`,
		id, "u-"+id.String()); err != nil {
		t.Fatal(err)
	}

	quota, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer quota.Rollback(ctx) //nolint:errcheck
	if _, err := quota.Exec(ctx, `UPDATE users SET gpu_quota = 7 WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}

	done := make(chan int)
	go func() {
		req := httptest.NewRequest(http.MethodPatch, "/", strings.NewReader(`{"name":"renamed-`+id.String()+`"}`))
		req.SetPathValue("id", id.String())
		rec := httptest.NewRecorder()
		s.handleUpdateUser(rec, req, nil)
		done <- rec.Code
	}()
	// Give the PATCH time to reach the row the quota transaction holds.
	time.Sleep(300 * time.Millisecond)
	if err := quota.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if code := <-done; code != http.StatusOK {
		t.Fatalf("PATCH status = %d", code)
	}

	var gpus int32
	var name string
	if err := pool.QueryRow(ctx, `SELECT gpu_quota, name FROM users WHERE id = $1`, id).Scan(&gpus, &name); err != nil {
		t.Fatal(err)
	}
	if gpus != 7 || name != "renamed-"+id.String() {
		t.Fatalf("gpu_quota = %d, name = %q; want 7 and the new name", gpus, name)
	}
}
