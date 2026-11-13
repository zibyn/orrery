package client

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/zibyn/orrery/internal/manifest"
)

// fakeResourceServer is the smallest Control Plane plus object store a push
// and pull need: one Resource, one version, CAS objects verified on PUT.
type fakeResourceServer struct {
	mu       sync.Mutex
	objects  map[string][]byte
	manifest []byte
	// urlLife > 0 expires a signing round's URLs after that many object
	// requests, the way a presigned URL stops working an hour after signing.
	urlLife int
	rounds  []int // object requests served per signing round
}

// sign starts a signing round and returns the query that marks its URLs.
func (f *fakeResourceServer) sign() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rounds = append(f.rounds, 0)
	return "?round=" + strconv.Itoa(len(f.rounds)-1)
}

// expired counts one object request and reports whether its URL has expired.
func (f *fakeResourceServer) expired(r *http.Request) bool {
	round, _ := strconv.Atoi(r.URL.Query().Get("round"))
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.urlLife > 0 && f.rounds[round] >= f.urlLife {
		return true
	}
	f.rounds[round]++
	return false
}

func (f *fakeResourceServer) handler(t *testing.T, base func() string) http.Handler {
	mux := http.NewServeMux()
	resource := `{"id":"r1","name":"data","current_version_id":"0192f3a4-0000-7000-8000-000000000001"}`
	mux.HandleFunc("GET /api/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"items":[`+resource+`]}`)
	})
	mux.HandleFunc("POST /api/v1/resources/r1/uploads", func(w http.ResponseWriter, r *http.Request) {
		var m manifest.Manifest
		if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
			t.Error(err)
		}
		m, err := manifest.Normalize(m)
		if err == nil {
			err = manifest.RequireBlocks(m)
		}
		if err != nil {
			t.Errorf("upload manifest: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.manifest, _ = json.Marshal(m)
		f.mu.Unlock()
		query := f.sign()
		puts := []map[string]string{}
		for _, object := range manifest.FileObjects(m) {
			f.mu.Lock()
			_, stored := f.objects[object.SHA256]
			f.mu.Unlock()
			if !stored { // a resubmitted manifest only signs what is still missing
				puts = append(puts, map[string]string{"sha256": object.SHA256, "url": base() + "/cas/" + object.SHA256 + query})
			}
		}
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"upload_id": "u1", "puts": puts})
	})
	mux.HandleFunc("PUT /cas/{digest}", func(w http.ResponseWriter, r *http.Request) {
		if f.expired(r) {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		body, _ := io.ReadAll(r.Body)
		sum := sha256.Sum256(body)
		if r.Header.Get("x-amz-checksum-sha256") != base64.StdEncoding.EncodeToString(sum[:]) ||
			hex.EncodeToString(sum[:]) != r.PathValue("digest") {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.objects[r.PathValue("digest")] = body
		f.mu.Unlock()
	})
	mux.HandleFunc("GET /cas/{digest}", func(w http.ResponseWriter, r *http.Request) {
		if f.expired(r) {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Write(f.objects[r.PathValue("digest")])
	})
	mux.HandleFunc("POST /api/v1/resources/r1/versions", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"id":"v1","state":"ready"}`)
	})
	mux.HandleFunc("GET /api/v1/resources/r1/versions/{v}/manifest", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Write(f.manifest)
	})
	mux.HandleFunc("POST /api/v1/resources/r1/versions/{v}/downloads", func(w http.ResponseWriter, r *http.Request) {
		var request struct{ SHA256 []string }
		json.NewDecoder(r.Body).Decode(&request)
		if len(request.SHA256) > downloadBatch {
			t.Errorf("downloads signed %d objects at once, want at most %d", len(request.SHA256), downloadBatch)
		}
		query := f.sign()
		urls := []string{}
		for _, digest := range request.SHA256 {
			urls = append(urls, base()+"/cas/"+digest+query)
		}
		json.NewEncoder(w).Encode(map[string]any{"urls": urls})
	})
	return mux
}

func TestPushPullRoundTripsBlockedFiles(t *testing.T) {
	testPushPullRoundTrip(t, 0)
}

// A transfer that outlives its presigned URLs signs again and finishes.
func TestPushPullSurviveExpiredURLs(t *testing.T) {
	testPushPullRoundTrip(t, 1)
}

func testPushPullRoundTrip(t *testing.T, urlLife int) {
	defer func(batch int) { downloadBatch = batch }(downloadBatch)
	downloadBatch = 2 // four objects, two downloads batches
	fake := &fakeResourceServer{objects: map[string][]byte{}, urlLife: urlLife}
	var server *httptest.Server
	server = httptest.NewServer(fake.handler(t, func() string { return server.URL }))
	defer server.Close()

	src := t.TempDir()
	big := make([]byte, 9<<20)
	for i := range big {
		big[i] = byte(i*7/4096 + i>>22*13) // distinct blocks
	}
	if err := os.WriteFile(filepath.Join(src, "big.bin"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "kib.bin"), big[5<<20:5<<20+1024], 0o755); err != nil {
		t.Fatal(err)
	}
	c := New(server.URL, "token")
	ctx := context.Background()
	if _, err := c.PushResource(ctx, src, "data"); err != nil {
		t.Fatal(err)
	}
	var m manifest.Manifest
	json.Unmarshal(fake.manifest, &m)
	if len(m.Entries[0].Blocks) != 3 || len(fake.objects) != 4 {
		t.Fatalf("stored %d objects for manifest %s, want 3 blocks and one small file", len(fake.objects), fake.manifest)
	}

	dst := filepath.Join(t.TempDir(), "out")
	if err := c.PullResource(ctx, "data", dst); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"big.bin", "kib.bin"} {
		want, _ := os.ReadFile(filepath.Join(src, name))
		got, err := os.ReadFile(filepath.Join(dst, name))
		if err != nil || string(got) != string(want) {
			t.Fatalf("%s: pulled %d bytes, want %d (err %v)", name, len(got), len(want), err)
		}
	}
	if info, _ := os.Stat(filepath.Join(dst, "kib.bin")); info.Mode().Perm()&0o100 == 0 {
		t.Fatal("kib.bin lost its executable bit")
	}
}
