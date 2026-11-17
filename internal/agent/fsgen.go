package agent

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/zibyn/orrery/internal/generation"
	"github.com/zibyn/orrery/internal/manifest"
)

// statRecord is one file's stat as of the last commit and the digests it was
// committed with: a file whose stat still matches is not read again
// (runtime.md §16).
type statRecord struct {
	Size   int64    `json:"size"`
	Mtime  int64    `json:"mtime"`
	Ctime  int64    `json:"ctime"`
	Ino    uint64   `json:"ino"`
	Mode   uint32   `json:"mode"`
	SHA256 string   `json:"sha256,omitempty"`
	Blocks []string `json:"blocks,omitempty"`
}

// fsScan is one scan of a Sandbox's filesystem.
type fsScan struct {
	manifest generation.Manifest
	// files is the stat table to keep once this scan is committed.
	files map[string]statRecord
	// changed are the file entries hashed by this scan, the ones
	// SubmitGeneration reports; objects locates their bytes under the root.
	changed []generation.Entry
	objects map[string]manifest.Source
}

// Overlay keeps its own state in these xattr namespaces; only the opaque
// marker means anything to a Generation.
var overlayXattrPrefixes = []string{"trusted.overlay.", "user.overlay."}

// scanFilesystem stat-scans root (a container upperdir for KindDiff, a Local
// working directory for KindTree) into a Generation manifest. Paths in
// exclude, and everything under them, are runtime mounts and not the
// Sandbox's. Sockets, FIFOs and devices other than whiteouts have no entry
// type and are left out. *.lock files inside a Git directory are skipped.
func scanFilesystem(root, kind string, exclude []string, prev map[string]statRecord) (fsScan, error) {
	scan := fsScan{
		manifest: generation.Manifest{Kind: kind, Entries: []generation.Entry{}},
		files:    map[string]statRecord{},
		objects:  map[string]manifest.Source{},
	}
	excluded := map[string]bool{}
	for _, p := range exclude {
		excluded[strings.Trim(filepath.Clean("/"+p), "/")] = true
	}
	gitDirs := map[string]bool{}
	linked := map[uint64]string{} // inode -> the path its file entry has
	fsys := os.DirFS(root)
	err := filepath.WalkDir(root, func(full string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, full)
		if err != nil || rel == "." {
			return err
		}
		if excluded[rel] {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		var st unix.Stat_t
		if err := unix.Lstat(full, &st); err != nil {
			return err
		}
		entry := generation.Entry{
			Entry: manifest.Entry{Path: rel},
			Mode:  st.Mode & 0o7777, UID: st.Uid, GID: st.Gid,
			Mtime: st.Mtim.Nano(),
		}
		switch st.Mode & unix.S_IFMT {
		case unix.S_IFDIR:
			entry.Type = manifest.TypeDir
			if isGitDir(full) {
				gitDirs[rel] = true
			}
		case unix.S_IFLNK:
			target, err := os.Readlink(full)
			if err != nil {
				return err
			}
			entry.Type, entry.Target = manifest.TypeSymlink, target
		case unix.S_IFCHR:
			if st.Rdev != 0 {
				return nil
			}
			scan.manifest.Entries = append(scan.manifest.Entries, generation.Entry{
				Entry: manifest.Entry{Path: rel, Type: generation.TypeWhiteout},
			})
			return nil
		case unix.S_IFREG:
			if strings.HasSuffix(rel, ".lock") && underGitDir(gitDirs, rel) {
				return nil
			}
			if st.Nlink > 1 {
				if first, ok := linked[st.Ino]; ok {
					scan.manifest.Entries = append(scan.manifest.Entries, generation.Entry{
						Entry: manifest.Entry{Path: rel, Type: generation.TypeHardlink, Target: first},
					})
					return nil
				}
				linked[st.Ino] = rel
			}
			entry.Type = manifest.TypeFile
			if err := scan.addFile(fsys, &entry, st, prev[rel]); err != nil {
				return err
			}
		default:
			return nil // shortcut: sockets, FIFOs and devices are not committed, add types if Sandboxes need them
		}
		xattrs, opaque, err := readXattrs(full)
		if err != nil {
			return err
		}
		entry.Xattrs = xattrs
		entry.Opaque = opaque && kind == generation.KindDiff && entry.Type == manifest.TypeDir
		scan.manifest.Entries = append(scan.manifest.Entries, entry)
		return nil
	})
	if err != nil {
		return fsScan{}, err
	}
	return scan, nil
}

// addFile fills a file entry's size and digests, reusing prev's when the
// stat is unchanged.
func (s *fsScan) addFile(fsys fs.FS, entry *generation.Entry, st unix.Stat_t, prev statRecord) error {
	record := statRecord{
		Size: st.Size, Mtime: st.Mtim.Nano(), Ctime: st.Ctim.Nano(),
		Ino: st.Ino, Mode: st.Mode,
	}
	size := st.Size
	entry.Size = &size
	if prev.SHA256 != "" || prev.Blocks != nil {
		same := record
		same.SHA256, same.Blocks = prev.SHA256, prev.Blocks
		if same.Size == prev.Size && same.Mtime == prev.Mtime && same.Ctime == prev.Ctime &&
			same.Ino == prev.Ino && same.Mode == prev.Mode {
			entry.SHA256, entry.Blocks = prev.SHA256, prev.Blocks
			s.files[entry.Path] = same
			return nil
		}
	}
	digests, err := manifest.HashFile(fsys, entry.Path, size)
	if err != nil {
		return err
	}
	if size > manifest.BlockSize {
		entry.Blocks, record.Blocks = digests, digests
	} else {
		entry.SHA256, record.SHA256 = digests[0], digests[0]
	}
	for i, digest := range digests {
		if _, ok := s.objects[digest]; !ok {
			s.objects[digest] = manifest.Source{Path: entry.Path, Offset: int64(i) * manifest.BlockSize, Size: manifest.BlockLength(size, i)}
		}
	}
	s.files[entry.Path] = record
	s.changed = append(s.changed, *entry)
	return nil
}

// isGitDir: a directory holding both HEAD and objects/ (runtime.md §16).
// shortcut: only the scanned tree is looked at, so a repository whose HEAD is
// in the SnapshotVersion and only a lock in the upperdir keeps that lock.
func isGitDir(dir string) bool {
	head, err := os.Lstat(filepath.Join(dir, "HEAD"))
	if err != nil || !head.Mode().IsRegular() {
		return false
	}
	objects, err := os.Lstat(filepath.Join(dir, "objects"))
	return err == nil && objects.IsDir()
}

func underGitDir(gitDirs map[string]bool, rel string) bool {
	for dir := filepath.Dir(rel); dir != "."; dir = filepath.Dir(dir) {
		if gitDirs[dir] {
			return true
		}
	}
	return false
}

// readXattrs returns path's extended attributes without overlay's own, and
// whether overlay marks it opaque.
func readXattrs(path string) (map[string][]byte, bool, error) {
	size, err := unix.Llistxattr(path, nil)
	if errors.Is(err, unix.ENOTSUP) || size == 0 {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("list xattrs of %s: %w", path, err)
	}
	buf := make([]byte, size)
	size, err = unix.Llistxattr(path, buf)
	if err != nil {
		return nil, false, fmt.Errorf("list xattrs of %s: %w", path, err)
	}
	var out map[string][]byte
	opaque := false
	for _, name := range strings.Split(strings.TrimRight(string(buf[:size]), "\x00"), "\x00") {
		value, err := getXattr(path, name)
		if errors.Is(err, unix.ENODATA) {
			continue // removed since the list
		}
		if err != nil {
			return nil, false, err
		}
		if overlayXattr(name) {
			opaque = opaque || (strings.HasSuffix(name, ".overlay.opaque") && string(value) == "y")
			continue
		}
		if out == nil {
			out = map[string][]byte{}
		}
		out[name] = value
	}
	return out, opaque, nil
}

func overlayXattr(name string) bool {
	for _, prefix := range overlayXattrPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func getXattr(path, name string) ([]byte, error) {
	for {
		size, err := unix.Lgetxattr(path, name, nil)
		if err != nil {
			return nil, fmt.Errorf("read xattr %s of %s: %w", name, path, err)
		}
		buf := make([]byte, size)
		n, err := unix.Lgetxattr(path, name, buf)
		if errors.Is(err, syscall.ERANGE) {
			continue // grew since the size query
		}
		if err != nil {
			return nil, fmt.Errorf("read xattr %s of %s: %w", name, path, err)
		}
		return buf[:n], nil
	}
}
