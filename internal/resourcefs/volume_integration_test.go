package resourcefs

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/zibyn/orrery/internal/generation"
	"github.com/zibyn/orrery/internal/manifest"
)

// Like the Resource tests, these mount real FUSE file systems: root and
// /dev/fuse, so they run on a Node (a800), not in CI.

const testVolume = "0198c7a2-0000-7000-8000-0000000000aa"

func (h *harness) volumeDir() string { return filepath.Join(h.root, "volumes", testVolume, "mnt") }

// mountVolume mounts testVolume from a head holding "seed".
func (h *harness) mountVolume() {
	h.t.Helper()
	size := int64(4)
	m, err := generation.Encode(generation.Manifest{Kind: generation.KindTree, Entries: []generation.Entry{
		{Entry: manifest.Entry{Path: "seed", Type: manifest.TypeFile, Size: &size, SHA256: h.o.put([]byte("seed"))}, Mode: 0o644},
	}})
	if err != nil {
		h.t.Fatal(err)
	}
	h.o.putGeneration("head-1", m)
	if err := h.client().MountVolume(context.Background(), testVolume, "head-1"); err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { detach(h.volumeDir()) })
}

func TestVolumeMountPOSIX(t *testing.T) {
	h := newHarness(t)
	h.start(false)
	h.waitServing()
	h.mountVolume()
	dir := h.volumeDir()
	if got, err := os.ReadFile(filepath.Join(dir, "seed")); err != nil || string(got) != "seed" {
		t.Fatalf("seed = %q, %v", got, err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	// The lock-file pattern Git uses: write a temporary, rename it over.
	must(os.WriteFile(filepath.Join(dir, "config.lock"), []byte("new"), 0o644))
	must(os.WriteFile(filepath.Join(dir, "config"), []byte("old"), 0o644))
	must(os.Rename(filepath.Join(dir, "config.lock"), filepath.Join(dir, "config")))
	must(os.Link(filepath.Join(dir, "config"), filepath.Join(dir, "config.hard")))
	var st syscall.Stat_t
	must(syscall.Stat(filepath.Join(dir, "config.hard"), &st))
	if st.Nlink != 2 {
		t.Fatalf("nlink = %d", st.Nlink)
	}
	// O_APPEND from two handles.
	for i := 0; i < 2; i++ {
		f, err := os.OpenFile(filepath.Join(dir, "log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		must(err)
		_, err = f.WriteString(fmt.Sprintf("line %d\n", i))
		must(err)
		must(f.Close())
	}
	// A shared mmap write reaches the file.
	f, err := os.OpenFile(filepath.Join(dir, "mapped"), os.O_CREATE|os.O_RDWR, 0o644)
	must(err)
	must(f.Truncate(4096))
	mem, err := syscall.Mmap(int(f.Fd()), 0, 4096, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	must(err)
	copy(mem, "mapped!")
	must(syscall.Munmap(mem))
	must(f.Sync())
	must(f.Close())
	// flock is the kernel's, local to the Node.
	lock, err := os.Open(filepath.Join(dir, "log"))
	must(err)
	must(syscall.Flock(int(lock.Fd()), syscall.LOCK_EX))
	lock.Close()

	s, err := h.client().SealVolume(context.Background(), testVolume)
	must(err)
	got := map[string]generation.Entry{}
	for _, e := range s.Manifest.Entries {
		got[e.Path] = e
	}
	if got["config"].SHA256 != digest([]byte("new")) && got["config.hard"].SHA256 != digest([]byte("new")) {
		t.Fatalf("config = %+v", got["config"])
	}
	if got["log"].SHA256 != digest([]byte("line 0\nline 1\n")) {
		t.Fatal("O_APPEND writes are not both in the commit")
	}
	mapped := make([]byte, 4096)
	copy(mapped, "mapped!")
	if got["mapped"].SHA256 != digest(mapped) {
		t.Fatal("the mmap write is not in the commit")
	}
	if got["seed"].SHA256 != digest([]byte("seed")) {
		t.Fatal("the head's file is not in the commit")
	}
}

// A handover while a writer runs keeps every write, committed or not.
func TestVolumeHandoverDuringWrites(t *testing.T) {
	h := newHarness(t)
	_, current := h.start(false)
	h.waitServing()
	h.mountVolume()
	dir := h.volumeDir()

	var stop atomic.Bool
	var written atomic.Int64
	var firstErr atomic.Value
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		log, err := os.OpenFile(filepath.Join(dir, "open-log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			firstErr.CompareAndSwap(nil, err)
			return
		}
		defer log.Close()
		for i := 0; !stop.Load(); i++ {
			if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%05d", i)), []byte(fmt.Sprint(i)), 0o644); err != nil {
				firstErr.CompareAndSwap(nil, err)
				return
			}
			if _, err := fmt.Fprintf(log, "%d\n", i); err != nil {
				firstErr.CompareAndSwap(nil, err)
				return
			}
			written.Store(int64(i + 1))
		}
	}()
	time.Sleep(300 * time.Millisecond)
	// One commit lands before the handovers; the rest stays uncommitted.
	s, err := h.client().SealVolume(context.Background(), testVolume)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.client().VolumeCommitted(context.Background(), testVolume, "gen-2", s.Seq); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		time.Sleep(300 * time.Millisecond)
		current = h.handover(current)
	}
	time.Sleep(200 * time.Millisecond)
	stop.Store(true)
	wg.Wait()
	if err := firstErr.Load(); err != nil {
		t.Fatalf("a write failed during the handovers: %v", err)
	}
	n := int(written.Load())
	var log bytes.Buffer
	for i := 0; i < n; i++ {
		got, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("f%05d", i)))
		if err != nil || string(got) != fmt.Sprint(i) {
			t.Fatalf("f%05d = %q, %v", i, got, err)
		}
		fmt.Fprintf(&log, "%d\n", i)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "open-log"))
	if !bytes.Equal(got, log.Bytes()) {
		t.Fatalf("open-log has %d bytes, want %d", len(got), log.Len())
	}
	after, err := h.client().SealVolume(context.Background(), testVolume)
	if err != nil {
		t.Fatal(err)
	}
	if after.Base != "gen-2" || !after.Dirty {
		t.Fatalf("after the handovers: base %q dirty %v", after.Base, after.Dirty)
	}
	t.Logf("%d files written across 2 handovers", n)
}

// Readers and writers on different files run on through back-to-back
// commits (runtime.md §17): each read returns what was written, a ref
// renamed over seals whole, and the last seal holds exactly what was written.
func TestVolumeConcurrentIOAcrossCommits(t *testing.T) {
	h := newHarness(t)
	h.start(false)
	h.waitServing()
	h.mountVolume()
	dir := h.volumeDir()
	ctx := context.Background()
	const writers, rounds, chunk = 4, 200, 64 << 10
	// expected is writer w's file once its first n rounds are written.
	expected := func(w, n int) []byte {
		var b []byte
		for r := range n {
			b = append(b, pattern(chunk, byte(w*rounds+r))...)
		}
		return b
	}
	refs := map[string]bool{}
	for r := range rounds {
		refs[digest(fmt.Appendf(nil, "ref %d\n", r))] = true
	}

	var work, background sync.WaitGroup
	for w := range writers {
		work.Go(func() {
			f, err := os.OpenFile(filepath.Join(dir, fmt.Sprintf("w%d", w)), os.O_CREATE|os.O_RDWR, 0o644)
			if err != nil {
				t.Error(err)
				return
			}
			defer f.Close()
			for r := range rounds {
				data, off := pattern(chunk, byte(w*rounds+r)), int64(r*chunk)
				if _, err := f.WriteAt(data, off); err != nil {
					t.Errorf("w%d round %d: %v", w, r, err)
					return
				}
				got := make([]byte, chunk)
				if _, err := f.ReadAt(got, off); err != nil || !bytes.Equal(got, data) {
					t.Errorf("w%d round %d reads other bytes (%v)", w, r, err)
					return
				}
				if info, err := f.Stat(); err != nil || info.Size() != off+chunk {
					t.Errorf("w%d round %d: size %v, want %d", w, r, info, off+chunk)
					return
				}
				if r%16 == 0 {
					if err := f.Sync(); err != nil {
						t.Errorf("fsync w%d: %v", w, err)
						return
					}
				}
			}
		})
	}
	// Git's pattern: write a lock file, rename it over the ref. notes is
	// written rarely, so commits drop its clean copy and the next append
	// copies it up from the block cache.
	var notes []byte
	work.Go(func() {
		lock, ref := filepath.Join(dir, "ref.lock"), filepath.Join(dir, "ref")
		for r := range rounds {
			want := fmt.Appendf(nil, "ref %d\n", r)
			if err := os.WriteFile(lock, want, 0o644); err != nil {
				t.Error(err)
				return
			}
			if err := os.Rename(lock, ref); err != nil {
				t.Error(err)
				return
			}
			if got, err := os.ReadFile(ref); err != nil || !bytes.Equal(got, want) {
				t.Errorf("ref reads %q after rename, want %q (%v)", got, want, err)
				return
			}
			if r%50 == 0 {
				notes = fmt.Appendf(notes, "note %d\n", r)
				f, err := os.OpenFile(filepath.Join(dir, "notes"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
				if err == nil {
					_, err = fmt.Fprintf(f, "note %d\n", r)
					f.Close()
				}
				if err != nil {
					t.Error(err)
					return
				}
			}
		}
	})
	stop := make(chan struct{})
	// Readers of the other files: whatever has been written so far reads as
	// its writer's bytes, and the head's file reads as committed.
	for range 2 {
		background.Go(func() {
			full := make([][]byte, writers)
			for w := range full {
				full[w] = expected(w, rounds)
			}
			for {
				select {
				case <-stop:
					return
				default:
				}
				if got, err := os.ReadFile(filepath.Join(dir, "seed")); err != nil || string(got) != "seed" {
					t.Errorf("seed reads %q (%v)", got, err)
					return
				}
				for w := range writers {
					got, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("w%d", w)))
					if err != nil && !os.IsNotExist(err) {
						t.Error(err)
						return
					}
					if len(got) > len(full[w]) || !bytes.Equal(got, full[w][:len(got)]) {
						t.Errorf("w%d reads %d bytes not all its writer's", w, len(got))
						return
					}
				}
			}
		})
	}
	// The Agent's commit loop, back to back; every sealed ref is one whole
	// version of it.
	commits := 0
	background.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			s, err := h.client().SealVolume(ctx, testVolume)
			if err != nil {
				t.Error(err)
				return
			}
			if !s.Dirty {
				continue
			}
			for _, e := range s.Manifest.Entries {
				if e.Path == "ref" && !refs[e.SHA256] {
					t.Errorf("a seal holds a ref no rename wrote: %s", e.SHA256)
				}
			}
			commits++
			if err := h.client().VolumeCommitted(ctx, testVolume, fmt.Sprintf("gen-%d", commits), s.Seq); err != nil {
				t.Error(err)
				return
			}
		}
	})
	work.Wait()
	close(stop)
	background.Wait()
	if t.Failed() {
		return
	}

	want := map[string][]byte{"seed": []byte("seed"), "ref": fmt.Appendf(nil, "ref %d\n", rounds-1), "notes": notes}
	for w := range writers {
		want[fmt.Sprintf("w%d", w)] = expected(w, rounds)
	}
	s, err := h.client().SealVolume(ctx, testVolume)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]generation.Entry{}
	for _, e := range s.Manifest.Entries {
		got[e.Path] = e
	}
	for path, content := range want {
		if data, err := os.ReadFile(filepath.Join(dir, path)); err != nil || !bytes.Equal(data, content) {
			t.Errorf("%s reads %d bytes, want %d (%v)", path, len(data), len(content), err)
		}
		address := digest(content)
		if int64(len(content)) > manifest.BlockSize {
			address = digestOf(content)
		}
		if got[path].SHA256 != address {
			t.Errorf("the last seal has %s as %s, want %s", path, got[path].SHA256, address)
		}
	}
	t.Logf("%d commits during the run", commits)
}
