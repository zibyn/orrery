package resourcefs

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"testing"

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
	if s.Manifest == nil {
		return s, nil
	}
	var lines []string
	for _, e := range s.Manifest.Entries {
		line := fmt.Sprintf("%s %s %o %s", e.Path, e.Type, e.Mode, e.Target)
		if e.Type == manifest.TypeFile {
			line += " " + e.SHA256
		}
		lines = append(lines, line)
	}
	return s, lines
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
