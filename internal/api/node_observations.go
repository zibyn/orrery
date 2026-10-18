package api

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	nodev1 "github.com/zibyn/stars-sandbox/gen/node/v1"
	"github.com/zibyn/stars-sandbox/internal/nodeproto"
	"github.com/zibyn/stars-sandbox/internal/store"
)

// recordSandboxObserved applies what the Agent saw with the generation fence:
// an observation from an older generation is dropped (ADR-0015).
func (n *nodeService) recordSandboxObserved(ctx context.Context, nodeID uuid.UUID, obs *nodev1.SandboxObserved) {
	id, err := uuid.Parse(obs.SandboxId)
	if err != nil {
		return
	}
	// Only the Node the Sandbox is bound to may report on it (ADR-0010).
	sb, err := n.s.q.GetSandbox(ctx, id)
	if err != nil || sb.NodeID == nil || *sb.NodeID != nodeID {
		return
	}
	switch nodeproto.SandboxPhase(obs.Phase) {
	case nodeproto.SandboxPhaseFetchingResources:
		// The Node is pulling Resource version trees; the condition is public
		// until the Runtime is running (node-protocol.md, runtime.md §11).
		if _, err := n.s.q.SetSandboxConditions(ctx, store.SetSandboxConditionsParams{
			ID: id, Generation: obs.Generation,
			Conditions: encodeSandboxConditions(sandboxConditions{FetchingResources: true}),
		}); err != nil {
			n.s.log.Warn("record SandboxObserved fetching_resources", "sandbox_id", id, "error", err)
		}
	case nodeproto.SandboxPhaseRunning:
		rows, err := n.s.q.MarkSandboxReady(ctx, store.MarkSandboxReadyParams{
			ID: id, Generation: obs.Generation,
		})
		if err != nil {
			n.s.log.Warn("record SandboxObserved running", "sandbox_id", id, "error", err)
		}
		if rows > 0 {
			n.s.reconcile.notify()
		}
	case nodeproto.SandboxPhaseCreateFailed:
		cause := obs.Cause
		if cause == "" {
			cause = "runtime_create_failed"
		}
		rows, err := n.s.q.MarkSandboxFailed(ctx, store.MarkSandboxFailedParams{
			ID: id, Generation: obs.Generation, Cause: &cause,
		})
		if err != nil {
			n.s.log.Warn("record SandboxObserved create_failed", "sandbox_id", id, "error", err)
			return
		}
		if rows > 0 {
			n.releaseCompute(ctx, id)
			n.s.reconcile.notify()
		}
	case nodeproto.SandboxPhaseGone:
		n.recordSandboxGone(ctx, id, obs.Generation)
	}
}

func (n *nodeService) recordSandboxGone(ctx context.Context, id uuid.UUID, generation int64) {
	// The runtime can no longer produce output; an Exec still marked active is
	// lost with it (api.md §4).
	if _, err := n.s.q.FinishSandboxExecs(ctx, store.FinishSandboxExecsParams{
		SandboxID: id, Cause: strPtr("runtime_lost"), Generation: generation,
	}); err != nil {
		n.s.log.Warn("finish lost Execs", "sandbox_id", id, "error", err)
	}
	stopped, err := n.s.q.MarkSandboxStopped(ctx, store.MarkSandboxStoppedParams{
		ID: id, Generation: generation,
	})
	if err != nil {
		n.s.log.Warn("record SandboxObserved gone", "sandbox_id", id, "error", err)
		return
	}
	lost, err := n.s.q.MarkSandboxRuntimeLost(ctx, store.MarkSandboxRuntimeLostParams{
		ID: id, Generation: generation, Cause: strPtr("runtime_lost"),
	})
	if err != nil {
		n.s.log.Warn("record runtime lost", "sandbox_id", id, "error", err)
		return
	}
	if stopped > 0 || lost > 0 {
		n.releaseCompute(ctx, id)
		n.s.reconcile.notify()
		return
	}
	sb, err := n.s.q.GetSandbox(ctx, id)
	if err != nil {
		return
	}
	// A Sandbox failed by a Workspace loss keeps its failed cause; the Runtime
	// being gone is what releases its compute (ADR-0017).
	if SandboxState(sb.State) == SandboxFailed {
		n.releaseCompute(ctx, id)
		n.s.reconcile.notify()
		return
	}
	// A deleted Sandbox waits for its runtime to be gone before the Workspace
	// can be removed.
	if SandboxDesiredState(sb.DesiredState) != SandboxDesiredDeleted {
		return
	}
	tx, err := n.s.pool.Begin(ctx)
	if err != nil {
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	q := n.s.q.WithTx(tx)
	if err := releaseCompute(ctx, q, id); err != nil {
		n.s.log.Warn("release compute", "sandbox_id", id, "error", err)
		return
	}
	ws, wsErr := q.GetSandboxWorkspace(ctx, id)
	switch {
	case wsErr == nil && ws.Kind == "ephemeral":
		if _, err := q.SetWorkspaceDeleting(ctx, ws.ID); err != nil {
			n.s.log.Warn("mark workspace deleting", "workspace_id", ws.ID, "error", err)
			return
		}
	case wsErr != nil && !errors.Is(wsErr, pgx.ErrNoRows):
		// A transient read error must not orphan an implicit Workspace; the
		// next sweep sends the stop again.
		n.s.log.Warn("look up Workspace for teardown", "sandbox_id", id, "error", wsErr)
		return
	default:
		if _, err := q.MarkSandboxDeleted(ctx, id); err != nil {
			n.s.log.Warn("mark Sandbox deleted", "sandbox_id", id, "error", err)
			return
		}
	}
	if err := tx.Commit(ctx); err != nil {
		n.s.log.Warn("commit Sandbox teardown", "sandbox_id", id, "error", err)
		return
	}
	n.s.reconcile.notify()
}

// recordExecObserved stamps the execve time and its display-only deadline.
func (n *nodeService) recordExecObserved(ctx context.Context, nodeID uuid.UUID, obs *nodev1.ExecObserved) {
	id, err := uuid.Parse(obs.ExecId)
	if err != nil {
		return
	}
	row, err := n.s.q.GetExecWithNode(ctx, id)
	if err != nil || row.SandboxNodeID == nil || *row.SandboxNodeID != nodeID {
		return
	}
	started, err := time.Parse(time.RFC3339Nano, obs.StartedAt)
	if err != nil {
		n.s.log.Warn("parse Exec started_at", "exec_id", id, "error", err)
		return
	}
	if err := n.s.q.SetExecStarted(ctx, store.SetExecStartedParams{
		ID: id, StartedAt: pgtype.Timestamptz{Time: started, Valid: true},
	}); err != nil {
		n.s.log.Warn("record Exec started", "exec_id", id, "error", err)
	}
}

func (n *nodeService) releaseCompute(ctx context.Context, id uuid.UUID) {
	if err := releaseCompute(ctx, n.s.q, id); err != nil {
		n.s.log.Warn("release compute", "sandbox_id", id, "error", err)
	}
}

func (n *nodeService) recordWorkspaceObserved(ctx context.Context, nodeID uuid.UUID, obs *nodev1.WorkspaceObserved) {
	id, err := uuid.Parse(obs.WorkspaceId)
	if err != nil {
		return
	}
	ws, err := n.s.q.GetWorkspace(ctx, id)
	if err != nil || ws.NodeID == nil || *ws.NodeID != nodeID {
		return
	}
	switch nodeproto.WorkspacePhase(obs.Phase) {
	case nodeproto.WorkspacePhaseReady:
		rows, err := n.s.q.MarkWorkspaceReady(ctx, store.MarkWorkspaceReadyParams{
			ID: id, Generation: obs.Generation,
		})
		if err != nil {
			n.s.log.Warn("record WorkspaceObserved ready", "workspace_id", id, "error", err)
			return
		}
		if rows > 0 {
			if err := n.s.q.SetWorkspaceConditions(ctx, store.SetWorkspaceConditionsParams{ID: id}); err != nil {
				n.s.log.Warn("clear Workspace conditions", "workspace_id", id, "error", err)
			}
			n.s.reconcile.notify()
		}
	case nodeproto.WorkspacePhaseGone:
		rows, err := n.s.q.MarkWorkspaceDeleted(ctx, store.MarkWorkspaceDeletedParams{
			ID: id, Generation: obs.Generation,
		})
		if err != nil {
			n.s.log.Warn("record WorkspaceObserved gone", "workspace_id", id, "error", err)
			return
		}
		if rows > 0 {
			if ws, err := n.s.q.GetWorkspace(ctx, id); err == nil && ws.SandboxID != nil {
				if _, err := n.s.q.MarkSandboxDeleted(ctx, *ws.SandboxID); err != nil {
					n.s.log.Warn("mark sandbox deleted", "sandbox_id", *ws.SandboxID, "error", err)
				}
			}
			n.s.reconcile.notify()
		}
	}
}
