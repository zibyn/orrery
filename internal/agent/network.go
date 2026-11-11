package agent

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"

	nodev1 "github.com/zibyn/orrery/gen/node/v1"
)

const (
	sandboxBridge    = "orrery0"
	sandboxInterface = "eth0"
)

func netnsName(id uuid.UUID) string { return "orrery-" + id.String() }
func netnsPath(id uuid.UUID) string { return "/run/netns/" + netnsName(id) }

// sandboxNetwork is what the Agent records in the Sandbox directory for Stop
// and reconciliation (runtime.md §13).
type sandboxNetwork struct {
	HostVeth string `json:"host_veth"`
	Address  string `json:"address"`
	// Policy is the Egress Policy resolved at Create.
	Policy []egressRule `json:"policy"`
	// LinkTarget is CreateSandbox.link_target: the inbound chain is rendered
	// before any SandboxLinks names this Sandbox (node-protocol.md).
	LinkTarget bool `json:"link_target,omitempty"`
}

// recordedAddress is the address in the Sandbox's network.json, or empty when
// it has none (a Local Sandbox, or one torn down).
func recordedAddress(dataRoot string, id uuid.UUID) string {
	raw, err := os.ReadFile(filepath.Join(sandboxPath(dataRoot, id), "network.json"))
	if err != nil {
		return ""
	}
	var network sandboxNetwork
	if err := json.Unmarshal(raw, &network); err != nil {
		slog.Warn("decode network.json", "sandbox_id", id, "error", err)
	}
	return network.Address
}

// hostVethName is the host side of a Sandbox's veth pair: "veth" and the
// first 11 hex digits of its id, the most IFNAMSIZ allows.
func hostVethName(id uuid.UUID) string {
	return "veth" + strings.ReplaceAll(id.String(), "-", "")[:11]
}

// sandboxGateway is the subnet and its first address, which the bridge holds
// and every Sandbox routes through (runtime.md §13).
func sandboxGateway(subnet string) (netip.Prefix, netip.Addr, error) {
	prefix, err := netip.ParsePrefix(subnet)
	if err != nil || !prefix.Addr().Is4() {
		return netip.Prefix{}, netip.Addr{}, fmt.Errorf("sandbox subnet %q is not an IPv4 prefix", subnet)
	}
	prefix = prefix.Masked()
	return prefix, prefix.Addr().Next(), nil
}

// broadcastAddr is the last address of an IPv4 prefix.
func broadcastAddr(prefix netip.Prefix) netip.Addr {
	a := prefix.Masked().Addr().As4()
	binary.BigEndian.PutUint32(a[:], binary.BigEndian.Uint32(a[:])|(1<<(32-prefix.Bits())-1))
	return netip.AddrFrom4(a)
}

// allocateAddress hands id the next free address after the last one handed
// out, wrapping around the subnet. Taken means recorded in a network.json in
// the data root, held in a Sandbox netns on the host (Sandboxes from before
// an Agent restart or upgrade, and other Agents'), or handed out by this
// Agent and not yet released (runtime.md §13).
func (d *containerdDriver) allocateAddress(id uuid.UUID) (netip.Addr, error) {
	subnet, gateway, err := sandboxGateway(d.subnet)
	if err != nil {
		return netip.Addr{}, err
	}
	d.networkMu.Lock()
	defer d.networkMu.Unlock()
	recorded, err := recordedNetworks(d.r.dataRoot)
	if err != nil {
		return netip.Addr{}, err
	}
	taken := liveAddresses()
	for _, network := range recorded {
		if addr, err := netip.ParseAddr(network.Address); err == nil {
			taken[addr] = true
		}
	}
	if d.addrs == nil {
		d.addrs = map[netip.Addr]uuid.UUID{}
	}
	last := broadcastAddr(subnet).Prev()
	addr := d.lastAddr
	for range 1 << (32 - subnet.Bits()) {
		if addr = addr.Next(); !addr.IsValid() || addr.Compare(gateway) <= 0 || addr.Compare(last) > 0 {
			addr = gateway.Next()
		}
		if _, held := d.addrs[addr]; !held && !taken[addr] && subnet.Contains(addr) {
			d.lastAddr, d.addrs[addr] = addr, id
			return addr, nil
		}
	}
	return netip.Addr{}, fmt.Errorf("no free address in %s", subnet)
}

// releaseAddress returns id's address once its veth is gone.
func (d *containerdDriver) releaseAddress(id uuid.UUID) {
	d.networkMu.Lock()
	defer d.networkMu.Unlock()
	for addr, owner := range d.addrs {
		if owner == id {
			delete(d.addrs, addr)
		}
	}
}

// netipNet is prefix's address with its mask, as netlink takes it.
func netipNet(prefix netip.Prefix) *net.IPNet {
	return &net.IPNet{IP: prefix.Addr().AsSlice(), Mask: net.CIDRMask(prefix.Bits(), 32)}
}

// ensureBridge brings up orrery0 holding the gateway, with forwarding on.
func ensureBridge(gateway netip.Prefix) (netlink.Link, error) {
	attrs := netlink.NewLinkAttrs()
	attrs.Name = sandboxBridge
	if err := netlink.LinkAdd(&netlink.Bridge{LinkAttrs: attrs}); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("create %s: %w", sandboxBridge, err)
	}
	bridge, err := netlink.LinkByName(sandboxBridge)
	if err != nil {
		return nil, fmt.Errorf("find %s: %w", sandboxBridge, err)
	}
	addr := &netlink.Addr{IPNet: netipNet(gateway)}
	if err := netlink.AddrReplace(bridge, addr); err != nil {
		return nil, fmt.Errorf("address %s on %s: %w", gateway, sandboxBridge, err)
	}
	if err := netlink.LinkSetUp(bridge); err != nil {
		return nil, fmt.Errorf("bring up %s: %w", sandboxBridge, err)
	}
	if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0o644); err != nil {
		return nil, fmt.Errorf("enable IPv4 forwarding: %w", err)
	}
	return bridge, nil
}

// createNetns creates and bind-mounts the netns at netnsPath. netns.NewNamed
// moves the calling thread into it, so it runs on a locked thread that is
// retired with its goroutine instead of going back to the scheduler.
func createNetns(name string) (netns.NsHandle, error) {
	type result struct {
		ns  netns.NsHandle
		err error
	}
	done := make(chan result, 1)
	go func() {
		runtime.LockOSThread()
		ns, err := netns.NewNamed(name)
		done <- result{ns, err}
	}()
	r := <-done
	return r.ns, r.err
}

// plugSandbox creates the netns and the veth pair from the bridge to its
// eth0, and gives the netns lo, the address and the default route.
func plugSandbox(id uuid.UUID, bridge netlink.Link, hostVeth string, addr netip.Prefix, gateway netip.Addr) error {
	ns, err := createNetns(netnsName(id))
	if err != nil {
		return fmt.Errorf("create netns %s: %w", netnsName(id), err)
	}
	defer ns.Close() //nolint:errcheck // the bind mount keeps the netns
	attrs := netlink.NewLinkAttrs()
	attrs.Name, attrs.MasterIndex = hostVeth, bridge.Attrs().Index
	if err := netlink.LinkAdd(&netlink.Veth{LinkAttrs: attrs, PeerName: sandboxInterface, PeerNamespace: netlink.NsFd(ns)}); err != nil {
		return fmt.Errorf("create veth %s: %w", hostVeth, err)
	}
	host, err := netlink.LinkByName(hostVeth)
	if err == nil {
		err = netlink.LinkSetUp(host)
	}
	if err != nil {
		return fmt.Errorf("bring up %s: %w", hostVeth, err)
	}
	h, err := netlink.NewHandleAt(ns)
	if err != nil {
		return fmt.Errorf("netlink in netns %s: %w", netnsName(id), err)
	}
	defer h.Close()
	lo, err := h.LinkByName("lo")
	if err == nil {
		err = h.LinkSetUp(lo)
	}
	if err != nil {
		return fmt.Errorf("bring up lo: %w", err)
	}
	eth, err := h.LinkByName(sandboxInterface)
	if err != nil {
		return fmt.Errorf("find %s: %w", sandboxInterface, err)
	}
	if err := h.AddrAdd(eth, &netlink.Addr{IPNet: netipNet(addr)}); err != nil {
		return fmt.Errorf("address %s on %s: %w", addr, sandboxInterface, err)
	}
	if err := h.LinkSetUp(eth); err != nil {
		return fmt.Errorf("bring up %s: %w", sandboxInterface, err)
	}
	if err := h.RouteAdd(&netlink.Route{LinkIndex: eth.Attrs().Index, Gw: net.IP(gateway.AsSlice())}); err != nil {
		return fmt.Errorf("default route via %s: %w", gateway, err)
	}
	return nil
}

// netnsMarker records in the data root that this Agent created id's netns,
// before it holds an address, so a sweep after a crash finds it.
func netnsMarker(dataRoot string, id uuid.UUID) string {
	return filepath.Join(dataRoot, "netns", id.String())
}

// orphanNetns is every netns marker among names with no network.json under
// the data root: a probe, Create or teardown an Agent crash cut short.
func orphanNetns(names []string, dataRoot string) []uuid.UUID {
	var orphans []uuid.UUID
	for _, name := range names {
		id, err := uuid.Parse(name)
		if err != nil {
			continue
		}
		if _, err := os.Stat(filepath.Join(sandboxPath(dataRoot, id), "network.json")); errors.Is(err, os.ErrNotExist) {
			orphans = append(orphans, id)
		}
	}
	return orphans
}

// sweepOrphanNetworks deletes the veth and netns of each of this Agent's
// orphans at startup, releasing their addresses (runtime.md §13). Another
// Agent's netns on the same host is never touched.
func sweepOrphanNetworks(dataRoot string) {
	entries, err := os.ReadDir(filepath.Join(dataRoot, "netns"))
	if err != nil {
		return // no netns created yet
	}
	names := make([]string, len(entries))
	for i, entry := range entries {
		names[i] = entry.Name()
	}
	for _, id := range orphanNetns(names, dataRoot) {
		if link, err := netlink.LinkByName(hostVethName(id)); err == nil {
			_ = netlink.LinkDel(link) // deleting the netns below takes the pair with it anyway
		}
		if _, err := os.Stat(netnsPath(id)); err == nil {
			if err := netns.DeleteNamed(netnsName(id)); err != nil {
				slog.Warn("delete an orphan Sandbox netns", "sandbox_id", id, "error", err)
				continue
			}
		}
		_ = os.Remove(netnsMarker(dataRoot, id))
	}
}

// liveAddresses is every IPv4 address in a Sandbox netns on this host: this
// Agent's, one from before an upgrade, or another Agent's on the same bridge.
// shortcut: two Agents allocating at the same instant can still race; add a
// host-wide lock if Nodes ever share a host outside e2e.
func liveAddresses() map[netip.Addr]bool {
	live := map[netip.Addr]bool{}
	entries, _ := os.ReadDir(filepath.Dir(netnsPath(uuid.Nil)))
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "orrery-") {
			continue
		}
		ns, err := netns.GetFromName(entry.Name())
		if err != nil {
			continue
		}
		h, err := netlink.NewHandleAt(ns)
		ns.Close() //nolint:errcheck // the handle holds its own sockets
		if err != nil {
			continue
		}
		addrs, err := h.AddrList(nil, netlink.FAMILY_V4)
		h.Close()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if addr, ok := netip.AddrFromSlice(a.IP); ok {
				live[addr.Unmap()] = true
			}
		}
	}
	return live
}

// setupNetwork builds the netns at netnsPath(id) on the bridge. It writes
// into dir network.json (veth, address, Egress Policy and Link target flag,
// which a render reads) and the files mounted over /etc (runtime.md §2, §13).
// The caller renders.
func (d *containerdDriver) setupNetwork(id uuid.UUID, dir string, policy []egressRule, linkTarget bool) (sandboxNetwork, error) {
	subnet, gateway, err := sandboxGateway(d.subnet)
	if err != nil {
		return sandboxNetwork{}, err
	}
	bridge, err := ensureBridge(netip.PrefixFrom(gateway, subnet.Bits()))
	if err != nil {
		return sandboxNetwork{}, err
	}
	addr, err := d.allocateAddress(id)
	if err != nil {
		return sandboxNetwork{}, err
	}
	network := sandboxNetwork{HostVeth: hostVethName(id), Address: addr.String(), Policy: policy, LinkTarget: linkTarget}
	if err := os.MkdirAll(filepath.Dir(netnsMarker(d.r.dataRoot, id)), 0o755); err != nil {
		return sandboxNetwork{}, err
	}
	if err := os.WriteFile(netnsMarker(d.r.dataRoot, id), nil, 0o644); err != nil {
		return sandboxNetwork{}, fmt.Errorf("record netns marker: %w", err)
	}
	if err := plugSandbox(id, bridge, network.HostVeth, netip.PrefixFrom(addr, subnet.Bits()), gateway); err != nil {
		return sandboxNetwork{}, err
	}
	raw, _ := json.Marshal(network)
	if err := os.WriteFile(filepath.Join(dir, "network.json"), raw, 0o644); err != nil {
		return sandboxNetwork{}, fmt.Errorf("record network.json: %w", err)
	}
	if err := writeSandboxEtc(filepath.Join(dir, "etc"), id.String(), network.Address); err != nil {
		return sandboxNetwork{}, err
	}
	return network, nil
}

// teardownNetwork drops the Sandbox's recorded network and re-renders the
// tables without its chain, then deletes the veth, releases the address and
// deletes the netns; every step is idempotent.
func (d *containerdDriver) teardownNetwork(ctx context.Context, id uuid.UUID, dir string) error {
	hostVeth, address := hostVethName(id), ""
	// A Sandbox from before #205 has the bridge plugin's veth name.
	if raw, err := os.ReadFile(filepath.Join(dir, "network.json")); err == nil {
		var network sandboxNetwork
		if json.Unmarshal(raw, &network) == nil {
			address = network.Address
			if network.HostVeth != "" {
				hostVeth = network.HostVeth
			}
		}
	}
	if err := os.Remove(filepath.Join(dir, "network.json")); err != nil && !os.IsNotExist(err) {
		return err
	}
	d.setProbe(id, nil)
	if err := d.applyNetwork(ctx); err != nil {
		return err
	}
	link, err := netlink.LinkByName(hostVeth)
	if err == nil {
		err = netlink.LinkDel(link)
	}
	if err != nil && !errors.As(err, &netlink.LinkNotFoundError{}) {
		return fmt.Errorf("delete veth %s: %w", hostVeth, err)
	}
	flushConntrack(address)
	d.releaseAddress(id)
	if _, err := os.Stat(netnsPath(id)); !errors.Is(err, os.ErrNotExist) {
		if err := netns.DeleteNamed(netnsName(id)); err != nil {
			return fmt.Errorf("delete netns %s: %w", netnsName(id), err)
		}
	}
	if err := os.Remove(netnsMarker(d.r.dataRoot, id)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// setProbe records a self-check probe's network, or forgets it when network
// is nil. Probes live only in memory, so the first render after a crash
// drops a leftover probe's chain.
func (d *containerdDriver) setProbe(id uuid.UUID, network *sandboxNetwork) {
	d.networkMu.Lock()
	defer d.networkMu.Unlock()
	if network == nil {
		delete(d.probes, id)
		return
	}
	if d.probes == nil {
		d.probes = map[uuid.UUID]sandboxNetwork{}
	}
	d.probes[id] = *network
}

// setProbeLink records the Link self-check's Link, or forgets it when link
// is nil; like the probes, it lives only in memory.
func (d *containerdDriver) setProbeLink(link *nodev1.SandboxLink) {
	d.networkMu.Lock()
	d.probeLink = link
	d.networkMu.Unlock()
}

// setLinks replaces the SandboxLinks set, records it for a restart, and
// re-renders (runtime.md §13). A failed record still renders the new set.
func (d *containerdDriver) setLinks(ctx context.Context, links []*nodev1.SandboxLink) error {
	d.networkMu.Lock()
	d.links = links
	saveErr := saveSandboxLinks(d.r.dataRoot, links)
	d.networkMu.Unlock()
	return errors.Join(d.applyNetwork(ctx), saveErr)
}

func sandboxLinksPath(dataRoot string) string { return filepath.Join(dataRoot, "sandbox-links.pb") }

// saveSandboxLinks records the last SandboxLinks set, atomically.
func saveSandboxLinks(dataRoot string, links []*nodev1.SandboxLink) error {
	raw, err := proto.Marshal(&nodev1.SandboxLinks{Links: links})
	if err != nil {
		return fmt.Errorf("encode SandboxLinks: %w", err)
	}
	tmp := sandboxLinksPath(dataRoot) + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return fmt.Errorf("record SandboxLinks: %w", err)
	}
	return os.Rename(tmp, sandboxLinksPath(dataRoot))
}

// loadSandboxLinks is the last recorded SandboxLinks set, or none. A target
// recorded with link_target still gets a drop-all inbound chain without it.
func loadSandboxLinks(dataRoot string) []*nodev1.SandboxLink {
	raw, err := os.ReadFile(sandboxLinksPath(dataRoot))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	var set nodev1.SandboxLinks
	if err == nil {
		err = proto.Unmarshal(raw, &set)
	}
	if err != nil {
		slog.Warn("read the recorded SandboxLinks", "error", err)
		return nil
	}
	return set.Links
}

// applyNetwork renders the bridge table orrery from every Sandbox's recorded
// network.json plus the running probes and the SandboxLinks set, and replaces
// each table in one netlink batch (runtime.md §13).
func (d *containerdDriver) applyNetwork(ctx context.Context) error {
	d.networkMu.Lock()
	defer d.networkMu.Unlock()
	subnet, _, err := sandboxGateway(d.subnet)
	if err != nil {
		return err
	}
	sandboxes, err := recordedNetworks(d.r.dataRoot)
	if err != nil {
		return err
	}
	state := networkState{sandboxes: sandboxes, subnet: subnet, links: d.links}
	for id, network := range d.probes {
		state.sandboxes[id] = network
	}
	if d.probeLink != nil {
		state.links = append(slices.Clone(d.links), d.probeLink)
	}
	return applyTables(renderNetwork(state))
}

// recordedNetworks is every Sandbox's network.json in the data root.
func recordedNetworks(dataRoot string) (map[uuid.UUID]sandboxNetwork, error) {
	paths, err := filepath.Glob(filepath.Join(dataRoot, "sandboxes", "*", "network.json"))
	if err != nil {
		return nil, err
	}
	networks := map[uuid.UUID]sandboxNetwork{}
	for _, path := range paths {
		id, err := uuid.Parse(filepath.Base(filepath.Dir(path)))
		if err != nil {
			continue
		}
		raw, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue // torn down since the glob
		}
		if err != nil {
			return nil, err
		}
		var network sandboxNetwork
		if err := json.Unmarshal(raw, &network); err != nil {
			return nil, fmt.Errorf("decode %s: %w", path, err)
		}
		networks[id] = network
	}
	return networks, nil
}

// hostResolvConf is the host's upstream resolver list: systemd-resolved's
// upstream file when present, else /etc/resolv.conf (runtime.md §2).
func hostResolvConf() string {
	for _, path := range []string{"/run/systemd/resolve/resolv.conf", "/etc/resolv.conf"} {
		if raw, err := os.ReadFile(path); err == nil {
			return string(raw)
		}
	}
	return ""
}

// sandboxResolvConf drops loopback nameservers: the Sandbox's loopback is not
// the host's.
func sandboxResolvConf(host string) string {
	var out strings.Builder
	for _, line := range strings.Split(strings.TrimSuffix(host, "\n"), "\n") {
		if fields := strings.Fields(line); len(fields) >= 2 && fields[0] == "nameserver" {
			if addr, err := netip.ParseAddr(fields[1]); err == nil && addr.IsLoopback() {
				continue
			}
		}
		if line == "" && out.Len() == 0 {
			continue
		}
		out.WriteString(line + "\n")
	}
	return out.String()
}

func writeSandboxEtc(dir, hostname, address string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	files := map[string]string{
		"resolv.conf": sandboxResolvConf(hostResolvConf()),
		"hosts":       "127.0.0.1\tlocalhost\n::1\tlocalhost ip6-localhost ip6-loopback\n" + address + "\t" + hostname + "\n",
		"hostname":    hostname + "\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// flushConntrack forgets the connections of a released address, so the next
// Sandbox given it does not inherit them through ct state established
// (runtime.md §13). Best effort: without nf_conntrack there is nothing to
// forget.
func flushConntrack(address string) {
	ip := net.ParseIP(address).To4()
	if ip == nil {
		return
	}
	var from, to netlink.ConntrackFilter
	if from.AddIP(netlink.ConntrackOrigSrcIP, ip) != nil || to.AddIP(netlink.ConntrackOrigDstIP, ip) != nil {
		return
	}
	if _, err := netlink.ConntrackDeleteFilters(netlink.ConntrackTable, unix.AF_INET, &from, &to); err != nil {
		slog.Warn("flush the conntrack entries of a released address", "address", address, "error", err)
	}
}
