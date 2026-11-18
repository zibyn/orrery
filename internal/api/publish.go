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

// errPublishPath marks a path that escapes the Sandbox's filesystem.
var errPublishPath = errors.New("path must stay inside the Sandbox's filesystem")

// normalizePublishPath turns a publish request's path into the form the
// Agent resolves (api.md §5): relative to /workspace, or absolute in the
// root filesystem when it starts with "/". ".." segments are refused.
func normalizePublishPath(raw string) (string, error) {
	rel, ok := cleanLocalPath(strings.TrimLeft(raw, "/"))
	if !ok {
		return "", errPublishPath
	}
	if strings.HasPrefix(raw, "/") {
		return path.Join("/", rel), nil
	}
	return rel, nil
}

// cleanLocalPath is raw cleaned, when it is lexically inside its root: no
// NUL, no ".." segment, not absolute. "" and "." name the root. A path that
// escapes is refused, never rewritten.
func cleanLocalPath(raw string) (string, bool) {
	if strings.ContainsRune(raw, 0) {
		return "", false
	}
	if raw == "" || raw == "." {
		return ".", true
	}
	for _, segment := range strings.Split(raw, "/") {
		if segment == ".." {
			return "", false
		}
	}
	// filepath.IsLocal is the same containment rule the Agent's file API
	// applies; on Linux it shares the slash separator.
	if !filepath.IsLocal(raw) {
		return "", false
	}
	return path.Clean(raw), true
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
	publishSandboxCommandPrefix = "publish-sandbox"
	publishGeneration           = int64(0)
)

func publishSandboxCommandID(versionID uuid.UUID) string {
	return commandID(publishSandboxCommandPrefix, versionID, publishGeneration)
}

// publishVersionFromCommandID extracts the version a Sandbox publish's
// CommandResult refers to.
func publishVersionFromCommandID(id string) (uuid.UUID, bool) {
	rest, ok := strings.CutPrefix(id, publishSandboxCommandPrefix+":")
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

// sandboxProvenance is `source: sandbox{id, path}` (schema.md); the path is
// relative to /workspace, "." for /workspace itself.
func sandboxProvenance(sandboxID uuid.UUID, publishPath string) (json.RawMessage, error) {
	type sandboxRef struct {
		ID   string `json:"id"`
		Path string `json:"path"`
	}
	return json.Marshal(struct {
		Source  string     `json:"source"`
		Sandbox sandboxRef `json:"sandbox"`
	}{Source: "sandbox", Sandbox: sandboxRef{ID: sandboxID.String(), Path: publishPath}})
}

// parseSandboxProvenance reads what sandboxProvenance wrote.
func parseSandboxProvenance(raw json.RawMessage) (string, bool) {
	var prov struct {
		Source  string `json:"source"`
		Sandbox struct {
			Path string `json:"path"`
		} `json:"sandbox"`
	}
	if err := json.Unmarshal(raw, &prov); err != nil || prov.Source != "sandbox" {
		return "", false
	}
	return prov.Sandbox.Path, true
}

// handlePublishSandbox creates a pending ResourceVersion for a path of a
// ready Sandbox (api.md §3); its Node walks the live filesystem. A path on a
// Volume is published from the Volume instead, without a Node.
func (s *Server) handlePublishSandbox(w http.ResponseWriter, r *http.Request, p *principal) {
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
	sb, ok := s.getOwnedSandbox(w, ctx, p, id)
	if !ok {
		return
	}
	lookup := func() (store.ResourceVersion, error) {
		return s.q.GetResourceVersionBySandboxPublishIdempotencyKey(ctx, store.GetResourceVersionBySandboxPublishIdempotencyKeyParams{
			PublishSandboxID: &sb.ID, PublishIdempotencyKey: key,
		})
	}
	if replayOr(s, w, "look up idempotent publish", key, lookup, func(v store.ResourceVersion) {
		writeJSON(w, http.StatusAccepted, resourceVersionToDTO(v))
	}) {
		return
	}
	var request publishRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	publishPath, err := normalizePublishPath(request.Path)
	if err != nil {
		writeProblem(w, "invalid_request", errPublishPath.Error())
		return
	}
	absolute := publishPath
	if !path.IsAbs(absolute) {
		absolute = path.Join("/workspace", publishPath)
	}
	if pathWithin(absolute, "/resources") {
		writeProblem(w, "invalid_request", "the path is on a Resource mount; a Resource is already published")
		return
	}
	for _, v := range s.sandboxVolumes(ctx, sb.ID) {
		if pathWithin(absolute, v.Path) {
			writeProblem(w, "invalid_request", "the path is on Volume "+v.Volume+"; publish it with POST /volumes/{id}/publish")
			return
		}
	}
	if !validDisplayName(request.Resource) {
		writeProblem(w, "invalid_request", "resource must be a non-empty, trimmed name of at most 255 characters")
		return
	}
	if SandboxState(sb.State) != SandboxReady {
		writeProblem(w, "conflict", "the Sandbox is not ready")
		return
	}
	if s.objects == nil {
		writeProblem(w, "unavailable", "object storage is not configured")
		return
	}
	if sb.NodeID == nil || !s.reconcile.hub.connected(*sb.NodeID) {
		writeProblem(w, "unavailable", "the Sandbox's Node is not connected")
		return
	}
	provenance, err := sandboxProvenance(sb.ID, publishPath)
	if err != nil {
		s.internal(w, "encode provenance", err)
		return
	}
	s.createPendingPublish(w, ctx, sb.OwnerID, request.Resource, key, lookup, provenance,
		store.CreatePendingResourceVersionParams{PublishSandboxID: &sb.ID})
}
