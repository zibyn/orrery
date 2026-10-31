package api

import (
	"context"
	"encoding/base64"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/google/uuid"

	nodev1 "github.com/zibyn/orrery/gen/node/v1"
	"github.com/zibyn/orrery/internal/store"
)

// meshLiveWindow is the Mesh path liveness window (defaults.md).
const meshLiveWindow = 180 * time.Second

// meshPeers is the MeshPeers set for self: every other Node with mesh,
// except those whose Sandbox subnet overlaps self's and those where neither
// side has an endpoint, since such a pair can never handshake
// (node-protocol.md, runtime.md §15).
func meshPeers(self store.Node, nodes []store.Node) []*nodev1.MeshPeer {
	selfSubnet, ok := meshSubnet(self)
	if !ok {
		return nil
	}
	var peers []*nodev1.MeshPeer
	for _, n := range nodes {
		subnet, ok := meshSubnet(n)
		if !ok || n.ID == self.ID || subnet.Overlaps(selfSubnet) {
			continue
		}
		endpoint := deref(n.MeshEndpoint)
		if endpoint == "" && deref(self.MeshEndpoint) == "" {
			continue
		}
		peers = append(peers, &nodev1.MeshPeer{
			NodeId: n.ID.String(), PublicKey: *n.MeshPublicKey,
			SandboxSubnet: subnet.String(), Endpoint: endpoint,
		})
	}
	return peers
}

// meshSubnet is the Node's Sandbox subnet when its latest Hello carried mesh.
func meshSubnet(n store.Node) (netip.Prefix, bool) {
	if n.MeshPublicKey == nil || n.SandboxSubnet == nil {
		return netip.Prefix{}, false
	}
	p, err := netip.ParsePrefix(*n.SandboxSubnet)
	return p, err == nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// helloMesh validates the Hello's mesh at the trust boundary. A malformed one
// is dropped, so the Node simply has no Mesh paths.
func helloMesh(m *nodev1.Mesh) (key, endpoint, subnet *string) {
	if m == nil {
		return nil, nil, nil
	}
	raw, err := base64.StdEncoding.DecodeString(m.PublicKey)
	p, perr := netip.ParsePrefix(m.SandboxSubnet)
	if err != nil || len(raw) != 32 || perr != nil || !p.Addr().Is4() {
		return nil, nil, nil
	}
	if m.Endpoint != "" {
		if _, _, err := net.SplitHostPort(m.Endpoint); err != nil {
			return nil, nil, nil
		}
		endpoint = &m.Endpoint
	}
	s := p.Masked().String()
	return &m.PublicKey, endpoint, &s
}

// sendMeshPeers sends every connected Node with mesh its full peer set. It
// runs on every Node register, reconnect and delete (node-protocol.md). A
// Node without mesh, which includes every Agent older than the Mesh minor, is
// never sent MeshPeers.
func (s *Server) sendMeshPeers(ctx context.Context) {
	nodes, err := s.q.ListAllNodes(ctx)
	if err != nil {
		s.log.Warn("list nodes for mesh peers", "error", err)
		return
	}
	// A lost Node keeps its row but has no Token: it is nobody's peer.
	var members []store.Node
	for _, n := range nodes {
		if _, ok := meshSubnet(n); ok && !n.LostAt.Valid {
			members = append(members, n)
		}
	}
	for _, n := range members {
		s.reconcile.hub.direct(n.ID, &nodev1.ControlMessage{Message: &nodev1.ControlMessage_MeshPeers{
			MeshPeers: &nodev1.MeshPeers{Peers: meshPeers(n, members)},
		}})
	}
}

// meshMatrix is the in-memory connectivity matrix (runtime.md §15). It is
// empty after a Control Plane restart until the next round of reports.
type meshMatrix struct {
	mu sync.Mutex
	// latest is the newest handshake either side of an unordered pair reported.
	latest map[[2]uuid.UUID]time.Time
}

func newMeshMatrix() *meshMatrix {
	return &meshMatrix{latest: make(map[[2]uuid.UUID]time.Time)}
}

func meshPair(a, b uuid.UUID) [2]uuid.UUID {
	if a.String() > b.String() {
		a, b = b, a
	}
	return [2]uuid.UUID{a, b}
}

// report records one MeshReport from a Node.
func (m *meshMatrix) report(from uuid.UUID, peers []*nodev1.MeshPeerHandshake) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range peers {
		id, err := uuid.Parse(p.NodeId)
		at, terr := time.Parse(time.RFC3339, p.LastHandshakeAt)
		if err != nil || terr != nil {
			continue
		}
		key := meshPair(from, id)
		if at.After(m.latest[key]) {
			m.latest[key] = at
		}
	}
}

// live reports whether Nodes a and b have a live Mesh path: either side
// reported a handshake within the liveness window. Placement asks this
// (ADR-0027).
func (m *meshMatrix) live(a, b uuid.UUID, now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return now.Sub(m.latest[meshPair(a, b)]) <= meshLiveWindow
}
