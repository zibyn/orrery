package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/zibyn/stars-sandbox/internal/store"
)

// workspaceConditions is the Workspace's conditions column. Attempts and
// next_attempt_at are the provisioning retry state (defaults.md); only
// provisioning_stalled is public (api.md §3).
type workspaceConditions struct {
	Attempts            int        `json:"attempts,omitempty"`
	NextAttemptAt       *time.Time `json:"next_attempt_at,omitempty"`
	ProvisioningStalled bool       `json:"provisioning_stalled,omitempty"`
}

func parseWorkspaceConditions(raw json.RawMessage) workspaceConditions {
	var c workspaceConditions
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &c)
	}
	return c
}

// provisionDue reports whether a further provisioning attempt may be sent.
func (c workspaceConditions) provisionDue(now time.Time) bool {
	return c.NextAttemptAt == nil || !now.Before(*c.NextAttemptAt)
}

// names is the public condition list.
func (c workspaceConditions) names() []string {
	names := []string{}
	if c.ProvisioningStalled {
		names = append(names, "provisioning_stalled")
	}
	return names
}

func encodeWorkspaceConditions(c workspaceConditions) json.RawMessage {
	raw, err := json.Marshal(c)
	if err != nil {
		return nil
	}
	return raw
}

// workspaceUsable reports whether a Sandbox may attach or start against the
// Workspace. A failed, lost or deleting Workspace cannot be mounted.
func workspaceUsable(ws store.Workspace) bool {
	return ws.State == "ready" || ws.State == "provisioning"
}

type workspaceDTO struct {
	ID          string     `json:"id"`
	Kind        string     `json:"kind"`
	NodeID      *string    `json:"node_id"`
	State       string     `json:"state"`
	Cause       *string    `json:"cause"`
	Unavailable bool       `json:"unavailable"`
	Conditions  []string   `json:"conditions"`
	SandboxID   *string    `json:"sandbox_id"`
	CreatedAt   time.Time  `json:"created_at"`
	DeletedAt   *time.Time `json:"deleted_at"`
}

type workspaceList struct {
	Items      []workspaceDTO `json:"items"`
	NextCursor *string        `json:"next_cursor"`
}

func (s *Server) handleCreateWorkspace(w http.ResponseWriter, r *http.Request, p *principal) {
	key, ok := idempotencyKey(r)
	if !ok {
		writeProblem(w, "invalid_request", "Idempotency-Key is too long")
		return
	}
	ctx := r.Context()
	if key != nil {
		if existing, err := s.q.GetWorkspaceByIdempotencyKey(ctx, store.GetWorkspaceByIdempotencyKeyParams{
			OwnerID: p.user.ID, IdempotencyKey: key,
		}); err == nil {
			writeJSON(w, http.StatusOK, s.workspaceToDTO(ctx, existing))
			return
		} else if !errors.Is(err, pgx.ErrNoRows) {
			s.internal(w, "look up idempotent Workspace", err)
			return
		}
	}
	id, err := uuid.NewV7()
	if err != nil {
		s.internal(w, "generate Workspace id", err)
		return
	}
	ws, err := s.q.CreateWorkspace(ctx, store.CreateWorkspaceParams{
		ID: id, OwnerID: p.user.ID, Kind: "persistent", IdempotencyKey: key,
	})
	if err != nil {
		if uniqueViolation(err) == "workspaces_owner_idempotency" && key != nil {
			if existing, lookupErr := s.q.GetWorkspaceByIdempotencyKey(ctx, store.GetWorkspaceByIdempotencyKeyParams{
				OwnerID: p.user.ID, IdempotencyKey: key,
			}); lookupErr == nil {
				writeJSON(w, http.StatusOK, s.workspaceToDTO(ctx, existing))
				return
			}
		}
		s.internal(w, "create Workspace", err)
		return
	}
	s.audit(ctx, p, "workspace.create", "workspace", id.String(), map[string]any{"kind": ws.Kind})
	s.changed(ctx)
	writeJSON(w, http.StatusCreated, s.workspaceToDTO(ctx, ws))
}

func (s *Server) handleListWorkspaces(w http.ResponseWriter, r *http.Request, p *principal) {
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
	rows, err := s.q.ListWorkspaces(r.Context(), store.ListWorkspacesParams{
		OwnerID: p.user.ID, After: after, Limit: limit + 1,
	})
	if err != nil {
		s.internal(w, "list Workspaces", err)
		return
	}
	list := workspaceList{Items: make([]workspaceDTO, 0, len(rows))}
	for i, row := range rows {
		if int32(i) == limit {
			cursor := EncodeCursor(rows[i-1].ID)
			list.NextCursor = &cursor
			break
		}
		list.Items = append(list.Items, s.workspaceToDTO(r.Context(), row))
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleGetWorkspace(w http.ResponseWriter, r *http.Request, p *principal) {
	id, ok := pathUUID(r, "id")
	if !ok {
		writeProblem(w, "invalid_request", "id must be a UUID")
		return
	}
	ws, ok := s.getOwnedWorkspace(w, r.Context(), p, id)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.workspaceToDTO(r.Context(), ws))
}

func (s *Server) handleDeleteWorkspace(w http.ResponseWriter, r *http.Request, p *principal) {
	id, ok := pathUUID(r, "id")
	if !ok {
		writeProblem(w, "invalid_request", "id must be a UUID")
		return
	}
	ctx := r.Context()
	ws, ok := s.getOwnedWorkspace(w, ctx, p, id)
	if !ok {
		return
	}
	if ws.State == "deleted" || ws.State == "deleting" {
		writeJSON(w, http.StatusAccepted, s.workspaceToDTO(ctx, ws))
		return
	}
	if _, err := s.q.SetWorkspaceDeleting(ctx, ws.ID); err != nil {
		s.internal(w, "delete Workspace", err)
		return
	}
	if current, err := s.q.GetWorkspace(ctx, ws.ID); err == nil {
		ws = current
	}
	s.audit(ctx, p, "workspace.delete", "workspace", ws.ID.String(), nil)
	s.changed(ctx)
	writeJSON(w, http.StatusAccepted, s.workspaceToDTO(ctx, ws))
}

// getOwnedWorkspace hides another User's Workspace behind 404 (api.md §1).
func (s *Server) getOwnedWorkspace(w http.ResponseWriter, ctx context.Context, p *principal, id uuid.UUID) (store.Workspace, bool) {
	ws, err := s.q.GetWorkspace(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !s.ownerOrAdmin(p, ws.OwnerID)) {
		writeProblem(w, "not_found", "no such Workspace")
		return store.Workspace{}, false
	}
	if err != nil {
		s.internal(w, "get Workspace", err)
		return store.Workspace{}, false
	}
	return ws, true
}

func (s *Server) workspaceToDTO(ctx context.Context, ws store.Workspace) workspaceDTO {
	dto := workspaceDTO{
		ID:        ws.ID.String(),
		Kind:      ws.Kind,
		NodeID:    uuidPtr(ws.NodeID),
		State:     ws.State,
		Cause:     ws.Cause,
		SandboxID: uuidPtr(ws.SandboxID),
		CreatedAt: ws.CreatedAt.Time.UTC(),
		DeletedAt: timePtr(ws.DeletedAt),
	}
	conditions := parseWorkspaceConditions(ws.Conditions)
	dto.Conditions = conditions.names()
	// unavailable is derived from Node reachability, never stored (api.md §3).
	if ws.NodeID != nil {
		node, err := s.q.GetNode(ctx, *ws.NodeID)
		if err != nil {
			dto.Unavailable = true
		} else {
			status := deriveNodeStatus(node, time.Now())
			dto.Unavailable = status == "unavailable" || status == "lost"
		}
	}
	return dto
}
