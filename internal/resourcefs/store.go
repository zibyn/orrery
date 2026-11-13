package resourcefs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zibyn/orrery/internal/manifest"
)

// Read constants (defaults.md, ADR-0030).
const (
	prefetchBlocks   = 4
	maxDownloads     = 200
	getTimeout       = 60 * time.Second
	getRetries       = 10
	presignBatch     = 1000
	urlLifetime      = time.Hour
	urlRenewalMargin = 10 * time.Minute
	// touchAfter bounds how often a cache hit refreshes the block's mtime,
	// the LRU clock eviction reads.
	touchAfter = 10 * time.Minute
)

// retryDelay is the pause before attempt n (n ≥ 1). A var so tests are fast.
var retryDelay = func(n int) time.Duration { return time.Duration(n) * 200 * time.Millisecond }

// Presigner turns object digests (files and blocks) or manifest addresses into
// presigned GET URLs, in order. The Agent implements it over Presign; the
// daemon never holds object-store credentials (ADR-0030).
type Presigner interface {
	Presign(ctx context.Context, objects, manifests []string) ([]string, error)
}

// blockRef is one unit of fetch and cache. key names the cache file; object
// is the CAS object read. A pre-ADR-0030 file over 4 MiB has no blocks: its
// refs are 4 MiB ranges of the whole object, cached unverified.
type blockRef struct {
	key            string
	object         string
	offset, length int64
	verify         bool
}

// refCount is how many blocks a file entry reads as.
func refCount(entry manifest.Entry) int {
	if size := *entry.Size; size > manifest.BlockSize {
		return manifest.BlockCount(size)
	}
	return 1
}

// refAt is block i of a file entry.
func refAt(entry manifest.Entry, i int) blockRef {
	size := *entry.Size
	switch {
	case entry.Blocks != nil:
		return blockRef{key: entry.Blocks[i], object: entry.Blocks[i], length: manifest.BlockLength(size, i), verify: true}
	case size <= manifest.BlockSize:
		return blockRef{key: entry.SHA256, object: entry.SHA256, length: size, verify: true}
	default:
		return blockRef{
			key: entry.SHA256 + "-" + strconv.Itoa(i), object: entry.SHA256,
			offset: int64(i) * manifest.BlockSize, length: manifest.BlockLength(size, i),
		}
	}
}

type call struct {
	done chan struct{}
	err  error
}

type signedURL struct {
	url     string
	expires time.Time
}

// store is the Node's block cache, <data-root>/cache/blocks/<key>.
type store struct {
	dir       string
	presigner Presigner
	http      *http.Client
	sem       chan struct{}

	mu       sync.Mutex
	inflight map[string]*call

	presignMu sync.Mutex // one Presign batch at a time; the next finds it cached
	urlMu     sync.Mutex
	urls      map[string]signedURL
}

func newStore(dir string, presigner Presigner) *store {
	return &store{
		dir: dir, presigner: presigner, http: &http.Client{},
		sem: make(chan struct{}, maxDownloads), inflight: map[string]*call{}, urls: map[string]signedURL{},
	}
}

func (s *store) path(key string) string { return filepath.Join(s.dir, key) }

// cleanTemp removes partial downloads a previous process left behind.
func (s *store) cleanTemp() error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	names, err := filepath.Glob(filepath.Join(s.dir, ".tmp-*"))
	if err != nil {
		return err
	}
	for _, name := range names {
		os.Remove(name) //nolint:errcheck // best effort
	}
	return nil
}

// ensure makes ref present in the cache. Concurrent callers for one block
// share a download. hint lists the blocks likely read next, to presign with
// ref in one batch.
func (s *store) ensure(ref blockRef, hint func() []blockRef) error {
	if info, err := os.Stat(s.path(ref.key)); err == nil {
		if time.Since(info.ModTime()) > touchAfter {
			now := time.Now()
			os.Chtimes(s.path(ref.key), now, now) //nolint:errcheck // LRU hint only
		}
		return nil
	}
	s.mu.Lock()
	if c, ok := s.inflight[ref.key]; ok {
		s.mu.Unlock()
		<-c.done
		return c.err
	}
	c := &call{done: make(chan struct{})}
	s.inflight[ref.key] = c
	s.mu.Unlock()
	c.err = s.download(ref, hint)
	close(c.done)
	s.mu.Lock()
	delete(s.inflight, ref.key)
	s.mu.Unlock()
	return c.err
}

// download tries once and retries getRetries times; a digest mismatch is a
// failed attempt. A failed URL is dropped so the retry signs a fresh one.
func (s *store) download(ref blockRef, hint func() []blockRef) error {
	var err error
	for attempt := 0; attempt <= getRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(retryDelay(attempt)) // without a download slot
		}
		var url string
		if url, err = s.url(ref, hint); err == nil {
			s.sem <- struct{}{}
			err = s.fetch(ref, url)
			<-s.sem
			if err == nil {
				return nil
			}
		}
		s.urlMu.Lock()
		delete(s.urls, ref.object)
		s.urlMu.Unlock()
	}
	return err
}

func (s *store) cachedURL(object string) (string, bool) {
	s.urlMu.Lock()
	defer s.urlMu.Unlock()
	signed, ok := s.urls[object]
	return signed.url, ok && time.Now().Before(signed.expires)
}

// url returns a presigned GET for ref.object, signing it together with the
// hinted blocks that are neither cached on disk nor signed yet.
func (s *store) url(ref blockRef, hint func() []blockRef) (string, error) {
	if url, ok := s.cachedURL(ref.object); ok {
		return url, nil
	}
	s.presignMu.Lock()
	defer s.presignMu.Unlock()
	if url, ok := s.cachedURL(ref.object); ok {
		return url, nil
	}
	batch := s.batch(ref, hint)
	ctx, cancel := context.WithTimeout(context.Background(), getTimeout)
	defer cancel()
	urls, err := s.presigner.Presign(ctx, batch, nil)
	if err != nil {
		return "", err
	}
	if len(urls) != len(batch) {
		return "", fmt.Errorf("presign returned %d URLs for %d objects", len(urls), len(batch))
	}
	expires := time.Now().Add(urlLifetime - urlRenewalMargin)
	s.urlMu.Lock()
	for i, object := range batch {
		s.urls[object] = signedURL{url: urls[i], expires: expires}
	}
	s.urlMu.Unlock()
	return urls[0], nil
}

// batch is ref.object followed by up to presignBatch-1 more objects from hint.
func (s *store) batch(ref blockRef, hint func() []blockRef) []string {
	batch := []string{ref.object}
	seen := map[string]bool{ref.object: true}
	if hint == nil {
		return batch
	}
	for _, next := range hint() {
		if len(batch) == presignBatch {
			break
		}
		if seen[next.object] {
			continue
		}
		seen[next.object] = true
		if _, ok := s.cachedURL(next.object); ok {
			continue
		}
		if _, err := os.Stat(s.path(next.key)); err == nil {
			continue
		}
		batch = append(batch, next.object)
	}
	return batch
}

// fetch GETs one block into a temporary file and renames it into place only
// when its length and, for verified refs, its SHA-256 match.
func (s *store) fetch(ref blockRef, url string) error {
	ctx, cancel := context.WithTimeout(context.Background(), getTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if !ref.verify {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", ref.offset, ref.offset+ref.length-1))
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck // read-only
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("fetch %s: %s", ref.key, resp.Status)
	}
	body := io.Reader(resp.Body)
	if !ref.verify && resp.StatusCode == http.StatusOK {
		// The store ignored Range and sent the whole object.
		if _, err := io.CopyN(io.Discard, body, ref.offset); err != nil {
			return err
		}
	}
	tmp, err := os.CreateTemp(s.dir, ".tmp-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) //nolint:errcheck // gone after a successful rename
	sum := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, sum), io.LimitReader(body, ref.length+1))
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	switch {
	case err != nil:
		return err
	case n != ref.length:
		return fmt.Errorf("block %s: got %d bytes, want %d", ref.key, n, ref.length)
	case ref.verify && hex.EncodeToString(sum.Sum(nil)) != ref.object:
		return fmt.Errorf("block %s: %w", ref.key, errDigestMismatch)
	}
	return os.Rename(tmp.Name(), s.path(ref.key))
}

var errDigestMismatch = errors.New("digest mismatch")

// readBlock copies ref's bytes [from, from+len(dst)) out of the cache.
func (s *store) readBlock(ref blockRef, from int64, dst []byte) error {
	f, err := os.Open(s.path(ref.key))
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck // read-only
	n, err := f.ReadAt(dst, from)
	if n == len(dst) {
		return nil
	}
	if err == nil || errors.Is(err, io.EOF) {
		err = fmt.Errorf("block %s is short", ref.key)
	}
	return err
}

// validAddress keeps an address usable as a path segment.
func validAddress(address string) bool {
	if len(address) != 64 {
		return false
	}
	return strings.Trim(address, "0123456789abcdef") == ""
}
