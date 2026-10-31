package agent

import (
	"encoding/json"
	"net/netip"
	"testing"

	"github.com/google/uuid"

	"github.com/zibyn/orrery/internal/nodeproto"
)

// packet is what the evaluator matches the rendered tables against. proto is
// tcp, udp, icmp or arp.
type packet struct {
	hook        string
	iif, oif    string
	src, dst    string
	proto       string
	port        uint16
	established bool
}

// evaluate walks the rendered tables the way nftables does: every table's
// base chain on the hook runs in turn, accept ends that base chain, drop is
// final, and a base chain that falls through takes its policy accept.
func evaluate(t *testing.T, tables []nftTable, p packet) bool {
	t.Helper()
	for _, table := range tables {
		chains := map[string]nftChain{}
		for _, c := range table.chains {
			chains[c.name] = c
		}
		var run func(c nftChain, depth int) string
		run = func(c nftChain, depth int) string {
			if depth > 16 {
				t.Fatalf("chain %s: jump loop", c.name)
			}
			for _, r := range c.rules {
				if !ruleMatches(t, r, p) {
					continue
				}
				switch r.verdict {
				case "accept", "drop":
					return r.verdict
				}
				target, ok := chains[r.verdict]
				if !ok {
					t.Fatalf("chain %s jumps to missing chain %q", c.name, r.verdict)
				}
				if v := run(target, depth+1); v != "" {
					return v
				}
			}
			return ""
		}
		for _, c := range table.chains {
			if c.hook == p.hook && run(c, 0) == "drop" {
				return false
			}
		}
	}
	return true
}

func ruleMatches(t *testing.T, r nftRule, p packet) bool {
	t.Helper()
	ip := p.proto != "arp"
	switch {
	case r.iif != "" && r.iif != p.iif,
		r.oif != "" && r.oif != p.oif,
		r.established && !p.established,
		r.arp && p.proto != "arp",
		r.notIP && ip,
		r.daddr.IsValid() && (!ip || !r.daddr.Contains(netip.MustParseAddr(p.dst))),
		r.port != 0 && ((p.proto != "tcp" && p.proto != "udp") || r.port != p.port):
		return false
	}
	return true
}

func TestRenderNetworkFiltersEachSandboxByItsEgressPolicy(t *testing.T) {
	recommendedDeny := []string{"169.254.169.254/32", "10.213.0.0/16", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"}
	rulesAllow := []string{"10.0.0.5/32:443"}
	policy := func(mode egressMode) []egressRule {
		p, err := resolveEgressPolicy(rulesAllow, recommendedDeny, mode)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	open, none, list := uuid.New(), uuid.New(), uuid.New()
	tables := renderNetwork(networkState{sandboxes: map[uuid.UUID]sandboxNetwork{
		open: {HostVeth: "vethopen", Address: "10.213.0.2", Policy: policy(egressMode{name: nodeproto.EgressOpen})},
		none: {HostVeth: "vethnone", Address: "10.213.0.3", Policy: policy(egressMode{name: nodeproto.EgressNone})},
		list: {HostVeth: "vethlist", Address: "10.213.0.4", Policy: policy(egressMode{
			name: nodeproto.EgressAllowlist, allow: []string{"1.1.1.0/24:443", "192.168.1.10/32"}})},
	}})

	out := func(veth, src, dst, proto string, port uint16) packet {
		return packet{hook: "input", iif: veth, oif: "eth0", src: src, dst: dst, proto: proto, port: port}
	}
	tests := []struct {
		name   string
		p      packet
		accept bool
	}{
		{"open reaches the internet", out("vethopen", "10.213.0.2", "8.8.8.8", "tcp", 443), true},
		{"open still obeys Rules deny", out("vethopen", "10.213.0.2", "169.254.169.254", "tcp", 80), false},
		{"open cannot reach the gateway past deny", out("vethopen", "10.213.0.2", "10.213.0.1", "tcp", 28000), false},
		{"Rules allow overrides deny", out("vethopen", "10.213.0.2", "10.0.0.5", "tcp", 443), true},
		{"open reaches another Sandbox only if Rules allow",
			packet{hook: "forward", iif: "vethopen", oif: "vethnone", src: "10.213.0.2", dst: "10.213.0.3", proto: "tcp", port: 80}, false},
		{"none drops the internet", out("vethnone", "10.213.0.3", "8.8.8.8", "tcp", 443), false},
		{"none drops ICMP", out("vethnone", "10.213.0.3", "8.8.8.8", "icmp", 0), false},
		{"none keeps what Rules allow", out("vethnone", "10.213.0.3", "10.0.0.5", "tcp", 443), true},
		{"none passes ARP", out("vethnone", "10.213.0.3", "", "arp", 0), true},
		{"none passes replies", packet{hook: "input", iif: "vethnone", src: "10.213.0.3", dst: "8.8.8.8",
			proto: "tcp", port: 443, established: true}, true},
		{"allowlist reaches a listed port", out("vethlist", "10.213.0.4", "1.1.1.1", "tcp", 443), true},
		{"allowlist drops an unlisted port", out("vethlist", "10.213.0.4", "1.1.1.1", "tcp", 80), false},
		{"allowlist drops an unlisted address", out("vethlist", "10.213.0.4", "8.8.8.8", "udp", 53), false},
		{"Rules deny wins over the allowlist", out("vethlist", "10.213.0.4", "192.168.1.10", "tcp", 22), false},
		{"traffic not from a Sandbox veth is untouched", out("eth0", "203.0.113.9", "10.213.0.3", "tcp", 22), true},
		{"a veth with no Sandbox is untouched", out("vethgone", "10.213.0.9", "8.8.8.8", "tcp", 443), true},
	}
	for _, tt := range tests {
		if got := evaluate(t, tables, tt.p); got != tt.accept {
			t.Errorf("%s: accepted = %v, want %v", tt.name, got, tt.accept)
		}
	}
}

func TestRenderNetworkDropsAStoppedSandboxsChain(t *testing.T) {
	none, _ := resolveEgressPolicy(nil, nil, egressMode{name: nodeproto.EgressNone})
	id := uuid.New()
	p := packet{hook: "forward", iif: "vethx", src: "10.213.0.2", dst: "8.8.8.8", proto: "tcp", port: 443}
	running := renderNetwork(networkState{sandboxes: map[uuid.UUID]sandboxNetwork{
		id: {HostVeth: "vethx", Address: "10.213.0.2", Policy: none}}})
	if evaluate(t, running, p) {
		t.Fatal("running Mode none Sandbox reached the internet")
	}
	stopped := renderNetwork(networkState{})
	for _, table := range stopped {
		for _, c := range table.chains {
			if c.name == egressChain(id) {
				t.Fatalf("table %s still has chain %s after Stop", table.name, c.name)
			}
		}
	}
}

// After an Agent restart the table is rebuilt from network.json, so the
// recorded Egress Policy must survive the round trip.
func TestRecordedNetworkRebuildsTheSamePolicy(t *testing.T) {
	policy, err := resolveEgressPolicy([]string{"10.0.0.5/32:443"}, []string{"10.0.0.0/8"},
		egressMode{name: nodeproto.EgressAllowlist, allow: []string{"1.1.1.0/24:443"}})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(sandboxNetwork{HostVeth: "vethx", Address: "10.213.0.2", Policy: policy})
	if err != nil {
		t.Fatal(err)
	}
	var recorded sandboxNetwork
	if err := json.Unmarshal(raw, &recorded); err != nil {
		t.Fatal(err)
	}
	tables := renderNetwork(networkState{sandboxes: map[uuid.UUID]sandboxNetwork{uuid.New(): recorded}})
	for _, tt := range []struct {
		dst    string
		port   uint16
		accept bool
	}{{"10.0.0.5", 443, true}, {"10.0.0.6", 443, false}, {"1.1.1.1", 443, true}, {"1.1.1.1", 80, false}} {
		p := packet{hook: "input", iif: "vethx", src: "10.213.0.2", dst: tt.dst, proto: "tcp", port: tt.port}
		if got := evaluate(t, tables, p); got != tt.accept {
			t.Errorf("%s:%d accepted = %v, want %v", tt.dst, tt.port, got, tt.accept)
		}
	}
}
