package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/core/images"
	"github.com/google/uuid"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// TestContainerdImageEviction: a containerd image record is removed by name
// and pinned by its target digest, whatever the name says (runtime.md §11).
func TestContainerdImageEviction(t *testing.T) {
	target := digest.Digest("sha256:" + strings.Repeat("a", 64))
	created := time.Unix(1234, 0)
	for _, name := range []string{
		"127.0.0.1:15000/snapshots/x@" + target.String(),
		"127.0.0.1:15000/e2e-evict:1",
	} {
		image := containerdCachedImage(images.Image{
			Name: name, CreatedAt: created, Target: ocispec.Descriptor{Digest: target},
		})
		if image.ID != name {
			t.Errorf("%s: removal key = %q", name, image.ID)
		}
		if !imagePinned(map[string]bool{target.String(): true}, image) {
			t.Errorf("%s: not pinned by its target digest", name)
		}
		if imagePinned(map[string]bool{"sha256:other": true}, image) {
			t.Errorf("%s: pinned by an unrelated digest", name)
		}
		if got := imageLastUsed(nil, image); !got.Equal(created) {
			t.Errorf("%s: untouched clock = %v, want CreatedAt", name, got)
		}
	}
}

// TestEvictionLRUOrder checks the three cache classes share one oldest-first
// order: the least recently used candidate goes first, regardless of class.
func TestEvictionLRUOrder(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	candidates := []evictionCandidate{
		{kind: evictBlock, key: "block-new", lastUsed: now},
		{kind: evictImage, key: "image-oldest", lastUsed: now.Add(-3 * time.Hour)},
		{kind: evictBlock, key: "block-old", lastUsed: now.Add(-2 * time.Hour)},
		{kind: evictImage, key: "image-new", lastUsed: now.Add(-time.Hour)},
	}
	sortEvictionCandidates(candidates)
	want := []string{"image-oldest", "block-old", "image-new", "block-new"}
	for i, key := range want {
		if candidates[i].key != key {
			t.Fatalf("candidate %d = %q, want %q (order %+v)", i, candidates[i].key, key, candidates)
		}
	}
}

// TestSandboxPinsSurviveStop is the retention half of eviction: the pins live
// in the Sandbox's data-root record, so a stopped Sandbox still protects its
// image after the Agent restarts.
func TestSandboxPinsSurviveStop(t *testing.T) {
	root := t.TempDir()
	sandboxDir := filepath.Join(root, "sandboxes", uuid.NewString())
	split := "sha256:" + "abcdef"
	if err := writeSandboxPins(sandboxDir, sandboxPins{
		ImageDigest: "127.0.0.1:15000/snapshots/x@" + split,
	}); err != nil {
		t.Fatal(err)
	}
	if images := loadImagePins(root); !images[split] {
		t.Fatalf("image %q is not pinned: %v", split, images)
	}
}

// TestBlockCandidates: every cached block is a candidate, by its mtime, even
// one of a mounted version; partial downloads are not (runtime.md §11).
func TestBlockCandidates(t *testing.T) {
	root := t.TempDir()
	blocks := blockCachePath(root)
	if err := os.MkdirAll(blocks, 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	for _, name := range []string{"aaaa", "bbbb", ".tmp-123"} {
		path := filepath.Join(blocks, name)
		if err := os.WriteFile(path, []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	r := newRuntime(ProviderLocal, root, "node")
	var got []string
	for _, candidate := range r.evictionCandidates(context.Background(), nil) {
		if candidate.kind == evictBlock {
			got = append(got, filepath.Base(candidate.key))
			if !candidate.lastUsed.Equal(old) || candidate.size != 4 {
				t.Fatalf("candidate %+v", candidate)
			}
		}
	}
	if strings.Join(got, ",") != "aaaa,bbbb" {
		t.Fatalf("block candidates = %v", got)
	}
}

// TestImageEvictionUsesLastUsed is the image half of LRU eviction
// (runtime.md §11): a touch recorded when a Sandbox uses an image beats both
// another image's newer Created time and an untouched image.
func TestImageEvictionUsesLastUsed(t *testing.T) {
	root := t.TempDir()
	used := cachedImage{
		ID:          "sha256:used",
		RepoDigests: []string{"registry.example/snapshots/x@sha256:used"},
		Created:     1000,
	}
	untouched := cachedImage{ID: "sha256:untouched", Created: 2000}

	touchImageUsage(root, "registry.example/snapshots/x@sha256:used")
	usage := loadImageUsage(root)
	if got := imageLastUsed(usage, untouched); !got.Equal(time.Unix(2000, 0)) {
		t.Fatalf("untouched image clock = %v, want Created", got)
	}
	candidates := []evictionCandidate{
		{kind: evictImage, key: used.ID, lastUsed: imageLastUsed(usage, used)},
		{kind: evictImage, key: untouched.ID, lastUsed: imageLastUsed(usage, untouched)},
	}
	sortEvictionCandidates(candidates)
	if candidates[0].key != untouched.ID {
		t.Fatalf("eviction order = %+v, want the untouched image first", candidates)
	}
}

func TestImagePinnedByDigest(t *testing.T) {
	split := "sha256:" + "0123456789"
	pins := map[string]bool{split: true}
	if !imagePinned(pins, cachedImage{ID: split, RepoDigests: []string{"a/b@" + split}}) {
		t.Fatal("an image matching a pinned digest is not pinned")
	}
	if imagePinned(pins, cachedImage{ID: "sha256:other", RepoDigests: []string{"a/b@sha256:other"}}) {
		t.Fatal("an unrelated image is pinned")
	}
}
