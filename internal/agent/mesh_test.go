package agent

import (
	"os"
	"path/filepath"
	"testing"
)

// The mesh key is generated once, 0600, survives a restart and is cleared
// with the identity (runtime.md §15).
func TestMeshKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, meshKeyFile)
	first, err := loadMeshKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mesh.key stat = %v, %v; want mode 0600", info, err)
	}
	again, err := loadMeshKey(path)
	if err != nil || again != first {
		t.Fatalf("restart got a different key (%v)", err)
	}
	removeMeshKey(dir)
	fresh, err := loadMeshKey(path)
	if err != nil || fresh == first {
		t.Fatalf("a cleared key was reused (%v)", err)
	}
}
