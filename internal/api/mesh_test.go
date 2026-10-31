package api

import (
	"testing"
	"time"

	"github.com/google/uuid"

	nodev1 "github.com/zibyn/orrery/gen/node/v1"
	"github.com/zibyn/orrery/internal/store"
)

func meshNode(subnet, endpoint string) store.Node {
	key := "key-" + subnet
	n := store.Node{ID: uuid.New(), MeshPublicKey: &key, SandboxSubnet: &subnet}
	if endpoint != "" {
		n.MeshEndpoint = &endpoint
	}
	return n
}

func peerIDs(peers []*nodev1.MeshPeer) map[string]*nodev1.MeshPeer {
	out := map[string]*nodev1.MeshPeer{}
	for _, p := range peers {
		out[p.NodeId] = p
	}
	return out
}

// MeshPeers is every other Node with mesh, minus overlapping subnets and
// pairs where neither side has an endpoint (runtime.md §15).
func TestMeshPeers(t *testing.T) {
	self := meshNode("10.213.1.0/24", "")
	public := meshNode("10.213.2.0/24", "198.51.100.10:51820")
	natted := meshNode("10.213.3.0/24", "")
	overlap := meshNode("10.213.0.0/16", "203.0.113.5:51820")
	noMesh := store.Node{ID: uuid.New()}

	nodes := []store.Node{self, public, natted, overlap, noMesh}
	got := peerIDs(meshPeers(self, nodes))
	if len(got) != 1 {
		t.Fatalf("peers = %v, want only the Node with an endpoint", got)
	}
	p := got[public.ID.String()]
	if p == nil || p.PublicKey != "key-10.213.2.0/24" || p.SandboxSubnet != "10.213.2.0/24" ||
		p.Endpoint != "198.51.100.10:51820" {
		t.Fatalf("peer = %v", p)
	}

	// The public Node sees the NAT-ed one: one side with an endpoint is enough.
	got = peerIDs(meshPeers(public, nodes))
	if got[self.ID.String()] == nil || got[natted.ID.String()] == nil {
		t.Fatalf("public Node peers = %v, want both NAT-ed Nodes", got)
	}
	if got[self.ID.String()].Endpoint != "" {
		t.Fatalf("NAT-ed peer got endpoint %q", got[self.ID.String()].Endpoint)
	}
	if got[overlap.ID.String()] != nil {
		t.Fatal("an overlapping subnet was peered")
	}

	if peers := meshPeers(noMesh, nodes); len(peers) != 0 {
		t.Fatalf("a Node without mesh got peers %v", peers)
	}
}

// A path is live while either side's latest handshake is within 180 s
// (defaults.md).
func TestMeshMatrix(t *testing.T) {
	now := time.Now()
	a, b, c := uuid.New(), uuid.New(), uuid.New()
	m := newMeshMatrix()
	if m.live(a, b, now) {
		t.Fatal("an empty matrix has a path")
	}

	m.report(a, []*nodev1.MeshPeerHandshake{
		{NodeId: b.String(), LastHandshakeAt: now.Add(-179 * time.Second).Format(time.RFC3339)},
		{NodeId: c.String(), LastHandshakeAt: now.Add(-181 * time.Second).Format(time.RFC3339)},
		{NodeId: "not-a-uuid", LastHandshakeAt: now.Format(time.RFC3339)},
	})
	if !m.live(a, b, now) || !m.live(b, a, now) {
		t.Fatal("a handshake within 180 s is not a live path both ways")
	}
	if m.live(a, c, now) {
		t.Fatal("a handshake older than 180 s is a live path")
	}

	// The other side's fresher report makes the path live; a stale report
	// from the first side does not undo it.
	m.report(c, []*nodev1.MeshPeerHandshake{{NodeId: a.String(), LastHandshakeAt: now.Format(time.RFC3339)}})
	m.report(a, []*nodev1.MeshPeerHandshake{{NodeId: c.String(), LastHandshakeAt: now.Add(-time.Hour).Format(time.RFC3339)}})
	if !m.live(a, c, now) {
		t.Fatal("either side's report within 180 s is a live path")
	}
	if m.live(a, c, now.Add(181*time.Second)) {
		t.Fatal("the path did not age out")
	}
}
