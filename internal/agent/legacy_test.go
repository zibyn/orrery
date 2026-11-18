package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
)

// writeLegacy lays out what an Agent before ADR-0031 left: the directory and
// the Sandbox's record of it.
func writeLegacy(t *testing.T, dataRoot string, sandbox uuid.UUID, mode string) string {
	t.Helper()
	dirID := uuid.NewString()
	dir := legacyDirPath(dataRoot, dirID)
	if err := os.MkdirAll(filepath.Join(dir, "repo"), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "repo", "f"), []byte("data"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("f", filepath.Join(dir, "repo", "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sandboxPath(dataRoot, sandbox), 0o755); err != nil {
		t.Fatal(err)
	}
	rec := `{"id":"` + dirID + `","mode":"` + mode + `"}`
	if err := os.WriteFile(legacyRecordPath(dataRoot, sandbox), []byte(rec), 0o644); err != nil {
		t.Fatal(err)
	}
	return dirID
}

// ADR-0031 "Migration": a Sandbox's own directory is merged into its
// filesystem at its next Create; a read-only mount and a directory that is
// now one of its Volumes are not.
func TestTakeLegacySandboxDir(t *testing.T) {
	dataRoot := t.TempDir()

	own := uuid.New()
	dirID := writeLegacy(t, dataRoot, own, "rw")
	if dir := legacyRunningDir(dataRoot, own); dir != legacyDirPath(dataRoot, dirID) {
		t.Fatalf("running dir = %q, want the old directory", dir)
	}
	dir, err := takeLegacySandboxDir(dataRoot, own, nil)
	if err != nil || dir != legacyDirPath(dataRoot, dirID) {
		t.Fatalf("own directory = %q, %v", dir, err)
	}
	if _, err := os.Lstat(legacyRecordPath(dataRoot, own)); !os.IsNotExist(err) {
		t.Fatalf("the record stays: %v", err)
	}
	workdir := t.TempDir()
	if err := mergeLegacyDir(context.Background(), dir, workdir); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(workdir, "repo", "f")); err != nil || string(got) != "data" {
		t.Fatalf("merged file = %q, %v", got, err)
	}
	if info, err := os.Stat(filepath.Join(workdir, "repo", "f")); err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("merged mode = %v, %v", info, err)
	}
	if target, err := os.Readlink(filepath.Join(workdir, "repo", "link")); err != nil || target != "f" {
		t.Fatalf("merged link = %q, %v", target, err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("the old directory stays: %v", err)
	}

	reader := uuid.New()
	writeLegacy(t, dataRoot, reader, "ro")
	if dir, err := takeLegacySandboxDir(dataRoot, reader, nil); err != nil || dir != "" {
		t.Fatalf("read-only mount = %q, %v, want dropped", dir, err)
	}

	writer := uuid.New()
	volumeID := writeLegacy(t, dataRoot, writer, "rw")
	if dir, err := takeLegacySandboxDir(dataRoot, writer, []volumeMount{{ID: volumeID, Path: "/workspace"}}); err != nil || dir != "" {
		t.Fatalf("persistent directory = %q, %v, want left to its Volume", dir, err)
	}
	if _, err := os.Stat(legacyDirPath(dataRoot, volumeID)); err != nil {
		t.Fatalf("the Volume's directory is gone: %v", err)
	}
}
