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
	Op      string `json:"op"` // mount, unmount, status, selftest, takeover
	Address string `json:"address,omitempty"`
}

type response struct {
	Error  string   `json:"error,omitempty"`
	Mounts []string `json:"mounts,omitempty"`
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

func (c Client) call(ctx context.Context, req request) ([]string, error) {
	var resp response
	if err := roundTrip(ctx, c.Socket, req, &resp); err != nil {
		return nil, err
	}
	if resp.Error == ErrHandedOver.Error() {
		return nil, ErrHandedOver
	}
	if resp.Error != "" {
		return nil, errors.New(resp.Error)
	}
	return resp.Mounts, nil
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
	SHA256    []string `json:"sha256,omitempty"`
	Manifests []string `json:"manifests,omitempty"`
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
			case len(req.SHA256)+len(req.Manifests) > presignBatch:
				resp.Error = fmt.Sprintf("at most %d objects per presign", presignBatch)
			default:
				if resp.URLs, err = p.Presign(ctx, req.SHA256, req.Manifests); err != nil {
					resp.Error = err.Error()
				}
			}
			json.NewEncoder(conn).Encode(resp) //nolint:errcheck // the daemon retries
		}()
	}
}

// listenUnix binds path atomically: it listens on a temporary name in the
// same directory and renames it over path, so a predecessor's socket is
// replaced, never unlinked from under a connecting client.
func listenUnix(path string) (*net.UnixListener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	tmp := fmt.Sprintf("%s.%d.%d", path, os.Getpid(), time.Now().UnixNano())
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
