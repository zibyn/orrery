// Package resourcefs is orrery-resourcefs, the Node's read-only FUSE daemon
// that mounts each ResourceVersion lazily at <data-root>/resources/<address>
// (runtime.md §11, ADR-0030). Its state is a pure function of the manifest:
// node IDs are sorted manifest positions and handles carry nothing, so a new
// process can take over a live /dev/fuse session from the old one.
package resourcefs

import (
	"strings"

	"github.com/zibyn/orrery/internal/manifest"
)

// rootID is the FUSE root node; manifest entry i (sorted by path) is i+2.
const rootID = 1

// node is one path of the version tree. A directory's kids are in sorted
// path order, which is also its readdir offset order.
type node struct {
	name   string
	entry  manifest.Entry // zero for the root
	parent uint64
	kids   []uint64
	byName map[string]uint64
}

func (n *node) isDir() bool { return n.entry.Type == "" || n.entry.Type == manifest.TypeDir }

// buildTree lays out a normalized manifest. Normalize sorts entries by path
// and guarantees every parent is an explicit dir entry that sorts before its
// children, so every process loading the same manifest assigns the same IDs.
func buildTree(m manifest.Manifest) []node {
	nodes := make([]node, len(m.Entries)+2) // 0 unused, 1 is the root
	nodes[rootID] = node{byName: map[string]uint64{}}
	ids := make(map[string]uint64, len(m.Entries)+1)
	ids[""] = rootID
	for i, entry := range m.Entries {
		id := uint64(i + 2)
		ids[entry.Path] = id
		parent, name := "", entry.Path
		if cut := strings.LastIndexByte(entry.Path, '/'); cut >= 0 {
			parent, name = entry.Path[:cut], entry.Path[cut+1:]
		}
		n := &nodes[id]
		n.name, n.entry, n.parent = name, entry, ids[parent]
		if n.isDir() {
			n.byName = map[string]uint64{}
		}
		p := &nodes[n.parent]
		p.kids = append(p.kids, id)
		p.byName[name] = id
	}
	return nodes
}
