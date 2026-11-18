package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/zibyn/orrery/internal/generation"
	"github.com/zibyn/orrery/internal/manifest"
	"github.com/zibyn/orrery/internal/resourcefs"
)

// fakeVolumes is orrery-resourcefs's writable side: one Volume whose next
// seal is set by the test, recording what the Agent tells it.
type fakeVolumes struct {
	mu        sync.Mutex
	sealed    resourcefs.Sealed
	seals     int
	committed []string
	unmounted []string
}

func (f *fakeVolumes) MountVolume(context.Context, string, string) error { return nil }

func (f *fakeVolumes) UnmountVolume(_ context.Context, volume string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unmounted = append(f.unmounted, volume)
	return nil
}

func (f *fakeVolumes) SealVolume(context.Context, string) (resourcefs.Sealed, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seals++
	return f.sealed, nil
}

func (f *fakeVolumes) SetVolumeUpload(_ context.Context, _ string, upload string, seq uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sealed.Upload, f.sealed.UploadSeq = upload, seq
	return nil
}

func (f *fakeVolumes) VolumeCommitted(_ context.Context, _ string, generation string, seq uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sealed.Upload = ""
	if generation != "" {
		f.committed = append(f.committed, generation)
		f.sealed.Base, f.sealed.Dirty, f.sealed.Manifest, f.sealed.Pending = generation, false, nil, nil
	}
	return nil
}

// dirtyVolume seals one new file per content, their objects written where
// the daemon keeps sealed objects.
func dirtyVolume(t *testing.T, root, volume string, contents ...string) resourcefs.Sealed {
	t.Helper()
	m := generation.Manifest{Kind: generation.KindTree, Entries: []generation.Entry{
		{Entry: manifest.Entry{Path: "out", Type: manifest.TypeDir}, Mode: 0o755},
	}}
	var pending []string
	for i, content := range contents {
		sum := sha256.Sum256([]byte(content))
		digest := hex.EncodeToString(sum[:])
		writeTestFile(t, filepath.Join(volumeObjectsPath(root, volume), digest), content, 0o600)
		size := int64(len(content))
		m.Entries = append(m.Entries, generation.Entry{Entry: manifest.Entry{
			Path: fmt.Sprintf("out/%d", i), Type: manifest.TypeFile, Size: &size, SHA256: digest,
		}, Mode: 0o644})
		pending = append(pending, digest)
	}
	return resourcefs.Sealed{Base: "head-0", Seq: 3, Dirty: true, Manifest: &m, Pending: pending}
}

// manyContents is n distinct small file contents.
func manyContents(n int) []string {
	contents := make([]string, n)
	for i := range contents {
		contents[i] = fmt.Sprintf("object %d", i)
	}
	return contents
}

// assertStored checks that every sealed object reached the store with its
// bytes.
func assertStored(t *testing.T, node *fakeGenerationNode, contents []string) {
	t.Helper()
	node.mu.Lock() // a PUT canceled by a failure may still be landing
	defer node.mu.Unlock()
	for _, content := range contents {
		sum := sha256.Sum256([]byte(content))
		if got := string(node.objects[hex.EncodeToString(sum[:])]); got != content {
			t.Fatalf("object %q stored as %q", content, got)
		}
	}
}

// A commit of many objects uploads them concurrently, at most 20 at a time
// (defaults.md "Node 并发对象上传"), and commits the sealed tree.
func TestVolumeCommitUploadsConcurrently(t *testing.T) {
	root := t.TempDir()
	volume := uuid.NewString()
	node := newFakeGenerationNode(t)
	node.putDelay = 20 * time.Millisecond
	contents := manyContents(100)
	sealed := dirtyVolume(t, root, volume, contents...)
	vols := &fakeVolumes{sealed: sealed}
	rt := newRuntime(ProviderContainerd, root, "node")
	rt.client, rt.volumes = node, vols

	if err := rt.commitVolumeOnce(context.Background(), 1, volume); err != nil {
		t.Fatal(err)
	}
	if node.maxInflight < 2 || node.maxInflight > 20 {
		t.Fatalf("%d PUTs in flight at most, want 2 to 20", node.maxInflight)
	}
	assertStored(t, node, contents)
	for digest, n := range node.puts {
		if n != 1 {
			t.Fatalf("object %s PUT %d times, want once", digest, n)
		}
	}
	want, err := generation.Encode(*sealed.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if node.head == "" || !bytes.Equal(node.manifests[node.head], want) {
		t.Fatalf("head %q holds a manifest other than the sealed tree", node.head)
	}
}

// One failed PUT fails the commit, cancels the other uploads in flight and
// starts no more, and leaves no upload for the daemon to retry; the next
// commit submits afresh on the same base and uploads what the store still
// lacks.
func TestVolumeCommitFailsOnOneFailedUpload(t *testing.T) {
	root := t.TempDir()
	volume := uuid.NewString()
	node := newFakeGenerationNode(t)
	contents := manyContents(100)
	vols := &fakeVolumes{sealed: dirtyVolume(t, root, volume, contents...)}
	node.failPut = vols.sealed.Pending[0] // the first PUT to start
	node.holdPuts = true                  // the others answer only once canceled
	rt := newRuntime(ProviderContainerd, root, "node")
	rt.client, rt.volumes = node, vols

	if err := rt.commitVolumeOnce(context.Background(), 1, volume); err == nil {
		t.Fatal("a commit with a failed upload succeeded")
	}
	if node.head != "" || len(vols.committed) != 0 || vols.sealed.Upload != "" {
		t.Fatalf("head %q, daemon told %v, pending upload %q after a failed upload", node.head, vols.committed, vols.sealed.Upload)
	}
	node.mu.Lock()
	stored := len(node.objects)
	node.holdPuts = false
	node.mu.Unlock()
	if stored != 0 {
		t.Fatalf("%d of %d objects stored after the first failure, want the rest canceled", stored, len(contents))
	}

	if err := rt.commitVolumeOnce(context.Background(), 1, volume); err != nil {
		t.Fatal(err)
	}
	if node.head == "" || len(vols.committed) != 1 || vols.committed[0] != node.head {
		t.Fatalf("head %q, daemon told %v", node.head, vols.committed)
	}
	if len(node.bases) != 2 || node.bases[1] != "head-0" {
		t.Fatalf("bases = %v, want the retry on the Volume's head", node.bases)
	}
	assertStored(t, node, contents)
}

func TestVolumeCommitUploadsTheDirtySetAndAdvances(t *testing.T) {
	root := t.TempDir()
	volume := uuid.NewString()
	node := newFakeGenerationNode(t)
	vols := &fakeVolumes{sealed: dirtyVolume(t, root, volume, "42")}
	rt := newRuntime(ProviderContainerd, root, "node")
	rt.client, rt.volumes = node, vols

	if err := rt.commitVolumeOnce(context.Background(), 1, volume); err != nil {
		t.Fatal(err)
	}
	if node.head == "" || len(vols.committed) != 1 || vols.committed[0] != node.head {
		t.Fatalf("head %q, daemon told %v", node.head, vols.committed)
	}
	if len(node.bases) != 1 || node.bases[0] != "head-0" {
		t.Fatalf("bases = %v, want the Volume's head", node.bases)
	}
	sum := sha256.Sum256([]byte("42"))
	if string(node.objects[hex.EncodeToString(sum[:])]) != "42" {
		t.Fatal("the sealed object was not uploaded")
	}
	m, err := generation.Decode(node.manifests[node.head])
	if err != nil || len(m.Entries) != 2 || m.Kind != generation.KindTree {
		t.Fatalf("uploaded manifest = %+v, %v", m, err)
	}

	// Nothing written since: no commit.
	if err := rt.commitVolumeOnce(context.Background(), 1, volume); err != nil {
		t.Fatal(err)
	}
	if len(node.bases) != 1 {
		t.Fatal("an unchanged Volume was committed again")
	}
}

// A commit sent before an Agent restart or a lost reply is retried, not
// rebuilt on a stale base.
func TestVolumeCommitRetriesThePendingUpload(t *testing.T) {
	root := t.TempDir()
	volume := uuid.NewString()
	node := newFakeGenerationNode(t)
	node.manifests["upload-1"] = []byte("{}")
	vols := &fakeVolumes{sealed: resourcefs.Sealed{Base: "head-0", Upload: "upload-1", UploadSeq: 2, Seq: 2, Dirty: true}}
	rt := newRuntime(ProviderContainerd, root, "node")
	rt.client, rt.volumes = node, vols
	if err := rt.commitVolumeOnce(context.Background(), 1, volume); err != nil {
		t.Fatal(err)
	}
	if node.head != "upload-1" || len(node.bases) != 0 {
		t.Fatalf("head %q after %d submits, want the pending upload and none", node.head, len(node.bases))
	}
}

func TestVolumeCommitHolderRefused(t *testing.T) {
	root := t.TempDir()
	volume := uuid.NewString()
	node := newFakeGenerationNode(t)
	node.refuse = status.Error(codes.FailedPrecondition, "no Sandbox holds the Volume")
	vols := &fakeVolumes{sealed: dirtyVolume(t, root, volume, "x")}
	rt := newRuntime(ProviderContainerd, root, "node")
	rt.client, rt.volumes = node, vols
	if err := rt.commitVolumeOnce(context.Background(), 1, volume); !errors.Is(err, errHolderRefused) {
		t.Fatalf("err = %v, want errHolderRefused", err)
	}
}

// A stop commits each Volume before it unmounts it and forgets the record.
func TestStopCommitsVolumesBeforeReleasingThem(t *testing.T) {
	fastCommitRetries(t)
	root := t.TempDir()
	volume := uuid.NewString()
	node := newFakeGenerationNode(t)
	node.unavailable = 1 // the first attempt fails and is retried
	vols := &fakeVolumes{sealed: dirtyVolume(t, root, volume, "final")}
	rt := newRuntime(ProviderContainerd, root, "node")
	rt.client, rt.volumes = node, vols
	id := uuid.New()
	if _, err := rt.mountVolumes(context.Background(), id, []volumeMount{{ID: volume, Path: "/data", Name: "data"}}); err != nil {
		t.Fatal(err)
	}
	if err := rt.commitVolumes(context.Background(), id, 1, func() {}); err != nil {
		t.Fatal(err)
	}
	if len(vols.committed) != 1 || len(vols.unmounted) != 1 || vols.unmounted[0] != volume {
		t.Fatalf("committed %v, unmounted %v", vols.committed, vols.unmounted)
	}
	if _, err := os.Stat(volumeRecordPath(root, id)); !os.IsNotExist(err) {
		t.Fatal("the Volume record outlived the stop")
	}
}
