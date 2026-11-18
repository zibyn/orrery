package resourcefs

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"time"
)

// Default socket paths. The control socket is the daemon's: the Agent
// mounts, unmounts and self-checks through it, and a successor process takes
// over through it. The presign socket is the Agent's.
const (
	DefaultControlSocket = "/run/orrery/resourcefs/control.sock"
	DefaultPresignSocket = "/run/orrery/resourcefs/presign.sock"
)

// ErrHandedOver answers a command that reached a process that has handed its
// mounts to a successor; the caller retries and reaches the successor.
var ErrHandedOver = errors.New("resourcefs handed over to a new process; retry")

// request is one control command, one JSON line per connection.
type request struct {
	// mount, unmount, status, selftest, takeover; mount-volume,
	// unmount-volume, seal-volume, upload-volume, committed-volume
	Op      string `json:"op"`
	Address string `json:"address,omitempty"`
	Volume  string `json:"volume,omitempty"`
	// Generation is the head to mount, or the one just committed.
	Generation string `json:"generation,omitempty"`
	Upload     string `json:"upload,omitempty"`
	Seq        uint64 `json:"seq,omitempty"`
}

type response struct {
	Error   string   `json:"error,omitempty"`
	Mounts  []string `json:"mounts,omitempty"`
	Volumes []string `json:"volumes,omitempty"`
	Sealed  *Sealed  `json:"sealed,omitempty"`
}

// Client is the Agent's handle on the daemon.
type Client struct {
	Socket string
}

// Mount mounts a version at <data-root>/resources/<address>; mounting one
// already mounted succeeds.
func (c Client) Mount(ctx context.Context, address string) error {
	_, err := c.call(ctx, request{Op: "mount", Address: address})
	return err
}

// Unmount detaches the version's mount.
func (c Client) Unmount(ctx context.Context, address string) error {
	_, err := c.call(ctx, request{Op: "unmount", Address: address})
	return err
}

// Status lists the mounted addresses.
func (c Client) Status(ctx context.Context) ([]string, error) {
	return c.call(ctx, request{Op: "status"})
}

// SelfTest mounts a built-in one-file version, reads it back and unmounts it.
func (c Client) SelfTest(ctx context.Context) error {
	_, err := c.call(ctx, request{Op: "selftest"})
	return err
}

// MountVolume mounts a Volume writable at <data-root>/volumes/<id>/mnt from
// its head Generation (empty when none); mounting one already mounted
// succeeds and keeps its uncommitted state.
func (c Client) MountVolume(ctx context.Context, volume, head string) error {
	_, err := c.do(ctx, request{Op: "mount-volume", Volume: volume, Generation: head})
	return err
}

// UnmountVolume detaches a Volume and drops its local state.
func (c Client) UnmountVolume(ctx context.Context, volume string) error {
	_, err := c.do(ctx, request{Op: "unmount-volume", Volume: volume})
	return err
}

// Volumes lists the mounted Volume ids.
func (c Client) Volumes(ctx context.Context) ([]string, error) {
	resp, err := c.do(ctx, request{Op: "status"})
	return resp.Volumes, err
}

// SealVolume returns what a commit of the Volume needs (runtime.md §17).
func (c Client) SealVolume(ctx context.Context, volume string) (Sealed, error) {
	resp, err := c.do(ctx, request{Op: "seal-volume", Volume: volume})
	if err != nil || resp.Sealed == nil {
		return Sealed{}, err
	}
	return *resp.Sealed, nil
}

// SetVolumeUpload records a CommitGeneration about to be sent for the seal
// at seq, so a lost reply is retried rather than rebuilt.
func (c Client) SetVolumeUpload(ctx context.Context, volume, upload string, seq uint64) error {
	_, err := c.do(ctx, request{Op: "upload-volume", Volume: volume, Upload: upload, Seq: seq})
	return err
}

// VolumeCommitted makes generation the Volume's base for the seal at seq;
// an empty generation only forgets a refused upload.
func (c Client) VolumeCommitted(ctx context.Context, volume, generation string, seq uint64) error {
	_, err := c.do(ctx, request{Op: "committed-volume", Volume: volume, Generation: generation, Seq: seq})
	return err
}

func (c Client) call(ctx context.Context, req request) ([]string, error) {
	resp, err := c.do(ctx, req)
	return resp.Mounts, err
}

func (c Client) do(ctx context.Context, req request) (response, error) {
	var resp response
	if err := roundTrip(ctx, c.Socket, req, &resp); err != nil {
		return resp, err
	}
	if resp.Error == ErrHandedOver.Error() {
		return resp, ErrHandedOver
	}
	if resp.Error != "" {
		return resp, errors.New(resp.Error)
	}
	return resp, nil
}

func roundTrip(ctx context.Context, socket string, req, resp any) error {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", socket)
	if err != nil {
		return err
	}
	defer conn.Close() //nolint:errcheck // one request per connection
	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline) //nolint:errcheck // a dead peer then times out
	}
	stop := context.AfterFunc(ctx, func() { conn.SetDeadline(time.Now()) }) //nolint:errcheck // unblocks I/O
	defer stop()
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return err
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return err
	}
	return json.Unmarshal(line, resp)
}

type presignRequest struct {
	SHA256      []string `json:"sha256,omitempty"`
	Manifests   []string `json:"manifests,omitempty"`
	Generations []string `json:"generations,omitempty"`
}

type presignResponse struct {
	URLs  []string `json:"urls,omitempty"`
	Error string   `json:"error,omitempty"`
}

// SocketPresigner asks the Agent over its presign socket.
type SocketPresigner struct {
	Socket string
}

func (p SocketPresigner) Presign(ctx context.Context, objects, manifests []string) ([]string, error) {
	var resp presignResponse
	if err := roundTrip(ctx, p.Socket, presignRequest{SHA256: objects, Manifests: manifests}, &resp); err != nil {
		return nil, err
	}
	if resp.Error != "" {
		return nil, errors.New(resp.Error)
	}
	return resp.URLs, nil
}

// PresignGenerations signs GETs of Generation manifests (a Volume's head).
func (p SocketPresigner) PresignGenerations(ctx context.Context, ids []string) ([]string, error) {
	var resp presignResponse
	if err := roundTrip(ctx, p.Socket, presignRequest{Generations: ids}, &resp); err != nil {
		return nil, err
	}
	if resp.Error != "" {
		return nil, errors.New(resp.Error)
	}
	return resp.URLs, nil
}

// GenerationPresigner signs Generation manifest GETs; a Presigner that is
// one can serve Volume mounts.
type GenerationPresigner interface {
	PresignGenerations(ctx context.Context, ids []string) ([]string, error)
}

// ServePresign answers the daemon's presign requests with p until ctx ends.
// The Agent runs it; requests over presignBatch objects are refused, as the
// Control Plane would.
func ServePresign(ctx context.Context, l net.Listener, p Presigner, log *slog.Logger) {
	context.AfterFunc(ctx, func() { l.Close() }) //nolint:errcheck // ends Accept
	for {
		conn, err := l.Accept()
		if err != nil {
			if ctx.Err() == nil {
				log.Error("accept presign request", "error", err)
			}
			return
		}
		go func() {
			defer conn.Close() //nolint:errcheck // one request per connection
			var req presignRequest
			line, err := bufio.NewReader(conn).ReadBytes('\n')
			if err == nil {
				err = json.Unmarshal(line, &req)
			}
			var resp presignResponse
			switch {
			case err != nil:
				resp.Error = err.Error()
			case len(req.SHA256)+len(req.Manifests)+len(req.Generations) > presignBatch:
				resp.Error = fmt.Sprintf("at most %d objects per presign", presignBatch)
			case len(req.Generations) > 0:
				gp, ok := p.(GenerationPresigner)
				if !ok {
					resp.Error = "this presigner signs no Generations"
				} else if resp.URLs, err = gp.PresignGenerations(ctx, req.Generations); err != nil {
					resp.Error = err.Error()
				}
			default:
				if resp.URLs, err = p.Presign(ctx, req.SHA256, req.Manifests); err != nil {
					resp.Error = err.Error()
				}
			}
			json.NewEncoder(conn).Encode(resp) //nolint:errcheck // the daemon retries
		}()
	}
}

var listenSeq atomic.Int64

// listenUnix binds path atomically: it listens on a temporary name in the
// same directory and renames it over path, so a predecessor's socket is
// replaced, never unlinked from under a connecting client.
func listenUnix(path string) (*net.UnixListener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	// sun_path holds 107 bytes, so the temporary name is kept about as short as
	// a socket's own name; pid plus a per-process sequence keeps it unique.
	tmp := filepath.Join(filepath.Dir(path),
		"."+strconv.FormatInt(int64(os.Getpid()), 36)+"-"+strconv.FormatInt(listenSeq.Add(1), 36))
	os.Remove(tmp) //nolint:errcheck // stale from a crashed process
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: tmp, Net: "unix"})
	if err != nil {
		return nil, err
	}
	// The path may belong to a successor by the time this listener closes.
	l.SetUnlinkOnClose(false)
	if err := os.Chmod(tmp, 0o600); err != nil {
		l.Close() //nolint:errcheck // report the chmod error
		return nil, err
	}
	if err := os.Rename(tmp, path); err != nil {
		l.Close()      //nolint:errcheck // report the rename error
		os.Remove(tmp) //nolint:errcheck // best effort
		return nil, err
	}
	return l, nil
}

// ListenPresign binds the Agent's presign socket.
func ListenPresign(path string) (net.Listener, error) { return listenUnix(path) }
