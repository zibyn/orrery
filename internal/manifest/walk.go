package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"
)

// readLinkFS is the optional interface a filesystem exposes to read symlink
// targets; os.Root.FS() implements it.
type readLinkFS interface {
	ReadLink(string) (string, error)
}

// Walk builds a normalized Manifest for the tree rooted at base inside fsys.
// Symlinks are recorded and never followed, empty directories and the
// executable bit survive, and devices/FIFOs/sockets are rejected (ADR-0012).
// When base is a directory the directory itself is not an entry; a file or
// symlink base is published under its own name. Files over BlockSize get
// Blocks. objects maps each object digest (file or block) to the bytes an
// upload PUT must read.
func Walk(fsys fs.FS, base string) (Manifest, map[string]Source, error) {
	if base == "" {
		base = "."
	}
	links, ok := fsys.(readLinkFS)
	if !ok {
		return Manifest{}, nil, errors.New("filesystem cannot read symlinks")
	}
	entries := []Entry{}
	objects := map[string]Source{}
	err := fs.WalkDir(fsys, base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative := p
		if base != "." {
			relative = strings.TrimPrefix(p, base)
			relative = strings.TrimPrefix(relative, "/")
		}
		if relative == "" {
			// The published path is a single file or symlink.
			relative = path.Base(base)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			target, err := links.ReadLink(p)
			if err != nil {
				return err
			}
			entries = append(entries, Entry{
				Path: relative, Type: TypeSymlink, Target: target,
			})
		case info.IsDir():
			if p == base {
				return nil
			}
			entries = append(entries, Entry{Path: relative, Type: TypeDir})
		case info.Mode().IsRegular():
			if info.Size() > MaxFileSize {
				return fmt.Errorf("%s: %w", relative, ErrTooLarge)
			}
			size := info.Size()
			digests, err := hashFile(fsys, p, size)
			if err != nil {
				return err
			}
			entry := Entry{
				Path: relative, Type: TypeFile, Size: &size,
				Executable: info.Mode().Perm()&0o111 != 0,
			}
			if size > BlockSize {
				entry.Blocks = digests
			} else {
				entry.SHA256 = digests[0]
			}
			for i, digest := range digests {
				if _, exists := objects[digest]; !exists {
					objects[digest] = Source{Path: p, Offset: int64(i) * BlockSize, Size: BlockLength(size, i)}
				}
			}
			entries = append(entries, entry)
		default:
			return fmt.Errorf("%s is not a regular file, directory or symlink", relative)
		}
		return nil
	})
	if err != nil {
		return Manifest{}, nil, err
	}
	normalized, err := Normalize(Manifest{Entries: entries})
	if err != nil {
		return Manifest{}, nil, err
	}
	return normalized, objects, nil
}

// Source is where one object's bytes live in the walked tree.
type Source struct {
	Path         string
	Offset, Size int64
}

// hashFile returns the digest of each BlockSize block of a file of size, or
// the whole file's digest when it is one object. A file that changed size
// while being read is an error, since its entry would not match its objects.
func hashFile(fsys fs.FS, name string, size int64) ([]string, error) {
	f, err := fsys.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only
	count := 1
	if size > BlockSize {
		count = BlockCount(size)
	}
	digests := make([]string, 0, count)
	var read int64
	for i := range count {
		want := size
		if count > 1 {
			want = BlockLength(size, i)
		}
		sum := sha256.New()
		n, err := io.Copy(sum, io.LimitReader(f, want))
		if err != nil {
			return nil, err
		}
		read += n
		digests = append(digests, hex.EncodeToString(sum.Sum(nil)))
	}
	if extra, _ := f.Read(make([]byte, 1)); read != size || extra != 0 {
		return nil, fmt.Errorf("%s changed while it was read", name)
	}
	return digests, nil
}
