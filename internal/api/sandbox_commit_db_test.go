package api

import (
	"slices"
	"testing"

	nodev1 "github.com/zibyn/orrery/gen/node/v1"
)

func (f *generationFixture) observe(phase, cause string, retrying bool) sandboxDTO {
	f.t.Helper()
	n := &nodeService{s: f.s}
	n.recordSandboxObserved(f.ctx, f.node.ID, &nodev1.SandboxObserved{
		SandboxId: f.sandbox.String(), Generation: 1, Phase: phase, Cause: cause, CommitRetrying: retrying,
	})
	sb, err := f.s.q.GetSandboxIncludingDeleted(f.ctx, f.sandbox)
	if err != nil {
		f.t.Fatal(err)
	}
	return f.s.sandboxToDTO(f.ctx, sb)
}

// A stop whose commit keeps failing stays stopping with commit_stalled, and
// ends stopped without it once the commit lands (api.md §3, runtime.md §16).
func TestCommitStalledShowsUntilGone(t *testing.T) {
	f := newGenerationFixture(t)
	f.s.reconcile = &reconciler{wake: make(chan struct{}, 1)}

	if got := f.observe("committing", "", false); got.State != "stopping" || len(got.Conditions) != 0 {
		t.Fatalf("committing = %s %v", got.State, got.Conditions)
	}
	if got := f.observe("committing", "", true); got.State != "stopping" || !slices.Equal(got.Conditions, []string{"commit_stalled"}) {
		t.Fatalf("retrying = %s %v", got.State, got.Conditions)
	}
	if got := f.observe("gone", "", false); got.State != "stopped" || len(got.Conditions) != 0 {
		t.Fatalf("gone = %s %v", got.State, got.Conditions)
	}
}

// An unreadable upperdir fails the Sandbox commit_failed.
func TestCommitFailedFailsTheSandbox(t *testing.T) {
	f := newGenerationFixture(t)
	f.s.reconcile = &reconciler{wake: make(chan struct{}, 1)}
	f.observe("committing", "", true)
	got := f.observe("commit_failed", "commit_failed", false)
	if got.State != "failed" || got.Cause == nil || *got.Cause != "commit_failed" || len(got.Conditions) != 0 {
		t.Fatalf("commit_failed = %s %v %v", got.State, got.Cause, got.Conditions)
	}
}
