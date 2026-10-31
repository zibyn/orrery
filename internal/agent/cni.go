package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/google/uuid"

	nodev1 "github.com/zibyn/orrery/gen/node/v1"
)

const (
	// shortcut: the standard CNI plugin directory; make it a flag when a
	// Node installs plugins elsewhere.
	cniBinDir     = "/opt/cni/bin"
	sandboxBridge = "orrery0"
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
}

// recordedAddress is the address in the Sandbox's network.json, or empty when
// it has none (a Local Sandbox, or one torn down).
func recordedAddress(dataRoot string, id uuid.UUID) string {
	raw, err := os.ReadFile(filepath.Join(sandboxPath(dataRoot, id), "network.json"))
	if err != nil {
		return ""
	}
	var network sandboxNetwork
	_ = json.Unmarshal(raw, &network)
	return network.Address
}

func (d *containerdDriver) bridgeConf() []byte {
	conf, _ := json.Marshal(map[string]any{
		"cniVersion": "1.0.0",
		"name":       "orrery",
		"type":       "bridge",
		"bridge":     sandboxBridge,
		"isGateway":  true,
		// orrery_nat masquerades instead: CNI's rule would also rewrite
		// traffic to other Nodes' Sandbox subnets (runtime.md §13).
		"ipMasq": false,
		"ipam": map[string]any{
			"type":   "host-local",
			"ranges": [][]map[string]string{{{"subnet": d.subnet}}},
			"routes": []map[string]string{{"dst": "0.0.0.0/0"}},
		},
	})
	return conf
}

var loopbackConf = []byte(`{"cniVersion":"1.0.0","name":"orrery-lo","type":"loopback"}`)

// cniExec calls one CNI plugin directly (CNI spec, "Execution protocol").
func cniExec(ctx context.Context, plugin, command string, conf []byte, id uuid.UUID, ifname string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, filepath.Join(cniBinDir, plugin))
	cmd.Env = append(os.Environ(),
		"CNI_COMMAND="+command,
		"CNI_CONTAINERID="+id.String(),
		"CNI_NETNS="+netnsPath(id),
		"CNI_IFNAME="+ifname,
		"CNI_PATH="+cniBinDir)
	cmd.Stdin = bytes.NewReader(conf)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("CNI %s %s: %w: %s%s", plugin, command, err, out, stderr.String())
	}
	return out, nil
}

// parseCNIResult picks the host-side veth and the Sandbox address out of a
// bridge plugin result: the veth is the interface outside any netns that is
// not the bridge itself.
func parseCNIResult(raw []byte, bridge string) (sandboxNetwork, error) {
	var result struct {
		Interfaces []struct {
			Name    string `json:"name"`
			Sandbox string `json:"sandbox"`
		} `json:"interfaces"`
		IPs []struct {
			Address string `json:"address"`
		} `json:"ips"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return sandboxNetwork{}, fmt.Errorf("decode CNI result: %w", err)
	}
	var network sandboxNetwork
	for _, iface := range result.Interfaces {
		if iface.Sandbox == "" && iface.Name != bridge {
			network.HostVeth = iface.Name
		}
	}
	if len(result.IPs) > 0 {
		if prefix, err := netip.ParsePrefix(result.IPs[0].Address); err == nil {
			network.Address = prefix.Addr().String()
		}
	}
	if network.HostVeth == "" || network.Address == "" {
		return sandboxNetwork{}, fmt.Errorf("CNI result has no host veth or address: %s", raw)
	}
	return network, nil
}

// setupNetwork builds the netns at netnsPath(id) on the bridge, and writes
// into dir network.json, the veth, address and Egress Policy a render
// filters it by, and the files mounted over /etc (runtime.md §2, §13). The
// caller renders.
func (d *containerdDriver) setupNetwork(ctx context.Context, id uuid.UUID, dir string, policy []egressRule) (sandboxNetwork, error) {
	if _, err := runLocalCommand(ctx, "ip", "netns", "add", netnsName(id)); err != nil {
		return sandboxNetwork{}, err
	}
	if _, err := cniExec(ctx, "loopback", "ADD", loopbackConf, id, "lo"); err != nil {
		return sandboxNetwork{}, err
	}
	out, err := cniExec(ctx, "bridge", "ADD", d.bridgeConf(), id, "eth0")
	if err != nil {
		return sandboxNetwork{}, err
	}
	network, err := parseCNIResult(out, sandboxBridge)
	if err != nil {
		return sandboxNetwork{}, err
	}
	network.Policy = policy
	raw, _ := json.Marshal(network)
	if err := os.WriteFile(filepath.Join(dir, "network.json"), raw, 0o644); err != nil {
		return sandboxNetwork{}, err
	}
	if err := writeSandboxEtc(filepath.Join(dir, "etc"), id.String(), network.Address); err != nil {
		return sandboxNetwork{}, err
	}
	return network, nil
}

// teardownNetwork drops the Sandbox's recorded network and re-renders the
// table without its chain, then CNI DEL and the netns; every step is
// idempotent.
func (d *containerdDriver) teardownNetwork(ctx context.Context, id uuid.UUID, dir string) error {
	if err := os.Remove(filepath.Join(dir, "network.json")); err != nil && !os.IsNotExist(err) {
		return err
	}
	d.setProbe(id, nil)
	if err := d.applyNetwork(ctx); err != nil {
		return err
	}
	// The bridge plugin's DEL tolerates a missing netns and still releases
	// the address.
	if _, err := cniExec(ctx, "bridge", "DEL", d.bridgeConf(), id, "eth0"); err != nil {
		return err
	}
	if _, err := os.Stat(netnsPath(id)); !errors.Is(err, os.ErrNotExist) {
		if _, err := runLocalCommand(ctx, "ip", "netns", "delete", netnsName(id)); err != nil {
			return err
		}
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

// setLinks replaces the SandboxLinks set and re-renders (runtime.md §13).
func (d *containerdDriver) setLinks(ctx context.Context, links []*nodev1.SandboxLink) error {
	d.networkMu.Lock()
	d.links = links
	d.networkMu.Unlock()
	return d.applyNetwork(ctx)
}

// applyNetwork renders the bridge table orrery from every Sandbox's recorded
// network.json plus the running probes and the SandboxLinks set, and replaces
// it in one nft transaction (runtime.md §13).
func (d *containerdDriver) applyNetwork(ctx context.Context) error {
	d.networkMu.Lock()
	defer d.networkMu.Unlock()
	state := networkState{sandboxes: map[uuid.UUID]sandboxNetwork{}, links: d.links}
	subnet, err := netip.ParsePrefix(d.subnet)
	if err != nil {
		return fmt.Errorf("sandbox subnet: %w", err)
	}
	state.subnet = subnet.Masked()
	paths, err := filepath.Glob(filepath.Join(d.r.dataRoot, "sandboxes", "*", "network.json"))
	if err != nil {
		return err
	}
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
			return err
		}
		var network sandboxNetwork
		if err := json.Unmarshal(raw, &network); err != nil {
			return fmt.Errorf("decode %s: %w", path, err)
		}
		state.sandboxes[id] = network
	}
	for id, network := range d.probes {
		state.sandboxes[id] = network
	}
	if d.probeLink != nil {
		state.links = append(slices.Clone(d.links), d.probeLink)
	}
	return nft(ctx, nftScript(renderNetwork(state)), "-f", "-")
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
