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
	seen := pgtype.Timestamptz{Time: now.Add(-time.Second), Valid: true}
	healthy := "healthy"
	unhealthy := "unhealthy"
	minor := int32(nodeproto.Minor)

	required := make([]nodeproto.CapabilityEntry, 0, len(nodeproto.Required))
	for _, name := range nodeproto.Required {
		required = append(required, nodeproto.CapabilityEntry{Name: name, Value: nodeproto.ValueEnforced})
	}
	caps, err := json.Marshal(nodeproto.CapabilityReport{Runtime: "docker", Entries: required})
	if err != nil {
		t.Fatal(err)
	}
	unsupported := append([]nodeproto.CapabilityEntry(nil), required...)
	unsupported[0].Value = nodeproto.ValueUnsupported
	weakCaps, err := json.Marshal(nodeproto.CapabilityReport{Runtime: "docker", Entries: unsupported})
	if err != nil {
		t.Fatal(err)
	}

	ready := store.Node{
		LastSeenAt:    seen,
		RuntimeHealth: &healthy,
		Capabilities:  caps,
		ProtoMinor:    &minor,
	}
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

func mutate(node store.Node, change func(*store.Node)) store.Node {
	change(&node)
	return node
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
