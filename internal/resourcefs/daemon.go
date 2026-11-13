package resourcefs

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/zibyn/orrery/internal/manifest"
)

const (
	// maxManifestBytes matches the Control Plane's manifest bound.
	maxManifestBytes = 256 << 20
	// pokeInterval wakes the old process's readers blocked in read(2) on an
	// idle mount during a handover (resource-fuse-handover-spike.md).
	pokeInterval = 20 * time.Millisecond
	drainTimeout = 30 * time.Second
)

// Config configures one daemon process.
type Config struct {
	DataRoot      string
	ControlSocket string
	Presigner     Presigner
	// Takeover takes every mount over from the process serving
	// ControlSocket instead of starting empty.
	Takeover bool
	Log      *slog.Logger
}

type mount struct {
	address string
	dir     string
	srv     *fuse.Server
}

type daemon struct {
	cfg   Config
	store *store

	mu         sync.Mutex // serializes commands with each other and with a handover
	mounts     map[string]*mount
	handedOver bool
	done       chan struct{} // closed once a successor serves every mount
}

// Run serves until ctx ends or a successor takes the mounts over. Mounts are
// not unmounted on return: a successor serves them, or a restarted daemon
// replaces them on the Agent's next mount.
func Run(ctx context.Context, cfg Config) error {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	d := &daemon{
		cfg:    cfg,
		store:  newStore(filepath.Join(cfg.DataRoot, "cache", "blocks"), cfg.Presigner),
		mounts: map[string]*mount{},
		done:   make(chan struct{}),
	}
	if err := d.store.cleanTemp(); err != nil {
		return err
	}
	var predecessor *net.UnixConn
	if cfg.Takeover {
		conn, err := d.takeover(ctx)
		if err != nil {
			return fmt.Errorf("take over: %w", err)
		}
		predecessor = conn
		// The predecessor waits for this before it stops reading; until then
		// both processes answer, identically.
		if _, err := conn.Write([]byte("ready\n")); err != nil {
			conn.Close() //nolint:errcheck // report the write error
			return fmt.Errorf("take over: %w", err)
		}
	}
	l, err := listenUnix(cfg.ControlSocket)
	if predecessor != nil {
		predecessor.Close() //nolint:errcheck // the handover is complete
	}
	if err != nil {
		return err
	}
	defer l.Close() //nolint:errcheck // SetUnlinkOnClose(false): the path may be a successor's
	cfg.Log.Info("resourcefs serving", "socket", cfg.ControlSocket, "mounts", len(d.mounts))
	go d.accept(l)
	select {
	case <-ctx.Done():
	case <-d.done:
		cfg.Log.Info("resourcefs handed over")
	}
	return nil
}

func (d *daemon) accept(l *net.UnixListener) {
	for {
		conn, err := l.AcceptUnix()
		if err != nil {
			return
		}
		go d.serveConn(conn)
	}
}

func (d *daemon) serveConn(conn *net.UnixConn) {
	defer conn.Close() //nolint:errcheck // one request per connection
	reader := bufio.NewReader(conn)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		return
	}
	var req request
	if err := json.Unmarshal(line, &req); err != nil {
		json.NewEncoder(conn).Encode(response{Error: err.Error()}) //nolint:errcheck // the peer sent garbage
		return
	}
	if req.Op == "takeover" {
		d.handOver(conn, reader)
		return
	}
	var resp response
	if err := d.command(req, &resp); err != nil {
		resp.Error = err.Error()
	}
	json.NewEncoder(conn).Encode(resp) //nolint:errcheck // the Agent retries
}

func (d *daemon) command(req request, resp *response) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.handedOver {
		return ErrHandedOver
	}
	if req.Op == "mount" && validAddress(req.Address) {
		// The manifest read can take a minute; it needs no lock.
		d.mu.Unlock()
		m, err := d.loadManifest(req.Address)
		d.mu.Lock()
		if err != nil {
			return err
		}
		if d.handedOver {
			return ErrHandedOver
		}
		if _, ok := d.mounts[req.Address]; ok {
			return nil
		}
		return d.mount(req.Address, m)
	}
	switch req.Op {
	case "status":
		resp.Mounts = make([]string, 0, len(d.mounts))
		for address := range d.mounts {
			resp.Mounts = append(resp.Mounts, address)
		}
		sort.Strings(resp.Mounts)
		return nil
	case "selftest":
		return d.selfTest()
	}
	if !validAddress(req.Address) {
		return fmt.Errorf("invalid address %q", req.Address)
	}
	switch req.Op {
	case "unmount":
		return d.unmount(req.Address)
	}
	return fmt.Errorf("unknown op %q", req.Op)
}

func (d *daemon) mountDir(address string) string {
	return filepath.Join(d.cfg.DataRoot, "resources", address)
}

// mount serves m at the version's directory. A mount left by a crashed
// daemon is dead (ENOTCONN), so it is detached first.
func (d *daemon) mount(address string, m manifest.Manifest) error {
	dir := d.mountDir(address)
	detach(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	srv, err := fuse.NewServer(newRawFS(m, d.store, d.cfg.Log), dir, mountOptions(address))
	if err != nil {
		return fmt.Errorf("mount %s: %w", address, err)
	}
	mt := &mount{address: address, dir: dir, srv: srv}
	d.serve(mt)
	if err := srv.WaitMount(); err != nil {
		delete(d.mounts, address) // a retry must mount again, not find this
		detach(dir)
		return err
	}
	return nil
}

// serve runs a server and forgets its mount when the session ends without a
// handover, e.g. after an external umount.
func (d *daemon) serve(mt *mount) {
	d.mounts[mt.address] = mt
	go func() {
		mt.srv.Serve()
		d.mu.Lock()
		defer d.mu.Unlock()
		if !d.handedOver && d.mounts[mt.address] == mt {
			delete(d.mounts, mt.address)
		}
	}()
}

func (d *daemon) unmount(address string) error {
	mt, ok := d.mounts[address]
	if !ok {
		return nil
	}
	delete(d.mounts, address)
	// Lazily: a Sandbox being torn down may still hold the bind mount; the
	// session ends when the last reference goes.
	if err := syscall.Unmount(mt.dir, syscall.MNT_DETACH); err != nil {
		return fmt.Errorf("unmount %s: %w", address, err)
	}
	os.Remove(mt.dir) //nolint:errcheck // an empty mount point; harmless if left
	return nil
}

func detach(dir string) {
	syscall.Unmount(dir, syscall.MNT_DETACH) //nolint:errcheck // EINVAL when nothing is mounted
}

// loadManifest reads the canonical manifest from cache/manifests/<address>
// or through a presigned GET, and checks that its SHA-256 is the address.
func (d *daemon) loadManifest(address string) (manifest.Manifest, error) {
	path := filepath.Join(d.cfg.DataRoot, "cache", "manifests", address)
	data, err := os.ReadFile(path)
	if err != nil || !matches(data, address) {
		if data, err = d.fetchManifest(address); err != nil {
			return manifest.Manifest{}, err
		}
		if err := writeFileAtomic(path, data); err != nil {
			return manifest.Manifest{}, err
		}
	}
	var m manifest.Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return manifest.Manifest{}, fmt.Errorf("decode manifest %s: %w", address, err)
	}
	return manifest.Normalize(m)
}

func matches(data []byte, address string) bool {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]) == address
}

func (d *daemon) fetchManifest(address string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), getTimeout)
	defer cancel()
	urls, err := d.cfg.Presigner.Presign(ctx, nil, []string{address})
	if err != nil {
		return nil, fmt.Errorf("presign manifest %s: %w", address, err)
	}
	if len(urls) != 1 {
		return nil, fmt.Errorf("presign returned %d manifest URLs", len(urls))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urls[0], nil)
	if err != nil {
		return nil, err
	}
	resp, err := d.store.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck // read-only
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch manifest %s: %s", address, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestBytes+1))
	if err != nil {
		return nil, err
	}
	if !matches(data, address) {
		return nil, fmt.Errorf("manifest %s does not match its address", address)
	}
	return data, nil
}

func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) //nolint:errcheck // gone after the rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close() //nolint:errcheck // report the write error
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// selfTestContent is the one file of the self-test version. Its block is
// written to the cache first, so the read goes through FUSE without the
// object store.
var selfTestContent = []byte("orrery-resourcefs self-test\n")

func (d *daemon) selfTest() error {
	sum := sha256.Sum256(selfTestContent)
	digest := hex.EncodeToString(sum[:])
	if err := writeFileAtomic(d.store.path(digest), selfTestContent); err != nil {
		return err
	}
	size := int64(len(selfTestContent))
	m, err := manifest.Normalize(manifest.Manifest{Entries: []manifest.Entry{
		{Path: "self-test", Type: manifest.TypeFile, Size: &size, SHA256: digest},
	}})
	if err != nil {
		return err
	}
	address, _ := manifest.Address(m)
	if err := d.mount(address, m); err != nil {
		return err
	}
	defer d.unmount(address) //nolint:errcheck // a leftover is detached on the next mount
	got, err := os.ReadFile(filepath.Join(d.mountDir(address), "self-test"))
	if err != nil {
		return fmt.Errorf("self-test read: %w", err)
	}
	if string(got) != string(selfTestContent) {
		return errors.New("self-test read returned other bytes")
	}
	return nil
}
