package generation

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/zibyn/orrery/internal/manifest"
)

func size(n int64) *int64 { return &n }

var (
	digestA = strings.Repeat("a", 64)
	digestB = strings.Repeat("b", 64)
	block1  = strings.Repeat("1", 64)
	block2  = strings.Repeat("2", 64)
	block3  = strings.Repeat("3", 64)
)

func file(path string, n int64, digest string) Entry {
	return Entry{Entry: manifest.Entry{Path: path, Type: manifest.TypeFile, Size: size(n), SHA256: digest}, Mode: 0o644}
}

func dir(path string) Entry {
	return Entry{Entry: manifest.Entry{Path: path, Type: manifest.TypeDir}, Mode: 0o755}
}

// A diff Generation carries everything an upperdir can express: metadata,
// xattrs, hard links, whiteouts, opaque directories and blocked files.
func TestDiffRoundTrip(t *testing.T) {
	big := file("usr/lib/big.so", 9<<20, "")
	big.Blocks = []string{block1, block2, block3}
	big.Mode = 0o4755
	big.UID, big.GID = 1000, 100
	big.Mtime = 1_700_000_000_123_456_789
	big.Xattrs = map[string][]byte{"security.capability": {0x01, 0x00, 0xff}}
	opaque := dir("etc/app")
	opaque.Opaque = true
	m := Manifest{Kind: KindDiff, Entries: []Entry{
		{Entry: manifest.Entry{Path: "usr/lib/link.so", Type: TypeHardlink, Target: "usr/lib/big.so"}},
		dir("usr"), dir("usr/lib"), big,
		{Entry: manifest.Entry{Path: "usr/lib/old.so", Type: TypeWhiteout}},
		dir("etc"), opaque,
		file("etc/app/conf", 3, digestA),
		{Entry: manifest.Entry{Path: "etc/app/current", Type: manifest.TypeSymlink, Target: "conf"}, Mode: 0o777},
	}}

	data, err := Encode(m)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	again, err := Encode(got)
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != string(data) {
		t.Fatalf("re-encoding changed the bytes:\n%s\n%s", data, again)
	}
	if got.Kind != KindDiff || len(got.Entries) != len(m.Entries) {
		t.Fatalf("decoded = %+v", got)
	}
	byPath := map[string]Entry{}
	for _, e := range got.Entries {
		byPath[e.Path] = e
	}
	gotBig := byPath["usr/lib/big.so"]
	if gotBig.SHA256 != manifest.FileAddress(big.Blocks) {
		t.Fatalf("blocked file sha256 = %q, want the block formula", gotBig.SHA256)
	}
	if gotBig.Mode != 0o4755 || gotBig.UID != 1000 || gotBig.GID != 100 || gotBig.Mtime != big.Mtime ||
		!reflect.DeepEqual(gotBig.Xattrs, big.Xattrs) {
		t.Fatalf("metadata lost: %+v", gotBig)
	}
	if e := byPath["usr/lib/link.so"]; e.Type != TypeHardlink || e.Target != "usr/lib/big.so" {
		t.Fatalf("hard link = %+v", e)
	}
	if e := byPath["usr/lib/old.so"]; e.Type != TypeWhiteout {
		t.Fatalf("whiteout = %+v", e)
	}
	if !byPath["etc/app"].Opaque {
		t.Fatal("opaque flag lost")
	}

	wantObjects := []manifest.FileObject{
		{SHA256: block1, Size: 4 << 20}, {SHA256: block2, Size: 4 << 20}, {SHA256: block3, Size: 1 << 20},
		{SHA256: digestA, Size: 3},
	}
	if objects := Objects(got); !reflect.DeepEqual(objects, wantObjects) {
		t.Fatalf("objects = %+v, want %+v", objects, wantObjects)
	}
	if got := Size(got); got != 9<<20+3 {
		t.Fatalf("size = %d", got)
	}
}

func TestRejectsInvalidManifests(t *testing.T) {
	whiteout := Entry{Entry: manifest.Entry{Path: "gone", Type: TypeWhiteout}}
	opaque := dir("o")
	opaque.Opaque = true
	link := func(target string) Entry {
		return Entry{Entry: manifest.Entry{Path: "l", Type: TypeHardlink, Target: target}}
	}
	exec := file("x", 1, digestA)
	exec.Executable = true
	huge := file("h", 9<<20, digestA)
	badMode := file("m", 1, digestA)
	badMode.Mode = 0o10000
	badXattr := file("xa", 1, digestA)
	badXattr.Xattrs = map[string][]byte{"": nil}
	cases := map[string]Manifest{
		"unknown kind":          {Kind: "full", Entries: []Entry{dir("a")}},
		"whiteout in a tree":    {Kind: KindTree, Entries: []Entry{whiteout}},
		"opaque in a tree":      {Kind: KindTree, Entries: []Entry{opaque}},
		"opaque file":           {Kind: KindDiff, Entries: []Entry{func() Entry { e := file("f", 1, digestA); e.Opaque = true; return e }()}},
		"dangling hard link":    {Kind: KindTree, Entries: []Entry{link("nowhere")}},
		"hard link to a dir":    {Kind: KindTree, Entries: []Entry{dir("d"), link("d")}},
		"executable flag":       {Kind: KindTree, Entries: []Entry{exec}},
		"large file w/o blocks": {Kind: KindTree, Entries: []Entry{huge}},
		"mode beyond 07777":     {Kind: KindTree, Entries: []Entry{badMode}},
		"empty xattr name":      {Kind: KindTree, Entries: []Entry{badXattr}},
		"missing parent":        {Kind: KindDiff, Entries: []Entry{file("a/b", 1, digestA)}},
		"duplicate path":        {Kind: KindTree, Entries: []Entry{dir("a"), dir("a")}},
		"whiteout with content": {Kind: KindDiff, Entries: []Entry{func() Entry { e := whiteout; e.SHA256 = digestB; return e }()}},
	}
	for name, m := range cases {
		if _, err := Encode(m); !errors.Is(err, manifest.ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
}
