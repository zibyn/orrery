package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	nodev1 "github.com/zibyn/orrery/gen/node/v1"
	"github.com/zibyn/orrery/internal/store"
)

// The Reconciler derives commands from desired state on every tick; the
// desired-state tables are the queue, there is no command table (ADR-0014).
// A NOTIFY only shortens the wait.
const (
	reconcileInterval = time.Second
	reconcileChannel  = "orrery_reconcile"
	// reconcileLockID guards the sweep across replicas (ADR-0014).
	reconcileLockID = 724001
)

type reconciler struct {
	s    *Server
	hub  *nodeHub
	wake chan struct{}
}

func newReconciler(s *Server) *reconciler {
	r := &reconciler{s: s, hub: newNodeHub(), wake: make(chan struct{}, 1)}
	r.hub.onConnect = r.notify
	return r
}

func (r *reconciler) notify() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *reconciler) run(ctx context.Context) {
	go r.listen(ctx)
	ticker := time.NewTicker(reconcileInterval)
	defer ticker.Stop()
	for {
		r.once(ctx)
		select {
		case <-ctx.Done():
			return
		case <-r.wake:
		case <-ticker.C:
		}
	}
}

// listen turns the NOTIFY sent by handlers after they commit into a wake. Any
// LISTEN failure falls back to the periodic tick.
func (r *reconciler) listen(ctx context.Context) {
	for ctx.Err() == nil {
		conn, err := r.s.pool.Acquire(ctx)
		if err != nil {
			if ctx.Err() == nil {
				r.s.log.Warn("reconciler LISTEN acquire", "error", err)
				time.Sleep(time.Second)
			}
			continue
		}
		if _, err := conn.Exec(ctx, "LISTEN "+reconcileChannel); err == nil {
			for {
				if _, err := conn.Conn().WaitForNotification(ctx); err != nil {
					break
				}
				r.notify()
			}
		}
		conn.Release()
		if ctx.Err() == nil {
			time.Sleep(time.Second)
		}
	}
}

func (r *reconciler) once(ctx context.Context) {
	// The advisory lock is transaction-scoped, so hold one for the whole sweep.
	tx, err := r.s.pool.Begin(ctx)
	if err != nil {
		if ctx.Err() == nil {
			r.s.log.Warn("reconcile: begin sweep", "error", err)
		}
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck // release the lock
	var locked bool
	if err := tx.QueryRow(ctx, "SELECT pg_try_advisory_xact_lock($1)", reconcileLockID).Scan(&locked); err != nil || !locked {
		return
	}
	sandboxes, err := r.s.q.ListReconcilableSandboxes(ctx)
	if err != nil {
		if ctx.Err() == nil {
			r.s.log.Warn("reconcile: list sandboxes", "error", err)
		}
		return
	}
	for _, sb := range sandboxes {
		switch sandboxStep(SandboxState(sb.State), SandboxDesiredState(sb.DesiredState)) {
		case sandboxStepPending:
			r.pending(ctx, sb)
		case sandboxStepCreate:
			r.create(ctx, sb)
		case sandboxStepStop:
			r.stop(ctx, sb)
		case sandboxStepDelete:
			r.stopForDelete(ctx, sb)
		}
	}
	r.deleteWorkspaces(ctx)
	r.workspaces(ctx)
	r.publishWorkspaces(ctx)
	r.builds(ctx)
	r.execs(ctx)
	r.idle(ctx)
	// Admission is last so its writes commit with the sweep and its own
	// capacity arithmetic sees everything this tick already released.
	if err := r.admit(ctx, tx); err != nil {
		if ctx.Err() == nil {
			r.s.log.Warn("reconcile: admit", "error", err)
		}
		return // the deferred Rollback discards a partial admission
	}
	if err := tx.Commit(ctx); err != nil && ctx.Err() == nil {
		r.s.log.Warn("reconcile: commit sweep", "error", err)
	}
}

// execs derives StartExec and CancelExec from the unavoidable truth of the
// execs table: Active rows are the queue (ADR-0014).
func (r *reconciler) execs(ctx context.Context) {
	rows, err := r.s.q.ListActiveExecs(ctx)
	if err != nil {
		if ctx.Err() == nil {
			r.s.log.Warn("reconcile: list execs", "error", err)
		}
		return
	}
	for _, row := range rows {
		e := row.Exec
		connected := row.SandboxNodeID != nil && r.hub.connected(*row.SandboxNodeID)
		delivered := row.SandboxNodeID != nil &&
			r.hub.wasSent(*row.SandboxNodeID, commandID("start-exec", e.ID, e.Generation))
		facts := execFacts{
			sandboxState:    SandboxState(row.SandboxState),
			desired:         SandboxDesiredState(row.SandboxDesiredState),
			cancelRequested: e.CancelRequestedAt.Valid,
			started:         e.StartedAt.Valid,
		}
		switch execStep(facts, connected, delivered) {
		case execRuntimeLost:
			// The Runtime is gone, so every remaining Exec failed with it.
			r.endExec(ctx, e.ID, ExecRuntimeLost)
		case execFinishCanceled:
			// The command never reached the Supervisor; there is nothing to
			// signal.
			r.endExec(ctx, e.ID, ExecCanceled)
		case execSendCancel:
			id := commandID("cancel-exec", e.ID, e.Generation)
			r.send(ctx, row.SandboxNodeID, id, &nodev1.ControlMessage{
				Message: &nodev1.ControlMessage_CancelExec{CancelExec: &nodev1.CancelExec{
					CommandId: id, ExecId: e.ID.String(),
				}},
			})
		case execCancelSandbox:
			// Stop and delete wait for the runtime to be gone; turn the active
			// Execs into cancel intents first (api.md §3).
			if _, err := r.s.q.CancelSandboxExecs(ctx, e.SandboxID); err != nil {
				r.s.log.Warn("reconcile: cancel execs", "sandbox_id", e.SandboxID, "error", err)
			}
		case execStart:
			r.startExec(ctx, row)
		}
	}
}

func (r *reconciler) startExec(ctx context.Context, row store.ListActiveExecsRow) {
	e := row.Exec
	var argv []string
	if err := json.Unmarshal(e.Argv, &argv); err != nil || len(argv) == 0 {
		r.endExec(ctx, e.ID, ExecStartFailed)
		return
	}
	env := map[string]string{}
	if len(e.Env) > 0 {
		if err := json.Unmarshal(e.Env, &env); err != nil {
			r.endExec(ctx, e.ID, ExecStartFailed)
			return
		}
	}
	// The Secret snapshot is decrypted per StartExec, so an Agent restart or a
	// repeated command still gets the values (schema.md, node-protocol.md).
	secretEnv, err := decryptSecretEnv(r.s.cfg.EncryptionKeys, row.SandboxOwnerID, row.SandboxSecretEnvCt)
	if err != nil {
		r.s.log.Warn("reconcile: decrypt Secrets", "exec_id", e.ID, "error", err)
		r.endExec(ctx, e.ID, ExecStartFailed)
		return
	}
	// The Sandbox Token travels in secret_env, not in CreateSandbox, so a
	// restarted Agent or a repeated command still receives it (ADR-0016).
	secret, err := sandboxTokenSecret(r.s.cfg.EncryptionKeys, row.SandboxOwnerID,
		row.SandboxTokenCt, row.SandboxTokenKeyID)
	if err != nil {
		r.s.log.Warn("reconcile: open Sandbox Token", "exec_id", e.ID, "error", err)
		r.endExec(ctx, e.ID, ExecStartFailed)
		return
	}
	if secretEnv == nil {
		secretEnv = make(map[string]string, 1)
	}
	secretEnv["ORRERY_SANDBOX_TOKEN"] = SandboxToken(e.SandboxID, secret)
	// ORRERY_URL is the Sandbox's only clue about where `api` lives
	// (node-protocol.md). An unconfigured deployment leaves it out.
	if r.s.cfg.AccessURL != "" {
		env["ORRERY_URL"] = r.s.cfg.AccessURL
	}
	var timeout int64
	if e.TimeoutSeconds != nil {
		timeout = int64(*e.TimeoutSeconds)
	}
	var pty *nodev1.PtySpec
	if hasPTY(e.Pty) {
		var size ptySize
		if err := json.Unmarshal(e.Pty, &size); err != nil {
			r.endExec(ctx, e.ID, ExecStartFailed)
			return
		}
		pty = &nodev1.PtySpec{Rows: int32(size.Rows), Cols: int32(size.Cols)}
	}
	id := commandID("start-exec", e.ID, e.Generation)
	r.send(ctx, row.SandboxNodeID, id, &nodev1.ControlMessage{
		Message: &nodev1.ControlMessage_StartExec{StartExec: &nodev1.StartExec{
			CommandId: id, ExecId: e.ID.String(), SandboxId: e.SandboxID.String(),
			Generation: e.Generation, Argv: argv, Env: env, SecretEnv: secretEnv, Cwd: e.Cwd,
			TimeoutSeconds: timeout, LogCap: r.s.cfg.ExecLogCap, Stdin: e.Stdin,
			Pty: pty, PtyIdleSeconds: r.s.cfg.PtyIdleSeconds,
		}},
	})
}

// endExec finishes one Exec from the Reconciler and logs a failed write; the
// shared write is finishExec.
func (r *reconciler) endExec(ctx context.Context, id uuid.UUID, cause ExecCause) {
	if err := finishExec(ctx, r.s.q, id, cause, nil, nil); err != nil {
		r.s.log.Warn("reconcile: finish Exec", "exec_id", id, "error", err)
	}
}

// pending provisions a Sandbox that the admission sweep has placed. Placement
// itself is the admission queue's job (admission.go); a Sandbox without a Node
// is still waiting for capacity.
func (r *reconciler) pending(ctx context.Context, sb store.Sandbox) {
	if sb.NodeID == nil {
		return
	}
	ws, err := r.s.q.GetSandboxWorkspace(ctx, sb.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		r.fail(ctx, sb, "runtime_create_failed")
		return
	}
	if err != nil {
		r.s.log.Warn("reconcile: workspace lookup", "sandbox_id", sb.ID, "error", err)
		return
	}
	if ws.NodeID == nil {
		if _, err := r.s.q.SetWorkspaceNode(ctx, store.SetWorkspaceNodeParams{
			ID: ws.ID, NodeID: sb.NodeID,
		}); err != nil {
			r.s.log.Warn("reconcile: bind workspace", "workspace_id", ws.ID, "error", err)
		}
		return
	}
	switch WorkspaceState(ws.State) {
	case WorkspaceProvisioning:
		// The Workspace sweep sends ProvisionWorkspace with its retry backoff.
	case WorkspaceReady:
		if _, err := r.s.q.SetSandboxProvisioning(ctx, sb.ID); err != nil {
			r.s.log.Warn("reconcile: mark sandbox provisioning", "sandbox_id", sb.ID, "error", err)
		}
	default:
		// failed, lost, deleting or deleted: nothing to mount.
		r.fail(ctx, sb, "runtime_create_failed")
	}
}

func (r *reconciler) create(ctx context.Context, sb store.Sandbox) {
	att, err := r.s.q.GetAttachment(ctx, sb.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		r.fail(ctx, sb, "runtime_create_failed")
		return
	}
	if err != nil {
		r.s.log.Warn("reconcile: attachment lookup", "sandbox_id", sb.ID, "error", err)
		return
	}
	if sb.NodeID == nil {
		return
	}
	gpus, err := r.s.q.ListSandboxGPUs(ctx, sb.ID)
	if err != nil {
		r.s.log.Warn("reconcile: list sandbox GPUs", "sandbox_id", sb.ID, "error", err)
		return
	}
	mounts, err := r.s.q.ListSandboxResourceMounts(ctx, sb.ID)
	if err != nil {
		r.s.log.Warn("reconcile: list sandbox Resource mounts", "sandbox_id", sb.ID, "error", err)
		return
	}
	resources := make([]*nodev1.ResourceMount, 0, len(mounts))
	for _, mount := range mounts {
		if mount.Address == nil {
			// A version without a manifest cannot be mounted; creation already
			// rejects non-ready versions, so this is defensive.
			continue
		}
		resources = append(resources, &nodev1.ResourceMount{Address: *mount.Address, Mount: mount.Mount})
	}
	id := commandID("create-sandbox", sb.ID, sb.Generation)
	r.send(ctx, sb.NodeID, id, &nodev1.ControlMessage{Message: &nodev1.ControlMessage_CreateSandbox{
		CreateSandbox: &nodev1.CreateSandbox{
			CommandId:   id,
			SandboxId:   sb.ID.String(),
			Generation:  sb.Generation,
			ImageDigest: sb.SnapshotDigest,
			Limits:      &nodev1.Limits{Cpu: sb.Cpu, Memory: sb.Memory},
			Workspace:   &nodev1.WorkspaceMount{WorkspaceId: att.WorkspaceID.String(), Mode: att.Mode},
			GpuUuids:    gpus,
			Resources:   resources,
		},
	}})
}

func (r *reconciler) stop(ctx context.Context, sb store.Sandbox) {
	if _, err := r.s.q.SetSandboxStopping(ctx, sb.ID); err != nil {
		r.s.log.Warn("reconcile: mark stopping", "sandbox_id", sb.ID, "error", err)
	}
	if sb.NodeID == nil {
		// Never placed: there is no runtime to stop.
		r.finalizeStopped(ctx, sb.ID)
		return
	}
	// Execs must finish before the runtime goes away, or their output is lost.
	// The exec sweep delivers the cancels; the next tick sends the stop.
	if !r.execsDrained(ctx, sb.ID) {
		return
	}
	r.sendStopRuntime(ctx, sb)
}

// sendStopRuntime asks the Node to tear down a Sandbox's runtime. Stopping and
// deleting share it; deletion finishes when the Node observes the runtime gone.
func (r *reconciler) sendStopRuntime(ctx context.Context, sb store.Sandbox) {
	id := commandID("stop-sandbox", sb.ID, sb.Generation)
	r.send(ctx, sb.NodeID, id, &nodev1.ControlMessage{Message: &nodev1.ControlMessage_StopSandbox{
		StopSandbox: &nodev1.StopSandbox{
			CommandId: id, SandboxId: sb.ID.String(), Generation: sb.Generation,
		},
	}})
}

// finalizeStopped releases the compute and the Attachment once the runtime is
// gone. It runs in the observed handler and for a never-placed Sandbox.
func (r *reconciler) finalizeStopped(ctx context.Context, sandboxID uuid.UUID) {
	tx, err := r.s.pool.Begin(ctx)
	if err != nil {
		r.s.log.Warn("reconcile: finalize stop", "sandbox_id", sandboxID, "error", err)
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	q := r.s.q.WithTx(tx)
	if _, err := q.SetSandboxStopped(ctx, sandboxID); err != nil {
		r.s.log.Warn("reconcile: mark stopped", "sandbox_id", sandboxID, "error", err)
		return
	}
	if err := releaseRuntime(ctx, q, sandboxID); err != nil {
		r.s.log.Warn("reconcile: release runtime", "sandbox_id", sandboxID, "error", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		r.s.log.Warn("reconcile: commit stop", "sandbox_id", sandboxID, "error", err)
	}
}

// stopForDelete tears down a Sandbox whose desired state is deleted. Without a
// Node there is nothing to stop; otherwise the Node's observed "gone" completes
// the deletion.
func (r *reconciler) stopForDelete(ctx context.Context, sb store.Sandbox) {
	if sb.NodeID == nil {
		// Nothing ever ran. Release the Runtime and let the implicit
		// Workspace's cleanup finish the Sandbox, or finish a persistent one
		// now (ADR-0018).
		if err := finalizeGone(ctx, r.s.q, sb.ID); err != nil {
			r.s.log.Warn("reconcile: finalize deleted Sandbox", "sandbox_id", sb.ID, "error", err)
		}
		return
	}
	if !r.execsDrained(ctx, sb.ID) {
		return
	}
	r.sendStopRuntime(ctx, sb)
}

// execsDrained marks a Sandbox's active Execs for cancellation and reports
// whether every one has finished. Commands run on a Sandbox being deleted are
// canceled, not killed with their logs (api.md §3, delete-cascade).
func (r *reconciler) execsDrained(ctx context.Context, sandboxID uuid.UUID) bool {
	if _, err := r.s.q.CancelSandboxExecs(ctx, sandboxID); err != nil {
		r.s.log.Warn("reconcile: cancel execs", "sandbox_id", sandboxID, "error", err)
		return false
	}
	active, err := r.s.q.CountActiveExecs(ctx, sandboxID)
	if err != nil {
		r.s.log.Warn("reconcile: count execs", "sandbox_id", sandboxID, "error", err)
		return false
	}
	return active == 0
}

// workspaceProvisionBackoff is the delay after each failed provisioning
// attempt (defaults.md: 1, 2, 4, 8, 16 seconds; the third failure raises
// provisioning_stalled).
var workspaceProvisionBackoff = []time.Duration{
	time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second,
}

const workspaceStalledAfter = 3

// workspaces provisions every Workspace whose Node is known, honoring the
// retry backoff. A persistent Workspace without a Node is placed first
// (ADR-0014: a paused Node takes no new Workspaces); an ephemeral one is bound
// by its Sandbox's admission.
func (r *reconciler) workspaces(ctx context.Context) {
	rows, err := r.s.q.ListProvisioningWorkspaces(ctx)
	if err != nil {
		if ctx.Err() == nil {
			r.s.log.Warn("reconcile: list provisioning Workspaces", "error", err)
		}
		return
	}
	now := time.Now()
	for _, ws := range rows {
		if ws.NodeID == nil {
			if ws.Kind == "persistent" {
				r.placeWorkspace(ctx, ws, now)
			}
			continue
		}
		if !parseWorkspaceConditions(ws.Conditions).provisionDue(now) {
			continue
		}
		id := commandID("provision-workspace", ws.ID, ws.Generation)
		r.send(ctx, ws.NodeID, id, &nodev1.ControlMessage{
			Message: &nodev1.ControlMessage_ProvisionWorkspace{
				ProvisionWorkspace: &nodev1.ProvisionWorkspace{
					CommandId:   id,
					WorkspaceId: ws.ID.String(),
					Generation:  ws.Generation,
				},
			},
		})
	}
}

func (r *reconciler) placeWorkspace(ctx context.Context, ws store.Workspace, now time.Time) {
	nodes, err := r.s.q.ListAllNodes(ctx)
	if err != nil {
		r.s.log.Warn("reconcile: place Workspace", "workspace_id", ws.ID, "error", err)
		return
	}
	for _, node := range nodes {
		if deriveNodeStatus(node, now) != "ready" || !r.hub.connected(node.ID) {
			continue
		}
		if _, err := r.s.q.SetWorkspaceNode(ctx, store.SetWorkspaceNodeParams{
			ID: ws.ID, NodeID: &node.ID,
		}); err != nil {
			r.s.log.Warn("reconcile: bind Workspace", "workspace_id", ws.ID, "error", err)
		}
		return
	}
}

// publishWorkspaces derives PublishWorkspace for every pending
// workspace-source ResourceVersion and sends it to the Workspace's bound Node.
// nodeHub clears its dedupe memory on reconnect, so an Agent restart redelivers
// the command and the walk converges (publish-agent-restart).
func (r *reconciler) publishWorkspaces(ctx context.Context) {
	rows, err := r.s.q.ListPendingWorkspacePublishes(ctx)
	if err != nil {
		if ctx.Err() == nil {
			r.s.log.Warn("reconcile: list pending publishes", "error", err)
		}
		return
	}
	for _, row := range rows {
		if row.NodeID == nil {
			continue
		}
		workspaceID, publishPath, ok := parseWorkspaceProvenance(row.Provenance)
		if !ok {
			r.s.log.Warn("reconcile: publish has no workspace provenance", "version_id", row.ID)
			continue
		}
		id := publishCommandID(row.ID)
		r.send(ctx, row.NodeID, id, &nodev1.ControlMessage{
			Message: &nodev1.ControlMessage_PublishWorkspace{
				PublishWorkspace: &nodev1.PublishWorkspace{
					CommandId:   id,
					VersionId:   row.ID.String(),
					WorkspaceId: workspaceID.String(),
					Path:        publishPath,
				},
			},
		})
	}
}

// idleExpired reports whether a ready Sandbox has been idle long enough to
// stop. A running Exec or a live data-plane connection is activity, and a
// Sandbox without any activity yet starts its idle clock now (ADR-0015).
func idleExpired(lastActivity time.Time, hasActivity bool, idleSeconds int32, now time.Time, activeExecs, liveConnections int) bool {
	if idleSeconds <= 0 || !hasActivity || activeExecs > 0 || liveConnections > 0 {
		return false
	}
	return !now.Before(lastActivity.Add(time.Duration(idleSeconds) * time.Second))
}

// idle stops Sandboxes whose Idle Stop period has elapsed.
func (r *reconciler) idle(ctx context.Context) {
	rows, err := r.s.q.ListIdleSandboxes(ctx)
	if err != nil {
		if ctx.Err() == nil {
			r.s.log.Warn("reconcile: list idle Sandboxes", "error", err)
		}
		return
	}
	now := time.Now()
	for _, sb := range rows {
		running, err := r.s.q.CountRunningExecs(ctx, sb.ID)
		if err != nil {
			r.s.log.Warn("reconcile: count Execs", "sandbox_id", sb.ID, "error", err)
			continue
		}
		live := r.s.activity.count(sb.ID)
		last, hasLast := sb.LastActivityAt.Time, sb.LastActivityAt.Valid
		// A request or Exec shorter than one sweep never appears as active,
		// so the tracker's timestamp is authoritative when it is newer.
		if tracked, ok := r.s.activity.lastActivity(sb.ID); ok && (!hasLast || tracked.After(last)) {
			last, hasLast = tracked, true
		}
		if idleExpired(last, hasLast, sb.IdleStopSeconds, now, int(running), live) {
			if _, err := r.s.q.StopSandbox(ctx, sb.ID); err != nil {
				if !errors.Is(err, pgx.ErrNoRows) {
					r.s.log.Warn("reconcile: idle stop", "sandbox_id", sb.ID, "error", err)
				}
			} else {
				r.s.log.Info("idle stop", "sandbox_id", sb.ID)
			}
			continue
		}
		// Activity keeps the clock moving; a Sandbox that has never been
		// active (predating the column) starts it now.
		if running > 0 || live > 0 || !sb.LastActivityAt.Valid {
			if err := r.s.q.TouchSandboxActivity(ctx, sb.ID); err != nil {
				r.s.log.Warn("reconcile: touch activity", "sandbox_id", sb.ID, "error", err)
			}
		}
	}
}

func (r *reconciler) deleteWorkspaces(ctx context.Context) {
	workspaces, err := r.s.q.ListDeletingWorkspaces(ctx)
	if err != nil {
		if ctx.Err() == nil {
			r.s.log.Warn("reconcile: list workspaces", "error", err)
		}
		return
	}
	for _, ws := range workspaces {
		if ws.NodeID == nil {
			r.finalizeWorkspace(ctx, ws)
			continue
		}
		id := commandID("delete-workspace", ws.ID, ws.Generation)
		r.send(ctx, ws.NodeID, id, &nodev1.ControlMessage{Message: &nodev1.ControlMessage_DeleteWorkspace{
			DeleteWorkspace: &nodev1.DeleteWorkspace{
				CommandId: id, WorkspaceId: ws.ID.String(), Generation: ws.Generation,
			},
		}})
	}
}

func (r *reconciler) finalizeWorkspace(ctx context.Context, ws store.Workspace) {
	if _, err := r.s.q.MarkWorkspaceDeleted(ctx, store.MarkWorkspaceDeletedParams{
		ID: ws.ID, Generation: ws.Generation,
	}); err != nil {
		r.s.log.Warn("reconcile: mark workspace deleted", "workspace_id", ws.ID, "error", err)
		return
	}
	if ws.SandboxID != nil {
		// A Workspace going away does not release a Sandbox that still runs
		// (ADR-0018); only its own deletion finishes it.
		if sb, err := r.s.q.GetSandbox(ctx, *ws.SandboxID); err == nil &&
			SandboxDesiredState(sb.DesiredState) == SandboxDesiredDeleted {
			if err := finalizeGone(ctx, r.s.q, *ws.SandboxID); err != nil {
				r.s.log.Warn("reconcile: finalize Sandbox", "sandbox_id", *ws.SandboxID, "error", err)
			}
		}
	}
}

func (r *reconciler) fail(ctx context.Context, sb store.Sandbox, cause string) {
	tx, err := r.s.pool.Begin(ctx)
	if err != nil {
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	q := r.s.q.WithTx(tx)
	rows, err := q.MarkSandboxFailed(ctx, store.MarkSandboxFailedParams{
		ID: sb.ID, Generation: sb.Generation, Cause: &cause,
	})
	if err != nil {
		r.s.log.Warn("reconcile: mark failed", "sandbox_id", sb.ID, "error", err)
		return
	}
	if rows > 0 {
		if err := releaseRuntime(ctx, q, sb.ID); err != nil {
			r.s.log.Warn("reconcile: release runtime", "sandbox_id", sb.ID, "error", err)
			return
		}
	}
	r.endAdmission(ctx, q, sb.ID, "canceled")
	if err := tx.Commit(ctx); err != nil {
		r.s.log.Warn("reconcile: commit failure", "sandbox_id", sb.ID, "error", err)
	}
}

func (r *reconciler) send(ctx context.Context, nodeID *uuid.UUID, commandID string, msg *nodev1.ControlMessage) {
	if nodeID == nil {
		return
	}
	r.hub.send(*nodeID, commandID, msg)
}

// commandID names one derived command. The Reconciler derives the same id for
// the same work, so the Node can ignore a duplicate delivery.
func commandID(prefix string, id uuid.UUID, generation int64) string {
	return fmt.Sprintf("%s:%s:%d", prefix, id, generation)
}

// nodeHub fans commands out to the Agent's Control stream. A command id that
// was already queued is not queued twice; a reconnect clears that memory so
// every command is derived and delivered again.
type nodeHub struct {
	mu        sync.Mutex
	conns     map[uuid.UUID]*agentConn
	sent      map[uuid.UUID]map[string]bool
	onConnect func()
}

type agentConn struct {
	nodeID uuid.UUID
	send   chan *nodev1.ControlMessage
	done   chan struct{}
	once   sync.Once
}

// close ends the Control stream; the handler returns UNAUTHENTICATED so the
// Agent clears its identity (ADR-0014).
func (c *agentConn) close() {
	c.once.Do(func() { close(c.done) })
}

func newNodeHub() *nodeHub {
	return &nodeHub{
		conns: make(map[uuid.UUID]*agentConn),
		sent:  make(map[uuid.UUID]map[string]bool),
	}
}

func (h *nodeHub) register(c *agentConn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.conns[c.nodeID] = c
	delete(h.sent, c.nodeID)
	if h.onConnect != nil {
		go h.onConnect()
	}
}

func (h *nodeHub) unregister(c *agentConn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.conns[c.nodeID] == c {
		delete(h.conns, c.nodeID)
	}
}

func (h *nodeHub) connected(nodeID uuid.UUID) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.conns[nodeID] != nil
}

// close ends a Node's live stream, if any, so the Agent reconnects with a
// cleared Token and is refused (ADR-0014).
func (h *nodeHub) close(nodeID uuid.UUID) {
	h.mu.Lock()
	conn := h.conns[nodeID]
	h.mu.Unlock()
	if conn != nil {
		conn.close()
	}
}

func (h *nodeHub) send(nodeID uuid.UUID, commandID string, msg *nodev1.ControlMessage) {
	h.mu.Lock()
	defer h.mu.Unlock()
	conn := h.conns[nodeID]
	if conn == nil {
		return
	}
	if h.sent[nodeID] == nil {
		h.sent[nodeID] = make(map[string]bool)
	}
	if h.sent[nodeID][commandID] {
		return
	}
	select {
	case conn.send <- msg:
		h.sent[nodeID][commandID] = true
	default: // retried on the next tick
	}
}

// direct delivers a command without the dedupe map. OpenTunnel is per-attach
// and carries a unique tunnel id, so a repeated delivery cannot apply twice.
func (h *nodeHub) direct(nodeID uuid.UUID, msg *nodev1.ControlMessage) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	conn := h.conns[nodeID]
	if conn == nil {
		return false
	}
	select {
	case conn.send <- msg:
		return true
	default: // the Agent is not reading; the attach fails and is retried by the client
		return false
	}
}

// wasSent reports whether a command id was delivered on the current connection.
func (h *nodeHub) wasSent(nodeID uuid.UUID, commandID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sent[nodeID][commandID]
}

// failed forgets a delivered command so the next sweep sends it again.
func (h *nodeHub) failed(nodeID uuid.UUID, commandID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.sent[nodeID], commandID)
}
