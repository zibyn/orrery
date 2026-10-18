package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestEvictionLRUOrder checks the three cache classes share one oldest-first
// order: the least recently used candidate goes first, regardless of class.
func TestEvictionLRUOrder(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	candidates := []evictionCandidate{
		{kind: evictTree, key: "tree-new", lastUsed: now},
		{kind: evictImage, key: "image-oldest", lastUsed: now.Add(-3 * time.Hour)},
		{kind: evictTree, key: "tree-old", lastUsed: now.Add(-2 * time.Hour)},
		{kind: evictImage, key: "image-new", lastUsed: now.Add(-time.Hour)},
	}
	sortEvictionCandidates(candidates)
	want := []string{"image-oldest", "tree-old", "image-new", "tree-new"}
	for i, key := range want {
		if candidates[i].key != key {
			t.Fatalf("candidate %d = %q, want %q (order %+v)", i, candidates[i].key, key, candidates)
		}
	}
}

// TestSandboxPinsSurviveStop is the retention half of eviction: the pins live
// in the Sandbox's data-root record, so a stopped Sandbox still protects its
// version trees and image after the Agent restarts.
func TestSandboxPinsSurviveStop(t *testing.T) {
	root := t.TempDir()
	sandboxDir := filepath.Join(root, "sandboxes", uuid.NewString())
	split := "sha256:" + "abcdef"
	if err := writeSandboxPins(sandboxDir, sandboxPins{
		ImageDigest: "127.0.0.1:15000/snapshots/x@" + split,
		Resources:   []string{"address-v1", "address-v2"},
	}); err != nil {
		t.Fatal(err)
	}

	trees, images := loadNodePins(root)
	for _, address := range []string{"address-v1", "address-v2"} {
		if !trees[address] {
			t.Fatalf("tree %q is not pinned: %v", address, trees)
		}
	}
	if !images[split] {
		t.Fatalf("image %q is not pinned: %v", split, images)
	}
}

func TestImagePinnedByDigest(t *testing.T) {
	split := "sha256:" + "0123456789"
	pins := map[string]bool{split: true}
	if !imagePinned(pins, dockerImage{ID: split, RepoDigests: []string{"a/b@" + split}}) {
		t.Fatal("an image matching a pinned digest is not pinned")
	}
	if imagePinned(pins, dockerImage{ID: "sha256:other", RepoDigests: []string{"a/b@sha256:other"}}) {
		t.Fatal("an unrelated image is pinned")
	}
}

// TestReclaimObjects is tree -> object reclamation (runtime.md §11): an object
// is kept while a version tree hard-links it and removed once the last tree is
// gone.
func TestReclaimObjects(t *testing.T) {
	root := t.TempDir()
	cache := newResourceCache(root)
	object := cache.objectPath("aaaa", false)
	if err := os.MkdirAll(filepath.Dir(object), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(object, []byte("payload"), 0o444); err != nil {
		t.Fatal(err)
	}
	tree := cache.versionPath("address")
	if err := os.MkdirAll(tree, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(object, filepath.Join(tree, "payload")); err != nil {
		t.Fatal(err)
	}

	cache.reclaimObjects()
	if _, err := os.Stat(object); err != nil {
		t.Fatalf("a hard-linked object was reclaimed: %v", err)
	}

	if err := removeTree(tree); err != nil {
		t.Fatalf("remove tree: %v", err)
	}
	cache.reclaimObjects()
	if _, err := os.Stat(object); !os.IsNotExist(err) {
		t.Fatalf("an unlinked object survived reclamation (stat err = %v)", err)
	}
	if entries, err := os.ReadDir(filepath.Dir(object)); err == nil && len(entries) != 0 {
		t.Fatalf("empty object directory was not removed: %v", entries)
	}
}
