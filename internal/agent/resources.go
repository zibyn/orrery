package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	specs "github.com/opencontainers/runtime-spec/specs-go"

	nodev1 "github.com/zibyn/orrery/gen/node/v1"
	"github.com/zibyn/orrery/internal/resourcefs"
)

// resourceMountRoot is where every Resource version appears in a containerd
// Sandbox (runtime.md §2).
const resourceMountRoot = "/resources"

// resourceFetchBackoff is the Resource mount retry schedule (defaults.md:
// 1, 2, 4, 8, 16 seconds, the same group as Workspace provisioning). A var so
// tests can shorten it.
var resourceFetchBackoff = []time.Duration{
	time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second,
}

// resourceMounter is orrery-resourcefs as the Agent drives it; tests fake it.
type resourceMounter interface {
	Mount(ctx context.Context, address string) error
	Unmount(ctx context.Context, address string) error
	Status(ctx context.Context) ([]string, error)
	SelfTest(ctx context.Context) error
}

// resourceVersionPath is the daemon's mount of one version (runtime.md §11).
func resourceVersionPath(dataRoot, address string) string {
	return filepath.Join(dataRoot, "resources", address)
}

// blockCachePath is the daemon's block cache, which eviction shares.
func blockCachePath(dataRoot string) string { return filepath.Join(dataRoot, "cache", "blocks") }

// sandboxResourcesPath holds one symlink per <mount> to its version mount. On
// Local it is the Sandbox's ORRERY_RESOURCES (runtime.md §10); on every Node
// it is the record that rebuilds the reference counts after a restart.
func sandboxResourcesPath(dataRoot string, id uuid.UUID) string {
	return filepath.Join(sandboxPath(dataRoot, id), "resources")
}

// resourceRefs counts which Sandboxes (and builds) hold each version. A
// version is unmounted when its last holder stops. Per-address locks order a
// mount against an unmount of the same version without serializing the Node.
type resourceRefs struct {
	mu      sync.Mutex
	holders map[string][]string // holder id → addresses
	locks   map[string]*sync.Mutex
}

func (refs *resourceRefs) hold(id string, addresses []string) {
	refs.mu.Lock()
	defer refs.mu.Unlock()
	if refs.holders == nil {
		refs.holders = map[string][]string{}
	}
	refs.holders[id] = addresses
}

// release drops a holder and returns the addresses it held.
func (refs *resourceRefs) release(id string) []string {
	refs.mu.Lock()
	defer refs.mu.Unlock()
	addresses := refs.holders[id]
	delete(refs.holders, id)
	return addresses
}

func (refs *resourceRefs) used(address string) bool {
	refs.mu.Lock()
	defer refs.mu.Unlock()
	for _, addresses := range refs.holders {
		for _, held := range addresses {
			if held == address {
				return true
			}
		}
	}
	return false
}

func (refs *resourceRefs) lock(address string) func() {
	refs.mu.Lock()
	if refs.locks == nil {
		refs.locks = map[string]*sync.Mutex{}
	}
	l, ok := refs.locks[address]
	if !ok {
		l = &sync.Mutex{}
		refs.locks[address] = l
	}
	refs.mu.Unlock()
	l.Lock()
	return l.Unlock
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

// validResourceMount keeps the mount one path segment. The Control Plane
// validates this too; the Agent does not trust it blindly.
func validResourceMount(mount string) bool {
	return mount != "" && mount != "." && mount != ".." && !strings.ContainsAny(mount, "/\x00")
}

// mountResources has the daemon mount every version the Sandbox uses and
// records them as <sandbox>/resources/<mount> symlinks. Only the manifest is
// read now; objects are fetched when read (ADR-0030).
func (r *Runtime) mountResources(ctx context.Context, id uuid.UUID, resources []resourceMount) (string, error) {
	addresses := make([]string, 0, len(resources))
	for _, resource := range resources {
		if !validResourceMount(resource.mount) || !validAddress(resource.address) {
			return "runtime_create_failed", fmt.Errorf("invalid Resource mount %q@%q", resource.mount, resource.address)
		}
		addresses = append(addresses, resource.address)
	}
	if len(resources) > 0 && r.resourcefs == nil {
		return "resource_fetch_failed", errors.New("orrery-resourcefs is not configured")
	}
	r.resourceRefs.hold(id.String(), addresses)
	for _, address := range addresses {
		unlock := r.resourceRefs.lock(address)
		err := retryResourceFetch(ctx, resourceFetchBackoff, func() error {
			return r.resourcefs.Mount(ctx, address)
		})
		unlock()
		if err != nil {
			r.releaseResources(context.WithoutCancel(ctx), id)
			return "resource_fetch_failed", fmt.Errorf("mount Resource %s: %w", address, err)
		}
	}
	dir := sandboxResourcesPath(r.dataRoot, id)
	if err := os.RemoveAll(dir); err != nil {
		return "runtime_create_failed", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "runtime_create_failed", err
	}
	for _, resource := range resources {
		if err := os.Symlink(resourceVersionPath(r.dataRoot, resource.address), filepath.Join(dir, resource.mount)); err != nil {
			return "runtime_create_failed", err
		}
	}
	return "", nil
}

// releaseResources drops the Sandbox's hold, unmounts every version no other
// holder uses and removes the symlink directory.
func (r *Runtime) releaseResources(ctx context.Context, id uuid.UUID) {
	for _, address := range r.resourceRefs.release(id.String()) {
		r.unmountUnused(ctx, address)
	}
	if err := os.RemoveAll(sandboxResourcesPath(r.dataRoot, id)); err != nil {
		slog.Warn("remove Sandbox Resource links", "sandbox_id", id, "error", err)
	}
}

func (r *Runtime) unmountUnused(ctx context.Context, address string) {
	unlock := r.resourceRefs.lock(address)
	defer unlock()
	if r.resourceRefs.used(address) || r.resourcefs == nil {
		return
	}
	if err := r.resourcefs.Unmount(ctx, address); err != nil {
		slog.Warn("unmount Resource version", "address", address, "error", err)
	}
}

// rebuildResourceRefs restores the holds of the Sandboxes this Agent adopted
// after a restart from their symlink records, then unmounts what nothing
// holds any more. Versions already mounted are not mounted again.
func (r *Runtime) rebuildResourceRefs(ctx context.Context) {
	for id := range r.managedSnapshot() {
		sandboxID, err := uuid.Parse(id)
		if err != nil {
			continue
		}
		if addresses := readResourceLinks(r.dataRoot, sandboxID); len(addresses) > 0 {
			r.resourceRefs.hold(id, addresses)
		}
	}
	if r.resourcefs == nil {
		return
	}
	mounted, err := r.resourcefs.Status(ctx)
	if err != nil {
		slog.Warn("list Resource mounts", "error", err)
		return
	}
	for _, address := range mounted {
		r.unmountUnused(ctx, address)
	}
}

func readResourceLinks(dataRoot string, id uuid.UUID) []string {
	entries, err := os.ReadDir(sandboxResourcesPath(dataRoot, id))
	if err != nil {
		return nil
	}
	var addresses []string
	for _, entry := range entries {
		target, err := os.Readlink(filepath.Join(sandboxResourcesPath(dataRoot, id), entry.Name()))
		if err == nil && validAddress(filepath.Base(target)) {
			addresses = append(addresses, filepath.Base(target))
		}
	}
	return addresses
}

func validAddress(address string) bool {
	return len(address) == 64 && strings.Trim(address, "0123456789abcdef") == ""
}

// resourceBindMounts bind-mounts each version read-only at /resources/<mount>.
func (r *Runtime) resourceBindMounts(resources []resourceMount) []specs.Mount {
	mounts := make([]specs.Mount, 0, len(resources))
	for _, resource := range resources {
		mounts = append(mounts, bindMount(resourceVersionPath(r.dataRoot, resource.address), resourceMountRoot+"/"+resource.mount, "ro"))
	}
	return mounts
}

// servePresign answers orrery-resourcefs: its batches become Presign RPCs
// (node-protocol.md), so the daemon never holds object-store credentials.
func (r *Runtime) servePresign(ctx context.Context, socket string) {
	l, err := resourcefs.ListenPresign(socket)
	if err != nil {
		slog.Error("listen for resourcefs presign requests", "socket", socket, "error", err)
		return
	}
	resourcefs.ServePresign(ctx, l, presignFunc(func(ctx context.Context, objects, manifests []string) ([]string, error) {
		resp, err := r.presign(ctx, &nodev1.PresignRequest{Sha256: objects, Manifests: manifests, Method: "GET"})
		if err != nil {
			return nil, err
		}
		return resp.GetUrls(), nil
	}), slog.Default())
}

type presignFunc func(ctx context.Context, objects, manifests []string) ([]string, error)

func (f presignFunc) Presign(ctx context.Context, objects, manifests []string) ([]string, error) {
	return f(ctx, objects, manifests)
}

// removeLegacyResourceCache deletes the eager path's object cache and version
// trees (before ADR-0030). Tree directories are 0555, so they are made
// writable first.
func removeLegacyResourceCache(dataRoot string) {
	for _, dir := range []string{"objects", "versions"} {
		path := filepath.Join(dataRoot, "cache", dir)
		_ = filepath.WalkDir(path, func(p string, entry fs.DirEntry, err error) error {
			if err == nil && entry.IsDir() {
				_ = os.Chmod(p, 0o755)
			}
			return nil
		})
		if err := os.RemoveAll(path); err != nil {
			slog.Warn("remove the pre-ADR-0030 Resource cache", "path", path, "error", err)
		}
	}
}
