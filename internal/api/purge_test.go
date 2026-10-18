package api

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestResourceVersionExpired(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	ttl := 30 * 24 * time.Hour
	old := now.Add(-31 * 24 * time.Hour)
	fresh := now.Add(-29 * 24 * time.Hour)
	cases := []struct {
		name       string
		state      string
		createdAt  time.Time
		current    bool
		referenced bool
		ttl        time.Duration
		want       bool
	}{
		{"non-current unreferenced past the TTL", "ready", old, false, false, ttl, true},
		{"the current version never expires", "ready", old, true, false, ttl, false},
		{"a Sandbox reference retains the version", "ready", old, false, true, ttl, false},
		{"a fresh version survives", "ready", fresh, false, false, ttl, false},
		{"a pending publish is in flight", "pending", old, false, false, ttl, false},
		{"an unavailable version still expires", "unavailable", old, false, false, ttl, true},
		{"a zero TTL disables expiry", "ready", old, false, false, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := resourceVersionExpired(now, c.ttl, resourceVersionRetention{
				State: c.state, CreatedAt: c.createdAt, Current: c.current, Referenced: c.referenced,
			})
			if got != c.want {
				t.Fatalf("expired = %v, want %v", got, c.want)
			}
		})
	}
}

func TestSnapshotVersionCleanable(t *testing.T) {
	// A Snapshot deleted mid-build keeps its building version so the build
	// sweep can still release the Sandbox and Allocation (ADR-0014); terminal
	// versions are tombstone garbage.
	cases := map[string]bool{
		"building":  false,
		"ready":     true,
		"failed":    true,
		"cancelled": true,
	}
	for state, want := range cases {
		if got := snapshotVersionCleanable(state); got != want {
			t.Fatalf("cleanable(%q) = %v, want %v", state, got, want)
		}
	}
}

func TestCASSweepable(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	grace := 24 * time.Hour
	old := now.Add(-25 * time.Hour)
	fresh := now.Add(-23 * time.Hour)
	identical := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	ownerA, ownerB := uuid.New(), uuid.New()

	referenced := map[casObjectKey]bool{
		{owner: ownerA, sha256: identical}: true,
	}
	cases := []struct {
		name      string
		key       casObjectKey
		createdAt time.Time
		grace     time.Duration
		want      bool
	}{
		{"unreferenced past the grace", casObjectKey{owner: ownerA, sha256: "b"}, old, grace, true},
		{"referenced by a version", casObjectKey{owner: ownerA, sha256: identical}, old, grace, false},
		// The CAS key is owner-scoped (ADR-0021): owner A's reference must not
		// protect owner B's row for the same content hash.
		{"the same hash under another owner", casObjectKey{owner: ownerB, sha256: identical}, old, grace, true},
		{"inside the grace window", casObjectKey{owner: ownerA, sha256: "c"}, fresh, grace, false},
		{"a zero grace disables the sweep", casObjectKey{owner: ownerA, sha256: "d"}, old, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := casSweepable(now, c.grace, c.key, c.createdAt, referenced); got != c.want {
				t.Fatalf("sweepable = %v, want %v", got, c.want)
			}
		})
	}
}
