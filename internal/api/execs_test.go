package api

import "testing"

// api.md §4: cwd is relative to /workspace. Absolute paths and traversal are
// rejected; a path that cleans back inside is accepted.
func TestValidExecCwd(t *testing.T) {
	for _, cwd := range []string{"", ".", "sub", "sub/dir", "sub/../dir"} {
		if !validExecCwd(cwd) {
			t.Errorf("validExecCwd(%q) = false, want true", cwd)
		}
	}
	for _, cwd := range []string{"/etc", "/workspace/../etc", "..", "../etc", "sub/../../etc"} {
		if validExecCwd(cwd) {
			t.Errorf("validExecCwd(%q) = true, want false", cwd)
		}
	}
}
