package manifest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func blocksOf(n int) []string {
	blocks := make([]string, n)
	for i := range blocks {
		sum := sha256.Sum256([]byte{byte(i)})
		blocks[i] = hex.EncodeToString(sum[:])
	}
	return blocks
}

func TestBlockCountAndLastLength(t *testing.T) {
	cases := []struct {
		size  int64
		count int
		last  int64
	}{
		{BlockSize + 1, 2, 1},
		{2 * BlockSize, 2, BlockSize},
		{9 << 20, 3, 1 << 20},
		{MaxFileSize, 262_144, BlockSize},
	}
	for _, tc := range cases {
		if got := BlockCount(tc.size); got != tc.count {
			t.Fatalf("BlockCount(%d) = %d, want %d", tc.size, got, tc.count)
		}
		if got := BlockLength(tc.size, tc.count-1); got != tc.last {
			t.Fatalf("BlockLength(%d, last) = %d, want %d", tc.size, got, tc.last)
		}
	}
}

func TestFileAddressFormula(t *testing.T) {
	blocks := blocksOf(3)
	var raw []byte
	for _, block := range blocks {
		b, _ := hex.DecodeString(block)
		raw = append(raw, b...)
	}
	want := sha256.Sum256(append([]byte("orrery-blocks-v1\n"), raw...))
	if got := FileAddress(blocks); got != hex.EncodeToString(want[:]) {
		t.Fatalf("FileAddress = %s, want %s", got, hex.EncodeToString(want[:]))
	}
	// A small file whose content is exactly the concatenated digests does
	// not collide with the blocked file.
	plain := sha256.Sum256(raw)
	if hex.EncodeToString(plain[:]) == FileAddress(blocks) {
		t.Fatal("a small file of the raw digests collides with the blocked file")
	}
}

func TestValidateBlockedEntries(t *testing.T) {
	nine := int64(9 << 20)
	good := Entry{Path: "f", Type: TypeFile, Size: &nine, Blocks: blocksOf(3)}
	normalized, err := Normalize(Manifest{Entries: []Entry{good}})
	if err != nil {
		t.Fatal(err)
	}
	if got := normalized.Entries[0].SHA256; got != FileAddress(good.Blocks) {
		t.Fatalf("normalized sha256 = %q, want FileAddress", got)
	}
	// The normalized form, sha256 included, validates again (stored manifests).
	if err := Validate(normalized); err != nil {
		t.Fatalf("normalized manifest: %v", err)
	}
	bad := map[string]Entry{
		"too few blocks":  {Path: "f", Type: TypeFile, Size: &nine, Blocks: blocksOf(2)},
		"too many blocks": {Path: "f", Type: TypeFile, Size: &nine, Blocks: blocksOf(4)},
		"bad digest":      {Path: "f", Type: TypeFile, Size: &nine, Blocks: append(blocksOf(2), "xyz")},
		"client sha256":   {Path: "f", Type: TypeFile, Size: &nine, Blocks: blocksOf(3), SHA256: digestA},
		"small file":      {Path: "f", Type: TypeFile, Size: size(BlockSize), Blocks: blocksOf(1)},
		"dir blocks":      {Path: "f", Type: TypeDir, Blocks: blocksOf(1)},
	}
	for name, entry := range bad {
		if err := Validate(Manifest{Entries: []Entry{entry}}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
}

func TestLegacyLargeFileValidatesButIsNotNew(t *testing.T) {
	legacy := Manifest{Entries: []Entry{{Path: "f", Type: TypeFile, Size: size(9 << 20), SHA256: digestA}}}
	if err := Validate(legacy); err != nil {
		t.Fatalf("legacy manifest: %v", err)
	}
	if objects := FileObjects(legacy); len(objects) != 1 || objects[0].SHA256 != digestA {
		t.Fatalf("legacy objects = %+v", objects)
	}
	if err := RequireBlocks(legacy); !errors.Is(err, ErrInvalid) {
		t.Fatalf("RequireBlocks(legacy) = %v, want ErrInvalid", err)
	}
}

func TestFileObjectsListsBlocks(t *testing.T) {
	nine := int64(9 << 20)
	blocks := blocksOf(3)
	m := Manifest{Entries: []Entry{
		{Path: "big", Type: TypeFile, Size: &nine, Blocks: blocks},
		{Path: "small", Type: TypeFile, Size: size(1 << 20), SHA256: blocks[2]}, // same bytes as the last block
	}}
	objects := FileObjects(m)
	if len(objects) != 3 {
		t.Fatalf("FileObjects = %+v, want 3 blocks", objects)
	}
	for _, object := range objects {
		want := BlockSize
		if object.SHA256 == blocks[2] {
			want = 1 << 20
		}
		if object.Size != want {
			t.Fatalf("object %s size = %d, want %d", object.SHA256, object.Size, want)
		}
	}
}

func TestWalkSplitsLargeFiles(t *testing.T) {
	dir := t.TempDir()
	big := bytes.Repeat([]byte("0123456789abcdef"), (9<<20)/16)
	if err := os.WriteFile(filepath.Join(dir, "big"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "small"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	m, objects, err := Walk(root.FS(), ".")
	if err != nil {
		t.Fatal(err)
	}
	if err := RequireBlocks(m); err != nil {
		t.Fatal(err)
	}
	entry := m.Entries[0]
	if entry.Path != "big" || len(entry.Blocks) != 3 || entry.SHA256 != FileAddress(entry.Blocks) {
		t.Fatalf("big entry = %+v", entry)
	}
	// The first two blocks have identical content and dedupe to one object.
	if entry.Blocks[0] != entry.Blocks[1] || len(objects) != 3 {
		t.Fatalf("objects = %+v", objects)
	}
	for i, block := range entry.Blocks {
		src := objects[block]
		sum := sha256.Sum256(big[src.Offset : src.Offset+src.Size])
		if src.Path != "big" || hex.EncodeToString(sum[:]) != block {
			t.Fatalf("block %d source = %+v", i, src)
		}
	}
	if small := m.Entries[1]; small.Blocks != nil || !strings.HasPrefix(objects[small.SHA256].Path, "small") {
		t.Fatalf("small entry = %+v", small)
	}
}
