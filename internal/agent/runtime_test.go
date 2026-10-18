package agent

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	nodev1 "github.com/zibyn/stars-sandbox/gen/node/v1"
	"github.com/zibyn/stars-sandbox/internal/supervisor"
)

// runtime.md §8: an Agent adopts only containers carrying its own
// stars.node=<node_id> label, so two Agents sharing a daemon never take over
// each other's Runtimes.
func TestContainerOwnedBy(t *testing.T) {
	const mine = "11111111-1111-1111-1111-111111111111"
	for name, tt := range map[string]struct {
		labels map[string]string
		want   bool
	}{
		"this Node": {
			labels: map[string]string{"stars.managed": "true", "stars.node": mine},
			want:   true,
		},
		"another Node": {
			labels: map[string]string{"stars.managed": "true", "stars.node": "22222222-2222-2222-2222-222222222222"},
			want:   false,
		},
		"no Node label": {
			labels: map[string]string{"stars.managed": "true"},
			want:   false,
		},
		"not managed": {
			labels: map[string]string{"stars.node": mine},
			want:   false,
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := containerOwnedBy(tt.labels, mine); got != tt.want {
				t.Fatalf("containerOwnedBy(%v) = %v, want %v", tt.labels, got, tt.want)
			}
		})
	}
}

// #93: only docker is implemented; podman and local have their own issues and
// must fail loudly instead of silently running Docker.
func TestResolveRuntime(t *testing.T) {
	if got, err := resolveRuntime(""); err != nil || got != "docker" {
		t.Fatalf("resolveRuntime(\"\") = %q, %v; want docker", got, err)
	}
	if got, err := resolveRuntime("docker"); err != nil || got != "docker" {
		t.Fatalf("resolveRuntime(docker) = %q, %v", got, err)
	}
	for _, name := range []string{"podman", "local"} {
		if _, err := resolveRuntime(name); err == nil || !strings.Contains(err.Error(), "not implemented") {
			t.Fatalf("resolveRuntime(%s) error = %v, want not implemented", name, err)
		}
	}
	if _, err := resolveRuntime("bogus"); err == nil {
		t.Fatal("resolveRuntime(bogus) accepted an unknown provider")
	}
}

func TestLimitsMatch(t *testing.T) {
	// NanoCpus 0.5 CPU is a 50000 us quota over the fixed 100000 us period.
	want := supervisor.Result{
		CPUMax:            "50000 100000",
		MemoryMax:         "134217728",
		PidsMax:           "2048",
		WorkspaceWritable: true,
	}
	if !limitsMatch(want, 500_000_000, 134217728, 0, "rw") {
		t.Fatal("matching readback was rejected")
	}
	// A daemon that ignored a limit must fail creation instead of running
	// unbounded (runtime.md §9). A container that sees more or fewer cards
	// than requested fails it too: whole-card isolation is the readback.
	for name, tt := range map[string]struct {
		mutate func(*supervisor.Result)
		gpus   int
	}{
		"cpu":       {mutate: func(r *supervisor.Result) { r.CPUMax = "max 100000" }},
		"memory":    {mutate: func(r *supervisor.Result) { r.MemoryMax = "max" }},
		"pids":      {mutate: func(r *supervisor.Result) { r.PidsMax = "max" }},
		"workspace": {mutate: func(r *supervisor.Result) { r.WorkspaceWritable = false }},
		"gpus": {
			mutate: func(r *supervisor.Result) { r.NvidiaDevices = nil },
			gpus:   1,
		},
		"extra gpus": {
			mutate: func(r *supervisor.Result) { r.NvidiaDevices = []string{"/dev/nvidia0", "/dev/nvidia1"} },
			gpus:   1,
		},
		"unrequested gpus": {
			mutate: func(r *supervisor.Result) { r.NvidiaDevices = []string{"/dev/nvidia0"} },
		},
	} {
		got := want
		tt.mutate(&got)
		if limitsMatch(got, 500_000_000, 134217728, tt.gpus, "rw") {
			t.Errorf("%s mismatch was accepted", name)
		}
	}
	// One numbered device per requested card is enough; control nodes do not
	// count.
	withGPU := want
	withGPU.NvidiaDevices = []string{"/dev/nvidia0", "/dev/nvidiactl", "/dev/nvidia-uvm"}
	if !limitsMatch(withGPU, 500_000_000, 134217728, 1, "rw") {
		t.Fatal("a readback with the requested card was rejected")
	}
	// A read-only Attachment must read back as not writable, and a writable
	// mount must not satisfy a read-only request.
	readOnly := want
	readOnly.WorkspaceWritable = false
	if !limitsMatch(readOnly, 500_000_000, 134217728, 0, "ro") {
		t.Fatal("a read-only Workspace readback was rejected")
	}
	if limitsMatch(readOnly, 500_000_000, 134217728, 0, "rw") {
		t.Fatal("a read-only Workspace was accepted for rw")
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
	r := &Runtime{dataRoot: t.TempDir(), managed: map[string]int64{}}
	id := uuid.New()
	r.track(id, 2)
	// A delayed die event from the container this generation replaced is not
	// evidence the current Runtime died.
	if _, ok := r.takeIf(id.String(), 1); ok {
		t.Fatal("a replaced generation's die event was reported")
	}
	if gen, ok := r.takeIf(id.String(), 2); !ok || gen != 2 {
		t.Fatalf("takeIf = %d, %v; want 2, true", gen, ok)
	}
	// A second die event for the same container must not report again.
	if _, ok := r.takeIf(id.String(), 2); ok {
		t.Fatal("an untracked Sandbox was reported")
	}
}
