package api

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/zibyn/stars-sandbox/internal/registry"
	"github.com/zibyn/stars-sandbox/internal/store"
)

// purgeInterval is the periodic cleanup cadence (defaults.md, 10 minutes).
const purgeInterval = 10 * time.Minute

// gcBatch bounds every sweep query so one tick's transaction and object
// deletions stay short; a backlog drains over several ticks (ADR-0012).
const gcBatch = 100

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
	now := time.Now()
	s.purgeGC(ctx, tx, q, now)
	if err := tx.Commit(ctx); err != nil && ctx.Err() == nil {
		s.log.Warn("purge: commit", "error", err)
	}
}

// purgeGC runs the ADR-0012 sweeps in one savepoint. A failed sweep must not
// abort the transaction: the next tick retries it, and the retention deletes
// above still commit.
func (s *Server) purgeGC(ctx context.Context, tx pgx.Tx, q *store.Queries, now time.Time) {
	if _, err := tx.Exec(ctx, "SAVEPOINT gc_sweep"); err != nil {
		s.logWarn(ctx, "start purge savepoint", err)
		return
	}
	err := func() error {
		// Deleted Sandboxes no longer retain versions, but their
		// sandbox_resources rows outlive them; release those first or the
		// resource_versions delete hits the RESTRICT foreign key.
		if _, err := q.DeleteDeletedSandboxResources(ctx); err != nil {
			return fmt.Errorf("release deleted Sandbox references: %w", err)
		}
		if _, err := q.DeleteExpiredUploads(ctx); err != nil {
			return fmt.Errorf("expire uploads: %w", err)
		}
		if err := s.purgeResourceVersions(ctx, q, now); err != nil {
			return err
		}
		if err := s.purgeCASObjects(ctx, q, now); err != nil {
			return err
		}
		if err := s.purgeResourceTombstones(ctx, q); err != nil {
			return err
		}
		return s.purgeSnapshotTombstones(ctx, q)
	}()
	if err != nil {
		s.logWarn(ctx, "purge garbage collection", err)
		if _, rbErr := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT gc_sweep"); rbErr != nil {
			s.logWarn(ctx, "roll back purge savepoint", rbErr)
		}
		return
	}
	if _, err := tx.Exec(ctx, "RELEASE SAVEPOINT gc_sweep"); err != nil {
		s.logWarn(ctx, "release purge savepoint", err)
	}
}

// purgeResourceVersions expires non-current versions with no Sandbox
// reference. The manifest object goes before the rows, so a retry after a
// partial failure is safe: deleting a missing manifest is a no-op.
func (s *Server) purgeResourceVersions(ctx context.Context, q *store.Queries, now time.Time) error {
	if s.cfg.ResourceVersionTTL <= 0 || s.objects == nil {
		return nil
	}
	cutoff := pgtype.Timestamptz{Time: now.Add(-s.cfg.ResourceVersionTTL), Valid: true}
	for {
		rows, err := q.ListExpiredResourceVersions(ctx, store.ListExpiredResourceVersionsParams{
			Cutoff: cutoff, Limit: gcBatch,
		})
		if err != nil {
			return fmt.Errorf("list expired ResourceVersions: %w", err)
		}
		progress := 0
		for _, row := range rows {
			if !resourceVersionExpired(now, s.cfg.ResourceVersionTTL, resourceVersionRetention{
				State: row.State, CreatedAt: row.CreatedAt.Time, Current: row.Current,
			}) {
				continue
			}
			if row.Address != nil {
				// Manifests are content-addressed: an identical version that
				// is still alive keeps the object.
				others, err := q.CountResourceManifestReferences(ctx, store.CountResourceManifestReferencesParams{
					OwnerID: row.OwnerID, Address: row.Address, ID: row.ID,
				})
				if err != nil {
					s.logWarn(ctx, "count manifest references", err)
					continue
				}
				if others == 0 {
					if err := s.objects.DeleteManifest(ctx, row.OwnerID.String(), *row.Address); err != nil {
						s.logWarn(ctx, "delete ResourceVersion manifest", err)
						continue
					}
				}
			}
			if err := q.DeleteResourceVersionObjects(ctx, row.ID); err != nil {
				s.logWarn(ctx, "delete ResourceVersion objects", err)
				continue
			}
			if err := q.DeleteResourceVersion(ctx, row.ID); err != nil {
				s.logWarn(ctx, "delete ResourceVersion", err)
				continue
			}
			progress++
		}
		if len(rows) < gcBatch || progress == 0 {
			return nil
		}
	}
}

// purgeCASObjects removes unreferenced objects past the grace window, object
// first and then the row (ADR-0021). The owner-scoped reference query is what
// keeps one owner's version from protecting another owner's identical hash.
func (s *Server) purgeCASObjects(ctx context.Context, q *store.Queries, now time.Time) error {
	if s.cfg.CASGrace <= 0 || s.objects == nil {
		return nil
	}
	cutoff := pgtype.Timestamptz{Time: now.Add(-s.cfg.CASGrace), Valid: true}
	for {
		rows, err := q.ListSweepableCASObjects(ctx, store.ListSweepableCASObjectsParams{
			Cutoff: cutoff, Limit: gcBatch,
		})
		if err != nil {
			return fmt.Errorf("list sweepable CAS objects: %w", err)
		}
		progress := 0
		for _, row := range rows {
			key := casObjectKey{owner: row.OwnerID, sha256: row.Sha256}
			if !casSweepable(now, s.cfg.CASGrace, key, row.CreatedAt.Time, nil) {
				continue
			}
			if err := s.objects.DeleteCASObject(ctx, key.owner.String(), key.sha256); err != nil {
				s.logWarn(ctx, "delete CAS object", err)
				continue
			}
			if err := q.DeleteCASObject(ctx, store.DeleteCASObjectParams{
				OwnerID: key.owner, Sha256: key.sha256,
			}); err != nil {
				s.logWarn(ctx, "delete CAS row", err)
				continue
			}
			progress++
		}
		if len(rows) < gcBatch || progress == 0 {
			return nil
		}
	}
}

// purgeResourceTombstones physically removes a deleted Resource's versions
// once no Sandbox references them, then the row. Versions that are still
// referenced stay (schema.md: the reference retains them).
func (s *Server) purgeResourceTombstones(ctx context.Context, q *store.Queries) error {
	if s.objects == nil {
		return nil
	}
	resources, err := q.ListDeletedResources(ctx, gcBatch)
	if err != nil {
		return fmt.Errorf("list deleted Resources: %w", err)
	}
	for _, resource := range resources {
		// Resource-level tombstone: the current pointer no longer retains
		// anything once the Resource is gone.
		if err := q.ClearResourceCurrent(ctx, resource.ID); err != nil {
			return fmt.Errorf("clear Resource current: %w", err)
		}
		versions, err := q.ListResourceVersionsForCleanup(ctx, store.ListResourceVersionsForCleanupParams{
			ResourceID: resource.ID, Limit: gcBatch,
		})
		if err != nil {
			return fmt.Errorf("list Resource versions for cleanup: %w", err)
		}
		for _, version := range versions {
			if version.Address != nil {
				others, err := q.CountResourceManifestReferences(ctx, store.CountResourceManifestReferencesParams{
					OwnerID: version.OwnerID, Address: version.Address, ID: version.ID,
				})
				if err != nil {
					return fmt.Errorf("count tombstoned manifest references: %w", err)
				}
				if others == 0 {
					if err := s.objects.DeleteManifest(ctx, version.OwnerID.String(), *version.Address); err != nil {
						s.logWarn(ctx, "delete tombstoned manifest", err)
						continue
					}
				}
			}
			if err := q.DeleteResourceVersionObjects(ctx, version.ID); err != nil {
				return fmt.Errorf("delete tombstoned version objects: %w", err)
			}
			if err := q.DeleteResourceVersion(ctx, version.ID); err != nil {
				return fmt.Errorf("delete tombstoned version: %w", err)
			}
		}
		remaining, err := q.CountResourceVersions(ctx, resource.ID)
		if err != nil {
			return fmt.Errorf("count Resource versions: %w", err)
		}
		if remaining > 0 {
			continue
		}
		uploads, err := q.CountResourceUploads(ctx, resource.ID)
		if err != nil {
			return fmt.Errorf("count Resource uploads: %w", err)
		}
		if uploads > 0 {
			continue // expired uploads are removed by the sweep above
		}
		if err := q.DeleteDeletedResource(ctx, resource.ID); err != nil {
			return fmt.Errorf("delete Resource row: %w", err)
		}
	}
	return nil
}

// snapshotVersionCleanable reports whether a tombstoned Snapshot's version may
// be removed. A building version still belongs to the build sweep, which
// releases its Sandbox and Allocation; a later sweep removes it once terminal.
func snapshotVersionCleanable(state string) bool {
	return state == string(SnapshotVersionReady) ||
		state == string(SnapshotVersionFailed) ||
		state == string(SnapshotVersionCancelled)
}

// purgeSnapshotTombstones removes each version of a deleted Snapshot once the
// last Sandbox reference is gone: the registry manifest first, then the row,
// and finally the Snapshot. Blob GC stays an operator procedure (ADR-0014).
func (s *Server) purgeSnapshotTombstones(ctx context.Context, q *store.Queries) error {
	if s.cfg.RegistryPushAddr == "" {
		return nil
	}
	snapshots, err := q.ListDeletedSnapshots(ctx, gcBatch)
	if err != nil {
		return fmt.Errorf("list deleted Snapshots: %w", err)
	}
	for _, snap := range snapshots {
		if err := q.ClearSnapshotCurrent(ctx, snap.ID); err != nil {
			return fmt.Errorf("clear Snapshot current: %w", err)
		}
		repo := registry.Repo(snap.ID, platformName(snap))
		versions, err := q.ListSnapshotVersionsForCleanup(ctx, store.ListSnapshotVersionsForCleanupParams{
			SnapshotID: snap.ID, Limit: gcBatch,
		})
		if err != nil {
			return fmt.Errorf("list Snapshot versions for cleanup: %w", err)
		}
		for _, version := range versions {
			if !snapshotVersionCleanable(version.State) {
				// The build sweep still owns this version, its Sandbox and its
				// Exec; deleting it would strand the allocation (ADR-0014).
				continue
			}
			if version.Digest != nil {
				ref := repo + "@" + *version.Digest
				count, err := q.CountSandboxSnapshotReferences(ctx, ref)
				if err != nil {
					return fmt.Errorf("count Snapshot references: %w", err)
				}
				if count > 0 {
					continue
				}
				// Another version of this Snapshot may share the digest; the
				// manifest is content-addressed and must outlive it.
				shared, err := q.CountSnapshotVersionDigestReferences(ctx,
					store.CountSnapshotVersionDigestReferencesParams{
						SnapshotID: snap.ID, Digest: version.Digest, ID: version.ID,
					})
				if err != nil {
					return fmt.Errorf("count Snapshot digest references: %w", err)
				}
				if shared == 0 {
					if err := registry.DeleteManifest(ctx, registry.Ref(s.cfg.RegistryPushAddr, repo, *version.Digest),
						s.cfg.RegistryCredentialDoc); err != nil {
						s.logWarn(ctx, "delete Snapshot manifest", err)
						continue
					}
				}
			}
			if err := q.DeleteSnapshotVersion(ctx, store.DeleteSnapshotVersionParams{
				ID: version.ID, SnapshotID: snap.ID,
			}); err != nil {
				return fmt.Errorf("delete Snapshot version: %w", err)
			}
		}
		remaining, err := q.CountSnapshotVersions(ctx, snap.ID)
		if err != nil {
			return fmt.Errorf("count Snapshot versions: %w", err)
		}
		if remaining > 0 {
			continue
		}
		if err := q.DeleteDeletedSnapshot(ctx, snap.ID); err != nil {
			return fmt.Errorf("delete Snapshot row: %w", err)
		}
	}
	return nil
}

// logWarn reports a sweep error unless the process is shutting down.
func (s *Server) logWarn(ctx context.Context, what string, err error) {
	if ctx.Err() == nil {
		s.log.Warn(what, "error", err)
	}
}
