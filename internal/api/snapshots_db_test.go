package api

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

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
