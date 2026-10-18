package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/zibyn/stars-sandbox/internal/store"
)

// waitForLimit is fixed by api.md §1.
const waitForLimit = 60 * time.Second

const maxMemoryBytes = 1 << 50 // 1 PiB, a sanity bound only

type createSandboxRequest struct {
	Name      *string                  `json:"name"`
	Labels    map[string]string        `json:"labels"`
	Workspace *sandboxWorkspaceRequest `json:"workspace"`
	Snapshot  *string                  `json:"snapshot"`
	Resources []sandboxResourceRequest `json:"resources"`
	Secrets   map[string]string        `json:"secrets"`
	CPU       *float64                 `json:"cpu"`
	Memory    *int64                   `json:"memory"`
	GPU       *sandboxGPURequest       `json:"gpu"`
	IdleStop  *int32                   `json:"idle_stop"`
}

// sandboxResourceRequest pins one Resource version at create time (api.md §3).
// Version is a ResourceVersion id; omitted means the Resource's current
// version. Mount defaults to the Resource name and must be one path segment.
type sandboxResourceRequest struct {
	Resource string  `json:"resource"`
	Version  *string `json:"version"`
	Mount    *string `json:"mount"`
}

// sandboxWorkspaceRequest attaches an existing Workspace; omitting it creates
// an implicit ephemeral one (ADR-0018).
type sandboxWorkspaceRequest struct {
	ID   string `json:"id"`
	Mode string `json:"mode"`
}

type sandboxGPURequest struct {
	Count int32   `json:"count"`
	Model *string `json:"model"`
}

type sandboxWorkspaceDTO struct {
	ID   string `json:"id"`
	Mode string `json:"mode"`
}

type sandboxGPUDTO struct {
	Count int32   `json:"count"`
	Model *string `json:"model,omitempty"`
}

type sandboxDTO struct {
	ID            string               `json:"id"`
	Name          string               `json:"name"`
	Labels        map[string]string    `json:"labels"`
	State         string               `json:"state"`
	Cause         *string              `json:"cause"`
	NodeID        *string              `json:"node_id"`
	Snapshot      string               `json:"snapshot"`
	CPU           float64              `json:"cpu"`
	Memory        int64                `json:"memory"`
	GPU           *sandboxGPUDTO       `json:"gpu,omitempty"`
	IdleStop      int32                `json:"idle_stop"`
	Workspace     *sandboxWorkspaceDTO `json:"workspace,omitempty"`
	Conditions    []string             `json:"conditions"`
	QueuePosition *int64               `json:"queue_position,omitempty"`
	BlockedReason *string              `json:"blocked_reason,omitempty"`
	CreatedAt     time.Time            `json:"created_at"`
}

type sandboxList struct {
	Items      []sandboxDTO `json:"items"`
	NextCursor *string      `json:"next_cursor"`
}

func (s *Server) handleCreateSandbox(w http.ResponseWriter, r *http.Request, p *principal) {
	key, ok := idempotencyKey(r)
	if !ok {
		writeProblem(w, "invalid_request", "Idempotency-Key is too long")
		return
	}
	ctx := r.Context()
	lookup := func() (store.Sandbox, error) {
		return s.q.GetSandboxByIdempotencyKey(ctx, store.GetSandboxByIdempotencyKeyParams{
			OwnerID: p.user.ID, IdempotencyKey: key,
		})
	}
	if existing, replayed, err := replayExisting(key, lookup); err != nil {
		s.internal(w, "look up idempotent Sandbox", err)
		return
	} else if replayed {
		writeJSON(w, http.StatusOK, s.sandboxToDTO(ctx, existing))
		return
	}

	var req createSandboxRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !s.requireSandboxTokens(w) {
		return
	}
	if req.CPU == nil || *req.CPU <= 0 {
		writeProblem(w, "invalid_request", "cpu must be a positive number of cores")
		return
	}
	if req.Memory == nil || *req.Memory <= 0 || *req.Memory > maxMemoryBytes {
		writeProblem(w, "invalid_request", "memory must be a positive number of bytes")
		return
	}
	if !validLabels(req.Labels) {
		writeProblem(w, "invalid_request", "label keys must be non-empty; keys and values at most 255 characters")
		return
	}
	secretMapping := secretEnvRefs(req.Secrets)
	if len(secretMapping) > 0 {
		envNames := make([]string, 0, len(secretMapping))
		for env := range secretMapping {
			envNames = append(envNames, env)
		}
		slices.Sort(envNames)
		for _, env := range envNames {
			if code, detail := secretEnvNameProblem(env); code != "" {
				writeProblem(w, code, detail)
				return
			}
		}
		if !s.requireSecrets(w) {
			return
		}
	}
	idleStop := s.cfg.IdleStopSeconds
	if req.IdleStop != nil {
		if *req.IdleStop < 0 {
			writeProblem(w, "invalid_request", "idle_stop must be zero or a positive number of seconds")
			return
		}
		idleStop = *req.IdleStop
	}
	gpuCount := int32(0)
	var gpuModel *string
	if req.GPU != nil {
		if req.GPU.Count < 0 {
			writeProblem(w, "invalid_request", "gpu.count must be zero or a positive number")
			return
		}
		gpuCount = req.GPU.Count
		if req.GPU.Count > 0 && req.GPU.Model != nil && *req.GPU.Model != "" {
			if !validDisplayName(*req.GPU.Model) {
				writeProblem(w, "invalid_request", "gpu.model must be at most 255 characters")
				return
			}
			gpuModel = req.GPU.Model
		}
	}
	id, err := uuid.NewV7()
	if err != nil {
		s.internal(w, "generate Sandbox id", err)
		return
	}
	// The Sandbox Token is minted with the row and lives until deletion
	// (ADR-0016, schema.md).
	tokenCt, tokenKeyID, err := s.issueSandboxToken(p.user.ID)
	if err != nil {
		s.internal(w, "issue Sandbox Token", err)
		return
	}
	name := id.String()
	if req.Name != nil {
		if !validDisplayName(*req.Name) {
			writeProblem(w, "invalid_request", "name must be non-empty, trimmed, and at most 255 characters")
			return
		}
		name = *req.Name
	}
	labels, err := json.Marshal(req.Labels)
	if err != nil {
		s.internal(w, "encode labels", err)
		return
	}
	if req.Labels == nil {
		labels = []byte("{}")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		s.internal(w, "begin transaction", err)
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	q := s.q.WithTx(tx)
	// Resolve the Snapshot inside the transaction: the pinned version cannot
	// be deleted between the check and the row that references it.
	snapshot, pinCode, pinDetail := resolveSnapshotPin(ctx, q, s.cfg, p.user.ID, req.Snapshot)
	if pinCode != "" {
		writeProblem(w, pinCode, pinDetail)
		return
	}
	// Quota is checked at admission, under the User's row lock, so concurrent
	// creates cannot both take the last slot (ADR-0015).
	owner, err := q.LockUser(ctx, p.user.ID)
	if err != nil {
		s.internal(w, "lock User for quota", err)
		return
	}
	usage, err := q.CountActiveSandboxResources(ctx, p.user.ID)
	if err != nil {
		s.internal(w, "count active Sandboxes", err)
		return
	}
	if problem := quotaProblem(owner, usage, gpuCount); problem != "" {
		writeProblem(w, "quota_exceeded", problem)
		return
	}
	// Resources resolve to concrete versions now; the rows pin them for the
	// Sandbox's whole life, restarts included (api.md §3, runtime.md §11).
	resources, ok := s.resolveSandboxResources(w, ctx, q, p.user.ID, req.Resources)
	if !ok {
		return
	}
	// An explicit Workspace is attached as requested; otherwise the Sandbox
	// gets an implicit ephemeral one (ADR-0018).
	var attached *store.Workspace
	attachMode := "rw"
	if req.Workspace != nil {
		workspaceID, err := uuid.Parse(req.Workspace.ID)
		if err != nil {
			writeProblem(w, "invalid_request", "workspace.id must be a UUID")
			return
		}
		ws, err := q.GetWorkspace(ctx, workspaceID)
		if errors.Is(err, pgx.ErrNoRows) {
			writeProblem(w, "not_found", "no such Workspace")
			return
		}
		if err != nil {
			s.internal(w, "get Workspace", err)
			return
		}
		if !s.ownerOrAdmin(p, ws.OwnerID) {
			writeProblem(w, "not_found", "no such Workspace")
			return
		}
		if !workspaceUsable(ws) {
			writeProblem(w, "conflict", "the Workspace is not usable")
			return
		}
		attached = &ws
		if req.Workspace.Mode != "" {
			if req.Workspace.Mode != "rw" && req.Workspace.Mode != "ro" {
				writeProblem(w, "invalid_request", "workspace.mode must be rw or ro")
				return
			}
			attachMode = req.Workspace.Mode
		}
	}
	// The first start's Secret snapshot is written with the row: creation is a
	// start, so the ciphertexts are copied now and on every later start
	// (schema.md).
	secretEnvCt, secretEnv, ok := s.writeSecretSnapshot(w, ctx, q, p.user.ID, secretMapping, "invalid_request")
	if !ok {
		return
	}
	sandbox, err := q.CreateSandbox(ctx, store.CreateSandboxParams{
		ID: id, OwnerID: p.user.ID, Name: name, Labels: labels,
		SnapshotDigest: snapshot, Cpu: *req.CPU, Memory: *req.Memory,
		GpuCount: gpuCount, GpuModel: gpuModel,
		IdleStopSeconds: idleStop, IdempotencyKey: key, SecretEnvCt: secretEnvCt,
		TokenCt: tokenCt, TokenKeyID: tokenKeyID,
	})
	if err != nil {
		if existing, replayed := replayConflict(key, err, "sandboxes_owner_idempotency", lookup); replayed {
			writeJSON(w, http.StatusOK, s.sandboxToDTO(ctx, existing))
			return
		}
		if uniqueViolation(err) == "sandboxes_owner_name" {
			writeProblem(w, "conflict", "a Sandbox with that name already exists")
			return
		}
		s.internal(w, "create Sandbox", err)
		return
	}
	for _, resource := range resources {
		if err := q.CreateSandboxResource(ctx, store.CreateSandboxResourceParams{
			SandboxID: sandbox.ID, ResourceVersionID: resource.ResourceVersionID, Mount: resource.Mount,
		}); err != nil {
			s.internal(w, "pin Sandbox Resource", err)
			return
		}
	}
	workspaceID := uuid.Nil
	if attached != nil {
		workspaceID = attached.ID
	} else {
		var err error
		workspaceID, err = uuid.NewV7()
		if err != nil {
			s.internal(w, "generate Workspace id", err)
			return
		}
		if _, err := q.CreateWorkspace(ctx, store.CreateWorkspaceParams{
			ID: workspaceID, OwnerID: p.user.ID, Kind: "ephemeral", SandboxID: &sandbox.ID,
		}); err != nil {
			s.internal(w, "create Workspace", err)
			return
		}
	}
	if _, err := q.CreateAttachment(ctx, store.CreateAttachmentParams{
		SandboxID: sandbox.ID, WorkspaceID: workspaceID, Mode: attachMode,
		Generation: sandbox.Generation,
	}); err != nil {
		if uniqueViolation(err) == "attachments_workspace_rw" {
			writeProblem(w, "conflict", "the Workspace already has a read-write Attachment")
			return
		}
		s.internal(w, "create Attachment", err)
		return
	}
	// The Attachment is released on stop, so the Sandbox remembers the mount.
	if err := q.SetSandboxWorkspace(ctx, store.SetSandboxWorkspaceParams{
		ID: sandbox.ID, WorkspaceID: &workspaceID, WorkspaceMode: attachMode,
	}); err != nil {
		s.internal(w, "record Sandbox Workspace", err)
		return
	}
	sandbox.WorkspaceID, sandbox.WorkspaceMode = &workspaceID, attachMode
	if err := s.enqueueAdmission(ctx, q, sandbox.ID); err != nil {
		s.internal(w, "enqueue admission request", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		s.internal(w, "commit Sandbox", err)
		return
	}
	detail := map[string]any{
		"name": name, "cpu": *req.CPU, "memory": *req.Memory, "gpu": gpuCount, "snapshot": snapshot,
	}
	// The names a Sandbox was created with; never a value (ADR-0013).
	if names := referencedSecretNames(secretEnv); len(names) > 0 {
		detail["secrets"] = names
	}
	s.audit(ctx, p, "sandbox.create", "sandbox", sandbox.ID.String(), detail)
	s.changed(ctx)
	writeJSON(w, http.StatusCreated, s.sandboxToDTO(ctx, sandbox))
}

// quotaProblem returns the 429 detail when a create or start would exceed the
// owner's quota, or "" when it fits (ADR-0015).
func quotaProblem(owner store.User, usage store.CountActiveSandboxResourcesRow, gpuCount int32) string {
	if usage.Sandboxes >= int64(owner.SandboxQuota) {
		return fmt.Sprintf("sandbox quota is %d", owner.SandboxQuota)
	}
	if usage.Gpus+int64(gpuCount) > int64(owner.GpuQuota) {
		return fmt.Sprintf("GPU quota is %d", owner.GpuQuota)
	}
	return ""
}

// resolveSandboxResources turns the request's Resource references into the
// concrete ResourceVersions the Sandbox pins. A missing Resource is 404, a
// missing or non-ready version is 409, and a malformed or duplicate mount is
// 400 (issue #86; api.md §2/§3).
func (s *Server) resolveSandboxResources(w http.ResponseWriter, ctx context.Context, q *store.Queries, ownerID uuid.UUID, refs []sandboxResourceRequest) ([]store.CreateSandboxResourceParams, bool) {
	if len(refs) == 0 {
		return nil, true
	}
	mounts := make(map[string]struct{}, len(refs))
	resolved := make([]store.CreateSandboxResourceParams, 0, len(refs))
	for _, ref := range refs {
		resource, ok := s.resolveSandboxResource(w, ctx, q, ownerID, ref.Resource)
		if !ok {
			return nil, false
		}
		version, ok := s.resolveSandboxResourceVersion(w, ctx, q, resource, ref.Version)
		if !ok {
			return nil, false
		}
		mount := resource.Name
		if ref.Mount != nil && *ref.Mount != "" {
			mount = *ref.Mount
		}
		if !validMountName(mount) {
			writeProblem(w, "invalid_request", "mount must be a single non-empty path segment")
			return nil, false
		}
		if _, duplicate := mounts[mount]; duplicate {
			writeProblem(w, "invalid_request", "mount names must be unique within a Sandbox")
			return nil, false
		}
		mounts[mount] = struct{}{}
		resolved = append(resolved, store.CreateSandboxResourceParams{
			ResourceVersionID: version.ID, Mount: mount,
		})
	}
	return resolved, true
}

// resolveSandboxResource accepts a Resource id or its owner-scoped name.
func (s *Server) resolveSandboxResource(w http.ResponseWriter, ctx context.Context, q *store.Queries, ownerID uuid.UUID, reference string) (store.Resource, bool) {
	if reference == "" {
		writeProblem(w, "invalid_request", "resource is required")
		return store.Resource{}, false
	}
	var resource store.Resource
	var err error
	if id, parseErr := uuid.Parse(reference); parseErr == nil {
		resource, err = q.GetResource(ctx, id)
	} else {
		resource, err = q.GetResourceByName(ctx, store.GetResourceByNameParams{OwnerID: ownerID, Name: reference})
	}
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (resource.OwnerID != ownerID || resource.DeletedAt.Valid)) {
		writeProblem(w, "not_found", "no such Resource")
		return store.Resource{}, false
	}
	if err != nil {
		s.internal(w, "get Resource for Sandbox", err)
		return store.Resource{}, false
	}
	return resource, true
}

// resolveSandboxResourceVersion pins the requested version, or the current
// one. Only a ready version can be mounted (api.md §2).
func (s *Server) resolveSandboxResourceVersion(w http.ResponseWriter, ctx context.Context, q *store.Queries, resource store.Resource, reference *string) (store.ResourceVersion, bool) {
	var versionID uuid.UUID
	if reference == nil || *reference == "" {
		if resource.CurrentVersionID == nil {
			writeProblem(w, "conflict", "the Resource has no current version")
			return store.ResourceVersion{}, false
		}
		versionID = *resource.CurrentVersionID
	} else {
		id, err := uuid.Parse(*reference)
		if err != nil {
			writeProblem(w, "invalid_request", "version must be a ResourceVersion id")
			return store.ResourceVersion{}, false
		}
		versionID = id
	}
	version, err := q.GetResourceVersion(ctx, versionID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && version.ResourceID != resource.ID) {
		writeProblem(w, "conflict", "the ResourceVersion does not belong to the Resource")
		return store.ResourceVersion{}, false
	}
	if err != nil {
		s.internal(w, "read ResourceVersion for Sandbox", err)
		return store.ResourceVersion{}, false
	}
	if ResourceVersionState(version.State) != ResourceVersionReady {
		writeProblem(w, "conflict", "the ResourceVersion is not ready")
		return store.ResourceVersion{}, false
	}
	return version, true
}

// validMountName keeps the mount a single path segment: no separators, no
// traversal, no NUL (runtime.md §2).
func validMountName(mount string) bool {
	return mount != "" && mount != "." && mount != ".." &&
		len(mount) <= maxNameLen && !strings.ContainsAny(mount, "/\x00")
}

// sandboxConditions is the Sandbox's conditions column. Only the conditions
// the Agent actually reports are defined (api.md §3).
type sandboxConditions struct {
	FetchingResources bool `json:"fetching_resources,omitempty"`
}

func parseSandboxConditions(raw json.RawMessage) sandboxConditions {
	var c sandboxConditions
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &c)
	}
	return c
}

func (c sandboxConditions) names() []string {
	names := []string{}
	if c.FetchingResources {
		names = append(names, "fetching_resources")
	}
	return names
}

func encodeSandboxConditions(c sandboxConditions) json.RawMessage {
	raw, err := json.Marshal(c)
	if err != nil {
		return nil
	}
	return raw
}

// enqueueAdmission opens the admission request that holds the Sandbox's quota
// slot until it is placed or the deadline passes.
func (s *Server) enqueueAdmission(ctx context.Context, q *store.Queries, sandboxID uuid.UUID) error {
	deadline := time.Now().Add(s.cfg.AdmissionTimeout)
	id, err := uuid.NewV7()
	if err != nil {
		return err
	}
	// A restart can race the sweep that ends the previous request; canceling
	// first keeps the open-request unique index honest.
	if _, err := q.EndAdmissionRequest(ctx, store.EndAdmissionRequestParams{
		SandboxID: sandboxID, Outcome: strPtr("canceled"),
	}); err != nil {
		return err
	}
	_, err = q.CreateAdmissionRequest(ctx, store.CreateAdmissionRequestParams{
		ID: id, SandboxID: sandboxID, Priority: "user", DeadlineAt: timestamptzPtr(&deadline),
	})
	return err
}

// sandboxListOwner resolves GET /sandboxes' owner filter: a caller always sees
// its own Sandboxes, and an admin may name another User (api.md §3).
func sandboxListOwner(p *principal, raw string) (uuid.UUID, bool) {
	if raw == "" || !p.isAdmin() {
		return p.user.ID, true
	}
	id, err := uuid.Parse(raw)
	return id, err == nil
}

func (s *Server) handleListSandboxes(w http.ResponseWriter, r *http.Request, p *principal) {
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
	owner, ok := sandboxListOwner(p, r.URL.Query().Get("owner"))
	if !ok {
		writeProblem(w, "invalid_request", "owner must be a User id")
		return
	}
	params := store.ListSandboxesParams{OwnerID: owner, After: after, Limit: limit + 1}
	if state := r.URL.Query().Get("state"); state != "" {
		if !slices.Contains(sandboxStateNames, state) {
			writeProblem(w, "invalid_request", "unknown Sandbox state")
			return
		}
		params.State = &state
	}
	if raw := r.URL.Query().Get("node"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			writeProblem(w, "invalid_request", "node must be a UUID")
			return
		}
		params.NodeID = &id
	}
	if raw := r.URL.Query().Get("has_gpu"); raw != "" {
		hasGPU, err := strconv.ParseBool(raw)
		if err != nil {
			writeProblem(w, "invalid_request", "has_gpu must be true or false")
			return
		}
		params.HasGpu = &hasGPU
	}
	if raw := r.URL.Query().Get("label"); raw != "" {
		key, value, found := strings.Cut(raw, "=")
		if !found || !validLabel(key) || !validLabel(value) {
			writeProblem(w, "invalid_request", "label must be key=value")
			return
		}
		params.LabelKey, params.LabelValue = &key, &value
	}
	rows, err := s.q.ListSandboxes(r.Context(), params)
	if err != nil {
		s.internal(w, "list Sandboxes", err)
		return
	}
	rows, next := trimPage(rows, limit, func(sb store.Sandbox) string { return EncodeCursor(sb.ID) })
	list := sandboxList{Items: make([]sandboxDTO, 0, len(rows)), NextCursor: next}
	for _, row := range rows {
		list.Items = append(list.Items, s.sandboxToDTO(r.Context(), row))
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleGetSandbox(w http.ResponseWriter, r *http.Request, p *principal) {
	id, ok := pathUUID(r, "id")
	if !ok {
		writeProblem(w, "invalid_request", "id must be a UUID")
		return
	}
	sb, ok := s.getOwnedSandbox(w, r.Context(), p, id)
	if !ok {
		return
	}
	if want := r.URL.Query().Get("wait_for"); want != "" {
		if !slices.Contains(sandboxStateNames, want) {
			writeProblem(w, "invalid_request", "unknown Sandbox state")
			return
		}
		sb = s.waitForSandbox(r.Context(), sb, SandboxState(want))
	}
	writeJSON(w, http.StatusOK, s.sandboxToDTO(r.Context(), sb))
}

func (s *Server) handleStartSandbox(w http.ResponseWriter, r *http.Request, p *principal) {
	id, ok := pathUUID(r, "id")
	if !ok {
		writeProblem(w, "invalid_request", "id must be a UUID")
		return
	}
	ctx := r.Context()
	sb, ok := s.getOwnedSandbox(w, ctx, p, id)
	if !ok {
		return
	}
	if SandboxState(sb.State) == SandboxFailed {
		writeProblem(w, "conflict", "a failed Sandbox cannot be started; delete it")
		return
	}
	if SandboxState(sb.State) != SandboxStopped {
		writeProblem(w, "conflict", "the Sandbox is not stopped")
		return
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		s.internal(w, "begin transaction", err)
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	q := s.q.WithTx(tx)
	owner, err := q.LockUser(ctx, sb.OwnerID)
	if err != nil {
		s.internal(w, "lock Sandbox owner for quota", err)
		return
	}
	usage, err := q.CountActiveSandboxResources(ctx, sb.OwnerID)
	if err != nil {
		s.internal(w, "count active Sandboxes", err)
		return
	}
	started, err := q.StartSandbox(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeProblem(w, "conflict", "the Sandbox state changed; retry")
		return
	}
	if err != nil {
		s.internal(w, "start Sandbox", err)
		return
	}
	// Re-admission checks the same quota as creation (api.md §2). The stopped
	// Sandbox is not counted yet, so usage is the other Sandboxes.
	if quotaProblem(owner, usage, started.GpuCount) != "" {
		cause := "quota_exceeded"
		if _, err := q.FailPendingSandbox(ctx, store.FailPendingSandboxParams{
			ID: id, Generation: started.Generation, Cause: &cause,
		}); err != nil {
			s.internal(w, "fail Sandbox on quota", err)
			return
		}
		failed, err := q.GetSandbox(ctx, id)
		if err != nil {
			s.internal(w, "read failed Sandbox", err)
			return
		}
		if err := tx.Commit(ctx); err != nil {
			s.internal(w, "commit quota failure", err)
			return
		}
		s.changed(ctx)
		writeJSON(w, http.StatusAccepted, s.sandboxToDTO(ctx, failed))
		return
	}
	ws, err := q.GetSandboxWorkspace(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeProblem(w, "conflict", "the Sandbox has no Workspace")
		return
	}
	if err != nil {
		s.internal(w, "look up Workspace", err)
		return
	}
	if !workspaceUsable(ws) {
		writeProblem(w, "conflict", "the Workspace is not usable")
		return
	}
	// Stopping released the Attachment; start re-acquires it with the mode the
	// Sandbox was created with (ADR-0018). Admission re-acquires the
	// Allocation.
	if _, err := q.CreateAttachment(ctx, store.CreateAttachmentParams{
		SandboxID: id, WorkspaceID: ws.ID, Mode: sb.WorkspaceMode, Generation: started.Generation,
	}); err != nil {
		if uniqueViolation(err) == "attachments_workspace_rw" {
			writeProblem(w, "conflict", "the Workspace already has a read-write Attachment")
			return
		}
		s.internal(w, "re-create Attachment", err)
		return
	}
	// Every start re-resolves the Secret snapshot by name, so a Secret changed
	// or deleted while stopped is visible only from this start on
	// (schema.md). A deleted Secret fails the start instead of silently
	// dropping a variable.
	previous, err := decodeSecretEnv(started.SecretEnvCt)
	if err != nil {
		s.internal(w, "decode Secret snapshot", err)
		return
	}
	if len(previous) > 0 && !s.requireSecrets(w) {
		return
	}
	secretEnvCt, _, ok := s.writeSecretSnapshot(w, ctx, q, sb.OwnerID, previous, "conflict")
	if !ok {
		return
	}
	if err := q.SetSandboxSecretEnv(ctx, store.SetSandboxSecretEnvParams{
		ID: id, SecretEnvCt: secretEnvCt,
	}); err != nil {
		s.internal(w, "write Secret snapshot", err)
		return
	}
	if err := s.enqueueAdmission(ctx, q, id); err != nil {
		s.internal(w, "enqueue admission request", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		s.internal(w, "commit start", err)
		return
	}
	s.audit(ctx, p, "sandbox.start", "sandbox", id.String(), nil)
	s.changed(ctx)
	writeJSON(w, http.StatusAccepted, s.sandboxToDTO(ctx, started))
}

func (s *Server) handleStopSandbox(w http.ResponseWriter, r *http.Request, p *principal) {
	id, ok := pathUUID(r, "id")
	if !ok {
		writeProblem(w, "invalid_request", "id must be a UUID")
		return
	}
	ctx := r.Context()
	sb, ok := s.getOwnedSandbox(w, ctx, p, id)
	if !ok {
		return
	}
	switch SandboxState(sb.State) {
	case SandboxStopped:
		writeJSON(w, http.StatusAccepted, s.sandboxToDTO(r.Context(), sb))
		return
	case SandboxDeleting, SandboxDeleted:
		writeProblem(w, "conflict", "the Sandbox is being deleted")
		return
	}
	stopped, err := s.q.StopSandbox(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		// Already stopping: the action is idempotent.
		if current, getErr := s.q.GetSandbox(ctx, id); getErr == nil {
			writeJSON(w, http.StatusAccepted, s.sandboxToDTO(r.Context(), current))
			return
		}
		writeProblem(w, "conflict", "the Sandbox state changed; retry")
		return
	}
	if err != nil {
		s.internal(w, "stop Sandbox", err)
		return
	}
	s.audit(ctx, p, "sandbox.stop", "sandbox", id.String(), nil)
	s.changed(ctx)
	writeJSON(w, http.StatusAccepted, s.sandboxToDTO(r.Context(), stopped))
}

func (s *Server) handleDeleteSandbox(w http.ResponseWriter, r *http.Request, p *principal) {
	id, ok := pathUUID(r, "id")
	if !ok {
		writeProblem(w, "invalid_request", "id must be a UUID")
		return
	}
	ctx := r.Context()
	sb, ok := s.getOwnedSandbox(w, ctx, p, id)
	if !ok {
		return
	}
	if SandboxState(sb.State) == SandboxDeleting {
		writeJSON(w, http.StatusAccepted, s.sandboxToDTO(r.Context(), sb))
		return
	}
	deleted, err := s.q.DeleteSandbox(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeProblem(w, "conflict", "the Sandbox state changed; retry")
		return
	}
	if err != nil {
		s.internal(w, "delete Sandbox", err)
		return
	}
	s.audit(ctx, p, "sandbox.delete", "sandbox", id.String(), nil)
	s.changed(ctx)
	writeJSON(w, http.StatusAccepted, s.sandboxToDTO(r.Context(), deleted))
}

func (s *Server) getOwnedSandbox(w http.ResponseWriter, ctx context.Context, p *principal, id uuid.UUID) (store.Sandbox, bool) {
	sb, err := s.q.GetSandbox(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !s.ownerOrAdmin(p, sb.OwnerID)) {
		writeProblem(w, "not_found", "no such Sandbox")
		return store.Sandbox{}, false
	}
	if err != nil {
		s.internal(w, "get Sandbox", err)
		return store.Sandbox{}, false
	}
	return sb, true
}

func (s *Server) ownerOrAdmin(p *principal, owner uuid.UUID) bool {
	return p.isAdmin() || p.user.ID == owner
}

// waitForSandbox polls until the wanted state, a terminal state, or the
// 60-second cap. Expiry returns the current state, not an error (api.md §1).
func (s *Server) waitForSandbox(ctx context.Context, sb store.Sandbox, want SandboxState) store.Sandbox {
	deadline := time.Now().Add(waitForLimit)
	for state := SandboxState(sb.State); state != want && state != SandboxFailed && state != SandboxDeleted && time.Now().Before(deadline); state = SandboxState(sb.State) {
		select {
		case <-ctx.Done():
			return sb
		case <-time.After(250 * time.Millisecond):
		}
		current, err := s.q.GetSandbox(ctx, sb.ID)
		if err != nil {
			return sb
		}
		sb = current
	}
	return sb
}

func (s *Server) sandboxToDTO(ctx context.Context, sb store.Sandbox) sandboxDTO {
	labels := map[string]string{}
	if len(sb.Labels) > 0 {
		_ = json.Unmarshal(sb.Labels, &labels)
	}
	dto := sandboxDTO{
		ID: sb.ID.String(), Name: sb.Name, Labels: labels,
		State: sb.State, Cause: sb.Cause, NodeID: uuidPtr(sb.NodeID),
		Snapshot: sb.SnapshotDigest, CPU: sb.Cpu, Memory: sb.Memory,
		IdleStop: sb.IdleStopSeconds, CreatedAt: sb.CreatedAt.Time.UTC(),
		Conditions: parseSandboxConditions(sb.Conditions).names(),
	}
	if sb.GpuCount > 0 {
		dto.GPU = &sandboxGPUDTO{Count: sb.GpuCount, Model: sb.GpuModel}
	}
	if sb.WorkspaceID != nil {
		dto.Workspace = &sandboxWorkspaceDTO{ID: sb.WorkspaceID.String(), Mode: sb.WorkspaceMode}
	}
	// queue_position exists only while the admission request is open. Ignoring
	// a failed read degrades the display, not the response.
	if SandboxState(sb.State) == SandboxPending {
		if position, err := s.q.AdmissionQueuePosition(ctx, sb.ID); err == nil {
			hint := queueHint(sb)
			dto.QueuePosition = &position
			dto.BlockedReason = &hint
		}
	}
	return dto
}

// queueHint describes why a pending Sandbox is waiting. It is deliberately
// coarse: the exact fitting decision is the admission sweep's.
func queueHint(sb store.Sandbox) string {
	if sb.NodeID == nil {
		return "waiting for Node capacity"
	}
	return "the bound Node cannot take the Sandbox"
}

func validLabel(s string) bool {
	return s != "" && len(s) <= maxNameLen
}

func validLabels(labels map[string]string) bool {
	for key, value := range labels {
		if !validLabel(key) || len(value) > maxNameLen {
			return false
		}
	}
	return true
}

// changed wakes the Reconciler after the caller committed a desired-state
// write. NOTIFY never fails the request; the periodic tick is the fallback.
func (s *Server) changed(ctx context.Context) {
	if _, err := s.pool.Exec(ctx, "SELECT pg_notify($1, '')", reconcileChannel); err != nil && ctx.Err() == nil {
		s.log.Warn("notify reconciler", "error", err)
	}
	s.reconcile.notify()
}
