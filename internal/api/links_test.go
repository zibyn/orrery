package api

import (
	"testing"

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
			SourceID: local, SourceNodeID: a, SourceAddress: "10.213.0.3", LinksPorts: []int32{8080}},
		{TargetID: target, TargetNodeID: a, TargetAddress: "10.213.0.2",
			SourceID: remote, SourceNodeID: b, SourceAddress: "10.213.1.4", LinksPorts: []int32{}},
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

// #189: with no Mesh yet, a target goes only to the Node of its placed
// sources; no placed source leaves it free, and sources on two Nodes leave
// it nowhere to go.
func TestLinkPlaceable(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	for _, tt := range []struct {
		name    string
		sources []uuid.UUID
		node    uuid.UUID
		want    bool
	}{
		{"no placed source", nil, a, true},
		{"the sources' Node", []uuid.UUID{a}, a, true},
		{"another Node", []uuid.UUID{a}, b, false},
		{"sources on two Nodes", []uuid.UUID{a, b}, a, false},
	} {
		if got := linkPlaceable(tt.sources, tt.node); got != tt.want {
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
			SourceID: uuid.Nil, SourceNodeID: old.ID, SourceAddress: "10.213.0.3",
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
