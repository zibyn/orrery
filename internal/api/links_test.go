package api

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	nodev1 "github.com/zibyn/orrery/gen/node/v1"
	"github.com/zibyn/orrery/internal/store"
)

// node-protocol.md: each Node gets every Link with an end on it; a Link
// across two Nodes goes to both, and a Node with no Link end gets none.
func TestSandboxLinksByNode(t *testing.T) {
	a, b, c := uuid.New(), uuid.New(), uuid.New()
	target, local, remote := uuid.New(), uuid.New(), uuid.New()
	rows := []store.ListSandboxLinkEndsRow{
		{TargetID: target, TargetNodeID: a, TargetAddress: "10.213.0.2",
			SourceID: local, SourceNodeID: &a, SourceAddress: "10.213.0.3", LinksPorts: []int32{8080}},
		{TargetID: target, TargetNodeID: a, TargetAddress: "10.213.0.2",
			SourceID: remote, SourceNodeID: &b, SourceAddress: "10.213.1.4", LinksPorts: []int32{}},
	}
	got := sandboxLinksByNode(rows)
	want := map[uuid.UUID][]*nodev1.SandboxLink{
		a: {
			{TargetId: target.String(), TargetAddress: "10.213.0.2", SourceId: local.String(), SourceAddress: "10.213.0.3", Ports: []uint32{8080}},
			{TargetId: target.String(), TargetAddress: "10.213.0.2", SourceId: remote.String(), SourceAddress: "10.213.1.4"},
		},
		b: {
			{TargetId: target.String(), TargetAddress: "10.213.0.2", SourceId: remote.String(), SourceAddress: "10.213.1.4"},
		},
	}
	if len(got) != len(want) || len(got[c]) != 0 {
		t.Fatalf("Nodes = %d, want %d (and none for an unrelated Node)", len(got), len(want))
	}
	for node, links := range want {
		if !proto.Equal(&nodev1.SandboxLinks{Links: got[node]}, &nodev1.SandboxLinks{Links: links}) {
			t.Errorf("Node %s links = %v, want %v", node, got[node], links)
		}
	}
}

// runtime.md §15: a target may go to a source's Node, or to a Node with a
// live Mesh path to every placed source's Node; no placed source leaves it
// free.
func TestLinkPlaceable(t *testing.T) {
	a, b, c, d := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	// Paths: a-b, b-c. None to d.
	live := func(x, y uuid.UUID) bool {
		pair := map[[2]uuid.UUID]bool{{a, b}: true, {b, a}: true, {b, c}: true, {c, b}: true}
		return pair[[2]uuid.UUID{x, y}]
	}
	for _, tt := range []struct {
		name    string
		sources []uuid.UUID
		node    uuid.UUID
		want    bool
	}{
		{"no placed source", nil, d, true},
		{"the sources' Node", []uuid.UUID{a}, a, true},
		{"a Node with a path", []uuid.UUID{a}, b, true},
		{"a Node without a path", []uuid.UUID{a}, c, false},
		{"a Node with no paths at all", []uuid.UUID{a}, d, false},
		{"a Node with a path to every source", []uuid.UUID{a, c}, b, true},
		{"a source's Node with a path to the other", []uuid.UUID{a, b}, a, true},
		{"a source's Node without a path to the other", []uuid.UUID{a, c}, a, false},
	} {
		if got := linkPlaceable(tt.sources, tt.node, live); got != tt.want {
			t.Errorf("%s: linkPlaceable = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// runtime.md §13: the set is pushed whole when it changes for a Node,
// including an address change on restart, and not again while it holds.
// Agents older than minor 5 are never sent it.
func TestPushSandboxLinks(t *testing.T) {
	r := &reconciler{hub: newNodeHub()}
	minor := func(v int32) *int32 { return &v }
	current, old := store.Node{ID: uuid.New(), ProtoMinor: minor(5)}, store.Node{ID: uuid.New(), ProtoMinor: minor(4)}
	conn := &agentConn{nodeID: current.ID, send: make(chan *nodev1.ControlMessage, 8)}
	oldConn := &agentConn{nodeID: old.ID, send: make(chan *nodev1.ControlMessage, 8)}
	r.hub.register(conn)
	r.hub.register(oldConn)
	nodes := []store.Node{current, old}
	row := func(targetAddress string) []store.ListSandboxLinkEndsRow {
		return []store.ListSandboxLinkEndsRow{{
			TargetID: uuid.Nil, TargetNodeID: current.ID, TargetAddress: targetAddress,
			SourceID: uuid.Nil, SourceNodeID: &old.ID, SourceAddress: "10.213.0.3",
		}}
	}
	received := func() []*nodev1.SandboxLink {
		t.Helper()
		select {
		case msg := <-conn.send:
			return msg.GetSandboxLinks().GetLinks()
		default:
			t.Fatal("no SandboxLinks sent")
			return nil
		}
	}
	quiet := func(why string) {
		t.Helper()
		if len(conn.send) != 0 {
			t.Fatalf("SandboxLinks resent %s", why)
		}
	}

	r.pushSandboxLinks(nodes, row("10.213.0.2"))
	if links := received(); len(links) != 1 || links[0].TargetAddress != "10.213.0.2" {
		t.Fatalf("first push = %v", links)
	}
	r.pushSandboxLinks(nodes, row("10.213.0.2"))
	quiet("while unchanged")

	r.pushSandboxLinks(nodes, row("10.213.0.9"))
	if links := received(); len(links) != 1 || links[0].TargetAddress != "10.213.0.9" {
		t.Fatalf("after restart = %v, want the new target address", links)
	}
	r.pushSandboxLinks(nodes, nil)
	if links := received(); len(links) != 0 {
		t.Fatalf("after stop = %v, want an empty set", links)
	}
	r.pushSandboxLinks(nodes, row("10.213.0.9"))
	if links := received(); len(links) != 1 {
		t.Fatalf("a set seen before was not resent: %v", links)
	}
	if len(oldConn.send) != 0 {
		t.Fatal("an Agent older than minor 5 was sent SandboxLinks")
	}
}

// #194: while every source of a running target is stopped or deleted, the
// target's Node still gets its Links, with an empty source_address, so it
// keeps rendering the target's inbound chain (node-protocol.md).
func TestListSandboxLinkEndsSourcesStopped(t *testing.T) {
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
	user, node := uuid.New(), uuid.New()
	target, stopped, deleted := uuid.New(), uuid.New(), uuid.New()
	sandbox := `INSERT INTO sandboxes (id, owner_id, name, snapshot_digest, cpu, memory, node_id, address, deleted_at, links_from)
		VALUES ($1, $2, $3, 'sha256:x', 1, 1, $4, $5, $6, $7)`
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users (id, name, login_type) VALUES ($1, $2, 'none')`, []any{user, "u-" + user.String()}},
		{`INSERT INTO nodes (id, name, token_hash) VALUES ($1, $2, '')`, []any{node, "n-" + node.String()}},
		{sandbox, []any{stopped, user, "stopped", node, nil, nil, []uuid.UUID{}}},
		{sandbox, []any{deleted, user, "deleted", node, "10.213.0.4", time.Now(), []uuid.UUID{}}},
		{sandbox, []any{target, user, "target", node, "10.213.0.2", nil, []uuid.UUID{stopped, deleted}}},
	} {
		if _, err := pool.Exec(ctx, stmt.sql, stmt.args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	rows, err := store.New(pool).ListSandboxLinkEnds(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var mine []store.ListSandboxLinkEndsRow
	for _, row := range rows {
		if row.TargetID == target {
			mine = append(mine, row)
		}
	}
	got := sandboxLinksByNode(mine)[node]
	want := []*nodev1.SandboxLink{
		{TargetId: target.String(), TargetAddress: "10.213.0.2", SourceId: stopped.String()},
		{TargetId: target.String(), TargetAddress: "10.213.0.2", SourceId: deleted.String()},
	}
	if stopped.String() > deleted.String() {
		want[0], want[1] = want[1], want[0]
	}
	if !proto.Equal(&nodev1.SandboxLinks{Links: got}, &nodev1.SandboxLinks{Links: want}) {
		t.Fatalf("target Node links = %v, want %v", got, want)
	}
}
