package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/errdefs"
	"github.com/google/uuid"
)

// evictionInterval is how often the Node re-checks the disk watermark. It is
// short enough that a Node that crosses the watermark recovers without waiting
// for a new Sandbox, and light enough to be a no-op when space is fine.
const evictionInterval = 15 * time.Second

// evictionKind is one of the three caches the disk budget covers
// (runtime.md §11): version trees, local images and BuildKit cache.
type evictionKind int

const (
	evictTree evictionKind = iota
	evictImage
	evictBuildCache
)

// evictionCandidate is one reclaimable item. lastUsed is the LRU clock that
// orders all three classes together.
type evictionCandidate struct {
	kind     evictionKind
	key      string // tree path, image id, or "build-cache"
	size     int64  // bytes, when known
	lastUsed time.Time
}

// sortEvictionCandidates puts the least recently used first. The sort is
// stable so equal clocks keep a deterministic class order.
func sortEvictionCandidates(candidates []evictionCandidate) {
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].lastUsed.Before(candidates[j].lastUsed)
	})
}

// sandboxPins records what a Sandbox pins on this Node. It lives beside the
// Sandbox's data-root record (not in memory) for two reasons:
//
//   - A stopped Sandbox still references its version trees and image, and a
//     stopped Sandbox survives an Agent restart.
//   - The Agent has no "Sandbox deleted" command: StopSandbox is sent for both
//     stop and delete. Dropping the record on stop would let the next sweep
//     evict a stopped Sandbox's cache. So the record is retained (it is
//     overwritten by the next CreateSandbox for the same id) and can only be
//     dropped when the Sandbox itself is known to be gone. That leaves stale
//     records for deleted Sandboxes, which silently over-retain cache; eviction
//     is a cache policy, so the failure direction is safe.
type sandboxPins struct {
	ImageDigest string   `json:"image_digest"`
	Resources   []string `json:"resources,omitempty"`
}

func pinsPath(sandboxDir string) string { return filepath.Join(sandboxDir, "pinned.json") }

// writeSandboxPins is atomic: a torn record would under-pin a live Sandbox.
func writeSandboxPins(sandboxDir string, pins sandboxPins) error {
	raw, err := json.Marshal(pins)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(sandboxDir, 0o755); err != nil {
		return err
	}
	tmp := pinsPath(sandboxDir) + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, pinsPath(sandboxDir))
}

func readSandboxPins(sandboxDir string) (sandboxPins, bool) {
	raw, err := os.ReadFile(pinsPath(sandboxDir))
	if err != nil {
		return sandboxPins{}, false
	}
	var pins sandboxPins
	if json.Unmarshal(raw, &pins) != nil {
		return sandboxPins{}, false
	}
	return pins, true
}

// loadNodePins unions every Sandbox record into the address and image-digest
// sets the eviction candidates are filtered against.
func loadNodePins(dataRoot string) (trees, images map[string]bool) {
	trees, images = map[string]bool{}, map[string]bool{}
	entries, err := os.ReadDir(filepath.Join(dataRoot, "sandboxes"))
	if err != nil {
		return trees, images
	}
	for _, entry := range entries {
		if _, err := uuid.Parse(entry.Name()); err != nil {
			continue
		}
		pins, ok := readSandboxPins(filepath.Join(dataRoot, "sandboxes", entry.Name()))
		if !ok {
			continue
		}
		for _, address := range pins.Resources {
			trees[address] = true
		}
		if pins.ImageDigest != "" {
			images[imageDigestKey(pins.ImageDigest)] = true
		}
	}
	return trees, images
}

// imageDigestKey reduces a reference or image id to the digest the Docker API
// reports in RepoDigests.
func imageDigestKey(ref string) string {
	if i := strings.LastIndex(ref, "@"); i >= 0 {
		return ref[i+1:]
	}
	return ref
}

// imageUsagePath is the Node's persisted last-used clock for local images.
// Docker's Created timestamp is an image's build time, not its use time, so
// LRU eviction needs a touch record of its own (runtime.md §11).
func imageUsagePath(dataRoot string) string {
	return filepath.Join(dataRoot, "cache", "image-usage.json")
}

// imageUsageMu serializes the read-modify-write of the usage file.
var imageUsageMu sync.Mutex

// touchImageUsage records that a Sandbox on this Node uses the image. It is
// best effort: a failed write only weakens eviction order, so it must never
// fail a Sandbox.
func touchImageUsage(dataRoot, ref string) {
	key := imageDigestKey(ref)
	if key == "" {
		return
	}
	imageUsageMu.Lock()
	defer imageUsageMu.Unlock()
	usage := loadImageUsage(dataRoot)
	usage[key] = time.Now().UTC()
	raw, err := json.Marshal(usage)
	if err != nil {
		return
	}
	path := imageUsagePath(dataRoot)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

// loadImageUsage reads the touch record; a missing or torn file means "no
// known use", and eviction falls back to Created.
func loadImageUsage(dataRoot string) map[string]time.Time {
	raw, err := os.ReadFile(imageUsagePath(dataRoot))
	if err != nil {
		return map[string]time.Time{}
	}
	usage := map[string]time.Time{}
	if json.Unmarshal(raw, &usage) != nil {
		return map[string]time.Time{}
	}
	return usage
}

// imageLastUsed is the LRU clock for an image: the latest touch of any of its
// references, else its Created time.
func imageLastUsed(usage map[string]time.Time, image cachedImage) time.Time {
	var latest time.Time
	for _, key := range append([]string{imageDigestKey(image.ID)}, image.RepoDigests...) {
		if used := usage[imageDigestKey(key)]; used.After(latest) {
			latest = used
		}
	}
	if latest.IsZero() {
		return time.Unix(image.Created, 0)
	}
	return latest
}

func imagePinned(pins map[string]bool, image cachedImage) bool {
	if pins[imageDigestKey(image.ID)] {
		return true
	}
	for _, digest := range image.RepoDigests {
		if pins[imageDigestKey(digest)] {
			return true
		}
	}
	return false
}

// evictLoop reclaims cache while the disk is below the watermark. It runs
// beside the Control stream and never reports failures: eviction must not fail
// a Sandbox.
func (r *Runtime) evictLoop(ctx context.Context, watermark float64) {
	ticker := time.NewTicker(evictionInterval)
	defer ticker.Stop()
	for {
		r.evictOnce(ctx, watermark)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// evictOnce runs one LRU pass. Trees and images are only candidates when no
// Sandbox record pins them; BuildKit cache is pruned last because the daemon
// exposes it as one all-or-nothing operation.
func (r *Runtime) evictOnce(ctx context.Context, watermark float64) {
	if watermark <= 0 || !belowWatermark("/", watermark) {
		return
	}
	treePins, imagePins := loadNodePins(r.dataRoot)
	candidates := r.evictionCandidates(ctx, treePins, imagePins)
	sortEvictionCandidates(candidates)
	cache := newResourceCache(r.dataRoot)
	for _, candidate := range candidates {
		// The heartbeat and admission use the root filesystem's watermark
		// (runtime.md §11); eviction reacts to the same number.
		if !belowWatermark("/", watermark) {
			return
		}
		switch candidate.kind {
		case evictTree:
			// A tree may have been pinned by a CreateSandbox that raced the
			// candidate scan; re-check before deleting it.
			if pins, _ := loadNodePins(r.dataRoot); pins[filepath.Base(candidate.key)] {
				continue
			}
			// Tree assembly and object reclamation share this lock, so an
			// object downloaded for a tree cannot be reclaimed between the
			// download and its hard link.
			resourceTreeMu.Lock()
			err := removeTree(candidate.key)
			if err == nil {
				cache.reclaimObjects()
			}
			resourceTreeMu.Unlock()
			if err != nil {
				slog.Warn("evict Resource version tree", "path", candidate.key, "error", err)
			}
		case evictImage:
			if err := r.driver.(imageStore).removeImage(ctx, candidate.key); err != nil {
				slog.Debug("evict local image", "id", candidate.key, "error", err)
			}
		case evictBuildCache:
			if err := r.pruneBuildCache(ctx); err != nil {
				slog.Debug("prune BuildKit cache", "error", err)
			}
		}
	}
}

// evictionCandidates lists what may be reclaimed. Images are restricted to the
// platform registry this Node is credentialed for (ADR-0021: Nodes pull only
// from it), so a shared Docker daemon's unrelated images are not the Node's to
// evict.
func (r *Runtime) evictionCandidates(ctx context.Context, treePins, imagePins map[string]bool) []evictionCandidate {
	cache := newResourceCache(r.dataRoot)
	var candidates []evictionCandidate
	if entries, err := os.ReadDir(filepath.Join(cache.root, "versions")); err == nil {
		for _, entry := range entries {
			name := entry.Name()
			if !entry.IsDir() || strings.HasPrefix(name, ".tmp-") || treePins[name] {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				continue
			}
			path := filepath.Join(cache.root, "versions", name)
			candidates = append(candidates, evictionCandidate{
				kind: evictTree, key: path, size: treeSize(path), lastUsed: info.ModTime(),
			})
		}
	}
	if store, ok := r.driver.(imageStore); ok {
		images, _ := store.listImages(ctx) // an unreachable store has no candidates
		usage := loadImageUsage(r.dataRoot)
		for _, image := range images {
			if imagePinned(imagePins, image) || !r.managesImage(image) {
				continue
			}
			candidates = append(candidates, evictionCandidate{
				kind: evictImage, key: image.ID, size: image.Size, lastUsed: imageLastUsed(usage, image),
			})
		}
	}
	// BuildKit cache has no per-entry order through its prune API; the daemon
	// prunes every unused record at once, so it is the candidate of last
	// resort. Its clause is only reached when trees and images did not bring
	// the free space back above the watermark.
	candidates = append(candidates, evictionCandidate{
		kind: evictBuildCache, key: "build-cache", lastUsed: time.Now(),
	})
	return candidates
}

func (r *Runtime) managesImage(image cachedImage) bool {
	r.registryMu.Lock()
	defer r.registryMu.Unlock()
	if r.registry == nil {
		return false
	}
	refs := append(append([]string{}, image.RepoTags...), image.RepoDigests...)
	for _, ref := range refs {
		if _, ok := r.registry.Get(registryHost(ref)); ok {
			return true
		}
	}
	return false
}

func belowWatermark(path string, watermark float64) bool {
	free, total := diskUsage(path)
	if total == 0 {
		return false
	}
	return float64(free)/float64(total) < watermark
}

// treeSize is the tree's apparent size. Hard-linked objects are counted once
// per tree, which over-estimates a shared object; LRU order, not an exact
// budget, is what the watermark decision needs.
func treeSize(path string) int64 {
	var size int64
	_ = filepath.WalkDir(path, func(_ string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		if info, err := entry.Info(); err == nil {
			size += info.Size()
		}
		return nil
	})
	return size
}

// removeTree removes a read-only version tree. Directories are 0555
// (runtime.md §11) and RemoveAll needs write permission on each directory, so
// make them writable deepest-first first.
func removeTree(path string) error {
	var dirs []string
	_ = filepath.WalkDir(path, func(p string, entry os.DirEntry, err error) error {
		if err == nil && entry.IsDir() {
			dirs = append(dirs, p)
		}
		return nil
	})
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
	for _, dir := range dirs {
		_ = os.Chmod(dir, 0o755)
	}
	return os.RemoveAll(path)
}

// reclaimObjects removes cached objects no version tree hard-links any more.
// A tree link raises the inode's link count, so count 1 means only the object
// directory itself still names the file (runtime.md §11).
func (c *resourceCache) reclaimObjects() {
	objects := filepath.Join(c.root, "objects")
	entries, err := os.ReadDir(objects)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(objects, entry.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		remaining := 0
		for _, file := range files {
			path := filepath.Join(dir, file.Name())
			info, err := file.Info()
			if err != nil {
				remaining++
				continue
			}
			if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Nlink > 1 {
				remaining++
				continue
			}
			if err := os.Remove(path); err != nil {
				remaining++
			}
		}
		if remaining == 0 {
			_ = os.Remove(dir)
		}
	}
}

// imageStore is the Provider's local image store the disk budget covers
// (runtime.md §11). Local Nodes have none.
type imageStore interface {
	listImages(ctx context.Context) ([]cachedImage, error)
	removeImage(ctx context.Context, id string) error
}

// cachedImage is what eviction needs of a local image: the Docker
// GET /images/json subset, or a containerd image record (ID is then its name,
// the key the record is removed by).
type cachedImage struct {
	ID          string   `json:"Id"`
	RepoTags    []string `json:"RepoTags"`
	RepoDigests []string `json:"RepoDigests"`
	Created     int64    `json:"Created"`
	Size        int64    `json:"Size"`
}

func (d *dockerClient) listImages(ctx context.Context) ([]cachedImage, error) {
	var images []cachedImage
	if err := d.do(ctx, http.MethodGet, d.api("/images/json"), nil, &images); err != nil {
		return nil, err
	}
	return images, nil
}

func (d *dockerClient) removeImage(ctx context.Context, id string) error {
	resp, err := d.request(ctx, http.MethodDelete,
		d.api("/images/"+url.PathEscape(id))+"?force=true&noprune=false", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body) //nolint:errcheck // drain for reuse
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode < 300 {
		return nil
	}
	return fmt.Errorf("remove image: %s", resp.Status)
}

func (d containerDriver) listImages(ctx context.Context) ([]cachedImage, error) {
	if err := d.r.d.negotiate(ctx); err != nil {
		return nil, err
	}
	return d.r.d.listImages(ctx)
}

func (d containerDriver) removeImage(ctx context.Context, id string) error {
	return d.r.d.removeImage(ctx, id)
}

func (d *containerdDriver) listImages(ctx context.Context) ([]cachedImage, error) {
	records, err := d.client.ImageService().List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]cachedImage, 0, len(records))
	for _, record := range records {
		out = append(out, containerdCachedImage(record))
	}
	return out, nil
}

// containerdCachedImage keys a record by name and pins it by target digest:
// a Sandbox's image is pulled by digest, but the same content can carry a tag.
func containerdCachedImage(record images.Image) cachedImage {
	return cachedImage{
		ID:          record.Name,
		RepoTags:    []string{record.Name},
		RepoDigests: []string{record.Target.Digest.String()},
		Created:     record.CreatedAt.Unix(),
	}
}

// removeImage deletes the record synchronously, so containerd's garbage
// collector frees the unreferenced content and snapshots before it returns.
func (d *containerdDriver) removeImage(ctx context.Context, name string) error {
	err := d.client.ImageService().Delete(ctx, name, images.SynchronousDelete())
	if errdefs.IsNotFound(err) {
		return nil
	}
	return err
}

// pruneBuildCache prunes the unused records of the Node's BuildKit endpoint.
// A Docker Node without --buildkit prunes dockerd's built-in BuildKit through
// the Engine API.
func (r *Runtime) pruneBuildCache(ctx context.Context) error {
	if r.buildkit == "" && r.provider == ProviderDocker {
		if err := r.d.negotiate(ctx); err != nil {
			return err
		}
		_, err := r.d.pruneBuildCache(ctx)
		return err
	}
	c, err := r.buildkitClient(ctx)
	if err != nil {
		return err
	}
	defer c.Close() //nolint:errcheck // one-shot client
	return c.Prune(ctx, nil)
}

// pruneBuildCache runs `docker builder prune` through the Engine API and
// returns the reclaimed bytes.
func (d *dockerClient) pruneBuildCache(ctx context.Context) (int64, error) {
	var out struct {
		SpaceReclaimed int64 `json:"SpaceReclaimed"`
	}
	if err := d.do(ctx, http.MethodPost, d.api("/build/prune?all=false"), nil, &out); err != nil {
		return 0, err
	}
	return out.SpaceReclaimed, nil
}
