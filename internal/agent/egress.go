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
// rendered from: every local Sandbox's recorded network, the Sandbox
// subnet, and the latest SandboxLinks.
type networkState struct {
	sandboxes map[uuid.UUID]sandboxNetwork
	subnet    netip.Prefix
	links     []*nodev1.SandboxLink
}

// nftTable, nftChain and nftRule are the rendered tables, before nftScript
// turns them into nft syntax. A chain with a hook is a base chain with policy
// accept: nat at priority srcnat on postrouting, else filter.
type nftTable struct {
	family, name string
	chains       []nftChain
}

type nftChain struct {
	name  string
	hook  string
	rules []nftRule
}

// nftRule matches on every field that is set, then jumps to the chain jump
// or applies verdict.
type nftRule struct {
	iif, oif    string
	notOif      bool // oifname != oif
	established bool // ct state established,related
	arp         bool // ether type arp
	notIP       bool // ether type != ip
	saddr       netip.Prefix
	daddr       netip.Prefix
	notDaddr    bool   // ip daddr != daddr
	port        uint16 // TCP or UDP destination port
	tcpUDP      bool   // TCP or UDP, any port
	clampMSS    bool   // a TCP SYN's MSS set to the route MTU; with no verdict
	verdict     string // accept, drop or masquerade
	jump        string // a chain name
}

// chainPrelude opens both per-Sandbox chains: replies pass, ARP passes, and
// every other non-IPv4 frame is dropped (runtime.md §13).
func chainPrelude() []nftRule {
	return []nftRule{
		{established: true, verdict: "accept"},
		{arp: true, verdict: "accept"},
		{notIP: true, verdict: "drop"},
	}
}

func linkChain(id uuid.UUID) string { return "link-" + id.String() }

// linkEnds is a Link's two addresses as /32 prefixes; one that is empty or
// not IPv4 is the zero Prefix. ok reports that both are set.
func linkEnds(link *nodev1.SandboxLink) (target, source netip.Prefix, ok bool) {
	host := func(raw string) netip.Prefix {
		if addr, err := netip.ParseAddr(raw); err == nil && addr.Is4() {
			return netip.PrefixFrom(addr, 32)
		}
		return netip.Prefix{}
	}
	target, source = host(link.TargetAddress), host(link.SourceAddress)
	return target, source, target.IsValid() && source.IsValid()
}

// linkRules is match once per linked port, or once for every TCP and UDP
// port (api.md §3). A port out of range is left out, so it fails closed
// rather than matching all.
func linkRules(match nftRule, ports []uint32) []nftRule {
	if len(ports) == 0 {
		match.tcpUDP = true
		return []nftRule{match}
	}
	var rules []nftRule
	for _, port := range ports {
		if port >= 1 && port <= 65535 {
			match.port = uint16(port)
			rules = append(rules, match)
		}
	}
	return rules
}

// renderNetwork is the whole bridge table orrery: forward and input jump on
// each Sandbox's host-side veth into its chain, which accepts its Links and
// then applies its Egress Policy; forward and output jump on a Link target's
// veth into its inbound chain, and in forward every inbound jump comes first
// so an open source cannot bypass it (runtime.md §13).
func renderNetwork(state networkState) []nftTable {
	ids := make([]uuid.UUID, 0, len(state.sandboxes))
	for id := range state.sandboxes {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	forward := nftChain{name: "forward", hook: "forward"}
	input := nftChain{name: "input", hook: "input"}
	output := nftChain{name: "output", hook: "output"}
	var egressJumps []nftRule
	table := nftTable{family: egressFamily, name: egressTableName}
	for _, id := range ids {
		network := state.sandboxes[id]
		jump := nftRule{iif: network.HostVeth, jump: egressChain(id)}
		egressJumps = append(egressJumps, jump)
		input.rules = append(input.rules, jump)
		chain := nftChain{name: egressChain(id), rules: chainPrelude()}
		var inbound []nftRule
		isTarget := network.LinkTarget
		for _, link := range state.links {
			target, source, ok := linkEnds(link)
			if !target.IsValid() {
				continue
			}
			if link.SourceId == id.String() && ok {
				chain.rules = append(chain.rules, linkRules(nftRule{daddr: target, verdict: "accept"}, link.Ports)...)
			}
			if link.TargetId == id.String() {
				// A source with no address renders no accept; the chain still ends in a drop.
				isTarget = true
				if ok {
					inbound = append(inbound, linkRules(nftRule{saddr: source, verdict: "accept"}, link.Ports)...)
				}
			}
		}
		if isTarget {
			in := nftRule{oif: network.HostVeth, jump: linkChain(id)}
			forward.rules = append(forward.rules, in)
			output.rules = append(output.rules, in)
			rules := append(chainPrelude(), inbound...)
			table.chains = append(table.chains, nftChain{name: linkChain(id), rules: append(rules, nftRule{verdict: "drop"})})
		}
		for _, rule := range network.Policy {
			verdict := "drop"
			if rule.accept {
				verdict = "accept"
			}
			chain.rules = append(chain.rules, nftRule{daddr: rule.dest.Prefix, port: rule.dest.Port, verdict: verdict})
		}
		table.chains = append(table.chains, chain)
	}
	forward.rules = append(forward.rules, egressJumps...)
	table.chains = append([]nftChain{forward, input, output}, table.chains...)
	tables := []nftTable{table, meshTable(state)}
	if state.subnet.IsValid() {
		tables = append(tables, natTable(state.subnet))
	}
	return tables
}

// meshTable is the inet table orrery_mesh: through orrery-wg only established
// traffic and linked pairs with an end here pass, and the host itself sends
// nothing into it (runtime.md §15).
func meshTable(state networkState) nftTable {
	forward := nftChain{name: "forward", hook: "forward", rules: []nftRule{
		{iif: meshInterface, clampMSS: true},
		{oif: meshInterface, clampMSS: true},
		{iif: meshInterface, established: true, verdict: "accept"},
		{oif: meshInterface, established: true, verdict: "accept"},
	}}
	local := func(id string) bool {
		parsed, err := uuid.Parse(id)
		_, ok := state.sandboxes[parsed]
		return err == nil && ok
	}
	for _, link := range state.links {
		target, source, ok := linkEnds(link)
		if !ok {
			continue
		}
		pair := nftRule{saddr: source, daddr: target, verdict: "accept"}
		if local(link.TargetId) {
			pair.iif = meshInterface
			forward.rules = append(forward.rules, linkRules(pair, link.Ports)...)
		} else if local(link.SourceId) {
			pair.oif = meshInterface
			forward.rules = append(forward.rules, linkRules(pair, link.Ports)...)
		}
	}
	forward.rules = append(forward.rules, nftRule{iif: meshInterface, verdict: "drop"}, nftRule{oif: meshInterface, verdict: "drop"})
	output := nftChain{name: "output", hook: "output", rules: []nftRule{{oif: meshInterface, verdict: "drop"}}}
	return nftTable{family: "inet", name: "orrery_mesh", chains: []nftChain{forward, output}}
}

// natTable is the ip table orrery_nat: Sandbox traffic leaving the subnet
// takes the Node's address, except into the Mesh (runtime.md §13).
func natTable(subnet netip.Prefix) nftTable {
	return nftTable{family: "ip", name: "orrery_nat", chains: []nftChain{{
		name: "postrouting", hook: "postrouting",
		rules: []nftRule{{saddr: subnet, daddr: subnet, notDaddr: true, oif: meshInterface, notOif: true, verdict: "masquerade"}},
	}}}
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
			switch chain.hook {
			case "":
			case "postrouting":
				script.WriteString("\t\ttype nat hook postrouting priority srcnat; policy accept;\n")
			default:
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
	not := func(negate bool) string {
		if negate {
			return "!= "
		}
		return ""
	}
	if r.iif != "" {
		match = append(match, fmt.Sprintf("iifname %q", r.iif))
	}
	if r.oif != "" {
		match = append(match, fmt.Sprintf("oifname %s%q", not(r.notOif), r.oif))
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
	if r.saddr.IsValid() {
		match = append(match, "ip saddr "+r.saddr.String())
	}
	if r.daddr.IsValid() {
		match = append(match, "ip daddr "+not(r.notDaddr)+r.daddr.String())
	}
	if r.port != 0 {
		match = append(match, fmt.Sprintf("meta l4proto { tcp, udp } th dport %d", r.port))
	} else if r.tcpUDP {
		match = append(match, "meta l4proto { tcp, udp }")
	}
	if r.clampMSS {
		return strings.Join(append(match, "tcp flags syn tcp option maxseg size set rt mtu"), " ")
	}
	verdict := r.verdict
	if r.jump != "" {
		verdict = "jump " + r.jump
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
