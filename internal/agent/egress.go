package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os/exec"
	"strings"

	"github.com/google/uuid"

	nodev1 "github.com/zibyn/orrery/gen/node/v1"
	"github.com/zibyn/orrery/internal/nodeproto"
)

// Egress Modes (ADR-0026).
const (
	egressOpen      = "open"
	egressNone      = "none"
	egressAllowlist = "allowlist"
)

// egressTable is the Agent's own bridge-family table; sandboxVeths maps each
// Sandbox's host-side veth to its chain (runtime.md §13).
const (
	egressTable  = "bridge orrery"
	sandboxVeths = "sandboxes"
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
	case egressOpen:
		policy = append(policy, egressRule{dest: anywhere, accept: true})
	case egressNone:
		policy = append(policy, egressRule{dest: anywhere})
	case egressAllowlist:
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
		return egressMode{name: egressOpen}
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
	if mode.name != egressOpen && !enforced {
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

// applyEgress (re)writes the Sandbox's chain and points its veth at it, in
// one nft transaction.
func applyEgress(ctx context.Context, id uuid.UUID, veth string, policy []egressRule) error {
	chain := egressTable + " " + egressChain(id)
	var script strings.Builder
	fmt.Fprintf(&script, `table %s {
	map %s { type ifname : verdict; }
	chain forward { type filter hook forward priority filter; policy accept; iifname vmap @%s; }
	chain input { type filter hook input priority filter; policy accept; iifname vmap @%s; }
}
add chain %s
flush chain %s
add rule %s ct state established,related accept
add rule %s ether type arp accept
add rule %s ether type != ip drop
`, egressTable, sandboxVeths, sandboxVeths, sandboxVeths, chain, chain, chain, chain, chain)
	for _, rule := range policy {
		verdict := "drop"
		if rule.accept {
			verdict = "accept"
		}
		match := "ip daddr " + rule.dest.Prefix.String()
		if rule.dest.Port != 0 {
			match += fmt.Sprintf(" meta l4proto { tcp, udp } th dport %d", rule.dest.Port)
		}
		fmt.Fprintf(&script, "add rule %s %s %s\n", chain, match, verdict)
	}
	fmt.Fprintf(&script, "add element %s %s { %q : jump %s }\n", egressTable, sandboxVeths, veth, egressChain(id))
	return nft(ctx, script.String(), "-f", "-")
}

// removeEgress deletes the Sandbox's chain and every veth pointing at it. It
// does not need the recorded veth, so a probe a crash left behind is removed
// too.
func removeEgress(ctx context.Context, id uuid.UUID) error {
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, "nft", "-j", "list", "map", "bridge", "orrery", sandboxVeths)
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil // no table, so no chain either
	}
	var listing struct {
		Nftables []struct {
			Map *struct {
				Elem []json.RawMessage `json:"elem"`
			} `json:"map"`
		} `json:"nftables"`
	}
	if err := json.Unmarshal(out.Bytes(), &listing); err != nil {
		return fmt.Errorf("decode nft map: %w", err)
	}
	var script strings.Builder
	for _, item := range listing.Nftables {
		if item.Map == nil {
			continue
		}
		for _, raw := range item.Map.Elem {
			var elem []json.RawMessage
			var veth string
			var target struct {
				Jump struct {
					Target string `json:"target"`
				} `json:"jump"`
			}
			if json.Unmarshal(raw, &elem) != nil || len(elem) != 2 ||
				json.Unmarshal(elem[0], &veth) != nil || json.Unmarshal(elem[1], &target) != nil {
				continue
			}
			if target.Jump.Target == egressChain(id) {
				fmt.Fprintf(&script, "delete element %s %s { %q }\n", egressTable, sandboxVeths, veth)
			}
		}
	}
	// Adding first makes the delete succeed when the chain is already gone.
	fmt.Fprintf(&script, "add chain %s %s\ndelete chain %s %s\n", egressTable, egressChain(id), egressTable, egressChain(id))
	return nft(ctx, script.String(), "-f", "-")
}

func nft(ctx context.Context, stdin string, args ...string) error {
	cmd := exec.CommandContext(ctx, "nft", args...)
	cmd.Stdin = strings.NewReader(stdin)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("nft: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
