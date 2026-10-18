package agent

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"

	nodev1 "github.com/zibyn/stars-sandbox/gen/node/v1"
	"github.com/zibyn/stars-sandbox/internal/manifest"
)

// publishChunkEntries bounds one SubmitManifest message (well under gRPC's
// default 4 MiB frame limit).
const publishChunkEntries = 500

// presignBatch bounds one Presign request.
const presignBatch = 500

// publishHTTP uploads CAS objects through presigned URLs. Like the CLI's
// transfer client it has no timeout: a legitimate object can be 5 GiB.
var publishHTTP = &http.Client{}

// publishWorkspace walks this Node's copy of a Workspace and publishes it
// (node-protocol.md "Workspace publish"). No Sandbox is involved: the Agent
// reads the host directory. The command is redelivered after a reconnect, so
// every step is repeatable.
func (r *Runtime) publishWorkspace(ctx context.Context, cmd *nodev1.PublishWorkspace) error {
	workspaceID, err := uuid.Parse(cmd.GetWorkspaceId())
	if err != nil {
		return fmt.Errorf("invalid workspace id: %w", err)
	}
	root, err := os.OpenRoot(workspacePath(r.dataRoot, workspaceID))
	if err != nil {
		return err
	}
	defer root.Close() //nolint:errcheck // read-only handle

	m, files, err := walkWorkspaceManifest(root, cmd.GetPath())
	if err != nil {
		return err
	}
	missing, err := r.submitManifest(ctx, cmd.GetVersionId(), m)
	if err != nil {
		return err
	}
	return r.uploadPublishObjects(ctx, root, missing, files)
}

// walkWorkspaceManifest builds the manifest rooted at publishPath inside the
// Workspace. Symlinks are recorded and never followed, empty directories and
// the executable bit survive, and devices/FIFOs/sockets are rejected
// (ADR-0012). WalkDir never follows links.
func walkWorkspaceManifest(root *os.Root, publishPath string) (manifest.Manifest, map[string]string, error) {
	base, err := localPath(publishPath)
	if err != nil {
		return manifest.Manifest{}, nil, err
	}
	info, err := root.Lstat(base)
	if err != nil {
		return manifest.Manifest{}, nil, err
	}
	baseIsDir := info.IsDir()
	fsys := root.FS()
	entries := []manifest.Entry{}
	files := map[string]string{}
	err = fs.WalkDir(fsys, base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == base && baseIsDir {
			return nil
		}
		relative := p
		if base != "." {
			relative = strings.TrimPrefix(p, base)
			relative = strings.TrimPrefix(relative, "/")
		}
		if relative == "" {
			// The published path is a single file or symlink.
			relative = filepath.Base(base)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			target, err := root.Readlink(p)
			if err != nil {
				return err
			}
			entries = append(entries, manifest.Entry{
				Path: filepath.ToSlash(relative), Type: manifest.TypeSymlink, Target: target,
			})
		case info.IsDir():
			entries = append(entries, manifest.Entry{
				Path: filepath.ToSlash(relative), Type: manifest.TypeDir,
			})
		case info.Mode().IsRegular():
			if info.Size() > manifest.MaxFileSize {
				return fmt.Errorf("%s: %w", relative, manifest.ErrTooLarge)
			}
			digest, err := hashRootFile(fsys, p)
			if err != nil {
				return err
			}
			size := info.Size()
			if _, exists := files[digest]; !exists {
				files[digest] = p
			}
			entries = append(entries, manifest.Entry{
				Path: filepath.ToSlash(relative), Type: manifest.TypeFile, Size: &size, SHA256: digest,
				Executable: info.Mode().Perm()&0o111 != 0,
			})
		default:
			return fmt.Errorf("%s is not a regular file, directory or symlink", relative)
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

func hashRootFile(fsys fs.FS, name string) (string, error) {
	f, err := fsys.Open(name)
	if err != nil {
		return "", err
	}
	defer f.Close() //nolint:errcheck // read-only
	sum := sha256.New()
	n, err := io.Copy(sum, io.LimitReader(f, manifest.MaxFileSize+1))
	if err != nil {
		return "", err
	}
	if n > manifest.MaxFileSize {
		return "", manifest.ErrTooLarge
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

// submitManifest streams the tree and returns the objects the store lacks.
func (r *Runtime) submitManifest(ctx context.Context, versionID string, m manifest.Manifest) ([]string, error) {
	if r.client == nil {
		return nil, errors.New("the Node is not connected")
	}
	stream, err := r.client.SubmitManifest(ctx)
	if err != nil {
		return nil, err
	}
	if len(m.Entries) == 0 {
		if err := stream.Send(&nodev1.ManifestChunk{VersionId: versionID}); err != nil {
			return nil, err
		}
	}
	for start := 0; start < len(m.Entries); start += publishChunkEntries {
		end := start + publishChunkEntries
		if end > len(m.Entries) {
			end = len(m.Entries)
		}
		chunk := &nodev1.ManifestChunk{Entries: make([]*nodev1.ManifestEntry, 0, end-start)}
		if start == 0 {
			chunk.VersionId = versionID
		}
		for _, entry := range m.Entries[start:end] {
			converted := &nodev1.ManifestEntry{
				Path: entry.Path, Type: entry.Type, Sha256: entry.SHA256,
				Executable: entry.Executable, Target: entry.Target,
			}
			if entry.Size != nil {
				size := *entry.Size
				converted.Size = &size
			}
			chunk.Entries = append(chunk.Entries, converted)
		}
		if err := stream.Send(chunk); err != nil {
			return nil, err
		}
	}
	ack, err := stream.CloseAndRecv()
	if err != nil {
		return nil, err
	}
	return ack.GetMissingSha256(), nil
}

// uploadPublishObjects PUTs the missing CAS objects through Presign. The
// checksum header is the one the URL was signed with (ADR-0021).
func (r *Runtime) uploadPublishObjects(ctx context.Context, root *os.Root, missing []string, files map[string]string) error {
	for start := 0; start < len(missing); start += presignBatch {
		end := start + presignBatch
		if end > len(missing) {
			end = len(missing)
		}
		batch := missing[start:end]
		response, err := r.presign(ctx, &nodev1.PresignRequest{Sha256: batch, Method: "PUT"})
		if err != nil {
			return err
		}
		if len(response.GetUrls()) != len(batch) {
			return fmt.Errorf("Presign returned %d URLs for %d objects", len(response.GetUrls()), len(batch))
		}
		for i, digest := range batch {
			local, ok := files[digest]
			if !ok {
				return fmt.Errorf("the Control Plane asked for an object this tree does not contain: %s", digest)
			}
			if err := uploadRootObject(ctx, root, local, response.GetUrls()[i], digest); err != nil {
				return err
			}
		}
	}
	return nil
}

func uploadRootObject(ctx context.Context, root *os.Root, local, url, digest string) error {
	raw, err := hex.DecodeString(digest)
	if err != nil || len(raw) != sha256.Size {
		return fmt.Errorf("invalid sha256 %q", digest)
	}
	f, err := root.Open(local)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck // read-only
	info, err := f.Stat()
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, f)
	if err != nil {
		return err
	}
	req.ContentLength = info.Size()
	req.Header.Set("x-amz-checksum-sha256", base64.StdEncoding.EncodeToString(raw))
	resp, err := publishHTTP.Do(req)
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
