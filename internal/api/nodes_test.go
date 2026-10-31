package api

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	nodev1 "github.com/zibyn/orrery/gen/node/v1"
	"github.com/zibyn/orrery/internal/nodeproto"
	"github.com/zibyn/orrery/internal/store"
)

func TestDeriveNodeStatus(t *testing.T) {
	now := time.Now()
	unhealthy := "unhealthy"
	ready := readyNode(t, now)
	weakCaps := capabilityReport(t, true)

	tests := []struct {
		name string
		node store.Node
		want string
	}{
		{"lost wins over everything", mutate(ready, func(n *store.Node) {
			n.LostAt = pgtype.Timestamptz{Time: now, Valid: true}
		}), "lost"},
		{"paused", mutate(ready, func(n *store.Node) { n.Paused = true }), "paused"},
		{"never seen", mutate(ready, func(n *store.Node) {
			n.LastSeenAt = pgtype.Timestamptz{}
		}), "unavailable"},
		{"stale heartbeat", mutate(ready, func(n *store.Node) {
			n.LastSeenAt = pgtype.Timestamptz{Time: now.Add(-31 * time.Second), Valid: true}
		}), "unavailable"},
		{"unhealthy runtime", mutate(ready, func(n *store.Node) { n.RuntimeHealth = &unhealthy }), "unavailable"},
		{"no runtime health yet", mutate(ready, func(n *store.Node) { n.RuntimeHealth = nil }), "unavailable"},
		{"no capabilities yet", mutate(ready, func(n *store.Node) { n.Capabilities = nil }), "unavailable"},
		{"required capability unsupported", mutate(ready, func(n *store.Node) { n.Capabilities = weakCaps }), "unavailable"},
		{"ready", ready, "ready"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := deriveNodeStatus(tt.node, now); got != tt.want {
				t.Fatalf("deriveNodeStatus = %q, want %q", got, tt.want)
			}
		})
	}
}

// Declaring a Node lost is irreversible (api.md §3): a lost Node can never be
// resumed, so the resume handler refuses it.
func TestNodeLost(t *testing.T) {
	now := time.Now()
	lost := mutate(readyNode(t, now), func(n *store.Node) {
		n.LostAt = pgtype.Timestamptz{Time: now, Valid: true}
	})
	if !nodeLost(lost) {
		t.Fatal("a Node with lost_at was not reported lost")
	}
	if nodeLost(readyNode(t, now)) {
		t.Fatal("a live Node was reported lost")
	}
}

// Reachable is liveness only: a fresh heartbeat or an open stream. An
// unhealthy Runtime is still reachable, so declaring that Node lost is refused
// (ADR-0014).
func TestNodeReachable(t *testing.T) {
	now := time.Now()
	unhealthy := "unhealthy"
	fresh := mutate(readyNode(t, now), func(n *store.Node) { n.RuntimeHealth = &unhealthy })
	stale := mutate(fresh, func(n *store.Node) {
		n.LastSeenAt = pgtype.Timestamptz{Time: now.Add(-31 * time.Second), Valid: true}
	})
	lost := mutate(readyNode(t, now), func(n *store.Node) {
		n.LostAt = pgtype.Timestamptz{Time: now, Valid: true}
	})

	tests := []struct {
		name      string
		node      store.Node
		connected bool
		want      bool
	}{
		{"fresh heartbeat", fresh, false, true},
		{"open stream without a heartbeat yet", mutate(stale, func(n *store.Node) {
			n.LastSeenAt = pgtype.Timestamptz{}
		}), true, true},
		{"stale heartbeat and no stream", stale, false, false},
		{"already declared lost", lost, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := nodeReachable(tt.node, tt.connected, now); got != tt.want {
				t.Fatalf("nodeReachable = %v, want %v", got, tt.want)
			}
		})
	}
}

// A paused Node takes no new Workspace, but a Sandbox whose Workspace is
// already bound there can still start (ADR-0017).
func TestNodeSchedulable(t *testing.T) {
	now := time.Now()
	ready := readyNode(t, now)
	paused := mutate(ready, func(n *store.Node) { n.Paused = true })
	unavailable := mutate(ready, func(n *store.Node) { n.RuntimeHealth = nil })
	pausedUnavailable := mutate(unavailable, func(n *store.Node) { n.Paused = true })
	minorTooOld := int32(nodeproto.Minor) - 1
	outdated := mutate(ready, func(n *store.Node) { n.ProtoMinor = &minorTooOld })
	pausedOutdated := mutate(outdated, func(n *store.Node) { n.Paused = true })

	tests := []struct {
		name          string
		node          store.Node
		workspaceHere bool
		want          bool
	}{
		{"ready takes a new Workspace", ready, false, true},
		{"paused refuses a new Workspace or a build (no bound Workspace)", paused, false, false},
		{"paused runs a Sandbox for its bound Workspace", paused, true, true},
		{"unavailable takes nothing", unavailable, true, false},
		{"paused does not bypass an unhealthy Runtime", pausedUnavailable, true, false},
		{"paused does not bypass an outdated Agent", pausedOutdated, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := nodeSchedulable(tt.node, now, tt.workspaceHere); got != tt.want {
				t.Fatalf("nodeSchedulable = %v, want %v", got, tt.want)
			}
		})
	}
}

// Deleting a Node has to end its live Control stream: close fires the
// connection's done channel exactly once, so Control returns UNAUTHENTICATED
// (ADR-0014, api.md §3).
func TestNodeHubClose(t *testing.T) {
	hub := newNodeHub()
	id := uuid.New()
	conn := &agentConn{nodeID: id, send: make(chan *nodev1.ControlMessage, 1), done: make(chan struct{})}
	hub.register(conn)

	hub.close(id)
	hub.close(id) // idempotent: a second close must not panic

	select {
	case <-conn.done:
	default:
		t.Fatal("close did not end the connection")
	}
}

// A reconnecting Node is placed only on the Capabilities its new stream
// reports: until then admission sees no report, as on first registration
// (ADR-0014), while the stored report stays for the Node list.
func TestReconnectedNodeAwaitsCapabilities(t *testing.T) {
	now := time.Now()
	hub := newNodeHub()
	node := readyNode(t, now)
	node.ID = uuid.New()
	connect := func() *agentConn {
		conn := &agentConn{nodeID: node.ID, send: make(chan *nodev1.ControlMessage, 1), done: make(chan struct{})}
		hub.register(conn)
		return conn
	}
	schedulable := func() bool {
		seen := hub.currentReports([]store.Node{node})[0]
		return nodeSchedulable(seen, now, false)
	}

	first := connect()
	if schedulable() {
		t.Fatal("a Node was placeable before its stream reported Capabilities")
	}
	hub.capabilitiesReported(first)
	if !schedulable() {
		t.Fatal("a Node that reported Capabilities on its stream must be placeable")
	}

	hub.unregister(first)
	second := connect()
	if schedulable() {
		t.Fatal("a reconnected Node was placed on the previous stream's Capabilities")
	}
	if len(node.Capabilities) == 0 {
		t.Fatal("the stored report must not be cleared")
	}
	hub.capabilitiesReported(first) // a late report from the old stream does not count
	if schedulable() {
		t.Fatal("the old stream's report made the new stream placeable")
	}
	hub.capabilitiesReported(second)
	if !schedulable() {
		t.Fatal("a reconnected Node must be placeable once it reports Capabilities")
	}
}

func mutate(node store.Node, change func(*store.Node)) store.Node {
	change(&node)
	return node
}

// readyNode is a Node that reports a healthy Runtime and every required
// capability, so deriveNodeStatus calls it ready.
func readyNode(t *testing.T, now time.Time) store.Node {
	t.Helper()
	healthy := "healthy"
	minor := int32(nodeproto.Minor)
	return store.Node{
		LastSeenAt:    pgtype.Timestamptz{Time: now, Valid: true},
		RuntimeHealth: &healthy,
		Capabilities:  capabilityReport(t, false),
		ProtoMinor:    &minor,
	}
}

// capabilityReport is a complete Capability report; unsupported flips the
// first required entry.
func capabilityReport(t *testing.T, unsupported bool) json.RawMessage {
	t.Helper()
	entries := make([]nodeproto.CapabilityEntry, 0, len(nodeproto.Required))
	for i, name := range nodeproto.Required {
		value := nodeproto.ValueEnforced
		if unsupported && i == 0 {
			value = nodeproto.ValueUnsupported
		}
		entries = append(entries, nodeproto.CapabilityEntry{Name: name, Value: value})
	}
	raw, err := json.Marshal(nodeproto.CapabilityReport{RuntimeProvider: "docker", Entries: entries})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// The per-GPU share is three quarters of the allocatable amount, split evenly
// across the cards (defaults.md, ADR-0015).
func TestPerGPUShare(t *testing.T) {
	heartbeat := &nodev1.Heartbeat{
		CpuCores: 16, ReservedCpu: 1,
		MemoryBytes: 34 << 30, ReservedMemory: 2 << 30,
	}
	cpu, memory := perGPUShare(heartbeat, 4)
	if cpu != 15.0*0.75/4 {
		t.Fatalf("cpu per GPU = %v, want %v", cpu, 15.0*0.75/4)
	}
	if want := int64(32 << 30 * 0.75 / 4); memory != want {
		t.Fatalf("memory per GPU = %d, want %d", memory, want)
	}

	if cpu, memory := perGPUShare(heartbeat, 0); cpu != 0 || memory != 0 {
		t.Fatalf("a Node without cards has a share: %v, %d", cpu, memory)
	}
	// A reservation larger than the host must not produce a negative share.
	over := &nodev1.Heartbeat{CpuCores: 1, ReservedCpu: 2, MemoryBytes: 1 << 30, ReservedMemory: 2 << 30}
	if cpu, memory := perGPUShare(over, 2); cpu != 0 || memory != 0 {
		t.Fatalf("an over-reserved Node has a share: %v, %d", cpu, memory)
	}
}
