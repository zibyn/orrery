package resourcefs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"syscall"

	"github.com/google/uuid"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/zibyn/orrery/internal/generation"
)

func validVolume(volume string) bool {
	id, err := uuid.Parse(volume)
	return err == nil && id.String() == volume
}

// volumeDir holds a Volume's mount (mnt) and local state (runtime.md §17).
func (d *daemon) volumeDir(volume string) string {
	return filepath.Join(d.cfg.DataRoot, "volumes", volume)
}

// volumeCommand runs a Volume op; called with d.mu held, it returns with
// it held, but releases it around slow work.
func (d *daemon) volumeCommand(req request, resp *response) error {
	if !validVolume(req.Volume) {
		return fmt.Errorf("invalid Volume %q", req.Volume)
	}
	mt, mounted := d.volumes[req.Volume]
	switch req.Op {
	case "mount-volume":
		if mounted {
			if mt.volume.keeps(req.Generation) {
				return nil // an Agent restart or a re-sent Create: keep the state
			}
			// Left by a Sandbox that never ran here; the head has moved on.
			delete(d.volumes, req.Volume)
			detach(mt.dir)
		}
		d.mu.Unlock()
		m, err := d.loadGeneration(req.Generation)
		d.mu.Lock()
		if err != nil {
			return err
		}
		if d.handedOver {
			return ErrHandedOver
		}
		if _, ok := d.volumes[req.Volume]; ok {
			return nil
		}
		return d.mountVolume(req.Volume, req.Generation, m)
	case "unmount-volume":
		if !mounted {
			return nil
		}
		delete(d.volumes, req.Volume)
		if err := syscall.Unmount(mt.dir, syscall.MNT_DETACH); err != nil && !errors.Is(err, syscall.EINVAL) {
			return fmt.Errorf("unmount Volume %s: %w", req.Volume, err)
		}
		return os.RemoveAll(d.volumeDir(req.Volume))
	}
	if !mounted {
		return fmt.Errorf("Volume %s is not mounted", req.Volume)
	}
	// A seal hashes the dirty set; other commands need not wait for it. A
	// handover takes the Volume's own lock, so it never splits one.
	d.mu.Unlock()
	defer d.mu.Lock()
	switch req.Op {
	case "seal-volume":
		s, err := mt.volume.seal()
		resp.Sealed = &s
		return err
	case "upload-volume":
		return mt.volume.setUpload(req.Upload, req.Seq)
	default: // committed-volume
		return mt.volume.committed(req.Generation, req.Seq)
	}
}

func (d *daemon) mountVolume(volume, head string, m generation.Manifest) error {
	dir := d.volumeDir(volume)
	mnt := volumeMountDir(dir)
	detach(mnt)
	fs, err := newVolumeFS(volume, dir, head, m, d.store, d.cfg.Log)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(mnt, 0o755); err != nil {
		return err
	}
	srv, err := fuse.NewServer(fs, mnt, volumeMountOptions(volume))
	if err != nil {
		return fmt.Errorf("mount Volume %s: %w", volume, err)
	}
	mt := &mount{address: volume, dir: mnt, srv: srv, volume: fs}
	d.serve(mt)
	if err := srv.WaitMount(); err != nil {
		delete(d.volumes, volume)
		detach(mnt)
		return err
	}
	return nil
}

// loadGeneration reads a Volume's head manifest through a presigned GET; no
// head is an empty tree.
func (d *daemon) loadGeneration(id string) (generation.Manifest, error) {
	if id == "" {
		return generation.Manifest{Kind: generation.KindTree}, nil
	}
	gp, ok := d.cfg.Presigner.(GenerationPresigner)
	if !ok {
		return generation.Manifest{}, errors.New("the presigner cannot sign Generations")
	}
	ctx, cancel := context.WithTimeout(context.Background(), getTimeout)
	defer cancel()
	urls, err := gp.PresignGenerations(ctx, []string{id})
	if err != nil {
		return generation.Manifest{}, fmt.Errorf("presign Generation %s: %w", id, err)
	}
	if len(urls) != 1 {
		return generation.Manifest{}, fmt.Errorf("presign returned %d Generation URLs", len(urls))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urls[0], nil)
	if err != nil {
		return generation.Manifest{}, err
	}
	resp, err := d.store.http.Do(req)
	if err != nil {
		return generation.Manifest{}, err
	}
	defer resp.Body.Close() //nolint:errcheck // read-only
	if resp.StatusCode != http.StatusOK {
		return generation.Manifest{}, fmt.Errorf("fetch Generation %s: %s", id, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestBytes+1))
	if err != nil {
		return generation.Manifest{}, err
	}
	m, err := generation.Decode(data)
	if err != nil {
		return generation.Manifest{}, err
	}
	if m.Kind != generation.KindTree {
		return generation.Manifest{}, fmt.Errorf("Generation %s is a %s, not a Volume tree", id, m.Kind)
	}
	return m, nil
}
