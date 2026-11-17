package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	nodev1 "github.com/zibyn/orrery/gen/node/v1"
	"github.com/zibyn/orrery/internal/generation"
	"github.com/zibyn/orrery/internal/manifest"
	"github.com/zibyn/orrery/internal/resourcefs"
)

// volumeCommitInterval is how often a dirty Volume is committed while its
// Sandbox runs (runtime.md §17). A var so tests are fast.
var volumeCommitInterval = 60 * time.Second

// volumeMounter is orrery-resourcefs's writable side as the Agent drives
// it; tests fake it.
type volumeMounter interface {
	MountVolume(ctx context.Context, volume, head string) error
	UnmountVolume(ctx context.Context, volume string) error
	SealVolume(ctx context.Context, volume string) (resourcefs.Sealed, error)
	SetVolumeUpload(ctx context.Context, volume, upload string, seq uint64) error
	VolumeCommitted(ctx context.Context, volume, generation string, seq uint64) error
}

// volumeMount is one CreateSandbox volumes entry.
type volumeMount struct {
	ID   string `json:"id"`
	Path string `json:"path"`
	Name string `json:"name"`
	head string
}

func volumeMounts(in []*nodev1.VolumeMount) []volumeMount {
	out := make([]volumeMount, 0, len(in))
	for _, v := range in {
		out = append(out, volumeMount{ID: v.GetVolumeId(), Path: v.GetPath(), Name: v.GetName(), head: v.GetHeadGenerationId()})
	}
	return out
}

// volumeDaemonPath is the daemon's mount of a Volume (runtime.md §17).
func volumeDaemonPath(dataRoot, volume string) string {
	return filepath.Join(dataRoot, "volumes", volume, "mnt")
}

func volumeObjectsPath(dataRoot, volume string) string {
	return filepath.Join(dataRoot, "volumes", volume, "objects")
}

// sandboxVolumesPath holds one symlink per Volume name: a Local Sandbox's
// ORRERY_VOLUMES (runtime.md §10).
func sandboxVolumesPath(dataRoot string, id uuid.UUID) string {
	return filepath.Join(sandboxPath(dataRoot, id), "volumes")
}

// volumeRecordPath lists the Sandbox's Volumes, so a stop after an Agent
// restart still commits and releases them.
func volumeRecordPath(dataRoot string, id uuid.UUID) string {
	return filepath.Join(sandboxPath(dataRoot, id), "volumes.json")
}

func readVolumeRecord(dataRoot string, id uuid.UUID) []volumeMount {
	data, err := os.ReadFile(volumeRecordPath(dataRoot, id))
	if err != nil {
		return nil
	}
	var volumes []volumeMount
	if err := json.Unmarshal(data, &volumes); err != nil {
		slog.Warn("unreadable Volume record", "sandbox_id", id, "error", err)
	}
	return volumes
}

// mountVolumes has the daemon mount each Volume writable and records them.
// A Volume already mounted (an Agent restart, a re-sent Create) keeps its
// uncommitted state.
func (r *Runtime) mountVolumes(ctx context.Context, id uuid.UUID, volumes []volumeMount) (string, error) {
	if len(volumes) == 0 {
		return "", nil
	}
	if r.volumes == nil {
		return "resource_fetch_failed", errors.New("orrery-resourcefs is not configured")
	}
	for _, v := range volumes {
		if _, err := uuid.Parse(v.ID); err != nil || !validResourceMount(v.Name) {
			return "runtime_create_failed", fmt.Errorf("invalid Volume %q (%q)", v.ID, v.Name)
		}
	}
	data, err := json.Marshal(volumes)
	if err != nil {
		return "runtime_create_failed", err
	}
	if err := os.MkdirAll(sandboxPath(r.dataRoot, id), 0o755); err != nil {
		return "runtime_create_failed", err
	}
	if err := os.WriteFile(volumeRecordPath(r.dataRoot, id), data, 0o644); err != nil {
		return "runtime_create_failed", err
	}
	links := sandboxVolumesPath(r.dataRoot, id)
	if err := os.RemoveAll(links); err != nil {
		return "runtime_create_failed", err
	}
	if err := os.MkdirAll(links, 0o755); err != nil {
		return "runtime_create_failed", err
	}
	for _, v := range volumes {
		err := retryResourceFetch(ctx, resourceFetchBackoff, func() error {
			return r.volumes.MountVolume(ctx, v.ID, v.head)
		})
		if err != nil {
			return "resource_fetch_failed", fmt.Errorf("mount Volume %s: %w", v.ID, err)
		}
		if err := os.Symlink(volumeDaemonPath(r.dataRoot, v.ID), filepath.Join(links, v.Name)); err != nil {
			return "runtime_create_failed", err
		}
	}
	return "", nil
}

// volumeBindMounts bind-mounts each Volume read-write at its path.
func (r *Runtime) volumeBindMounts(volumes []volumeMount) []specs.Mount {
	mounts := make([]specs.Mount, 0, len(volumes))
	for _, v := range volumes {
		mounts = append(mounts, bindMount(volumeDaemonPath(r.dataRoot, v.ID), v.Path, "rw"))
	}
	return mounts
}

// volumeLoops are the running Sandboxes' periodic Volume commits.
type volumeLoops struct {
	mu    sync.Mutex
	loops map[string]volumeLoop
}

type volumeLoop struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// startVolumeCommits commits the Sandbox's dirty Volumes every
// volumeCommitInterval until stopVolumeCommits.
func (r *Runtime) startVolumeCommits(id uuid.UUID, generation int64) {
	volumes := readVolumeRecord(r.dataRoot, id)
	if len(volumes) == 0 {
		return
	}
	r.stopVolumeCommits(id)
	ctx, cancel := context.WithCancel(context.Background())
	loop := volumeLoop{cancel: cancel, done: make(chan struct{})}
	r.volumeLoops.mu.Lock()
	if r.volumeLoops.loops == nil {
		r.volumeLoops.loops = map[string]volumeLoop{}
	}
	r.volumeLoops.loops[id.String()] = loop
	r.volumeLoops.mu.Unlock()
	go func() {
		defer close(loop.done)
		ticker := time.NewTicker(volumeCommitInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			for _, v := range volumes {
				if err := r.commitVolumeOnce(ctx, generation, v.ID); err != nil && ctx.Err() == nil {
					slog.Warn("periodic Volume commit", "sandbox_id", id, "volume_id", v.ID, "error", err)
				}
			}
		}
	}()
}

// stopVolumeCommits ends the periodic loop and waits for a commit in hand.
func (r *Runtime) stopVolumeCommits(id uuid.UUID) {
	r.volumeLoops.mu.Lock()
	loop, ok := r.volumeLoops.loops[id.String()]
	delete(r.volumeLoops.loops, id.String())
	r.volumeLoops.mu.Unlock()
	if ok {
		loop.cancel()
		<-loop.done
	}
}

// resumeVolumeCommits restarts the loops of Sandboxes adopted after an
// Agent restart.
func (r *Runtime) resumeVolumeCommits() {
	for id, generation := range r.managedSnapshot() {
		if sandboxID, err := uuid.Parse(id); err == nil {
			r.startVolumeCommits(sandboxID, generation)
		}
	}
}

// commitVolumes commits each of a stopped Sandbox's Volumes, retrying, and
// unmounts it once committed (or once no Sandbox may commit it). A Volume
// that cannot be committed stays mounted with its writes; the Sandbox ends
// commit_failed.
func (r *Runtime) commitVolumes(ctx context.Context, id uuid.UUID, generation int64, retrying func()) error {
	volumes := readVolumeRecord(r.dataRoot, id)
	for _, v := range volumes {
		err := retryCommit(ctx, id, func() error { return r.commitVolumeOnce(ctx, generation, v.ID) }, retrying)
		if err != nil {
			return err
		}
		if err := r.volumes.UnmountVolume(ctx, v.ID); err != nil {
			return fmt.Errorf("unmount Volume %s: %w", v.ID, err)
		}
	}
	if err := os.RemoveAll(sandboxVolumesPath(r.dataRoot, id)); err != nil {
		return err
	}
	return os.RemoveAll(volumeRecordPath(r.dataRoot, id))
}

// commitVolumeOnce is node-protocol.md "Generation 提交" for a Volume: the
// daemon seals its dirty files into objects, the Agent uploads what the
// store lacks, and the Control Plane advances the Volume's head under the
// holder's fence. A Volume unchanged since its last commit sends nothing.
func (r *Runtime) commitVolumeOnce(ctx context.Context, gen int64, volume string) error {
	if r.client == nil {
		return errors.New("the Node is not connected")
	}
	s, err := r.volumes.SealVolume(ctx, volume)
	if err != nil {
		return err
	}
	if s.Upload != "" {
		// A commit whose reply was lost: retry it before building on it.
		if err := r.finishVolumeCommit(ctx, volume, s.Upload, s.UploadSeq); err != nil && !errors.Is(err, errUploadRefused) {
			return err
		}
		if s, err = r.volumes.SealVolume(ctx, volume); err != nil {
			return err
		}
	}
	if !s.Dirty || s.Manifest == nil {
		return nil
	}
	encoded, err := generation.Encode(*s.Manifest)
	if err != nil {
		return fmt.Errorf("%w: %w", errCommitFailed, err)
	}
	pending := map[string]bool{}
	for _, d := range s.Pending {
		pending[d] = true
	}
	var changed []generation.Entry
	for _, e := range s.Manifest.Entries {
		if e.Type != manifest.TypeFile {
			continue
		}
		hit := pending[e.SHA256]
		for _, b := range e.Blocks {
			hit = hit || pending[b]
		}
		if hit {
			changed = append(changed, e)
		}
	}
	ack, err := r.streamGeneration(ctx, &nodev1.GenerationChunk{VolumeId: volume, Generation: gen, BaseGenerationId: s.Base}, changed)
	if code := status.Code(err); code == codes.FailedPrecondition {
		return fmt.Errorf("%w: %w", errHolderRefused, err)
	} else if code == codes.InvalidArgument {
		return fmt.Errorf("%w: %w", errCommitFailed, err)
	} else if err != nil {
		return err
	}
	root, err := os.OpenRoot(volumeObjectsPath(r.dataRoot, volume))
	if err != nil {
		return err
	}
	defer root.Close() //nolint:errcheck // read-only handle
	sources := map[string]manifest.Source{}
	for _, d := range s.Pending {
		if info, err := root.Stat(d); err == nil {
			sources[d] = manifest.Source{Path: d, Size: info.Size()}
		}
	}
	if err := r.uploadPublishObjects(ctx, root, ack.GetMissingSha256(), sources); err != nil {
		return err
	}
	if err := r.putGenerationManifest(ctx, ack.GetUploadId(), encoded); err != nil {
		return err
	}
	if err := r.volumes.SetVolumeUpload(ctx, volume, ack.GetUploadId(), s.Seq); err != nil {
		return err
	}
	return r.finishVolumeCommit(ctx, volume, ack.GetUploadId(), s.Seq)
}

// errUploadRefused is a CommitGeneration the Control Plane refused (objects
// missing, head moved). The upload is forgotten and the next attempt submits
// afresh; only a refused Submit means no Sandbox may commit the Volume.
var errUploadRefused = errors.New("the Control Plane refused the upload")

// finishVolumeCommit sends CommitGeneration and tells the daemon its new
// base; a refused upload is forgotten, so the next attempt seals afresh.
func (r *Runtime) finishVolumeCommit(ctx context.Context, volume, upload string, seq uint64) error {
	_, err := r.client.CommitGeneration(ctx, &nodev1.CommitGenerationRequest{UploadId: upload})
	switch status.Code(err) {
	case codes.OK:
		return r.volumes.VolumeCommitted(ctx, volume, upload, seq)
	case codes.FailedPrecondition, codes.InvalidArgument:
		if err := r.volumes.VolumeCommitted(ctx, volume, "", 0); err != nil {
			return err
		}
		return fmt.Errorf("%w: %w", errUploadRefused, err)
	}
	return err
}
