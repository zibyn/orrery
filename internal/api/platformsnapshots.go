package api

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/zibyn/stars-sandbox/internal/registry"
	"github.com/zibyn/stars-sandbox/internal/store"
)

// importPlatformSnapshots copies STARS_PLATFORM_SNAPSHOTS entries that are
// not already in the registry (defaults.md, ADR-0021). It runs in the
// background: a failing import must not block startup, and a Sandbox that
// needs the missing Snapshot fails at create time.
func (s *Server) importPlatformSnapshots(ctx context.Context) {
	if len(s.cfg.PlatformSnapshots) == 0 || s.cfg.RegistryPushAddr == "" {
		if len(s.cfg.PlatformSnapshots) > 0 {
			s.log.Warn("platform Snapshots configured without a registry; skipping import")
		}
		return
	}
	for _, configured := range s.cfg.PlatformSnapshots {
		if ctx.Err() != nil {
			return
		}
		if err := s.importPlatformSnapshot(ctx, configured); err != nil {
			s.log.Error("import platform Snapshot", "name", configured.Name, "ref", configured.Ref, "error", err)
		}
	}
}

func (s *Server) importPlatformSnapshot(ctx context.Context, configured PlatformSnapshot) error {
	source, err := registry.ParseSourceRef(configured.Ref)
	if err != nil {
		return err
	}
	digest := source.DigestStr()
	// Idempotent across restarts: a ready version already carrying this
	// digest is the import.
	_, err = s.q.GetReadyPlatformSnapshotVersionByDigest(ctx, store.GetReadyPlatformSnapshotVersionByDigestParams{
		Name: configured.Name, Digest: &digest,
	})
	if err == nil {
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	snap, err := s.q.GetPlatformSnapshotByName(ctx, configured.Name)
	if errors.Is(err, pgx.ErrNoRows) {
		id, idErr := uuid.NewV7()
		if idErr != nil {
			return idErr
		}
		snap, err = s.q.CreateSnapshot(ctx, store.CreateSnapshotParams{ID: id, Name: configured.Name})
		if err != nil {
			if uniqueViolation(err) != "snapshots_owner_name" {
				return err
			}
			// Another instance won the race; use its row.
			snap, err = s.q.GetPlatformSnapshotByName(ctx, configured.Name)
		}
	}
	if err != nil {
		return err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return err
	}
	version, err := s.q.CreateSnapshotVersion(ctx, store.CreateSnapshotVersionParams{
		ID: id, SnapshotID: snap.ID, State: string(SnapshotVersionBuilding), Source: "import",
	})
	if err != nil {
		return err
	}
	// runImport is synchronous here; the caller is already a background
	// goroutine, and the platform list is small.
	s.runImport(version.ID, snap.ID, registry.Repo(snap.ID, configured.Name), source, nil)
	return nil
}
