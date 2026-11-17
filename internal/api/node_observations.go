package api

import (
	"context"
	"encoding/json"
	"net/netip"
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
	case nodeproto.SandboxPhaseRunning:
		rows, err := n.s.q.MarkSandboxReady(ctx, store.MarkSandboxReadyParams{
			ID: id, Generation: obs.Generation,
		})
		if err != nil {
			n.s.log.Warn("record SandboxObserved running", "sandbox_id", id, "error", err)
		}
		// Also on a re-report of a ready Sandbox: an adopted one, or one an
		// older Agent ran before the upgrade.
		node, err := n.s.q.GetNode(ctx, nodeID)
		if err == nil {
			err = n.s.q.SetSandboxAddress(ctx, store.SetSandboxAddressParams{
				ID: id, Generation: obs.Generation, Address: observedSandboxAddress(obs.Address, node.SandboxSubnet),
			})
		}
		if err != nil {
			n.s.log.Warn("record SandboxObserved address", "sandbox_id", id, "error", err)
		}
		if rows > 0 {
			n.s.reconcile.notify()
		}
	case nodeproto.SandboxPhaseCommitting:
		// The stop stays `stopping` until gone; a commit being retried shows
		// commit_stalled (runtime.md §16).
		conditions, err := json.Marshal(sandboxConditions{CommitStalled: obs.CommitRetrying})
		if err == nil {
			_, err = n.s.q.SetSandboxConditions(ctx, store.SetSandboxConditionsParams{
				ID: id, Generation: obs.Generation, Conditions: conditions,
			})
		}
		if err != nil {
			n.s.log.Warn("record SandboxObserved committing", "sandbox_id", id, "error", err)
		}
	case nodeproto.SandboxPhaseCreateFailed, nodeproto.SandboxPhaseCommitFailed:
		cause := obs.Cause
		if cause == "" {
			cause = "runtime_create_failed"
			if nodeproto.SandboxPhase(obs.Phase) == nodeproto.SandboxPhaseCommitFailed {
				cause = "commit_failed"
			}
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

// observedSandboxAddress validates a reported address at the trust boundary:
// it is kept only when IPv4 and inside the Node's Sandbox subnet, when known
// (runtime.md §13). Anything else reads as no address.
func observedSandboxAddress(raw string, subnet *string) string {
	addr, err := netip.ParseAddr(raw)
	if err != nil || !addr.Is4() {
		return ""
	}
	if subnet != nil {
		if p, err := netip.ParsePrefix(*subnet); err == nil && !p.Contains(addr) {
			return ""
		}
	}
	return addr.String()
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
	if sb, err := n.s.q.GetSandboxIncludingDeleted(ctx, id); err == nil && SandboxState(sb.State) == SandboxDeleted {
		n.s.reconcile.dropSandboxDisk(ctx, sb)
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
