package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/zibyn/orrery/internal/store"
)

// errPublishPath marks a path that is not a local path inside the Workspace.
var errPublishPath = errors.New("path must be relative to the Workspace and stay inside it")

// normalizePublishPath turns a publish request's path into the relative form
// the Agent walks. "/workspace" and its descendants are accepted because that
// is where the Workspace is mounted inside a Sandbox; any other absolute path
// and any ".." segment is rejected (ADR-0012: paths stay within /workspace).
func normalizePublishPath(raw string) (string, error) {
	if strings.ContainsRune(raw, 0) {
		return "", errPublishPath
	}
	switch {
	case raw == "/workspace" || raw == "/workspace/":
		raw = "."
	case strings.HasPrefix(raw, "/workspace/"):
		raw = strings.TrimPrefix(raw, "/workspace/")
	case strings.HasPrefix(raw, "/"):
		return "", errPublishPath
	}
	if raw == "" || raw == "." {
		return ".", nil
	}
	for _, segment := range strings.Split(raw, "/") {
		if segment == ".." {
			return "", errPublishPath
		}
	}
	cleaned := path.Clean(raw)
	if cleaned == "." {
		return ".", nil
	}
	// filepath.IsLocal is the same containment rule the Agent's file API
	// applies; on Linux it shares the slash separator.
	if path.IsAbs(cleaned) || !filepath.IsLocal(filepath.FromSlash(cleaned)) {
		return "", errPublishPath
	}
	return cleaned, nil
}

// workspaceProvenance is `source: workspace{id, path}` (schema.md). The path
// is relative to the Workspace root, with "." for the root itself.
func workspaceProvenance(workspaceID uuid.UUID, publishPath string) json.RawMessage {
	type workspaceRef struct {
		ID   string `json:"id"`
		Path string `json:"path"`
	}
	raw, err := json.Marshal(struct {
		Source    string       `json:"source"`
		Workspace workspaceRef `json:"workspace"`
	}{
		Source:    "workspace",
		Workspace: workspaceRef{ID: workspaceID.String(), Path: publishPath},
	})
	if err != nil {
		return nil
	}
	return raw
}

// parseWorkspaceProvenance reads what workspaceProvenance wrote.
func parseWorkspaceProvenance(raw json.RawMessage) (uuid.UUID, string, bool) {
	var prov struct {
		Source    string `json:"source"`
		Workspace struct {
			ID   string `json:"id"`
			Path string `json:"path"`
		} `json:"workspace"`
	}
	if err := json.Unmarshal(raw, &prov); err != nil || prov.Source != "workspace" {
		return uuid.Nil, "", false
	}
	id, err := uuid.Parse(prov.Workspace.ID)
	if err != nil {
		return uuid.Nil, "", false
	}
	return id, prov.Workspace.Path, true
}

// handlePublishWorkspace creates a pending ResourceVersion for a Workspace
// path (api.md §3). The Reconciler hands the walk to the Workspace's Node;
// no Sandbox needs to be running. A Node without a live Control stream is
// 503, because the command could not be delivered.
func (s *Server) handlePublishWorkspace(w http.ResponseWriter, r *http.Request, p *principal) {
	id, ok := pathUUID(r, "id")
	if !ok {
		writeProblem(w, "invalid_request", "id must be a UUID")
		return
	}
	key, ok := idempotencyKey(r)
	if !ok {
		writeProblem(w, "invalid_request", "Idempotency-Key is too long")
		return
	}
	ctx := r.Context()
	ws, ok := s.getOwnedWorkspace(w, ctx, p, id)
	if !ok {
		return
	}
	// Idempotency is per (Workspace, key) (api.md §1, migration 000014).
	lookup := func() (store.ResourceVersion, error) {
		return s.q.GetResourceVersionByPublishIdempotencyKey(ctx, store.GetResourceVersionByPublishIdempotencyKeyParams{
			PublishWorkspaceID: &ws.ID, PublishIdempotencyKey: key,
		})
	}
	if replayOr(s, w, "look up idempotent publish", key, lookup, func(v store.ResourceVersion) {
		writeJSON(w, http.StatusAccepted, resourceVersionToDTO(v))
	}) {
		return
	}
	if !workspaceUsable(ws) {
		writeProblem(w, "conflict", "the Workspace is not usable")
		return
	}
	if s.objects == nil {
		writeProblem(w, "unavailable", "object storage is not configured")
		return
	}
	if ws.NodeID == nil || !s.reconcile.hub.connected(*ws.NodeID) {
		writeProblem(w, "unavailable", "the Workspace's Node is not connected")
		return
	}

	var request struct {
		Path     string `json:"path"`
		Resource string `json:"resource"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	publishPath, err := normalizePublishPath(request.Path)
	if err != nil {
		writeProblem(w, "invalid_request", errPublishPath.Error())
		return
	}
	if !validDisplayName(request.Resource) {
		writeProblem(w, "invalid_request", "resource must be a non-empty, trimmed name of at most 255 characters")
		return
	}
	// The Resource belongs to the Workspace's owner, which is the caller
	// except when an admin publishes on someone else's Workspace (api.md §1:
	// admins may act on all resources).
	resource, err := s.ensureOwnedResource(ctx, ws.OwnerID, request.Resource)
	if err != nil {
		s.internal(w, "resolve Resource", err)
		return
	}

	versionID, err := uuid.NewV7()
	if err != nil {
		s.internal(w, "generate ResourceVersion id", err)
		return
	}
	version, replayed, err := createIdempotent(key, lookup,
		func() (store.ResourceVersion, error) {
			return s.q.CreatePendingResourceVersion(ctx, store.CreatePendingResourceVersionParams{
				ID:                    versionID,
				ResourceID:            resource.ID,
				Provenance:            workspaceProvenance(ws.ID, publishPath),
				PublishWorkspaceID:    &ws.ID,
				PublishIdempotencyKey: key,
			})
		})
	if err != nil {
		s.internal(w, "create pending ResourceVersion", err)
		return
	}
	if replayed {
		writeJSON(w, http.StatusAccepted, resourceVersionToDTO(version))
		return
	}
	s.changed(ctx)
	writeJSON(w, http.StatusAccepted, resourceVersionToDTO(version))
}

// ensureOwnedResource resolves a Resource name, creating it when absent — the
// same behavior as `resource push` (api.md §3). A concurrent create that wins
// the name is resolved again.
func (s *Server) ensureOwnedResource(ctx context.Context, ownerID uuid.UUID, name string) (store.Resource, error) {
	lookup := func() (store.Resource, error) {
		return s.q.GetLiveResourceByName(ctx, store.GetLiveResourceByNameParams{
			OwnerID: ownerID, Name: name,
		})
	}
	resource, err := lookup()
	if err == nil {
		return resource, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return store.Resource{}, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return store.Resource{}, err
	}
	resource, err = s.q.CreateResource(ctx, store.CreateResourceParams{
		ID: id, OwnerID: ownerID, Name: name,
	})
	if err != nil {
		if uniqueViolation(err) == "resources_owner_name" {
			return lookup()
		}
		return store.Resource{}, err
	}
	return resource, nil
}

// The Reconciler names a publish command deterministically so nodeHub can
// deduplicate it and clear its memory on reconnect. A ResourceVersion has no
// generation, so the fencing token is the constant 0: the Agent's
// commandRunner treats it as no fence (like CancelExec) and every redelivery
// of the same version is idempotent work, while a newer publish is a
// different version id and therefore a different command and lock key.
const (
	publishCommandPrefix = "publish-workspace"
	publishGeneration    = int64(0)
)

func publishCommandID(versionID uuid.UUID) string {
	return commandID(publishCommandPrefix, versionID, publishGeneration)
}

// publishVersionFromCommandID extracts the version a CommandResult refers to.
func publishVersionFromCommandID(id string) (uuid.UUID, bool) {
	rest, ok := strings.CutPrefix(id, publishCommandPrefix+":")
	if !ok {
		return uuid.Nil, false
	}
	versionPart, _, ok := strings.Cut(rest, ":")
	if !ok {
		return uuid.Nil, false
	}
	versionID, err := uuid.Parse(versionPart)
	if err != nil {
		return uuid.Nil, false
	}
	return versionID, true
}
