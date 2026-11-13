package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// fakeMounter records what the Agent asks orrery-resourcefs to do.
type fakeMounter struct {
	mu      sync.Mutex
	mounted map[string]bool
	mounts  int
	fail    error
}

func (f *fakeMounter) Mount(_ context.Context, address string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return f.fail
	}
	if f.mounted == nil {
		f.mounted = map[string]bool{}
	}
	if !f.mounted[address] {
		f.mounts++
	}
	f.mounted[address] = true
	return nil
}

func (f *fakeMounter) Unmount(_ context.Context, address string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.mounted, address)
	return nil
}

func (f *fakeMounter) Status(context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for address := range f.mounted {
		out = append(out, address)
	}
	return out, nil
}

func (f *fakeMounter) SelfTest(context.Context) error { return nil }

func (f *fakeMounter) isMounted(address string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mounted[address]
}

var (
	versionA = strings.Repeat("a", 64)
	versionB = strings.Repeat("b", 64)
)

func newResourceRuntime(t *testing.T, root string, mounter *fakeMounter) *Runtime {
	t.Helper()
	r := newRuntime(ProviderLocal, root, "node")
	r.resourcefs = mounter
	return r
}

// Two Sandboxes share a version: stopping one keeps it mounted, stopping the
// last unmounts it (runtime.md §11).
func TestResourceMountsAreCounted(t *testing.T) {
	root := t.TempDir()
	mounter := &fakeMounter{}
	r := newResourceRuntime(t, root, mounter)
	ctx := context.Background()
	first, second := uuid.New(), uuid.New()
	if _, err := r.mountResources(ctx, first, []resourceMount{{address: versionA, mount: "data"}, {address: versionB, mount: "other"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.mountResources(ctx, second, []resourceMount{{address: versionA, mount: "data"}}); err != nil {
		t.Fatal(err)
	}
	if mounter.mounts != 2 {
		t.Fatalf("mounts = %d, want one per version", mounter.mounts)
	}
	link, err := os.Readlink(filepath.Join(sandboxResourcesPath(root, first), "data"))
	if err != nil || link != resourceVersionPath(root, versionA) {
		t.Fatalf("data link = %q, %v", link, err)
	}

	r.releaseResources(ctx, first)
	if !mounter.isMounted(versionA) || mounter.isMounted(versionB) {
		t.Fatalf("after the first stop: mounted = %v, want only A", mounter.mounted)
	}
	if _, err := os.Stat(sandboxResourcesPath(root, first)); !os.IsNotExist(err) {
		t.Fatalf("the stopped Sandbox's links survive: %v", err)
	}
	r.releaseResources(ctx, second)
	if mounter.isMounted(versionA) {
		t.Fatal("the last stop left the version mounted")
	}
}

// A restarted Agent rebuilds the holds of the Sandboxes it adopts from their
// link records and unmounts versions nothing holds, without mounting again.
func TestResourceRefsRebuildAfterRestart(t *testing.T) {
	root := t.TempDir()
	mounter := &fakeMounter{}
	before := newResourceRuntime(t, root, mounter)
	ctx := context.Background()
	running, stopped := uuid.New(), uuid.New()
	before.mountResources(ctx, running, []resourceMount{{address: versionA, mount: "data"}})
	before.mountResources(ctx, stopped, []resourceMount{{address: versionB, mount: "data"}})
	mounts := mounter.mounts

	after := newResourceRuntime(t, root, mounter)
	after.track(running, 1) // adopted; the other Runtime is gone
	after.rebuildResourceRefs(ctx)
	if mounter.mounts != mounts {
		t.Fatalf("rebuild mounted again: %d mounts, want %d", mounter.mounts, mounts)
	}
	if !mounter.isMounted(versionA) || mounter.isMounted(versionB) {
		t.Fatalf("after rebuild: mounted = %v, want only the adopted Sandbox's", mounter.mounted)
	}
	after.releaseResources(ctx, running)
	if mounter.isMounted(versionA) {
		t.Fatal("the rebuilt hold did not unmount on stop")
	}
}

func TestResourceMountFailureIsResourceFetchFailed(t *testing.T) {
	resourceFetchBackoff = []time.Duration{time.Millisecond}
	t.Cleanup(func() {
		resourceFetchBackoff = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second}
	})
	r := newResourceRuntime(t, t.TempDir(), &fakeMounter{fail: errors.New("manifest unreadable")})
	cause, err := r.mountResources(context.Background(), uuid.New(), []resourceMount{{address: versionA, mount: "data"}})
	if err == nil || cause != "resource_fetch_failed" {
		t.Fatalf("cause = %q, err = %v", cause, err)
	}
	if r.resourceRefs.used(versionA) {
		t.Fatal("a failed mount left its hold")
	}
}

func TestRetryResourceFetch(t *testing.T) {
	backoff := []time.Duration{time.Millisecond, time.Millisecond}
	attempts := 0
	if err := retryResourceFetch(context.Background(), backoff, func() error {
		attempts++
		if attempts < 3 {
			return errors.New("not yet")
		}
		return nil
	}); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}

	attempts = 0
	err := retryResourceFetch(context.Background(), backoff, func() error {
		attempts++
		return errors.New("down")
	})
	if err == nil {
		t.Fatal("retry succeeded against a permanent failure")
	}
	if want := len(backoff) + 1; attempts != want {
		t.Fatalf("attempts = %d, want %d", attempts, want)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	attempts = 0
	if err := retryResourceFetch(ctx, []time.Duration{time.Hour}, func() error {
		attempts++
		return errors.New("down")
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled retry = %v, want context.Canceled", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts after cancel = %d, want 1", attempts)
	}
}

func TestValidResourceMount(t *testing.T) {
	for _, mount := range []string{"data", "a.b", "UPPER"} {
		if !validResourceMount(mount) {
			t.Fatalf("%q rejected", mount)
		}
	}
	for _, mount := range []string{"", ".", "..", "a/b", "/etc", "a\x00b"} {
		if validResourceMount(mount) {
			t.Fatalf("%q accepted", mount)
		}
	}
}
