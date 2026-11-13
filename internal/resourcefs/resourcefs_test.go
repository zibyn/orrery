package resourcefs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/zibyn/orrery/internal/manifest"
)

func TestBuildTreeIsDeterministic(t *testing.T) {
	o := newObjectServer(t)
	files := map[string][]byte{"a/x": []byte("1"), "a-b": []byte("2"), "a/y/z": []byte("3"), "c": []byte("4")}
	m, _ := o.tree(t, files)
	first, second := buildTree(m), buildTree(m)
	if !reflect.DeepEqual(first, second) {
		t.Fatal("two builds of one manifest differ")
	}
	// IDs are sorted positions: "a" < "a-b" < "a/x" < "a/y" < "a/y/z" < "c".
	want := []string{"a", "a-b", "x", "y", "z", "c"}
	for i, name := range want {
		if first[i+2].name != name {
			t.Fatalf("node %d = %q, want %q", i+2, first[i+2].name, name)
		}
	}
	if got := first[rootID].kids; !reflect.DeepEqual(got, []uint64{2, 3, 7}) {
		t.Fatalf("root kids = %v", got)
	}
}

// read calls the FUSE Read handler for [off, off+n) of path.
func read(t *testing.T, f *rawFS, path string, off, n int64) ([]byte, fuse.Status) {
	t.Helper()
	for id := range f.nodes {
		if f.nodes[id].entry.Path == path {
			res, status := f.Read(nil, &fuse.ReadIn{InHeader: fuse.InHeader{NodeId: uint64(id)}, Offset: uint64(off), Size: uint32(n)}, make([]byte, n))
			if status != fuse.OK {
				return nil, status
			}
			data, _ := res.Bytes(nil)
			return data, status
		}
	}
	t.Fatalf("no node %q", path)
	return nil, 0
}

func newTestFS(t *testing.T, o *objectServer, m manifest.Manifest) *rawFS {
	s := newStore(filepath.Join(t.TempDir(), "blocks"), o)
	if err := s.cleanTemp(); err != nil {
		t.Fatal(err)
	}
	return newRawFS(m, s, testLog())
}

func TestReadAcrossBlocks(t *testing.T) {
	o := newObjectServer(t)
	big := pattern(9<<20, 1)
	m, _ := o.tree(t, map[string][]byte{"big": big})
	f := newTestFS(t, o, m)
	off := int64(4<<20) - 1000 // straddles blocks 0 and 1
	got, status := read(t, f, "big", off, 1<<20)
	if status != fuse.OK || !bytes.Equal(got, big[off:off+1<<20]) {
		t.Fatalf("read across blocks: status %v, %d bytes", status, len(got))
	}
	got, _ = read(t, f, "big", 9<<20-10, 100) // short read at EOF
	if !bytes.Equal(got, big[9<<20-10:]) {
		t.Fatalf("read at EOF returned %d bytes", len(got))
	}
}

func TestDigestMismatchRetriesThenEIO(t *testing.T) {
	o := newObjectServer(t)
	m, _ := o.tree(t, map[string][]byte{"f": []byte("hello")})
	o.corrupt = true
	f := newTestFS(t, o, m)
	if _, status := read(t, f, "f", 0, 5); status != fuse.EIO {
		t.Fatalf("status = %v, want EIO", status)
	}
	if got := o.gets[m.Entries[0].SHA256]; got != 1+getRetries {
		t.Fatalf("GETs = %d, want %d", got, 1+getRetries)
	}
	// A recovered store serves the block on the next read.
	o.corrupt = false
	if got, status := read(t, f, "f", 0, 5); status != fuse.OK || string(got) != "hello" {
		t.Fatalf("after recovery: %q %v", got, status)
	}
}

func TestPresignBatchesFollowingBlocks(t *testing.T) {
	o := newObjectServer(t)
	files := map[string][]byte{}
	for i := 0; i < 2500; i++ {
		files[fmt.Sprintf("d/%04d", i)] = []byte(fmt.Sprint(i))
	}
	m, _ := o.tree(t, files)
	f := newTestFS(t, o, m)
	if got, _ := read(t, f, "d/0000", 0, 10); string(got) != "0" {
		t.Fatalf("read = %q", got)
	}
	for i := 1; i < 1000; i++ {
		read(t, f, fmt.Sprintf("d/%04d", i), 0, 10)
	}
	if o.presigns != 1 || len(o.batches[0]) != presignBatch {
		t.Fatalf("presigns = %d, first batch %d, want one batch of %d", o.presigns, len(o.batches[0]), presignBatch)
	}
	read(t, f, "d/1000", 0, 10)
	for _, batch := range o.batches {
		if len(batch) > presignBatch {
			t.Fatalf("batch of %d", len(batch))
		}
	}
	if o.presigns != 2 {
		t.Fatalf("presigns = %d after the 1001st file, want 2", o.presigns)
	}
}

func TestServePresignRefusesOverlongBatches(t *testing.T) {
	o := newObjectServer(t)
	socket := filepath.Join(t.TempDir(), "p.sock")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ServePresign(ctx, l, o, testLog())
	p := SocketPresigner{Socket: socket}
	if urls, err := p.Presign(ctx, []string{"a", "b"}, nil); err != nil || len(urls) != 2 {
		t.Fatalf("presign = %v %v", urls, err)
	}
	if _, err := p.Presign(ctx, make([]string, presignBatch+1), nil); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("presign of %d = %v, want refusal", presignBatch+1, err)
	}
}

func TestLegacyLargeFileReadsByRange(t *testing.T) {
	o := newObjectServer(t)
	big := pattern(9<<20, 3)
	size := int64(len(big))
	m, err := manifest.Normalize(manifest.Manifest{Entries: []manifest.Entry{
		{Path: "old", Type: manifest.TypeFile, Size: &size, SHA256: o.put(big)},
	}})
	if err != nil {
		t.Fatal(err)
	}
	f := newTestFS(t, o, m)
	off := int64(8<<20) - 7
	got, status := read(t, f, "old", off, 4096)
	if status != fuse.OK || !bytes.Equal(got, big[off:off+4096]) {
		t.Fatalf("legacy read: %v, %d bytes", status, len(got))
	}
	// Two 4 MiB ranges were fetched, not the whole object twice.
	if n := o.gets[m.Entries[0].SHA256]; n != 2 {
		t.Fatalf("GETs = %d, want 2 ranges", n)
	}
}

// Eviction may delete a block between the cache check and the read.
func TestReadRefetchesAnEvictedBlock(t *testing.T) {
	o := newObjectServer(t)
	m, _ := o.tree(t, map[string][]byte{"f": []byte("hello")})
	f := newTestFS(t, o, m)
	if got, _ := read(t, f, "f", 0, 5); string(got) != "hello" {
		t.Fatalf("first read = %q", got)
	}
	ref := refAt(m.Entries[0], 0)
	if err := os.Remove(f.store.path(ref.key)); err != nil {
		t.Fatal(err)
	}
	if err := f.store.readBlock(ref, 0, make([]byte, 5)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("readBlock after eviction = %v, want ErrNotExist", err)
	}
	if got, status := read(t, f, "f", 0, 5); status != fuse.OK || string(got) != "hello" {
		t.Fatalf("read after eviction = %q %v", got, status)
	}
}
