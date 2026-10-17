package api

import (
	"testing"
	"time"
)

func TestIdleExpired(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	last := now.Add(-900 * time.Second)
	for _, tt := range []struct {
		name    string
		last    time.Time
		hasLast bool
		idle    int32
		execs   int
		live    int
		want    bool
	}{
		{"idle stop disabled with 0", last, true, 0, 0, 0, false},
		{"exactly at the deadline", last, true, 900, 0, 0, true},
		{"one second before the deadline", last.Add(time.Second), true, 900, 0, 0, false},
		{"a running Exec is activity", last, true, 900, 1, 0, false},
		{"a live connection is activity", last, true, 900, 0, 1, false},
		{"no activity yet", last, false, 900, 0, 0, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := idleExpired(tt.last, tt.hasLast, tt.idle, now, tt.execs, tt.live); got != tt.want {
				t.Fatalf("idleExpired = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestWorkspaceConditions(t *testing.T) {
	now := time.Now()
	future := now.Add(time.Second)
	past := now.Add(-time.Second)

	if !(workspaceConditions{}).provisionDue(now) {
		t.Fatal("a first attempt is due immediately")
	}
	if (workspaceConditions{NextAttemptAt: &future}).provisionDue(now) {
		t.Fatal("a backoff in the future is not due")
	}
	if !(workspaceConditions{NextAttemptAt: &past}).provisionDue(now) {
		t.Fatal("an elapsed backoff is due")
	}

	raw := encodeWorkspaceConditions(workspaceConditions{Attempts: 3, ProvisioningStalled: true})
	parsed := parseWorkspaceConditions(raw)
	if parsed.Attempts != 3 || !parsed.ProvisioningStalled {
		t.Fatalf("round trip = %+v", parsed)
	}
	names := parsed.names()
	if len(names) != 1 || names[0] != "provisioning_stalled" {
		t.Fatalf("condition names = %v", names)
	}
	if got := parseWorkspaceConditions(nil).names(); len(got) != 0 {
		t.Fatalf("nil conditions = %v, want empty", got)
	}
}
