package resourcefs

import (
	"path/filepath"
	"strings"
	"testing"
)

// A socket path that fits sun_path binds even though the temporary name it
// binds first lives in the same directory.
func TestListenUnixBindsAPathNearTheLimit(t *testing.T) {
	dir := t.TempDir()
	base := "rfs.sock"
	// 107 bytes: the most sun_path holds with its terminating NUL.
	pad := 107 - len(dir) - len("//") - len(base)
	if pad < 1 {
		t.Skipf("temp dir %s is too long", dir)
	}
	path := filepath.Join(dir, strings.Repeat("x", pad), base)
	l, err := listenUnix(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if len(path) != 107 {
		t.Fatalf("path is %d bytes", len(path))
	}
}
