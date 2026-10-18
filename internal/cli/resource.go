package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/google/uuid"

	starsclient "github.com/zibyn/stars-sandbox/client"
	"github.com/zibyn/stars-sandbox/internal/castransfer"
	"github.com/zibyn/stars-sandbox/internal/manifest"
)

func runResource(c *client, args []string, stderr io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: stars-sandbox resource create|ls|rm|versions|current|push|pull")
	}
	switch args[0] {
	case "create":
		fs := flag.NewFlagSet("resource create", flag.ContinueOnError)
		fs.SetOutput(stderr)
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return fmt.Errorf("usage: stars-sandbox resource create <name>")
		}
		resource, err := c.CreateResource(ctx, fs.Arg(0))
		if err != nil {
			return err
		}
		return c.print(resource)
	case "ls":
		fs := flag.NewFlagSet("resource ls", flag.ContinueOnError)
		fs.SetOutput(stderr)
		limit := fs.Int("limit", 0, "page size (1-200)")
		cursor := fs.String("cursor", "", "page cursor")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return fmt.Errorf("usage: stars-sandbox resource ls")
		}
		return listResources(c, *limit, *cursor)
	case "rm":
		if len(args) != 2 {
			return fmt.Errorf("usage: stars-sandbox resource rm <name|id>")
		}
		resource, err := resolveResource(c, args[1])
		if err != nil {
			return err
		}
		deleted, err := c.DeleteResource(ctx, resource.ID)
		if err != nil {
			return err
		}
		return c.print(deleted)
	case "versions":
		if len(args) != 2 {
			return fmt.Errorf("usage: stars-sandbox resource versions <name|id>")
		}
		return listResourceVersions(c, args[1])
	case "current":
		if len(args) != 2 {
			return fmt.Errorf("usage: stars-sandbox resource current <name|id>")
		}
		resource, err := resolveResource(c, args[1])
		if err != nil {
			return err
		}
		version, err := getResourceVersion(c, resource, "")
		if err != nil {
			return err
		}
		return c.print(version)
	case "push":
		if len(args) != 3 {
			return fmt.Errorf("usage: stars-sandbox resource push <dir> <name>")
		}
		return pushResource(c, args[1], args[2])
	case "pull":
		if len(args) != 3 {
			return fmt.Errorf("usage: stars-sandbox resource pull <name>[@version] <dir>")
		}
		return pullResource(c, args[1], args[2])
	default:
		return fmt.Errorf("unknown resource command %q", args[0])
	}
}

// resolveResource accepts an id directly or lists pages until the name matches.
func resolveResource(c *client, arg string) (*starsclient.Resource, error) {
	if _, err := uuid.Parse(arg); err == nil {
		return c.GetResource(ctx, arg)
	}
	cursor := ""
	for {
		page, err := c.ListResources(ctx, 200, cursor)
		if err != nil {
			return nil, err
		}
		for _, item := range page.Items {
			if item.Name == arg {
				return &item, nil
			}
		}
		if page.NextCursor == nil {
			return nil, fmt.Errorf("no Resource named %q", arg)
		}
		cursor = *page.NextCursor
	}
}

func listResources(c *client, limit int, cursor string) error {
	page, err := c.ListResources(ctx, limit, cursor)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(c.out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tCURRENT\tCREATED")
	for _, item := range page.Items {
		current := "-"
		if item.CurrentVersionID != nil {
			current = *item.CurrentVersionID
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", item.ID, item.Name, current, item.CreatedAt)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if page.NextCursor != nil {
		fmt.Fprintln(c.out, "next_cursor:", *page.NextCursor)
	}
	return nil
}

func listResourceVersions(c *client, ref string) error {
	resource, err := resolveResource(c, ref)
	if err != nil {
		return err
	}
	page, err := c.ListResourceVersions(ctx, resource.ID, 200, "")
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(c.out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tADDRESS\tSIZE\tSTATE\tCREATED")
	for _, item := range page.Items {
		marker := " "
		if resource.CurrentVersionID != nil && *resource.CurrentVersionID == item.ID {
			marker = "*"
		}
		address := "-"
		if item.Address != nil {
			address = *item.Address
		}
		fmt.Fprintf(tw, "%s%s\t%s\t%d\t%s\t%s\n", marker, item.ID, address, item.Size, item.State, item.CreatedAt)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if page.NextCursor != nil {
		fmt.Fprintln(c.out, "next_cursor:", *page.NextCursor)
	}
	return nil
}

// getResourceVersion resolves an explicit version, or the current one.
func getResourceVersion(c *client, resource *starsclient.Resource, version string) (*starsclient.ResourceVersion, error) {
	if version == "" {
		if resource.CurrentVersionID == nil {
			return nil, fmt.Errorf("Resource %q has no current version", resource.Name)
		}
		version = *resource.CurrentVersionID
	}
	return c.GetResourceVersion(ctx, resource.ID, version)
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
func ensureResource(c *client, name string) (*starsclient.Resource, error) {
	if _, err := uuid.Parse(name); err != nil {
		// A name: reuse an existing Resource before creating one.
		if resource, err := resolveResource(c, name); err == nil {
			return resource, nil
		}
		resource, err := c.CreateResource(ctx, name)
		if err == nil {
			return resource, nil
		}
		var problem *starsclient.Error
		if !errors.As(err, &problem) || problem.Status != http.StatusConflict {
			return nil, err
		}
		// A concurrent create won the name; resolve it again below.
	}
	resource, err := resolveResource(c, name)
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

func pushResource(c *client, dir, name string) error {
	m, files, err := buildManifest(dir)
	if err != nil {
		return err
	}
	resource, err := ensureResource(c, name)
	if err != nil {
		return err
	}
	upload, err := c.CreateResourceUpload(ctx, resource.ID, m)
	if err != nil {
		return err
	}
	for _, put := range upload.Puts {
		path, ok := files[put.SHA256]
		if !ok {
			return fmt.Errorf("the server asked for an object this tree does not contain: %s", put.SHA256)
		}
		if err := putObject(put.URL, path, put.SHA256); err != nil {
			return err
		}
	}
	version, err := c.PublishResourceUpload(ctx, resource.ID, upload.UploadID)
	if err != nil {
		return err
	}
	return c.print(version)
}

// putObject sends one CAS object through a presigned URL. The store verifies
// x-amz-checksum-sha256 (ADR-0021).
func putObject(url, path, digest string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return castransfer.Put(context.Background(), transferClient, url, f, fileSize(f), digest)
}

func fileSize(f *os.File) int64 {
	info, err := f.Stat()
	if err != nil {
		return 0
	}
	return info.Size()
}

func pullResource(c *client, ref, dest string) error {
	name, version := splitResourceVersion(ref)
	resource, err := resolveResource(c, name)
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
		if err := downloadObject(target, urls[entry.SHA256], entry.SHA256, mode); err != nil {
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

func downloadObject(path, url, digest string, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if err := castransfer.Get(context.Background(), transferClient, url, f, digest, 0); err != nil {
		f.Close() //nolint:errcheck // report the transfer error instead
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}
