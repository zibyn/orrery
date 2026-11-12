package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	nodev1 "github.com/zibyn/orrery/gen/node/v1"
	"github.com/zibyn/orrery/internal/nodeproto"
	"github.com/zibyn/orrery/internal/store"
)

// sandboxLinksMinor is the protocol minor that carries SandboxLinks.
const sandboxLinksMinor = 5

// sandboxLinksByNode cuts each Node's SandboxLinks from the Link rows: a Link
// goes to the Node of each of its ends (node-protocol.md).
func sandboxLinksByNode(rows []store.ListSandboxLinkEndsRow) map[uuid.UUID][]*nodev1.SandboxLink {
	out := map[uuid.UUID][]*nodev1.SandboxLink{}
	for _, row := range rows {
		link := &nodev1.SandboxLink{
			TargetId: row.TargetID.String(), TargetAddress: row.TargetAddress,
			SourceId: row.SourceID.String(), SourceAddress: row.SourceAddress,
		}
		for _, port := range row.LinksPorts {
			link.Ports = append(link.Ports, uint32(port))
		}
		out[row.TargetNodeID] = append(out[row.TargetNodeID], link)
		if row.SourceNodeID != nil && *row.SourceNodeID != row.TargetNodeID {
			out[*row.SourceNodeID] = append(out[*row.SourceNodeID], link)
		}
	}
	return out
}

// linkPeerNodes is the distinct Nodes of a Sandbox's placed Link peers: its
// sources, and the targets that list it as one (api.md §3). A pending peer
// constrains nothing.
func linkPeerNodes(ctx context.Context, q *store.Queries, sb store.Sandbox) ([]uuid.UUID, error) {
	var sources []uuid.UUID
	if len(sb.LinksFrom) > 0 {
		var err error
		if sources, err = q.ListLinkSourceNodes(ctx, sb.ID); err != nil {
			return nil, err
		}
	}
	targets, err := q.ListLinkTargetNodes(ctx, sb.ID)
	if err != nil {
		return nil, err
	}
	nodes := append(sources, targets...)
	slices.SortFunc(nodes, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	return slices.Compact(nodes), nil
}

// linkPlaceable reports whether a Sandbox may go to node, given the distinct
// Nodes of its placed Link peers: each must be node itself or have a live
// Mesh path to it (runtime.md §15).
func linkPlaceable(peerNodes []uuid.UUID, node uuid.UUID, live func(a, b uuid.UUID) bool) bool {
	for _, peer := range peerNodes {
		if peer != node && !live(peer, node) {
			return false
		}
	}
	return true
}

// linksCapable reports whether a Node may run a Sandbox with these Link
// sources: any at all need sandbox_links enforced (api.md §3).
func linksCapable(from []uuid.UUID) func(store.Node) bool {
	return func(node store.Node) bool {
		return len(from) == 0 || nodeproto.CapabilityEnforced(node.Capabilities, nodeproto.CapSandboxLinks)
	}
}

// sandboxLinks pushes each Node its SandboxLinks when the set changed,
// including on an address change at start or stop (runtime.md §13).
func (r *reconciler) sandboxLinks(ctx context.Context) {
	rows, err := r.s.q.ListSandboxLinkEnds(ctx)
	if err != nil {
		r.s.log.Warn("reconcile: list sandbox links", "error", err)
		return
	}
	nodes, err := r.s.q.ListAllNodes(ctx)
	if err != nil {
		r.s.log.Warn("reconcile: list nodes for sandbox links", "error", err)
		return
	}
	r.pushSandboxLinks(nodes, rows)
}

// pushSandboxLinks sends the set under a command id derived from its
// content, so the hub's dedupe sends it once per connection and a changed
// set is a new command. An Agent older than the Links minor is never sent it.
func (r *reconciler) pushSandboxLinks(nodes []store.Node, rows []store.ListSandboxLinkEndsRow) {
	byNode := sandboxLinksByNode(rows)
	if r.linksSent == nil {
		r.linksSent = map[uuid.UUID]string{}
	}
	forgetOthers(r.linksSent, nodes)
	for _, n := range nodes {
		if n.ProtoMinor == nil || *n.ProtoMinor < sandboxLinksMinor {
			continue
		}
		msg := &nodev1.SandboxLinks{Links: byNode[n.ID]}
		r.pushSet(r.linksSent, n.ID, "sandbox-links", msg,
			&nodev1.ControlMessage{Message: &nodev1.ControlMessage_SandboxLinks{SandboxLinks: msg}})
	}
}

// forgetOthers drops the last sent set of every Node not in nodes.
func forgetOthers(sent map[uuid.UUID]string, nodes []store.Node) {
	keep := map[uuid.UUID]bool{}
	for _, n := range nodes {
		keep[n.ID] = true
	}
	for id := range sent {
		if !keep[id] {
			delete(sent, id)
		}
	}
}

// pushSet sends a full-set message under a command id derived from its
// content, so the hub's dedupe sends it once per connection, a changed set is
// a new command, and a send the full queue refused is retried next sweep.
func (r *reconciler) pushSet(sent map[uuid.UUID]string, node uuid.UUID, kind string, content proto.Message, msg *nodev1.ControlMessage) {
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(content)
	if err != nil {
		r.s.log.Warn("reconcile: encode "+kind, "node_id", node, "error", err)
		return
	}
	sum := sha256.Sum256(raw)
	id := kind + ":" + hex.EncodeToString(sum[:16])
	if prev := sent[node]; prev != id {
		// Forget the previous set, so returning to it later is resent.
		r.hub.failed(node, prev)
		sent[node] = id
	}
	r.hub.send(node, id, msg)
}
