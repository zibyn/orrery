package api

import (
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/zibyn/orrery/internal/manifest"
	"github.com/zibyn/orrery/internal/objectstore"

	nodev1 "github.com/zibyn/orrery/gen/node/v1"
)

func TestManifestAccumulatorNormalizesChunks(t *testing.T) {
	var acc manifestAccumulator
	if err := acc.add(&nodev1.ManifestChunk{VersionId: "v1", Entries: []*nodev1.ManifestEntry{
		{Path: "b", Type: "dir"},
	}}); err != nil {
		t.Fatal(err)
	}
	size := int64(3)
	if err := acc.add(&nodev1.ManifestChunk{Entries: []*nodev1.ManifestEntry{
		{Path: "b/c.txt", Type: "file", Size: &size, Sha256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	}}); err != nil {
		t.Fatal(err)
	}
	m, err := acc.finish()
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Entries) != 2 || m.Entries[0].Path != "b" || m.Entries[1].Path != "b/c.txt" {
		t.Fatalf("normalized entries = %+v", m.Entries)
	}
	if m.Entries[1].Size == nil || *m.Entries[1].Size != 3 {
		t.Fatalf("file size = %v", m.Entries[1].Size)
	}
}

func TestManifestAccumulatorRejectsBadChunks(t *testing.T) {
	var acc manifestAccumulator
	if err := acc.add(&nodev1.ManifestChunk{Entries: []*nodev1.ManifestEntry{{Path: "a", Type: "dir"}}}); err == nil {
		t.Fatal("first chunk without version_id was accepted")
	}

	acc = manifestAccumulator{}
	if err := acc.add(&nodev1.ManifestChunk{VersionId: "v1"}); err != nil {
		t.Fatal(err)
	}
	if err := acc.add(&nodev1.ManifestChunk{VersionId: "v2"}); err == nil {
		t.Fatal("a changed version_id was accepted")
	}

	acc = manifestAccumulator{}
	if err := acc.add(&nodev1.ManifestChunk{VersionId: "v1", Entries: []*nodev1.ManifestEntry{
		{Path: "a", Type: "dir"}, {Path: "a", Type: "dir"},
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := acc.finish(); err == nil {
		t.Fatal("duplicate paths were accepted")
	}
}

func TestMissingObjects(t *testing.T) {
	objects := []manifest.FileObject{
		{SHA256: "a"},
		{SHA256: "b"},
		{SHA256: "c"},
	}
	head := func(digest string) (string, error) {
		switch digest {
		case "a":
			return "a", nil // stored and correct
		case "b":
			return "", objectstore.ErrNotFound
		case "c":
			return "different", nil // corrupt: upload again
		}
		return "", errors.New("unexpected")
	}
	missing, err := missingObjects(objects, head)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 2 || missing[0] != "b" || missing[1] != "c" {
		t.Fatalf("missing = %v, want [b c]", missing)
	}
}

// Publishing a blocked file verifies each block: one missing block is the
// one object the 409 lists, and the file's sha256 is the block formula.
func TestVerifyObjectsListsAMissingBlock(t *testing.T) {
	size := int64(9 << 20)
	blocks := []string{strings.Repeat("1", 64), strings.Repeat("2", 64), strings.Repeat("3", 64)}
	m, err := manifest.Normalize(manifest.Manifest{Entries: []manifest.Entry{
		{Path: "big", Type: manifest.TypeFile, Size: &size, Blocks: blocks},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Entries[0].SHA256; got != manifest.FileAddress(blocks) {
		t.Fatalf("file sha256 = %s, want the block formula", got)
	}
	present := map[string]bool{blocks[0]: true, blocks[2]: true}
	head := func(digest string) (string, error) {
		if present[digest] {
			return digest, nil
		}
		return "", objectstore.ErrNotFound
	}
	bad, err := verifyObjects(manifest.FileObjects(m), head)
	if err != nil {
		t.Fatal(err)
	}
	if len(bad) != 1 || bad[0].SHA256 != blocks[1] || bad[0].Reason != "missing" {
		t.Fatalf("bad = %+v, want block 1 missing", bad)
	}
	present[blocks[1]] = true
	if bad, _ := verifyObjects(manifest.FileObjects(m), head); len(bad) != 0 {
		t.Fatalf("bad = %+v with every block present", bad)
	}
}

// One Presign signs at most 1,000 objects; a larger batch is refused whole
// before anything is looked up (node-protocol.md).
func TestPresignRefusesMoreThan1000Objects(t *testing.T) {
	n := &nodeService{s: &Server{}}
	_, err := n.Presign(context.Background(), &nodev1.PresignRequest{Sha256: make([]string, 1001), Method: "GET"})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("Presign(1001) = %v, want InvalidArgument", err)
	}
}
