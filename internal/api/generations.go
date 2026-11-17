package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	nodev1 "github.com/zibyn/orrery/gen/node/v1"
	"github.com/zibyn/orrery/internal/generation"
	"github.com/zibyn/orrery/internal/manifest"
	"github.com/zibyn/orrery/internal/objectstore"
	"github.com/zibyn/orrery/internal/store"
)

// generationStore is the slice of the object store Generation commits and
// their purge use, with the reads and manifest writes of Volume files and
// publish; *objectstore.Store implements it.
type generationStore interface {
	HeadSHA256(ctx context.Context, ownerID, sha256hex string) (string, error)
	GetGeneration(ctx context.Context, ownerID, id string) ([]byte, error)
	DeleteGeneration(ctx context.Context, ownerID, id string) error
	PresignGetURL(ctx context.Context, ownerID, sha256hex string) (string, error)
	PutManifest(ctx context.Context, ownerID, address string, data []byte) error
}

// SubmitGeneration opens a Generation commit (node-protocol.md "Generation
// 提交" step 1).
func (n *nodeService) SubmitGeneration(stream nodev1.Node_SubmitGenerationServer) error {
	node, err := n.s.authenticateNode(stream.Context())
	if err != nil {
		return err
	}
	var chunks []*nodev1.GenerationChunk
	entries := 0
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if entries += len(chunk.GetEntries()); entries > manifest.MaxEntries {
			return status.Error(codes.InvalidArgument, manifest.ErrTooManyEntries.Error())
		}
		chunks = append(chunks, chunk)
	}
	ack, err := n.s.submitGeneration(stream.Context(), node, chunks)
	if err != nil {
		return err
	}
	return stream.SendAndClose(ack)
}

// CommitGeneration finishes one (step 3).
func (n *nodeService) CommitGeneration(ctx context.Context, req *nodev1.CommitGenerationRequest) (*nodev1.CommitGenerationResponse, error) {
	node, err := n.s.authenticateNode(ctx)
	if err != nil {
		return nil, err
	}
	return n.s.commitGeneration(ctx, node, req)
}

// generationHolder checks that sb may commit from node under fence. Every
// refusal is FailedPrecondition: never NotFound, which would make the Agent
// clear its identity.
func generationHolder(sb store.Sandbox, node store.Node, fence int64) error {
	switch {
	case sb.DeletedAt.Valid:
		return status.Error(codes.FailedPrecondition, "the Sandbox is deleted")
	case sb.NodeID == nil || *sb.NodeID != node.ID:
		return status.Error(codes.FailedPrecondition, "the Sandbox is not on this Node")
	case sb.Generation != fence:
		return status.Error(codes.FailedPrecondition, "generation is stale")
	}
	return nil
}

// generationKind is the kind a Sandbox commits: a container's upperdir is a
// diff against its SnapshotVersion, a Local working directory a whole tree.
func generationKind(sb store.Sandbox) string {
	if sb.Runtime == "local" {
		return generation.KindTree
	}
	return generation.KindDiff
}

func (s *Server) submitGeneration(ctx context.Context, node store.Node, chunks []*nodev1.GenerationChunk) (*nodev1.GenerationAck, error) {
	if len(chunks) == 0 {
		return nil, status.Error(codes.InvalidArgument, "the first chunk must name the holder")
	}
	first := chunks[0]
	var volumeID *uuid.UUID
	var sandboxID uuid.UUID
	if first.GetVolumeId() != "" {
		if first.GetSandboxId() != "" {
			return nil, status.Error(codes.InvalidArgument, "name one of sandbox_id or volume_id")
		}
		id, err := uuid.Parse(first.GetVolumeId())
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, "volume_id must be a UUID")
		}
		// A Volume commits under the fence of the Sandbox holding it.
		holder, err := s.q.GetVolumeHolder(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, status.Error(codes.FailedPrecondition, "no Sandbox holds the Volume")
		}
		if err != nil {
			s.log.Error("load Volume holder", "volume_id", id, "error", err)
			return nil, status.Error(codes.Internal, "load Volume holder")
		}
		volumeID, sandboxID = &id, holder
	} else {
		id, err := uuid.Parse(first.GetSandboxId())
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, "sandbox_id must be a UUID")
		}
		sandboxID = id
	}
	var base *uuid.UUID
	if first.GetBaseGenerationId() != "" {
		id, err := uuid.Parse(first.GetBaseGenerationId())
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, "base_generation_id must be a UUID")
		}
		base = &id
	}
	var files []manifest.Entry
	for i, chunk := range chunks {
		if i > 0 && (chunk.GetSandboxId() != "" || chunk.GetVolumeId() != "" || chunk.GetGeneration() != 0 || chunk.GetBaseGenerationId() != "") {
			return nil, status.Error(codes.InvalidArgument, "only the first chunk names the holder")
		}
		for _, e := range chunk.GetEntries() {
			if e.GetType() != manifest.TypeFile {
				continue
			}
			entry := manifest.Entry{Path: e.GetPath(), Type: e.GetType(), Size: e.Size, SHA256: e.GetSha256()}
			if len(e.GetBlocks()) > 0 {
				entry.Blocks = e.GetBlocks()
			}
			if err := manifest.ValidateEntry(entry); err != nil {
				return nil, status.Error(codes.InvalidArgument, err.Error())
			}
			files = append(files, entry)
		}
	}
	if s.generations == nil {
		return nil, status.Error(codes.Unavailable, "object storage is not configured")
	}
	sb, err := s.q.GetSandboxIncludingDeleted(ctx, sandboxID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, status.Error(codes.FailedPrecondition, "the Sandbox is not on this Node")
	}
	if err != nil {
		s.log.Error("load committing Sandbox", "sandbox_id", sandboxID, "error", err)
		return nil, status.Error(codes.Internal, "load Sandbox")
	}
	if err := generationHolder(sb, node, first.GetGeneration()); err != nil {
		return nil, err
	}
	missing, err := missingObjects(manifest.FileObjects(manifest.Manifest{Entries: files}), func(digest string) (string, error) {
		return s.generations.HeadSHA256(ctx, sb.OwnerID.String(), digest)
	})
	if err != nil {
		s.log.Error("check Generation objects", "sandbox_id", sandboxID, "error", err)
		return nil, status.Error(codes.Internal, "check objects")
	}
	id, err := uuid.NewV7()
	if err != nil {
		return nil, status.Error(codes.Internal, "generate upload id")
	}
	if err := s.q.CreateGenerationUpload(ctx, store.CreateGenerationUploadParams{
		ID: id, OwnerID: sb.OwnerID, SandboxID: sandboxID, NodeID: node.ID,
		Generation: first.GetGeneration(), BaseGenerationID: base, Missing: missing,
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(uploadTTL), Valid: true}, VolumeID: volumeID,
	}); err != nil {
		s.log.Error("open Generation upload", "sandbox_id", sandboxID, "error", err)
		return nil, status.Error(codes.Internal, "open upload")
	}
	return &nodev1.GenerationAck{UploadId: id.String(), MissingSha256: missing}, nil
}

func (s *Server) commitGeneration(ctx context.Context, node store.Node, req *nodev1.CommitGenerationRequest) (*nodev1.CommitGenerationResponse, error) {
	uploadID, err := uuid.Parse(req.GetUploadId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "upload_id must be a UUID")
	}
	if s.generations == nil {
		return nil, status.Error(codes.Unavailable, "object storage is not configured")
	}
	upload, err := s.q.GetGenerationUpload(ctx, uploadID)
	if errors.Is(err, pgx.ErrNoRows) {
		return s.alreadyCommitted(ctx, uploadID)
	}
	if err != nil {
		s.log.Error("load Generation upload", "upload_id", uploadID, "error", err)
		return nil, status.Error(codes.Internal, "load upload")
	}
	if upload.NodeID != node.ID {
		return nil, status.Error(codes.FailedPrecondition, "the upload is not this Node's")
	}
	owner := upload.OwnerID.String()
	data, err := s.generations.GetGeneration(ctx, owner, uploadID.String())
	if errors.Is(err, objectstore.ErrNotFound) {
		return nil, status.Error(codes.FailedPrecondition, "the Generation manifest is not uploaded")
	}
	if err != nil {
		s.log.Error("read Generation manifest", "upload_id", uploadID, "error", err)
		return nil, status.Error(codes.Internal, "read manifest")
	}
	m, err := generation.Decode(data)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	// The checks against the store run before the transaction, so a slow
	// HeadObject never holds the Sandbox row lock (issue #121). An object
	// removed in between is the CAS sweep's grace window to prevent.
	objects := generation.Objects(m)
	bad, err := verifyObjects(objects, func(digest string) (string, error) {
		return s.generations.HeadSHA256(ctx, owner, digest)
	})
	if err != nil {
		s.log.Error("verify Generation objects", "upload_id", uploadID, "error", err)
		return nil, status.Error(codes.Internal, "verify objects")
	}
	if len(bad) > 0 {
		return nil, status.Errorf(codes.FailedPrecondition, "%d objects are missing or corrupt, first %s", len(bad), bad[0].SHA256)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, status.Error(codes.Internal, "begin commit")
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	q := s.q.WithTx(tx)
	sb, err := q.LockGenerationSandbox(ctx, upload.SandboxID)
	if err != nil {
		s.log.Error("lock committing Sandbox", "sandbox_id", upload.SandboxID, "error", err)
		return nil, status.Error(codes.Internal, "lock Sandbox")
	}
	if err := generationHolder(sb, node, upload.Generation); err != nil {
		return nil, err
	}
	var volume *store.Volume
	if upload.VolumeID != nil {
		if volume, err = lockCommittingVolume(ctx, q, sb.ID, *upload.VolumeID); err != nil {
			if status.Code(err) == codes.Unknown {
				s.log.Error("lock committing Volume", "volume_id", *upload.VolumeID, "error", err)
				return nil, status.Error(codes.Internal, "lock Volume")
			}
			return nil, err
		}
	}
	head, kind := sb.HeadGenerationID, generationKind(sb)
	if volume != nil {
		head, kind = volume.HeadGenerationID, generation.KindTree
	}
	if !sameGeneration(head, upload.BaseGenerationID) {
		return nil, status.Error(codes.FailedPrecondition, "the head moved since base_generation_id")
	}
	if m.Kind != kind {
		return nil, status.Errorf(codes.InvalidArgument, "this holder commits kind %q", kind)
	}
	if err := s.recordGeneration(ctx, q, sb, volume, uploadID, m, objects); err != nil {
		if status.Code(err) == codes.Unknown {
			s.log.Error("record Generation", "upload_id", uploadID, "error", err)
			return nil, status.Error(codes.Internal, "record Generation")
		}
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, status.Error(codes.Internal, "commit Generation")
	}
	return &nodev1.CommitGenerationResponse{GenerationId: uploadID.String()}, nil
}

// lockCommittingVolume locks a Volume a commit advances, after its holder:
// the holder must still hold it, and it must not be deleted.
func lockCommittingVolume(ctx context.Context, q *store.Queries, sandboxID, volumeID uuid.UUID) (*store.Volume, error) {
	holder, err := q.GetVolumeHolder(ctx, volumeID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && holder != sandboxID) {
		return nil, status.Error(codes.FailedPrecondition, "the Sandbox no longer holds the Volume")
	}
	if err != nil {
		return nil, err
	}
	v, err := q.LockGenerationVolume(ctx, volumeID)
	if err != nil {
		return nil, err
	}
	if v.State == "deleted" {
		return nil, status.Error(codes.FailedPrecondition, "the Volume is deleted")
	}
	return &v, nil
}

// recordGeneration writes the row, its objects, and the head move of the
// Sandbox, or of volume when set, inside the caller's transaction.
func (s *Server) recordGeneration(ctx context.Context, q *store.Queries, sb store.Sandbox, volume *store.Volume, id uuid.UUID, m generation.Manifest, objects []manifest.FileObject) error {
	consumed, err := q.ConsumeGenerationUpload(ctx, id)
	if err != nil {
		return err
	}
	if consumed == 0 {
		return status.Error(codes.FailedPrecondition, "the upload expired")
	}
	row := store.InsertGenerationParams{
		ID: id, OwnerID: sb.OwnerID, SandboxID: &sb.ID, Kind: m.Kind,
		ManifestKey: objectstore.GenerationKey(sb.OwnerID.String(), id.String()), Size: generation.Size(m),
	}
	if volume != nil {
		row.SandboxID, row.VolumeID = nil, &volume.ID
		row.Seq, err = q.NextVolumeGenerationSeq(ctx, &volume.ID)
	} else {
		row.Seq, err = q.NextGenerationSeq(ctx, &sb.ID)
	}
	if err != nil {
		return err
	}
	if err := q.InsertGeneration(ctx, row); err != nil {
		return err
	}
	for _, object := range objects {
		if err := q.InsertGenerationObject(ctx, store.InsertGenerationObjectParams{GenerationID: id, Sha256: object.SHA256}); err != nil {
			return err
		}
		if err := q.UpsertCASObject(ctx, store.UpsertCASObjectParams{OwnerID: sb.OwnerID, Sha256: object.SHA256, Size: object.Size}); err != nil {
			return err
		}
	}
	var moved int64
	if volume != nil {
		moved, err = q.SetVolumeHeadGeneration(ctx, store.SetVolumeHeadGenerationParams{ID: volume.ID, Head: id, Base: volume.HeadGenerationID})
	} else {
		moved, err = q.SetSandboxHeadGeneration(ctx, store.SetSandboxHeadGenerationParams{ID: sb.ID, Head: id, Base: sb.HeadGenerationID})
	}
	if err != nil {
		return err
	}
	if moved != 1 {
		return fmt.Errorf("head of %s moved under the row lock", sb.ID)
	}
	return nil
}

// alreadyCommitted makes a retried CommitGeneration whose reply was lost
// succeed while its Generation is still the head.
func (s *Server) alreadyCommitted(ctx context.Context, uploadID uuid.UUID) (*nodev1.CommitGenerationResponse, error) {
	g, err := s.q.GetGeneration(ctx, uploadID)
	if err == nil && g.SandboxID != nil {
		sb, err := s.q.GetSandboxIncludingDeleted(ctx, *g.SandboxID)
		if err == nil && sameGeneration(sb.HeadGenerationID, &uploadID) {
			return &nodev1.CommitGenerationResponse{GenerationId: uploadID.String()}, nil
		}
	}
	if err == nil && g.VolumeID != nil {
		v, err := s.q.GetVolume(ctx, *g.VolumeID)
		if err == nil && sameGeneration(v.HeadGenerationID, &uploadID) {
			return &nodev1.CommitGenerationResponse{GenerationId: uploadID.String()}, nil
		}
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		s.log.Error("load committed Generation", "upload_id", uploadID, "error", err)
		return nil, status.Error(codes.Internal, "load Generation")
	}
	return nil, status.Error(codes.FailedPrecondition, "no open upload with this id")
}

func sameGeneration(a, b *uuid.UUID) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// purgeGenerations keeps only heads (schema.md generations): the rest lose
// their manifest and then their row, and the CAS sweep collects the objects
// no remaining Generation or version references. Expired uploads go the same
// way.
func (s *Server) purgeGenerations(ctx context.Context) error {
	if s.generations == nil {
		return nil
	}
	for {
		rows, err := s.q.ListGarbageGenerations(ctx, gcBatch)
		if err != nil {
			return fmt.Errorf("list garbage Generations: %w", err)
		}
		err = deleteObjectsThenRows(ctx, rows,
			func(ctx context.Context, g store.Generation) error {
				return s.generations.DeleteGeneration(ctx, g.OwnerID.String(), g.ID.String())
			},
			func(ctx context.Context) error {
				return s.purgeTx(ctx, func(q *store.Queries) error {
					for _, g := range rows {
						if err := q.ClearDeletedVolumeHead(ctx, &g.ID); err != nil {
							return err
						}
						if err := q.ClearDeletedSandboxHead(ctx, &g.ID); err != nil {
							return err
						}
						if err := q.DeleteGarbageGeneration(ctx, g.ID); err != nil {
							return err
						}
					}
					return nil
				})
			})
		if err != nil {
			return fmt.Errorf("delete garbage Generations: %w", err)
		}
		if len(rows) < gcBatch {
			break
		}
	}
	uploads, err := s.q.ListExpiredGenerationUploads(ctx, gcBatch)
	if err != nil {
		return fmt.Errorf("list expired Generation uploads: %w", err)
	}
	return deleteObjectsThenRows(ctx, uploads,
		func(ctx context.Context, u store.GenerationUpload) error {
			return s.generations.DeleteGeneration(ctx, u.OwnerID.String(), u.ID.String())
		},
		func(ctx context.Context) error {
			return s.purgeTx(ctx, func(q *store.Queries) error {
				for _, u := range uploads {
					if err := q.DeleteGenerationUpload(ctx, u.ID); err != nil {
						return err
					}
				}
				return nil
			})
		})
}
