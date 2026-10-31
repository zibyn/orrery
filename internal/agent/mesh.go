package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/vishvananda/netlink"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	nodev1 "github.com/zibyn/orrery/gen/node/v1"
)

// Node Mesh constants (runtime.md §15, defaults.md).
const (
	meshInterface      = "orrery-wg"
	meshKeyFile        = "mesh.key"
	DefaultMeshPort    = 51820
	meshKeepalive      = 25 * time.Second
	meshReportInterval = 30 * time.Second
)

// mesh owns the kernel WireGuard interface orrery-wg. Everything goes over
// netlink: no wireguard-tools, no ip.
type mesh struct {
	wg       *wgctrl.Client
	link     netlink.Link
	key      wgtypes.Key
	subnet   string
	endpoint string

	mu    sync.Mutex
	peers map[wgtypes.Key]string // public key -> node id
}

// startMesh brings up orrery-wg with the Node's key and listen port. An error
// means this Node has no Mesh: the caller logs it and runs on without one.
func startMesh(dataRoot string, port int, endpoint, subnet string) (*mesh, error) {
	key, err := loadMeshKey(filepath.Join(dataRoot, meshKeyFile))
	if err != nil {
		return nil, err
	}
	attrs := netlink.NewLinkAttrs()
	attrs.Name = meshInterface
	if err := netlink.LinkAdd(&netlink.Wireguard{LinkAttrs: attrs}); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("create %s (no kernel WireGuard?): %w", meshInterface, err)
	}
	link, err := netlink.LinkByName(meshInterface)
	if err != nil {
		return nil, err
	}
	wg, err := wgctrl.New()
	if err != nil {
		return nil, err
	}
	if err := wg.ConfigureDevice(meshInterface, wgtypes.Config{PrivateKey: &key, ListenPort: &port}); err != nil {
		wg.Close() //nolint:errcheck // already failing
		return nil, fmt.Errorf("configure %s: %w", meshInterface, err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		wg.Close() //nolint:errcheck // already failing
		return nil, fmt.Errorf("bring up %s: %w", meshInterface, err)
	}
	return &mesh{wg: wg, link: link, key: key, subnet: subnet, endpoint: endpoint,
		peers: map[wgtypes.Key]string{}}, nil
}

// loadMeshKey reads the Node's WireGuard private key, generating it once.
func loadMeshKey(path string) (wgtypes.Key, error) {
	raw, err := os.ReadFile(path)
	if err == nil {
		return wgtypes.ParseKey(string(raw))
	}
	if !os.IsNotExist(err) {
		return wgtypes.Key{}, err
	}
	key, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		return wgtypes.Key{}, err
	}
	if err := os.WriteFile(path, []byte(key.String()), 0o600); err != nil {
		return wgtypes.Key{}, fmt.Errorf("write %s: %w", path, err)
	}
	return key, nil
}

// removeMeshKey clears the key with the Node identity (runtime.md §15).
func removeMeshKey(dataRoot string) {
	if err := os.Remove(filepath.Join(dataRoot, meshKeyFile)); err != nil && !os.IsNotExist(err) {
		slog.Warn("mesh key could not be removed", "error", err)
	}
}

// hello is the Hello's mesh; nil when the Node has no Mesh.
func (m *mesh) hello() *nodev1.Mesh {
	if m == nil {
		return nil
	}
	return &nodev1.Mesh{PublicKey: m.key.PublicKey().String(), SandboxSubnet: m.subnet, Endpoint: m.endpoint}
}

// apply replaces the peer set and the routes to the peers' Sandbox subnets.
func (m *mesh) apply(peers []*nodev1.MeshPeer) error {
	cfg := wgtypes.Config{ReplacePeers: true}
	byKey := map[wgtypes.Key]string{}
	want := map[string]bool{}
	for _, p := range peers {
		key, err := wgtypes.ParseKey(p.PublicKey)
		if err != nil {
			return fmt.Errorf("peer %s: %w", p.NodeId, err)
		}
		_, subnet, err := net.ParseCIDR(p.SandboxSubnet)
		if err != nil {
			return fmt.Errorf("peer %s: %w", p.NodeId, err)
		}
		peer := wgtypes.PeerConfig{PublicKey: key, ReplaceAllowedIPs: true, AllowedIPs: []net.IPNet{*subnet}}
		if p.Endpoint != "" {
			addr, err := net.ResolveUDPAddr("udp4", p.Endpoint)
			if err != nil {
				return fmt.Errorf("peer %s endpoint: %w", p.NodeId, err)
			}
			keepalive := meshKeepalive
			peer.Endpoint, peer.PersistentKeepaliveInterval = addr, &keepalive
		}
		cfg.Peers = append(cfg.Peers, peer)
		byKey[key] = p.NodeId
		want[subnet.String()] = true
	}
	if err := m.wg.ConfigureDevice(meshInterface, cfg); err != nil {
		return err
	}
	m.mu.Lock()
	m.peers = byKey
	m.mu.Unlock()

	routes, err := netlink.RouteList(m.link, netlink.FAMILY_V4)
	if err != nil {
		return err
	}
	for _, r := range routes {
		if r.Dst != nil && !want[r.Dst.String()] {
			if err := netlink.RouteDel(&r); err != nil {
				return err
			}
		}
	}
	for dst := range want {
		_, ipnet, _ := net.ParseCIDR(dst)
		if err := netlink.RouteReplace(&netlink.Route{LinkIndex: m.link.Attrs().Index, Dst: ipnet}); err != nil {
			return fmt.Errorf("route %s dev %s: %w", dst, meshInterface, err)
		}
	}
	return nil
}

// report is each peer's latest handshake; peers that never shook hands are
// left out.
func (m *mesh) report() (*nodev1.MeshReport, error) {
	dev, err := m.wg.Device(meshInterface)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := &nodev1.MeshReport{}
	for _, p := range dev.Peers {
		id, ok := m.peers[p.PublicKey]
		if !ok || p.LastHandshakeTime.IsZero() {
			continue
		}
		out.Peers = append(out.Peers, &nodev1.MeshPeerHandshake{
			NodeId: id, LastHandshakeAt: p.LastHandshakeTime.UTC().Format(time.RFC3339),
		})
	}
	return out, nil
}

// reportLoop sends MeshReport every report interval until ctx ends.
func (m *mesh) reportLoop(ctx context.Context, send func(*nodev1.AgentMessage)) {
	ticker := time.NewTicker(meshReportInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			report, err := m.report()
			if err != nil {
				slog.Warn("read mesh handshakes", "error", err)
				continue
			}
			send(&nodev1.AgentMessage{Message: &nodev1.AgentMessage_MeshReport{MeshReport: report}})
		}
	}
}
