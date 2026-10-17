package agent

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"

	nodev1 "github.com/zibyn/stars-sandbox/gen/node/v1"
	"github.com/zibyn/stars-sandbox/internal/supervisor"
)

func TestLimitsMatch(t *testing.T) {
	// NanoCpus 0.5 CPU is a 50000 us quota over the fixed 100000 us period.
	want := supervisor.Result{
		CPUMax:            "50000 100000",
		MemoryMax:         "134217728",
		PidsMax:           "2048",
		WorkspaceWritable: true,
	}
	if !limitsMatch(want, 500_000_000, 134217728) {
		t.Fatal("matching readback was rejected")
	}
	// A daemon that ignored a limit must fail creation instead of running
	// unbounded (runtime.md §9).
	for name, mutate := range map[string]func(*supervisor.Result){
		"cpu":       func(r *supervisor.Result) { r.CPUMax = "max 100000" },
		"memory":    func(r *supervisor.Result) { r.MemoryMax = "max" },
		"pids":      func(r *supervisor.Result) { r.PidsMax = "max" },
		"workspace": func(r *supervisor.Result) { r.WorkspaceWritable = false },
	} {
		got := want
		mutate(&got)
		if limitsMatch(got, 500_000_000, 134217728) {
			t.Errorf("%s mismatch was accepted", name)
		}
	}
}

func TestCommandRunnerFencesByGeneration(t *testing.T) {
	var mu sync.Mutex
	var results []string
	send := func(msg *nodev1.AgentMessage) {
		mu.Lock()
		defer mu.Unlock()
		results = append(results, msg.GetCommandResult().GetError())
	}
	c := &commandRunner{ctx: context.Background(), send: send,
		gens: map[string]int64{}, locks: map[string]*sync.Mutex{}}

	ran := 0
	c.run("sandbox:x", "new", 5, func() error { ran++; return nil })
	c.run("sandbox:x", "old", 4, func() error { ran++; return nil })
	if ran != 1 {
		t.Fatalf("commands ran %d times, want 1", ran)
	}
	if results[1] != "stale generation" {
		t.Fatalf("stale command result = %q", results[1])
	}
}

func TestTakeReportsEachSandboxOnce(t *testing.T) {
	r := &Runtime{managed: map[string]int64{}}
	id := uuid.New()
	r.track(id, 3)
	if gen, ok := r.take(id.String()); !ok || gen != 3 {
		t.Fatalf("take = %d, %v; want 3, true", gen, ok)
	}
	// A second die event for the same container must not report again.
	if _, ok := r.take(id.String()); ok {
		t.Fatal("an untracked Sandbox was reported")
	}
}
