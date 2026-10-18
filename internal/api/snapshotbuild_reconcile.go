package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	nodev1 "github.com/zibyn/stars-sandbox/gen/node/v1"
	"github.com/zibyn/stars-sandbox/internal/registry"
	"github.com/zibyn/stars-sandbox/internal/store"
)

// --- Reconciler ---------------------------------------------------------

// builds derives BuildSnapshot and build cleanup from snapshot_versions. It is
// the build counterpart of the execs sweep: the desired-state tables are the
// queue (ADR-0014).
func (r *reconciler) builds(ctx context.Context) {
	rows, err := r.s.q.ListBuildRuns(ctx)
	if err != nil {
		if ctx.Err() == nil {
			r.s.log.Warn("reconcile: list builds", "error", err)
		}
		return
	}
	now := time.Now()
	for _, row := range rows {
		r.build(ctx, row, now)
	}
}

func (r *reconciler) build(ctx context.Context, row store.ListBuildRunsRow, now time.Time) {
	version, sb := row.SnapshotVersion, row.Sandbox
	if version.State != string(SnapshotVersionBuilding) {
		r.cleanupBuild(ctx, row, execCauseForVersion(version))
		return
	}
	if sb.State == string(SandboxFailed) || sb.State == string(SandboxStopped) || sb.State == string(SandboxDeleted) {
		r.failBuild(ctx, row, versionCauseForSandbox(sb))
		return
	}
	if sb.NodeID == nil {
		return // waiting for admission
	}
	node, err := r.s.q.GetNode(ctx, *sb.NodeID)
	if err == nil && node.LostAt.Valid {
		r.failBuild(ctx, row, "node_lost")
		return
	}
	if row.ExecCancelRequestedAt.Valid {
		if _, err := r.s.q.CancelSnapshotVersion(ctx, version.ID); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			r.s.log.Warn("reconcile: cancel build version", "version_id", version.ID, "error", err)
		}
		r.cleanupBuild(ctx, row, "canceled")
		return
	}
	if buildExpired(row, now) {
		r.failBuild(ctx, row, "timed_out")
		return
	}
	cmd, err := r.buildCommand(ctx, row)
	if err != nil {
		r.s.log.Warn("reconcile: derive build command", "version_id", version.ID, "error", err)
		r.failBuild(ctx, row, "build_failed")
		return
	}
	if !row.ExecStartedAt.Valid {
		// The build Exec never runs through the Supervisor; the sweep stamps
		// its start so the API and the timeout have a clock.
		if err := r.s.q.SetExecStarted(ctx, store.SetExecStartedParams{
			ID: row.ExecID, StartedAt: timestamptzPtr(&now),
		}); err != nil {
			r.s.log.Warn("reconcile: start build Exec", "exec_id", row.ExecID, "error", err)
			return
		}
	}
	// The build row's generation is the stable token: SnapshotVersions have no
	// generation of their own, and this row never changes it.
	id := commandID("build-snapshot", version.ID, row.ExecGeneration)
	r.send(ctx, sb.NodeID, id, &nodev1.ControlMessage{
		Message: &nodev1.ControlMessage_BuildSnapshot{BuildSnapshot: cmd},
	})
}

// failBuild moves a still-building version to failed with cause and releases
// the build. MarkSnapshotVersionFailed is conditional on building, so a cancel
// or observation that won the race is left alone.
func (r *reconciler) failBuild(ctx context.Context, row store.ListBuildRunsRow, cause string) {
	version := row.SnapshotVersion
	if _, err := r.s.q.MarkSnapshotVersionFailed(ctx, store.MarkSnapshotVersionFailedParams{
		ID: version.ID, Cause: &cause,
	}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		r.s.log.Warn("reconcile: fail build version", "version_id", version.ID, "error", err)
	}
	r.cleanupBuild(ctx, row, execCauseForVersionCause(cause))
}

// buildCommand assembles the wire command from the rows. Every field is
// re-derived on every tick, so a reconnect redelivers the same work.
func (r *reconciler) buildCommand(ctx context.Context, row store.ListBuildRunsRow) (*nodev1.BuildSnapshot, error) {
	version, sb := row.SnapshotVersion, row.Sandbox
	var request storedBuildRequest
	if err := json.Unmarshal(version.BuildRequest, &request); err != nil || request.Dockerfile == "" {
		return nil, fmt.Errorf("build request is missing or malformed: %w", err)
	}
	mounts, err := r.s.q.ListSandboxResourceMounts(ctx, sb.ID)
	if err != nil {
		return nil, err
	}
	contexts := make([]*nodev1.BuildContext, 0, len(mounts))
	for _, mount := range mounts {
		if mount.Address == nil {
			return nil, fmt.Errorf("context %q has no manifest address", mount.Mount)
		}
		contexts = append(contexts, &nodev1.BuildContext{Name: mount.Mount, Address: *mount.Address})
	}
	// Secrets are decrypted only when the command is sent, the same rule as
	// StartExec.secret_env (schema.md, node-protocol.md).
	secretEnv, err := decryptSecretEnv(r.s.cfg.EncryptionKeys, sb.OwnerID, sb.SecretEnvCt)
	if err != nil {
		return nil, fmt.Errorf("decrypt build Secrets: %w", err)
	}
	ids := make([]string, 0, len(secretEnv))
	for id := range secretEnv {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	secrets := make([]*nodev1.BuildSecret, 0, len(ids))
	for _, id := range ids {
		secrets = append(secrets, &nodev1.BuildSecret{Id: id, Value: secretEnv[id]})
	}
	snapshot, err := r.s.q.GetSnapshot(ctx, version.SnapshotID)
	if err != nil {
		return nil, err
	}
	if r.s.cfg.RegistryAddr == "" {
		return nil, errors.New("the platform registry is not configured")
	}
	timeout := int64(r.s.cfg.BuildTimeout / time.Second)
	if row.ExecTimeoutSeconds != nil && *row.ExecTimeoutSeconds > 0 {
		timeout = int64(*row.ExecTimeoutSeconds)
	}
	pushRepository := strings.TrimSuffix(r.s.cfg.RegistryAddr, "/") + "/" +
		registry.Repo(snapshot.ID, platformName(snapshot))
	return &nodev1.BuildSnapshot{
		CommandId:      commandID("build-snapshot", version.ID, row.ExecGeneration),
		VersionId:      version.ID.String(),
		SandboxId:      sb.ID.String(),
		ExecId:         row.ExecID.String(),
		Dockerfile:     request.Dockerfile,
		BaseDigest:     sb.SnapshotDigest,
		Contexts:       contexts,
		Secrets:        secrets,
		Limits:         &nodev1.Limits{Cpu: sb.Cpu, Memory: sb.Memory},
		TimeoutSeconds: timeout,
		PushRepository: pushRepository,
	}, nil
}

// cleanupBuild stops the build on the Node (best effort) and releases the
// build row once its version is terminal. Every step is idempotent, so the
// sweep retries until it succeeds.
func (r *reconciler) cleanupBuild(ctx context.Context, row store.ListBuildRunsRow, cause string) {
	if !row.ExecFinishedAt.Valid {
		id := commandID("cancel-exec", row.ExecID, row.ExecGeneration)
		r.send(ctx, row.Sandbox.NodeID, id, &nodev1.ControlMessage{
			Message: &nodev1.ControlMessage_CancelExec{CancelExec: &nodev1.CancelExec{
				CommandId: id, ExecId: row.ExecID.String(),
			}},
		})
	}
	tx, err := r.s.pool.Begin(ctx)
	if err != nil {
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	q := r.s.q.WithTx(tx)
	if !row.ExecFinishedAt.Valid {
		if _, err := q.FinishExec(ctx, store.FinishExecParams{
			ID: row.ExecID, Cause: &cause,
		}); err != nil {
			r.s.log.Warn("reconcile: finish build Exec", "exec_id", row.ExecID, "error", err)
			return
		}
	}
	if err := releaseCompute(ctx, q, row.Sandbox.ID); err != nil {
		r.s.log.Warn("reconcile: release build compute", "sandbox_id", row.Sandbox.ID, "error", err)
		return
	}
	if _, err := q.MarkBuildSandboxDone(ctx, row.Sandbox.ID); err != nil {
		r.s.log.Warn("reconcile: finish build Sandbox", "sandbox_id", row.Sandbox.ID, "error", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		r.s.log.Warn("reconcile: commit build cleanup", "version_id", row.SnapshotVersion.ID, "error", err)
	}
}

// buildExpired applies the deployment timeout while the Exec is still running
// (after SetExecStarted) or while it waits to start.
func buildExpired(row store.ListBuildRunsRow, now time.Time) bool {
	if row.ExecDeadlineAt.Valid {
		return !now.Before(row.ExecDeadlineAt.Time)
	}
	if row.ExecTimeoutSeconds != nil && *row.ExecTimeoutSeconds > 0 {
		return !now.Before(row.ExecAcceptedAt.Time.Add(time.Duration(*row.ExecTimeoutSeconds) * time.Second))
	}
	return false
}

// versionCauseForSandbox maps a failed build Sandbox to its SnapshotVersion
// cause (api.md §1's closed set).
func versionCauseForSandbox(sb store.Sandbox) string {
	if sb.Cause != nil {
		switch *sb.Cause {
		case "admission_timeout":
			return "timed_out"
		case "node_lost":
			return "node_lost"
		}
	}
	return "build_failed"
}

func execCauseForVersion(version store.SnapshotVersion) string {
	if version.State == string(SnapshotVersionCancelled) {
		return "canceled"
	}
	if version.State == string(SnapshotVersionFailed) && version.Cause != nil {
		return execCauseForVersionCause(*version.Cause)
	}
	return "exited"
}

func execCauseForVersionCause(cause string) string {
	switch cause {
	case "timed_out":
		return "timed_out"
	case "build_interrupted":
		return "runtime_lost"
	case "node_lost":
		return "node_lost"
	default:
		return "exited"
	}
}
