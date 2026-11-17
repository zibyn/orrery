package api

import (
	"context"
	"io"
	"log/slog"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	nodev1 "github.com/zibyn/orrery/gen/node/v1"
	"github.com/zibyn/orrery/internal/nodeproto"
	"github.com/zibyn/orrery/internal/store"
)

// placementFixture is two ready containerd Nodes and one owner: enough for a
// Sandbox to move between Nodes (ADR-0031).
type placementFixture struct {
	t     *testing.T
	ctx   context.Context
	s     *Server
	r     *reconciler
	user  uuid.UUID
	nodes []store.Node
}

func newPlacementFixture(t *testing.T) *placementFixture {
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
	if err := Migrate(ctx, pool, dsn, ""); err != nil {
		t.Fatal(err)
	}
	s := &Server{pool: pool, q: store.New(pool), log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	r := &reconciler{s: s, hub: newNodeHub(), wake: make(chan struct{}, 1)}
	s.reconcile = r
	f := &placementFixture{t: t, ctx: ctx, s: s, r: r, user: uuid.New()}
	f.exec(`INSERT INTO users (id, name) VALUES ($1, $2)`, f.user, "u-"+f.user.String())
	values := map[string]string{}
	for _, name := range nodeproto.RequiredFor("containerd") {
		values[name] = nodeproto.ValueEnforced
	}
	caps := capabilitySnapshot(t, "containerd", values)
	hw := mustHardware(t, nodeHardware{CPUCores: 8, MemoryBytes: 16 << 30, DiskFree: 90, DiskTotal: 100})
	minor, health := int32(nodeproto.Minor), "healthy"
	for i := range 2 {
		id := uuid.New()
		created := time.Now().Add(time.Duration(i) * time.Second)
		f.exec(`INSERT INTO nodes (id, name, token_hash, created_at) VALUES ($1, $2, '', $3)`, id, "n-"+id.String(), created)
		f.nodes = append(f.nodes, store.Node{
			ID: id, Capabilities: caps, Hardware: hw, ProtoMinor: &minor, RuntimeHealth: &health,
			LastSeenAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
			CreatedAt:  pgtype.Timestamptz{Time: created, Valid: true},
		})
		r.hub.register(&agentConn{nodeID: id, send: make(chan *nodev1.ControlMessage, 16), reported: true})
	}
	return f
}

func (f *placementFixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.s.pool.Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
}

// sandbox is a user Sandbox in state that last ran on node (nil: never ran).
func (f *placementFixture) sandbox(state string, node *uuid.UUID) store.Sandbox {
	f.t.Helper()
	id := uuid.New()
	desired := "running"
	if state == "stopped" || state == "stopping" {
		desired = "stopped"
	}
	f.exec(`INSERT INTO sandboxes (id, owner_id, name, snapshot_digest, cpu, memory, state, desired_state, node_id)
		VALUES ($1, $2, $3, 'sha256:x', 1, 1073741824, $4, $5, $6)`, id, f.user, "sb-"+id.String(), state, desired, node)
	return f.get(id)
}

func (f *placementFixture) get(id uuid.UUID) store.Sandbox {
	f.t.Helper()
	sb, err := f.s.q.GetSandboxIncludingDeleted(f.ctx, id)
	if err != nil {
		f.t.Fatal(err)
	}
	return sb
}

// admit runs one placement of sb over the fixture's Nodes and returns the
// Node it was placed on, or nil.
func (f *placementFixture) admit(sb store.Sandbox, pinned *uuid.UUID, nodes []store.Node) *uuid.UUID {
	f.t.Helper()
	placed, err := f.r.tryAdmit(f.ctx, f.s.q, sb, pinned, nodes, time.Now())
	if err != nil {
		f.t.Fatal(err)
	}
	if !placed {
		return nil
	}
	return f.get(sb.ID).NodeID
}

// ADR-0031: a stopped Sandbox's Node is a preference, not a constraint.
func TestRestartedSandboxPlacement(t *testing.T) {
	f := newPlacementFixture(t)
	older, newer := f.nodes[0], f.nodes[1]

	t.Run("its Node infeasible, it goes elsewhere", func(t *testing.T) {
		sb := f.sandbox("pending", &older.ID)
		paused := older
		paused.Paused = true
		got := f.admit(sb, nil, []store.Node{paused, newer})
		if got == nil || *got != newer.ID {
			t.Fatalf("placed on %v, want the other Node %s", got, newer.ID)
		}
	})

	t.Run("its Node feasible, it is preferred over an earlier Node", func(t *testing.T) {
		sb := f.sandbox("pending", &newer.ID)
		got := f.admit(sb, nil, f.nodes)
		if got == nil || *got != newer.ID {
			t.Fatalf("placed on %v, want its last Node %s", got, newer.ID)
		}
	})

	t.Run("a paused Node takes no restarted Sandbox even when pinned there", func(t *testing.T) {
		sb := f.sandbox("pending", &older.ID)
		paused := older
		paused.Paused = true
		if got := f.admit(sb, &older.ID, []store.Node{paused, newer}); got != nil {
			t.Fatalf("placed on %v, want none", got)
		}
	})
}

// Migration 000037: a directory from before ADR-0031 pins its Sandbox, and
// the Sandboxes mounting its Volume, to its Node until a commit after the
// next Create there holds it.
func TestLegacyDirectoryPinsItsNode(t *testing.T) {
	f := newPlacementFixture(t)
	older, newer := f.nodes[0], f.nodes[1]
	pinnedTo := func(sb store.Sandbox) *uuid.UUID {
		t.Helper()
		nodes, err := f.s.q.ListLegacyDirNodes(f.ctx, sb.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(nodes) == 0 {
			return nil
		}
		return &nodes[0]
	}

	own := f.sandbox("pending", &newer.ID)
	f.exec(`INSERT INTO legacy_dirs (id, node_id, sandbox_id) VALUES ($1, $2, $3)`, uuid.New(), older.ID, own.ID)
	volume := uuid.New()
	f.exec(`INSERT INTO volumes (id, owner_id, name) VALUES ($1, $2, 'v')`, volume, f.user)
	f.exec(`INSERT INTO legacy_dirs (id, node_id, volume_id) VALUES ($1, $2, $1)`, volume, older.ID)
	mounting := f.sandbox("pending", &newer.ID)
	f.exec(`INSERT INTO sandbox_volumes (sandbox_id, volume_id, path, held) VALUES ($1, $2, '/data', true)`, mounting.ID, volume)

	for name, sb := range map[string]store.Sandbox{"own directory": own, "Volume's directory": mounting} {
		if got := f.admit(sb, pinnedTo(sb), f.nodes); got == nil || *got != older.ID {
			t.Fatalf("%s: placed on %v, want the directory's Node %s", name, got, older.ID)
		}
		// Committed after that Create, the Sandbox moves freely again.
		if err := f.s.q.FinishLegacyDirs(f.ctx, sb.ID); err != nil {
			t.Fatal(err)
		}
		if got := pinnedTo(sb); got != nil {
			t.Fatalf("%s: still pinned to %s after the commit", name, got)
		}
	}

	// A commit from before the upgrade's first Create there holds nothing.
	running := f.sandbox("ready", &older.ID)
	f.exec(`INSERT INTO legacy_dirs (id, node_id, sandbox_id) VALUES ($1, $2, $3)`, uuid.New(), older.ID, running.ID)
	if err := f.s.q.FinishLegacyDirs(f.ctx, running.ID); err != nil {
		t.Fatal(err)
	}
	if got := pinnedTo(running); got == nil {
		t.Fatal("a commit before the merging Create released the pin")
	}
}

// api.md §3 "宣告 lost": running, stopping and bound pending Sandboxes become
// stopped/node_lost with changes_lost and start again elsewhere; stopped ones
// keep their state.
func TestNodeLostStopsSandboxes(t *testing.T) {
	f := newPlacementFixture(t)
	lost, other := f.nodes[0], f.nodes[1]
	ready := f.sandbox("ready", &lost.ID)
	stopping := f.sandbox("stopping", &lost.ID)
	stopped := f.sandbox("stopped", &lost.ID)
	queued := f.sandbox("pending", &lost.ID) // waiting, not bound: not affected
	bound := f.sandbox("pending", &lost.ID)
	for _, sb := range []store.Sandbox{ready, stopping, bound} {
		f.exec(`INSERT INTO allocations (sandbox_id, node_id, cpu, memory, generation) VALUES ($1, $2, 1, 1, 1)`, sb.ID, lost.ID)
	}

	if err := declareNodeLost(f.ctx, f.s.q, lost.ID); err != nil {
		t.Fatal(err)
	}
	for name, sb := range map[string]store.Sandbox{"ready": ready, "stopping": stopping, "bound pending": bound} {
		got := f.s.sandboxToDTO(f.ctx, f.get(sb.ID))
		if got.State != "stopped" || got.Cause == nil || *got.Cause != "node_lost" ||
			!slices.Equal(got.Conditions, []string{"changes_lost"}) {
			t.Errorf("%s: %s %v %v, want stopped node_lost [changes_lost]", name, got.State, got.Cause, got.Conditions)
		}
	}
	if got := f.get(stopped.ID); got.State != "stopped" || got.Cause != nil {
		t.Errorf("stopped: %s %v, want untouched", got.State, got.Cause)
	}
	if got := f.get(queued.ID); got.State != "pending" {
		t.Errorf("queued pending: %s, want pending", got.State)
	}
	var allocations int
	if err := f.s.pool.QueryRow(f.ctx, `SELECT count(*) FROM allocations WHERE node_id = $1`, lost.ID).Scan(&allocations); err != nil {
		t.Fatal(err)
	}
	if allocations != 0 {
		t.Errorf("%d allocations left on the lost Node", allocations)
	}

	// Started again, it runs on the other Node.
	restarted, err := f.s.q.StartSandbox(f.ctx, ready.ID)
	if err != nil {
		t.Fatal(err)
	}
	lostNode := lost
	lostNode.Paused, lostNode.LostAt = true, pgtype.Timestamptz{Time: time.Now(), Valid: true}
	if got := f.admit(restarted, nil, []store.Node{lostNode, other}); got == nil || *got != other.ID {
		t.Fatalf("restart placed on %v, want %s", got, other.ID)
	}
}
