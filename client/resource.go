package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"

	"github.com/zibyn/orrery/internal/castransfer"
	"github.com/zibyn/orrery/internal/manifest"
)

// PushResource publishes the tree at dir as a Resource Version of name,
// creating the Resource when it does not exist. It returns the published
// version (api.md §3).
func (c *Client) PushResource(ctx context.Context, dir, name string) (*ResourceVersion, error) {
	m, files, err := buildManifest(dir)
	if err != nil {
		return nil, err
	}
	resource, err := c.ensureResource(ctx, name)
	if err != nil {
		return nil, err
	}
	for {
		// Resubmitting the manifest resumes the upload: the server signs fresh
		// PUTs for the objects still missing (api.md §3).
		upload, err := c.CreateResourceUpload(ctx, resource.ID, m)
		if err != nil {
			return nil, err
		}
		expired, err := c.putObjects(ctx, upload.Puts, files)
		if err != nil {
			return nil, err
		}
		if !expired {
			return c.PublishResourceUpload(ctx, resource.ID, upload.UploadID)
		}
	}
}

// putObjects uploads puts in order. It reports expired, meaning sign again,
// when a URL is rejected after at least one object of this round went through,
// so each round makes progress; a fresh round's first rejection is an error.
func (c *Client) putObjects(ctx context.Context, puts []UploadPut, files map[string]manifest.Source) (expired bool, err error) {
	for i, put := range puts {
		source, ok := files[put.SHA256]
		if !ok {
			return false, fmt.Errorf("the server asked for an object this tree does not contain: %s", put.SHA256)
		}
		err := c.putObject(ctx, put.URL, source, put.SHA256)
		if i > 0 && errors.Is(err, castransfer.ErrForbidden) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
	}
	return false, nil
}

// downloadBatch is how many objects one downloads request signs.
var downloadBatch = 1000 // a var so tests can shrink it

// PullResource downloads a Resource Version into dest. ref is NAME or
// NAME@VERSION; a missing version means the current one (api.md §3).
func (c *Client) PullResource(ctx context.Context, ref, dest string) error {
	name, version := splitResourceVersion(ref)
	resource, err := c.ResolveResource(ctx, name)
	if err != nil {
		return err
	}
	if version == "" && resource.CurrentVersionID != nil {
		version = *resource.CurrentVersionID
	}
	if version == "" {
		return fmt.Errorf("Resource %q has no current version", resource.Name)
	}
	raw, err := c.ResourceManifest(ctx, resource.ID, version)
	if err != nil {
		return err
	}
	var m manifest.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("decode manifest: %w", err)
	}
	if err := manifest.Validate(m); err != nil {
		return err
	}
	// Objects in the order the loop below fetches them, each once.
	var digests []string
	position := map[string]int{}
	for _, entry := range m.Entries {
		if entry.Type != manifest.TypeFile {
			continue
		}
		blocks := entry.Blocks
		if blocks == nil {
			blocks = []string{entry.SHA256}
		}
		for _, digest := range blocks {
			if _, seen := position[digest]; !seen {
				position[digest] = len(digests)
				digests = append(digests, digest)
			}
		}
	}
	// URLs are signed a batch at a time just before use, so none outlives its
	// hour by much and no request outgrows the server's body limit.
	urls := map[string]string{}
	sign := func(from int) error {
		batch := digests[from:min(from+downloadBatch, len(digests))]
		downloads, err := c.CreateResourceDownloads(ctx, resource.ID, version, batch)
		if err != nil {
			return err
		}
		if len(downloads.URLs) != len(batch) {
			return fmt.Errorf("got %d download URLs for %d objects", len(downloads.URLs), len(batch))
		}
		clear(urls)
		for i, digest := range batch {
			urls[digest] = downloads.URLs[i]
		}
		return nil
	}
	get := func(dst io.Writer, digest string) error {
		for attempt := 0; ; attempt++ {
			if _, ok := urls[digest]; !ok {
				if err := sign(position[digest]); err != nil {
					return err
				}
			}
			err := castransfer.Get(ctx, c.transfers, urls[digest], dst, digest, 0)
			if attempt > 0 || !errors.Is(err, castransfer.ErrForbidden) {
				return err
			}
			// The batch expired; a 403 writes nothing, so sign again and retry.
			clear(urls)
		}
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	// Directories first so empty ones survive and file parents exist.
	for _, entry := range m.Entries {
		if entry.Type == manifest.TypeDir {
			if err := os.MkdirAll(filepath.Join(dest, filepath.FromSlash(entry.Path)), 0o755); err != nil {
				return err
			}
		}
	}
	for _, entry := range m.Entries {
		if entry.Type != manifest.TypeFile {
			continue
		}
		target := filepath.Join(dest, filepath.FromSlash(entry.Path))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if entry.Executable {
			mode = 0o755
		}
		if err := downloadFile(target, entry, get, mode); err != nil {
			return err
		}
	}
	for _, entry := range m.Entries {
		if entry.Type != manifest.TypeSymlink {
			continue
		}
		target := filepath.Join(dest, filepath.FromSlash(entry.Path))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.Symlink(entry.Target, target); err != nil {
			return err
		}
	}
	return nil
}

// ResolveResource accepts an id directly or lists pages until the name
// matches.
func (c *Client) ResolveResource(ctx context.Context, ref string) (*Resource, error) {
	if _, err := uuid.Parse(ref); err == nil {
		return c.GetResource(ctx, ref)
	}
	cursor := ""
	for {
		page, err := c.ListResources(ctx, 200, cursor)
		if err != nil {
			return nil, err
		}
		for _, item := range page.Items {
			if item.Name == ref {
				return &item, nil
			}
		}
		if page.NextCursor == nil {
			return nil, fmt.Errorf("no Resource named %q", ref)
		}
		cursor = *page.NextCursor
	}
}

// splitResourceVersion recognizes NAME@<version-uuid>; anything else is a
// Resource reference.
func splitResourceVersion(arg string) (string, string) {
	index := strings.LastIndexByte(arg, '@')
	if index <= 0 {
		return arg, ""
	}
	if _, err := uuid.Parse(arg[index+1:]); err != nil {
		return arg, ""
	}
	return arg[:index], arg[index+1:]
}

// ensureResource returns the named Resource, creating it when absent.
func (c *Client) ensureResource(ctx context.Context, name string) (*Resource, error) {
	if _, err := uuid.Parse(name); err != nil {
		// A name: reuse an existing Resource before creating one.
		if resource, err := c.ResolveResource(ctx, name); err == nil {
			return resource, nil
		}
		resource, err := c.CreateResource(ctx, name)
		if err == nil {
			return resource, nil
		}
		var problem *Error
		if !errors.As(err, &problem) || problem.Status != http.StatusConflict {
			return nil, err
		}
		// A concurrent create won the name; resolve it again below.
	}
	resource, err := c.ResolveResource(ctx, name)
	if err == nil {
		return resource, nil
	}
	// The argument may be a bare id that does not exist yet, which cannot be
	// created: names are the only handle POST /resources takes.
	if _, parseErr := uuid.Parse(name); parseErr == nil {
		return nil, err
	}
	return nil, fmt.Errorf("create Resource %q: %w", name, err)
}

// buildManifest walks root deterministically. Symlinks are recorded, never
// followed; devices, FIFOs and sockets are rejected.
func buildManifest(root string) (manifest.Manifest, map[string]manifest.Source, error) {
	info, err := os.Stat(root)
	if err != nil {
		return manifest.Manifest{}, nil, err
	}
	if !info.IsDir() {
		return manifest.Manifest{}, nil, fmt.Errorf("%s is not a directory", root)
	}
	dir, err := os.OpenRoot(root)
	if err != nil {
		return manifest.Manifest{}, nil, err
	}
	defer dir.Close()
	m, files, err := manifest.Walk(dir.FS(), ".")
	if err != nil {
		return manifest.Manifest{}, nil, err
	}
	// The walker reports filesystem paths; uploads open host paths.
	for digest, source := range files {
		source.Path = filepath.Join(root, filepath.FromSlash(source.Path))
		files[digest] = source
	}
	return m, files, nil
}

// putObject sends one CAS object, a whole file or one block of it, through a
// presigned URL. The store verifies x-amz-checksum-sha256 (ADR-0021).
func (c *Client) putObject(ctx context.Context, url string, source manifest.Source, digest string) error {
	f, err := os.Open(source.Path)
	if err != nil {
		return err
	}
	defer f.Close()
	return castransfer.Put(ctx, c.transfers, url, io.NewSectionReader(f, source.Offset, source.Size), source.Size, digest)
}

// downloadFile writes one file entry, fetching its blocks in order when it
// has them; each object is verified against its digest.
func downloadFile(path string, entry manifest.Entry, get func(io.Writer, string) error, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	digests := entry.Blocks
	if digests == nil {
		digests = []string{entry.SHA256}
	}
	for _, digest := range digests {
		if err := get(f, digest); err != nil {
			f.Close() //nolint:errcheck // report the transfer error instead
			return err
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}
