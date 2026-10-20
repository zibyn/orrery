//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zibyn/orrery/internal/objectstore"
)

// TestObjectStoreChecksum is the object-store half of #84's acceptance:
// a presigned PUT with the wrong x-amz-checksum-sha256 is rejected, the right
// bytes succeed, HeadSHA256 returns the digest, and a presigned GET round-trips.
func TestObjectStoreChecksum(t *testing.T) {
	store := e2eObjectStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	content := []byte("orrery object store e2e " + fmt.Sprint(time.Now().UnixNano()))
	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])
	owner := fmt.Sprintf("e2e-owner-%d", time.Now().UnixNano())

	if _, err := store.HeadSHA256(ctx, owner, digest); !errors.Is(err, objectstore.ErrNotFound) {
		t.Fatalf("HeadSHA256 before upload = %v, want ErrNotFound", err)
	}

	putURL, err := store.PresignPutURL(ctx, owner, digest)
	if err != nil {
		t.Fatal(err)
	}
	// Wrong declared digest: the signature covers x-amz-checksum-sha256, so the
	// store rejects the write.
	wrong := sha256.Sum256([]byte("not the content"))
	status, body := putObject(t, putURL, content, base64.StdEncoding.EncodeToString(wrong[:]))
	if status >= 200 && status < 300 {
		t.Fatalf("PUT with a wrong checksum succeeded: %d %s", status, body)
	}
	if _, err := store.HeadSHA256(ctx, owner, digest); !errors.Is(err, objectstore.ErrNotFound) {
		t.Fatalf("a rejected PUT left an object behind: %v", err)
	}

	status, body = putObject(t, putURL, content, base64.StdEncoding.EncodeToString(sum[:]))
	if status < 200 || status >= 300 {
		t.Fatalf("PUT with the correct checksum: %d %s", status, body)
	}

	got, err := store.HeadSHA256(ctx, owner, digest)
	if err != nil {
		t.Fatalf("HeadSHA256 after upload: %v", err)
	}
	if got != digest {
		t.Fatalf("HeadSHA256 = %q, want %q", got, digest)
	}

	getURL, err := store.PresignGetURL(ctx, owner, digest)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(getURL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("presigned GET: %d", resp.StatusCode)
	}
	roundTrip, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(roundTrip, content) {
		t.Fatalf("GET returned %q, want %q", roundTrip, content)
	}
}

func e2eObjectStore(t *testing.T) *objectstore.Store {
	t.Helper()
	endpoint := os.Getenv("ORRERY_E2E_S3_ENDPOINT")
	bucket := os.Getenv("ORRERY_E2E_S3_BUCKET")
	access := os.Getenv("ORRERY_E2E_S3_ACCESS_KEY")
	secret := os.Getenv("ORRERY_E2E_S3_SECRET_KEY")
	if endpoint == "" || bucket == "" || access == "" || secret == "" {
		t.Fatal("ORRERY_E2E_S3_* is not set; run ./scripts/e2e.sh")
	}
	store, err := objectstore.New(context.Background(), objectstore.Config{
		Endpoint: endpoint, PublicEndpoint: endpoint,
		Bucket: bucket, AccessKey: access, SecretKey: secret,
	})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func putObject(t *testing.T, url string, content []byte, checksum string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("x-amz-checksum-sha256", checksum)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, strings.TrimSpace(string(body))
}
