package resourcefs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"
	"golang.org/x/sync/errgroup"

	"github.com/zibyn/orrery/internal/generation"
	"github.com/zibyn/orrery/internal/manifest"
)

// A writable Volume mount (runtime.md §17). The tree and every inode's
// metadata live in memory; file content is either the committed CAS objects
// (read lazily through the block cache, as a Resource) or, once written, a
// local file under <state>/data. A commit seals each dirty local file into
// CAS objects under <state>/objects, keeping the file as a clean copy; the
// objects move into the block cache once the Generation is committed. Node
// IDs, lookup counts and directory cookies are part of the state, and
// handles carry nothing, so a successor process takes the mount over from a
// serialized copy (ADR-0030's handover).

// Inode types beyond the Generation's: FIFOs and sockets work inside a
// session but are not committed, as the upperdir scanner skips them.
const (
	typeFIFO   = "fifo"
	typeSocket = "socket"
)

// volumeTTL is the kernel's entry and attribute cache lifetime. Every change
// comes through the kernel, so it could be longer; it costs little (spike).
const volumeTTL = time.Second

type dirent struct {
	Name   string `json:"name"`
	Ino    uint64 `json:"ino"`
	Cookie uint64 `json:"cookie"`
}

// vnode is one inode. A file's content is the committed SHA256/Blocks, or
// Local (data/<ino>): dirty without digests, a clean copy of them once
// sealed. A directory's Kids are in cookie order, which is its readdir
// offset order, so removals never shift other entries.
type vnode struct {
	Type    string            `json:"type"`
	Mode    uint32            `json:"mode"` // 07777
	UID     uint32            `json:"uid"`
	GID     uint32            `json:"gid"`
	Mtime   int64             `json:"mtime"` // Unix ns
	Ctime   int64             `json:"ctime"`
	Xattrs  map[string][]byte `json:"xattrs,omitempty"`
	Target  string            `json:"target,omitempty"`
	Size    int64             `json:"size,omitempty"`
	SHA256  string            `json:"sha256,omitempty"`
	Blocks  []string          `json:"blocks,omitempty"`
	Local   bool              `json:"local,omitempty"`
	Nlink   uint32            `json:"nlink"`
	Lookups uint64            `json:"lookups,omitempty"`
	Kids    []dirent          `json:"kids,omitempty"`
	Cookie  uint64            `json:"cookie,omitempty"` // the last cookie given out

	names map[string]dirent // index of Kids, rebuilt on demand

	// mu orders a file's content IO (shared) with its truncate, fallocate,
	// copy-up, seal and drop (exclusive); file is its open write-cache file
	// under mu, nil until first used.
	mu   sync.RWMutex
	file *os.File
	// frozen, set under the tree lock, is open from a seal's snapshot until
	// the seal has hashed the file: its content must stay as of then.
	frozen chan struct{}
}

func (n *vnode) isDir() bool { return n.Type == manifest.TypeDir }

// dirty reports content written since the last seal; f.mu is held.
func (n *vnode) dirty() bool { return n.Local && n.SHA256 == "" }

func (n *vnode) index() map[string]dirent {
	if n.names == nil {
		n.names = make(map[string]dirent, len(n.Kids))
		for _, k := range n.Kids {
			n.names[k.Name] = k
		}
	}
	return n.names
}

func (n *vnode) addKid(name string, ino uint64) {
	n.Cookie++
	d := dirent{Name: name, Ino: ino, Cookie: n.Cookie}
	n.Kids = append(n.Kids, d)
	n.index()[name] = d
}

func (n *vnode) removeKid(name string) {
	d := n.index()[name]
	i := sort.Search(len(n.Kids), func(i int) bool { return n.Kids[i].Cookie >= d.Cookie })
	n.Kids = append(n.Kids[:i], n.Kids[i+1:]...)
	delete(n.names, name)
}

// volumeState is everything a successor needs besides the files on disk.
type volumeState struct {
	Volume string `json:"volume"`
	// Base is the head Generation the tree descends from; empty before the
	// first commit.
	Base string `json:"base,omitempty"`
	// Upload is a commit sent but not acknowledged, at UploadSeq.
	Upload    string `json:"upload,omitempty"`
	UploadSeq uint64 `json:"upload_seq,omitempty"`
	// Seq counts changes; the tree is dirty while it is past CommittedSeq.
	Seq          uint64 `json:"seq"`
	CommittedSeq uint64 `json:"committed_seq"`
	// Pending are sealed objects not yet in a committed Generation.
	Pending map[string]int64  `json:"pending,omitempty"`
	NextIno uint64            `json:"next_ino"`
	Nodes   map[uint64]*vnode `json:"nodes"`
}

// volumeFS serves one Volume (runtime.md §17). The tree lock mu guards st:
// the inode table, directory entries and every inode's metadata, and is
// never held across disk IO. A file's content IO takes only that file's
// vnode.mu, then mu briefly to publish size and mtime; mu is never held
// while waiting for a vnode.mu. Lock order: sealMu, vnode.mu, mu.
type volumeFS struct {
	fuse.RawFileSystem
	dir   string // <data-root>/volumes/<id>; the mount is dir/mnt
	store *store
	log   *slog.Logger

	// sealMu orders seals, commits and a pause, so a pause never sees a
	// seal half done; st.Pending changes only under it.
	sealMu sync.Mutex
	mu     sync.Mutex
	st     volumeState
	freed  map[uint64]*vnode // freed under mu; unlockTree drops their files
	paused bool              // handed to a successor: commands answer ErrHandedOver
}

func volumeMountDir(dir string) string { return filepath.Join(dir, "mnt") }

func volumeStatePath(dir string) string { return filepath.Join(dir, "handover.json") }

func (f *volumeFS) dataPath(ino uint64) string {
	return filepath.Join(f.dir, "data", strconv.FormatUint(ino, 10))
}

func (f *volumeFS) objectPath(digest string) string { return filepath.Join(f.dir, "objects", digest) }

func (f *volumeFS) String() string { return "orrery-resourcefs-volume" }

func now() int64 { return time.Now().UnixNano() }

// newVolumeFS lays out a Volume's head Generation (an empty tree before the
// first commit) over a fresh state directory: what a crashed daemon left is
// uncommitted and lost (runtime.md §17).
func newVolumeFS(volume, dir, base string, m generation.Manifest, s *store, log *slog.Logger) (*volumeFS, error) {
	for _, sub := range []string{"data", "objects"} {
		if err := os.RemoveAll(filepath.Join(dir, sub)); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			return nil, err
		}
	}
	os.Remove(volumeStatePath(dir)) //nolint:errcheck // a stale handover copy
	t := now()
	st := volumeState{Volume: volume, Base: base, Nodes: map[uint64]*vnode{
		rootID: {Type: manifest.TypeDir, Mode: 0o777, Mtime: t, Ctime: t, Nlink: 2},
	}}
	ids := map[string]uint64{"": rootID}
	st.NextIno = rootID + 1
	var links []generation.Entry
	for _, e := range m.Entries {
		if e.Type == generation.TypeWhiteout {
			continue
		}
		parentPath, name := "", e.Path
		if cut := strings.LastIndexByte(e.Path, '/'); cut >= 0 {
			parentPath, name = e.Path[:cut], e.Path[cut+1:]
		}
		parent := st.Nodes[ids[parentPath]]
		if e.Type == generation.TypeHardlink {
			links = append(links, e)
			continue
		}
		n := &vnode{Type: e.Type, Mode: e.Mode, UID: e.UID, GID: e.GID, Mtime: e.Mtime, Ctime: e.Mtime,
			Xattrs: e.Xattrs, Target: e.Target, SHA256: e.SHA256, Blocks: e.Blocks, Nlink: 1}
		if e.Size != nil {
			n.Size = *e.Size
		}
		if n.isDir() {
			n.Nlink = 2
			parent.Nlink++
		}
		ino := st.NextIno
		st.NextIno++
		st.Nodes[ino] = n
		ids[e.Path] = ino
		parent.addKid(name, ino)
	}
	for _, e := range links {
		parentPath, name := "", e.Path
		if cut := strings.LastIndexByte(e.Path, '/'); cut >= 0 {
			parentPath, name = e.Path[:cut], e.Path[cut+1:]
		}
		ino := ids[e.Target]
		st.Nodes[ino].Nlink++
		st.Nodes[ids[parentPath]].addKid(name, ino)
	}
	return &volumeFS{RawFileSystem: fuse.NewDefaultRawFileSystem(), dir: dir, store: s, log: log, st: st}, nil
}

// loadVolumeFS rebuilds a mount from the state its predecessor wrote.
func loadVolumeFS(dir string, s *store, log *slog.Logger) (*volumeFS, error) {
	data, err := os.ReadFile(volumeStatePath(dir))
	if err != nil {
		return nil, err
	}
	var st volumeState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("decode Volume state: %w", err)
	}
	return &volumeFS{RawFileSystem: fuse.NewDefaultRawFileSystem(), dir: dir, store: s, log: log, st: st}, nil
}

// pause stops command access and writes the state for a successor. The
// FUSE session must already be drained; a seal in hand finishes first, so
// no file is handed over half sealed.
func (f *volumeFS) pause() error {
	f.sealMu.Lock()
	defer f.sealMu.Unlock()
	f.mu.Lock()
	defer f.mu.Unlock()
	data, err := json.Marshal(f.st)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(volumeStatePath(f.dir), data); err != nil {
		return err
	}
	f.paused = true
	for _, n := range f.st.Nodes {
		if n.file != nil {
			n.file.Close() //nolint:errcheck // read-write data file; content is already written
			n.file = nil
		}
	}
	return nil
}

// keeps reports whether a mount request for head finds this mount current:
// it descends from head, or holds writes not committed yet (a re-sent
// Create may name an older head than a periodic commit made since).
func (f *volumeFS) keeps(head string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.st.Base == head || f.st.Seq != f.st.CommittedSeq || f.st.Upload != ""
}

func (f *volumeFS) resume() {
	f.mu.Lock()
	f.paused = false
	f.mu.Unlock()
}

// openLocal opens n's write-cache file, creating it; n.mu is held
// exclusively.
func (f *volumeFS) openLocal(ino uint64, n *vnode) (*os.File, error) {
	if n.file == nil {
		file, err := os.OpenFile(f.dataPath(ino), os.O_RDWR|os.O_CREATE, 0o600)
		if err != nil {
			return nil, err
		}
		n.file = file
	}
	return n.file, nil
}

// dropLocal removes n's write-cache file; n.mu is held exclusively.
func (f *volumeFS) dropLocal(ino uint64, n *vnode) {
	if n.file != nil {
		n.file.Close() //nolint:errcheck // being removed
		n.file = nil
	}
	os.Remove(f.dataPath(ino)) //nolint:errcheck // absent for committed content
}

func contentEntry(n *vnode) manifest.Entry {
	size := n.Size
	return manifest.Entry{Size: &size, SHA256: n.SHA256, Blocks: n.Blocks}
}

// readRef copies block bytes [from, from+len(dst)): a sealed object not yet
// committed is read from objects/, anything else through the block cache.
func (f *volumeFS) readRef(ref blockRef, from int64, dst []byte, hint func() []blockRef) error {
	if file, err := os.Open(f.objectPath(ref.key)); err == nil {
		defer file.Close() //nolint:errcheck // read-only
		if n, err := file.ReadAt(dst, from); n == len(dst) {
			return nil
		} else if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		return fmt.Errorf("object %s is short", ref.key)
	}
	if err := f.store.ensure(ref, hint); err != nil {
		return err
	}
	err := f.store.readBlock(ref, from, dst)
	if errors.Is(err, fs.ErrNotExist) {
		// Evicted after ensure, or moved in by a commit meanwhile.
		if err = f.store.ensure(ref, hint); err == nil {
			err = f.store.readBlock(ref, from, dst)
		}
	}
	return err
}

// readCommitted reads committed content into dest at off.
func (f *volumeFS) readCommitted(entry manifest.Entry, off int64, dest []byte) (int, error) {
	end := min(off+int64(len(dest)), *entry.Size)
	done := 0
	hint := func(index int) func() []blockRef {
		return func() []blockRef {
			var refs []blockRef
			for i := index; i < refCount(entry) && len(refs) < presignBatch; i++ {
				refs = append(refs, refAt(entry, i))
			}
			return refs
		}
	}
	for pos := off; pos < end; {
		index := int(pos / manifest.BlockSize)
		ref := refAt(entry, index)
		from := pos - int64(index)*manifest.BlockSize
		chunk := min(end-pos, ref.length-from)
		if err := f.readRef(ref, from, dest[done:done+int(chunk)], hint(index)); err != nil {
			return done, err
		}
		done += int(chunk)
		pos += chunk
	}
	return done, nil
}

// copyUp opens n's write-cache file, first copying committed content into
// it, keeping its first limit bytes. n.mu is held exclusively, f.mu not.
func (f *volumeFS) copyUp(ino uint64, n *vnode, limit int64) error {
	f.mu.Lock()
	local, entry := n.Local, contentEntry(n)
	f.mu.Unlock()
	file, err := f.openLocal(ino, n)
	if err != nil || local {
		return err
	}
	if err := file.Truncate(0); err != nil {
		return err
	}
	keep := min(limit, *entry.Size)
	buf := make([]byte, min(keep, manifest.BlockSize))
	for off := int64(0); off < keep; {
		chunk := buf[:min(int64(len(buf)), keep-off)]
		if _, err := f.readCommitted(entry, off, chunk); err != nil {
			return err
		}
		if _, err := file.WriteAt(chunk, off); err != nil {
			return err
		}
		off += int64(len(chunk))
	}
	f.mu.Lock()
	n.Local, n.SHA256, n.Blocks, n.Size = true, "", nil, keep
	f.mu.Unlock()
	return nil
}

// Sealed is a Volume's state for a commit (runtime.md §17).
type Sealed struct {
	Base      string `json:"base,omitempty"`
	Upload    string `json:"upload,omitempty"`
	UploadSeq uint64 `json:"upload_seq,omitempty"`
	Seq       uint64 `json:"seq"`
	Dirty     bool   `json:"dirty"`
	// Manifest is the whole tree, set only when Dirty.
	Manifest *generation.Manifest `json:"manifest,omitempty"`
	// Pending are the sealed objects of earlier and this seal that no
	// committed Generation holds yet: what the store may lack.
	Pending []string `json:"pending,omitempty"`
}

// beforeHash runs as a seal turns to a frozen file, before taking its
// content lock; a test holds one there.
var beforeHash = func(uint64) {}

// seal returns the whole tree as a Generation, as of one point in time
// (runtime.md §17). Under the tree lock it walks the tree and freezes the
// dirty files; outside it, they are hashed into CAS objects in parallel,
// each under its own content lock, so a write waits only for its own
// frozen file. A clean copy a seal finds has gone a commit unwritten and is
// dropped.
func (f *volumeFS) seal() (Sealed, error) {
	f.sealMu.Lock()
	defer f.sealMu.Unlock()
	f.mu.Lock()
	if f.paused {
		f.mu.Unlock()
		return Sealed{}, ErrHandedOver
	}
	out := Sealed{Base: f.st.Base, Upload: f.st.Upload, UploadSeq: f.st.UploadSeq, Seq: f.st.Seq,
		Dirty: f.st.Seq != f.st.CommittedSeq}
	var clean, dirty []localFile
	for ino, n := range f.st.Nodes {
		if n.Local && !n.dirty() {
			clean = append(clean, localFile{ino: ino, n: n})
		}
	}
	var m generation.Manifest
	if out.Dirty {
		m, dirty = f.snapshot()
	}
	f.mu.Unlock()
	var g errgroup.Group
	g.SetLimit(min(runtime.NumCPU(), 8))
	for _, c := range clean {
		g.Go(func() error { f.dropClean(c.ino, c.n); return nil })
	}
	for _, d := range dirty {
		g.Go(func() error {
			if err := f.sealFile(d.ino, d.n, &m.Entries[d.entry].Entry); err != nil {
				return fmt.Errorf("seal %d: %w", d.ino, err)
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return Sealed{}, err
	}
	if !out.Dirty {
		return out, nil
	}
	normalized, err := generation.Normalize(m)
	if err != nil {
		return Sealed{}, err
	}
	out.Manifest = &normalized
	for digest := range f.st.Pending {
		out.Pending = append(out.Pending, digest)
	}
	sort.Strings(out.Pending)
	return out, nil
}

// localFile is a write-cache file a seal hashes or drops; entry is a
// hashed file's in the manifest.
type localFile struct {
	ino   uint64
	n     *vnode
	entry int
}

// dropClean drops n's clean copy unless it was written meanwhile; its reads
// go back to the block cache.
func (f *volumeFS) dropClean(ino uint64, n *vnode) {
	n.mu.Lock()
	defer n.mu.Unlock()
	f.mu.Lock()
	drop := f.st.Nodes[ino] == n && n.Local && !n.dirty()
	if drop {
		n.Local = false
	}
	f.mu.Unlock()
	if drop {
		f.dropLocal(ino, n)
	}
}

// snapshot walks the tree into a manifest with f.mu held, freezing each
// dirty file; its entry gets its content once hashed.
func (f *volumeFS) snapshot() (generation.Manifest, []localFile) {
	m := generation.Manifest{Kind: generation.KindTree}
	var dirty []localFile
	first := map[uint64]string{}
	var walk func(ino uint64, prefix string)
	walk = func(ino uint64, prefix string) {
		kids := append([]dirent(nil), f.st.Nodes[ino].Kids...)
		sort.Slice(kids, func(i, j int) bool { return kids[i].Name < kids[j].Name })
		for _, k := range kids {
			n := f.st.Nodes[k.Ino]
			path := prefix + k.Name
			e := generation.Entry{Entry: manifest.Entry{Path: path, Type: n.Type}}
			switch n.Type {
			case typeFIFO, typeSocket:
				continue
			case manifest.TypeFile:
				if target, ok := first[k.Ino]; ok {
					e.Type, e.Target = generation.TypeHardlink, target
					m.Entries = append(m.Entries, e)
					continue
				}
				first[k.Ino] = path
				if n.dirty() {
					n.frozen = make(chan struct{})
					dirty = append(dirty, localFile{ino: k.Ino, n: n, entry: len(m.Entries)})
				} else {
					size := n.Size
					e.Size, e.SHA256, e.Blocks = &size, n.SHA256, n.Blocks
				}
			case manifest.TypeSymlink:
				e.Target = n.Target
			}
			e.Mode, e.UID, e.GID, e.Mtime = n.Mode, n.UID, n.GID, n.Mtime
			if len(n.Xattrs) > 0 {
				e.Xattrs = maps.Clone(n.Xattrs) // read after f.mu is released
			}
			m.Entries = append(m.Entries, e)
			if n.isDir() {
				walk(k.Ino, path+"/")
			}
		}
	}
	walk(rootID, "")
	return m, dirty
}

// sealFile hashes frozen file n into objects/ under its content lock,
// points n and its manifest entry e at them and thaws n. The write-cache
// file stays as a clean copy, so the next write copies nothing.
func (f *volumeFS) sealFile(ino uint64, n *vnode, e *manifest.Entry) error {
	beforeHash(ino)
	n.mu.Lock()
	defer n.mu.Unlock()
	defer func() {
		f.mu.Lock()
		close(n.frozen)
		n.frozen = nil
		f.mu.Unlock()
	}()
	f.mu.Lock()
	size := n.Size
	f.mu.Unlock()
	file, err := f.openLocal(ino, n)
	if err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() != size {
		if err := file.Truncate(size); err != nil {
			return err
		}
	}
	objects := map[string]int64{}
	var blocks []string
	for i := range max(manifest.BlockCount(size), 1) {
		length := int64(0)
		if size > 0 {
			length = manifest.BlockLength(size, i)
		}
		digest, err := f.writeObject(io.NewSectionReader(file, int64(i)*manifest.BlockSize, length))
		if err != nil {
			return err
		}
		objects[digest] = length
		blocks = append(blocks, digest)
	}
	f.mu.Lock()
	if f.st.Pending == nil {
		f.st.Pending = map[string]int64{}
	}
	maps.Copy(f.st.Pending, objects)
	if size > manifest.BlockSize {
		n.Blocks, n.SHA256 = blocks, manifest.FileAddress(blocks)
	} else {
		n.Blocks, n.SHA256 = nil, blocks[0]
	}
	e.Size, e.SHA256, e.Blocks = &size, n.SHA256, n.Blocks
	f.mu.Unlock()
	return nil
}

func (f *volumeFS) writeObject(r io.Reader) (string, error) {
	tmp, err := os.CreateTemp(filepath.Join(f.dir, "objects"), ".tmp-")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name()) //nolint:errcheck // gone after the rename
	sum := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, sum), r); err != nil {
		tmp.Close() //nolint:errcheck // report the copy error
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	digest := hex.EncodeToString(sum.Sum(nil))
	return digest, os.Rename(tmp.Name(), f.objectPath(digest))
}

// setUpload records a CommitGeneration about to be sent for the seal at seq.
func (f *volumeFS) setUpload(upload string, seq uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.paused {
		return ErrHandedOver
	}
	f.st.Upload, f.st.UploadSeq = upload, seq
	return nil
}

// moveObject moves a sealed object into the block cache; a test makes one
// slow.
var moveObject = os.Rename

// committed makes generationID the base. An empty id only clears a pending
// upload that was refused. Sealed objects move into the block cache: they
// are now the store's. The moves hold sealMu, which guards Pending, not the
// tree lock.
func (f *volumeFS) committed(generationID string, seq uint64) error {
	f.sealMu.Lock()
	defer f.sealMu.Unlock()
	f.mu.Lock()
	if f.paused {
		f.mu.Unlock()
		return ErrHandedOver
	}
	f.st.Upload, f.st.UploadSeq = "", 0
	if generationID != "" {
		f.st.Base = generationID
		f.st.CommittedSeq = max(f.st.CommittedSeq, seq)
	}
	f.mu.Unlock()
	if generationID == "" {
		return nil
	}
	for digest := range f.st.Pending {
		// shortcut: rename needs objects/ and the block cache on one file
		// system (both are under the data root), copy instead if they split.
		if err := moveObject(f.objectPath(digest), f.store.path(digest)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		delete(f.st.Pending, digest)
	}
	return nil
}

func (f *volumeFS) changed(n *vnode) {
	f.st.Seq++
	n.Ctime = now()
}

// wrote records a change of n's content: a clean copy is dirty again.
func (f *volumeFS) wrote(n *vnode) {
	n.SHA256, n.Blocks = "", nil
	f.changed(n)
}

func (f *volumeFS) statfs(out *fuse.StatfsOut) {
	var s syscall.Statfs_t
	if syscall.Statfs(f.dir, &s) == nil {
		out.Blocks, out.Bfree, out.Bavail, out.Bsize = s.Blocks, s.Bfree, s.Bavail, uint32(s.Bsize)
		out.Frsize = uint32(s.Bsize)
	}
	f.mu.Lock()
	out.Files = uint64(len(f.st.Nodes))
	f.mu.Unlock()
	out.Ffree, out.NameLen = 1<<32, 255
}
