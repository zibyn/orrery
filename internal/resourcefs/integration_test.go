package resourcefs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/zibyn/orrery/internal/manifest"
)

// These tests mount real FUSE file systems: they need root (or a user
// namespace, `unshare -rm`) and /dev/fuse. CI lacks both; run them on a Node.
func requireFUSE(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root for FUSE mounts")
	}
	f, err := os.OpenFile("/dev/fuse", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("no /dev/fuse: %v", err)
	}
	f.Close()
}

type harness struct {
	t       *testing.T
	o       *objectServer
	root    string
	socket  string
	files   map[string][]byte
	address string
	m       manifest.Manifest
}

func newHarness(t *testing.T) *harness {
	requireFUSE(t)
	h := &harness{t: t, o: newObjectServer(t), root: t.TempDir(), files: map[string][]byte{}}
	h.socket = filepath.Join(h.root, "control.sock")
	for i := 0; i < 300; i++ {
		h.files[fmt.Sprintf("small/%02d/%03d", i%10, i)] = pattern(1000+i*97, byte(i))
	}
	h.files["big/nine"] = pattern(9<<20, 5)
	h.files["big/exactly-4m"] = pattern(4<<20, 6)
	h.files["empty"] = nil
	h.m, h.address = h.o.tree(t, h.files)
	t.Cleanup(func() { detach(filepath.Join(h.root, "resources", h.address)) })
	return h
}

func (h *harness) dir() string { return filepath.Join(h.root, "resources", h.address) }

// start runs a daemon; the returned channel yields Run's result.
func (h *harness) start(takeover bool) (context.CancelFunc, chan error) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Config{DataRoot: h.root, ControlSocket: h.socket, Presigner: h.o, Takeover: takeover, Log: testLog()})
	}()
	h.t.Cleanup(cancel)
	return cancel, done
}

func (h *harness) client() Client { return Client{Socket: h.socket} }

// waitServing polls until a daemon answers status on the socket.
func (h *harness) waitServing() []string {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		mounts, err := h.client().Status(context.Background())
		if err == nil {
			return mounts
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("daemon did not come up: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (h *harness) checkAll() {
	h.t.Helper()
	for path, want := range h.files {
		got, err := os.ReadFile(filepath.Join(h.dir(), path))
		if err != nil || !bytes.Equal(got, want) {
			h.t.Fatalf("%s: %d bytes, want %d (err %v)", path, len(got), len(want), err)
		}
	}
}

// handover starts a successor and waits until the predecessor has exited.
func (h *harness) handover(predecessor chan error) chan error {
	h.t.Helper()
	_, next := h.start(true)
	select {
	case err := <-predecessor:
		if err != nil {
			h.t.Fatalf("predecessor: %v", err)
		}
	case err := <-next:
		h.t.Fatalf("successor exited: %v", err)
	case <-time.After(30 * time.Second):
		h.t.Fatal("handover did not finish")
	}
	h.waitServing()
	return next
}

func TestMountMatchesTheSource(t *testing.T) {
	h := newHarness(t)
	_, done := h.start(false)
	h.waitServing()
	ctx := context.Background()
	if err := h.client().Mount(ctx, h.address); err != nil {
		t.Fatal(err)
	}
	if err := h.client().Mount(ctx, h.address); err != nil {
		t.Fatalf("second mount: %v", err)
	}
	h.checkAll()
	entries, err := os.ReadDir(filepath.Join(h.dir(), "small"))
	if err != nil || len(entries) != 10 {
		t.Fatalf("readdir small = %d entries, %v", len(entries), err)
	}
	if err := os.WriteFile(filepath.Join(h.dir(), "new"), nil, 0o644); !errors.Is(err, syscall.EROFS) && !errors.Is(err, syscall.EACCES) && !errors.Is(err, syscall.ENOSYS) {
		t.Fatalf("write = %v, want a read-only refusal", err)
	}
	if mounts, _ := h.client().Status(ctx); len(mounts) != 1 || mounts[0] != h.address {
		t.Fatalf("status = %v", mounts)
	}
	if err := h.client().SelfTest(ctx); err != nil {
		t.Fatalf("self-test: %v", err)
	}
	if err := h.client().Unmount(ctx, h.address); err != nil {
		t.Fatal(err)
	}
	if mounts, _ := h.client().Status(ctx); len(mounts) != 0 {
		t.Fatalf("status after unmount = %v", mounts)
	}
	select {
	case err := <-done:
		t.Fatalf("daemon exited: %v", err)
	default:
	}
}

func TestHandoverUnderLoadAndIdle(t *testing.T) {
	h := newHarness(t)
	_, current := h.start(false)
	h.waitServing()
	if err := h.client().Mount(context.Background(), h.address); err != nil {
		t.Fatal(err)
	}
	paths := make([]string, 0, len(h.files))
	for path := range h.files {
		paths = append(paths, path)
	}

	var stop atomic.Bool
	var reads, failures atomic.Int64
	var firstErr atomic.Value
	fail := func(err error) {
		failures.Add(1)
		firstErr.CompareAndSwap(nil, err)
	}
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				path := paths[rand.IntN(len(paths))]
				got, err := os.ReadFile(filepath.Join(h.dir(), path))
				if err != nil || !bytes.Equal(got, h.files[path]) {
					fail(fmt.Errorf("%s: %d bytes, %v", path, len(got), err))
				}
				reads.Add(1)
			}
		}()
	}
	wg.Add(1)
	go func() { // one handle open across every handover
		defer wg.Done()
		f, err := os.Open(filepath.Join(h.dir(), "big/nine"))
		if err != nil {
			fail(err)
			return
		}
		defer f.Close()
		buf := make([]byte, 1<<20)
		want := h.files["big/nine"]
		for off := int64(0); !stop.Load(); off = (off + 1<<20) % int64(len(want)) {
			n, err := f.ReadAt(buf, off)
			if (err != nil && n != len(want)-int(off)) || !bytes.Equal(buf[:n], want[off:off+int64(n)]) {
				fail(fmt.Errorf("open handle at %d: %d bytes, %v", off, n, err))
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()

	for i := 0; i < 3; i++ {
		time.Sleep(300 * time.Millisecond)
		current = h.handover(current)
	}
	time.Sleep(300 * time.Millisecond)
	stop.Store(true)
	wg.Wait()
	if failures.Load() > 0 {
		t.Fatalf("%d of %d reads failed during handovers; first: %v", failures.Load(), reads.Load(), firstErr.Load())
	}
	t.Logf("%d reads across 3 handovers", reads.Load())

	// Idle: nothing reads while the old process drains.
	time.Sleep(200 * time.Millisecond)
	current = h.handover(current)
	h.checkAll()
	if mounts := h.waitServing(); len(mounts) != 1 {
		t.Fatalf("successor status = %v", mounts)
	}
	_ = current
}

// A successor that dies before "ready" leaves the old process serving.
func TestFailedTakeoverKeepsServing(t *testing.T) {
	h := newHarness(t)
	_, done := h.start(false)
	h.waitServing()
	if err := h.client().Mount(context.Background(), h.address); err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("unix", h.socket)
	if err != nil {
		t.Fatal(err)
	}
	json.NewEncoder(conn).Encode(request{Op: "takeover"})
	buf, oob := make([]byte, 4096), make([]byte, syscall.CmsgSpace(4))
	if _, oobn, _, _, err := conn.(*net.UnixConn).ReadMsgUnix(buf, oob); err != nil {
		t.Fatal(err)
	} else if fd, err := receivedFD(oob[:oobn]); err != nil {
		t.Fatal(err)
	} else {
		syscall.Close(fd)
	}
	conn.Close() // dies holding the descriptor, before ok or ready

	h.checkAll()
	if mounts := h.waitServing(); len(mounts) != 1 {
		t.Fatalf("status after failed takeover = %v", mounts)
	}
	select {
	case err := <-done:
		t.Fatalf("old daemon exited: %v", err)
	default:
	}
	// A real handover still works afterwards.
	h.handover(done)
	h.checkAll()
}
