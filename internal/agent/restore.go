package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"

	nodev1 "github.com/zibyn/orrery/gen/node/v1"
	"github.com/zibyn/orrery/internal/generation"
	"github.com/zibyn/orrery/internal/manifest"
	"github.com/zibyn/orrery/internal/resourcefs"
)

// cachedDiffMatches reports whether this Node's copy of the Sandbox's
// filesystem is the Generation restore (runtime.md §16): its stat table
// describes that head. A commit whose reply was lost but which became the
// head is settled into the table first. No head never matches: a Sandbox that
// never committed starts from its SnapshotVersion.
func (r *Runtime) cachedDiffMatches(id uuid.UUID, restore string) bool {
	if restore == "" {
		return false
	}
	tablePath, pendingPath := statTablePath(r.dataRoot, id), pendingTablePath(r.dataRoot, id)
	if pending, err := readStatTable(pendingPath); err == nil && pending.Generation == restore {
		if os.Rename(pendingPath, tablePath) != nil {
			return false
		}
	}
	table, err := readStatTable(tablePath)
	return err == nil && table.Generation == restore
}

// forgetCachedDiff removes the stat tables and the clean mark: the filesystem
// they described is gone or about to change.
func (r *Runtime) forgetCachedDiff(id uuid.UUID) error {
	for _, path := range []string{statTablePath(r.dataRoot, id), pendingTablePath(r.dataRoot, id), cleanMarkPath(r.dataRoot, id)} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// cleanMarkPath exists while the Node's copy of a stopped Sandbox equals its
// last commit: the stop has nothing to commit and the disk watermark may
// evict it (runtime.md §16, defaults.md).
func cleanMarkPath(dataRoot string, id uuid.UUID) string {
	return filepath.Join(sandboxPath(dataRoot, id), "disk-committed")
}

// restoreFilesystem writes Generation genID into root, a fresh directory (a
// new snapshot's upperdir or a Local working directory), fetching objects
// through the block cache, and records the stat table so the next commit is
// based on genID (runtime.md §16).
func (r *Runtime) restoreFilesystem(ctx context.Context, id uuid.UUID, genID, root string) error {
	m, err := r.fetchGenerationManifest(ctx, genID)
	if err != nil {
		return fmt.Errorf("read Generation %s: %w", genID, err)
	}
	cache, err := resourcefs.NewCache(blockCachePath(r.dataRoot), presignFunc(r.presignGET))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	files, err := restoreTree(root, m, cache)
	if err != nil {
		return fmt.Errorf("restore Generation %s: %w", genID, err)
	}
	if err := r.forgetCachedDiff(id); err != nil {
		return err
	}
	return writeStatTable(statTablePath(r.dataRoot, id), statTable{Generation: genID, Files: files})
}

func (r *Runtime) fetchGenerationManifest(ctx context.Context, genID string) (generation.Manifest, error) {
	resp, err := r.presign(ctx, &nodev1.PresignRequest{Generations: []string{genID}, Method: "GET"})
	if err != nil {
		return generation.Manifest{}, err
	}
	if len(resp.GetUrls()) != 1 {
		return generation.Manifest{}, fmt.Errorf("Presign returned %d URLs for one Generation", len(resp.GetUrls()))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, resp.GetUrls()[0], nil)
	if err != nil {
		return generation.Manifest{}, err
	}
	res, err := publishHTTP.Do(req)
	if err != nil {
		return generation.Manifest{}, err
	}
	defer res.Body.Close() //nolint:errcheck // read-only
	data, err := io.ReadAll(res.Body)
	if err != nil {
		return generation.Manifest{}, err
	}
	if res.StatusCode != http.StatusOK {
		return generation.Manifest{}, fmt.Errorf("HTTP %d: %s", res.StatusCode, bytes.TrimSpace(data[:min(len(data), 4096)]))
	}
	return generation.Decode(data)
}

// presignGET signs object and Resource manifest reads (node-protocol.md
// "Presign"), for the block cache.
func (r *Runtime) presignGET(ctx context.Context, objects, manifests []string) ([]string, error) {
	resp, err := r.presign(ctx, &nodev1.PresignRequest{Sha256: objects, Manifests: manifests, Method: "GET"})
	if err != nil {
		return nil, err
	}
	return resp.GetUrls(), nil
}

// contentSource writes a file entry's bytes; next hints what follows.
type contentSource interface {
	Copy(w io.Writer, entry manifest.Entry, next []manifest.Entry) error
}

// restoreTree writes a normalized manifest under root, which must not already
// hold its paths, and returns the stat table of what it wrote. A directory's
// mode and every mtime are set last, bottom-up, so writing children changes
// neither. Without root privileges owners other than the Agent's are left as
// they land, and opaque is marked in the user namespace as rootless overlay
// does.
func restoreTree(root string, m generation.Manifest, src contentSource) (map[string]statRecord, error) {
	privileged := os.Geteuid() == 0
	opaqueXattr := "trusted.overlay.opaque"
	if !privileged {
		opaqueXattr = "user.overlay.opaque"
	}
	byPath := make(map[string]generation.Entry, len(m.Entries))
	var contents []manifest.Entry
	for _, e := range m.Entries {
		byPath[e.Path] = e
		if e.Type == manifest.TypeFile && *e.Size > 0 {
			contents = append(contents, e.Entry)
		}
	}
	next := 0
	for _, e := range m.Entries {
		p := filepath.Join(root, e.Path)
		switch e.Type {
		case manifest.TypeDir:
			if err := os.Mkdir(p, 0o700); err != nil {
				return nil, err
			}
		case manifest.TypeFile:
			f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				return nil, err
			}
			if *e.Size > 0 {
				next++
				err = src.Copy(f, e.Entry, contents[next:])
			}
			if closeErr := f.Close(); err == nil {
				err = closeErr
			}
			if err != nil {
				return nil, fmt.Errorf("%s: %w", e.Path, err)
			}
		case manifest.TypeSymlink:
			if err := os.Symlink(e.Target, p); err != nil {
				return nil, err
			}
		case generation.TypeWhiteout:
			if err := unix.Mknod(p, unix.S_IFCHR, 0); err != nil {
				return nil, fmt.Errorf("whiteout %s: %w", e.Path, err)
			}
			continue
		case generation.TypeHardlink:
			continue // once every file exists
		default:
			return nil, fmt.Errorf("%s: unknown entry type %q", e.Path, e.Type)
		}
		if err := unix.Lchown(p, int(e.UID), int(e.GID)); err != nil && (privileged || !errors.Is(err, unix.EPERM)) {
			return nil, fmt.Errorf("chown %s: %w", e.Path, err)
		}
		for name, value := range e.Xattrs {
			if err := unix.Lsetxattr(p, name, value, 0); err != nil {
				return nil, fmt.Errorf("xattr %s of %s: %w", name, e.Path, err)
			}
		}
		if e.Opaque {
			if err := unix.Lsetxattr(p, opaqueXattr, []byte("y"), 0); err != nil {
				return nil, fmt.Errorf("opaque %s: %w", e.Path, err)
			}
		}
		if e.Type == manifest.TypeFile {
			// After chown, which clears setuid and setgid.
			if err := os.Chmod(p, fileMode(e.Mode)); err != nil {
				return nil, err
			}
		}
	}
	for _, e := range m.Entries {
		if e.Type == generation.TypeHardlink {
			if err := os.Link(filepath.Join(root, e.Target), filepath.Join(root, e.Path)); err != nil {
				return nil, err
			}
		}
	}
	// Entries are sorted, so walking them backwards meets children first.
	for i := len(m.Entries) - 1; i >= 0; i-- {
		e := m.Entries[i]
		p := filepath.Join(root, e.Path)
		switch e.Type {
		case generation.TypeWhiteout, generation.TypeHardlink:
			continue
		case manifest.TypeDir:
			if err := os.Chmod(p, fileMode(e.Mode)); err != nil {
				return nil, err
			}
		}
		times := []unix.Timespec{unix.NsecToTimespec(e.Mtime), unix.NsecToTimespec(e.Mtime)}
		if err := unix.UtimesNanoAt(unix.AT_FDCWD, p, times, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return nil, fmt.Errorf("mtime %s: %w", e.Path, err)
		}
	}
	files := map[string]statRecord{}
	for _, e := range m.Entries {
		file := e
		if e.Type == generation.TypeHardlink {
			file = byPath[e.Target]
		} else if e.Type != manifest.TypeFile {
			continue
		}
		var st unix.Stat_t
		if err := unix.Lstat(filepath.Join(root, e.Path), &st); err != nil {
			return nil, err
		}
		record := statRecord{Size: st.Size, Mtime: st.Mtim.Nano(), Ctime: st.Ctim.Nano(), Ino: st.Ino, Mode: st.Mode}
		if file.Blocks != nil {
			record.Blocks = file.Blocks
		} else {
			record.SHA256 = file.SHA256
		}
		files[e.Path] = record
	}
	return files, nil
}

// fileMode turns the manifest's 07777 bits into Go's mode with setuid,
// setgid and sticky.
func fileMode(mode uint32) os.FileMode {
	m := os.FileMode(mode & 0o777)
	if mode&unix.S_ISUID != 0 {
		m |= os.ModeSetuid
	}
	if mode&unix.S_ISGID != 0 {
		m |= os.ModeSetgid
	}
	if mode&unix.S_ISVTX != 0 {
		m |= os.ModeSticky
	}
	return m
}
