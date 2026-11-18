package agent

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"

	nodev1 "github.com/zibyn/orrery/gen/node/v1"
	"github.com/zibyn/orrery/internal/generation"
)

// A Generation committed on one Node restores on another into a tree that
// scans back to the same manifest (mode, owner, mtime, xattrs, whiteout,
// opaque, hard and symbolic links), and the restoring Node's next commit is
// based on it without re-hashing what it restored (runtime.md §16).
func TestRestoreRoundTrip(t *testing.T) {
	f := newFakeGenerationNode(t)
	id := uuid.New()
	upper := t.TempDir()
	writeTestFile(t, filepath.Join(upper, "usr/bin/tool"), "#!/bin/sh\n", 0o4755)
	writeTestFile(t, filepath.Join(upper, "workspace/repo/README"), "hello\n", 0o600)
	if err := os.MkdirAll(filepath.Join(upper, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mknod(filepath.Join(upper, "etc/issue"), unix.S_IFCHR, 0); err != nil {
		t.Fatalf("whiteout: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(upper, "opt/app"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := unix.Setxattr(filepath.Join(upper, "opt/app"), "user.overlay.opaque", []byte("y"), 0); err != nil {
		t.Skipf("no user xattrs here: %v", err)
	}
	writeTestFile(t, filepath.Join(upper, "opt/app/new"), "new", 0o640)
	if err := unix.Setxattr(filepath.Join(upper, "opt/app/new"), "user.mime", []byte("text/plain"), 0); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(upper, "opt/app/new"), filepath.Join(upper, "opt/app-same")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../usr/bin/tool", filepath.Join(upper, "opt/link")); err != nil {
		t.Fatal(err)
	}
	large := bytes.Repeat([]byte("0123456789abcdef"), 600_000) // two blocks
	writeTestFile(t, filepath.Join(upper, "workspace/big.bin"), string(large), 0o644)

	nodeA := newRuntime(ProviderContainerd, t.TempDir(), "a")
	nodeA.client = f
	if err := nodeA.commitWithRetry(context.Background(), id, 1, commitTarget{root: upper, kind: generation.KindDiff}, func() {}); err != nil {
		t.Fatal(err)
	}
	head := f.head
	committed, err := generation.Decode(f.manifests[head])
	if err != nil {
		t.Fatal(err)
	}

	nodeB := newRuntime(ProviderContainerd, t.TempDir(), "b")
	nodeB.client = f
	restored := filepath.Join(t.TempDir(), "fs")
	if err := nodeB.restoreFilesystem(context.Background(), id, head, restored); err != nil {
		t.Fatal(err)
	}
	scan, err := scanFilesystem(restored, generation.KindDiff, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := generation.Encode(committed)
	got, err := generation.Encode(scan.manifest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("restored tree scans to\n%s\nwant\n%s", got, want)
	}
	if !nodeB.cachedDiffMatches(id, head) {
		t.Fatal("the restore did not record its Generation as the cached diff")
	}

	writeTestFile(t, filepath.Join(restored, "workspace/added"), "added", 0o644)
	if err := nodeB.commitWithRetry(context.Background(), id, 2, commitTarget{root: restored, kind: generation.KindDiff}, func() {}); err != nil {
		t.Fatal(err)
	}
	if base := f.bases[len(f.bases)-1]; base != head {
		t.Fatalf("the next commit is based on %q, want the restored %q", base, head)
	}
	if hashed := f.submitted[len(f.submitted)-1]; len(hashed) != 1 || hashed[0] != digestOf("added") {
		t.Fatalf("the next commit hashed %v, want only the added file", hashed)
	}
}

// A cached diff is reused only for the Generation it holds; a Sandbox that
// committed elsewhere since is restored instead.
func TestCachedDiffMatchesOnlyItsGeneration(t *testing.T) {
	rt := newRuntime(ProviderContainerd, t.TempDir(), "a")
	id := uuid.New()
	if rt.cachedDiffMatches(id, "") {
		t.Fatal("a Sandbox without a head reuses a cache")
	}
	if err := writeStatTable(statTablePath(rt.dataRoot, id), statTable{Generation: "g1"}); err != nil {
		t.Fatal(err)
	}
	if !rt.cachedDiffMatches(id, "g1") || rt.cachedDiffMatches(id, "g2") {
		t.Fatal("the cache must match exactly its Generation")
	}
	// A commit whose reply was lost but which became the head counts.
	if err := writeStatTable(pendingTablePath(rt.dataRoot, id), statTable{Generation: "g2"}); err != nil {
		t.Fatal(err)
	}
	if !rt.cachedDiffMatches(id, "g2") {
		t.Fatal("a pending commit that became the head is not the cache")
	}
	if table, _ := readStatTable(statTablePath(rt.dataRoot, id)); table.Generation != "g2" {
		t.Fatalf("table = %q after settling the pending commit", table.Generation)
	}
}

// A stopped Sandbox's diff is an eviction candidate only once committed; a
// Create that takes it first keeps it (runtime.md §16, defaults.md).
func TestEvictCommittedSandboxDisk(t *testing.T) {
	rt := newRuntime(ProviderContainerd, t.TempDir(), "a")
	driver := &stopRecorder{dropped: make(chan struct{})}
	rt.driver = driver
	var sent []*nodev1.SandboxObserved
	rt.send = func(m *nodev1.AgentMessage) { sent = append(sent, m.GetSandboxObserved()) }
	committed, running := uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{committed, running} {
		if err := writeStatTable(statTablePath(rt.dataRoot, id), statTable{Generation: "g1"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(cleanMarkPath(rt.dataRoot, committed), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var disks []string
	for _, c := range rt.evictionCandidates(context.Background(), nil) {
		if c.kind == evictSandboxDisk {
			disks = append(disks, c.key)
		}
	}
	if len(disks) != 1 || disks[0] != committed.String() {
		t.Fatalf("disk candidates = %v, want only %s", disks, committed)
	}
	rt.evictSandboxDisk(context.Background(), running.String()) // taken by a Create since
	select {
	case <-driver.dropped:
		t.Fatal("evicted a disk that is not committed")
	default:
	}
	rt.evictSandboxDisk(context.Background(), committed.String())
	select {
	case <-driver.dropped:
	default:
		t.Fatal("the committed disk was not evicted")
	}
	if rt.cachedDiffMatches(committed, "g1") {
		t.Fatal("an evicted disk still claims to be the head")
	}
	// The Control Plane hears of it, so it stops preferring this Node.
	if len(sent) != 1 || sent[0].GetSandboxId() != committed.String() || sent[0].GetPhase() != "disk_dropped" {
		t.Fatalf("reports = %v, want one disk_dropped for %s", sent, committed)
	}
}
