package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	nodev1 "github.com/zibyn/stars-sandbox/gen/node/v1"
	"github.com/zibyn/stars-sandbox/internal/manifest"
)

// resourceMountRoot is where every Resource version tree appears in the
// Sandbox (runtime.md §2).
const resourceMountRoot = "/resources"

// maxResourceManifestBytes matches the Control Plane's manifest body bound.
const maxResourceManifestBytes = 256 << 20

// resourceFetchBackoff is the Resource fetch retry schedule (defaults.md:
// 1, 2, 4, 8, 16 seconds, the same group as Workspace provisioning). A var so
// tests can shorten it.
var resourceFetchBackoff = []time.Duration{
	time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second,
}

// resourceTreeMu serializes tree assembly. Two Sandboxes mounting the same
// version may create it concurrently; the tree is built in a temporary
// directory and renamed, so one build wins.
var resourceTreeMu sync.Mutex

// resourceHTTPClient fetches presigned URLs. The URL lifetime is an hour, so a
// long single-file download needs more than the default client timeout.
var resourceHTTPClient = &http.Client{Timeout: 10 * time.Minute}

// resourceCache is the Node's on-disk Content Addressable cache
// (runtime.md §11):
//
//	cache/objects/<sha256>/data  mode 0444
//	cache/objects/<sha256>/exec  mode 0555
//	cache/versions/<address>/... version tree built from hard links
//
// Keying the object by (sha256, executable bit) is what lets a tree hard-link
// it: a hard link shares the inode, hence the mode, so the two variants cannot
// be one file.
type resourceCache struct {
	root string
}

func newResourceCache(dataRoot string) *resourceCache {
	return &resourceCache{root: filepath.Join(dataRoot, "cache")}
}

func (c *resourceCache) objectPath(digest string, executable bool) string {
	variant := "data"
	if executable {
		variant = "exec"
	}
	return filepath.Join(c.root, "objects", digest, variant)
}

func (c *resourceCache) versionPath(address string) string {
	return filepath.Join(c.root, "versions", address)
}

func objectMode(executable bool) os.FileMode {
	if executable {
		return 0o555
	}
	return 0o444
}

// retryResourceFetch runs fetch until it succeeds, sleeping the schedule after
// each failure. len(backoff) retries means len(backoff)+1 attempts, matching
// the Workspace provisioning loop.
func retryResourceFetch(ctx context.Context, backoff []time.Duration, fetch func() error) error {
	var err error
	for attempt := 0; ; attempt++ {
		if err = fetch(); err == nil {
			return nil
		}
		if attempt >= len(backoff) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff[attempt]):
		}
	}
}

// ensureResourceVersion downloads whatever the cache lacks and returns the
// version tree path. A complete tree is reused as is, including after an Agent
// restart, so a restarted Sandbox mounts the version pinned at create time.
func (r *Runtime) ensureResourceVersion(ctx context.Context, address string) (string, error) {
	cache := newResourceCache(r.dataRoot)
	tree := cache.versionPath(address)
	if _, err := os.Stat(tree); err == nil {
		return tree, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}

	var m manifest.Manifest
	err := retryResourceFetch(ctx, resourceFetchBackoff, func() error {
		fetched, err := r.fetchResourceManifest(ctx, address)
		if err != nil {
			return err
		}
		m = fetched
		return r.fetchResourceObjects(ctx, cache, m)
	})
	if err != nil {
		return "", err
	}
	return cache.assembleTree(address, m)
}

// fetchResourceManifest reads the version manifest through a Presign GET
// (node-protocol.md: Nodes never reach the object store directly).
func (r *Runtime) fetchResourceManifest(ctx context.Context, address string) (manifest.Manifest, error) {
	resp, err := r.presign(ctx, &nodev1.PresignRequest{Manifests: []string{address}, Method: "GET"})
	if err != nil {
		return manifest.Manifest{}, err
	}
	if len(resp.GetUrls()) != 1 {
		return manifest.Manifest{}, fmt.Errorf("presign returned %d manifest URLs", len(resp.GetUrls()))
	}
	data, err := downloadResource(ctx, resp.GetUrls()[0], maxResourceManifestBytes)
	if err != nil {
		return manifest.Manifest{}, err
	}
	var m manifest.Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return manifest.Manifest{}, fmt.Errorf("decode manifest %s: %w", address, err)
	}
	return manifest.Normalize(m)
}

type objectVariant struct {
	digest     string
	executable bool
}

// fetchResourceObjects downloads every missing (sha256, executable) object,
// verifying the digest on the way to disk.
func (r *Runtime) fetchResourceObjects(ctx context.Context, cache *resourceCache, m manifest.Manifest) error {
	needed := make(map[objectVariant]struct{})
	for _, entry := range m.Entries {
		if entry.Type == manifest.TypeFile {
			needed[objectVariant{digest: entry.SHA256, executable: entry.Executable}] = struct{}{}
		}
	}
	digests := make(map[string][]objectVariant)
	for key := range needed {
		if _, err := os.Stat(cache.objectPath(key.digest, key.executable)); err == nil {
			continue
		}
		digests[key.digest] = append(digests[key.digest], key)
	}
	if len(digests) == 0 {
		return nil
	}
	sha := make([]string, 0, len(digests))
	for digest := range digests {
		sha = append(sha, digest)
	}
	sort.Strings(sha)
	resp, err := r.presign(ctx, &nodev1.PresignRequest{Sha256: sha, Method: "GET"})
	if err != nil {
		return err
	}
	if len(resp.GetUrls()) != len(sha) {
		return fmt.Errorf("presign returned %d object URLs, want %d", len(resp.GetUrls()), len(sha))
	}
	for i, digest := range sha {
		if err := downloadObject(ctx, resp.GetUrls()[i], cache, digest, digests[digest]); err != nil {
			return err
		}
	}
	return nil
}

// downloadObject streams one object once and installs every missing variant.
// Variants differ only in mode, so a second variant is a copy of the same
// bytes rather than a hard link.
func downloadObject(ctx context.Context, url string, cache *resourceCache, digest string, variants []objectVariant) error {
	dir := filepath.Dir(cache.objectPath(digest, false))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(dir, ".tmp-"+digest)
	if err := downloadVerified(ctx, url, tmp, digest); err != nil {
		return err
	}
	if len(variants) == 1 {
		// The common case: install the downloaded file directly, read-only.
		mode := objectMode(variants[0].executable)
		if err := os.Chmod(tmp, mode); err != nil {
			os.Remove(tmp) //nolint:errcheck // best effort
			return err
		}
		if err := os.Rename(tmp, cache.objectPath(variants[0].digest, variants[0].executable)); err != nil {
			os.Remove(tmp) //nolint:errcheck // best effort
			return err
		}
		return nil
	}
	// Both modes are needed; they cannot share an inode, so copy the bytes.
	defer os.Remove(tmp) //nolint:errcheck // best effort
	for _, variant := range variants {
		if err := copyWithMode(tmp, cache.objectPath(variant.digest, variant.executable), objectMode(variant.executable)); err != nil {
			return err
		}
	}
	return nil
}

// downloadVerified writes url to path and rejects bytes whose SHA-256 differs
// from the manifest's digest.
func downloadVerified(ctx context.Context, url, path, want string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	response, err := resourceHTTPClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch object: %s", response.Status)
	}
	out, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	hasher := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(out, hasher), io.LimitReader(response.Body, manifest.MaxFileSize+1))
	closeErr := out.Close()
	if copyErr != nil {
		os.Remove(path) //nolint:errcheck // best effort
		return copyErr
	}
	if closeErr != nil {
		os.Remove(path) //nolint:errcheck // best effort
		return closeErr
	}
	if got := hex.EncodeToString(hasher.Sum(nil)); got != want {
		os.Remove(path) //nolint:errcheck // best effort
		return fmt.Errorf("object %s has sha256 %s", want, got)
	}
	return nil
}

func downloadResource(ctx context.Context, url string, limit int64) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	response, err := resourceHTTPClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch manifest: %s", response.Status)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("manifest is too large")
	}
	return data, nil
}

func copyWithMode(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close() //nolint:errcheck // read-only
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close() //nolint:errcheck // report the copy error instead
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, mode)
}

// assembleTree builds the version tree from hard links in a temporary
// directory and renames it into place. The caller has already downloaded every
// referenced object.
func (c *resourceCache) assembleTree(address string, m manifest.Manifest) (string, error) {
	resourceTreeMu.Lock()
	defer resourceTreeMu.Unlock()
	final := c.versionPath(address)
	if _, err := os.Stat(final); err == nil {
		return final, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	versionsDir := filepath.Join(c.root, "versions")
	if err := os.MkdirAll(versionsDir, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(versionsDir, ".tmp-")
	if err != nil {
		return "", err
	}
	cleanup := func() { os.RemoveAll(tmp) } //nolint:errcheck // best effort
	for _, entry := range m.Entries {
		target := filepath.Join(tmp, filepath.FromSlash(entry.Path))
		switch entry.Type {
		case manifest.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				cleanup()
				return "", err
			}
		case manifest.TypeFile:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				cleanup()
				return "", err
			}
			if err := os.Link(c.objectPath(entry.SHA256, entry.Executable), target); err != nil {
				cleanup()
				return "", err
			}
		case manifest.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				cleanup()
				return "", err
			}
			if err := os.Symlink(entry.Target, target); err != nil {
				cleanup()
				return "", err
			}
		}
	}
	// Files are done; make the directories read-only from the deepest up.
	var dirs []string
	if err := filepath.WalkDir(tmp, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			dirs = append(dirs, path)
		}
		return nil
	}); err != nil {
		cleanup()
		return "", err
	}
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
	for _, dir := range dirs {
		if err := os.Chmod(dir, 0o555); err != nil {
			cleanup()
			return "", err
		}
	}
	if err := os.Rename(tmp, final); err != nil {
		cleanup()
		return "", err
	}
	return final, nil
}

// validResourceMount keeps the mount one path segment. The Control Plane
// validates this too; the Agent does not trust it blindly.
func validResourceMount(mount string) bool {
	return mount != "" && mount != "." && mount != ".." && !strings.ContainsAny(mount, "/\x00")
}

// resourceBinds fetches every version and returns the read-only binds for the
// Sandbox's HostConfig.
func (r *Runtime) resourceBinds(ctx context.Context, resources []resourceMount) ([]string, string, error) {
	binds := make([]string, 0, len(resources))
	for _, resource := range resources {
		if !validResourceMount(resource.mount) {
			return nil, "runtime_create_failed", fmt.Errorf("invalid Resource mount %q", resource.mount)
		}
		tree, err := r.ensureResourceVersion(ctx, resource.address)
		if err != nil {
			return nil, "resource_fetch_failed", err
		}
		binds = append(binds, tree+":"+resourceMountRoot+"/"+resource.mount+":ro")
	}
	return binds, "", nil
}
