package resourcefs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zibyn/orrery/internal/manifest"
)

func init() { retryDelay = func(int) time.Duration { return 0 } }

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// objectServer is a presigning object store: GET /o/<digest> serves an
// object (with Range), GET /m/<address> a manifest.
type objectServer struct {
	*httptest.Server
	mu       sync.Mutex
	objects  map[string][]byte
	gets     map[string]int
	batches  [][]string
	corrupt  bool // serve other bytes than the digest names
	presigns int
}

func newObjectServer(t *testing.T) *objectServer {
	o := &objectServer{objects: map[string][]byte{}, gets: map[string]int{}}
	o.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Path[3:]
		o.mu.Lock()
		data, ok := o.objects[key]
		o.gets[key]++
		corrupt := o.corrupt
		o.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		if corrupt {
			data = bytes.ToUpper(append([]byte("x"), data...))[:len(data)]
		}
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(data))
	}))
	t.Cleanup(o.Close)
	return o
}

func (o *objectServer) put(data []byte) string {
	d := digest(data)
	o.mu.Lock()
	o.objects[d] = data
	o.mu.Unlock()
	return d
}

func (o *objectServer) Presign(_ context.Context, objects, manifests []string) ([]string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.presigns++
	o.batches = append(o.batches, append([]string(nil), objects...))
	urls := []string{}
	for _, d := range objects {
		urls = append(urls, o.URL+"/o/"+d)
	}
	for _, a := range manifests {
		urls = append(urls, o.URL+"/o/"+a)
	}
	return urls, nil
}

// tree publishes files (path → content) the way the CLI would: blocks for
// files over 4 MiB. Directories in paths become dir entries.
func (o *objectServer) tree(t *testing.T, files map[string][]byte) (manifest.Manifest, string) {
	t.Helper()
	entries := []manifest.Entry{}
	dirs := map[string]bool{}
	for path, data := range files {
		for p := path; strings.Contains(p, "/"); {
			p = p[:strings.LastIndexByte(p, '/')]
			dirs[p] = true
		}
		size := int64(len(data))
		entry := manifest.Entry{Path: path, Type: manifest.TypeFile, Size: &size}
		if size > manifest.BlockSize {
			for i := 0; i < manifest.BlockCount(size); i++ {
				start := int64(i) * manifest.BlockSize
				entry.Blocks = append(entry.Blocks, o.put(data[start:start+manifest.BlockLength(size, i)]))
			}
		} else {
			entry.SHA256 = o.put(data)
		}
		entries = append(entries, entry)
	}
	for dir := range dirs {
		entries = append(entries, manifest.Entry{Path: dir, Type: manifest.TypeDir})
	}
	m, err := manifest.Normalize(manifest.Manifest{Entries: entries})
	if err != nil {
		t.Fatal(err)
	}
	canonical, _ := json.Marshal(m)
	return m, o.put(canonical)
}

func pattern(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*31/1024) + seed + byte(i>>22)*17
	}
	return b
}

func testLog() *slog.Logger { return slog.New(slog.DiscardHandler) }
