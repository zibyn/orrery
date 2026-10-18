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
// symlink base is published under its own name. files maps each digest to the
// path an upload PUT must read.
func Walk(fsys fs.FS, base string) (Manifest, map[string]string, error) {
	if base == "" {
		base = "."
	}
	links, ok := fsys.(readLinkFS)
	if !ok {
		return Manifest{}, nil, errors.New("filesystem cannot read symlinks")
	}
	entries := []Entry{}
	files := map[string]string{}
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
			digest, err := hashFile(fsys, p)
			if err != nil {
				return err
			}
			size := info.Size()
			if _, exists := files[digest]; !exists {
				files[digest] = p
			}
			entries = append(entries, Entry{
				Path: relative, Type: TypeFile, Size: &size, SHA256: digest,
				Executable: info.Mode().Perm()&0o111 != 0,
			})
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
	return normalized, files, nil
}

func hashFile(fsys fs.FS, name string) (string, error) {
	f, err := fsys.Open(name)
	if err != nil {
		return "", err
	}
	defer f.Close() //nolint:errcheck // read-only
	sum := sha256.New()
	n, err := io.Copy(sum, io.LimitReader(f, MaxFileSize+1))
	if err != nil {
		return "", err
	}
	if n > MaxFileSize {
		return "", ErrTooLarge
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}
