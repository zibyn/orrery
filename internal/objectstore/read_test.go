package objectstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The Control Plane reads a CAS object itself over the internal endpoint
// and verifies it: a presigned URL is signed for the Node-facing public
// endpoint, which the Control Plane's own container may not reach (in e2e
// it is 127.0.0.1 on the host).
func TestGetObjectReadsTheInternalEndpointAndVerifies(t *testing.T) {
	content := []byte("hello")
	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])
	var paths []string
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path == "/orrery/cas/owner/sha256/"+digest {
			_, _ = w.Write(content)
			return
		}
		_, _ = w.Write([]byte("tampered"))
	}))
	defer internal.Close()
	store, err := New(context.Background(), Config{
		Endpoint: internal.URL, PublicEndpoint: "http://127.0.0.1:1",
		Bucket: "orrery", AccessKey: "access", SecretKey: "secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.GetObject(context.Background(), "owner", digest)
	if err != nil || string(got) != "hello" {
		t.Fatalf("GetObject = %q, %v", got, err)
	}
	if len(paths) != 1 {
		t.Fatalf("internal endpoint saw %v", paths)
	}
	other := hex.EncodeToString(make([]byte, 32))
	if _, err := store.GetObject(context.Background(), "owner", other); err == nil {
		t.Fatal("an object whose bytes do not match its digest was accepted")
	}
}
