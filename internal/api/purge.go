package api

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// purgeInterval is the periodic cleanup cadence (defaults.md, 10 minutes).
const purgeInterval = 10 * time.Minute

// purgeLockID guards the purge across replicas (ADR-0017).
const purgeLockID = 724002

// resourceVersionRetention is the decision input for ResourceVersion expiry
// (ADR-0012): a version lives while it is current or a Sandbox still names
// it, and a pending publish is in flight rather than stale.
type resourceVersionRetention struct {
	State      string
	CreatedAt  time.Time
	Current    bool
	Referenced bool
}

// resourceVersionExpired reports whether a version has outlived the deployment
// TTL with no reason to stay. A zero TTL keeps versions forever (defaults.md:
// purge configuration 0 means "do not delete").
func resourceVersionExpired(now time.Time, ttl time.Duration, r resourceVersionRetention) bool {
	if ttl <= 0 || r.Current || r.Referenced || r.State == "pending" {
		return false
	}
	return r.CreatedAt.Before(now.Add(-ttl))
}

// casObjectKey is the owner-scoped CAS address (ADR-0021). References must be
// matched per owner: knowing a hash must not protect another owner's object.
type casObjectKey struct {
	owner  uuid.UUID
	sha256 string
}

// casSweepable reports whether an unreferenced CAS object has passed the
// grace window. A zero grace disables the sweep (defaults.md).
func casSweepable(now time.Time, grace time.Duration, key casObjectKey, createdAt time.Time, referenced map[casObjectKey]bool) bool {
	if grace <= 0 || referenced[key] {
		return false
	}
	return createdAt.Before(now.Add(-grace))
}

// purgeExpired removes every row whose retention has passed: Idempotency Keys
// (api.md §1), used or expired Enrollment Packets, expired API Tokens and
// terminal admission requests, plus Execution Logs and Audit Records at the
// deployment retentions (ADR-0017). Deletes are bounded, so a backlog drains
// over several ticks.
func (s *Server) purgeExpired(ctx context.Context) {
	interval := s.cfg.PurgeInterval
	if interval <= 0 {
		interval = purgeInterval
	}
	ticker := time.NewTicker(interval)
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
	check("Resource idempotency keys", q.PurgeResourceIdempotencyKeys(ctx))
	check("ResourceVersion idempotency keys", q.PurgeResourceVersionIdempotencyKeys(ctx))
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
