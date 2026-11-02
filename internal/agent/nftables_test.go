package agent

import (
	"net/netip"
	"reflect"
	"testing"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
)

// runtime.md §13: the netlink rules are what nft compiled from the same
// rule, with nft's implicit protocol dependencies.
func TestNftRuleExprs(t *testing.T) {
	accept := &expr.Verdict{Kind: expr.VerdictAccept}
	ipDep := []expr.Any{
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseLLHeader, Offset: 12, Len: 2},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{0x08, 0x00}},
	}
	daddr := []expr.Any{
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 16, Len: 4},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: []byte{255, 0, 0, 0}, Xor: []byte{0, 0, 0, 0}},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{10, 0, 0, 0}},
	}
	l4 := func(proto byte) []expr.Any {
		return []expr.Any{
			&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{proto}},
			&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 2, Len: 2},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{0x1f, 0x90}},
		}
	}
	cat := func(parts ...[]expr.Any) []expr.Any {
		var out []expr.Any
		for _, p := range parts {
			out = append(out, p...)
		}
		return out
	}
	for _, tt := range []struct {
		name   string
		rule   nftRule
		family nftables.TableFamily
		want   [][]expr.Any
	}{
		{"jump on a veth", nftRule{iif: "vethabc", jump: "sandbox-x"}, nftables.TableFamilyBridge, [][]expr.Any{{
			&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: append([]byte("vethabc"), make([]byte, 9)...)},
			&expr.Verdict{Kind: expr.VerdictJump, Chain: "sandbox-x"},
		}}},
		{"established first", nftRule{established: true, verdict: "accept"}, nftables.TableFamilyBridge, [][]expr.Any{{
			&expr.Ct{Key: expr.CtKeySTATE, Register: 1},
			&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: []byte{6, 0, 0, 0}, Xor: []byte{0, 0, 0, 0}},
			&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: []byte{0, 0, 0, 0}},
			accept,
		}}},
		{"ether type != ip", nftRule{notIP: true, verdict: "drop"}, nftables.TableFamilyBridge, [][]expr.Any{{
			&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseLLHeader, Offset: 12, Len: 2},
			&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: []byte{0x08, 0x00}},
			&expr.Verdict{Kind: expr.VerdictDrop},
		}}},
		{"a port is one rule per TCP and UDP", nftRule{daddr: netip.MustParsePrefix("10.0.0.0/8"), port: 8080, verdict: "accept"},
			nftables.TableFamilyBridge, [][]expr.Any{
				cat(ipDep, daddr, l4(6), []expr.Any{accept}),
				cat(ipDep, daddr, l4(17), []expr.Any{accept}),
			}},
		{"inet depends on nfproto", nftRule{saddr: netip.MustParsePrefix("10.213.1.4/32"), verdict: "accept"}, nftables.TableFamilyINet, [][]expr.Any{{
			&expr.Meta{Key: expr.MetaKeyNFPROTO, Register: 1},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{2}},
			&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 12, Len: 4},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{10, 213, 1, 4}},
			accept,
		}}},
		{"MSS clamp", nftRule{oif: meshInterface, clampMSS: true}, nftables.TableFamilyINet, [][]expr.Any{{
			&expr.Meta{Key: expr.MetaKeyOIFNAME, Register: 1},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: append([]byte(meshInterface), make([]byte, 7)...)},
			&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{6}},
			&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 13, Len: 1},
			&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 1, Mask: []byte{0x02}, Xor: []byte{0}},
			&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: []byte{0}},
			&expr.Rt{Register: 1, Key: expr.RtTCPMSS},
			&expr.Byteorder{SourceRegister: 1, DestRegister: 1, Op: expr.ByteorderHton, Len: 2, Size: 2},
			&expr.Exthdr{SourceRegister: 1, Type: 2, Offset: 2, Len: 2, Op: expr.ExthdrOpTcpopt},
		}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.rule.exprs(tt.family); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("exprs =\n%#v\nwant\n%#v", got, tt.want)
			}
		})
	}
}

// nft's priority filter is -200 in the bridge family and 0 elsewhere.
func TestNftChainPriority(t *testing.T) {
	for family, want := range map[nftables.TableFamily]int32{nftables.TableFamilyBridge: -200, nftables.TableFamilyINet: 0} {
		chain := nftChainOf(&nftables.Table{Family: family}, nftChain{name: "forward", hook: "forward"})
		if chain.Type != nftables.ChainTypeFilter || *chain.Priority != nftables.ChainPriority(want) || *chain.Policy != nftables.ChainPolicyAccept {
			t.Errorf("family %d: chain = %+v, want type filter priority %d policy accept", family, chain, want)
		}
	}
}
