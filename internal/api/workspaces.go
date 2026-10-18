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
	return WorkspaceState(ws.State) == WorkspaceReady || WorkspaceState(ws.State) == WorkspaceProvisioning
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
	lookup := func() (store.Workspace, error) {
		return s.q.GetWorkspaceByIdempotencyKey(ctx, store.GetWorkspaceByIdempotencyKeyParams{
			OwnerID: p.user.ID, IdempotencyKey: key,
		})
	}
	if replayOr(s, w, "look up idempotent Workspace", key, lookup, func(ws store.Workspace) {
		writeJSON(w, http.StatusOK, s.workspaceToDTO(ctx, ws))
	}) {
		return
	}
	id, err := uuid.NewV7()
	if err != nil {
		s.internal(w, "generate Workspace id", err)
		return
	}
	ws, replayed, err := createIdempotent(key, "workspaces_owner_idempotency", lookup,
		func() (store.Workspace, error) {
			return s.q.CreateWorkspace(ctx, store.CreateWorkspaceParams{
				ID: id, OwnerID: p.user.ID, Kind: "persistent", IdempotencyKey: key,
			})
		})
	if err != nil {
		s.internal(w, "create Workspace", err)
		return
	}
	if replayed {
		writeJSON(w, http.StatusOK, s.workspaceToDTO(ctx, ws))
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
	rows, next := trimPage(rows, limit, func(ws store.Workspace) string { return EncodeCursor(ws.ID) })
	list := workspaceList{Items: make([]workspaceDTO, 0, len(rows)), NextCursor: next}
	for _, row := range rows {
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
	if WorkspaceState(ws.State) == WorkspaceDeleted || WorkspaceState(ws.State) == WorkspaceDeleting {
		writeJSON(w, http.StatusAccepted, s.workspaceToDTO(ctx, ws))
		return
	}
	// A lost Workspace has no live storage to clean up: it reaches deleted
	// without a Node command (ADR-0014).
	if WorkspaceState(ws.State) == WorkspaceLost {
		if _, err := s.q.MarkWorkspaceDeleted(ctx, store.MarkWorkspaceDeletedParams{
			ID: ws.ID, Generation: ws.Generation,
		}); err != nil {
			s.internal(w, "delete Workspace", err)
			return
		}
		if current, err := s.q.GetWorkspace(ctx, ws.ID); err == nil {
			ws = current
		}
		s.audit(ctx, p, "workspace.delete", "workspace", ws.ID.String(), nil)
		s.changed(ctx)
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

// handleDeclareWorkspaceLost is the admin act for one Workspace on a live Node
// whose storage failed (ADR-0014). Its Sandboxes fail with workspace_lost, and
// the Reconciler later tears their Runtimes down through the stop path.
func (s *Server) handleDeclareWorkspaceLost(w http.ResponseWriter, r *http.Request, p *principal) {
	id, ok := pathUUID(r, "id")
	if !ok {
		writeProblem(w, "invalid_request", "id must be a UUID")
		return
	}
	ctx := r.Context()
	ws, err := s.q.GetWorkspace(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeProblem(w, "not_found", "no such Workspace")
		return
	}
	if err != nil {
		s.internal(w, "get Workspace", err)
		return
	}
	switch WorkspaceState(ws.State) {
	case WorkspaceProvisioning, WorkspaceReady:
	default:
		writeProblem(w, "conflict", "only a provisioning or ready Workspace can be declared lost")
		return
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		s.internal(w, "declare Workspace lost", err)
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	q := s.q.WithTx(tx)
	lost, err := q.MarkWorkspaceLost(ctx, id)
	if err != nil {
		s.internal(w, "declare Workspace lost", err)
		return
	}
	if lost == 0 {
		writeProblem(w, "conflict", "the Workspace is no longer provisioning or ready")
		return
	}
	if _, err := q.FailSandboxesOnWorkspaceLost(ctx, &id); err != nil {
		s.internal(w, "declare Workspace lost", err)
		return
	}
	if _, err := q.FinishExecsOnWorkspaceLost(ctx, &id); err != nil {
		s.internal(w, "declare Workspace lost", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		s.internal(w, "declare Workspace lost", err)
		return
	}
	s.audit(ctx, p, "workspace.declare_lost", "workspace", id.String(), nil)
	s.changed(ctx)

	if ws, err = s.q.GetWorkspace(ctx, id); err != nil {
		s.internal(w, "get Workspace", err)
		return
	}
	writeJSON(w, http.StatusAccepted, s.workspaceToDTO(ctx, ws))
}

// handleAbandonWorkspace drops the cleanup of a Workspace stuck in deleting,
// reaching deleted without a Node command (api.md §3).
func (s *Server) handleAbandonWorkspace(w http.ResponseWriter, r *http.Request, p *principal) {
	id, ok := pathUUID(r, "id")
	if !ok {
		writeProblem(w, "invalid_request", "id must be a UUID")
		return
	}
	ctx := r.Context()
	ws, err := s.q.GetWorkspace(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeProblem(w, "not_found", "no such Workspace")
		return
	}
	if err != nil {
		s.internal(w, "get Workspace", err)
		return
	}
	if WorkspaceState(ws.State) != WorkspaceDeleting {
		writeProblem(w, "conflict", "only a deleting Workspace can be abandoned")
		return
	}
	rows, err := s.q.MarkWorkspaceAbandoned(ctx, id)
	if err != nil {
		s.internal(w, "abandon Workspace", err)
		return
	}
	if rows == 0 {
		writeProblem(w, "conflict", "the Workspace is no longer deleting")
		return
	}
	s.audit(ctx, p, "workspace.abandon", "workspace", id.String(), nil)
	s.changed(ctx)

	if ws, err = s.q.GetWorkspace(ctx, id); err != nil {
		s.internal(w, "get Workspace", err)
		return
	}
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
