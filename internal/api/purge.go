package api

import (
	"context"
	"time"
)

// purgeInterval is the periodic cleanup cadence (defaults.md, 10 minutes).
const purgeInterval = 10 * time.Minute

// purgeLockID guards the purge across replicas (ADR-0017).
const purgeLockID = 724002

// purgeExpired removes every row whose retention has passed: Idempotency Keys
// (api.md §1), used or expired Enrollment Packets, expired API Tokens and
// terminal admission requests, plus Execution Logs and Audit Records at the
// deployment retentions (ADR-0017). Deletes are bounded, so a backlog drains
// over several ticks.
func (s *Server) purgeExpired(ctx context.Context) {
	ticker := time.NewTicker(purgeInterval)
	defer ticker.Stop()
	for {
		s.purgeOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Server) purgeOnce(ctx context.Context) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Warn("purge: begin", "error", err)
		}
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck // released with the lock either way
	var locked bool
	if err := tx.QueryRow(ctx, "SELECT pg_try_advisory_xact_lock($1)", purgeLockID).Scan(&locked); err != nil || !locked {
		return
	}
	q := s.q.WithTx(tx)
	check := func(what string, err error) {
		if err != nil && ctx.Err() == nil {
			s.log.Warn("purge "+what, "error", err)
		}
	}
	check("User idempotency keys", q.PurgeUserIdempotencyKeys(ctx))
	check("token idempotency keys", q.PurgeTokenIdempotencyKeys(ctx))
	check("Sandbox idempotency keys", q.PurgeSandboxIdempotencyKeys(ctx))
	check("Workspace idempotency keys", q.PurgeWorkspaceIdempotencyKeys(ctx))
	check("Enrollment Packets", q.PurgeEnrollmentPackets(ctx))
	check("expired tokens", q.PurgeExpiredTokens(ctx))
	check("terminal admission requests", q.PurgeTerminalAdmissionRequests(ctx))
	if s.cfg.ExecLogRetention > 0 {
		check("Execution Logs", q.PurgeExecutionLogs(ctx, int64(s.cfg.ExecLogRetention/time.Second)))
	}
	if s.cfg.AuditRetention > 0 {
		check("Audit Records", q.PurgeAuditRecords(ctx, int64(s.cfg.AuditRetention/time.Second)))
	}
	if err := tx.Commit(ctx); err != nil && ctx.Err() == nil {
		s.log.Warn("purge: commit", "error", err)
	}
}
