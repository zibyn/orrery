package api

import (
	"errors"
	"testing"

	"github.com/zibyn/stars-sandbox/internal/manifest"
	"github.com/zibyn/stars-sandbox/internal/objectstore"

	nodev1 "github.com/zibyn/stars-sandbox/gen/node/v1"
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
