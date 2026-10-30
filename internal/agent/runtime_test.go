package agent

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	nodev1 "github.com/zibyn/orrery/gen/node/v1"
)

// runtime.md §8: an Agent adopts only containers carrying its own
// orrery.node=<node_id> label, so two Agents sharing a daemon never take over
// each other's Runtimes.
func TestContainerOwnedBy(t *testing.T) {
	const mine = "11111111-1111-1111-1111-111111111111"
	for name, tt := range map[string]struct {
		labels map[string]string
		want   bool
	}{
		"this Node": {
			labels: map[string]string{"orrery.managed": "true", "orrery.node": mine},
			want:   true,
		},
		"another Node": {
			labels: map[string]string{"orrery.managed": "true", "orrery.node": "22222222-2222-2222-2222-222222222222"},
			want:   false,
		},
		"no Node label": {
			labels: map[string]string{"orrery.managed": "true"},
			want:   false,
		},
		"not managed": {
			labels: map[string]string{"orrery.node": mine},
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

// runtime.md §8: a Create that finds a running Runtime under its name adopts
// it only when this Node owns it at the requested generation; anything else
// is torn down and created afresh.
func TestCreateAdopts(t *testing.T) {
	const mine = "11111111-1111-1111-1111-111111111111"
	owned := func(gen string) map[string]string {
		return map[string]string{"orrery.managed": "true", "orrery.node": mine, "orrery.generation": gen}
	}
	for name, tt := range map[string]struct {
		labels map[string]string
		want   bool
	}{
		"this Node, same generation":  {labels: owned("3"), want: true},
		"this Node, older generation": {labels: owned("2"), want: false},
		"no generation label":         {labels: map[string]string{"orrery.managed": "true", "orrery.node": mine}, want: false},
		"another Node": {
			labels: map[string]string{"orrery.managed": "true", "orrery.node": "22222222-2222-2222-2222-222222222222", "orrery.generation": "3"},
			want:   false,
		},
		"no Node label": {labels: map[string]string{"orrery.managed": "true", "orrery.generation": "3"}, want: false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := createAdopts(tt.labels, mine, 3); got != tt.want {
				t.Fatalf("createAdopts(%v) = %v, want %v", tt.labels, got, tt.want)
			}
		})
	}
}

// #93/#96/#97, #170: the --runtime flag is the single source of truth.
// containerd is the default, local is explicit, the removed docker and podman
// fail at startup pointing at containerd, and anything else fails loudly.
func TestResolveRuntime(t *testing.T) {
	if got, err := resolveRuntime(""); err != nil || got != "containerd" {
		t.Fatalf("resolveRuntime(\"\") = %q, %v; want containerd", got, err)
	}
	for _, name := range []string{"containerd", "local"} {
		if got, err := resolveRuntime(name); err != nil || got != name {
			t.Fatalf("resolveRuntime(%s) = %q, %v; want %s", name, got, err, name)
		}
	}
	for _, name := range []string{"docker", "podman"} {
		_, err := resolveRuntime(name)
		if err == nil || !strings.Contains(err.Error(), "--runtime containerd") {
			t.Fatalf("resolveRuntime(%s) = %v; want an error pointing at --runtime containerd", name, err)
		}
	}
	if _, err := resolveRuntime("bogus"); err == nil {
		t.Fatal("resolveRuntime(bogus) accepted an unknown provider")
	}
}

// #170: a stale --docker socket fails Run before anything else happens.
func TestRunRejectsDockerFlag(t *testing.T) {
	err := Run(context.Background(), RunOptions{Docker: "/var/run/docker.sock"})
	if err == nil || !strings.Contains(err.Error(), "--containerd") {
		t.Fatalf("Run(--docker) = %v; want an error pointing at --containerd", err)
	}
}

// #168: --isolation is a containerd Node's choice, runc by default, and is
// rejected on every other Provider.
func TestResolveIsolation(t *testing.T) {
	for _, tt := range []struct {
		provider, isolation, want string
		ok                        bool
	}{
		{"containerd", "", "runc", true},
		{"containerd", "runc", "runc", true},
		{"containerd", "gvisor", "gvisor", true},
		{"containerd", "kata", "", false},
		{"local", "", "", true},
		{"local", "gvisor", "", false},
	} {
		got, err := resolveIsolation(tt.provider, tt.isolation)
		if (err == nil) != tt.ok || got != tt.want {
			t.Errorf("resolveIsolation(%q, %q) = %q, %v; want %q, ok %v", tt.provider, tt.isolation, got, err, tt.want, tt.ok)
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
