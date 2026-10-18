package supervisor

import (
	"path/filepath"
	"testing"
)

// Local Runtime names an abstract socket because the host record path may
// exceed sun_path; a container keeps the filesystem ctl.sock (runtime.md §10).
func TestCtlSocketAddress(t *testing.T) {
	dir := "/data/sandboxes/sandbox/run/exec"
	if got, abstract := ctlSocketAddress(dir); abstract || got != filepath.Join(dir, "ctl.sock") {
		t.Fatalf("default = %q, abstract %v", got, abstract)
	}
	t.Setenv("STARS_CTL_SOCKET", "@stars-exec-exec")
	if got, abstract := ctlSocketAddress(dir); !abstract || got != "@stars-exec-exec" {
		t.Fatalf("env = %q, abstract %v", got, abstract)
	}
}
