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

	nodev1 "github.com/zibyn/stars-sandbox/gen/node/v1"
	"github.com/zibyn/stars-sandbox/internal/store"
)

// The Reconciler derives commands from desired state on every tick; the
// desired-state tables are the queue, there is no command table (ADR-0014).
// A NOTIFY only shortens the wait.
const (
	reconcileInterval = time.Second
	reconcileChannel  = "stars_reconcile"
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
		switch sb.DesiredState {
		case "running":
			switch sb.State {
			case "pending":
				r.pending(ctx, sb)
			case "provisioning":
				r.create(ctx, sb)
			}
		case "stopped":
			r.stop(ctx, sb)
		case "deleted":
			r.delete(ctx, sb)
		}
	}
	r.deleteWorkspaces(ctx)
}

// pending places a Sandbox on a Node, provisions its Workspace there, and then
// hands off to create on the next tick.
func (r *reconciler) pending(ctx context.Context, sb store.Sandbox) {
	ws, err := r.s.q.GetSandboxWorkspace(ctx, &sb.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		r.fail(ctx, sb, "runtime_create_failed")
		return
	}
	if err != nil {
		r.s.log.Warn("reconcile: workspace lookup", "sandbox_id", sb.ID, "error", err)
		return
	}
	if sb.NodeID == nil {
		nodeID, ok := r.place(ctx, sb)
		if !ok {
			return // stays pending; blocked_reason is derived on read
		}
		if err := r.bind(ctx, sb, ws, nodeID); err != nil {
			r.s.log.Warn("reconcile: bind sandbox", "sandbox_id", sb.ID, "error", err)
		}
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
	if ws.State == "provisioning" {
		r.send(ctx, ws.NodeID, workspaceCommandId("provision-workspace", ws),
			&nodev1.ControlMessage{Message: &nodev1.ControlMessage_ProvisionWorkspace{
				ProvisionWorkspace: &nodev1.ProvisionWorkspace{
					CommandId:   workspaceCommandId("provision-workspace", ws),
					WorkspaceId: ws.ID.String(),
					Generation:  ws.Generation,
				},
			}})
		return
	}
	if ws.State == "ready" {
		if _, err := r.s.q.SetSandboxProvisioning(ctx, sb.ID); err != nil {
			r.s.log.Warn("reconcile: mark sandbox provisioning", "sandbox_id", sb.ID, "error", err)
		}
	}
}

func (r *reconciler) bind(ctx context.Context, sb store.Sandbox, ws store.Workspace, nodeID uuid.UUID) error {
	tx, err := r.s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	q := r.s.q.WithTx(tx)
	// The state guards make a concurrent stop or delete win the race: then the
	// Sandbox is not bound and no Allocation exists.
	bound, err := q.SetSandboxNode(ctx, store.SetSandboxNodeParams{ID: sb.ID, NodeID: &nodeID})
	if err != nil {
		return err
	}
	if bound == 0 {
		// A concurrent stop or delete won the state guard: write nothing.
		return nil
	}
	if _, err := q.SetWorkspaceNode(ctx, store.SetWorkspaceNodeParams{ID: ws.ID, NodeID: &nodeID}); err != nil {
		return err
	}
	if err := q.CreateAllocation(ctx, store.CreateAllocationParams{
		SandboxID: sb.ID, NodeID: nodeID, Cpu: sb.Cpu, Memory: sb.Memory, Generation: sb.Generation,
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
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
	id := sandboxCommandId("create-sandbox", sb)
	r.send(ctx, sb.NodeID, id, &nodev1.ControlMessage{Message: &nodev1.ControlMessage_CreateSandbox{
		CreateSandbox: &nodev1.CreateSandbox{
			CommandId:   id,
			SandboxId:   sb.ID.String(),
			Generation:  sb.Generation,
			ImageDigest: sb.SnapshotDigest,
			Limits:      &nodev1.Limits{Cpu: sb.Cpu, Memory: sb.Memory},
			Workspace:   &nodev1.WorkspaceMount{WorkspaceId: att.WorkspaceID.String(), Mode: att.Mode},
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
	id := sandboxCommandId("stop-sandbox", sb)
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
	if err := q.DeleteAllocation(ctx, sandboxID); err != nil {
		r.s.log.Warn("reconcile: delete allocation", "sandbox_id", sandboxID, "error", err)
		return
	}
	if err := q.DeleteAttachment(ctx, sandboxID); err != nil {
		r.s.log.Warn("reconcile: delete attachment", "sandbox_id", sandboxID, "error", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		r.s.log.Warn("reconcile: commit stop", "sandbox_id", sandboxID, "error", err)
	}
}

func (r *reconciler) delete(ctx context.Context, sb store.Sandbox) {
	if sb.NodeID == nil {
		// Nothing ever ran. Release the Workspace directly.
		if ws, err := r.s.q.GetSandboxWorkspace(ctx, &sb.ID); err == nil {
			if _, err := r.s.q.SetWorkspaceDeleting(ctx, ws.ID); err != nil {
				r.s.log.Warn("reconcile: mark workspace deleting", "workspace_id", ws.ID, "error", err)
			}
		}
		if err := r.s.q.DeleteAttachment(ctx, sb.ID); err != nil {
			r.s.log.Warn("reconcile: delete attachment", "sandbox_id", sb.ID, "error", err)
		}
		return
	}
	id := sandboxCommandId("stop-sandbox", sb)
	r.send(ctx, sb.NodeID, id, &nodev1.ControlMessage{Message: &nodev1.ControlMessage_StopSandbox{
		StopSandbox: &nodev1.StopSandbox{
			CommandId: id, SandboxId: sb.ID.String(), Generation: sb.Generation,
		},
	}})
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
		id := workspaceCommandId("delete-workspace", ws)
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
		if _, err := r.s.q.MarkSandboxDeleted(ctx, *ws.SandboxID); err != nil {
			r.s.log.Warn("reconcile: mark sandbox deleted", "sandbox_id", *ws.SandboxID, "error", err)
		}
	}
}

// place picks the first ready Node with enough unreserved CPU and memory. FIFO
// admission is the GPU scheduling ticket's job.
func (r *reconciler) place(ctx context.Context, sb store.Sandbox) (uuid.UUID, bool) {
	nodes, err := r.s.q.ListAllNodes(ctx)
	if err != nil {
		r.s.log.Warn("reconcile: list nodes", "error", err)
		return uuid.UUID{}, false
	}
	now := time.Now()
	for _, node := range nodes {
		// A Node that just dropped still looks ready for nodeUnreachableAfter;
		// binding there would leave the Sandbox without a command stream.
		if deriveNodeStatus(node, now) != "ready" || !r.hub.connected(node.ID) {
			continue
		}
		var hw struct {
			CPUCores    float64 `json:"cpu_cores"`
			MemoryBytes int64   `json:"memory_bytes"`
		}
		if len(node.Hardware) == 0 || json.Unmarshal(node.Hardware, &hw) != nil {
			continue
		}
		used, err := r.s.q.NodeAllocated(ctx, node.ID)
		if err != nil {
			continue
		}
		if hw.CPUCores-node.ReservedCpu-used.Cpu < sb.Cpu {
			continue
		}
		if hw.MemoryBytes-node.ReservedMemory-used.Memory < sb.Memory {
			continue
		}
		return node.ID, true
	}
	return uuid.UUID{}, false
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
		if err := q.DeleteAllocation(ctx, sb.ID); err != nil {
			r.s.log.Warn("reconcile: release allocation", "sandbox_id", sb.ID, "error", err)
			return
		}
		if err := q.DeleteAttachment(ctx, sb.ID); err != nil {
			r.s.log.Warn("reconcile: release attachment", "sandbox_id", sb.ID, "error", err)
			return
		}
	}
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

func sandboxCommandId(prefix string, sb store.Sandbox) string {
	return fmt.Sprintf("%s:%s:%d", prefix, sb.ID, sb.Generation)
}

func workspaceCommandId(prefix string, ws store.Workspace) string {
	return fmt.Sprintf("%s:%s:%d", prefix, ws.ID, ws.Generation)
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

// failed forgets a delivered command so the next sweep sends it again.
func (h *nodeHub) failed(nodeID uuid.UUID, commandID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.sent[nodeID], commandID)
}
