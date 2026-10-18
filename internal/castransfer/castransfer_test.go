package castransfer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPutDeclaresChecksum(t *testing.T) {
	content := []byte("payload")
	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])
	var gotChecksum string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotChecksum = r.Header.Get("x-amz-checksum-sha256")
		body, _ := io.ReadAll(r.Body)
		if !bytes.Equal(body, content) {
			t.Errorf("uploaded %q, want %q", body, content)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	if err := Put(context.Background(), server.Client(), server.URL,
		bytes.NewReader(content), int64(len(content)), digest); err != nil {
		t.Fatal(err)
	}
	want := base64.StdEncoding.EncodeToString(sum[:])
	if gotChecksum != want {
		t.Fatalf("x-amz-checksum-sha256 = %q, want %q", gotChecksum, want)
	}
}

func TestGetVerifiesAndLimits(t *testing.T) {
	content := []byte("payload")
	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])

	cases := []struct {
		name    string
		body    []byte
		digest  string
		limit   int64
		wantErr bool
	}{
		{"matching digest", content, digest, 0, false},
		{"mismatched digest", content, "00" + digest[2:], 0, true},
		{"over the size limit", content, digest, int64(len(content) - 1), true},
		{"within the size limit", content, digest, int64(len(content)), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write(c.body)
			}))
			defer server.Close()
			var out bytes.Buffer
			err := Get(context.Background(), server.Client(), server.URL, &out, c.digest, c.limit)
			if (err != nil) != c.wantErr {
				t.Fatalf("Get error = %v, wantErr %v", err, c.wantErr)
			}
		})
	}
}
