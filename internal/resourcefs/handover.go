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
	if err := d.sendMounts(conn, reader); err != nil {
		d.cfg.Log.Warn("handover failed; still serving", "error", err)
		return
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

func (d *daemon) sendMounts(conn *net.UnixConn, reader *bufio.Reader) error {
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
			return err
		}
		if err := expectLine(reader, "ok"); err != nil {
			return fmt.Errorf("mount %s: %w", address, err)
		}
	}
	end, _ := json.Marshal(handoverMount{End: true})
	if _, err := conn.Write(append(end, '\n')); err != nil {
		return err
	}
	return expectLine(reader, "ready")
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
func drain(mt *mount, log interface{ Warn(string, ...any) }) {
	done := mt.srv.LoopsDone()
	deadline := time.After(drainTimeout)
	for pokes := 0; ; pokes++ {
		select {
		case <-done:
			return
		case <-deadline:
			log.Warn("handover drain timed out", "address", mt.address)
			return
		case <-time.After(pokeInterval):
			os.Lstat(filepath.Join(mt.dir, fmt.Sprintf(".orrery-handover-%d-%d-%d", os.Getpid(), time.Now().UnixNano(), pokes))) //nolint:errcheck // ENOENT expected
		}
	}
}

// takeover receives every mount from the process on the control socket and
// starts serving them. The caller then says "ready" on the returned conn.
func (d *daemon) takeover(ctx context.Context) (*net.UnixConn, error) {
	var dialer net.Dialer
	c, err := dialer.DialContext(ctx, "unix", d.cfg.ControlSocket)
	if err != nil {
		return nil, err
	}
	conn := c.(*net.UnixConn)
	fail := func(err error) (*net.UnixConn, error) {
		conn.Close() //nolint:errcheck // report err; the predecessor keeps serving
		return nil, err
	}
	if err := json.NewEncoder(conn).Encode(request{Op: "takeover"}); err != nil {
		return fail(err)
	}
	buf, oob := make([]byte, 64<<10), make([]byte, syscall.CmsgSpace(4))
	// Nothing reads a session until every mount is adopted: a successor that
	// fails part-way must not exit holding requests it read but never answered.
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
			d.mu.Lock()
			for _, mt := range adopted {
				d.serve(mt)
			}
			d.mu.Unlock()
			return conn, nil
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
// predecessor built from the same manifest.
func (d *daemon) adopt(header handoverMount, fd int) (*mount, error) {
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
