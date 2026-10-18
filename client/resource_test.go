package client

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/zibyn/stars-sandbox/internal/manifest"
)

func TestBuildManifestRecordsTheTree(t *testing.T) {
	root := t.TempDir()
	content := []byte("hello stars\n")
	if err := os.WriteFile(filepath.Join(root, "hello.txt"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "run.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("hello.txt", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}

	m, files, err := buildManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"bin", "bin/run.sh", "empty", "hello.txt", "link"}
	if len(m.Entries) != len(want) {
		t.Fatalf("entries = %+v, want %d", m.Entries, len(want))
	}
	for i, entry := range m.Entries {
		if entry.Path != want[i] {
			t.Fatalf("entry %d = %q, want %q", i, entry.Path, want[i])
		}
	}
	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])
	byPath := map[string]manifest.Entry{}
	for _, entry := range m.Entries {
		byPath[entry.Path] = entry
	}
	if got := byPath["hello.txt"]; got.Type != manifest.TypeFile || got.Size == nil || *got.Size != int64(len(content)) || got.SHA256 != digest || got.Executable {
		t.Fatalf("hello.txt = %+v", got)
	}
	if got := byPath["bin/run.sh"]; !got.Executable {
		t.Fatalf("bin/run.sh is not executable: %+v", got)
	}
	if got := byPath["empty"]; got.Type != manifest.TypeDir {
		t.Fatalf("empty = %+v, want a dir entry", got)
	}
	if got := byPath["link"]; got.Type != manifest.TypeSymlink || got.Target != "hello.txt" {
		t.Fatalf("link = %+v", got)
	}
	if files[digest] != filepath.Join(root, "hello.txt") {
		t.Fatalf("files[%s] = %q", digest, files[digest])
	}

	// Re-walking an unchanged tree produces the same address.
	again, _, err := buildManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := manifest.Address(m)
	second, _ := manifest.Address(again)
	if first != second {
		t.Fatalf("address changed between walks: %s vs %s", first, second)
	}
}

func TestBuildManifestRejectsSpecialFiles(t *testing.T) {
	root := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(root, "pipe"), 0o644); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	if _, _, err := buildManifest(root); err == nil {
		t.Fatal("buildManifest accepted a FIFO")
	}
}

func TestSplitResourceVersion(t *testing.T) {
	version := "0192f3a4-0000-7000-8000-000000000001"
	cases := []struct {
		arg, name, version string
	}{
		{"data", "data", ""},
		{"data@" + version, "data", version},
		{"data@not-a-version", "data@not-a-version", ""},
		{"@" + version, "@" + version, ""},
	}
	for _, tc := range cases {
		name, got := splitResourceVersion(tc.arg)
		if name != tc.name || got != tc.version {
			t.Fatalf("splitResourceVersion(%q) = (%q, %q), want (%q, %q)", tc.arg, name, got, tc.name, tc.version)
		}
	}
}
