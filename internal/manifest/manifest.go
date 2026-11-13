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
	// MaxFileSize is the single-file upload limit (defaults.md, 1 TiB).
	MaxFileSize int64 = 1 << 40
	// BlockSize splits files larger than it into blocks (ADR-0030); a file of
	// at most BlockSize is one object.
	BlockSize int64 = 4 << 20
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
	ErrTooLarge = errors.New("file exceeds 1 TiB")
	// ErrTooManyEntries is a manifest over MaxEntries; the API maps it to 413.
	ErrTooManyEntries = errors.New("manifest has more than 1,000,000 entries")
)

// Manifest is {"entries":[...]} (api.md §3). Entry order is not significant:
// normalization sorts it.
type Manifest struct {
	Entries []Entry `json:"entries"`
}

// Entry is one path in the tree. Size, SHA256 and Blocks belong to files,
// Target to symlinks; sizes are pointers so a zero-byte file still encodes
// "size":0. A file over BlockSize lists its block digests in Blocks and its
// SHA256 is FileAddress(Blocks), filled in by Normalize; a file published
// before ADR-0030 may be larger without Blocks, its SHA256 the whole file's.
type Entry struct {
	Path       string   `json:"path"`
	Type       string   `json:"type"`
	Size       *int64   `json:"size,omitempty"`
	SHA256     string   `json:"sha256,omitempty"`
	Blocks     []string `json:"blocks,omitempty"`
	Executable bool     `json:"executable,omitempty"`
	Target     string   `json:"target,omitempty"`
}

// FileObject is one unique CAS object referenced by a manifest.
type FileObject struct {
	SHA256 string
	Size   int64
}

// Normalize validates the manifest and returns it with entries sorted by path
// and every blocked file's SHA256 computed from its blocks.
func Normalize(m Manifest) (Manifest, error) {
	if err := Validate(m); err != nil {
		return Manifest{}, err
	}
	entries := append([]Entry(nil), m.Entries...)
	for i := range entries {
		if entries[i].Blocks != nil {
			entries[i].SHA256 = FileAddress(entries[i].Blocks)
		}
	}
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
		if entry.Blocks != nil {
			if err := validateBlocks(entry); err != nil {
				return err
			}
		} else if !validDigest(entry.SHA256) {
			return fmt.Errorf("%w: file %q has no valid sha256", ErrInvalid, entry.Path)
		}
		if entry.Target != "" {
			return fmt.Errorf("%w: file %q has a target", ErrInvalid, entry.Path)
		}
	case TypeDir:
		if entry.Size != nil || entry.SHA256 != "" || entry.Blocks != nil || entry.Target != "" || entry.Executable {
			return fmt.Errorf("%w: dir %q carries file fields", ErrInvalid, entry.Path)
		}
	case TypeSymlink:
		if entry.Target == "" || strings.ContainsRune(entry.Target, 0) {
			return fmt.Errorf("%w: symlink %q has no usable target", ErrInvalid, entry.Path)
		}
		if entry.Size != nil || entry.SHA256 != "" || entry.Blocks != nil || entry.Executable {
			return fmt.Errorf("%w: symlink %q carries file fields", ErrInvalid, entry.Path)
		}
	default:
		return fmt.Errorf("%w: %q has type %q", ErrInvalid, entry.Path, entry.Type)
	}
	return nil
}

// validateBlocks checks a blocked file: only files over BlockSize are split,
// into exactly BlockCount(size) blocks. The file's own sha256 is the server's
// to compute; one the client supplies must already equal it.
func validateBlocks(entry Entry) error {
	size := *entry.Size
	if size <= BlockSize {
		return fmt.Errorf("%w: file %q is not over 4 MiB and must not have blocks", ErrInvalid, entry.Path)
	}
	if len(entry.Blocks) != BlockCount(size) {
		return fmt.Errorf("%w: file %q has %d blocks, want %d", ErrInvalid, entry.Path, len(entry.Blocks), BlockCount(size))
	}
	for _, block := range entry.Blocks {
		if !validDigest(block) {
			return fmt.Errorf("%w: file %q has an invalid block digest", ErrInvalid, entry.Path)
		}
	}
	if entry.SHA256 != "" && entry.SHA256 != FileAddress(entry.Blocks) {
		return fmt.Errorf("%w: blocked file %q must not carry its own sha256", ErrInvalid, entry.Path)
	}
	return nil
}

// RequireBlocks rejects a file over BlockSize without blocks. Validate
// accepts such legacy entries for versions published before ADR-0030; new
// uploads and Workspace publishes must split them.
func RequireBlocks(m Manifest) error {
	for _, entry := range m.Entries {
		if entry.Type == TypeFile && entry.Blocks == nil && entry.Size != nil && *entry.Size > BlockSize {
			return fmt.Errorf("%w: file %q is over 4 MiB and needs blocks", ErrInvalid, entry.Path)
		}
	}
	return nil
}

// BlockCount is the number of BlockSize blocks a file of size splits into.
func BlockCount(size int64) int { return int((size + BlockSize - 1) / BlockSize) }

// BlockLength is the length of block i of a file of size; only the last is
// shorter than BlockSize.
func BlockLength(size int64, i int) int64 { return min(BlockSize, size-int64(i)*BlockSize) }

// blockPrefix keeps a blocked file's address apart from a small file whose
// content happens to be the concatenated digests (api.md §3).
const blockPrefix = "orrery-blocks-v1\n"

// FileAddress is a blocked file's sha256: SHA-256 of blockPrefix followed by
// each block's 32 raw digest bytes in order.
func FileAddress(blocks []string) string {
	sum := sha256.New()
	sum.Write([]byte(blockPrefix))
	for _, block := range blocks {
		raw, _ := hex.DecodeString(block) // validated digests
		sum.Write(raw)
	}
	return hex.EncodeToString(sum.Sum(nil))
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
// digest: whole files and the blocks of blocked files. Duplicate digests are
// one object; the first occurrence's size is used.
func FileObjects(m Manifest) []FileObject {
	byDigest := make(map[string]int64)
	add := func(digest string, size int64) {
		if _, exists := byDigest[digest]; !exists {
			byDigest[digest] = size
		}
	}
	for _, entry := range m.Entries {
		if entry.Type != TypeFile {
			continue
		}
		var size int64
		if entry.Size != nil {
			size = *entry.Size
		}
		if entry.Blocks == nil {
			add(entry.SHA256, size)
			continue
		}
		for i, block := range entry.Blocks {
			add(block, BlockLength(size, i))
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
