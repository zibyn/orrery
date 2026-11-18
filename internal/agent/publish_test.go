package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/zibyn/orrery/internal/castransfer"
	"github.com/zibyn/orrery/internal/manifest"
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

func TestWalkManifestRecordsTheTree(t *testing.T) {
	root, _ := publishTestRoot(t)
	m, files, err := walkManifest(root, ".")
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

func TestWalkManifestRootsAtThePublishedPath(t *testing.T) {
	root, _ := publishTestRoot(t)
	m, _, err := walkManifest(root, "bin")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Entries) != 1 || m.Entries[0].Path != "run.sh" {
		t.Fatalf("publishing bin/ entries = %+v", m.Entries)
	}
	m, _, err = walkManifest(root, "bin/run.sh")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Entries) != 1 || m.Entries[0].Path != "run.sh" || !m.Entries[0].Executable {
		t.Fatalf("publishing a single file entries = %+v", m.Entries)
	}
}

func TestWalkManifestRejectsUnsafePaths(t *testing.T) {
	root, ws := publishTestRoot(t)
	if _, _, err := walkManifest(root, "../outside"); err == nil {
		t.Fatal("traversal path was accepted")
	}
	if _, _, err := walkManifest(root, "missing"); err == nil {
		t.Fatal("missing path was accepted")
	}
	if err := syscall.Mkfifo(filepath.Join(ws, "pipe"), 0o644); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	if _, _, err := walkManifest(root, "pipe"); err == nil {
		t.Fatal("FIFO was accepted")
	}
}

// A file over 4 MiB is published as blocks: the entry carries them,
// each block uploads from its byte range, and the Control Plane computes the
// file's sha256.
func TestWalkManifestSplitsLargeFiles(t *testing.T) {
	root, ws := publishTestRoot(t)
	big := make([]byte, 5<<20)
	for i := range big {
		big[i] = byte(i >> 20)
	}
	if err := os.WriteFile(filepath.Join(ws, "big.bin"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	m, files, err := walkManifest(root, ".")
	if err != nil {
		t.Fatal(err)
	}
	var entry manifest.Entry
	for _, e := range m.Entries {
		if e.Path == "big.bin" {
			entry = e
		}
	}
	if len(entry.Blocks) != 2 || entry.SHA256 != manifest.FileAddress(entry.Blocks) {
		t.Fatalf("big.bin entry = %+v, want two blocks", entry)
	}
	if last := files[entry.Blocks[1]]; last.Offset != 4<<20 || last.Size != 1<<20 {
		t.Fatalf("last block source = %+v", last)
	}
}

// uploadTestObjects is n small files under a root, as the objects a
// publish or commit uploads.
func uploadTestObjects(t *testing.T, n int) (*os.Root, []string, map[string]manifest.Source) {
	t.Helper()
	dir := t.TempDir()
	var digests []string
	files := map[string]manifest.Source{}
	for i := range n {
		content := fmt.Sprintf("object %d", i)
		writeTestFile(t, filepath.Join(dir, fmt.Sprint(i)), content, 0o644)
		digests = append(digests, digestOf(content))
		files[digestOf(content)] = manifest.Source{Path: fmt.Sprint(i), Size: int64(len(content))}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	return root, digests, files
}

// An upload whose context ends before its PUTs start fails, as nothing
// reached the store.
func TestUploadObjectsFailsWhenCanceled(t *testing.T) {
	node := newFakeGenerationNode(t)
	rt := newRuntime(ProviderContainerd, t.TempDir(), "node")
	rt.client = node
	root, missing, files := uploadTestObjects(t, 3)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := rt.uploadPublishObjects(ctx, root, missing, files); err == nil {
		t.Fatal("a canceled upload succeeded")
	}
}

// The Control Plane asking for an object the tree lacks fails the upload
// before any PUT starts, though it comes after more than fit in flight.
func TestUploadObjectsRefusesAnUnknownObject(t *testing.T) {
	node := newFakeGenerationNode(t)
	rt := newRuntime(ProviderContainerd, t.TempDir(), "node")
	rt.client = node
	root, missing, files := uploadTestObjects(t, castransfer.MaxUploads+5)
	err := rt.uploadPublishObjects(context.Background(), root, append(missing, digestOf("elsewhere")), files)
	if err == nil || !strings.Contains(err.Error(), "does not contain") {
		t.Fatalf("upload = %v, want the unknown object refused", err)
	}
	node.mu.Lock()
	defer node.mu.Unlock()
	if len(node.objects) != 0 {
		t.Fatalf("%d objects uploaded before the refusal", len(node.objects))
	}
}
