package agent

import (
	"encoding/json"
	"net/netip"
	"testing"

	"github.com/google/uuid"

	nodev1 "github.com/zibyn/orrery/gen/node/v1"
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
		r.saddr.IsValid() && (!ip || !r.saddr.Contains(netip.MustParseAddr(p.src))),
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

// runtime.md §13: a Link target's inbound chain admits only its linked
// sources on the linked ports, ahead of every Egress Policy, and also covers
// what the bridge itself sends (the Node's own processes, the Mesh).
func TestRenderNetworkSandboxLinks(t *testing.T) {
	policy := func(mode string) []egressRule {
		p, err := resolveEgressPolicy(nil, nil, egressMode{name: mode})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	denyRules, err := resolveEgressPolicy(nil, []string{"10.213.0.0/16"}, egressMode{name: nodeproto.EgressOpen})
	if err != nil {
		t.Fatal(err)
	}
	target, source, ruled, open, other := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	tables := renderNetwork(networkState{
		sandboxes: map[uuid.UUID]sandboxNetwork{
			target: {HostVeth: "vethtarget", Address: "10.213.0.2", Policy: policy(nodeproto.EgressOpen)},
			source: {HostVeth: "vethsource", Address: "10.213.0.3", Policy: policy(nodeproto.EgressNone)},
			ruled:  {HostVeth: "vethruled", Address: "10.213.0.4", Policy: denyRules},
			open:   {HostVeth: "vethopen", Address: "10.213.0.5", Policy: policy(nodeproto.EgressOpen)},
			other:  {HostVeth: "vethother", Address: "10.213.0.6", Policy: policy(nodeproto.EgressNone)},
		},
		links: []*nodev1.SandboxLink{
			{TargetId: target.String(), TargetAddress: "10.213.0.2", SourceId: source.String(), SourceAddress: "10.213.0.3", Ports: []uint32{8080, 9090}},
			{TargetId: target.String(), TargetAddress: "10.213.0.2", SourceId: ruled.String(), SourceAddress: "10.213.0.4", Ports: []uint32{8080}},
			// A source on another Node, routed in from the Mesh.
			{TargetId: target.String(), TargetAddress: "10.213.0.2", SourceId: uuid.NewString(), SourceAddress: "10.213.1.7"},
		},
	})
	to := func(veth, src string, port uint16) packet {
		return packet{hook: "forward", iif: veth, oif: "vethtarget", src: src, dst: "10.213.0.2", proto: "tcp", port: port}
	}
	routed := func(src string, port uint16) packet {
		return packet{hook: "output", oif: "vethtarget", src: src, dst: "10.213.0.2", proto: "tcp", port: port}
	}
	tests := []struct {
		name   string
		p      packet
		accept bool
	}{
		{"a Mode none source reaches the linked port", to("vethsource", "10.213.0.3", 8080), true},
		{"and its other linked port", to("vethsource", "10.213.0.3", 9090), true},
		{"but not a port that is not linked", to("vethsource", "10.213.0.3", 22), false},
		{"the Link wins over the Egress Rules deny", to("vethruled", "10.213.0.4", 8080), true},
		{"an open Sandbox cannot reach an unlinked target", to("vethopen", "10.213.0.5", 8080), false},
		{"the source's other traffic keeps Mode none", packet{hook: "forward", iif: "vethsource", oif: "vethother",
			src: "10.213.0.3", dst: "10.213.0.6", proto: "tcp", port: 8080}, false},
		{"the source cannot leave the Node", packet{hook: "input", iif: "vethsource", src: "10.213.0.3",
			dst: "8.8.8.8", proto: "tcp", port: 8080}, false},
		{"the target's replies pass", packet{hook: "forward", iif: "vethtarget", oif: "vethsource", src: "10.213.0.2",
			dst: "10.213.0.3", proto: "tcp", port: 40000, established: true}, true},
		{"the target still answers ARP", packet{hook: "forward", iif: "vethsource", oif: "vethtarget", proto: "arp"}, true},
		{"the Node's own processes cannot reach the target", routed("10.213.0.1", 8080), false},
		{"a linked source routed in from the Mesh is checked in output", routed("10.213.1.7", 5432), true},
		{"an unlinked address routed in is dropped", routed("10.213.1.8", 5432), false},
		{"an unlinked Sandbox is not filtered inbound", packet{hook: "output", oif: "vethother", src: "10.213.0.1",
			dst: "10.213.0.6", proto: "tcp", port: 22}, true},
	}
	for _, tt := range tests {
		if got := evaluate(t, tables, tt.p); got != tt.accept {
			t.Errorf("%s: accepted = %v, want %v", tt.name, got, tt.accept)
		}
	}
}

// A source's Link comes before its Egress Policy in its own chain, so it
// also holds where no target chain runs first (a target on another Node).
func TestRenderNetworkLinkPrecedesTheSourcesEgressPolicy(t *testing.T) {
	none, _ := resolveEgressPolicy(nil, []string{"10.213.0.0/16"}, egressMode{name: nodeproto.EgressNone})
	source := uuid.New()
	tables := renderNetwork(networkState{
		sandboxes: map[uuid.UUID]sandboxNetwork{source: {HostVeth: "vethsource", Address: "10.213.0.3", Policy: none}},
		links: []*nodev1.SandboxLink{{TargetId: uuid.NewString(), TargetAddress: "10.213.1.7",
			SourceId: source.String(), SourceAddress: "10.213.0.3", Ports: []uint32{8080}}},
	})
	out := func(dst string, port uint16) packet {
		return packet{hook: "input", iif: "vethsource", src: "10.213.0.3", dst: dst, proto: "tcp", port: port}
	}
	if !evaluate(t, tables, out("10.213.1.7", 8080)) {
		t.Error("the linked destination was dropped by Mode none and the Rules deny")
	}
	if evaluate(t, tables, out("10.213.1.7", 22)) {
		t.Error("an unlinked port of the target was accepted")
	}
	if evaluate(t, tables, out("10.213.1.8", 8080)) {
		t.Error("an unlinked address was accepted")
	}
}
