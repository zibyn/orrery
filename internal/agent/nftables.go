package agent

import (
	"encoding/binary"
	"fmt"
	"net/netip"

	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
	"golang.org/x/sys/unix"
)

var nftFamilies = map[string]nftables.TableFamily{
	"bridge": nftables.TableFamilyBridge,
	"inet":   nftables.TableFamilyINet,
	"ip":     nftables.TableFamilyIPv4,
}

var nftHooks = map[string]*nftables.ChainHook{
	"forward":     nftables.ChainHookForward,
	"input":       nftables.ChainHookInput,
	"output":      nftables.ChainHookOutput,
	"postrouting": nftables.ChainHookPostrouting,
}

// applyTables replaces each table whole in one netlink batch, as
// `add table; delete table; table {...}` does in one nft transaction: adding
// first makes the delete succeed when the table does not exist yet.
func applyTables(tables []nftTable) error {
	conn, err := nftables.New()
	if err != nil {
		return fmt.Errorf("nftables: %w", err)
	}
	for _, t := range tables {
		table := &nftables.Table{Family: nftFamilies[t.family], Name: t.name}
		conn.AddTable(table)
		conn.DelTable(table)
		conn.AddTable(table)
		// Every chain before any rule, so a jump finds its chain.
		chains := make([]*nftables.Chain, len(t.chains))
		for i, c := range t.chains {
			chains[i] = conn.AddChain(nftChainOf(table, c))
		}
		for i, c := range t.chains {
			for _, rule := range c.rules {
				for _, exprs := range rule.exprs(table.Family) {
					conn.AddRule(&nftables.Rule{Table: table, Chain: chains[i], Exprs: exprs})
				}
			}
		}
		if err := conn.Flush(); err != nil {
			return fmt.Errorf("nftables %s %s: %w", t.family, t.name, err)
		}
	}
	return nil
}

// nftChainOf is a regular chain, or a base chain with policy accept at the
// named priority filter or srcnat of its family.
func nftChainOf(table *nftables.Table, c nftChain) *nftables.Chain {
	chain := &nftables.Chain{Name: c.name, Table: table}
	if c.hook == "" {
		return chain
	}
	accept := nftables.ChainPolicyAccept
	chain.Hooknum, chain.Policy = nftHooks[c.hook], &accept
	switch {
	case c.hook == "postrouting":
		chain.Type, chain.Priority = nftables.ChainTypeNAT, nftables.ChainPriorityNATSource
	case table.Family == nftables.TableFamilyBridge:
		// nft's priority filter is -200 in the bridge family.
		chain.Type, chain.Priority = nftables.ChainTypeFilter, nftables.ChainPriorityRef(-200)
	default:
		chain.Type, chain.Priority = nftables.ChainTypeFilter, nftables.ChainPriorityFilter
	}
	return chain
}

// exprs is the rule as nft compiles it, with the implicit protocol
// dependencies nft adds. A rule on TCP and UDP becomes one rule per
// protocol, where nft would use an anonymous set.
func (r nftRule) exprs(family nftables.TableFamily) [][]expr.Any {
	var match []expr.Any
	load := func(e expr.Any) { match = append(match, e) }
	cmp := func(op expr.CmpOp, data []byte) { load(&expr.Cmp{Op: op, Register: 1, Data: data}) }
	op := func(negate bool) expr.CmpOp {
		if negate {
			return expr.CmpOpNeq
		}
		return expr.CmpOpEq
	}
	etherType := func(negate bool, etherType uint16) {
		load(&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseLLHeader, Offset: 12, Len: 2})
		cmp(op(negate), binaryutil.BigEndian.PutUint16(etherType))
	}
	if r.iif != "" {
		load(&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1})
		cmp(expr.CmpOpEq, ifname(r.iif))
	}
	if r.oif != "" {
		load(&expr.Meta{Key: expr.MetaKeyOIFNAME, Register: 1})
		cmp(op(r.notOif), ifname(r.oif))
	}
	if r.established {
		load(&expr.Ct{Key: expr.CtKeySTATE, Register: 1})
		load(&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4,
			Mask: binaryutil.NativeEndian.PutUint32(expr.CtStateBitESTABLISHED | expr.CtStateBitRELATED),
			Xor:  make([]byte, 4)})
		cmp(expr.CmpOpNeq, make([]byte, 4))
	}
	if r.arp {
		etherType(false, unix.ETH_P_ARP)
	}
	if r.notIP {
		etherType(true, unix.ETH_P_IP)
	}
	if r.saddr.IsValid() || r.daddr.IsValid() {
		switch family {
		case nftables.TableFamilyBridge:
			etherType(false, unix.ETH_P_IP)
		case nftables.TableFamilyINet:
			load(&expr.Meta{Key: expr.MetaKeyNFPROTO, Register: 1})
			cmp(expr.CmpOpEq, []byte{unix.NFPROTO_IPV4})
		}
		for _, m := range []struct {
			offset uint32
			prefix netip.Prefix
			negate bool
		}{{12, r.saddr, false}, {16, r.daddr, r.notDaddr}} {
			if !m.prefix.IsValid() {
				continue
			}
			load(&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: m.offset, Len: 4})
			if bits := m.prefix.Bits(); bits < 32 {
				mask := make([]byte, 4)
				binary.BigEndian.PutUint32(mask, ^uint32(0)<<(32-bits))
				load(&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: mask, Xor: make([]byte, 4)})
			}
			cmp(op(m.negate), m.prefix.Masked().Addr().AsSlice())
		}
	}
	var protos []byte
	switch {
	case r.clampMSS:
		protos = []byte{unix.IPPROTO_TCP}
	case r.port != 0 || r.tcpUDP:
		protos = []byte{unix.IPPROTO_TCP, unix.IPPROTO_UDP}
	}
	tail := r.tail()
	if protos == nil {
		return [][]expr.Any{append(match, tail...)}
	}
	var rules [][]expr.Any
	for _, proto := range protos {
		rule := append(append([]expr.Any{}, match...),
			&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{proto}})
		if r.port != 0 {
			rule = append(rule,
				&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 2, Len: 2},
				&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: binaryutil.BigEndian.PutUint16(r.port)})
		}
		rules = append(rules, append(rule, tail...))
	}
	return rules
}

// tail is the rule's statement: tcp flags syn tcp option maxseg size set rt
// mtu for clampMSS, else its jump or verdict.
func (r nftRule) tail() []expr.Any {
	switch {
	case r.clampMSS:
		return []expr.Any{
			&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 13, Len: 1},
			&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 1, Mask: []byte{0x02}, Xor: []byte{0}},
			&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: []byte{0}},
			&expr.Rt{Register: 1, Key: expr.RtTCPMSS},
			&expr.Byteorder{SourceRegister: 1, DestRegister: 1, Op: expr.ByteorderHton, Len: 2, Size: 2},
			&expr.Exthdr{SourceRegister: 1, Type: 2, Offset: 2, Len: 2, Op: expr.ExthdrOpTcpopt},
		}
	case r.jump != "":
		return []expr.Any{&expr.Verdict{Kind: expr.VerdictJump, Chain: r.jump}}
	case r.verdict == "masquerade":
		return []expr.Any{&expr.Masq{}}
	case r.verdict == "accept":
		return []expr.Any{&expr.Verdict{Kind: expr.VerdictAccept}}
	default:
		return []expr.Any{&expr.Verdict{Kind: expr.VerdictDrop}}
	}
}

// ifname is an interface name zero-padded to IFNAMSIZ, as nft compares it.
func ifname(name string) []byte {
	b := make([]byte, unix.IFNAMSIZ)
	copy(b, name)
	return b
}
