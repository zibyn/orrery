package resourcefs

import (
	"bytes"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/zibyn/orrery/internal/generation"
	"github.com/zibyn/orrery/internal/manifest"
)

// vol drives a volumeFS through its FUSE operations, as the kernel would.
type vol struct {
	t *testing.T
	f *volumeFS
}

func caller() fuse.InHeader {
	return fuse.InHeader{Caller: fuse.Caller{Owner: fuse.Owner{Uid: 1000, Gid: 1000}}}
}

func hdr(ino uint64) fuse.InHeader { h := caller(); h.NodeId = ino; return h }

// ino resolves a path by Lookup.
func (v vol) ino(path string) uint64 {
	v.t.Helper()
	id := uint64(rootID)
	if path == "" {
		return id
	}
	for _, name := range strings.Split(path, "/") {
		var out fuse.EntryOut
		h := hdr(id)
		if st := v.f.Lookup(nil, &h, name, &out); st != fuse.OK {
			v.t.Fatalf("lookup %s: %v", path, st)
		}
		id = out.NodeId
	}
	return id
}

func split(path string) (string, string) {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[:i], path[i+1:]
	}
	return "", path
}

func (v vol) check(op string, st fuse.Status) {
	v.t.Helper()
	if st != fuse.OK {
		v.t.Fatalf("%s: %v", op, st)
	}
}

func (v vol) create(path string, data string) {
	v.t.Helper()
	dir, name := split(path)
	var out fuse.CreateOut
	v.check("create "+path, v.f.Create(nil, &fuse.CreateIn{InHeader: hdr(v.ino(dir)), Mode: 0o644}, name, &out))
	v.write(path, 0, data)
}

func (v vol) write(path string, off int, data string) {
	v.t.Helper()
	n, st := v.f.Write(nil, &fuse.WriteIn{InHeader: hdr(v.ino(path)), Offset: uint64(off)}, []byte(data))
	v.check("write "+path, st)
	if int(n) != len(data) {
		v.t.Fatalf("wrote %d of %d", n, len(data))
	}
}

func (v vol) read(path string, off, n int) string {
	v.t.Helper()
	res, st := v.f.Read(nil, &fuse.ReadIn{InHeader: hdr(v.ino(path)), Offset: uint64(off), Size: uint32(n)}, make([]byte, n))
	v.check("read "+path, st)
	data, _ := res.Bytes(nil)
	return string(data)
}

func (v vol) mkdir(path string) {
	v.t.Helper()
	dir, name := split(path)
	var out fuse.EntryOut
	v.check("mkdir "+path, v.f.Mkdir(nil, &fuse.MkdirIn{InHeader: hdr(v.ino(dir)), Mode: 0o755}, name, &out))
}

func (v vol) rename(from, to string) fuse.Status {
	v.t.Helper()
	fd, fn := split(from)
	td, tn := split(to)
	h := hdr(v.ino(fd))
	return v.f.Rename(nil, &fuse.RenameIn{InHeader: h, Newdir: v.ino(td)}, fn, tn)
}

func (v vol) link(from, to string) {
	v.t.Helper()
	td, tn := split(to)
	var out fuse.EntryOut
	v.check("link "+to, v.f.Link(nil, &fuse.LinkIn{InHeader: hdr(v.ino(td)), Oldnodeid: v.ino(from)}, tn, &out))
}

func (v vol) unlink(path string) {
	v.t.Helper()
	dir, name := split(path)
	h := hdr(v.ino(dir))
	v.check("unlink "+path, v.f.Unlink(nil, &h, name))
}

// seal commits nothing; it returns the tree as entries "path type mode
// target content-digest", and the manifest.
func (v vol) seal() (Sealed, []string) {
	v.t.Helper()
	s, err := v.f.seal()
	if err != nil {
		v.t.Fatal(err)
	}
	return s, treeLines(s)
}

func treeLines(s Sealed) []string {
	if s.Manifest == nil {
		return nil
	}
	var lines []string
	for _, e := range s.Manifest.Entries {
		line := fmt.Sprintf("%s %s %o %s", e.Path, e.Type, e.Mode, e.Target)
		if e.Type == manifest.TypeFile {
			line += " " + e.SHA256
		}
		lines = append(lines, line)
	}
	return lines
}

// goSeal seals in the background.
func goSeal(v vol) <-chan Sealed {
	sealed := make(chan Sealed, 1)
	go func() {
		s, err := v.f.seal()
		if err != nil {
			v.t.Error(err)
		}
		sealed <- s
	}()
	return sealed
}

// holdSeal stops a seal as it turns to path's frozen file, before hashing
// it, until release; wait returns once a seal is held there.
func holdSeal(v vol, path string) (wait, release func()) {
	ino := v.ino(path)
	held, done := make(chan struct{}), make(chan struct{})
	var once sync.Once
	beforeHash = func(i uint64) {
		if i == ino {
			once.Do(func() {
				close(held)
				select {
				case <-done:
				case <-time.After(5 * time.Second): // were others stuck behind it, free them to fail
				}
			})
		}
	}
	v.t.Cleanup(func() { beforeHash = func(uint64) {} })
	wait = func() {
		v.t.Helper()
		select {
		case <-held:
		case <-time.After(5 * time.Second):
			v.t.Fatalf("no seal reached %s", path)
		}
	}
	return wait, sync.OnceFunc(func() { close(done) })
}

// notYet fails if c delivers within a moment.
func notYet[T any](t *testing.T, c <-chan T, what string) {
	t.Helper()
	select {
	case <-c:
		t.Fatal(what)
	case <-time.After(50 * time.Millisecond):
	}
}

func newTestVolume(t *testing.T) (vol, *objectServer) {
	o := newObjectServer(t)
	big := pattern(9<<20, 3)
	var blocks []string
	for i := 0; i < manifest.BlockCount(int64(len(big))); i++ {
		start := int64(i) * manifest.BlockSize
		blocks = append(blocks, o.put(big[start:start+manifest.BlockLength(int64(len(big)), i)]))
	}
	size := func(n int) *int64 { s := int64(n); return &s }
	m, err := generation.Normalize(generation.Manifest{Kind: generation.KindTree, Entries: []generation.Entry{
		{Entry: manifest.Entry{Path: "docs", Type: manifest.TypeDir}, Mode: 0o755},
		{Entry: manifest.Entry{Path: "docs/a.txt", Type: manifest.TypeFile, Size: size(5), SHA256: o.put([]byte("alpha"))}, Mode: 0o644},
		{Entry: manifest.Entry{Path: "big", Type: manifest.TypeFile, Size: size(len(big)), Blocks: blocks}, Mode: 0o644},
		{Entry: manifest.Entry{Path: "keep", Type: manifest.TypeFile, Size: size(1), SHA256: o.put([]byte("k"))}, Mode: 0o644},
		{Entry: manifest.Entry{Path: "keep2", Type: generation.TypeHardlink, Target: "keep"}},
		{Entry: manifest.Entry{Path: "ln", Type: manifest.TypeSymlink, Target: "docs/a.txt"}, Mode: 0o777},
	}})
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	s := newStore(filepath.Join(root, "cache", "blocks"), o)
	if err := s.cleanTemp(); err != nil {
		t.Fatal(err)
	}
	f, err := newVolumeFS("0198c7a2-0000-7000-8000-000000000001", filepath.Join(root, "volumes", "v"), "base-gen", m, s, testLog())
	if err != nil {
		t.Fatal(err)
	}
	return vol{t: t, f: f}, o
}

func TestVolumeCommitsWhatWasWritten(t *testing.T) {
	v, _ := newTestVolume(t)
	big := pattern(9<<20, 3)
	if got := v.read("big", 5<<20, 10); got != string(big[5<<20:5<<20+10]) {
		t.Fatal("lazy read of a committed block returned other bytes")
	}
	if s, _ := v.seal(); s.Dirty {
		t.Fatal("an unchanged Volume is dirty")
	}

	v.write("docs/a.txt", 5, "+more") // change a committed file
	v.create("new", "fresh")          // a new file
	v.unlink("keep")                  // one name of a hard link
	v.check("rename", v.rename("docs/a.txt", "renamed.txt"))
	v.create("tmp", "replaces the symlink")
	v.check("rename over", v.rename("tmp", "ln")) // rename over an existing name
	v.link("renamed.txt", "docs/hard")
	v.mkdir("gone")
	h := hdr(rootID)
	v.check("rmdir", v.f.Rmdir(nil, &h, "gone"))
	var attr fuse.AttrOut
	v.check("chmod", v.f.SetAttr(nil, &fuse.SetAttrIn{SetAttrInCommon: fuse.SetAttrInCommon{
		InHeader: hdr(v.ino("big")), Valid: fuse.FATTR_MODE, Mode: 0o600}}, &attr))
	v.check("setxattr", v.f.SetXAttr(nil, &fuse.SetXAttrIn{InHeader: hdr(v.ino("docs"))}, "user.tag", []byte("x")))

	s, lines := v.seal()
	if !s.Dirty || s.Base != "base-gen" {
		t.Fatalf("seal = dirty %v base %q", s.Dirty, s.Base)
	}
	want := []string{
		"big file 600  " + digestOf(big),
		"docs dir 755 ",
		"docs/hard file 644  " + digest([]byte("alpha+more")),
		"keep2 file 644  " + digest([]byte("k")),
		"ln file 644  " + digest([]byte("replaces the symlink")),
		"new file 644  " + digest([]byte("fresh")),
		"renamed.txt hardlink 0 docs/hard",
	}
	if !reflect.DeepEqual(lines, want) {
		t.Fatalf("sealed tree:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
	docs := s.Manifest.Entries[1]
	if string(docs.Xattrs["user.tag"]) != "x" || docs.UID != 0 {
		t.Fatalf("docs = %+v", docs)
	}
	newEntry := s.Manifest.Entries[5]
	if newEntry.UID != 1000 || newEntry.GID != 1000 {
		t.Fatalf("a new file is owned by %d:%d, want the caller", newEntry.UID, newEntry.GID)
	}
	// Only the dirty set became new objects; "big" and "keep" kept theirs.
	wantPending := []string{digest([]byte("alpha+more")), digest([]byte("fresh")), digest([]byte("replaces the symlink"))}
	sort.Strings(wantPending)
	if !reflect.DeepEqual(s.Pending, wantPending) {
		t.Fatalf("pending = %v, want %v", s.Pending, wantPending)
	}
	if got := v.read("renamed.txt", 0, 100); got != "alpha+more" {
		t.Fatalf("a sealed file reads %q", got)
	}

	if err := v.f.committed("gen-2", s.Seq); err != nil {
		t.Fatal(err)
	}
	if again, _ := v.seal(); again.Dirty || again.Base != "gen-2" {
		t.Fatalf("after the commit: dirty %v base %q", again.Dirty, again.Base)
	}
	// The sealed objects are the block cache's now.
	if got := v.read("new", 0, 100); got != "fresh" {
		t.Fatalf("a committed file reads %q", got)
	}
	if _, err := os.Stat(v.f.store.path(digest([]byte("fresh")))); err != nil {
		t.Fatal("the sealed object did not move into the block cache")
	}
}

// digestOf is a blocked file's address, from its 4 MiB blocks.
func digestOf(data []byte) string {
	var blocks []string
	for i := 0; i < manifest.BlockCount(int64(len(data))); i++ {
		start := int64(i) * manifest.BlockSize
		blocks = append(blocks, digest(data[start:start+manifest.BlockLength(int64(len(data)), i)]))
	}
	return manifest.FileAddress(blocks)
}

func TestVolumeWriteDuringCommitStaysDirty(t *testing.T) {
	v, _ := newTestVolume(t)
	v.create("a", "1")
	s, _ := v.seal()
	v.write("a", 1, "2") // after the seal, before the commit lands
	if err := v.f.committed("gen-2", s.Seq); err != nil {
		t.Fatal(err)
	}
	again, lines := v.seal()
	if !again.Dirty {
		t.Fatal("a write after the seal was dropped from the next commit")
	}
	if want := "a file 644  " + digest([]byte("12")); lines[0] != want {
		t.Fatalf("a = %q, want %q", lines[0], want)
	}
}

func TestVolumeBigWriteSealsInBlocks(t *testing.T) {
	v, _ := newTestVolume(t)
	data := pattern(6<<20, 9)
	v.create("large", "")
	v.write("large", 0, string(data))
	_, lines := v.seal()
	if want := "large file 644  " + digestOf(data); !contains(lines, want) {
		t.Fatalf("tree %v lacks %q", lines, want)
	}
	if got := v.read("large", 4<<20, 8); got != string(data[4<<20:4<<20+8]) {
		t.Fatal("the second sealed block reads other bytes")
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func TestVolumeTruncateAndUnlinkedOpenFile(t *testing.T) {
	v, _ := newTestVolume(t)
	ino := v.ino("docs/a.txt") // the kernel's reference, as an open file holds
	var attr fuse.AttrOut
	v.check("truncate", v.f.SetAttr(nil, &fuse.SetAttrIn{SetAttrInCommon: fuse.SetAttrInCommon{
		InHeader: hdr(ino), Valid: fuse.FATTR_SIZE, Size: 3}}, &attr))
	v.unlink("docs/a.txt")
	res, st := v.f.Read(nil, &fuse.ReadIn{InHeader: hdr(ino), Size: 10}, make([]byte, 10))
	v.check("read unlinked", st)
	if got, _ := res.Bytes(nil); string(got) != "alp" {
		t.Fatalf("an unlinked open file reads %q", got)
	}
	v.f.Forget(ino, 1)
	if _, ok := v.f.st.Nodes[ino]; ok {
		t.Fatal("the inode outlived its last name and reference")
	}
	if _, err := os.Stat(v.f.dataPath(ino)); !os.IsNotExist(err) {
		t.Fatal("its local data stayed")
	}
}

// A slow fsync holds up only its own file (runtime.md §17): open, read,
// write and stat of another file answer while it runs.
func TestVolumeFsyncBlocksOnlyItsFile(t *testing.T) {
	v, _ := newTestVolume(t)
	v.create("checkpoint", "weights")
	v.create("notes", "draft")
	slow, notes := v.ino("checkpoint"), v.ino("notes")
	entered, release := make(chan struct{}), make(chan struct{})
	syncFile = func(file *os.File) error {
		if file.Name() == v.f.dataPath(slow) {
			close(entered)
			select {
			case <-release:
			case <-time.After(5 * time.Second): // were notes stuck behind it, free them to fail below
			}
		}
		return file.Sync()
	}
	t.Cleanup(func() { syncFile = (*os.File).Sync })
	synced := make(chan fuse.Status, 1)
	go func() { synced <- v.f.Fsync(nil, &fuse.FsyncIn{InHeader: hdr(slow)}) }()
	<-entered

	v.check("open", v.f.Open(nil, &fuse.OpenIn{InHeader: hdr(notes)}, &fuse.OpenOut{}))
	v.write("notes", 5, " two")
	if got := v.read("notes", 0, 100); got != "draft two" {
		t.Fatalf("notes read %q", got)
	}
	var attr fuse.AttrOut
	v.check("stat", v.f.GetAttr(nil, &fuse.GetAttrIn{InHeader: hdr(notes)}, &attr))
	if attr.Size != 9 {
		t.Fatalf("notes is %d bytes, want 9", attr.Size)
	}
	select {
	case <-synced:
		t.Fatal("notes waited for the fsync of checkpoint")
	default:
	}
	close(release)
	if st := <-synced; st != fuse.OK {
		t.Fatalf("fsync = %v", st)
	}
}

// Concurrent IO keeps POSIX results: files written side by side read back
// their own bytes, and writes racing truncates of one file leave a state some
// serial order of them would. Seals run meanwhile.
func TestVolumeConcurrentIO(t *testing.T) {
	v, _ := newTestVolume(t)
	const files, rounds, chunk, writers = 8, 100, 1000, 4
	inos := make([]uint64, files)
	for i := range inos {
		v.create(fmt.Sprintf("f%d", i), "")
		inos[i] = v.ino(fmt.Sprintf("f%d", i))
	}
	shared, big := v.ino("docs/a.txt"), v.ino("big") // committed, so the first change copies shared up
	write := func(ino uint64, off int, data []byte) {
		if n, st := v.f.Write(nil, &fuse.WriteIn{InHeader: hdr(ino), Offset: uint64(off)}, data); st != fuse.OK || int(n) != len(data) {
			t.Errorf("write %d at %d: %d, %v", ino, off, n, st)
		}
	}
	read := func(ino uint64, off, size int) []byte {
		res, st := v.f.Read(nil, &fuse.ReadIn{InHeader: hdr(ino), Offset: uint64(off), Size: uint32(size)}, make([]byte, size))
		if st != fuse.OK {
			t.Errorf("read %d at %d: %v", ino, off, st)
			return nil
		}
		data, _ := res.Bytes(nil)
		return data
	}
	stop, sealed := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(sealed)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := v.f.seal(); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	var wg sync.WaitGroup
	for i, ino := range inos {
		wg.Go(func() {
			for r := range rounds {
				data := pattern(chunk, byte(i*rounds+r))
				write(ino, r*chunk, data)
				if !bytes.Equal(read(ino, r*chunk, chunk), data) {
					t.Errorf("f%d round %d reads other bytes", i, r)
				}
				if st := v.f.Fsync(nil, &fuse.FsyncIn{InHeader: hdr(ino)}); st != fuse.OK {
					t.Errorf("fsync f%d: %v", i, st)
				}
			}
		})
	}
	// Each writer owns a region of shared; cuts land meanwhile.
	cuts := []uint64{0, 1500, 2500, writers*chunk - 1}
	for w := range writers {
		wg.Go(func() {
			for range rounds {
				write(shared, w*chunk, bytes.Repeat([]byte{byte('a' + w)}, chunk))
			}
		})
	}
	wg.Go(func() {
		for r := range rounds {
			var attr fuse.AttrOut
			in := &fuse.SetAttrIn{SetAttrInCommon: fuse.SetAttrInCommon{InHeader: hdr(shared), Valid: fuse.FATTR_SIZE, Size: cuts[r%len(cuts)]}}
			if st := v.f.SetAttr(nil, in, &attr); st != fuse.OK {
				t.Errorf("truncate: %v", st)
			}
		}
	})
	committed := pattern(9<<20, 3)
	wg.Go(func() {
		for r := range rounds {
			if off := r * 90_000; !bytes.Equal(read(big, off, 100), committed[off:off+100]) {
				t.Errorf("big at %d reads other bytes", off)
			}
		}
	})
	wg.Wait()
	close(stop)
	<-sealed

	_, lines := v.seal()
	for i := range inos {
		var want []byte
		for r := range rounds {
			want = append(want, pattern(chunk, byte(i*rounds+r))...)
		}
		if line := fmt.Sprintf("f%d file 644  %s", i, digest(want)); !contains(lines, line) {
			t.Errorf("f%d was not sealed with what was written to it", i)
		}
	}
	var attr fuse.AttrOut
	v.check("stat", v.f.GetAttr(nil, &fuse.GetAttrIn{InHeader: hdr(shared)}, &attr))
	got := read(shared, 0, writers*chunk)
	if uint64(len(got)) != attr.Size || (attr.Size%chunk != 0 && !slices.Contains(cuts, attr.Size)) {
		t.Fatalf("shared is %d bytes and reads %d: no serial order ends so", attr.Size, len(got))
	}
	// Serially, a region holds its writer's bytes then zeros where a cut
	// took them, and the region a write extended the file to is whole.
	for w := 0; w*chunk < len(got); w++ {
		region := got[w*chunk : min((w+1)*chunk, len(got))]
		rest := bytes.TrimLeft(region, string(rune('a'+w)))
		if len(bytes.TrimLeft(rest, "\x00")) != 0 || ((w+1)*chunk == len(got) && len(rest) != 0) {
			t.Fatalf("region %d of shared is %q", w, region)
		}
	}
}

// A write whose copy-up fails leaves the file reading its committed bytes.
func TestVolumeFailedCopyUpKeepsCommittedContent(t *testing.T) {
	v, o := newTestVolume(t)
	o.mu.Lock()
	o.corrupt = true
	o.mu.Unlock()
	if _, st := v.f.Write(nil, &fuse.WriteIn{InHeader: hdr(v.ino("docs/a.txt"))}, []byte("x")); st != fuse.EIO {
		t.Fatalf("write over a failed copy-up = %v, want EIO", st)
	}
	o.mu.Lock()
	o.corrupt = false
	o.mu.Unlock()
	if got := v.read("docs/a.txt", 0, 10); got != "alpha" {
		t.Fatalf("after a failed copy-up the file reads %q", got)
	}
}

func TestVolumeRefusals(t *testing.T) {
	v, _ := newTestVolume(t)
	v.create("f", "x")
	if st := v.f.Rename(nil, &fuse.RenameIn{InHeader: hdr(rootID), Newdir: rootID, Flags: renameNoReplace}, "f", "keep"); st != fuse.Status(syscall.EEXIST) {
		t.Fatalf("RENAME_NOREPLACE over a name = %v", st)
	}
	if st := v.rename("docs", "f"); st != fuse.ENOTDIR {
		t.Fatalf("a dir over a file = %v", st)
	}
	v.mkdir("e")
	v.create("e/x", "")
	if st := v.rename("docs", "e"); st != fuse.Status(syscall.ENOTEMPTY) {
		t.Fatalf("a dir over a non-empty dir = %v", st)
	}
	var out fuse.EntryOut
	if st := v.f.Mknod(nil, &fuse.MknodIn{InHeader: hdr(rootID), Mode: syscall.S_IFCHR | 0o600}, "dev", &out); st != fuse.EPERM {
		t.Fatalf("mknod of a device = %v", st)
	}
	if st := v.f.Mknod(nil, &fuse.MknodIn{InHeader: hdr(rootID), Mode: syscall.S_IFIFO | 0o600}, "pipe", &out); st != fuse.OK {
		t.Fatalf("mkfifo = %v", st)
	}
	if _, lines := v.seal(); contains(lines, "pipe fifo 600 ") {
		t.Fatal("a FIFO was committed")
	}
}

// A successor rebuilt from the paused state serves the same tree, with the
// same node IDs the kernel holds, and keeps the uncommitted writes.
func TestVolumeHandoverKeepsUncommittedState(t *testing.T) {
	v, _ := newTestVolume(t)
	v.create("draft", "unsaved")
	ino := v.ino("draft")
	if err := v.f.setUpload("upload-1", 7); err != nil {
		t.Fatal(err)
	}
	if err := v.f.pause(); err != nil {
		t.Fatal(err)
	}
	if _, err := v.f.seal(); err != ErrHandedOver {
		t.Fatalf("a paused mount sealed: %v", err)
	}
	next, err := loadVolumeFS(v.f.dir, v.f.store, testLog())
	if err != nil {
		t.Fatal(err)
	}
	w := vol{t: t, f: next}
	if got := w.ino("draft"); got != ino {
		t.Fatalf("draft is node %d, was %d", got, ino)
	}
	res, st := next.Read(nil, &fuse.ReadIn{InHeader: hdr(ino), Size: 20}, make([]byte, 20))
	w.check("read", st)
	if got, _ := res.Bytes(nil); string(got) != "unsaved" {
		t.Fatalf("draft reads %q after the handover", got)
	}
	s, _ := w.seal()
	if !s.Dirty || s.Upload != "upload-1" || s.UploadSeq != 7 {
		t.Fatalf("successor seal = %+v", s)
	}
}

// A seal hashing a large dirty file holds up no other file (runtime.md §17):
// open, read, write and create of others answer meanwhile.
func TestVolumeSealHoldsUpNoOtherFile(t *testing.T) {
	v, _ := newTestVolume(t)
	v.create("large", string(pattern(6<<20, 9)))
	wait, release := holdSeal(v, "large")
	defer release()
	sealed := goSeal(v)
	wait()

	v.check("open", v.f.Open(nil, &fuse.OpenIn{InHeader: hdr(v.ino("docs/a.txt"))}, &fuse.OpenOut{}))
	v.write("docs/a.txt", 5, "+more")
	if got := v.read("docs/a.txt", 0, 100); got != "alpha+more" {
		t.Fatalf("docs/a.txt reads %q", got)
	}
	if got := v.read("big", 5<<20, 10); got != string(pattern(9<<20, 3)[5<<20:5<<20+10]) {
		t.Fatal("a committed file reads other bytes")
	}
	v.create("late", "new")
	if got := v.read("late", 0, 10); got != "new" {
		t.Fatalf("late reads %q", got)
	}
	select {
	case <-sealed:
		t.Fatal("other files waited for the seal of large")
	default:
	}
	release()
	if s := <-sealed; !s.Dirty {
		t.Fatal("the seal found nothing dirty")
	}
}

// A seal is one point in time (runtime.md §17): what changes after it
// begins is absent or old in its manifest, and in the next one.
func TestVolumeSealIsAPointInTime(t *testing.T) {
	v, _ := newTestVolume(t)
	data := pattern(6<<20, 9)
	v.create("large", string(data))
	v.create("clean", "one")
	v.seal() // clean is a clean copy now
	v.write("large", 0, string(data[:1]))
	ino := v.ino("large")
	wait, release := holdSeal(v, "large")
	defer release()
	sealed := goSeal(v)
	wait()

	v.write("docs/a.txt", 5, "+more")
	v.write("clean", 3, " two")
	v.create("late", "new")
	v.check("rename", v.rename("keep", "moved"))
	v.unlink("large")
	forgot := make(chan struct{})
	go func() { v.f.Forget(ino, math.MaxUint64); close(forgot) }() // its last reference
	select {
	case <-forgot:
	case <-time.After(50 * time.Millisecond):
	}
	release()
	lines := treeLines(<-sealed)
	<-forgot
	for _, want := range []string{
		"clean file 644  " + digest([]byte("one")),
		"docs/a.txt file 644  " + digest([]byte("alpha")),
		"keep file 644  " + digest([]byte("k")),
		"large file 644  " + digestOf(data),
	} {
		if !contains(lines, want) {
			t.Errorf("the seal lacks %q", want)
		}
	}
	for _, path := range []string{"late", "moved"} {
		if slices.ContainsFunc(lines, func(l string) bool { return strings.HasPrefix(l, path+" ") }) {
			t.Errorf("the seal has %s, made after it began", path)
		}
	}

	_, lines = v.seal()
	for _, want := range []string{
		"clean file 644  " + digest([]byte("one two")),
		"docs/a.txt file 644  " + digest([]byte("alpha+more")),
		"late file 644  " + digest([]byte("new")),
		"moved hardlink 0 keep2",
	} {
		if !contains(lines, want) {
			t.Errorf("the next seal lacks %q", want)
		}
	}
	if slices.ContainsFunc(lines, func(l string) bool { return strings.HasPrefix(l, "large ") }) {
		t.Error("the next seal still has large")
	}
}

// A write to a frozen file waits until the seal has hashed that file, and
// goes into the next commit.
func TestVolumeWriteToFrozenFileWaitsForItsHash(t *testing.T) {
	v, _ := newTestVolume(t)
	data := pattern(6<<20, 9)
	v.create("large", string(data))
	ino := v.ino("large")
	wait, release := holdSeal(v, "large")
	defer release()
	sealed := goSeal(v)
	wait()

	wrote := make(chan fuse.Status, 1)
	go func() {
		_, st := v.f.Write(nil, &fuse.WriteIn{InHeader: hdr(ino), Offset: 1 << 20}, []byte("X"))
		wrote <- st
	}()
	notYet(t, wrote, "a write to a frozen file landed before its hash")
	release()
	if want := "large file 644  " + digestOf(data); !contains(treeLines(<-sealed), want) {
		t.Fatalf("the seal lacks %q", want)
	}
	if st := <-wrote; st != fuse.OK {
		t.Fatalf("write = %v", st)
	}
	data[1<<20] = 'X'
	if _, lines := v.seal(); !contains(lines, "large file 644  "+digestOf(data)) {
		t.Fatal("the next seal lacks the write")
	}
}

// After a commit, an append copies nothing: the write-cache file stays, its
// bytes untouched, and the next seal hashes the whole file.
func TestVolumeAppendAfterCommitCopiesNothing(t *testing.T) {
	v, _ := newTestVolume(t)
	v.create("log", "one ")
	path := v.f.dataPath(v.ino("log"))
	s, _ := v.seal()
	if err := v.f.committed("gen-2", s.Seq); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal("the commit dropped the write-cache file")
	}
	v.write("log", 4, "two")
	after, err := os.Stat(path)
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("the append replaced the write-cache file")
	}
	if raw, _ := os.ReadFile(path); string(raw) != "one two" {
		t.Fatalf("the write-cache file holds %q", raw)
	}
	if got := v.read("log", 0, 100); got != "one two" {
		t.Fatalf("log reads %q", got)
	}
	if _, lines := v.seal(); !contains(lines, "log file 644  "+digest([]byte("one two"))) {
		t.Fatalf("the next seal lacks the append: %v", lines)
	}
}

// A clean copy unwritten for a commit period is dropped; the file then
// reads the same bytes from the block cache.
func TestVolumeUnwrittenCleanCopyIsDropped(t *testing.T) {
	v, _ := newTestVolume(t)
	data := pattern(5<<20, 7)
	v.create("ckpt", string(data))
	path := v.f.dataPath(v.ino("ckpt"))
	s, _ := v.seal()
	if err := v.f.committed("gen-2", s.Seq); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("the commit dropped the write-cache file")
	}
	if s, _ := v.seal(); s.Dirty { // the next periodic commit: nothing changed
		t.Fatal("an unchanged Volume is dirty")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("a clean copy unwritten for a commit stayed")
	}
	if got := v.read("ckpt", 0, len(data)); got != string(data) {
		t.Fatal("after the drop ckpt reads other bytes")
	}
}

// Moving a commit's objects into the block cache holds up no other op.
func TestVolumeCommittedMovesObjectsOffTheTreeLock(t *testing.T) {
	v, _ := newTestVolume(t)
	v.create("a", "1")
	s, _ := v.seal()
	entered, done := make(chan struct{}), make(chan struct{})
	var once sync.Once
	moveObject = func(from, to string) error {
		once.Do(func() {
			close(entered)
			select {
			case <-done:
			case <-time.After(5 * time.Second): // were others stuck behind it, free them to fail
			}
		})
		return os.Rename(from, to)
	}
	t.Cleanup(func() { moveObject = os.Rename })
	committed := make(chan error, 1)
	go func() { committed <- v.f.committed("gen-2", s.Seq) }()
	<-entered

	v.create("b", "2")
	if got := v.read("a", 0, 10); got != "1" {
		t.Fatalf("a reads %q", got)
	}
	select {
	case <-committed:
		t.Fatal("other ops waited for the objects to move")
	default:
	}
	close(done)
	if err := <-committed; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(v.f.store.path(digest([]byte("1")))); err != nil {
		t.Fatal("the sealed object did not move into the block cache")
	}
}

// A handover during a seal waits for it, then hands every uncommitted write
// over: those the seal hashed, and those made while it ran.
func TestVolumeHandoverDuringSealKeepsUncommittedWrites(t *testing.T) {
	v, _ := newTestVolume(t)
	data := pattern(6<<20, 9)
	v.create("draft", "unsaved")
	v.create("large", string(data))
	wait, release := holdSeal(v, "large")
	defer release()
	sealed := goSeal(v)
	wait()
	v.write("docs/a.txt", 5, "+more")
	v.create("late", "new")
	paused := make(chan error, 1)
	go func() { paused <- v.f.pause() }()
	notYet(t, paused, "the handover did not wait for the seal")
	release()
	<-sealed
	if err := <-paused; err != nil {
		t.Fatal(err)
	}

	next, err := loadVolumeFS(v.f.dir, v.f.store, testLog())
	if err != nil {
		t.Fatal(err)
	}
	w := vol{t: t, f: next}
	want := map[string]string{"draft": "unsaved", "large": string(data), "docs/a.txt": "alpha+more", "late": "new"}
	for path, content := range want {
		if got := w.read(path, 0, len(content)+1); got != content {
			t.Errorf("%s reads %d bytes after the handover, want %d", path, len(got), len(content))
		}
	}
	_, lines := w.seal()
	for path, content := range want {
		d := digest([]byte(content))
		if path == "large" {
			d = digestOf(data)
		}
		if line := fmt.Sprintf("%s file 644  %s", path, d); !contains(lines, line) {
			t.Errorf("the successor's seal lacks %q", line)
		}
	}
}
