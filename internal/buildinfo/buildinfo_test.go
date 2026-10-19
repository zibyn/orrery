package buildinfo

import "testing"

func TestVersionPrefersTheStampedValue(t *testing.T) {
	old := version
	t.Cleanup(func() { version = old })
	version = "v1.2.3"
	if got := Version(); got != "v1.2.3" {
		t.Fatalf("Version() = %q, want the stamped v1.2.3", got)
	}
}

func TestVersionNeverEmpty(t *testing.T) {
	old := version
	t.Cleanup(func() { version = old })
	version = ""
	if got := Version(); got == "" || got == "(devel)" {
		t.Fatalf("Version() = %q, want a real value", got)
	}
}
