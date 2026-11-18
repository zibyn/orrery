package resourcefs

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"
)

// handoverMount is the header sent with each mount's /dev/fuse descriptor:
// the INIT the kernel negotiated with the first server, which the successor
// cannot redo (resource-fuse-handover-spike.md).
type handoverMount struct {
	Address    string      `json:"address,omitempty"`
	Volume     string      `json:"volume,omitempty"`
	Init       fuse.InitIn `json:"init"`
	Negotiated uint64      `json:"negotiated"`
	End        bool        `json:"end,omitempty"`
}

// handOver gives every mount to the successor on conn. Commands wait on d.mu
// meanwhile. Nothing stops before the successor says "ready": if it fails
// first, this process keeps serving as if nothing happened.
func (d *daemon) handOver(conn *net.UnixConn, reader *bufio.Reader) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.handedOver {
		return
	}
	paused, err := d.sendMounts(conn, reader)
	if err != nil {
		d.cfg.Log.Warn("handover failed; still serving", "error", err)
		d.resumeVolumes(paused)
		return
	}
	for _, p := range paused {
		syscall.Close(p.fd) //nolint:errcheck // the successor holds its own copy
	}
	d.handedOver = true
	for _, mt := range d.mounts {
		mt.srv.StopReading()
	}
	for _, mt := range d.mounts {
		drain(mt, d.cfg.Log)
	}
	close(d.done)
}

// pausedVolume is a Volume session this process stopped reading for the
// handover; fd is kept to serve it again if the handover fails.
type pausedVolume struct {
	mt         *mount
	fd         int
	init       fuse.InitIn
	negotiated uint64
}

func (d *daemon) sendMounts(conn *net.UnixConn, reader *bufio.Reader) ([]pausedVolume, error) {
	addresses := make([]string, 0, len(d.mounts))
	for address := range d.mounts {
		addresses = append(addresses, address)
	}
	sort.Strings(addresses)
	conn.SetDeadline(time.Now().Add(5 * time.Minute)) //nolint:errcheck // bounds a hung successor
	// One mount per message, acknowledged before the next, so a stream read
	// never merges two descriptors' headers.
	for _, address := range addresses {
		srv := d.mounts[address].srv
		header, _ := json.Marshal(handoverMount{
			Address: address, Init: *srv.KernelSettings(), Negotiated: srv.NegotiatedFlags(),
		})
		rights := syscall.UnixRights(int(srv.SessionFile().Fd()))
		if _, _, err := conn.WriteMsgUnix(append(header, '\n'), rights, nil); err != nil {
			return nil, err
		}
		if err := expectLine(reader, "ok"); err != nil {
			return nil, fmt.Errorf("mount %s: %w", address, err)
		}
	}
	// A writable session cannot be answered by two processes: each Volume
	// stops and drains here, and its state goes over with it (runtime.md §17).
	volumes := make([]string, 0, len(d.volumes))
	for volume := range d.volumes {
		volumes = append(volumes, volume)
	}
	sort.Strings(volumes)
	var paused []pausedVolume
	for _, volume := range volumes {
		p, err := d.pauseVolume(d.volumes[volume])
		if err != nil {
			return paused, fmt.Errorf("Volume %s: %w", volume, err)
		}
		paused = append(paused, p)
		header, _ := json.Marshal(handoverMount{Volume: volume, Init: p.init, Negotiated: p.negotiated})
		if _, _, err := conn.WriteMsgUnix(append(header, '\n'), syscall.UnixRights(p.fd), nil); err != nil {
			return paused, err
		}
		if err := expectLine(reader, "ok"); err != nil {
			return paused, fmt.Errorf("Volume %s: %w", volume, err)
		}
	}
	end, _ := json.Marshal(handoverMount{End: true})
	if _, err := conn.Write(append(end, '\n')); err != nil {
		return paused, err
	}
	return paused, expectLine(reader, "ready")
}

// pauseVolume stops reading a Volume's session, waits for the requests in
// hand, and writes its state for the successor. Requests arriving meanwhile
// wait in the kernel.
func (d *daemon) pauseVolume(mt *mount) (pausedVolume, error) {
	fd, err := syscall.Dup(int(mt.srv.SessionFile().Fd()))
	if err != nil {
		return pausedVolume{}, err
	}
	p := pausedVolume{mt: mt, fd: fd, init: *mt.srv.KernelSettings(), negotiated: mt.srv.NegotiatedFlags()}
	mt.srv.StopReading()
	if !drain(mt, d.cfg.Log) {
		return p, errors.New("requests did not drain")
	}
	return p, mt.volume.pause()
}

// resumeVolumes serves paused Volumes again from their kept descriptors,
// with the state this process still holds.
func (d *daemon) resumeVolumes(paused []pausedVolume) {
	for _, p := range paused {
		p.mt.volume.resume()
		srv, err := fuse.NewServerFromFD(p.mt.volume, p.fd, p.mt.dir, volumeMountOptions(p.mt.address), p.init, p.negotiated)
		if err != nil {
			d.cfg.Log.Error("serve a Volume again after a failed handover", "volume", p.mt.address, "error", err)
			syscall.Close(p.fd) //nolint:errcheck // the mount is lost
			continue
		}
		d.serve(&mount{address: p.mt.address, dir: p.mt.dir, srv: srv, volume: p.mt.volume})
	}
}

func expectLine(reader *bufio.Reader, want string) error {
	line, err := reader.ReadString('\n')
	if err != nil {
		return err
	}
	if line != want+"\n" {
		return fmt.Errorf("successor said %q, want %q", line, want)
	}
	return nil
}

// drain waits until every reader of mt has answered the request it held. An
// idle reader is blocked in read(2), so lookups of fresh names wake it.
func drain(mt *mount, log interface{ Warn(string, ...any) }) bool {
	done := mt.srv.LoopsDone()
	deadline := time.After(drainTimeout)
	for pokes := 0; ; pokes++ {
		select {
		case <-done:
			return true
		case <-deadline:
			log.Warn("handover drain timed out", "address", mt.address)
			return false
		case <-time.After(pokeInterval):
			// Asynchronously: a poke the last reader did not take waits for the
			// successor, which a Volume's drain must not block on.
			go os.Lstat(filepath.Join(mt.dir, fmt.Sprintf(".orrery-handover-%d-%d-%d", os.Getpid(), time.Now().UnixNano(), pokes))) //nolint:errcheck // ENOENT expected
		}
	}
}

// takeover receives every mount from the process on the control socket and
// returns them unserved. The caller says "ready" on the returned conn, then
// serves them: a successor that fails before that has read no request.
func (d *daemon) takeover(ctx context.Context) (*net.UnixConn, []*mount, error) {
	var dialer net.Dialer
	c, err := dialer.DialContext(ctx, "unix", d.cfg.ControlSocket)
	if err != nil {
		return nil, nil, err
	}
	conn := c.(*net.UnixConn)
	fail := func(err error) (*net.UnixConn, []*mount, error) {
		conn.Close() //nolint:errcheck // report err; the predecessor keeps serving
		return nil, nil, err
	}
	if err := json.NewEncoder(conn).Encode(request{Op: "takeover"}); err != nil {
		return fail(err)
	}
	buf, oob := make([]byte, 64<<10), make([]byte, syscall.CmsgSpace(4))
	var adopted []*mount
	for {
		n, oobn, _, _, err := conn.ReadMsgUnix(buf, oob)
		if err != nil {
			return fail(err)
		}
		var header handoverMount
		if err := json.Unmarshal(buf[:n], &header); err != nil {
			return fail(fmt.Errorf("handover header: %w", err))
		}
		if header.End {
			return conn, adopted, nil
		}
		fd, err := receivedFD(oob[:oobn])
		if err != nil {
			return fail(err)
		}
		mt, err := d.adopt(header, fd)
		if err != nil {
			syscall.Close(fd) //nolint:errcheck // the predecessor still holds its copy
			return fail(err)
		}
		adopted = append(adopted, mt)
		if _, err := conn.Write([]byte("ok\n")); err != nil {
			return fail(err)
		}
	}
}

func receivedFD(oob []byte) (int, error) {
	msgs, err := syscall.ParseSocketControlMessage(oob)
	if err != nil {
		return -1, err
	}
	if len(msgs) != 1 {
		return -1, errors.New("handover message carries no descriptor")
	}
	fds, err := syscall.ParseUnixRights(&msgs[0])
	if err != nil {
		return -1, err
	}
	if len(fds) != 1 {
		return -1, fmt.Errorf("handover message carries %d descriptors", len(fds))
	}
	return fds[0], nil
}

// adopt serves an already initialized session with the same state the
// predecessor built from the same manifest, or, for a Volume, wrote.
func (d *daemon) adopt(header handoverMount, fd int) (*mount, error) {
	if header.Volume != "" {
		if !validVolume(header.Volume) {
			return nil, fmt.Errorf("invalid Volume %q", header.Volume)
		}
		dir := d.volumeDir(header.Volume)
		fs, err := loadVolumeFS(dir, d.store, d.cfg.Log)
		if err != nil {
			return nil, err
		}
		srv, err := fuse.NewServerFromFD(fs, fd, volumeMountDir(dir), volumeMountOptions(header.Volume), header.Init, header.Negotiated)
		if err != nil {
			return nil, err
		}
		return &mount{address: header.Volume, dir: volumeMountDir(dir), srv: srv, volume: fs}, nil
	}
	if !validAddress(header.Address) {
		return nil, fmt.Errorf("invalid address %q", header.Address)
	}
	m, err := d.loadManifest(header.Address)
	if err != nil {
		return nil, err
	}
	dir := d.mountDir(header.Address)
	srv, err := fuse.NewServerFromFD(newRawFS(m, d.store, d.cfg.Log), fd, dir, mountOptions(header.Address), header.Init, header.Negotiated)
	if err != nil {
		return nil, err
	}
	return &mount{address: header.Address, dir: dir, srv: srv}, nil
}

// peerPID is the process on the other end of a unix socket, or 0.
func peerPID(conn *net.UnixConn) int {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0
	}
	pid := 0
	_ = raw.Control(func(fd uintptr) {
		if cred, err := syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED); err == nil {
			pid = int(cred.Pid)
		}
	})
	return pid
}
