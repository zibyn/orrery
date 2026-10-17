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
	var next *string
	if len(rows) > int(limit) {
		cursor := EncodeCursor(rows[limit-1].ID)
		next = &cursor
		rows = rows[:limit]
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
