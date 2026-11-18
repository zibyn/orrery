package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/zibyn/orrery/internal/generation"
	"github.com/zibyn/orrery/internal/manifest"
)

func writeTestFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func digestOf(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func entriesByPath(m generation.Manifest) map[string]generation.Entry {
	out := map[string]generation.Entry{}
	for _, e := range m.Entries {
		out[e.Path] = e
	}
	return out
}

// An upperdir as overlayfs leaves it after a Sandbox installed a package,
// edited a base file, deleted one, replaced a directory and worked in a Git
// repository (runtime.md §16).
func TestScanFilesystemRecordsAnUpperdir(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "usr/bin/tool"), "#!/bin/sh\n", os.ModeSetuid|0o755)
	writeTestFile(t, filepath.Join(root, "etc/motd"), "hello\n", 0o644)
	if err := unix.Mknod(filepath.Join(root, "etc/issue"), unix.S_IFCHR, 0); err != nil {
		t.Fatalf("whiteout: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "opt/app"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := unix.Setxattr(filepath.Join(root, "opt/app"), "user.overlay.opaque", []byte("y"), 0); err != nil {
		t.Skipf("no user xattrs here: %v", err)
	}
	writeTestFile(t, filepath.Join(root, "opt/app/new"), "new", 0o600)
	if err := unix.Setxattr(filepath.Join(root, "opt/app/new"), "user.mime", []byte("text/plain"), 0); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(root, "opt/app/new"), filepath.Join(root, "opt/app/same")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../usr/bin/tool", filepath.Join(root, "opt/link")); err != nil {
		t.Fatal(err)
	}
	git := filepath.Join(root, "workspace/repo/.git")
	writeTestFile(t, filepath.Join(git, "HEAD"), "ref: refs/heads/main\n", 0o644)
	writeTestFile(t, filepath.Join(git, "index.lock"), "x", 0o644)
	writeTestFile(t, filepath.Join(git, "refs/heads/main.lock"), "x", 0o644)
	if err := os.MkdirAll(filepath.Join(git, "objects"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(root, "workspace/notes.lock"), "kept outside Git", 0o644)
	// A runtime bind mount's stub and contents are not the Sandbox's.
	writeTestFile(t, filepath.Join(root, ".orrery/run/x"), "record", 0o644)

	scan, err := scanFilesystem(root, generation.KindDiff, []string{".orrery"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := entriesByPath(scan.manifest)

	tool := got["usr/bin/tool"]
	if tool.Type != manifest.TypeFile || tool.SHA256 != digestOf("#!/bin/sh\n") || tool.Mode != 0o4755 {
		t.Fatalf("tool = %+v", tool)
	}
	if tool.UID != uint32(os.Getuid()) || tool.Mtime == 0 {
		t.Fatalf("tool owner/mtime = %d/%d", tool.UID, tool.Mtime)
	}
	if got["usr"].Type != manifest.TypeDir || got["usr/bin"].Type != manifest.TypeDir {
		t.Fatalf("parents not recorded: %+v %+v", got["usr"], got["usr/bin"])
	}
	if e := got["etc/issue"]; e.Type != generation.TypeWhiteout {
		t.Fatalf("whiteout = %+v", e)
	}
	if e := got["opt/app"]; !e.Opaque || e.Xattrs != nil {
		t.Fatalf("opaque dir = %+v", e)
	}
	if e := got["opt/app/new"]; string(e.Xattrs["user.mime"]) != "text/plain" || e.Mode != 0o600 {
		t.Fatalf("xattr file = %+v", e)
	}
	if e := got["opt/app/same"]; e.Type != generation.TypeHardlink || e.Target != "opt/app/new" {
		t.Fatalf("hard link = %+v", e)
	}
	if e := got["opt/link"]; e.Type != manifest.TypeSymlink || e.Target != "../usr/bin/tool" {
		t.Fatalf("symlink = %+v", e)
	}
	for _, skipped := range []string{"workspace/repo/.git/index.lock", "workspace/repo/.git/refs/heads/main.lock", ".orrery", ".orrery/run/x"} {
		if _, ok := got[skipped]; ok {
			t.Fatalf("%s was committed", skipped)
		}
	}
	if _, ok := got["workspace/notes.lock"]; !ok {
		t.Fatal("a lock file outside Git was skipped")
	}
	if _, err := generation.Encode(scan.manifest); err != nil {
		t.Fatalf("the scan does not encode: %v", err)
	}
	if len(scan.changed) != 5 { // tool, motd, new, HEAD, notes.lock
		t.Fatalf("changed = %d, want every file on the first scan", len(scan.changed))
	}
}

// The second scan hashes only what changed since the last commit's table.
func TestScanFilesystemHashesOnlyChangedFiles(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "a"), "one", 0o644)
	writeTestFile(t, filepath.Join(root, "b"), "two", 0o644)
	first, err := scanFilesystem(root, generation.KindTree, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(root, "b"), "changed", 0o644)
	writeTestFile(t, filepath.Join(root, "c"), "three", 0o644)
	second, err := scanFilesystem(root, generation.KindTree, nil, first.files)
	if err != nil {
		t.Fatal(err)
	}
	changed := map[string]string{}
	for _, e := range second.changed {
		changed[e.Path] = e.SHA256
	}
	if len(changed) != 2 || changed["b"] != digestOf("changed") || changed["c"] != digestOf("three") {
		t.Fatalf("changed = %v", changed)
	}
	if got := entriesByPath(second.manifest)["a"]; got.SHA256 != digestOf("one") {
		t.Fatalf("unchanged a lost its digest: %+v", got)
	}
	if _, ok := second.objects[digestOf("one")]; ok {
		t.Fatal("an unchanged file is offered for upload")
	}
}

func TestScanFilesystemSplitsLargeFiles(t *testing.T) {
	root := t.TempDir()
	big := make([]byte, manifest.BlockSize+1)
	if err := os.WriteFile(filepath.Join(root, "big"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	scan, err := scanFilesystem(root, generation.KindTree, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	e := entriesByPath(scan.manifest)["big"]
	if len(e.Blocks) != 2 {
		t.Fatalf("big = %+v", e)
	}
	if src := scan.objects[e.Blocks[1]]; src.Offset != manifest.BlockSize || src.Size != 1 {
		t.Fatalf("second block source = %+v", src)
	}
}
