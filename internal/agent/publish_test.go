package agent

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/zibyn/stars-sandbox/internal/manifest"
)

func publishTestRoot(t *testing.T) (*os.Root, string) {
	t.Helper()
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(ws, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(ws, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "bin", "run.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a.txt", filepath.Join(ws, "link")); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(ws)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	return root, ws
}

func TestWalkWorkspaceManifestRecordsTheTree(t *testing.T) {
	root, _ := publishTestRoot(t)
	m, files, err := walkWorkspaceManifest(root, ".")
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]manifest.Entry{}
	for _, entry := range m.Entries {
		byPath[entry.Path] = entry
	}
	if len(m.Entries) != 5 {
		t.Fatalf("entries = %+v", m.Entries)
	}
	if entry, ok := byPath["bin"]; !ok || entry.Type != manifest.TypeDir {
		t.Fatalf("bin entry = %+v", byPath["bin"])
	}
	if entry, ok := byPath["empty"]; !ok || entry.Type != manifest.TypeDir {
		t.Fatalf("empty dir entry = %+v", byPath["empty"])
	}
	run, ok := byPath["bin/run.sh"]
	if !ok || run.Type != manifest.TypeFile || !run.Executable || run.Size == nil || *run.Size != 10 {
		t.Fatalf("run.sh entry = %+v", run)
	}
	link, ok := byPath["link"]
	if !ok || link.Type != manifest.TypeSymlink || link.Target != "a.txt" {
		t.Fatalf("link entry = %+v", link)
	}
	if _, ok := files[run.SHA256]; !ok {
		t.Fatalf("files map = %v has no run.sh digest %s", files, run.SHA256)
	}
}

func TestWalkWorkspaceManifestRootsAtThePublishedPath(t *testing.T) {
	root, _ := publishTestRoot(t)
	m, _, err := walkWorkspaceManifest(root, "bin")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Entries) != 1 || m.Entries[0].Path != "run.sh" {
		t.Fatalf("publishing bin/ entries = %+v", m.Entries)
	}
	m, _, err = walkWorkspaceManifest(root, "bin/run.sh")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Entries) != 1 || m.Entries[0].Path != "run.sh" || !m.Entries[0].Executable {
		t.Fatalf("publishing a single file entries = %+v", m.Entries)
	}
}

func TestWalkWorkspaceManifestRejectsUnsafePaths(t *testing.T) {
	root, ws := publishTestRoot(t)
	if _, _, err := walkWorkspaceManifest(root, "../outside"); err == nil {
		t.Fatal("traversal path was accepted")
	}
	if _, _, err := walkWorkspaceManifest(root, "missing"); err == nil {
		t.Fatal("missing path was accepted")
	}
	if err := syscall.Mkfifo(filepath.Join(ws, "pipe"), 0o644); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	if _, _, err := walkWorkspaceManifest(root, "pipe"); err == nil {
		t.Fatal("FIFO was accepted")
	}
}
