package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zibyn/orrery/internal/manifest"
)

func TestResourceCachePaths(t *testing.T) {
	cache := newResourceCache("/data")
	digest := strings.Repeat("ab", 32)
	if got, want := cache.objectPath(digest, false), filepath.Join("/data", "cache", "objects", digest, "data"); got != want {
		t.Fatalf("plain object path = %q, want %q", got, want)
	}
	if got, want := cache.objectPath(digest, true), filepath.Join("/data", "cache", "objects", digest, "exec"); got != want {
		t.Fatalf("executable object path = %q, want %q", got, want)
	}
	if got, want := cache.versionPath(digest), filepath.Join("/data", "cache", "versions", digest); got != want {
		t.Fatalf("version path = %q, want %q", got, want)
	}
	// The executable bit splits the key: one inode cannot carry two modes.
	if cache.objectPath(digest, false) == cache.objectPath(digest, true) {
		t.Fatal("executable and plain objects share a cache path")
	}
}

func TestAssembleTreeSharesObjectsByHardLink(t *testing.T) {
	root := t.TempDir()
	makeRemovable(t, root)
	cache := newResourceCache(root)
	shared := writeObject(t, cache, []byte("shared content\n"), false)
	other := writeObject(t, cache, []byte("second version\n"), false)
	run := writeObject(t, cache, []byte("#!/bin/sh\n"), true)

	first := manifest.Manifest{Entries: []manifest.Entry{
		{Path: "d", Type: manifest.TypeDir},
		{Path: "d/shared.txt", Type: manifest.TypeFile, Size: sizeOf(len("shared content\n")), SHA256: shared},
		{Path: "empty", Type: manifest.TypeDir},
		{Path: "link", Type: manifest.TypeSymlink, Target: "d/shared.txt"},
		{Path: "run.sh", Type: manifest.TypeFile, Size: sizeOf(len("#!/bin/sh\n")), SHA256: run, Executable: true},
	}}
	tree, err := cache.assembleTree("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", first)
	if err != nil {
		t.Fatalf("assemble first tree: %v", err)
	}
	assertFile(t, filepath.Join(tree, "d", "shared.txt"), "shared content\n", 0o444)
	assertFile(t, filepath.Join(tree, "run.sh"), "#!/bin/sh\n", 0o555)
	if info, err := os.Stat(filepath.Join(tree, "empty")); err != nil || !info.IsDir() {
		t.Fatalf("empty directory missing: %v", err)
	}
	if target, err := os.Readlink(filepath.Join(tree, "link")); err != nil || target != "d/shared.txt" {
		t.Fatalf("symlink = %q, %v", target, err)
	}
	if info, err := os.Stat(tree); err != nil || info.Mode().Perm() != 0o555 {
		t.Fatalf("tree root mode = %v, %v", info.Mode().Perm(), err)
	}

	// A second version that differs by one file hard-links the shared object:
	// the cache stores those bytes once.
	second := manifest.Manifest{Entries: []manifest.Entry{
		{Path: "d", Type: manifest.TypeDir},
		{Path: "d/shared.txt", Type: manifest.TypeFile, Size: sizeOf(len("shared content\n")), SHA256: shared},
		{Path: "other.txt", Type: manifest.TypeFile, Size: sizeOf(len("second version\n")), SHA256: other},
	}}
	tree2, err := cache.assembleTree("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", second)
	if err != nil {
		t.Fatalf("assemble second tree: %v", err)
	}
	firstInfo, err := os.Stat(filepath.Join(tree, "d", "shared.txt"))
	if err != nil {
		t.Fatal(err)
	}
	secondInfo, err := os.Stat(filepath.Join(tree2, "d", "shared.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(firstInfo, secondInfo) {
		t.Fatal("shared file is not a hard link across version trees")
	}
	// One cached object per (sha256, executable bit).
	entries, err := os.ReadDir(filepath.Join(cache.root, "objects", shared))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "data" {
		t.Fatalf("object directory entries = %v, want only data", entries)
	}

	// An executable reference to the same bytes needs its own object and
	// its own inode, because the mode is part of the key.
	writeObject(t, cache, []byte("shared content\n"), true)
	execTree, err := cache.assembleTree("cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", manifest.Manifest{Entries: []manifest.Entry{
		{Path: "d", Type: manifest.TypeDir},
		{Path: "d/shared.txt", Type: manifest.TypeFile, Size: sizeOf(len("shared content\n")), SHA256: shared, Executable: true},
	}})
	if err != nil {
		t.Fatalf("assemble executable tree: %v", err)
	}
	assertFile(t, filepath.Join(execTree, "d", "shared.txt"), "shared content\n", 0o555)
	execInfo, err := os.Stat(filepath.Join(execTree, "d", "shared.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(firstInfo, execInfo) {
		t.Fatal("executable variant shares an inode with the plain variant")
	}

	// Rebuilding a version reuses the existing tree.
	again, err := cache.assembleTree("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", first)
	if err != nil || again != tree {
		t.Fatalf("second assembly = %q, %v; want %q", again, err, tree)
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

func TestDownloadVerified(t *testing.T) {
	content := []byte("object bytes")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(content)
	}))
	defer server.Close()
	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])

	path := filepath.Join(t.TempDir(), "object")
	if err := downloadVerified(context.Background(), server.URL, path, digest); err != nil {
		t.Fatalf("download verified: %v", err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != string(content) {
		t.Fatalf("downloaded = %q, %v", data, err)
	}

	wrong := strings.Repeat("0", 64)
	if err := downloadVerified(context.Background(), server.URL, path, wrong); err == nil {
		t.Fatal("download with a wrong digest succeeded")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("mismatched download was kept: %v", err)
	}
}

func TestDownloadObjectInstallsBothVariants(t *testing.T) {
	content := []byte("both modes\n")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(content)
	}))
	defer server.Close()
	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])
	cache := newResourceCache(t.TempDir())

	err := downloadObject(context.Background(), server.URL, cache, digest, []objectVariant{
		{digest: digest, executable: false},
		{digest: digest, executable: true},
	})
	if err != nil {
		t.Fatalf("download object: %v", err)
	}
	assertFile(t, cache.objectPath(digest, false), string(content), 0o444)
	assertFile(t, cache.objectPath(digest, true), string(content), 0o555)
	plain, err := os.Stat(cache.objectPath(digest, false))
	if err != nil {
		t.Fatal(err)
	}
	exec, err := os.Stat(cache.objectPath(digest, true))
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(plain, exec) {
		t.Fatal("variants share an inode and therefore a mode")
	}
}

func TestDownloadObjectSingleVariantIsReadOnly(t *testing.T) {
	content := []byte("one mode\n")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(content)
	}))
	defer server.Close()
	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])
	cache := newResourceCache(t.TempDir())
	if err := downloadObject(context.Background(), server.URL, cache, digest, []objectVariant{
		{digest: digest, executable: true},
	}); err != nil {
		t.Fatalf("download object: %v", err)
	}
	assertFile(t, cache.objectPath(digest, true), string(content), 0o555)
}

// writeObject puts one object in the cache and returns its digest.
func writeObject(t *testing.T, cache *resourceCache, content []byte, executable bool) string {
	t.Helper()
	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])
	path := cache.objectPath(digest, executable)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, objectMode(executable)); err != nil {
		t.Fatal(err)
	}
	return digest
}

func assertFile(t *testing.T, path, want string, mode os.FileMode) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(data) != want {
		t.Fatalf("%s = %q, want %q", path, data, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != mode {
		t.Fatalf("%s mode = %o, want %o", path, info.Mode().Perm(), mode)
	}
}

func sizeOf(n int) *int64 {
	value := int64(n)
	return &value
}

// makeRemovable lets TempDir's cleanup delete the read-only trees this test
// creates.
func makeRemovable(t *testing.T, root string) {
	t.Helper()
	t.Cleanup(func() {
		_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if err == nil {
				_ = os.Chmod(path, 0o755)
			}
			return nil
		})
	})
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
