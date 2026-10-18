package api

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	nodev1 "github.com/zibyn/stars-sandbox/gen/node/v1"
	"github.com/zibyn/stars-sandbox/internal/nodeproto"
	"github.com/zibyn/stars-sandbox/internal/store"
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
		{"paused refuses a new Workspace", paused, false, false},
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
