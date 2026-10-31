package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os/exec"
	"slices"
	"strings"

	"github.com/google/uuid"

	nodev1 "github.com/zibyn/orrery/gen/node/v1"
	"github.com/zibyn/orrery/internal/nodeproto"
)

// The Agent's own bridge-family table (runtime.md §13).
const (
	egressFamily    = "bridge"
	egressTableName = "orrery"
)

// egressMode is a Sandbox's Egress Mode: allow is set only for allowlist.
type egressMode struct {
	name  string
	allow []string
}

// egressRule is one entry of a resolved Egress Policy. The first entry that
// matches a destination decides.
type egressRule struct {
	dest   nodeproto.EgressEntry
	accept bool
}

var anywhere = nodeproto.EgressEntry{Prefix: netip.MustParsePrefix("0.0.0.0/0")}

// resolveEgressPolicy orders Rules allow, Rules deny, then the Mode
// (runtime.md §13). The last entry always matches every destination.
func resolveEgressPolicy(allow, deny []string, mode egressMode) ([]egressRule, error) {
	var policy []egressRule
	add := func(entries []string, accept bool) error {
		for _, raw := range entries {
			entry, err := nodeproto.ParseEgressEntry(raw)
			if err != nil {
				return err
			}
			policy = append(policy, egressRule{dest: entry, accept: accept})
		}
		return nil
	}
	if err := add(allow, true); err != nil {
		return nil, err
	}
	if err := add(deny, false); err != nil {
		return nil, err
	}
	switch mode.name {
	case nodeproto.EgressOpen:
		policy = append(policy, egressRule{dest: anywhere, accept: true})
	case nodeproto.EgressNone:
		policy = append(policy, egressRule{dest: anywhere})
	case nodeproto.EgressAllowlist:
		if len(mode.allow) == 0 {
			return nil, errors.New("egress mode allowlist needs a non-empty allow")
		}
		if err := add(mode.allow, true); err != nil {
			return nil, err
		}
		policy = append(policy, egressRule{dest: anywhere})
	default:
		return nil, fmt.Errorf("unknown egress mode %q", mode.name)
	}
	return policy, nil
}

// setEgressRules keeps the Egress Rules from HelloAck in memory only
// (node-protocol.md).
func (r *Runtime) setEgressRules(allow, deny []string) {
	r.egressMu.Lock()
	r.egressAllow, r.egressDeny = allow, deny
	r.egressMu.Unlock()
}

// egressModeOf reads CreateSandbox.egress; a Control Plane that sends none
// means open.
func egressModeOf(m *nodev1.EgressMode) egressMode {
	if m.GetMode() == "" {
		return egressMode{name: nodeproto.EgressOpen}
	}
	return egressMode{name: m.GetMode(), allow: m.GetAllow()}
}

// setEgressEnforced records the egress_policy this Agent last reported.
func (r *Runtime) setEgressEnforced(report nodeproto.CapabilityReport) {
	enforced := false
	for _, entry := range report.Entries {
		if entry.Name == nodeproto.CapEgressPolicy {
			enforced = entry.Value == nodeproto.ValueEnforced
		}
	}
	r.egressMu.Lock()
	r.egressEnforced = enforced
	r.egressMu.Unlock()
}

// egressRefused is the Create-time check: a Mode other than open on a Node
// that does not enforce egress_policy fails rather than running unfiltered
// (node-protocol.md).
func (r *Runtime) egressRefused(mode egressMode) error {
	r.egressMu.Lock()
	enforced := r.egressEnforced
	r.egressMu.Unlock()
	if mode.name != nodeproto.EgressOpen && !enforced {
		return fmt.Errorf("egress mode %s needs egress_policy enforced on this Node", mode.name)
	}
	return nil
}

// egressPolicy resolves mode against the Rules this Agent holds now.
func (r *Runtime) egressPolicy(mode egressMode) ([]egressRule, error) {
	r.egressMu.Lock()
	allow, deny := r.egressAllow, r.egressDeny
	r.egressMu.Unlock()
	return resolveEgressPolicy(allow, deny, mode)
}

func egressChain(id uuid.UUID) string { return "sandbox-" + id.String() }

// MarshalJSON records a resolved entry in network.json, so a render after an
// Agent restart uses the Rules held at Create (runtime.md §13).
func (r egressRule) MarshalJSON() ([]byte, error) {
	return json.Marshal(egressRuleJSON{r.dest, r.accept})
}

func (r *egressRule) UnmarshalJSON(raw []byte) error {
	var v egressRuleJSON
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	r.dest, r.accept = v.Dest, v.Accept
	return nil
}

type egressRuleJSON struct {
	Dest   nodeproto.EgressEntry `json:"dest"`
	Accept bool                  `json:"accept"`
}

// networkState is the local state the Agent-owned nftables tables are
// rendered from: every local Sandbox's recorded network.
type networkState struct {
	sandboxes map[uuid.UUID]sandboxNetwork
}

// nftTable, nftChain and nftRule are the rendered tables, before nftScript
// turns them into nft syntax. A chain with a hook is a base chain at priority
// filter with policy accept.
type nftTable struct {
	family, name string
	chains       []nftChain
}

type nftChain struct {
	name  string
	hook  string
	rules []nftRule
}

// nftRule matches on every field that is set. verdict is accept, drop, or the
// name of a chain to jump to.
type nftRule struct {
	iif, oif    string
	established bool // ct state established,related
	arp         bool // ether type arp
	notIP       bool // ether type != ip
	daddr       netip.Prefix
	port        uint16 // TCP or UDP destination port
	verdict     string
}

// renderNetwork is the whole bridge table orrery: forward and input jump on
// each Sandbox's host-side veth into its chain, which ends in its Egress
// Policy (runtime.md §13).
func renderNetwork(state networkState) []nftTable {
	ids := make([]uuid.UUID, 0, len(state.sandboxes))
	for id := range state.sandboxes {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	forward := nftChain{name: "forward", hook: "forward"}
	input := nftChain{name: "input", hook: "input"}
	table := nftTable{family: egressFamily, name: egressTableName}
	for _, id := range ids {
		network := state.sandboxes[id]
		jump := nftRule{iif: network.HostVeth, verdict: egressChain(id)}
		forward.rules = append(forward.rules, jump)
		input.rules = append(input.rules, jump)
		chain := nftChain{name: egressChain(id), rules: []nftRule{
			{established: true, verdict: "accept"},
			{arp: true, verdict: "accept"},
			{notIP: true, verdict: "drop"},
		}}
		for _, rule := range network.Policy {
			verdict := "drop"
			if rule.accept {
				verdict = "accept"
			}
			chain.rules = append(chain.rules, nftRule{daddr: rule.dest.Prefix, port: rule.dest.Port, verdict: verdict})
		}
		table.chains = append(table.chains, chain)
	}
	table.chains = append([]nftChain{forward, input}, table.chains...)
	return []nftTable{table}
}

// nftScript replaces each table whole in one nft transaction: adding first
// makes the delete succeed when the table does not exist yet.
func nftScript(tables []nftTable) string {
	var script strings.Builder
	for _, table := range tables {
		name := table.family + " " + table.name
		fmt.Fprintf(&script, "add table %s\ndelete table %s\ntable %s {\n", name, name, name)
		for _, chain := range table.chains {
			fmt.Fprintf(&script, "\tchain %s {\n", chain.name)
			if chain.hook != "" {
				fmt.Fprintf(&script, "\t\ttype filter hook %s priority filter; policy accept;\n", chain.hook)
			}
			for _, rule := range chain.rules {
				script.WriteString("\t\t" + rule.String() + "\n")
			}
			script.WriteString("\t}\n")
		}
		script.WriteString("}\n")
	}
	return script.String()
}

func (r nftRule) String() string {
	var match []string
	if r.iif != "" {
		match = append(match, fmt.Sprintf("iifname %q", r.iif))
	}
	if r.oif != "" {
		match = append(match, fmt.Sprintf("oifname %q", r.oif))
	}
	if r.established {
		match = append(match, "ct state established,related")
	}
	if r.arp {
		match = append(match, "ether type arp")
	}
	if r.notIP {
		match = append(match, "ether type != ip")
	}
	if r.daddr.IsValid() {
		match = append(match, "ip daddr "+r.daddr.String())
	}
	if r.port != 0 {
		match = append(match, fmt.Sprintf("meta l4proto { tcp, udp } th dport %d", r.port))
	}
	verdict := r.verdict
	if verdict != "accept" && verdict != "drop" {
		verdict = "jump " + verdict
	}
	return strings.Join(append(match, verdict), " ")
}

func nft(ctx context.Context, stdin string, args ...string) error {
	cmd := exec.CommandContext(ctx, "nft", args...)
	cmd.Stdin = strings.NewReader(stdin)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("nft: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
