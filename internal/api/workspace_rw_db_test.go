package api

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/zibyn/orrery/internal/store"
)

// #220: the single-writer rule is a partial unique index on sandboxes. At most
// one Sandbox in pending, provisioning, ready, stopping or deleting mounts a
// Workspace read-write.
func TestWorkspaceSingleWriter(t *testing.T) {
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
	if err := Migrate(ctx, pool, dsn, ""); err != nil {
		t.Fatal(err)
	}
	q := store.New(pool)

	t.Run("second rw mount conflicts, ro mounts do not", func(t *testing.T) {
		f := newWorkspaceFixture(t, ctx, pool)
		if err := f.mount(f.sandbox("ready"), "rw"); err != nil {
			t.Fatal(err)
		}
		if got := uniqueViolation(f.mount(f.sandbox("pending"), "rw")); got != conflict {
			t.Fatalf("second rw mount violation = %q, want %q", got, conflict)
		}
		for range 2 {
			if err := f.mount(f.sandbox("pending"), "ro"); err != nil {
				t.Fatalf("ro mount: %v", err)
			}
		}
	})

	t.Run("stop releases, start re-acquires", func(t *testing.T) {
		f := newWorkspaceFixture(t, ctx, pool)
		a := f.sandbox("stopping")
		if err := f.mount(a, "rw"); err != nil {
			t.Fatal(err)
		}
		f.set(a, "desired_state = 'stopped'")
		if _, err := q.SetSandboxStopped(ctx, a); err != nil {
			t.Fatal(err)
		}
		if err := f.mount(f.sandbox("pending"), "rw"); err != nil {
			t.Fatalf("rw mount after stop: %v", err)
		}
		if _, err := q.StartSandbox(ctx, a); uniqueViolation(err) != conflict {
			t.Fatalf("start of the stopped writer = %v, want %s", err, conflict)
		}
	})

	t.Run("deleting from an active state keeps the write", func(t *testing.T) {
		f := newWorkspaceFixture(t, ctx, pool)
		a := f.sandbox("ready")
		if err := f.mount(a, "rw"); err != nil {
			t.Fatal(err)
		}
		if _, err := q.DeleteSandbox(ctx, a); err != nil {
			t.Fatal(err)
		}
		if got := uniqueViolation(f.mount(f.sandbox("pending"), "rw")); got != conflict {
			t.Fatalf("rw mount beside a deleting writer violation = %q, want %q", got, conflict)
		}
	})

	t.Run("node lost releases the write", func(t *testing.T) {
		f := newWorkspaceFixture(t, ctx, pool)
		node := uuid.New()
		f.exec(`INSERT INTO nodes (id, name, token_hash) VALUES ($1, $2, '')`, node, "n-"+node.String())
		a := f.sandbox("ready")
		f.set(a, "node_id = '"+node.String()+"'")
		if err := f.mount(a, "rw"); err != nil {
			t.Fatal(err)
		}
		if _, err := q.FailSandboxesOnNodeLost(ctx, &node); err != nil {
			t.Fatal(err)
		}
		if err := f.mount(f.sandbox("pending"), "rw"); err != nil {
			t.Fatalf("rw mount after Node lost: %v", err)
		}
	})

	for _, state := range []string{"stopped", "failed"} {
		t.Run("deleting a "+state+" writer does not take the write back", func(t *testing.T) {
			f := newWorkspaceFixture(t, ctx, pool)
			a := f.sandbox(state)
			if err := f.mount(a, "rw"); err != nil {
				t.Fatal(err)
			}
			if err := f.mount(f.sandbox("ready"), "rw"); err != nil {
				t.Fatal(err)
			}
			if _, err := q.DeleteSandbox(ctx, a); err != nil {
				t.Fatalf("delete %s writer beside a ready writer: %v", state, err)
			}
		})
	}
}

type workspaceFixture struct {
	t         *testing.T
	ctx       context.Context
	pool      *pgxpool.Pool
	user      uuid.UUID
	workspace uuid.UUID
}

func newWorkspaceFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool) *workspaceFixture {
	t.Helper()
	f := &workspaceFixture{t: t, ctx: ctx, pool: pool, user: uuid.New(), workspace: uuid.New()}
	f.exec(`INSERT INTO users (id, name) VALUES ($1, $2)`, f.user, "u-"+f.user.String())
	f.exec(`INSERT INTO workspaces (id, owner_id, kind, state) VALUES ($1, $2, 'persistent', 'ready')`, f.workspace, f.user)
	return f
}

// sandbox inserts a Sandbox in state with no Workspace yet.
func (f *workspaceFixture) sandbox(state string) uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	f.exec(`INSERT INTO sandboxes (id, owner_id, name, snapshot_digest, cpu, memory, state)
		VALUES ($1, $2, $3, 'sha256:x', 1, 1, $4)`, id, f.user, id.String(), state)
	return id
}

// mount records the Sandbox's Workspace the way creation does.
func (f *workspaceFixture) mount(sandbox uuid.UUID, mode string) error {
	return store.New(f.pool).SetSandboxWorkspace(f.ctx, store.SetSandboxWorkspaceParams{
		ID: sandbox, WorkspaceID: &f.workspace, WorkspaceMode: mode,
	})
}

func (f *workspaceFixture) set(sandbox uuid.UUID, assignment string) {
	f.t.Helper()
	f.exec(`UPDATE sandboxes SET `+assignment+` WHERE id = $1`, sandbox)
}

func (f *workspaceFixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatalf("seed: %v", err)
	}
}

// #220: the migration keeps every write an Attachment row held and drops the
// write of a deleting Sandbox that had none, so the new index builds over
// existing data.
func TestMigrateAttachmentsIntoSandboxes(t *testing.T) {
	dsn := os.Getenv("ORRERY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ORRERY_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, dbDSN := scratchDatabaseAt(t, ctx, dsn, dropAttachmentsVersion-1)

	// Workspace one: a ready writer, and a deleting rw Sandbox deleted from
	// stopped (no Attachment). Workspace two: a writer deleted while ready.
	f1 := newWorkspaceFixture(t, ctx, pool)
	ready, deletedStopped := f1.sandbox("ready"), f1.sandbox("deleting")
	f2 := newWorkspaceFixture(t, ctx, pool)
	deletedReady := f2.sandbox("deleting")
	for _, seed := range []struct {
		f       *workspaceFixture
		sandbox uuid.UUID
		attach  bool
	}{{f1, ready, true}, {f1, deletedStopped, false}, {f2, deletedReady, true}} {
		if err := seed.f.mount(seed.sandbox, "rw"); err != nil {
			t.Fatal(err)
		}
		if seed.attach {
			seed.f.exec(`INSERT INTO attachments (sandbox_id, workspace_id, mode, generation) VALUES ($1, $2, 'rw', 1)`,
				seed.sandbox, seed.f.workspace)
		}
	}

	if err := Migrate(ctx, pool, dbDSN, ""); err != nil {
		t.Fatalf("Migrate over existing Attachments: %v", err)
	}
	for _, f := range []*workspaceFixture{f1, f2} {
		if got := uniqueViolation(f.mount(f.sandbox("pending"), "rw")); got != conflict {
			t.Fatalf("rw mount beside a migrated writer violation = %q, want %q", got, conflict)
		}
	}
	var mode string
	if err := pool.QueryRow(ctx, `SELECT workspace_mode FROM sandboxes WHERE id = $1`, deletedStopped).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "ro" {
		t.Fatalf("deleting Sandbox without an Attachment mode = %q, want ro", mode)
	}
}

const (
	// dropAttachmentsVersion is migration 000031_drop_attachments.
	dropAttachmentsVersion = 31
	// conflict is the single-writer index a second rw mount violates.
	conflict = "sandboxes_workspace_rw"
)
