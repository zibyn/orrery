package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
)

// DropSandboxDisk follows the Sandbox's deletion, so nothing of it stays
// under the data root: no stat tables, records or network state.
func TestDropSandboxDiskRemovesTheSandboxDirectory(t *testing.T) {
	rt := newRuntime(ProviderContainerd, t.TempDir(), "a")
	driver := &stopRecorder{dropped: make(chan struct{})}
	rt.driver = driver
	id := uuid.New()
	if err := writeStatTable(statTablePath(rt.dataRoot, id), statTable{Generation: "g1"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"network.json", "volumes.json", "run/result"} {
		path := filepath.Join(sandboxPath(rt.dataRoot, id), name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := rt.dropSandboxDisk(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	<-driver.dropped
	if _, err := os.Stat(sandboxPath(rt.dataRoot, id)); !os.IsNotExist(err) {
		t.Fatalf("Sandbox directory after DropSandboxDisk: %v, want it gone", err)
	}
	// Repeatable: a redelivered command finds nothing to remove.
	driver.dropped = make(chan struct{})
	if err := rt.dropSandboxDisk(context.Background(), id); err != nil {
		t.Fatal(err)
	}
}
