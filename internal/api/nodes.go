package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/zibyn/stars-sandbox/internal/nodeproto"
	"github.com/zibyn/stars-sandbox/internal/store"
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
	limit, ok := parseLimit(r)
	if !ok {
		writeProblem(w, "invalid_request", "limit must be between 1 and 200")
		return
	}
	after, ok := parseCursor(r)
	if !ok {
		writeProblem(w, "invalid_request", "invalid cursor")
		return
	}
	rows, err := s.q.ListNodes(r.Context(), store.ListNodesParams{After: after, Limit: limit + 1})
	if err != nil {
		s.internal(w, "list nodes", err)
		return
	}
	rows, next := trimPage(rows, limit, func(n store.Node) string { return EncodeCursor(n.ID) })
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
	writeJSON(w, http.StatusOK, dto)
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
	s.reconcile.hub.disconnect(id)
	s.audit(ctx, p, "node.delete", "node", id.String(), nil)
	dto, err := s.nodeToDTO(ctx, node, time.Now())
	if err != nil {
		s.internal(w, "read node GPUs", err)
		return
	}
	writeJSON(w, http.StatusOK, dto)
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
