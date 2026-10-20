package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	upload, err := c.CreateResourceUpload(ctx, resource.ID, m)
	if err != nil {
		return nil, err
	}
	for _, put := range upload.Puts {
		path, ok := files[put.SHA256]
		if !ok {
			return nil, fmt.Errorf("the server asked for an object this tree does not contain: %s", put.SHA256)
		}
		if err := c.putObject(ctx, put.URL, path, put.SHA256); err != nil {
			return nil, err
		}
	}
	return c.PublishResourceUpload(ctx, resource.ID, upload.UploadID)
}

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
	objects := manifest.FileObjects(m)
	digests := make([]string, 0, len(objects))
	for _, object := range objects {
		digests = append(digests, object.SHA256)
	}
	downloads, err := c.CreateResourceDownloads(ctx, resource.ID, version, digests)
	if err != nil {
		return err
	}
	if len(downloads.URLs) != len(digests) {
		return fmt.Errorf("got %d download URLs for %d objects", len(downloads.URLs), len(digests))
	}
	urls := make(map[string]string, len(digests))
	for i, digest := range digests {
		urls[digest] = downloads.URLs[i]
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
		if err := c.downloadObject(ctx, target, urls[entry.SHA256], entry.SHA256, mode); err != nil {
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
func buildManifest(root string) (manifest.Manifest, map[string]string, error) {
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
	for digest, fsPath := range files {
		files[digest] = filepath.Join(root, filepath.FromSlash(fsPath))
	}
	return m, files, nil
}

// putObject sends one CAS object through a presigned URL. The store verifies
// x-amz-checksum-sha256 (ADR-0021).
func (c *Client) putObject(ctx context.Context, url, path, digest string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return castransfer.Put(ctx, c.transfers, url, f, fileSize(f), digest)
}

func fileSize(f *os.File) int64 {
	info, err := f.Stat()
	if err != nil {
		return 0
	}
	return info.Size()
}

func (c *Client) downloadObject(ctx context.Context, path, url, digest string, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if err := castransfer.Get(ctx, c.transfers, url, f, digest, 0); err != nil {
		f.Close() //nolint:errcheck // report the transfer error instead
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}
