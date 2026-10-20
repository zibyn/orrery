package api

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	nodev1 "github.com/zibyn/orrery/gen/node/v1"
	"github.com/zibyn/orrery/internal/nodeproto"
	"github.com/zibyn/orrery/internal/store"
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
			if err := releaseRuntime(ctx, n.s.q, id); err != nil {
				n.s.log.Warn("release runtime", "sandbox_id", id, "error", err)
			}
			n.s.reconcile.notify()
		}
	case nodeproto.SandboxPhaseGone:
		n.recordSandboxGone(ctx, id, obs.Generation)
	}
}

func (n *nodeService) recordSandboxGone(ctx context.Context, id uuid.UUID, generation int64) {
	// The runtime can no longer produce output; an Exec still marked active is
	// lost with it (api.md §4).
	if err := finishSandboxExecs(ctx, n.s.q, id, ExecRuntimeLost, generation); err != nil {
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
	// A stale observation (a restart bumped the generation) must not release a
	// live Runtime's compute; only a transition that matched the generation, a
	// failed Sandbox, or a deletion waiting for the Runtime has work left.
	if stopped == 0 && lost == 0 {
		sb, err := n.s.q.GetSandbox(ctx, id)
		if err != nil {
			return
		}
		if SandboxState(sb.State) != SandboxFailed &&
			SandboxDesiredState(sb.DesiredState) != SandboxDesiredDeleted {
			return
		}
	}
	// finalizeGone releases the compute and finishes a deletion; it covers the
	// stopped, runtime-lost, failed and already-gone cases alike.
	tx, err := n.s.pool.Begin(ctx)
	if err != nil {
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	if err := finalizeGone(ctx, n.s.q.WithTx(tx), id); err != nil {
		n.s.log.Warn("finalize gone Sandbox", "sandbox_id", id, "error", err)
		return
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
				// Only a Sandbox being deleted finishes with its Workspace; a
				// running one keeps its Runtime and compute (ADR-0018).
				if sb, err := n.s.q.GetSandbox(ctx, *ws.SandboxID); err == nil &&
					SandboxDesiredState(sb.DesiredState) == SandboxDesiredDeleted {
					if err := finalizeGone(ctx, n.s.q, *ws.SandboxID); err != nil {
						n.s.log.Warn("finalize Sandbox for gone Workspace", "sandbox_id", *ws.SandboxID, "error", err)
					}
				}
			}
			n.s.reconcile.notify()
		}
	}
}
