package agent

import (
	"net/netip"
	"strings"
	"testing"
)

// runtime.md §13: the Agent's own `ip orrery_nat` table masquerades Sandbox
// traffic that leaves the subnet, except into the Mesh.
func TestRenderNetworkNAT(t *testing.T) {
	subnet := netip.MustParsePrefix("10.88.0.0/16")
	tables := renderNetwork(networkState{subnet: subnet})
	var nat *nftTable
	for i := range tables {
		if tables[i].family == "ip" && tables[i].name == "orrery_nat" {
			nat = &tables[i]
		}
	}
	if nat == nil || len(nat.chains) != 1 || len(nat.chains[0].rules) != 1 {
		t.Fatalf("no single-rule orrery_nat table in %+v", tables)
	}
	chain := nat.chains[0]
	if chain.hook != "postrouting" {
		t.Errorf("hook = %q, want postrouting", chain.hook)
	}
	script := nftScript([]nftTable{*nat})
	for _, want := range []string{
		"type nat hook postrouting priority srcnat; policy accept;",
		"ip saddr 10.88.0.0/16",
		"ip daddr != 10.88.0.0/16",
		`oifname != "orrery-wg"`,
		"masquerade\n",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script lacks %q:\n%s", want, script)
		}
	}
}

// The bridge plugin no longer masquerades; orrery_nat does (runtime.md §13).
func TestBridgeConfHasNoIPMasq(t *testing.T) {
	conf := (&containerdDriver{subnet: "10.88.0.0/16"}).bridgeConf()
	if !strings.Contains(string(conf), `"ipMasq":false`) {
		t.Errorf("bridge conf = %s, want ipMasq false", conf)
	}
}
