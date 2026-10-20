package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/zibyn/orrery/internal/nodeproto"
	"github.com/zibyn/orrery/internal/store"
)

// nodeUnreachableAfter is twice the heartbeat interval (defaults.md).
const nodeUnreachableAfter = 30 * time.Second

type enrollmentPacketDTO struct {
	ID        string    `json:"id"`
	Packet    string    `json:"packet"`
	ExpiresAt time.Time `json:"expires_at"`
}

type gpuDTO struct {
	UUID  string `json:"uuid"`
	Model string `json:"model"`
}

type nodeDTO struct {
	ID            string          `json:"id"`
	Name          string          `json:"name"`
	Status        string          `json:"status"`
	Paused        bool            `json:"paused"`
	LostAt        *time.Time      `json:"lost_at"`
	AgentVersion  *string         `json:"agent_version"`
	ProtoMajor    *int32          `json:"proto_major"`
	ProtoMinor    *int32          `json:"proto_minor"`
	LastSeenAt    *time.Time      `json:"last_seen_at"`
	RuntimeHealth *string         `json:"runtime_health"`
	Capabilities  json.RawMessage `json:"capabilities"`
	Hardware      json.RawMessage `json:"hardware"`
	GPUs          []gpuDTO        `json:"gpus"`
	CreatedAt     time.Time       `json:"created_at"`
}

type nodeList struct {
	Items      []nodeDTO `json:"items"`
	NextCursor *string   `json:"next_cursor"`
}

func (s *Server) handleCreateEnrollmentPacket(w http.ResponseWriter, r *http.Request, p *principal) {
	// No Idempotency-Key: the raw Packet exists only in this response, so a
	// retry cannot return the earlier one (api.md §1).
	raw, id, hash, err := NewEnrollmentPacket()
	if err != nil {
		s.internal(w, "generate enrollment packet", err)
		return
	}
	expires := time.Now().Add(s.cfg.EnrollmentPacketTTL)
	packet, err := s.q.CreateEnrollmentPacket(r.Context(), store.CreateEnrollmentPacketParams{
		ID:         id,
		SecretHash: hash,
		ExpiresAt:  timestamptzPtr(&expires),
		CreatedBy:  p.user.ID,
	})
	if err != nil {
		s.internal(w, "create enrollment packet", err)
		return
	}
	s.audit(r.Context(), p, "enrollment_packet.create", "enrollment_packet", id.String(), map[string]any{
		"expires_at": packet.ExpiresAt.Time.UTC(),
	})
	writeJSON(w, http.StatusCreated, enrollmentPacketDTO{
		ID:        id.String(),
		Packet:    raw,
		ExpiresAt: packet.ExpiresAt.Time.UTC(),
	})
}

func (s *Server) handleListNodes(w http.ResponseWriter, r *http.Request, p *principal) {
	rows, next, ok := readPage(s, w, r, "list nodes", parseCursor,
		func(ctx context.Context, limit int32, after *uuid.UUID) ([]store.Node, error) {
			return s.q.ListNodes(ctx, store.ListNodesParams{After: after, Limit: limit})
		},
		func(n store.Node) string { return EncodeCursor(n.ID) })
	if !ok {
		return
	}
	out := nodeList{Items: make([]nodeDTO, 0, len(rows)), NextCursor: next}
	now := time.Now()
	for _, row := range rows {
		dto, err := s.nodeToDTO(r.Context(), row, now)
		if err != nil {
			s.internal(w, "read node GPUs", err)
			return
		}
		out.Items = append(out.Items, dto)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetNode(w http.ResponseWriter, r *http.Request, p *principal) {
	id, ok := pathUUID(r, "id")
	if !ok {
		writeProblem(w, "not_found", "")
		return
	}
	node, err := s.q.GetNode(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeProblem(w, "not_found", "")
		return
	}
	if err != nil {
		s.internal(w, "get node", err)
		return
	}
	dto, err := s.nodeToDTO(r.Context(), node, time.Now())
	if err != nil {
		s.internal(w, "read node GPUs", err)
		return
	}
	writeJSON(w, http.StatusOK, dto)
}

func (s *Server) handlePauseNode(w http.ResponseWriter, r *http.Request, p *principal) {
	s.setNodePaused(w, r, p, true)
}

func (s *Server) handleResumeNode(w http.ResponseWriter, r *http.Request, p *principal) {
	s.setNodePaused(w, r, p, false)
}

// setNodePaused flips the server-side flag (ADR-0017). A paused Node takes no
// new Workspace and no build; Workspaces already bound to it can still start
// Sandboxes so their owners can move data out.
func (s *Server) setNodePaused(w http.ResponseWriter, r *http.Request, p *principal, paused bool) {
	id, ok := pathUUID(r, "id")
	if !ok {
		writeProblem(w, "not_found", "")
		return
	}
	var node store.Node
	var err error
	action := "node.resume"
	if paused {
		action = "node.pause"
		node, err = s.q.PauseNode(r.Context(), id)
	} else {
		node, err = s.q.ResumeNode(r.Context(), id)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		// ResumeNode refuses a lost Node in SQL; classify the refusal so the
		// caller learns it is irreversible rather than gone (api.md §3).
		if !paused {
			if existing, getErr := s.q.GetNode(r.Context(), id); getErr == nil && nodeLost(existing) {
				writeProblem(w, "conflict", "a lost Node cannot be resumed")
				return
			}
		}
		writeProblem(w, "not_found", "")
		return
	}
	if err != nil {
		s.internal(w, action, err)
		return
	}
	s.audit(r.Context(), p, action, "node", id.String(), nil)
	dto, err := s.nodeToDTO(r.Context(), node, time.Now())
	if err != nil {
		s.internal(w, "read node GPUs", err)
		return
	}
	writeJSON(w, http.StatusAccepted, dto)
}

// nodeLost reports whether the Node was declared lost. The declaration is
// irreversible (ADR-0014): the token is gone and lost_at is set.
func nodeLost(node store.Node) bool {
	return node.LostAt.Valid
}

// handleDeleteNode leaves a tombstone (api.md §3, ADR-0014): the Node must be
// paused with no non-terminal Workspace and no non-terminal Sandbox, build
// rows included. The token is cleared and the GPU rows are removed; the row
// stays because Workspaces and Sandboxes still reference it.
func (s *Server) handleDeleteNode(w http.ResponseWriter, r *http.Request, p *principal) {
	id, ok := pathUUID(r, "id")
	if !ok {
		writeProblem(w, "not_found", "")
		return
	}
	ctx := r.Context()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		s.internal(w, "begin node delete", err)
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	q := s.q.WithTx(tx)
	node, err := q.GetNode(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeProblem(w, "not_found", "")
		return
	}
	if err != nil {
		s.internal(w, "get node", err)
		return
	}
	if !node.Paused {
		writeProblem(w, "conflict", "the Node must be paused before it can be deleted")
		return
	}
	workspaces, err := q.CountNodeNonTerminalWorkspaces(ctx, &id)
	if err != nil {
		s.internal(w, "count Node Workspaces", err)
		return
	}
	sandboxes, err := q.CountNodeNonTerminalSandboxes(ctx, &id)
	if err != nil {
		s.internal(w, "count Node Sandboxes", err)
		return
	}
	if workspaces > 0 || sandboxes > 0 {
		writeProblem(w, "conflict", "the Node still has non-terminal Workspaces or Sandboxes")
		return
	}
	rows, err := q.TombstoneNode(ctx, id)
	if err != nil {
		s.internal(w, "tombstone Node", err)
		return
	}
	if rows == 0 {
		// A concurrent delete won; the Node is already a tombstone.
		writeProblem(w, "not_found", "")
		return
	}
	if err := q.DeleteNodeGPUs(ctx, id); err != nil {
		s.internal(w, "delete Node GPUs", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		s.internal(w, "commit node delete", err)
		return
	}
	// A still-connected Agent gets UNAUTHENTICATED and drops its identity
	// (ADR-0014).
	s.reconcile.hub.close(id)
	s.audit(ctx, p, "node.delete", "node", id.String(), nil)
	dto, err := s.nodeToDTO(ctx, node, time.Now())
	if err != nil {
		s.internal(w, "read node GPUs", err)
		return
	}
	writeJSON(w, http.StatusAccepted, dto)
}

func (s *Server) nodeToDTO(ctx context.Context, node store.Node, now time.Time) (nodeDTO, error) {
	gpus, err := s.q.ListNodeGPUs(ctx, node.ID)
	if err != nil {
		return nodeDTO{}, err
	}
	dto := nodeDTO{
		ID:            node.ID.String(),
		Name:          node.Name,
		Status:        deriveNodeStatus(node, now),
		Paused:        node.Paused,
		LostAt:        timePtr(node.LostAt),
		AgentVersion:  node.AgentVersion,
		ProtoMajor:    node.ProtoMajor,
		ProtoMinor:    node.ProtoMinor,
		LastSeenAt:    timePtr(node.LastSeenAt),
		RuntimeHealth: node.RuntimeHealth,
		Capabilities:  node.Capabilities,
		Hardware:      node.Hardware,
		GPUs:          make([]gpuDTO, 0, len(gpus)),
		CreatedAt:     node.CreatedAt.Time.UTC(),
	}
	for _, gpu := range gpus {
		dto.GPUs = append(dto.GPUs, gpuDTO{UUID: gpu.Uuid, Model: gpu.Model})
	}
	return dto, nil
}

// handleDeclareNodeLost is the explicit admin act that makes a Node lost
// (ADR-0014). It is refused while the Node is reachable: liveness, not Runtime
// health, decides. The whole cascade commits in one transaction.
func (s *Server) handleDeclareNodeLost(w http.ResponseWriter, r *http.Request, p *principal) {
	id, ok := pathUUID(r, "id")
	if !ok {
		writeProblem(w, "not_found", "")
		return
	}
	ctx := r.Context()
	node, err := s.q.GetNode(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeProblem(w, "not_found", "")
		return
	}
	if err != nil {
		s.internal(w, "get node", err)
		return
	}
	if nodeReachable(node, s.reconcile.hub.connected(id), time.Now()) {
		writeProblem(w, "conflict", "the Node is reachable")
		return
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		s.internal(w, "declare Node lost", err)
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	q := s.q.WithTx(tx)
	step := func(what string, err error) bool {
		if err == nil {
			return false
		}
		s.log.Error("declare Node lost: "+what, "node_id", id, "error", err)
		s.internal(w, "declare Node lost", err)
		return true
	}
	if _, err := q.MarkNodeLost(ctx, id); step("clear Node token", err) {
		return
	}
	if _, err := q.FailSandboxesOnNodeLost(ctx, &id); step("fail Sandboxes", err) {
		return
	}
	if _, err := q.DeleteSandboxAttachmentsOnNodeLost(ctx, &id); step("delete Attachments", err) {
		return
	}
	if _, err := q.DeleteGPUAllocationsOnNodeLost(ctx, &id); step("release GPU allocations", err) {
		return
	}
	if _, err := q.DeleteAllocationsOnNodeLost(ctx, &id); step("release allocations", err) {
		return
	}
	if _, err := q.FinishExecsOnNodeLost(ctx, &id); step("finish Execs", err) {
		return
	}
	if _, err := q.MarkDeletingWorkspacesDeletedOnNodeLost(ctx, &id); step("finish Workspace deletions", err) {
		return
	}
	if _, err := q.MarkWorkspacesLostOnNodeLost(ctx, &id); step("mark Workspaces lost", err) {
		return
	}
	if _, err := q.FailBuildsOnNodeLost(ctx, &id); step("fail builds", err) {
		return
	}
	if _, err := q.FailPublishesOnNodeLost(ctx, &id); step("fail publishes", err) {
		return
	}
	if err := tx.Commit(ctx); err != nil {
		s.internal(w, "declare Node lost", err)
		return
	}
	// A still-connected Agent must learn it is no longer authorized and clear
	// its identity (ADR-0014).
	s.reconcile.hub.close(id)
	s.audit(ctx, p, "node.declare_lost", "node", id.String(), nil)

	node, err = s.q.GetNode(ctx, id)
	if err != nil {
		s.internal(w, "get node", err)
		return
	}
	dto, err := s.nodeToDTO(ctx, node, time.Now())
	if err != nil {
		s.internal(w, "read node GPUs", err)
		return
	}
	writeJSON(w, http.StatusAccepted, dto)
}

// nodeReachable is liveness, not health: a fresh heartbeat or an open Agent
// stream. An unhealthy Runtime is still reachable, so declaring that Node lost
// is refused with conflict; a lost Node is never reachable again (ADR-0014).
func nodeReachable(node store.Node, connected bool, now time.Time) bool {
	if node.LostAt.Valid {
		return false
	}
	return connected || (node.LastSeenAt.Valid && now.Sub(node.LastSeenAt.Time) <= nodeUnreachableAfter)
}

// deriveNodeStatus computes the read-time status (api.md §3, ADR-0014).
// `ready` is fully schedulable; `paused` takes no new Workspace but can still
// run Sandboxes whose Workspace is bound there (ADR-0017). `outdated` is
// accepted but flagged; `unavailable` covers both a stale heartbeat and a
// Runtime that cannot take work.
func deriveNodeStatus(node store.Node, now time.Time) string {
	switch {
	case node.LostAt.Valid:
		return "lost"
	case node.Paused:
		return "paused"
	case !node.LastSeenAt.Valid || now.Sub(node.LastSeenAt.Time) > nodeUnreachableAfter:
		return "unavailable"
	case node.RuntimeHealth == nil || *node.RuntimeHealth != "healthy":
		return "unavailable"
	case !nodeproto.CoreEnforced(node.Capabilities):
		return "unavailable"
	case node.ProtoMinor != nil && *node.ProtoMinor < int32(nodeproto.Minor):
		return "outdated"
	default:
		return "ready"
	}
}
