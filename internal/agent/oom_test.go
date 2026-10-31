package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/zibyn/orrery/internal/supervisor"
)

// hostOOMCause is runtime.md §9's runsc rule: only a plain SIGKILL or an
// Agent-written runtime_lost becomes oom_killed, and only if the host counter
// rose.
func TestHostOOMCause(t *testing.T) {
	sigkill, sigterm := int32(9), int32(15)
	code := int32(137)
	cases := []struct {
		name string
		exit supervisor.Exit
		rose bool
		want string
	}{
		{"stub killed", supervisor.Exit{Cause: "exited", Signal: &sigkill}, true, "oom_killed"},
		{"Sentry killed", supervisor.Exit{Cause: "runtime_lost"}, true, "oom_killed"},
		{"kill -9 without OOM", supervisor.Exit{Cause: "exited", Signal: &sigkill}, false, "exited"},
		{"Runtime lost without OOM", supervisor.Exit{Cause: "runtime_lost"}, false, "runtime_lost"},
		{"canceled", supervisor.Exit{Cause: "canceled", Signal: &sigkill}, true, "canceled"},
		{"timed out", supervisor.Exit{Cause: "timed_out", Signal: &sigkill}, true, "timed_out"},
		{"other signal", supervisor.Exit{Cause: "exited", Signal: &sigterm}, true, "exited"},
		{"exit code 137", supervisor.Exit{Cause: "exited", ExitCode: &code}, true, "exited"},
		{"already oom_killed", supervisor.Exit{Cause: "oom_killed", Signal: &sigkill}, true, "oom_killed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hostOOMCause(tc.exit, tc.rose); got != tc.want {
				t.Fatalf("hostOOMCause = %q, want %q", got, tc.want)
			}
		})
	}
}

// The baseline lives in the record, so an Agent restarted after the Exec
// started still attributes its kill (runtime.md §9).
func TestHostOOMBaselineSurvivesRestart(t *testing.T) {
	root, cgroups := t.TempDir(), t.TempDir()
	sandboxID, execID := uuid.New(), uuid.New()
	events := filepath.Join(cgroups, containerdNamespace, containerName(sandboxID))
	if err := os.MkdirAll(events, 0o755); err != nil {
		t.Fatal(err)
	}
	setCount := func(n string) {
		if err := os.WriteFile(filepath.Join(events, "memory.events"), []byte("low 0\noom 0\noom_kill "+n+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dir := filepath.Join(sandboxRunPath(root, sandboxID), execID.String())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	setCount("2")
	before := newTestRuntime(root, nil)
	before.hostCgroup = cgroups
	before.writeOOMBaseline(sandboxID, dir)

	sigkill := int32(9)
	exit := supervisor.Exit{Cause: "exited", Signal: &sigkill}
	after := newTestRuntime(root, nil) // the restarted Agent
	after.hostCgroup = cgroups
	if got := after.attributeHostOOM(sandboxID, dir, exit); got.Cause != "exited" {
		t.Fatalf("no OOM since the baseline: cause = %q", got.Cause)
	}
	setCount("3")
	if got := after.attributeHostOOM(sandboxID, dir, exit); got.Cause != "oom_killed" {
		t.Fatalf("counter rose: cause = %q, want oom_killed", got.Cause)
	}
	// The rewrite is in the record, so a redelivery reports the same cause.
	if got, _ := readExit(dir); got.Cause != "oom_killed" {
		t.Fatalf("record cause = %q, want oom_killed", got.Cause)
	}

	// finalizeLost after a Sentry kill ends unfinished Execs oom_killed.
	if err := os.Remove(filepath.Join(dir, "exit")); err != nil {
		t.Fatal(err)
	}
	after.finalizeLost(sandboxID.String())
	if got, _ := readExit(dir); got.Cause != "oom_killed" {
		t.Fatalf("lost Sandbox cause = %q, want oom_killed", got.Cause)
	}
}
