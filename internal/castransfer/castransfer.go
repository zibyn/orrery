// Package castransfer moves CAS objects through presigned URLs. The Control
// Plane signs the URL with the object's checksum, so the writer declares it and
// the reader verifies what it received (ADR-0021).
package castransfer

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Put streams body to a presigned PUT URL, declaring digest as the object's
// SHA-256 checksum.
func Put(ctx context.Context, client *http.Client, url string, body io.Reader, size int64, digest string) error {
	raw, err := hex.DecodeString(digest)
	if err != nil || len(raw) != sha256.Size {
		return fmt.Errorf("invalid sha256 %q", digest)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, body)
	if err != nil {
		return err
	}
	req.ContentLength = size
	req.Header.Set("x-amz-checksum-sha256", base64.StdEncoding.EncodeToString(raw))
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("upload %s: HTTP %d: %s", digest, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return nil
}

// Get streams a presigned GET into dst and rejects bytes whose SHA-256 differs
// from digest. A positive limit rejects an object larger than limit bytes; 0
// means no limit.
func Get(ctx context.Context, client *http.Client, url string, dst io.Writer, digest string, limit int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch %s: %s", digest, resp.Status)
	}
	hasher := sha256.New()
	source := io.Reader(resp.Body)
	if limit > 0 {
		source = io.LimitReader(resp.Body, limit+1)
	}
	n, err := io.Copy(io.MultiWriter(dst, hasher), source)
	if err != nil {
		return err
	}
	if limit > 0 && n > limit {
		return fmt.Errorf("object %s is larger than %d bytes", digest, limit)
	}
	if got := hex.EncodeToString(hasher.Sum(nil)); got != digest {
		return fmt.Errorf("object %s has sha256 %s", digest, got)
	}
	return nil
}
