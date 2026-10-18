package cli

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/google/uuid"

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
		return c.sendAndPrint(http.MethodPost, "/api/v1/resources", map[string]string{"name": fs.Arg(0)}, "")
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
		return c.sendAndPrint(http.MethodDelete, "/api/v1/resources/"+resource.ID, nil, "")
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
		version, _, err := getResourceVersion(c, resource, "")
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

// resourceRef is the id and current pointer of a Resource.
type resourceRef struct {
	ID               string  `json:"id"`
	Name             string  `json:"name"`
	CurrentVersionID *string `json:"current_version_id"`
}

type resourceVersionRef struct {
	ID         string  `json:"id"`
	ResourceID string  `json:"resource_id"`
	Address    *string `json:"address"`
	Size       int64   `json:"size"`
	State      string  `json:"state"`
	CreatedAt  string  `json:"created_at"`
}

// resolveResource accepts an id directly or lists pages until the name matches.
func resolveResource(c *client, arg string) (resourceRef, error) {
	if _, err := uuid.Parse(arg); err == nil {
		data, err := c.do(http.MethodGet, "/api/v1/resources/"+arg, nil, nil, "")
		if err != nil {
			return resourceRef{}, err
		}
		var resource resourceRef
		return resource, json.Unmarshal(data, &resource)
	}
	cursor := ""
	for {
		query := url.Values{"limit": {"200"}}
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		data, err := c.do(http.MethodGet, "/api/v1/resources", query, nil, "")
		if err != nil {
			return resourceRef{}, err
		}
		var page struct {
			Items      []resourceRef `json:"items"`
			NextCursor *string       `json:"next_cursor"`
		}
		if err := json.Unmarshal(data, &page); err != nil {
			return resourceRef{}, fmt.Errorf("decode Resource list: %w", err)
		}
		for _, item := range page.Items {
			if item.Name == arg {
				return item, nil
			}
		}
		if page.NextCursor == nil {
			return resourceRef{}, fmt.Errorf("no Resource named %q", arg)
		}
		cursor = *page.NextCursor
	}
}

func listResources(c *client, limit int, cursor string) error {
	query := url.Values{}
	if limit > 0 {
		query.Set("limit", fmt.Sprint(limit))
	}
	if cursor != "" {
		query.Set("cursor", cursor)
	}
	data, err := c.do(http.MethodGet, "/api/v1/resources", query, nil, "")
	if err != nil {
		return err
	}
	var page struct {
		Items []struct {
			ID               string  `json:"id"`
			Name             string  `json:"name"`
			CurrentVersionID *string `json:"current_version_id"`
			CreatedAt        string  `json:"created_at"`
		} `json:"items"`
		NextCursor *string `json:"next_cursor"`
	}
	if err := json.Unmarshal(data, &page); err != nil {
		return fmt.Errorf("decode Resource list: %w", err)
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
	data, err := c.do(http.MethodGet, "/api/v1/resources/"+resource.ID+"/versions", url.Values{"limit": {"200"}}, nil, "")
	if err != nil {
		return err
	}
	var page struct {
		Items      []resourceVersionRef `json:"items"`
		NextCursor *string              `json:"next_cursor"`
	}
	if err := json.Unmarshal(data, &page); err != nil {
		return fmt.Errorf("decode ResourceVersion list: %w", err)
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
func getResourceVersion(c *client, resource resourceRef, version string) (json.RawMessage, resourceVersionRef, error) {
	if version == "" {
		if resource.CurrentVersionID == nil {
			return nil, resourceVersionRef{}, fmt.Errorf("Resource %q has no current version", resource.Name)
		}
		version = *resource.CurrentVersionID
	}
	data, err := c.do(http.MethodGet, "/api/v1/resources/"+resource.ID+"/versions/"+version, nil, nil, "")
	if err != nil {
		return nil, resourceVersionRef{}, err
	}
	var ref resourceVersionRef
	if err := json.Unmarshal(data, &ref); err != nil {
		return nil, resourceVersionRef{}, fmt.Errorf("decode ResourceVersion: %w", err)
	}
	return data, ref, nil
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
func ensureResource(c *client, name string) (resourceRef, error) {
	if _, err := uuid.Parse(name); err != nil {
		// A name: reuse an existing Resource before creating one.
		if resource, err := resolveResource(c, name); err == nil {
			return resource, nil
		}
		status, data, err := c.send(http.MethodPost, "/api/v1/resources", nil, map[string]string{"name": name}, "")
		if err != nil {
			return resourceRef{}, err
		}
		if status == http.StatusCreated || status == http.StatusOK {
			var resource resourceRef
			return resource, json.Unmarshal(data, &resource)
		}
		if status != http.StatusConflict {
			return resourceRef{}, problemError(status, data)
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
		return resourceRef{}, err
	}
	return resourceRef{}, fmt.Errorf("create Resource %q: %w", name, err)
}

// buildManifest walks root deterministically. Symlinks are recorded, never
// followed; devices, FIFOs and sockets are rejected. files maps each digest
// to a local path for the upload PUTs.
func buildManifest(root string) (manifest.Manifest, map[string]string, error) {
	info, err := os.Stat(root)
	if err != nil {
		return manifest.Manifest{}, nil, err
	}
	if !info.IsDir() {
		return manifest.Manifest{}, nil, fmt.Errorf("%s is not a directory", root)
	}
	entries := []manifest.Entry{}
	files := map[string]string{}
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(relative)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			entries = append(entries, manifest.Entry{Path: name, Type: manifest.TypeSymlink, Target: target})
		case entry.IsDir():
			entries = append(entries, manifest.Entry{Path: name, Type: manifest.TypeDir})
		case info.Mode().IsRegular():
			if info.Size() > manifest.MaxFileSize {
				return fmt.Errorf("%s is larger than 5 GiB", name)
			}
			digest, err := hashFile(path)
			if err != nil {
				return err
			}
			size := info.Size()
			files[digest] = path
			entries = append(entries, manifest.Entry{
				Path: name, Type: manifest.TypeFile, Size: &size, SHA256: digest,
				Executable: info.Mode().Perm()&0o111 != 0,
			})
		default:
			return fmt.Errorf("%s is not a regular file, directory or symlink", name)
		}
		return nil
	})
	if err != nil {
		return manifest.Manifest{}, nil, err
	}
	normalized, err := manifest.Normalize(manifest.Manifest{Entries: entries})
	if err != nil {
		return manifest.Manifest{}, nil, err
	}
	return normalized, files, nil
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
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
	data, err := c.do(http.MethodPost, "/api/v1/resources/"+resource.ID+"/uploads", nil, m, "")
	if err != nil {
		return err
	}
	var upload struct {
		UploadID string `json:"upload_id"`
		Puts     []struct {
			SHA256 string `json:"sha256"`
			URL    string `json:"url"`
		} `json:"puts"`
	}
	if err := json.Unmarshal(data, &upload); err != nil {
		return fmt.Errorf("decode upload session: %w", err)
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
	version, err := c.do(http.MethodPost, "/api/v1/resources/"+resource.ID+"/versions", nil, map[string]string{
		"upload_id": upload.UploadID,
	}, "")
	if err != nil {
		return err
	}
	return c.print(version)
}

// putObject sends one CAS object through a presigned URL. The store verifies
// x-amz-checksum-sha256 (ADR-0021).
func putObject(url, path, digest string) error {
	raw, err := hex.DecodeString(digest)
	if err != nil || len(raw) != sha256.Size {
		return fmt.Errorf("invalid sha256 %q", digest)
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	req, err := http.NewRequest(http.MethodPut, url, f)
	if err != nil {
		return err
	}
	req.ContentLength = fileSize(f)
	req.Header.Set("x-amz-checksum-sha256", base64.StdEncoding.EncodeToString(raw))
	resp, err := transferClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("upload %s: HTTP %d: %s", digest, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
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
	raw, err := c.do(http.MethodGet, "/api/v1/resources/"+resource.ID+"/versions/"+version+"/manifest", nil, nil, "")
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
	data, err := c.do(http.MethodPost, "/api/v1/resources/"+resource.ID+"/versions/"+version+"/downloads", nil, map[string]any{
		"sha256": digests,
	}, "")
	if err != nil {
		return err
	}
	var downloads struct {
		URLs []string `json:"urls"`
	}
	if err := json.Unmarshal(data, &downloads); err != nil {
		return fmt.Errorf("decode download URLs: %w", err)
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
	resp, err := transferClient.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %d", digest, resp.StatusCode)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	sum := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, sum), resp.Body); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if got := hex.EncodeToString(sum.Sum(nil)); got != digest {
		return fmt.Errorf("%s downloaded with sha256 %s", path, got)
	}
	return os.Chmod(path, mode)
}
