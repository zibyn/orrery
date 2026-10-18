package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func size(n int64) *int64 { return &n }

const (
	digestA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digestB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	digestC = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

func TestNormalizeSortsAndValidatesParents(t *testing.T) {
	m := Manifest{Entries: []Entry{
		{Path: "b.txt", Type: TypeFile, Size: size(2), SHA256: digestB},
		{Path: "sub", Type: TypeDir},
		{Path: "a.txt", Type: TypeFile, Size: size(1), SHA256: digestA},
		{Path: "sub/c.txt", Type: TypeFile, Size: size(3), SHA256: digestC},
	}}
	got, err := Normalize(m)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a.txt", "b.txt", "sub", "sub/c.txt"}
	for i, entry := range got.Entries {
		if entry.Path != want[i] {
			t.Fatalf("entry %d = %q, want %q", i, entry.Path, want[i])
		}
	}
	if err := Validate(got); err != nil {
		t.Fatalf("normalized manifest is not valid: %v", err)
	}
}

func TestManifestRejectsInvalidPaths(t *testing.T) {
	cases := []string{
		"", "/abs", "a//b", "a/./b", "a/../b", "..", ".", "a/", "nul\x00byte",
	}
	for _, path := range cases {
		m := Manifest{Entries: []Entry{
			{Path: "parent", Type: TypeDir},
			{Path: path, Type: TypeDir},
		}}
		if err := Validate(m); !errors.Is(err, ErrInvalid) {
			t.Fatalf("path %q: err = %v, want ErrInvalid", path, err)
		}
	}
}

func TestManifestRejectsDuplicatePaths(t *testing.T) {
	m := Manifest{Entries: []Entry{
		{Path: "a", Type: TypeDir},
		{Path: "a", Type: TypeDir},
	}}
	if err := Validate(m); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

func TestManifestRequiresExplicitParentDirectories(t *testing.T) {
	m := Manifest{Entries: []Entry{
		{Path: "a/b.txt", Type: TypeFile, Size: size(1), SHA256: digestA},
	}}
	if err := Validate(m); !errors.Is(err, ErrInvalid) {
		t.Fatalf("missing parent: err = %v, want ErrInvalid", err)
	}
	// A file cannot stand in for a parent directory.
	m.Entries = append([]Entry{{Path: "a", Type: TypeFile, Size: size(1), SHA256: digestA}}, m.Entries...)
	if err := Validate(m); !errors.Is(err, ErrInvalid) {
		t.Fatalf("file parent: err = %v, want ErrInvalid", err)
	}
}

func TestFileEntryRules(t *testing.T) {
	base := func() Manifest {
		return Manifest{Entries: []Entry{{Path: "f", Type: TypeFile}}}
	}
	cases := []struct {
		name  string
		entry Entry
		want  error
	}{
		{"missing size", Entry{Path: "f", Type: TypeFile, SHA256: digestA}, ErrInvalid},
		{"missing sha", Entry{Path: "f", Type: TypeFile, Size: size(1)}, ErrInvalid},
		{"short sha", Entry{Path: "f", Type: TypeFile, Size: size(1), SHA256: "abc"}, ErrInvalid},
		{"uppercase sha", Entry{Path: "f", Type: TypeFile, Size: size(1), SHA256: strings.ToUpper(digestA)}, ErrInvalid},
		{"negative size", Entry{Path: "f", Type: TypeFile, Size: size(-1), SHA256: digestA}, ErrInvalid},
		{"over 5 GiB", Entry{Path: "f", Type: TypeFile, Size: size(MaxFileSize + 1), SHA256: digestA}, ErrTooLarge},
		{"target on file", Entry{Path: "f", Type: TypeFile, Size: size(1), SHA256: digestA, Target: "x"}, ErrInvalid},
	}
	for _, tc := range cases {
		m := base()
		m.Entries[0] = tc.entry
		if err := Validate(m); !errors.Is(err, tc.want) {
			t.Fatalf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
	// Exactly 5 GiB is allowed.
	m := Manifest{Entries: []Entry{{Path: "f", Type: TypeFile, Size: size(MaxFileSize), SHA256: digestA}}}
	if err := Validate(m); err != nil {
		t.Fatalf("5 GiB entry: %v", err)
	}
}

func TestSymlinkAndDirRules(t *testing.T) {
	valid := Manifest{Entries: []Entry{
		{Path: "d", Type: TypeDir},
		{Path: "d/link", Type: TypeSymlink, Target: "../target"},
	}}
	if err := Validate(valid); err != nil {
		t.Fatal(err)
	}
	bad := []Entry{
		{Path: "d", Type: TypeSymlink},                               // missing target
		{Path: "d", Type: TypeSymlink, Target: ""},                   // empty target
		{Path: "d", Type: TypeSymlink, Target: "x", SHA256: digestA}, // file fields
		{Path: "d", Type: TypeDir, Executable: true},                 // executable on dir
		{Path: "d", Type: TypeFile, Size: size(1), SHA256: digestA, Target: "x"},
		{Path: "d", Type: "socket"}, // unknown type
	}
	for i, entry := range bad {
		if err := Validate(Manifest{Entries: []Entry{entry}}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("case %d (%+v): err = %v, want ErrInvalid", i, entry, err)
		}
	}
}

func TestEntryLimit(t *testing.T) {
	if MaxEntries != 1_000_000 {
		t.Fatalf("MaxEntries = %d, want 1,000,000 (defaults.md)", MaxEntries)
	}
	m := Manifest{}
	for i := 0; i < 4; i++ {
		m.Entries = append(m.Entries, Entry{Path: fmt.Sprintf("d%d", i), Type: TypeDir})
	}
	if err := validate(m, 4); err != nil {
		t.Fatalf("4 entries with limit 4: %v", err)
	}
	if err := validate(m, 3); !errors.Is(err, ErrTooManyEntries) {
		t.Fatalf("4 entries with limit 3: err = %v, want ErrTooManyEntries", err)
	}
}

func TestAddressIsCanonicalAndStable(t *testing.T) {
	unsorted := Manifest{Entries: []Entry{
		{Path: "sub", Type: TypeDir},
		{Path: "sub/f", Type: TypeFile, Size: size(0), SHA256: digestA, Executable: true},
		{Path: "link", Type: TypeSymlink, Target: "sub/f"},
	}}
	sorted := Manifest{Entries: []Entry{
		{Path: "link", Type: TypeSymlink, Target: "sub/f"},
		{Path: "sub", Type: TypeDir},
		{Path: "sub/f", Type: TypeFile, Size: size(0), SHA256: digestA, Executable: true},
	}}
	address, err := Address(unsorted)
	if err != nil {
		t.Fatal(err)
	}
	again, err := Address(sorted)
	if err != nil {
		t.Fatal(err)
	}
	if address != again {
		t.Fatalf("address depends on entry order: %s vs %s", address, again)
	}
	canonical, err := Canonical(unsorted)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(canonical)
	if address != hex.EncodeToString(sum[:]) {
		t.Fatalf("address = %s, want sha256(canonical) = %s", address, hex.EncodeToString(sum[:]))
	}
	// The canonical form is compact JSON with the file's zero size present.
	if strings.Contains(string(canonical), " ") || !strings.Contains(string(canonical), `"size":0`) {
		t.Fatalf("canonical encoding = %s", canonical)
	}
}

func TestAddressChangesWithContent(t *testing.T) {
	base := Manifest{Entries: []Entry{{Path: "f", Type: TypeFile, Size: size(1), SHA256: digestA}}}
	other := Manifest{Entries: []Entry{{Path: "f", Type: TypeFile, Size: size(1), SHA256: digestB}}}
	a, _ := Address(base)
	b, _ := Address(other)
	if a == b {
		t.Fatalf("address did not change with content: %s", a)
	}
	exec := base
	exec.Entries = append([]Entry(nil), base.Entries...)
	exec.Entries[0].Executable = true
	e, _ := Address(exec)
	if e == a {
		t.Fatalf("address did not change with the executable bit: %s", a)
	}
}

func TestFileObjectsAndTotalSize(t *testing.T) {
	m := Manifest{Entries: []Entry{
		{Path: "a", Type: TypeDir},
		{Path: "a/one", Type: TypeFile, Size: size(10), SHA256: digestB},
		{Path: "a/two", Type: TypeFile, Size: size(20), SHA256: digestA},
		{Path: "a/dup", Type: TypeFile, Size: size(10), SHA256: digestB},
		{Path: "a/link", Type: TypeSymlink, Target: "one"},
	}}
	objects := FileObjects(m)
	if len(objects) != 2 {
		t.Fatalf("FileObjects = %+v, want 2 unique objects", objects)
	}
	if objects[0].SHA256 != digestA || objects[1].SHA256 != digestB {
		t.Fatalf("FileObjects not sorted by digest: %+v", objects)
	}
	if objects[1].Size != 10 {
		t.Fatalf("object size = %d, want 10", objects[1].Size)
	}
	if got, want := TotalSize(m), int64(40); got != want {
		t.Fatalf("TotalSize = %d, want %d", got, want)
	}
}

func TestDecodeRejectsUnknownFields(t *testing.T) {
	good := `{"entries":[{"path":"f","type":"file","size":1,"sha256":"` + digestA + `"}]}`
	var m Manifest
	dec := json.NewDecoder(strings.NewReader(good))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		t.Fatal(err)
	}
	if err := Validate(m); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		`{"entries":[{"path":"f","type":"file","size":1,"sha256":"` + digestA + `","mode":493}]}`,
		`{"entries":[{"path":"f","type":"file","size":1,"sha256":"` + digestA + `"}],"extra":true}`,
	} {
		dec := json.NewDecoder(strings.NewReader(body))
		dec.DisallowUnknownFields()
		var m Manifest
		if err := dec.Decode(&m); err == nil {
			t.Fatalf("decoded a manifest with an unknown field: %s", body)
		}
	}
}
