package castransfer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// An empty object is uploaded with Content-Length: 0, never chunked: S3
// refuses a chunked PUT without a length, which stalled commits of any empty
// file. The body may be an empty open file, as the commit path passes.
func TestPutEmptyObjectSendsAZeroLength(t *testing.T) {
	sum := sha256.Sum256(nil)
	digest := hex.EncodeToString(sum[:])
	var chunked bool
	var length string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunked = len(r.TransferEncoding) > 0
		length = r.Header.Get("Content-Length")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for name, body := range map[string]io.Reader{"file": f, "buffer": bytes.NewBuffer(nil)} {
		if err := Put(context.Background(), server.Client(), server.URL, body, 0, digest); err != nil {
			t.Fatal(err)
		}
		if chunked || length != "0" {
			t.Fatalf("%s: chunked = %v, Content-Length = %q, want 0 and not chunked", name, chunked, length)
		}
	}
}
