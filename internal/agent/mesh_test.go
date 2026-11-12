package agent

import (
	"os"
	"path/filepath"
	"testing"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	nodev1 "github.com/zibyn/orrery/gen/node/v1"
)

// The mesh key is generated once, 0600, survives a restart and is cleared
// with the identity (runtime.md §15).
func TestMeshKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, meshKeyFile)
	first, err := loadMeshKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mesh.key stat = %v, %v; want mode 0600", info, err)
	}
	again, err := loadMeshKey(path)
	if err != nil || again != first {
		t.Fatalf("restart got a different key (%v)", err)
	}
	removeMeshKey(dir)
	fresh, err := loadMeshKey(path)
	if err != nil || fresh == first {
		t.Fatalf("a cleared key was reused (%v)", err)
	}
}

// peerConfig updates the peer set in place: a dropped peer is removed, a kept
// or new one is upserted, and the whole set is never replaced (the result is
// per-peer only), so a kept peer keeps its session and learned endpoint.
func TestPeerConfigIsIncremental(t *testing.T) {
	key := func() wgtypes.Key {
		k, err := wgtypes.GeneratePrivateKey()
		if err != nil {
			t.Fatal(err)
		}
		return k.PublicKey()
	}
	kept, dropped, added := key(), key(), key()
	current := []wgtypes.Peer{{PublicKey: kept}, {PublicKey: dropped}}
	desired := []*nodev1.MeshPeer{
		{NodeId: "kept", PublicKey: kept.String(), SandboxSubnet: "10.89.1.0/24"},
		{NodeId: "added", PublicKey: added.String(), SandboxSubnet: "10.89.2.0/24", Endpoint: "192.0.2.7:51820"},
	}
	got, err := peerConfig(current, desired)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d peer configs, want 3: %+v", len(got), got)
	}
	k, a, d := got[0], got[1], got[2]
	if k.PublicKey != kept || k.Remove || !k.ReplaceAllowedIPs || k.Endpoint != nil ||
		len(k.AllowedIPs) != 1 || k.AllowedIPs[0].String() != "10.89.1.0/24" {
		t.Errorf("kept peer = %+v; want an upsert of its subnet without an endpoint", k)
	}
	if a.PublicKey != added || a.Remove || !a.ReplaceAllowedIPs || a.Endpoint == nil ||
		a.Endpoint.String() != "192.0.2.7:51820" || a.PersistentKeepaliveInterval == nil {
		t.Errorf("added peer = %+v; want an upsert with its endpoint and keepalive", a)
	}
	if d.PublicKey != dropped || !d.Remove {
		t.Errorf("dropped peer = %+v; want Remove", d)
	}
}
