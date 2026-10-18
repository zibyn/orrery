package objectstore

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"net/url"
	"strings"
	"testing"
)

func TestKeys(t *testing.T) {
	const owner = "0192f3a4-0000-7000-8000-000000000001"
	if got, want := CASKey(owner, "abc123"), "cas/"+owner+"/sha256/abc123"; got != want {
		t.Fatalf("CASKey = %q, want %q", got, want)
	}
	if got, want := ManifestKey(owner, "sha256:deadbeef"), "manifests/"+owner+"/sha256:deadbeef"; got != want {
		t.Fatalf("ManifestKey = %q, want %q", got, want)
	}
}

// A presigned PUT must carry x-amz-checksum-sha256 as a signed header, so the
// store rejects bytes that do not match (ADR-0021). This is checked offline:
// presigning makes no request.
func TestPresignPutSignsChecksum(t *testing.T) {
	store, err := New(context.Background(), Config{
		Endpoint:  "http://rustfs:9000",
		Bucket:    "stars",
		AccessKey: "access",
		SecretKey: "secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	const digest = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
	raw, err := store.PresignPutURL(context.Background(), "owner", digest)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := u.Path, "/stars/cas/owner/sha256/"+digest; got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
	signed := u.Query().Get("X-Amz-SignedHeaders")
	if !strings.Contains(strings.ToLower(signed), "x-amz-checksum-sha256") {
		t.Fatalf("signed headers = %q, want x-amz-checksum-sha256 included", signed)
	}
	if got, want := u.Query().Get("X-Amz-Expires"), "3600"; got != want {
		t.Fatalf("expiry = %q, want %q", got, want)
	}
	// The checksum belongs in a header, never a query parameter.
	if got := u.Query().Get("x-amz-checksum-sha256"); got != "" {
		t.Fatalf("checksum leaked into the query string: %q", got)
	}
	rawDigest, err := hex.DecodeString(digest)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := base64.StdEncoding.EncodeToString(rawDigest), "n4bQgYhMfWWaL+qgxVrQFaO/TxsrC4Is0V1sFbDwCgg="; got != want {
		t.Fatalf("header value would be %q, want %q", got, want)
	}
}

func TestPresignGetUsesPublicEndpoint(t *testing.T) {
	store, err := New(context.Background(), Config{
		Endpoint:       "http://rustfs:9000",
		PublicEndpoint: "http://127.0.0.1:19000",
		Bucket:         "stars",
		AccessKey:      "access",
		SecretKey:      "secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := store.PresignGetURL(context.Background(), "owner", "abc")
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := u.Scheme+"://"+u.Host, "http://127.0.0.1:19000"; got != want {
		t.Fatalf("presigned host = %q, want the Node-reachable public endpoint %q", got, want)
	}
	if got, want := u.Path, "/stars/cas/owner/sha256/abc"; got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
	if got := u.Query().Get("X-Amz-Expires"); got != "3600" {
		t.Fatalf("expiry = %q, want 3600 (defaults.md, 1 hour)", got)
	}
}

func TestManifestPresign(t *testing.T) {
	store, err := New(context.Background(), Config{Endpoint: "http://127.0.0.1:19000", Bucket: "stars", AccessKey: "a", SecretKey: "s"})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := store.PresignManifestGetURL(context.Background(), "owner", "addr")
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(raw)
	if got, want := u.Path, "/stars/manifests/owner/addr"; got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
}
