package agent

import (
	"net/netip"
	"testing"

	"github.com/zibyn/orrery/internal/nodeproto"
)

// verdict walks the resolved policy the way the per-Sandbox chain does: the
// first matching entry decides (runtime.md §13).
func verdict(t *testing.T, policy []egressRule, dst string, proto string, port uint16) bool {
	t.Helper()
	addr := netip.MustParseAddr(dst)
	for _, rule := range policy {
		if !rule.dest.Prefix.Contains(addr) {
			continue
		}
		if rule.dest.Port != 0 && (proto == "icmp" || rule.dest.Port != port) {
			continue
		}
		return rule.accept
	}
	t.Fatalf("policy has no entry matching %s %s/%d", dst, proto, port)
	return false
}

func TestResolveEgressPolicy(t *testing.T) {
	type probe struct {
		dst    string
		proto  string
		port   uint16
		accept bool
	}
	recommendedDeny := []string{"169.254.169.254/32", "10.213.0.0/16", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"}
	tests := []struct {
		name        string
		allow, deny []string
		mode        egressMode
		probes      []probe
	}{
		{
			name: "no rules and open reaches everything",
			mode: egressMode{name: nodeproto.EgressOpen},
			probes: []probe{
				{"169.254.169.254", "tcp", 80, true},
				{"10.213.0.7", "icmp", 0, true},
				{"8.8.8.8", "udp", 53, true},
			},
		},
		{
			name: "deny drops whole ranges for every protocol",
			deny: recommendedDeny, mode: egressMode{name: nodeproto.EgressOpen},
			probes: []probe{
				{"169.254.169.254", "tcp", 80, false},
				{"10.213.0.7", "tcp", 28001, false},
				{"10.213.0.7", "icmp", 0, false},
				{"172.20.1.1", "udp", 53, false},
				{"192.168.1.1", "tcp", 22, false},
				{"8.8.8.8", "tcp", 443, true},
			},
		},
		{
			name:  "allow overrides an overlapping deny, port by port",
			allow: []string{"10.0.0.5/32:443", "10.0.0.2:53"},
			deny:  recommendedDeny, mode: egressMode{name: nodeproto.EgressOpen},
			probes: []probe{
				{"10.0.0.5", "tcp", 443, true},
				{"10.0.0.5", "udp", 443, true},
				{"10.0.0.5", "tcp", 80, false},
				{"10.0.0.5", "icmp", 0, false},
				{"10.0.0.2", "udp", 53, true},
				{"10.0.0.6", "tcp", 443, false},
			},
		},
		{
			name:  "allow without port overrides deny for every port",
			allow: []string{"10.1.0.0/16"}, deny: []string{"10.0.0.0/8"},
			mode: egressMode{name: nodeproto.EgressOpen},
			probes: []probe{
				{"10.1.2.3", "tcp", 1, true},
				{"10.1.2.3", "icmp", 0, true},
				{"10.2.0.1", "tcp", 1, false},
			},
		},
		{
			name: "a ported deny leaves other ports of the address open",
			deny: []string{"8.8.8.8/32:53"}, mode: egressMode{name: nodeproto.EgressOpen},
			probes: []probe{
				{"8.8.8.8", "udp", 53, false},
				{"8.8.8.8", "tcp", 53, false},
				{"8.8.8.8", "tcp", 443, true},
				{"8.8.8.8", "icmp", 0, true},
			},
		},
		{
			name:  "none drops everything Rules allow does not name",
			allow: []string{"10.0.0.5/32:443"}, deny: recommendedDeny,
			mode: egressMode{name: nodeproto.EgressNone},
			probes: []probe{
				{"10.0.0.5", "tcp", 443, true},
				{"8.8.8.8", "tcp", 443, false},
				{"10.0.0.5", "tcp", 80, false},
			},
		},
		{
			name:  "allowlist narrows within Rules: deny still wins over the list",
			allow: []string{"10.0.0.5/32:443"}, deny: recommendedDeny,
			mode: egressMode{name: nodeproto.EgressAllowlist, allow: []string{"1.1.1.0/24:443", "192.168.1.10/32"}},
			probes: []probe{
				{"1.1.1.1", "tcp", 443, true},
				{"1.1.1.1", "tcp", 80, false},
				{"8.8.8.8", "tcp", 443, false},
				{"192.168.1.10", "tcp", 22, false},
				{"10.0.0.5", "tcp", 443, true},
			},
		},
		{
			name: "allowlist without Rules",
			mode: egressMode{name: nodeproto.EgressAllowlist, allow: []string{"10.213.0.1/32:28000"}},
			probes: []probe{
				{"10.213.0.1", "tcp", 28000, true},
				{"10.213.0.1", "tcp", 28001, false},
				{"10.213.0.2", "tcp", 28000, false},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policy, err := resolveEgressPolicy(tt.allow, tt.deny, tt.mode)
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range tt.probes {
				if got := verdict(t, policy, p.dst, p.proto, p.port); got != p.accept {
					t.Errorf("%s %s/%d accepted = %v, want %v", p.dst, p.proto, p.port, got, p.accept)
				}
			}
		})
	}
}

func TestResolveEgressPolicyRejectsBadInput(t *testing.T) {
	for name, tt := range map[string]struct {
		allow, deny []string
		mode        egressMode
	}{
		"bad rule":            {deny: []string{"10.0.0.0/8:x"}, mode: egressMode{name: nodeproto.EgressOpen}},
		"bad allowlist entry": {mode: egressMode{name: nodeproto.EgressAllowlist, allow: []string{"nope"}}},
		"empty allowlist":     {mode: egressMode{name: nodeproto.EgressAllowlist}},
		"unknown mode":        {mode: egressMode{name: "closed"}},
	} {
		if _, err := resolveEgressPolicy(tt.allow, tt.deny, tt.mode); err == nil {
			t.Errorf("%s: resolveEgressPolicy succeeded", name)
		}
	}
}
