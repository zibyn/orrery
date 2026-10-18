package api

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	nodev1 "github.com/zibyn/stars-sandbox/gen/node/v1"
	"github.com/zibyn/stars-sandbox/internal/registry"
	"github.com/zibyn/stars-sandbox/internal/store"
)

// --- BuildObserved ------------------------------------------------------

// recordBuildObserved applies the Agent's terminal build result. A stale
// observation (the version already failed, timed out or was cancelled) is
// dropped: only a building version can move.
func (n *nodeService) recordBuildObserved(ctx context.Context, nodeID uuid.UUID, obs *nodev1.BuildObserved) {
	versionID, err := uuid.Parse(obs.VersionId)
	if err != nil {
		return
	}
	version, err := n.s.q.GetSnapshotVersionByID(ctx, versionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return
	}
	if err != nil {
		n.s.log.Warn("record BuildObserved", "version_id", versionID, "error", err)
		return
	}
	if version.State != string(SnapshotVersionBuilding) || version.BuildSandboxID == nil || version.BuildExecID == nil {
		return
	}
	sb, err := n.s.q.GetSandboxIncludingDeleted(ctx, *version.BuildSandboxID)
	if err != nil || sb.Kind != string(SandboxKindBuild) || sb.NodeID == nil || *sb.NodeID != nodeID {
		return
	}
	if obs.Cause != "" {
		cause := buildFailureCause(obs.Cause)
		n.finishBuildObserved(ctx, version, sb, nil, cause)
		return
	}
	if obs.Digest == "" {
		n.s.log.Warn("BuildObserved carries neither a digest nor a cause", "version_id", versionID)
		return
	}
	if !digestPattern.MatchString(obs.Digest) {
		n.s.log.Warn("BuildObserved digest is malformed", "version_id", versionID, "digest", obs.Digest)
		n.finishBuildObserved(ctx, version, sb, nil, "build_failed")
		return
	}
	snapshot, err := n.s.q.GetSnapshot(ctx, version.SnapshotID)
	if err != nil {
		n.s.log.Warn("record BuildObserved Snapshot", "version_id", versionID, "error", err)
		return
	}
	// The Agent pushed to the registry the Node reaches; verify the manifest
	// through the Control Plane's push address before moving the pointer.
	ref := registry.Ref(n.s.cfg.RegistryPushAddr,
		registry.Repo(snapshot.ID, platformName(snapshot)), obs.Digest)
	verifyCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := registry.VerifyDigest(verifyCtx, ref, n.s.cfg.RegistryCredentialDoc); err != nil {
		n.s.log.Warn("build digest is not in the platform registry", "version_id", versionID, "error", err)
		n.finishBuildObserved(ctx, version, sb, nil, "build_failed")
		return
	}
	mounts, err := n.s.q.ListSandboxResourceMounts(ctx, sb.ID)
	if err != nil {
		n.s.log.Warn("record BuildObserved contexts", "version_id", versionID, "error", err)
		return
	}
	contexts := make([]buildContext, 0, len(mounts))
	for _, mount := range mounts {
		address := ""
		if mount.Address != nil {
			address = *mount.Address
		}
		contexts = append(contexts, buildContext{Name: mount.Mount, Address: address})
	}
	provenance, err := json.Marshal(buildProvenance{
		FromDigests: append([]string{}, obs.FromDigests...), Contexts: contexts,
	})
	if err != nil {
		n.s.log.Warn("encode build provenance", "version_id", versionID, "error", err)
		return
	}
	n.finishBuildObserved(ctx, version, sb, &store.MarkSnapshotVersionReadyParams{
		ID: version.ID, Digest: &obs.Digest, Provenance: provenance,
	}, "")
}

var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func buildFailureCause(cause string) string {
	switch cause {
	case "timed_out", "build_interrupted", "node_lost", "build_failed":
		return cause
	default:
		return "build_failed"
	}
}

// finishBuildObserved commits the version transition, the current pointer,
// the Exec result and the Allocation release together. A conditional update
// losing the race to a cancel or timeout writes nothing and is dropped.
func (n *nodeService) finishBuildObserved(ctx context.Context, version store.SnapshotVersion,
	sb store.Sandbox, ready *store.MarkSnapshotVersionReadyParams, cause string) {
	tx, err := n.s.pool.Begin(ctx)
	if err != nil {
		n.s.log.Warn("begin build observation", "version_id", version.ID, "error", err)
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	q := n.s.q.WithTx(tx)
	if ready != nil {
		if _, err := q.MarkSnapshotVersionReady(ctx, *ready); err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				n.s.log.Warn("mark build ready", "version_id", version.ID, "error", err)
			}
			return
		}
		if _, err := q.SetSnapshotCurrentVersion(ctx, store.SetSnapshotCurrentVersionParams{
			ID: version.SnapshotID, VersionID: &version.ID,
		}); err != nil {
			n.s.log.Warn("move Snapshot current pointer", "version_id", version.ID, "error", err)
			return
		}
	} else {
		if _, err := q.MarkSnapshotVersionFailed(ctx, store.MarkSnapshotVersionFailedParams{
			ID: version.ID, Cause: &cause,
		}); err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				n.s.log.Warn("mark build failed", "version_id", version.ID, "error", err)
			}
			return
		}
	}
	execCause := execCauseForVersionCause(cause)
	if ready != nil {
		execCause = ExecExited
	}
	if err := finishExec(ctx, q, *version.BuildExecID, execCause, nil, nil); err != nil {
		n.s.log.Warn("finish build Exec", "exec_id", *version.BuildExecID, "error", err)
		return
	}
	if err := releaseRuntime(ctx, q, sb.ID); err != nil {
		n.s.log.Warn("release build compute", "sandbox_id", sb.ID, "error", err)
		return
	}
	if _, err := q.MarkBuildSandboxDone(ctx, sb.ID); err != nil {
		n.s.log.Warn("finish build Sandbox", "sandbox_id", sb.ID, "error", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		n.s.log.Warn("commit build observation", "version_id", version.ID, "error", err)
		return
	}
	action := "snapshot.version.build.failed"
	detail := map[string]any{"version": version.ID.String(), "cause": cause}
	if ready != nil {
		action = "snapshot.version.build"
		detail["digest"] = *ready.Digest
	}
	owner := snapshotOwner(ctx, n.s.q, version.SnapshotID)
	n.s.auditAs(ctx, owner, nil, action, "snapshot", version.SnapshotID.String(), detail)
	n.s.reconcile.notify()
}

// snapshotOwner resolves the User a Snapshot belongs to, for audit records
// written from an Agent observation (which carries no principal).
func snapshotOwner(ctx context.Context, q *store.Queries, snapshotID uuid.UUID) *uuid.UUID {
	snap, err := q.GetSnapshot(ctx, snapshotID)
	if err != nil || snap.OwnerID == nil {
		return nil
	}
	return snap.OwnerID
}
