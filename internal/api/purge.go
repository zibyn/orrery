package api

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/zibyn/orrery/internal/registry"
	"github.com/zibyn/orrery/internal/store"
)

// purgeInterval is the periodic cleanup cadence (defaults.md, 10 minutes).
const purgeInterval = 10 * time.Minute

// gcBatch bounds every sweep query so one tick's transactions and object
// deletions stay short; a backlog drains over several ticks (ADR-0012).
const gcBatch = 100

// purgeLockID guards the purge across replicas (ADR-0017).
const purgeLockID = 724002

// errPurgeLocked reports that another replica holds the purge lock. The batch
// is skipped and a later tick retries it.
var errPurgeLocked = errors.New("purge: another replica holds the lock")

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
	s.purgeExpiredRows(ctx)
	s.purgeGC(ctx, time.Now())
}

// purgeExpiredRows runs the plain retention deletes in one short transaction.
// They touch no external system, so the advisory lock only has to cover them.
func (s *Server) purgeExpiredRows(ctx context.Context) {
	err := s.purgeTx(ctx, func(q *store.Queries) error {
		checks := []struct {
			what string
			run  func() error
		}{
			{"User idempotency keys", func() error { return q.PurgeUserIdempotencyKeys(ctx) }},
			{"token idempotency keys", func() error { return q.PurgeTokenIdempotencyKeys(ctx) }},
			{"Sandbox idempotency keys", func() error { return q.PurgeSandboxIdempotencyKeys(ctx) }},
			{"Workspace idempotency keys", func() error { return q.PurgeWorkspaceIdempotencyKeys(ctx) }},
			{"Snapshot idempotency keys", func() error { return q.PurgeSnapshotIdempotencyKeys(ctx) }},
			{"Resource idempotency keys", func() error { return q.PurgeResourceIdempotencyKeys(ctx) }},
			{"ResourceVersion idempotency keys", func() error { return q.PurgeResourceVersionIdempotencyKeys(ctx) }},
			{"Enrollment Packets", func() error { return q.PurgeEnrollmentPackets(ctx) }},
			{"expired tokens", func() error { return q.PurgeExpiredTokens(ctx) }},
			{"terminal admission requests", func() error { return q.PurgeTerminalAdmissionRequests(ctx) }},
			{"Execution Logs", func() error {
				if s.cfg.ExecLogRetention <= 0 {
					return nil
				}
				return q.PurgeExecutionLogs(ctx, int64(s.cfg.ExecLogRetention/time.Second))
			}},
			{"Audit Records", func() error {
				if s.cfg.AuditRetention <= 0 {
					return nil
				}
				return q.PurgeAuditRecords(ctx, int64(s.cfg.AuditRetention/time.Second))
			}},
		}
		for _, check := range checks {
			if err := check.run(); err != nil {
				return fmt.Errorf("%s: %w", check.what, err)
			}
		}
		return nil
	})
	if errors.Is(err, errPurgeLocked) {
		return
	}
	if err != nil {
		s.logWarn(ctx, "purge expired rows", err)
	}
}

// purgeGC runs the ADR-0012 sweeps. Every step owns its short transactions and
// deletes external objects before rows, so a slow S3 or registry request never
// pins a database transaction or the purge lock (issue #121). A failed step
// does not block the others; the next tick retries it.
func (s *Server) purgeGC(ctx context.Context, now time.Time) {
	steps := []struct {
		what string
		run  func(context.Context) error
	}{
		{"release deleted Sandbox references", s.purgeDeletedSandboxResources},
		{"expire uploads", s.purgeExpiredUploads},
		{"ResourceVersions", func(ctx context.Context) error { return s.purgeResourceVersions(ctx, now) }},
		{"CAS objects", func(ctx context.Context) error { return s.purgeCASObjects(ctx, now) }},
		{"Resource tombstones", s.purgeResourceTombstones},
		{"Snapshot tombstones", s.purgeSnapshotTombstones},
	}
	for _, step := range steps {
		err := step.run(ctx)
		if errors.Is(err, errPurgeLocked) {
			return // another replica is sweeping; retry next tick
		}
		if err != nil {
			s.logWarn(ctx, "purge "+step.what, err)
		}
	}
}

// purgeTx runs fn in one short transaction holding the purge advisory lock.
// External calls stay outside: the object store and the registry are not
// transactional, so including them would pin a connection for the length of a
// slow delete (issue #121).
func (s *Server) purgeTx(ctx context.Context, fn func(q *store.Queries) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // released with the lock either way
	var locked bool
	if err := tx.QueryRow(ctx, "SELECT pg_try_advisory_xact_lock($1)", purgeLockID).Scan(&locked); err != nil {
		return fmt.Errorf("acquire purge lock: %w", err)
	}
	if !locked {
		return errPurgeLocked
	}
	if err := fn(s.q.WithTx(tx)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// deleteObjectsThenRows deletes every external object before any row. The
// object store is not transactional, so deleting rows first would orphan an
// object forever if its delete then failed. Object deletes are idempotent, so
// a failure leaves the rows for the next tick to retry from the start.
func deleteObjectsThenRows[T any](ctx context.Context, items []T,
	deleteObject func(context.Context, T) error,
	deleteRows func(context.Context) error,
) error {
	for _, item := range items {
		if err := deleteObject(ctx, item); err != nil {
			return err
		}
	}
	return deleteRows(ctx)
}

// purgeDeletedSandboxResources releases the Resource references of deleted
// Sandboxes. Their sandbox_resources rows outlive the Sandbox, and the
// resource_versions delete below hits the RESTRICT foreign key until they go.
func (s *Server) purgeDeletedSandboxResources(ctx context.Context) error {
	return s.purgeTx(ctx, func(q *store.Queries) error {
		_, err := q.DeleteDeletedSandboxResources(ctx)
		return err
	})
}

func (s *Server) purgeExpiredUploads(ctx context.Context) error {
	return s.purgeTx(ctx, func(q *store.Queries) error {
		_, err := q.DeleteExpiredUploads(ctx)
		return err
	})
}

// purgeResourceVersions expires non-current versions with no Sandbox
// reference. The manifests go before the rows; a retry after a partial
// failure re-deletes objects that are already gone, which is a no-op.
func (s *Server) purgeResourceVersions(ctx context.Context, now time.Time) error {
	if s.cfg.ResourceVersionTTL <= 0 || s.objects == nil {
		return nil
	}
	cutoff := pgtype.Timestamptz{Time: now.Add(-s.cfg.ResourceVersionTTL), Valid: true}
	for {
		rows, err := s.q.ListExpiredResourceVersions(ctx, store.ListExpiredResourceVersionsParams{
			Cutoff: cutoff, Limit: gcBatch,
		})
		if err != nil {
			return fmt.Errorf("list expired ResourceVersions: %w", err)
		}
		expired := make([]store.ListExpiredResourceVersionsRow, 0, len(rows))
		for _, row := range rows {
			if resourceVersionExpired(now, s.cfg.ResourceVersionTTL, resourceVersionRetention{
				State: row.State, CreatedAt: row.CreatedAt.Time, Current: row.Current,
			}) {
				expired = append(expired, row)
			}
		}
		if len(expired) == 0 {
			return nil
		}
		err = deleteObjectsThenRows(ctx, expired,
			func(ctx context.Context, row store.ListExpiredResourceVersionsRow) error {
				if row.Address == nil {
					return nil
				}
				// Manifests are content-addressed: an identical version that
				// is still alive keeps the object.
				others, err := s.q.CountResourceManifestReferences(ctx, store.CountResourceManifestReferencesParams{
					OwnerID: row.OwnerID, Address: row.Address, ID: row.ID,
				})
				if err != nil {
					return fmt.Errorf("count manifest references: %w", err)
				}
				if others > 0 {
					return nil
				}
				if err := s.objects.DeleteManifest(ctx, row.OwnerID.String(), *row.Address); err != nil {
					return fmt.Errorf("delete ResourceVersion manifest: %w", err)
				}
				return nil
			},
			func(ctx context.Context) error {
				return s.purgeTx(ctx, func(q *store.Queries) error {
					for _, row := range expired {
						if err := q.DeleteResourceVersionObjects(ctx, row.ID); err != nil {
							return fmt.Errorf("delete ResourceVersion objects: %w", err)
						}
						if err := q.DeleteResourceVersion(ctx, row.ID); err != nil {
							return fmt.Errorf("delete ResourceVersion: %w", err)
						}
					}
					return nil
				})
			})
		if err != nil {
			return err
		}
		if len(rows) < gcBatch {
			return nil
		}
	}
}

// purgeCASObjects removes unreferenced objects past the grace window, object
// first and then the row (ADR-0021). The owner-scoped reference query is what
// keeps one owner's version from protecting another owner's identical hash.
func (s *Server) purgeCASObjects(ctx context.Context, now time.Time) error {
	if s.cfg.CASGrace <= 0 || s.objects == nil {
		return nil
	}
	cutoff := pgtype.Timestamptz{Time: now.Add(-s.cfg.CASGrace), Valid: true}
	for {
		rows, err := s.q.ListSweepableCASObjects(ctx, store.ListSweepableCASObjectsParams{
			Cutoff: cutoff, Limit: gcBatch,
		})
		if err != nil {
			return fmt.Errorf("list sweepable CAS objects: %w", err)
		}
		sweepable := make([]store.ListSweepableCASObjectsRow, 0, len(rows))
		for _, row := range rows {
			key := casObjectKey{owner: row.OwnerID, sha256: row.Sha256}
			if casSweepable(now, s.cfg.CASGrace, key, row.CreatedAt.Time, nil) {
				sweepable = append(sweepable, row)
			}
		}
		if len(sweepable) == 0 {
			return nil
		}
		err = deleteObjectsThenRows(ctx, sweepable,
			func(ctx context.Context, row store.ListSweepableCASObjectsRow) error {
				if err := s.objects.DeleteCASObject(ctx, row.OwnerID.String(), row.Sha256); err != nil {
					return fmt.Errorf("delete CAS object: %w", err)
				}
				return nil
			},
			func(ctx context.Context) error {
				return s.purgeTx(ctx, func(q *store.Queries) error {
					for _, row := range sweepable {
						if err := q.DeleteCASObject(ctx, store.DeleteCASObjectParams{
							OwnerID: row.OwnerID, Sha256: row.Sha256,
						}); err != nil {
							return fmt.Errorf("delete CAS row: %w", err)
						}
					}
					return nil
				})
			})
		if err != nil {
			return err
		}
		if len(rows) < gcBatch {
			return nil
		}
	}
}

// purgeResourceTombstones physically removes a deleted Resource's versions
// once no Sandbox references them, then the row. Versions that are still
// referenced stay (schema.md: the reference retains them).
func (s *Server) purgeResourceTombstones(ctx context.Context) error {
	if s.objects == nil {
		return nil
	}
	resources, err := s.q.ListDeletedResources(ctx, gcBatch)
	if err != nil {
		return fmt.Errorf("list deleted Resources: %w", err)
	}
	for _, resource := range resources {
		if err := s.purgeResourceTombstone(ctx, resource.ID); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) purgeResourceTombstone(ctx context.Context, resourceID uuid.UUID) error {
	versions, err := s.q.ListResourceVersionsForCleanup(ctx, store.ListResourceVersionsForCleanupParams{
		ResourceID: resourceID, Limit: gcBatch,
	})
	if err != nil {
		return fmt.Errorf("list Resource versions for cleanup: %w", err)
	}
	err = deleteObjectsThenRows(ctx, versions,
		func(ctx context.Context, version store.ListResourceVersionsForCleanupRow) error {
			if version.Address == nil {
				return nil
			}
			others, err := s.q.CountResourceManifestReferences(ctx, store.CountResourceManifestReferencesParams{
				OwnerID: version.OwnerID, Address: version.Address, ID: version.ID,
			})
			if err != nil {
				return fmt.Errorf("count tombstoned manifest references: %w", err)
			}
			if others > 0 {
				return nil
			}
			if err := s.objects.DeleteManifest(ctx, version.OwnerID.String(), *version.Address); err != nil {
				return fmt.Errorf("delete tombstoned manifest: %w", err)
			}
			return nil
		},
		func(ctx context.Context) error {
			return s.purgeTx(ctx, func(q *store.Queries) error {
				// Resource-level tombstone: the current pointer no longer
				// retains anything once the Resource is gone.
				if err := q.ClearResourceCurrent(ctx, resourceID); err != nil {
					return fmt.Errorf("clear Resource current: %w", err)
				}
				for _, version := range versions {
					if err := q.DeleteResourceVersionObjects(ctx, version.ID); err != nil {
						return fmt.Errorf("delete tombstoned version objects: %w", err)
					}
					if err := q.DeleteResourceVersion(ctx, version.ID); err != nil {
						return fmt.Errorf("delete tombstoned version: %w", err)
					}
				}
				return nil
			})
		})
	if err != nil {
		return err
	}
	remaining, err := s.q.CountResourceVersions(ctx, resourceID)
	if err != nil {
		return fmt.Errorf("count Resource versions: %w", err)
	}
	if remaining > 0 {
		return nil
	}
	uploads, err := s.q.CountResourceUploads(ctx, resourceID)
	if err != nil {
		return fmt.Errorf("count Resource uploads: %w", err)
	}
	if uploads > 0 {
		return nil // expired uploads are removed by the sweep above
	}
	return s.purgeTx(ctx, func(q *store.Queries) error {
		if err := q.DeleteDeletedResource(ctx, resourceID); err != nil {
			return fmt.Errorf("delete Resource row: %w", err)
		}
		return nil
	})
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
func (s *Server) purgeSnapshotTombstones(ctx context.Context) error {
	if s.cfg.RegistryPushAddr == "" {
		return nil
	}
	snapshots, err := s.q.ListDeletedSnapshots(ctx, gcBatch)
	if err != nil {
		return fmt.Errorf("list deleted Snapshots: %w", err)
	}
	for _, snap := range snapshots {
		if err := s.purgeSnapshotTombstone(ctx, snap); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) purgeSnapshotTombstone(ctx context.Context, snap store.Snapshot) error {
	repo := registry.Repo(snap.ID, platformName(snap))
	versions, err := s.q.ListSnapshotVersionsForCleanup(ctx, store.ListSnapshotVersionsForCleanupParams{
		SnapshotID: snap.ID, Limit: gcBatch,
	})
	if err != nil {
		return fmt.Errorf("list Snapshot versions for cleanup: %w", err)
	}
	// A version a Sandbox still references keeps both its row and its
	// manifest (schema.md: the reference retains it). An unreferenced version
	// always loses its row; the manifest is shared content, so an identical
	// digest another version names keeps the object.
	var deletable, manifests []store.SnapshotVersion
	for _, version := range versions {
		if !snapshotVersionCleanable(version.State) {
			continue
		}
		if version.Digest != nil {
			ref := repo + "@" + *version.Digest
			count, err := s.q.CountSandboxSnapshotReferences(ctx, ref)
			if err != nil {
				return fmt.Errorf("count Snapshot references: %w", err)
			}
			if count > 0 {
				continue
			}
			shared, err := s.q.CountSnapshotVersionDigestReferences(ctx,
				store.CountSnapshotVersionDigestReferencesParams{
					SnapshotID: snap.ID, Digest: version.Digest, ID: version.ID,
				})
			if err != nil {
				return fmt.Errorf("count Snapshot digest references: %w", err)
			}
			if shared == 0 {
				manifests = append(manifests, version)
			}
		}
		deletable = append(deletable, version)
	}
	err = deleteObjectsThenRows(ctx, manifests,
		func(ctx context.Context, version store.SnapshotVersion) error {
			if err := registry.DeleteManifest(ctx, registry.Ref(s.cfg.RegistryPushAddr, repo, *version.Digest),
				s.cfg.RegistryCredentialDoc); err != nil {
				return fmt.Errorf("delete Snapshot manifest: %w", err)
			}
			return nil
		},
		func(ctx context.Context) error {
			return s.purgeTx(ctx, func(q *store.Queries) error {
				if err := q.ClearSnapshotCurrent(ctx, snap.ID); err != nil {
					return fmt.Errorf("clear Snapshot current: %w", err)
				}
				for _, version := range deletable {
					if err := q.DeleteSnapshotVersion(ctx, store.DeleteSnapshotVersionParams{
						ID: version.ID, SnapshotID: snap.ID,
					}); err != nil {
						return fmt.Errorf("delete Snapshot version: %w", err)
					}
				}
				return nil
			})
		})
	if err != nil {
		return err
	}
	remaining, err := s.q.CountSnapshotVersions(ctx, snap.ID)
	if err != nil {
		return fmt.Errorf("count Snapshot versions: %w", err)
	}
	if remaining > 0 {
		return nil
	}
	return s.purgeTx(ctx, func(q *store.Queries) error {
		if err := q.DeleteDeletedSnapshot(ctx, snap.ID); err != nil {
			return fmt.Errorf("delete Snapshot row: %w", err)
		}
		return nil
	})
}

// logWarn reports a sweep error unless the process is shutting down.
func (s *Server) logWarn(ctx context.Context, what string, err error) {
	if ctx.Err() == nil {
		s.log.Warn(what, "error", err)
	}
}
