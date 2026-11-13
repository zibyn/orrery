package resourcefs

import (
	"log/slog"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/zibyn/orrery/internal/manifest"
)

// forever stands for "never expires": a version never changes.
const forever = 100 * 365 * 24 * time.Hour

func mountOptions(address string) *fuse.MountOptions {
	return &fuse.MountOptions{
		AllowOther: true, DirectMount: true,
		FsName: "orrery-resource-" + address[:12], Name: "orrery-resourcefs",
		DisableXAttrs: true, DisableReadDirPlus: true,
		MaxBackground: 64, MaxWrite: 1 << 20, MaxReadAhead: 1 << 20,
	}
}

// rawFS serves one version. It implements fuse.RawFileSystem directly: IDs
// come from the manifest and Open allocates nothing, so the state another
// process rebuilds from the same manifest is identical (ADR-0030).
type rawFS struct {
	fuse.RawFileSystem
	nodes   []node
	lastEnd []atomic.Int64 // per node, to detect sequential reads
	store   *store
	log     *slog.Logger
}

func newRawFS(m manifest.Manifest, s *store, log *slog.Logger) *rawFS {
	nodes := buildTree(m)
	return &rawFS{
		RawFileSystem: fuse.NewDefaultRawFileSystem(),
		nodes:         nodes, lastEnd: make([]atomic.Int64, len(nodes)), store: s, log: log,
	}
}

func (f *rawFS) String() string { return "orrery-resourcefs" }

func (f *rawFS) node(id uint64) *node {
	if id == 0 || id >= uint64(len(f.nodes)) {
		return nil
	}
	return &f.nodes[id]
}

func (f *rawFS) attr(id uint64, a *fuse.Attr) {
	n := &f.nodes[id]
	a.Ino = id
	switch n.entry.Type {
	case manifest.TypeFile:
		a.Mode, a.Nlink = syscall.S_IFREG|0o444, 1
		if n.entry.Executable {
			a.Mode |= 0o111
		}
		a.Size = uint64(*n.entry.Size)
		a.Blocks = (a.Size + 511) / 512
	case manifest.TypeSymlink:
		a.Mode, a.Nlink = syscall.S_IFLNK|0o777, 1
		a.Size = uint64(len(n.entry.Target))
	default:
		a.Mode, a.Nlink = syscall.S_IFDIR|0o555, 2
	}
}

func (f *rawFS) Lookup(_ <-chan struct{}, h *fuse.InHeader, name string, out *fuse.EntryOut) fuse.Status {
	parent := f.node(h.NodeId)
	if parent == nil || !parent.isDir() {
		return fuse.ENOTDIR
	}
	out.SetEntryTimeout(forever)
	id, ok := parent.byName[name]
	if !ok {
		out.NodeId = 0 // a negative entry, cached like any other
		return fuse.OK
	}
	out.NodeId, out.Generation = id, 1
	out.SetAttrTimeout(forever)
	f.attr(id, &out.Attr)
	return fuse.OK
}

// Forget is a no-op: node IDs are fixed by the manifest.
func (f *rawFS) Forget(uint64, uint64) {}

func (f *rawFS) GetAttr(_ <-chan struct{}, in *fuse.GetAttrIn, out *fuse.AttrOut) fuse.Status {
	if f.node(in.NodeId) == nil {
		return fuse.ENOENT
	}
	out.SetTimeout(forever)
	f.attr(in.NodeId, &out.Attr)
	return fuse.OK
}

func (f *rawFS) Readlink(_ <-chan struct{}, h *fuse.InHeader) ([]byte, fuse.Status) {
	n := f.node(h.NodeId)
	if n == nil || n.entry.Type != manifest.TypeSymlink {
		return nil, fuse.EINVAL
	}
	return []byte(n.entry.Target), fuse.OK
}

func (f *rawFS) Open(_ <-chan struct{}, in *fuse.OpenIn, out *fuse.OpenOut) fuse.Status {
	if in.Flags&(syscall.O_WRONLY|syscall.O_RDWR|syscall.O_TRUNC) != 0 {
		return fuse.EROFS
	}
	// No handle: Read needs only the node ID, so handles survive a handover.
	out.OpenFlags = fuse.FOPEN_KEEP_CACHE
	return fuse.OK
}

func (f *rawFS) OpenDir(_ <-chan struct{}, in *fuse.OpenIn, _ *fuse.OpenOut) fuse.Status {
	if n := f.node(in.NodeId); n == nil || !n.isDir() {
		return fuse.ENOTDIR
	}
	return fuse.OK
}

// ReadDir lists kids from in.Offset; the offset is the sorted position.
func (f *rawFS) ReadDir(_ <-chan struct{}, in *fuse.ReadIn, out *fuse.DirEntryList) fuse.Status {
	n := f.node(in.NodeId)
	if n == nil || !n.isDir() {
		return fuse.ENOTDIR
	}
	for i := in.Offset; i < uint64(len(n.kids)); i++ {
		id := n.kids[i]
		var a fuse.Attr
		f.attr(id, &a)
		if !out.AddDirEntry(fuse.DirEntry{Mode: a.Mode, Name: f.nodes[id].name, Ino: id, Off: i + 1}) {
			break
		}
	}
	return fuse.OK
}

func (f *rawFS) StatFs(_ <-chan struct{}, _ *fuse.InHeader, out *fuse.StatfsOut) fuse.Status {
	out.Bsize, out.NameLen, out.Files = uint32(manifest.BlockSize), 255, uint64(len(f.nodes)-1)
	return fuse.OK
}

func (f *rawFS) Read(_ <-chan struct{}, in *fuse.ReadIn, dest []byte) (fuse.ReadResult, fuse.Status) {
	n := f.node(in.NodeId)
	if n == nil || n.entry.Type != manifest.TypeFile {
		return nil, fuse.EINVAL
	}
	size := *n.entry.Size
	off := int64(in.Offset)
	end := min(off+int64(in.Size), size)
	if off >= end {
		return fuse.ReadResultData(nil), fuse.OK
	}
	if f.lastEnd[in.NodeId].Swap(end) == off && off > 0 {
		last := int((end - 1) / manifest.BlockSize)
		for next := last + 1; next <= last+prefetchBlocks && next < refCount(n.entry); next++ {
			go f.store.ensure(refAt(n.entry, next), f.hint(in.NodeId, next)) //nolint:errcheck // a later read retries
		}
	}
	done := 0
	for pos := off; pos < end; {
		index := int(pos / manifest.BlockSize)
		ref := refAt(n.entry, index)
		if err := f.store.ensure(ref, f.hint(in.NodeId, index)); err != nil {
			f.log.Error("read Resource block", "path", n.entry.Path, "block", index, "error", err)
			return nil, fuse.EIO
		}
		from := pos - int64(index)*manifest.BlockSize
		chunk := min(end-pos, ref.length-from)
		if err := f.store.readBlock(ref, from, dest[done:done+int(chunk)]); err != nil {
			f.log.Error("read cached Resource block", "path", n.entry.Path, "block", index, "error", err)
			return nil, fuse.EIO
		}
		done += int(chunk)
		pos += chunk
	}
	return fuse.ReadResultData(dest[:done]), fuse.OK
}

// hint lists what to presign with block index of file id: the file's later
// blocks, then the blocks of the files after it in the same directory.
func (f *rawFS) hint(id uint64, index int) func() []blockRef {
	return func() []blockRef {
		var refs []blockRef
		add := func(entry manifest.Entry, from int) {
			for i := from; i < refCount(entry) && len(refs) < presignBatch; i++ {
				refs = append(refs, refAt(entry, i))
			}
		}
		add(f.nodes[id].entry, index)
		siblings := f.nodes[f.nodes[id].parent].kids
		start := 0
		for start < len(siblings) && siblings[start] != id {
			start++
		}
		for _, kid := range siblings[min(start+1, len(siblings)):] {
			if len(refs) >= presignBatch {
				break
			}
			if entry := f.nodes[kid].entry; entry.Type == manifest.TypeFile {
				add(entry, 0)
			}
		}
		return refs
	}
}
