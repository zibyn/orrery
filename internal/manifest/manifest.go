// Package manifest defines the Resource manifest: a directory tree described
// by sorted relative paths, file SHA-256 digests, symlink targets and the
// executable bit (ADR-0012). The Control Plane normalizes a manifest and
// addresses the version by the SHA-256 of its canonical bytes. Only pure
// functions live here; #86/#87/#89 reuse them.
package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

const (
	// MaxFileSize is the single-file upload limit (defaults.md, 5 GiB).
	MaxFileSize int64 = 5 << 30
	// MaxEntries is the manifest entry limit (defaults.md, 1,000,000).
	MaxEntries = 1_000_000
)

// Entry types (api.md §3).
const (
	TypeFile    = "file"
	TypeDir     = "dir"
	TypeSymlink = "symlink"
)

var (
	// ErrInvalid is a malformed manifest; the API maps it to 400.
	ErrInvalid = errors.New("invalid manifest")
	// ErrTooLarge is a file over MaxFileSize; the API maps it to 413.
	ErrTooLarge = errors.New("file exceeds 5 GiB")
	// ErrTooManyEntries is a manifest over MaxEntries; the API maps it to 413.
	ErrTooManyEntries = errors.New("manifest has more than 1,000,000 entries")
)

// Manifest is {"entries":[...]} (api.md §3). Entry order is not significant:
// normalization sorts it.
type Manifest struct {
	Entries []Entry `json:"entries"`
}

// Entry is one path in the tree. Size and SHA256 belong to files, Target to
// symlinks; sizes are pointers so a zero-byte file still encodes "size":0.
type Entry struct {
	Path       string `json:"path"`
	Type       string `json:"type"`
	Size       *int64 `json:"size,omitempty"`
	SHA256     string `json:"sha256,omitempty"`
	Executable bool   `json:"executable,omitempty"`
	Target     string `json:"target,omitempty"`
}

// FileObject is one unique CAS object referenced by a manifest.
type FileObject struct {
	SHA256 string
	Size   int64
}

// Normalize validates the manifest and returns it with entries sorted by path.
func Normalize(m Manifest) (Manifest, error) {
	if err := Validate(m); err != nil {
		return Manifest{}, err
	}
	entries := append([]Entry(nil), m.Entries...)
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return Manifest{Entries: entries}, nil
}

// Validate enforces every manifest rule in api.md §3 and ADR-0012.
func Validate(m Manifest) error { return validate(m, MaxEntries) }

func validate(m Manifest, maxEntries int) error {
	if len(m.Entries) > maxEntries {
		return ErrTooManyEntries
	}
	seen := make(map[string]Entry, len(m.Entries))
	for _, entry := range m.Entries {
		if err := validatePath(entry.Path); err != nil {
			return err
		}
		if _, duplicate := seen[entry.Path]; duplicate {
			return fmt.Errorf("%w: duplicate path %q", ErrInvalid, entry.Path)
		}
		if err := validateEntry(entry); err != nil {
			return err
		}
		seen[entry.Path] = entry
	}
	// Every non-root entry needs an explicit parent directory, so empty
	// directories survive a round trip.
	for path := range seen {
		parent, ok := parentPath(path)
		if !ok {
			continue
		}
		entry, exists := seen[parent]
		if !exists || entry.Type != TypeDir {
			return fmt.Errorf("%w: %q has no parent dir entry", ErrInvalid, path)
		}
	}
	return nil
}

func validatePath(path string) error {
	switch {
	case path == "":
		return fmt.Errorf("%w: empty path", ErrInvalid)
	case strings.ContainsRune(path, 0):
		return fmt.Errorf("%w: path contains NUL", ErrInvalid)
	case strings.HasPrefix(path, "/"):
		return fmt.Errorf("%w: %q is absolute", ErrInvalid, path)
	}
	for _, segment := range strings.Split(path, "/") {
		if segment == "" {
			return fmt.Errorf("%w: %q has an empty segment", ErrInvalid, path)
		}
		if segment == "." || segment == ".." {
			return fmt.Errorf("%w: %q escapes the tree", ErrInvalid, path)
		}
	}
	return nil
}

func validateEntry(entry Entry) error {
	switch entry.Type {
	case TypeFile:
		if entry.Size == nil {
			return fmt.Errorf("%w: file %q has no size", ErrInvalid, entry.Path)
		}
		if *entry.Size < 0 {
			return fmt.Errorf("%w: file %q has a negative size", ErrInvalid, entry.Path)
		}
		if *entry.Size > MaxFileSize {
			return fmt.Errorf("%w: %q is %d bytes", ErrTooLarge, entry.Path, *entry.Size)
		}
		if !validDigest(entry.SHA256) {
			return fmt.Errorf("%w: file %q has no valid sha256", ErrInvalid, entry.Path)
		}
		if entry.Target != "" {
			return fmt.Errorf("%w: file %q has a target", ErrInvalid, entry.Path)
		}
	case TypeDir:
		if entry.Size != nil || entry.SHA256 != "" || entry.Target != "" || entry.Executable {
			return fmt.Errorf("%w: dir %q carries file fields", ErrInvalid, entry.Path)
		}
	case TypeSymlink:
		if entry.Target == "" || strings.ContainsRune(entry.Target, 0) {
			return fmt.Errorf("%w: symlink %q has no usable target", ErrInvalid, entry.Path)
		}
		if entry.Size != nil || entry.SHA256 != "" || entry.Executable {
			return fmt.Errorf("%w: symlink %q carries file fields", ErrInvalid, entry.Path)
		}
	default:
		return fmt.Errorf("%w: %q has type %q", ErrInvalid, entry.Path, entry.Type)
	}
	return nil
}

func validDigest(digest string) bool {
	if len(digest) != 64 {
		return false
	}
	for _, r := range digest {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// parentPath returns the parent of a slash-separated relative path, and false
// for a top-level entry.
func parentPath(path string) (string, bool) {
	index := strings.LastIndexByte(path, '/')
	if index < 0 {
		return "", false
	}
	return path[:index], true
}

// Canonical returns the deterministic encoding used for addressing and
// storage: compact JSON of the normalized entries in sorted order.
func Canonical(m Manifest) ([]byte, error) {
	normalized, err := Normalize(m)
	if err != nil {
		return nil, err
	}
	return json.Marshal(normalized)
}

// Address is the lowercase hex SHA-256 of the canonical manifest bytes.
func Address(m Manifest) (string, error) {
	canonical, err := Canonical(m)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

// FileObjects lists the unique CAS objects a manifest references, sorted by
// digest. Duplicate digests are one object; the first path's size is used.
func FileObjects(m Manifest) []FileObject {
	byDigest := make(map[string]int64)
	for _, entry := range m.Entries {
		if entry.Type != TypeFile {
			continue
		}
		if _, exists := byDigest[entry.SHA256]; !exists {
			var size int64
			if entry.Size != nil {
				size = *entry.Size
			}
			byDigest[entry.SHA256] = size
		}
	}
	digests := make([]string, 0, len(byDigest))
	for digest := range byDigest {
		digests = append(digests, digest)
	}
	sort.Strings(digests)
	objects := make([]FileObject, 0, len(digests))
	for _, digest := range digests {
		objects = append(objects, FileObject{SHA256: digest, Size: byDigest[digest]})
	}
	return objects
}

// TotalSize is the sum of every file entry's size, counted once per path.
func TotalSize(m Manifest) int64 {
	var total int64
	for _, entry := range m.Entries {
		if entry.Type == TypeFile && entry.Size != nil {
			total += *entry.Size
		}
	}
	return total
}
