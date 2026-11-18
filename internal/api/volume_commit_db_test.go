package api

import (
	"testing"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	nodev1 "github.com/zibyn/orrery/gen/node/v1"
	"github.com/zibyn/orrery/internal/generation"
	"github.com/zibyn/orrery/internal/manifest"
	"github.com/zibyn/orrery/internal/store"
)

// volumeTree is a one-file Volume tree whose file is digest.
func oneFileTree(digest string) generation.Manifest {
	return generation.Manifest{Kind: generation.KindTree, Entries: []generation.Entry{
		{Entry: manifest.Entry{Path: "data", Type: manifest.TypeFile, Size: ptr(int64(1)), SHA256: digest}, Mode: 0o644},
	}}
}

// withVolume makes the fixture's Sandbox hold a new Volume.
func (f *generationFixture) withVolume() uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	f.exec(`INSERT INTO volumes (id, owner_id, name) VALUES ($1, $2, $3)`, id, f.user, "v-"+id.String()[:8])
	f.exec(`INSERT INTO sandbox_volumes (sandbox_id, volume_id, path, held) VALUES ($1, $2, '/data', true)`, f.sandbox, id)
	return id
}

func (f *generationFixture) submitVolume(volume uuid.UUID, m generation.Manifest, fence int64, base *uuid.UUID) (*nodev1.GenerationAck, error) {
	chunk := &nodev1.GenerationChunk{VolumeId: volume.String(), Generation: fence}
	if base != nil {
		chunk.BaseGenerationId = base.String()
	}
	for _, e := range m.Entries {
		chunk.Entries = append(chunk.Entries, &nodev1.ManifestEntry{Path: e.Path, Type: e.Type, Size: e.Size, Sha256: e.SHA256})
	}
	return f.s.submitGeneration(f.ctx, f.node, []*nodev1.GenerationChunk{chunk})
}

func (f *generationFixture) volumeHead(volume uuid.UUID) *uuid.UUID {
	f.t.Helper()
	v, err := f.s.q.GetVolume(f.ctx, volume)
	if err != nil {
		f.t.Fatal(err)
	}
	return v.HeadGenerationID
}

func TestVolumeCommitAdvancesTheVolumeHead(t *testing.T) {
	f := newGenerationFixture(t)
	volume := f.withVolume()
	ack, err := f.submitVolume(volume, oneFileTree(objA), 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := ack.GetMissingSha256(); len(got) != 1 || got[0] != objA {
		t.Fatalf("missing = %v", got)
	}
	// The holder's Node may PUT the upload's manifest and missing object.
	if _, ok, err := f.s.authorizeGeneration(f.ctx, f.node, ack.GetUploadId(), "PUT"); err != nil || !ok {
		t.Fatalf("PUT of the Volume upload not authorized: %v", err)
	}
	f.upload(ack, oneFileTree(objA))
	if _, err := f.commit(ack); err != nil {
		t.Fatal(err)
	}
	head := f.volumeHead(volume)
	if head == nil || head.String() != ack.GetUploadId() {
		t.Fatalf("Volume head = %v, want %s", head, ack.GetUploadId())
	}
	if f.head() != nil {
		t.Fatal("a Volume commit moved the Sandbox's head")
	}
	// A retried commit of the head still succeeds.
	if _, err := f.commit(ack); err != nil {
		t.Fatalf("retried Volume commit = %v", err)
	}
	// The Node mounting it may read the head and its objects.
	if _, ok, err := f.s.authorizeGeneration(f.ctx, f.node, head.String(), "GET"); err != nil || !ok {
		t.Fatalf("GET of the Volume head not authorized: %v", err)
	}
	if owner, err := f.s.q.FindNodeHeadGenerationObjectOwner(f.ctx, store.FindNodeHeadGenerationObjectOwnerParams{
		NodeID: &f.node.ID, Sha256: objA,
	}); err != nil || owner != f.user {
		t.Fatalf("object GET owner = %v, %v", owner, err)
	}

	second, err := f.submitVolume(volume, oneFileTree(objB), 1, head)
	if err != nil {
		t.Fatal(err)
	}
	f.upload(second, oneFileTree(objB))
	if _, err := f.commit(second); err != nil {
		t.Fatal(err)
	}
	if h := f.volumeHead(volume); h == nil || h.String() != second.GetUploadId() {
		t.Fatalf("Volume head = %v after the second commit", h)
	}
}

func TestVolumeCommitRefusals(t *testing.T) {
	f := newGenerationFixture(t)
	volume := f.withVolume()
	if _, err := f.submitVolume(volume, oneFileTree(objA), 0, nil); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("stale fence = %v, want FailedPrecondition", err)
	}
	if _, err := f.submitVolume(uuid.New(), oneFileTree(objA), 1, nil); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("a Volume nobody holds = %v, want FailedPrecondition", err)
	}
	if _, err := f.submitVolume(volume, diff(objA), 1, nil); err != nil {
		t.Fatal(err) // the kind is checked against the uploaded manifest
	}

	// A moved head refuses the commit built on the old base.
	ack, err := f.submitVolume(volume, oneFileTree(objA), 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.upload(ack, oneFileTree(objA))
	other, err := f.submitVolume(volume, oneFileTree(objB), 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.upload(other, oneFileTree(objB))
	if _, err := f.commit(ack); err != nil {
		t.Fatal(err)
	}
	if _, err := f.commit(other); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("commit on a moved head = %v, want FailedPrecondition", err)
	}

	// A diff is not a Volume's kind.
	head := f.volumeHead(volume)
	bad, err := f.submitVolume(volume, oneFileTree(objC), 1, head)
	if err != nil {
		t.Fatal(err)
	}
	f.upload(bad, diff(objC))
	if _, err := f.commit(bad); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("a diff for a Volume = %v, want InvalidArgument", err)
	}

	// Once the Sandbox stops holding it, its uploads are refused.
	late, err := f.submitVolume(volume, oneFileTree(objC), 1, head)
	if err != nil {
		t.Fatal(err)
	}
	f.upload(late, oneFileTree(objC))
	f.exec(`UPDATE sandbox_volumes SET held = false WHERE volume_id = $1`, volume)
	if _, err := f.commit(late); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("commit after the hold ended = %v, want FailedPrecondition", err)
	}
	if h := f.volumeHead(volume); h == nil || *h != *head {
		t.Fatal("a refused commit moved the Volume head")
	}
}
