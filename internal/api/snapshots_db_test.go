package api

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/zibyn/orrery/internal/registry"
	"github.com/zibyn/orrery/internal/store"
)

// An import deleted while its copy was in flight still ends ready, so purge
// can find its digest, but never becomes current (#250).
func TestCommitImportLeavesATombstonedVersionNotCurrent(t *testing.T) {
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
	q := store.New(pool)
	s := &Server{pool: pool, q: q, log: slog.New(slog.DiscardHandler)}

	snap, err := q.CreateSnapshot(ctx, store.CreateSnapshotParams{ID: uuid.New(), Name: "imp-" + uuid.NewString()[:8]})
	if err != nil {
		t.Fatal(err)
	}
	importing := func() uuid.UUID {
		v, err := q.CreateSnapshotVersion(ctx, store.CreateSnapshotVersionParams{
			ID: uuid.New(), SnapshotID: snap.ID, State: "building", Source: "import",
		})
		if err != nil {
			t.Fatal(err)
		}
		return v.ID
	}
	current := func() *uuid.UUID {
		got, err := q.GetSnapshot(ctx, snap.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got.CurrentVersionID
	}

	live := importing()
	if err := s.commitImport(ctx, live, snap.ID, "sha256:"+strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	if c := current(); c == nil || *c != live {
		t.Fatalf("current after a live import = %v, want %v", c, live)
	}

	deletedDigest := "sha256:" + strings.Repeat("b", 64)
	deleted := importing()
	if _, err := q.TombstoneSnapshotVersion(ctx, store.TombstoneSnapshotVersionParams{ID: deleted, SnapshotID: snap.ID}); err != nil {
		t.Fatal(err)
	}
	if err := s.commitImport(ctx, deleted, snap.ID, deletedDigest); err != nil {
		t.Fatal(err)
	}
	if c := current(); c == nil || *c != live {
		t.Errorf("current after a tombstoned import = %v, want %v unchanged", c, live)
	}
	var state string
	var digest *string
	var tombstoned bool
	if err := pool.QueryRow(ctx, `SELECT state, digest, deleted_at IS NOT NULL FROM snapshot_versions WHERE id = $1`,
		deleted).Scan(&state, &digest, &tombstoned); err != nil {
		t.Fatal(err)
	}
	if state != "ready" || digest == nil || *digest != deletedDigest || !tombstoned {
		t.Errorf("tombstoned import = state %s digest %v tombstoned %v, want ready, its digest, still tombstoned",
			state, digest, tombstoned)
	}
}

// purgeTestServer opens the test database, or skips without one.
func purgeTestServer(t *testing.T) (*Server, *store.Queries) {
	t.Helper()
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
	q := store.New(pool)
	return &Server{pool: pool, q: q, log: slog.New(slog.DiscardHandler)}, q
}

// readyVersion commits an import of digest into snap and returns its row.
func readyVersion(t *testing.T, s *Server, q *store.Queries, snap uuid.UUID, digest string) store.SnapshotVersion {
	t.Helper()
	ctx := context.Background()
	v, err := q.CreateSnapshotVersion(ctx, store.CreateSnapshotVersionParams{
		ID: uuid.New(), SnapshotID: snap, State: "building", Source: "import",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.commitImport(ctx, v.ID, snap, digest); err != nil {
		t.Fatal(err)
	}
	v, err = q.GetSnapshotVersionByID(ctx, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// A tombstoned version keeps its manifest and row while an import into the
// same Snapshot is in flight: the import has no digest yet and may be copying
// the same one (#251).
func TestSnapshotVersionGarbageWaitsForBuildingImport(t *testing.T) {
	s, q := purgeTestServer(t)
	ctx := context.Background()
	snap, err := q.CreateSnapshot(ctx, store.CreateSnapshotParams{ID: uuid.New(), Name: "gc-" + uuid.NewString()[:8]})
	if err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + strings.Repeat("c", 64)
	old := readyVersion(t, s, q, snap.ID, digest)
	readyVersion(t, s, q, snap.ID, "sha256:"+strings.Repeat("d", 64)) // takes current
	if _, err := q.TombstoneSnapshotVersion(ctx, store.TombstoneSnapshotVersionParams{ID: old.ID, SnapshotID: snap.ID}); err != nil {
		t.Fatal(err)
	}
	inflight, err := q.CreateSnapshotVersion(ctx, store.CreateSnapshotVersionParams{
		ID: uuid.New(), SnapshotID: snap.ID, State: "building", Source: "import",
	})
	if err != nil {
		t.Fatal(err)
	}
	repo := registry.Repo(snap.ID, platformName(snap))
	if removable, _, err := s.snapshotVersionGarbage(ctx, repo, old); err != nil || removable {
		t.Fatalf("garbage during an import = removable %v, err %v; want kept", removable, err)
	}
	if _, err := q.MarkSnapshotVersionFailed(ctx, store.MarkSnapshotVersionFailedParams{
		ID: inflight.ID, Cause: strPtr("import_failed"),
	}); err != nil {
		t.Fatal(err)
	}
	if removable, manifest, err := s.snapshotVersionGarbage(ctx, repo, old); err != nil || !removable || !manifest {
		t.Fatalf("garbage after the import = removable %v manifest %v, err %v; want both", removable, manifest, err)
	}
}

// A queued build Sandbox pins its base digest, so the version stays
// referenced until the build is done (#251).
func TestSnapshotReferencesCountUnfinishedBuildSandbox(t *testing.T) {
	s, q := purgeTestServer(t)
	ctx := context.Background()
	owner := uuid.New()
	if _, err := s.pool.Exec(ctx, `INSERT INTO users (id, name) VALUES ($1, $2)`, owner, "u-"+owner.String()); err != nil {
		t.Fatal(err)
	}
	snap, err := q.CreateSnapshot(ctx, store.CreateSnapshotParams{ID: uuid.New(), Name: "ref-" + uuid.NewString()[:8]})
	if err != nil {
		t.Fatal(err)
	}
	base := readyVersion(t, s, q, snap.ID, "sha256:"+strings.Repeat("e", 64))
	readyVersion(t, s, q, snap.ID, "sha256:"+strings.Repeat("f", 64)) // base is no longer current
	repo := registry.Repo(snap.ID, platformName(snap))
	build := uuid.New()
	if _, err := q.CreateBuildSandbox(ctx, store.CreateBuildSandboxParams{
		ID: build, OwnerID: owner, Name: "build-" + build.String(),
		SnapshotDigest: registry.Ref("registry.test", repo, *base.Digest), Cpu: 1, Memory: 1 << 30,
	}); err != nil {
		t.Fatal(err)
	}
	if removable, _, err := s.snapshotVersionGarbage(ctx, repo, base); err != nil || removable {
		t.Fatalf("garbage under a pending build = removable %v, err %v; want kept", removable, err)
	}
	if _, err := q.MarkBuildSandboxDone(ctx, build); err != nil {
		t.Fatal(err)
	}
	if removable, manifest, err := s.snapshotVersionGarbage(ctx, repo, base); err != nil || !removable || !manifest {
		t.Fatalf("garbage after the build = removable %v manifest %v, err %v; want both", removable, manifest, err)
	}
}
