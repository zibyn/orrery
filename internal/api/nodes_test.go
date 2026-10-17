package api

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
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
