package agent

import (
	"context"
	"os"
	"os/user"
	"strconv"
	"testing"

	"github.com/zibyn/orrery/internal/nodeproto"
	"github.com/zibyn/orrery/internal/supervisor"
)

// TestPodmanUIDFromSocket pins the one socket layout rootless Podman may use
// (runtime.md §9): /run/user/<uid>/podman/podman.sock.
func TestPodmanUIDFromSocket(t *testing.T) {
	cases := map[string]struct {
		uid int
		ok  bool
	}{
		"/run/user/1000/podman/podman.sock": {1000, true},
		"/run/user/42/podman/podman.sock":   {42, true},
		"/var/run/docker.sock":              {0, false},
		"/run/podman/podman.sock":           {0, false},
		"/run/user/0/podman/podman.sock":    {0, false},
		"/run/user/abc/podman/podman.sock":  {0, false},
		"/run/user/1000/podman/other.sock":  {0, false},
		"/run/user/1000/docker.sock":        {0, false},
	}
	for socket, want := range cases {
		uid, ok := podmanUIDFromSocket(socket)
		if uid != want.uid || ok != want.ok {
			t.Errorf("podmanUIDFromSocket(%q) = (%d, %v), want (%d, %v)",
				socket, uid, ok, want.uid, want.ok)
		}
	}
}

// TestPodmanOwnerUsesTheUsersGID checks the group half of the rootless mapping:
// container root maps to the invoking user's uid and primary gid.
func TestPodmanOwnerUsesTheUsersGID(t *testing.T) {
	uid := os.Getuid()
	u, err := user.LookupId(strconv.Itoa(uid))
	if err != nil {
		t.Skipf("uid %d has no passwd entry: %v", uid, err)
	}
	wantGID, err := strconv.Atoi(u.Gid)
	if err != nil {
		t.Fatalf("passwd gid %q is not numeric: %v", u.Gid, err)
	}
	got := podmanOwner(uid)
	if got.uid != uid || got.gid != wantGID {
		t.Fatalf("podmanOwner(%d) = %+v, want uid %d gid %d", uid, got, uid, wantGID)
	}
}

// TestNewPodmanRuntimeKeepsOneProvider: the explicit --runtime podman
// constructor carries the provider, socket, owner and Node identity together.
func TestNewPodmanRuntimeKeepsOneProvider(t *testing.T) {
	const nodeID = "11111111-1111-1111-1111-111111111111"
	if _, err := newPodmanRuntime("/var/run/docker.sock", t.TempDir(), nodeID); err == nil {
		t.Fatal("newPodmanRuntime accepted a non-Podman socket")
	}
	socket := "/run/user/424242/podman/podman.sock"
	rt, err := newPodmanRuntime(socket, t.TempDir(), nodeID)
	if err != nil {
		t.Fatalf("newPodmanRuntime(%q): %v", socket, err)
	}
	if rt.provider != ProviderPodman || rt.socket != socket || rt.nodeID != nodeID {
		t.Fatalf("Runtime = provider %q socket %q node %q, want %q %q %q",
			rt.provider, rt.socket, rt.nodeID, ProviderPodman, socket, nodeID)
	}
	if rt.owner == nil || rt.owner.uid != 424242 {
		t.Fatalf("owner = %+v, want uid 424242", rt.owner)
	}
}

// TestPodmanNeedsAnExplicitBuildKitEndpoint: dockerd's /grpc upgrade is not a
// Podman feature, so an unset --buildkit means no endpoint, not the Runtime
// socket (runtime.md §12).
func TestPodmanNeedsAnExplicitBuildKitEndpoint(t *testing.T) {
	rt := &Runtime{provider: ProviderPodman}
	if _, err := rt.buildkitClient(context.Background()); err == nil {
		t.Fatal("a Podman Runtime with no --buildkit endpoint must not fall back to the Runtime socket")
	}
}

// TestCapabilitiesNameTheProvider is the registration readback: a Podman Node
// reports Podman even though the probe uses the same Engine API client.
func TestCapabilitiesNameTheProvider(t *testing.T) {
	caps := capabilities(supervisor.Result{WorkspaceWritable: true}, true, false, ProviderPodman)
	if caps.RuntimeProvider != ProviderPodman {
		t.Fatalf("runtime_provider = %q, want %q", caps.RuntimeProvider, ProviderPodman)
	}
	values := map[string]string{}
	for _, entry := range caps.Entries {
		values[entry.Name] = entry.Value
	}
	if values[nodeproto.CapCPUMemoryPIDLimits] != nodeproto.ValueEnforced {
		t.Fatalf("cpu_memory_pid_limits = %q, want enforced", values[nodeproto.CapCPUMemoryPIDLimits])
	}
}
