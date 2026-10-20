package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	nodev1 "github.com/zibyn/orrery/gen/node/v1"
	"github.com/zibyn/orrery/internal/manifest"
	"github.com/zibyn/orrery/internal/objectstore"
	"github.com/zibyn/orrery/internal/store"
)

// publishFailedCause is the single failure cause a Workspace publish reports
// (node-protocol.md "Workspace publish").
const publishFailedCause = "publish_failed"

// manifestAccumulator collects SubmitManifest chunks. version_id arrives on
// the first chunk and never changes; normalization happens once at the end.
type manifestAccumulator struct {
	versionID string
	entries   []manifest.Entry
}

func (a *manifestAccumulator) add(chunk *nodev1.ManifestChunk) error {
	switch {
	case a.versionID == "":
		if chunk.GetVersionId() == "" {
			return errors.New("the first chunk must carry version_id")
		}
		a.versionID = chunk.GetVersionId()
	case chunk.GetVersionId() != "" && chunk.GetVersionId() != a.versionID:
		return errors.New("version_id changed between chunks")
	}
	if len(a.entries)+len(chunk.GetEntries()) > manifest.MaxEntries {
		return manifest.ErrTooManyEntries
	}
	for _, entry := range chunk.GetEntries() {
		converted := manifest.Entry{
			Path:       entry.GetPath(),
			Type:       entry.GetType(),
			SHA256:     entry.GetSha256(),
			Executable: entry.GetExecutable(),
			Target:     entry.GetTarget(),
		}
		if entry.Size != nil {
			size := *entry.Size
			converted.Size = &size
		}
		a.entries = append(a.entries, converted)
	}
	return nil
}

func (a *manifestAccumulator) finish() (manifest.Manifest, error) {
	return manifest.Normalize(manifest.Manifest{Entries: a.entries})
}

// missingObjects is the ManifestAck calculation. It asks the store, not the
// database, so a resumed publish does not re-upload what already landed.
func missingObjects(objects []manifest.FileObject, head func(string) (string, error)) ([]string, error) {
	missing := []string{}
	for _, object := range objects {
		stored, err := head(object.SHA256)
		switch {
		case errors.Is(err, objectstore.ErrNotFound):
			missing = append(missing, object.SHA256)
		case err != nil:
			return nil, err
		case stored != object.SHA256:
			// The blob is corrupt at a content-addressed key; the Agent can
			// repair it with a checksum-verified PUT.
			missing = append(missing, object.SHA256)
		}
	}
	return missing, nil
}

// SubmitManifest receives a Workspace publish's manifest (node-protocol.md
// "Workspace publish"). It writes the canonical manifest, replaces the pending
// version's object set, and replies with the objects the store still lacks.
func (n *nodeService) SubmitManifest(stream nodev1.Node_SubmitManifestServer) error {
	ctx := stream.Context()
	node, err := n.s.authenticateNode(ctx)
	if err != nil {
		return err
	}
	if n.s.objects == nil {
		return status.Error(codes.Unavailable, "object storage is not configured")
	}
	var acc manifestAccumulator
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if err := acc.add(chunk); err != nil {
			return status.Error(codes.InvalidArgument, err.Error())
		}
	}
	m, err := acc.finish()
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	versionID, err := uuid.Parse(acc.versionID)
	if err != nil {
		return status.Error(codes.InvalidArgument, "version_id must be a UUID")
	}
	pending, err := n.s.q.GetPendingWorkspacePublish(ctx, versionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return status.Error(codes.InvalidArgument, "version is not a pending Workspace publish")
	}
	if err != nil {
		n.s.log.Error("load pending publish", "version_id", versionID, "error", err)
		return status.Error(codes.Internal, "load pending publish")
	}
	if pending.NodeID == nil || *pending.NodeID != node.ID {
		return status.Error(codes.InvalidArgument, "the publish is not assigned to this Node")
	}

	address, err := manifest.Address(m)
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	canonical, err := manifest.Canonical(m)
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	if err := n.s.objects.PutManifest(ctx, pending.OwnerID.String(), address, canonical); err != nil {
		n.s.log.Error("store publish manifest", "version_id", versionID, "error", err)
		return status.Error(codes.Internal, "store manifest")
	}

	tx, err := n.s.pool.Begin(ctx)
	if err != nil {
		return status.Error(codes.Internal, "begin manifest write")
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	q := n.s.q.WithTx(tx)
	manifestKey := objectstore.ManifestKey(pending.OwnerID.String(), address)
	updated, err := q.SetResourceVersionManifest(ctx, store.SetResourceVersionManifestParams{
		ID: versionID, Address: &address, ManifestKey: &manifestKey, Size: manifest.TotalSize(m),
	})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && updated.State != string(ResourceVersionPending)) {
		return status.Error(codes.InvalidArgument, "version is not a pending Workspace publish")
	}
	if err != nil {
		n.s.log.Error("record publish manifest", "version_id", versionID, "error", err)
		return status.Error(codes.Internal, "record manifest")
	}
	if err := q.DeleteResourceVersionObjects(ctx, versionID); err != nil {
		n.s.log.Error("clear publish objects", "version_id", versionID, "error", err)
		return status.Error(codes.Internal, "record manifest objects")
	}
	for _, object := range manifest.FileObjects(m) {
		if err := q.InsertResourceVersionObject(ctx, store.InsertResourceVersionObjectParams{
			VersionID: versionID, Sha256: object.SHA256,
		}); err != nil {
			n.s.log.Error("link publish object", "version_id", versionID, "error", err)
			return status.Error(codes.Internal, "record manifest objects")
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return status.Error(codes.Internal, "commit manifest")
	}

	missing, err := missingObjects(manifest.FileObjects(m), func(digest string) (string, error) {
		return n.s.objects.HeadSHA256(ctx, pending.OwnerID.String(), digest)
	})
	if err != nil {
		n.s.log.Error("check publish objects", "version_id", versionID, "error", err)
		return status.Error(codes.Internal, "check objects")
	}
	return stream.SendAndClose(&nodev1.ManifestAck{MissingSha256: missing})
}

// handlePublishCommandResult finalizes a Workspace publish once its Agent
// reports (node-protocol.md step 3-4). A failed command or a failed final
// validation marks the version failed; only a fully verified object set makes
// it ready and moves the current pointer.
func (n *nodeService) handlePublishCommandResult(ctx context.Context, node store.Node, result *nodev1.CommandResult) {
	versionID, ok := publishVersionFromCommandID(result.GetCommandId())
	if !ok {
		return
	}
	pending, err := n.s.q.GetPendingWorkspacePublish(ctx, versionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return // already terminal: a redelivered command changes nothing
	}
	if err != nil {
		n.s.log.Error("load pending publish", "version_id", versionID, "error", err)
		return
	}
	if pending.NodeID == nil || *pending.NodeID != node.ID {
		n.s.log.Warn("publish result from a Node that does not own it",
			"version_id", versionID, "node_id", node.ID)
		return
	}
	if result.GetError() != "" {
		n.markPublishFailed(ctx, versionID)
		return
	}

	// Verify every object with HeadObject checksum mode (ADR-0012). Any error
	// fails the version rather than leaving it stuck pending: the Reconciler
	// would not redeliver a command the Node already reported.
	if n.s.objects == nil || pending.Address == nil {
		n.markPublishFailed(ctx, versionID)
		return
	}
	data, err := n.s.objects.GetManifest(ctx, pending.OwnerID.String(), *pending.Address)
	if err != nil {
		n.s.log.Warn("read publish manifest", "version_id", versionID, "error", err)
		n.markPublishFailed(ctx, versionID)
		return
	}
	var m manifest.Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		n.markPublishFailed(ctx, versionID)
		return
	}
	address, err := manifest.Address(m)
	if err != nil || address != *pending.Address {
		n.markPublishFailed(ctx, versionID)
		return
	}
	objects := manifest.FileObjects(m)
	missing, err := missingObjects(objects, func(digest string) (string, error) {
		return n.s.objects.HeadSHA256(ctx, pending.OwnerID.String(), digest)
	})
	if err != nil {
		n.s.log.Warn("verify publish objects", "version_id", versionID, "error", err)
		n.markPublishFailed(ctx, versionID)
		return
	}
	if len(missing) > 0 {
		n.s.log.Warn("publish objects are missing", "version_id", versionID, "missing", len(missing))
		n.markPublishFailed(ctx, versionID)
		return
	}

	tx, err := n.s.pool.Begin(ctx)
	if err != nil {
		n.s.log.Error("begin publish completion", "version_id", versionID, "error", err)
		n.markPublishFailed(ctx, versionID)
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	q := n.s.q.WithTx(tx)
	// Serialize pointer moves of one Resource (ADR-0012: last to become ready
	// wins).
	if _, err := q.LockResource(ctx, pending.ResourceID); err != nil {
		n.s.log.Error("lock Resource for publish", "resource_id", pending.ResourceID, "error", err)
		n.markPublishFailed(ctx, versionID)
		return
	}
	rows, err := q.MarkResourceVersionReady(ctx, versionID)
	if err != nil {
		n.s.log.Error("mark publish ready", "version_id", versionID, "error", err)
		return
	}
	if rows == 0 {
		return // another delivery already made it terminal
	}
	for _, object := range objects {
		if err := q.UpsertCASObject(ctx, store.UpsertCASObjectParams{
			OwnerID: pending.OwnerID, Sha256: object.SHA256, Size: object.Size,
		}); err != nil {
			n.s.log.Error("record CAS object", "version_id", versionID, "error", err)
			return
		}
	}
	if _, err := q.SetResourceCurrent(ctx, store.SetResourceCurrentParams{
		ID: pending.ResourceID, CurrentVersionID: &versionID,
	}); err != nil {
		n.s.log.Error("move current pointer", "resource_id", pending.ResourceID, "error", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		n.s.log.Error("commit publish completion", "version_id", versionID, "error", err)
		return
	}
	n.s.auditAs(ctx, nil, nil, "resource.version.publish", "resource_version", versionID.String(), map[string]any{
		"resource_id": pending.ResourceID.String(),
		"address":     *pending.Address,
		"size":        pending.Size,
		"state":       string(ResourceVersionReady),
	})
	n.s.changed(ctx)
}

// markPublishFailed ends a pending publish without touching the current
// pointer. It is a no-op once the version is terminal.
func (n *nodeService) markPublishFailed(ctx context.Context, versionID uuid.UUID) {
	cause := publishFailedCause
	rows, err := n.s.q.MarkResourceVersionFailed(ctx, store.MarkResourceVersionFailedParams{
		ID: versionID, Cause: &cause,
	})
	if err != nil {
		n.s.log.Error("mark publish failed", "version_id", versionID, "error", err)
		return
	}
	if rows == 0 {
		return
	}
	version, err := n.s.q.GetResourceVersion(ctx, versionID)
	if err != nil {
		n.s.log.Error("load failed publish", "version_id", versionID, "error", err)
		return
	}
	n.s.auditAs(ctx, nil, nil, "resource.version.publish", "resource_version", versionID.String(), map[string]any{
		"resource_id": version.ResourceID.String(),
		"cause":       publishFailedCause,
		"state":       string(ResourceVersionFailed),
	})
}
