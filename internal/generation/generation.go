// Package generation defines the Generation manifest: the immutable committed
// state of a Sandbox's filesystem or of a Volume (ADR-0031). It extends the
// Resource manifest (internal/manifest, ADR-0012) with full mode, owner,
// mtime, extended attributes, hard links, and the overlay whiteouts and
// opaque directories a diff uses to delete from its SnapshotVersion. Content
// uses ADR-0030's layout: one CAS object up to 4 MiB, 4 MiB blocks beyond.
package generation

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/zibyn/orrery/internal/manifest"
)

// Kinds (schema.md generations.kind).
const (
	// KindDiff is a container Sandbox's upperdir against its SnapshotVersion.
	KindDiff = "diff"
	// KindTree is a whole tree: a Volume or a Local Sandbox's working directory.
	KindTree = "tree"
)

// Entry types beyond manifest's file, dir and symlink.
const (
	// TypeHardlink is another name of the file entry at Target.
	TypeHardlink = "hardlink"
	// TypeWhiteout deletes Path from the lower layer (diff only).
	TypeWhiteout = "whiteout"
)

// Manifest is {"kind":..., "entries":[...]}, entries sorted by path once
// normalized. Paths are relative to the root of the filesystem or Volume.
type Manifest struct {
	Kind    string  `json:"kind"`
	Entries []Entry `json:"entries"`
}

// Entry is one path. The embedded manifest.Entry carries path, type, size,
// sha256, blocks and target (a symlink's target, or a hard link's file path);
// Executable is unused because Mode holds the full permission bits. Mode,
// owner, mtime and xattrs of a hard link are its file's; a whiteout has none.
type Entry struct {
	manifest.Entry
	Mode   uint32            `json:"mode"` // permission bits incl. setuid/setgid/sticky (07777)
	UID    uint32            `json:"uid"`
	GID    uint32            `json:"gid"`
	Mtime  int64             `json:"mtime"` // Unix nanoseconds
	Xattrs map[string][]byte `json:"xattrs,omitempty"`
	// Opaque marks a diff directory that hides the lower layer's contents.
	Opaque bool `json:"opaque,omitempty"`
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{manifest.ErrInvalid}, args...)...)
}

// Normalize validates m and returns it with entries sorted by path and every
// blocked file's sha256 computed from its blocks.
func Normalize(m Manifest) (Manifest, error) {
	if m.Kind != KindDiff && m.Kind != KindTree {
		return Manifest{}, invalid("kind %q", m.Kind)
	}
	if len(m.Entries) > manifest.MaxEntries {
		return Manifest{}, manifest.ErrTooManyEntries
	}
	entries := append([]Entry(nil), m.Entries...)
	byPath := make(map[string]Entry, len(entries))
	for i := range entries {
		e := &entries[i]
		if err := validateEntry(m.Kind, *e); err != nil {
			return Manifest{}, err
		}
		if _, dup := byPath[e.Path]; dup {
			return Manifest{}, invalid("duplicate path %q", e.Path)
		}
		if e.Blocks != nil {
			e.SHA256 = manifest.FileAddress(e.Blocks)
		}
		byPath[e.Path] = *e
	}
	for _, e := range entries {
		// Parents are explicit in both kinds: overlay copies a changed path's
		// parents up into the upperdir.
		if i := strings.LastIndexByte(e.Path, '/'); i >= 0 {
			if parent, ok := byPath[e.Path[:i]]; !ok || parent.Type != manifest.TypeDir {
				return Manifest{}, invalid("%q has no parent dir entry", e.Path)
			}
		}
		if e.Type == TypeHardlink {
			if target, ok := byPath[e.Target]; !ok || target.Type != manifest.TypeFile {
				return Manifest{}, invalid("hard link %q does not name a file entry", e.Path)
			}
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return Manifest{Kind: m.Kind, Entries: entries}, nil
}

func validateEntry(kind string, e Entry) error {
	if e.Executable {
		return invalid("%q uses executable; mode holds it", e.Path)
	}
	if e.Mode > 0o7777 {
		return invalid("%q has mode %o", e.Path, e.Mode)
	}
	for name := range e.Xattrs {
		if name == "" {
			return invalid("%q has an empty xattr name", e.Path)
		}
	}
	if e.Opaque && (kind != KindDiff || e.Type != manifest.TypeDir) {
		return invalid("%q: only a diff dir can be opaque", e.Path)
	}
	switch e.Type {
	case TypeHardlink, TypeWhiteout:
		if e.Type == TypeWhiteout && kind != KindDiff {
			return invalid("whiteout %q outside a diff", e.Path)
		}
		if e.Type == TypeHardlink && e.Target == "" {
			return invalid("hard link %q has no target", e.Path)
		}
		if e.Type == TypeWhiteout && e.Target != "" {
			return invalid("whiteout %q has a target", e.Path)
		}
		if e.Size != nil || e.SHA256 != "" || e.Blocks != nil || e.Xattrs != nil {
			return invalid("%s %q carries file fields", e.Type, e.Path)
		}
		// Validate the path through manifest's rules on a stand-in dir.
		return manifest.ValidateEntry(manifest.Entry{Path: e.Path, Type: manifest.TypeDir})
	}
	if err := manifest.ValidateEntry(e.Entry); err != nil {
		return err
	}
	return manifest.RequireBlocks(manifest.Manifest{Entries: []manifest.Entry{e.Entry}})
}

// Encode returns the canonical bytes stored at generations/<owner_id>/<id>:
// compact JSON of the normalized manifest.
func Encode(m Manifest) ([]byte, error) {
	normalized, err := Normalize(m)
	if err != nil {
		return nil, err
	}
	return json.Marshal(normalized)
}

// Decode parses and normalizes stored manifest bytes.
func Decode(data []byte) (Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, invalid("%v", err)
	}
	return Normalize(m)
}

// files is m's file entries as a Resource manifest, for the object helpers.
func files(m Manifest) manifest.Manifest {
	out := manifest.Manifest{}
	for _, e := range m.Entries {
		if e.Type == manifest.TypeFile {
			out.Entries = append(out.Entries, e.Entry)
		}
	}
	return out
}

// Objects lists the unique CAS objects m references, sorted by digest.
func Objects(m Manifest) []manifest.FileObject { return manifest.FileObjects(files(m)) }

// Size is the total size of m's file entries; hard links count once.
func Size(m Manifest) int64 { return manifest.TotalSize(files(m)) }
