package resourcefs

import (
	"math"
	"os"
	"sort"
	"strings"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/zibyn/orrery/internal/manifest"
)

// The FUSE operations of a Volume mount. The POSIX scope (runtime.md §17):
// create, write, truncate, O_APPEND and shared mmap writes (both through the
// kernel's write path), rename including over an existing name and
// RENAME_NOREPLACE, hard links, symlinks, unlink of an open file, mode,
// owner, mtime and xattrs. Permission checks are the kernel's
// (default_permissions). flock and fcntl locks are the kernel's, local to
// the Node. RENAME_EXCHANGE and device nodes are refused; FIFOs and sockets
// work but are not committed.

const (
	renameNoReplace = 1 // RENAME_NOREPLACE
	xattrCreate     = 1 // XATTR_CREATE
	xattrReplace    = 2 // XATTR_REPLACE
)

func volumeMountOptions(volume string) *fuse.MountOptions {
	return &fuse.MountOptions{
		AllowOther: true, DirectMount: true,
		FsName: "orrery-volume-" + volume[:8], Name: "orrery-resourcefs",
		Options:            []string{"default_permissions"},
		DisableReadDirPlus: true,
		MaxBackground:      64, MaxWrite: 1 << 20, MaxReadAhead: 1 << 20,
	}
}

func errno(e syscall.Errno) fuse.Status { return fuse.Status(e) }

func typeBits(t string) uint32 {
	switch t {
	case manifest.TypeDir:
		return syscall.S_IFDIR
	case manifest.TypeSymlink:
		return syscall.S_IFLNK
	case typeFIFO:
		return syscall.S_IFIFO
	case typeSocket:
		return syscall.S_IFSOCK
	}
	return syscall.S_IFREG
}

func (f *volumeFS) fill(ino uint64, n *vnode, a *fuse.Attr) {
	a.Ino, a.Mode, a.Nlink = ino, typeBits(n.Type)|n.Mode, n.Nlink
	a.Uid, a.Gid = n.UID, n.GID
	a.Size = uint64(n.Size)
	if n.Type == manifest.TypeSymlink {
		a.Size = uint64(len(n.Target))
	}
	a.Blocks, a.Blksize = (a.Size+511)/512, 4096
	a.Mtime, a.Mtimensec = uint64(n.Mtime/1e9), uint32(n.Mtime%1e9)
	a.Atime, a.Atimensec = a.Mtime, a.Mtimensec
	a.Ctime, a.Ctimensec = uint64(n.Ctime/1e9), uint32(n.Ctime%1e9)
}

func (f *volumeFS) entry(ino uint64, out *fuse.EntryOut) {
	n := f.st.Nodes[ino]
	n.Lookups++
	out.NodeId, out.Generation = ino, 1
	out.SetEntryTimeout(volumeTTL)
	out.SetAttrTimeout(volumeTTL)
	f.fill(ino, n, &out.Attr)
}

// dir returns a directory inode under f.treeMu.
func (f *volumeFS) dirNode(ino uint64) (*vnode, fuse.Status) {
	n, ok := f.st.Nodes[ino]
	switch {
	case !ok:
		return nil, fuse.ENOENT
	case !n.isDir():
		return nil, fuse.ENOTDIR
	}
	return n, fuse.OK
}

func validName(name string) fuse.Status {
	if name == "" || name == "." || name == ".." || strings.ContainsRune(name, '/') {
		return fuse.EINVAL
	}
	if len(name) > 255 {
		return errno(syscall.ENAMETOOLONG)
	}
	return fuse.OK
}

func (f *volumeFS) Lookup(_ <-chan struct{}, h *fuse.InHeader, name string, out *fuse.EntryOut) fuse.Status {
	f.treeMu.Lock()
	defer f.treeMu.Unlock()
	parent, st := f.dirNode(h.NodeId)
	if !st.Ok() {
		return st
	}
	d, ok := parent.index()[name]
	if !ok {
		return fuse.ENOENT
	}
	f.entry(d.Ino, out)
	return fuse.OK
}

func (f *volumeFS) Forget(ino, nlookup uint64) {
	f.treeMu.Lock()
	defer f.unlockTree()
	n, ok := f.st.Nodes[ino]
	if !ok {
		return
	}
	n.Lookups -= min(n.Lookups, nlookup)
	f.maybeFree(ino, n)
}

// maybeFree drops an inode no name and no kernel reference keeps; its
// write-cache file goes in unlockTree.
func (f *volumeFS) maybeFree(ino uint64, n *vnode) {
	if ino != rootID && n.Nlink == 0 && n.Lookups == 0 {
		delete(f.st.Nodes, ino)
		if f.freed == nil {
			f.freed = map[uint64]*vnode{}
		}
		f.freed[ino] = n
	}
}

// unlockTree releases the tree lock, then removes the write-cache files of
// the inodes freed under it, each once its content IO is done and a seal
// that froze it has hashed it.
func (f *volumeFS) unlockTree() {
	freed := f.freed
	f.freed = nil
	f.treeMu.Unlock()
	for ino, n := range freed {
		f.treeMu.Lock()
		frozen := n.frozen
		f.treeMu.Unlock()
		if frozen != nil {
			<-frozen
		}
		n.mu.Lock()
		f.dropLocal(ino, n)
		n.mu.Unlock()
	}
}

// noCopy tells lockContent to leave committed content where it is.
const noCopy = -1

// lockContent takes a file's content lock without the tree lock held:
// shared for IO, exclusive for a change of its extent. Local content comes
// with n.file open; keep other than noCopy first copies committed content
// up, keeping keep bytes. Opening or copying takes the lock exclusively,
// even when shared was asked for. A change (exclusive, or a copy asked for)
// to a frozen file waits until the seal has hashed it. A copy asked for
// means a write or truncate follows: local content turns dirty in the step
// that finds it not frozen, before its bytes move, so a seal from then on
// freezes it and waits rather than recording a clean copy's old digests.
func (f *volumeFS) lockContent(ino uint64, exclusive bool, keep int64) (*vnode, func(), fuse.Status) {
	f.treeMu.Lock()
	n, ok := f.st.Nodes[ino]
	f.treeMu.Unlock()
	switch {
	case !ok:
		return nil, nil, fuse.ENOENT
	case n.Type != manifest.TypeFile:
		return nil, nil, fuse.EINVAL
	}
	change := exclusive || keep != noCopy
	for {
		lock, unlock := n.mu.RLock, n.mu.RUnlock
		if exclusive {
			lock, unlock = n.mu.Lock, n.mu.Unlock
		}
		lock()
		f.treeMu.Lock()
		alive, local, frozen := f.st.Nodes[ino] == n, n.Local, n.frozen
		if alive && local && frozen == nil && keep != noCopy {
			n.SHA256, n.Blocks = "", nil
		}
		f.treeMu.Unlock()
		switch {
		case !alive:
			unlock()
			return nil, nil, fuse.ENOENT
		case change && frozen != nil:
			unlock()
			<-frozen
			continue
		case (local && n.file != nil) || (!local && keep == noCopy):
			return n, unlock, fuse.OK
		case exclusive:
			if err := f.copyUp(ino, n, keep); err != nil {
				unlock()
				f.log.Error("copy a Volume file up", "error", err)
				return nil, nil, fuse.EIO
			}
			return n, unlock, fuse.OK
		}
		unlock()
		exclusive = true
	}
}

// truncate sets a file's size, copying up only the bytes it keeps.
func (f *volumeFS) truncate(ino uint64, size int64) fuse.Status {
	n, unlock, st := f.lockContent(ino, true, size)
	if !st.Ok() {
		return st
	}
	defer unlock()
	if err := n.file.Truncate(size); err != nil {
		f.log.Error("truncate Volume file", "error", err)
		return fuse.EIO
	}
	f.treeMu.Lock()
	defer f.treeMu.Unlock()
	n.Size, n.Mtime = size, now()
	f.wrote(n)
	return fuse.OK
}

func (f *volumeFS) GetAttr(_ <-chan struct{}, in *fuse.GetAttrIn, out *fuse.AttrOut) fuse.Status {
	f.treeMu.Lock()
	defer f.treeMu.Unlock()
	n, ok := f.st.Nodes[in.NodeId]
	if !ok {
		return fuse.ENOENT
	}
	out.SetTimeout(volumeTTL)
	f.fill(in.NodeId, n, &out.Attr)
	return fuse.OK
}

func (f *volumeFS) SetAttr(_ <-chan struct{}, in *fuse.SetAttrIn, out *fuse.AttrOut) fuse.Status {
	if size, ok := in.GetSize(); ok {
		if st := f.truncate(in.NodeId, int64(size)); !st.Ok() {
			return st
		}
	}
	f.treeMu.Lock()
	defer f.treeMu.Unlock()
	n, ok := f.st.Nodes[in.NodeId]
	if !ok {
		return fuse.ENOENT
	}
	if mode, ok := in.GetMode(); ok {
		n.Mode = mode & 0o7777
	}
	if uid, ok := in.GetUID(); ok {
		n.UID = uid
	}
	if gid, ok := in.GetGID(); ok {
		n.GID = gid
	}
	if in.Valid&fuse.FATTR_KILL_SUIDGID != 0 {
		n.Mode &^= syscall.S_ISUID | syscall.S_ISGID
	}
	if in.Valid&fuse.FATTR_MTIME_NOW != 0 {
		n.Mtime = now()
	} else if mtime, ok := in.GetMTime(); ok {
		n.Mtime = mtime.UnixNano()
	}
	f.changed(n)
	out.SetTimeout(volumeTTL)
	f.fill(in.NodeId, n, &out.Attr)
	return fuse.OK
}

// add creates a new inode named name in parent, owned by the caller, with
// a setgid parent's group (and, for a directory, its setgid bit).
func (f *volumeFS) add(h *fuse.InHeader, name string, n *vnode, out *fuse.EntryOut) fuse.Status {
	parent, st := f.dirNode(h.NodeId)
	if !st.Ok() {
		return st
	}
	if st := validName(name); !st.Ok() {
		return st
	}
	if _, exists := parent.index()[name]; exists {
		return errno(syscall.EEXIST)
	}
	t := now()
	n.UID, n.GID, n.Mtime, n.Ctime, n.Nlink = h.Uid, h.Gid, t, t, 1
	if parent.Mode&syscall.S_ISGID != 0 {
		n.GID = parent.GID
		if n.isDir() {
			n.Mode |= syscall.S_ISGID
		}
	}
	if n.isDir() {
		n.Nlink = 2
		parent.Nlink++
	}
	ino := f.st.NextIno
	f.st.NextIno++
	f.st.Nodes[ino] = n
	parent.addKid(name, ino)
	parent.Mtime = t
	f.changed(parent)
	f.entry(ino, out)
	return fuse.OK
}

func (f *volumeFS) Mknod(_ <-chan struct{}, in *fuse.MknodIn, name string, out *fuse.EntryOut) fuse.Status {
	f.treeMu.Lock()
	defer f.treeMu.Unlock()
	var t string
	switch in.Mode & syscall.S_IFMT {
	case syscall.S_IFREG:
		t = manifest.TypeFile
	case syscall.S_IFIFO:
		t = typeFIFO
	case syscall.S_IFSOCK:
		t = typeSocket
	default:
		return fuse.EPERM
	}
	return f.add(&in.InHeader, name, &vnode{Type: t, Mode: in.Mode & 0o7777, Local: t == manifest.TypeFile}, out)
}

func (f *volumeFS) Mkdir(_ <-chan struct{}, in *fuse.MkdirIn, name string, out *fuse.EntryOut) fuse.Status {
	f.treeMu.Lock()
	defer f.treeMu.Unlock()
	return f.add(&in.InHeader, name, &vnode{Type: manifest.TypeDir, Mode: in.Mode & 0o7777}, out)
}

func (f *volumeFS) Symlink(_ <-chan struct{}, h *fuse.InHeader, target, name string, out *fuse.EntryOut) fuse.Status {
	f.treeMu.Lock()
	defer f.treeMu.Unlock()
	return f.add(h, name, &vnode{Type: manifest.TypeSymlink, Mode: 0o777, Target: target}, out)
}

func (f *volumeFS) Create(_ <-chan struct{}, in *fuse.CreateIn, name string, out *fuse.CreateOut) fuse.Status {
	f.treeMu.Lock()
	defer f.treeMu.Unlock()
	return f.add(&in.InHeader, name, &vnode{Type: manifest.TypeFile, Mode: in.Mode & 0o7777, Local: true}, &out.EntryOut)
}

func (f *volumeFS) Link(_ <-chan struct{}, in *fuse.LinkIn, name string, out *fuse.EntryOut) fuse.Status {
	f.treeMu.Lock()
	defer f.treeMu.Unlock()
	parent, st := f.dirNode(in.NodeId)
	if !st.Ok() {
		return st
	}
	if st := validName(name); !st.Ok() {
		return st
	}
	n, ok := f.st.Nodes[in.Oldnodeid]
	switch {
	case !ok:
		return fuse.ENOENT
	case n.isDir():
		return fuse.EPERM
	}
	if _, exists := parent.index()[name]; exists {
		return errno(syscall.EEXIST)
	}
	n.Nlink++
	parent.addKid(name, in.Oldnodeid)
	parent.Mtime = now()
	f.changed(parent)
	f.changed(n)
	f.entry(in.Oldnodeid, out)
	return fuse.OK
}

// unlink removes parent's entry name, whose inode is n.
func (f *volumeFS) unlink(parent *vnode, name string, ino uint64, n *vnode) {
	parent.removeKid(name)
	parent.Mtime = now()
	f.changed(parent)
	n.Nlink--
	if n.isDir() {
		n.Nlink = 0
		parent.Nlink--
	}
	f.changed(n)
	f.maybeFree(ino, n)
}

func (f *volumeFS) remove(ino uint64, name string, dir bool) fuse.Status {
	f.treeMu.Lock()
	defer f.unlockTree()
	parent, st := f.dirNode(ino)
	if !st.Ok() {
		return st
	}
	d, ok := parent.index()[name]
	if !ok {
		return fuse.ENOENT
	}
	n := f.st.Nodes[d.Ino]
	switch {
	case dir && !n.isDir():
		return fuse.ENOTDIR
	case !dir && n.isDir():
		return errno(syscall.EISDIR)
	case dir && len(n.Kids) > 0:
		return errno(syscall.ENOTEMPTY)
	}
	f.unlink(parent, name, d.Ino, n)
	return fuse.OK
}

func (f *volumeFS) Unlink(_ <-chan struct{}, h *fuse.InHeader, name string) fuse.Status {
	return f.remove(h.NodeId, name, false)
}

func (f *volumeFS) Rmdir(_ <-chan struct{}, h *fuse.InHeader, name string) fuse.Status {
	return f.remove(h.NodeId, name, true)
}

// Rename moves oldName to newName, replacing a file or an empty directory
// there. The kernel refuses moving a directory into itself.
func (f *volumeFS) Rename(_ <-chan struct{}, in *fuse.RenameIn, oldName, newName string) fuse.Status {
	if in.Flags&^renameNoReplace != 0 {
		// shortcut: RENAME_EXCHANGE and RENAME_WHITEOUT get EINVAL, implement
		// them if a Sandbox workload needs them on a Volume.
		return fuse.EINVAL
	}
	f.treeMu.Lock()
	defer f.unlockTree()
	from, st := f.dirNode(in.NodeId)
	if !st.Ok() {
		return st
	}
	to, st := f.dirNode(in.Newdir)
	if !st.Ok() {
		return st
	}
	if st := validName(newName); !st.Ok() {
		return st
	}
	src, ok := from.index()[oldName]
	if !ok {
		return fuse.ENOENT
	}
	n := f.st.Nodes[src.Ino]
	if dst, exists := to.index()[newName]; exists {
		if in.Flags&renameNoReplace != 0 {
			return errno(syscall.EEXIST)
		}
		if dst.Ino == src.Ino {
			return fuse.OK // two names of one file: POSIX does nothing
		}
		old := f.st.Nodes[dst.Ino]
		switch {
		case n.isDir() && !old.isDir():
			return fuse.ENOTDIR
		case !n.isDir() && old.isDir():
			return errno(syscall.EISDIR)
		case old.isDir() && len(old.Kids) > 0:
			return errno(syscall.ENOTEMPTY)
		}
		f.unlink(to, newName, dst.Ino, old)
	}
	from.removeKid(oldName)
	to.addKid(newName, src.Ino)
	if n.isDir() && from != to {
		from.Nlink--
		to.Nlink++
	}
	t := now()
	from.Mtime, to.Mtime = t, t
	f.changed(from)
	f.changed(to)
	f.changed(n)
	return fuse.OK
}

func (f *volumeFS) Readlink(_ <-chan struct{}, h *fuse.InHeader) ([]byte, fuse.Status) {
	f.treeMu.Lock()
	defer f.treeMu.Unlock()
	n, ok := f.st.Nodes[h.NodeId]
	if !ok || n.Type != manifest.TypeSymlink {
		return nil, fuse.EINVAL
	}
	return []byte(n.Target), fuse.OK
}

func (f *volumeFS) GetXAttr(_ <-chan struct{}, h *fuse.InHeader, attr string, dest []byte) (uint32, fuse.Status) {
	f.treeMu.Lock()
	defer f.treeMu.Unlock()
	n, ok := f.st.Nodes[h.NodeId]
	if !ok {
		return 0, fuse.ENOENT
	}
	v, ok := n.Xattrs[attr]
	switch {
	case !ok:
		return 0, fuse.ENOATTR
	case len(dest) < len(v):
		return uint32(len(v)), fuse.ERANGE
	}
	return uint32(copy(dest, v)), fuse.OK
}

func (f *volumeFS) ListXAttr(_ <-chan struct{}, h *fuse.InHeader, dest []byte) (uint32, fuse.Status) {
	f.treeMu.Lock()
	defer f.treeMu.Unlock()
	n, ok := f.st.Nodes[h.NodeId]
	if !ok {
		return 0, fuse.ENOENT
	}
	names := make([]string, 0, len(n.Xattrs))
	for name := range n.Xattrs {
		names = append(names, name)
	}
	sort.Strings(names)
	var list []byte
	for _, name := range names {
		list = append(append(list, name...), 0)
	}
	if len(dest) < len(list) {
		return uint32(len(list)), fuse.ERANGE
	}
	return uint32(copy(dest, list)), fuse.OK
}

func (f *volumeFS) SetXAttr(_ <-chan struct{}, in *fuse.SetXAttrIn, attr string, data []byte) fuse.Status {
	f.treeMu.Lock()
	defer f.treeMu.Unlock()
	n, ok := f.st.Nodes[in.NodeId]
	if !ok {
		return fuse.ENOENT
	}
	_, exists := n.Xattrs[attr]
	switch {
	case attr == "":
		return fuse.EINVAL
	case in.Flags&xattrCreate != 0 && exists:
		return errno(syscall.EEXIST)
	case in.Flags&xattrReplace != 0 && !exists:
		return fuse.ENOATTR
	}
	if n.Xattrs == nil {
		n.Xattrs = map[string][]byte{}
	}
	n.Xattrs[attr] = append([]byte{}, data...)
	f.changed(n)
	return fuse.OK
}

func (f *volumeFS) RemoveXAttr(_ <-chan struct{}, h *fuse.InHeader, attr string) fuse.Status {
	f.treeMu.Lock()
	defer f.treeMu.Unlock()
	n, ok := f.st.Nodes[h.NodeId]
	if !ok {
		return fuse.ENOENT
	}
	if _, exists := n.Xattrs[attr]; !exists {
		return fuse.ENOATTR
	}
	delete(n.Xattrs, attr)
	f.changed(n)
	return fuse.OK
}

// Open allocates no handle: Read and Write go by node ID, so open files
// survive a handover. O_TRUNC is applied here when the kernel sends it.
func (f *volumeFS) Open(_ <-chan struct{}, in *fuse.OpenIn, _ *fuse.OpenOut) fuse.Status {
	f.treeMu.Lock()
	n, ok := f.st.Nodes[in.NodeId]
	dir := ok && n.isDir()
	trunc := ok && in.Flags&syscall.O_TRUNC != 0 && n.Type == manifest.TypeFile && n.Size > 0
	f.treeMu.Unlock()
	switch {
	case !ok:
		return fuse.ENOENT
	case dir:
		return errno(syscall.EISDIR)
	case trunc:
		return f.truncate(in.NodeId, 0)
	}
	return fuse.OK
}

func (f *volumeFS) Read(_ <-chan struct{}, in *fuse.ReadIn, dest []byte) (fuse.ReadResult, fuse.Status) {
	n, unlock, st := f.lockContent(in.NodeId, false, noCopy)
	if !st.Ok() {
		return nil, st
	}
	f.treeMu.Lock()
	local, size, entry := n.Local, n.Size, contentEntry(n)
	f.treeMu.Unlock()
	off := int64(in.Offset)
	end := min(off+int64(in.Size), size)
	if off >= end {
		unlock()
		return fuse.ReadResultData(nil), fuse.OK
	}
	dest = dest[:end-off]
	if local {
		defer unlock()
		got, _ := n.file.ReadAt(dest, off)
		clear(dest[got:]) // a hole past the data file's end
		return fuse.ReadResultData(dest), fuse.OK
	}
	unlock() // committed objects never change
	got, err := f.readCommitted(entry, off, dest)
	if err != nil {
		f.log.Error("read Volume block", "error", err)
		return nil, fuse.EIO
	}
	return fuse.ReadResultData(dest[:got]), fuse.OK
}

// writeAt is pwrite(2); a test holds one partway.
var writeAt = (*os.File).WriteAt

func (f *volumeFS) Write(_ <-chan struct{}, in *fuse.WriteIn, data []byte) (uint32, fuse.Status) {
	n, unlock, st := f.lockContent(in.NodeId, false, math.MaxInt64)
	if !st.Ok() {
		return 0, st
	}
	defer unlock()
	written, err := writeAt(n.file, data, int64(in.Offset))
	if err != nil {
		return uint32(written), fuse.ToStatus(err)
	}
	f.treeMu.Lock()
	defer f.treeMu.Unlock()
	n.Size = max(n.Size, int64(in.Offset)+int64(written))
	n.Mtime = now()
	f.wrote(n)
	return uint32(written), fuse.OK
}

func (f *volumeFS) Fallocate(_ <-chan struct{}, in *fuse.FallocateIn) fuse.Status {
	if in.Mode != 0 {
		return errno(syscall.EOPNOTSUPP)
	}
	n, unlock, st := f.lockContent(in.NodeId, true, noCopy)
	if !st.Ok() {
		return st
	}
	defer unlock()
	f.treeMu.Lock()
	size := n.Size
	f.treeMu.Unlock()
	end := int64(in.Offset + in.Length)
	if end <= size {
		return fuse.OK
	}
	err := f.copyUp(in.NodeId, n, size)
	if err == nil {
		err = n.file.Truncate(end)
	}
	if err != nil {
		return fuse.EIO
	}
	f.treeMu.Lock()
	defer f.treeMu.Unlock()
	n.Size = end
	f.wrote(n)
	return fuse.OK
}

// syncFile is fsync(2); a test makes one slow.
var syncFile = (*os.File).Sync

// Fsync makes the local data durable on the Node; it does not wait for a
// commit (runtime.md §17).
func (f *volumeFS) Fsync(_ <-chan struct{}, in *fuse.FsyncIn) fuse.Status {
	n, unlock, st := f.lockContent(in.NodeId, false, noCopy)
	if !st.Ok() {
		return st
	}
	defer unlock()
	if n.file != nil {
		if err := syncFile(n.file); err != nil {
			return fuse.EIO
		}
	}
	return fuse.OK
}

func (f *volumeFS) Flush(<-chan struct{}, *fuse.FlushIn) fuse.Status    { return fuse.OK }
func (f *volumeFS) Release(<-chan struct{}, *fuse.ReleaseIn)            {}
func (f *volumeFS) FsyncDir(<-chan struct{}, *fuse.FsyncIn) fuse.Status { return fuse.OK }
func (f *volumeFS) ReleaseDir(*fuse.ReleaseIn)                          {}
func (f *volumeFS) OpenDir(_ <-chan struct{}, in *fuse.OpenIn, _ *fuse.OpenOut) fuse.Status {
	f.treeMu.Lock()
	defer f.treeMu.Unlock()
	_, st := f.dirNode(in.NodeId)
	return st
}

// ReadDir lists entries after in.Offset; offsets are cookies, which stay
// valid while other entries come and go.
func (f *volumeFS) ReadDir(_ <-chan struct{}, in *fuse.ReadIn, out *fuse.DirEntryList) fuse.Status {
	f.treeMu.Lock()
	defer f.treeMu.Unlock()
	n, st := f.dirNode(in.NodeId)
	if !st.Ok() {
		return st
	}
	i := sort.Search(len(n.Kids), func(i int) bool { return n.Kids[i].Cookie > in.Offset })
	for ; i < len(n.Kids); i++ {
		k := n.Kids[i]
		mode := typeBits(f.st.Nodes[k.Ino].Type)
		if !out.AddDirEntry(fuse.DirEntry{Mode: mode, Name: k.Name, Ino: k.Ino, Off: k.Cookie}) {
			break
		}
	}
	return fuse.OK
}

func (f *volumeFS) StatFs(_ <-chan struct{}, _ *fuse.InHeader, out *fuse.StatfsOut) fuse.Status {
	f.statfs(out)
	return fuse.OK
}
