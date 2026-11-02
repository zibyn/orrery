package agent

import (
	"net/netip"
	"testing"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
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
	rule := chain.rules[0]
	if rule.saddr != subnet || rule.daddr != subnet || !rule.notDaddr || rule.oif != meshInterface || !rule.notOif || rule.verdict != "masquerade" {
		t.Errorf("rule = %+v, want ip saddr %s ip daddr != %[2]s oifname != %q masquerade", rule, subnet, meshInterface)
	}
	if got := nftChainOf(&nftables.Table{Family: nftables.TableFamilyIPv4}, chain); got.Type != nftables.ChainTypeNAT || *got.Priority != *nftables.ChainPriorityNATSource {
		t.Errorf("chain = %+v, want type nat priority srcnat", got)
	}
	exprs := rule.exprs(nftables.TableFamilyIPv4)
	if len(exprs) != 1 {
		t.Fatalf("exprs = %d rules, want 1", len(exprs))
	}
	if _, ok := exprs[0][len(exprs[0])-1].(*expr.Masq); !ok {
		t.Errorf("rule ends in %T, want masquerade", exprs[0][len(exprs[0])-1])
	}
}
