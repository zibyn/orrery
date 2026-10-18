package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/zibyn/stars-sandbox/internal/registry"
	"github.com/zibyn/stars-sandbox/internal/store"
)

// Snapshot import (api.md §3, ADR-0021). The Control Plane copies a whole
// index into the platform registry while keeping its digest; the copy runs
// in-process, so a restart turns a still-building import into a failed one.

type createSnapshotRequest struct {
	Name string `json:"name"`
}

type createSnapshotVersionRequest struct {
	Import *snapshotImportRequest `json:"import"`
	Build  *snapshotBuildRequest  `json:"build"`
}

type snapshotImportRequest struct {
	Ref        string  `json:"ref"`
	AuthSecret *string `json:"auth_secret"`
}

type setSnapshotCurrentRequest struct {
	Version string `json:"version"`
}

type snapshotDTO struct {
	ID               string     `json:"id"`
	OwnerID          *string    `json:"owner_id"`
	Name             string     `json:"name"`
	CurrentVersionID *string    `json:"current_version_id"`
	CreatedAt        *time.Time `json:"created_at"`
	DeletedAt        *time.Time `json:"deleted_at,omitempty"`
}

type snapshotVersionDTO struct {
	ID             string          `json:"id"`
	SnapshotID     string          `json:"snapshot_id"`
	Digest         *string         `json:"digest"`
	State          string          `json:"state"`
	Cause          *string         `json:"cause"`
	Source         string          `json:"source"`
	BuildSandboxID *string         `json:"build_sandbox_id,omitempty"`
	BuildExecID    *string         `json:"build_exec_id,omitempty"`
	Provenance     json.RawMessage `json:"provenance,omitempty"`
	CreatedAt      *time.Time      `json:"created_at"`
}

type snapshotList struct {
	Items      []snapshotDTO `json:"items"`
	NextCursor *string       `json:"next_cursor"`
}

type snapshotVersionList struct {
	Items      []snapshotVersionDTO `json:"items"`
	NextCursor *string              `json:"next_cursor"`
}

func snapshotToDTO(s store.Snapshot) snapshotDTO {
	return snapshotDTO{
		ID:               s.ID.String(),
		OwnerID:          uuidPtr(s.OwnerID),
		Name:             s.Name,
		CurrentVersionID: uuidPtr(s.CurrentVersionID),
		CreatedAt:        timePtr(s.CreatedAt),
		DeletedAt:        timePtr(s.DeletedAt),
	}
}

func snapshotVersionToDTO(v store.SnapshotVersion) snapshotVersionDTO {
	return snapshotVersionDTO{
		ID:             v.ID.String(),
		SnapshotID:     v.SnapshotID.String(),
		Digest:         v.Digest,
		State:          v.State,
		Cause:          v.Cause,
		Source:         v.Source,
		BuildSandboxID: uuidPtr(v.BuildSandboxID),
		BuildExecID:    uuidPtr(v.BuildExecID),
		Provenance:     v.Provenance,
		CreatedAt:      timePtr(v.CreatedAt),
	}
}

// snapshotVisible applies api.md §1: a platform Snapshot is readable by
// everyone, a user Snapshot by its owner and admins.
func snapshotVisible(p *principal, snap store.Snapshot) bool {
	if snap.OwnerID == nil {
		return true
	}
	return p != nil && (*snap.OwnerID == p.user.ID || p.isAdmin())
}

// loadSnapshot resolves a path id and writes not_found for a missing or
// invisible Snapshot.
func (s *Server) loadSnapshot(ctx context.Context, w http.ResponseWriter, p *principal, raw string) (store.Snapshot, bool) {
	id, err := uuid.Parse(raw)
	if err != nil {
		writeProblem(w, "not_found", "no such Snapshot")
		return store.Snapshot{}, false
	}
	snap, err := s.q.GetSnapshot(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !snapshotVisible(p, snap)) {
		writeProblem(w, "not_found", "no such Snapshot")
		return store.Snapshot{}, false
	}
	if err != nil {
		s.internal(w, "get Snapshot", err)
		return store.Snapshot{}, false
	}
	return snap, true
}

// requireWritableSnapshot rejects platform mutations (api.md §3: 403) and
// other Users' rows (404).
func (s *Server) requireWritableSnapshot(w http.ResponseWriter, p *principal, snap store.Snapshot) bool {
	if snap.OwnerID == nil {
		writeProblem(w, "permission_denied", "platform Snapshots cannot be changed")
		return false
	}
	if *snap.OwnerID != p.user.ID && !p.isAdmin() {
		writeProblem(w, "not_found", "no such Snapshot")
		return false
	}
	return true
}

func (s *Server) handleCreateSnapshot(w http.ResponseWriter, r *http.Request, p *principal) {
	var req createSnapshotRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !validDisplayName(req.Name) {
		writeProblem(w, "invalid_request", "name must be non-empty, trimmed, and at most 255 characters")
		return
	}
	id, err := uuid.NewV7()
	if err != nil {
		s.internal(w, "generate Snapshot id", err)
		return
	}
	ctx := r.Context()
	snap, err := s.q.CreateSnapshot(ctx, store.CreateSnapshotParams{
		ID: id, OwnerID: &p.user.ID, Name: req.Name,
	})
	if err != nil {
		if uniqueViolation(err) == "snapshots_owner_name" {
			writeProblem(w, "conflict", "a Snapshot with that name already exists")
			return
		}
		s.internal(w, "create Snapshot", err)
		return
	}
	s.audit(ctx, p, "snapshot.create", "snapshot", snap.ID.String(), map[string]any{"name": snap.Name})
	writeJSON(w, http.StatusCreated, snapshotToDTO(snap))
}

func (s *Server) handleListSnapshots(w http.ResponseWriter, r *http.Request, p *principal) {
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
	rows, err := s.q.ListSnapshots(r.Context(), store.ListSnapshotsParams{
		OwnerID: &p.user.ID, After: after, Limit: limit + 1,
	})
	if err != nil {
		s.internal(w, "list Snapshots", err)
		return
	}
	page, next := trimPage(rows, limit, func(s store.Snapshot) string { return EncodeCursor(s.ID) })
	items := make([]snapshotDTO, 0, len(page))
	for _, row := range page {
		items = append(items, snapshotToDTO(row))
	}
	writeJSON(w, http.StatusOK, snapshotList{Items: items, NextCursor: next})
}

func (s *Server) handleGetSnapshot(w http.ResponseWriter, r *http.Request, p *principal) {
	snap, ok := s.loadSnapshot(r.Context(), w, p, r.PathValue("id"))
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, snapshotToDTO(snap))
}

func (s *Server) handleDeleteSnapshot(w http.ResponseWriter, r *http.Request, p *principal) {
	snap, ok := s.loadSnapshot(r.Context(), w, p, r.PathValue("id"))
	if !ok {
		return
	}
	if !s.requireWritableSnapshot(w, p, snap) {
		return
	}
	deleted, err := s.q.DeleteSnapshot(r.Context(), store.DeleteSnapshotParams{
		ID: snap.ID, OwnerID: snap.OwnerID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeProblem(w, "not_found", "no such Snapshot")
		return
	}
	if err != nil {
		s.internal(w, "delete Snapshot", err)
		return
	}
	// Versions stay until #90's GC finds them unreferenced (ADR-0012).
	s.audit(r.Context(), p, "snapshot.delete", "snapshot", snap.ID.String(), map[string]any{"name": snap.Name})
	writeJSON(w, http.StatusAccepted, snapshotToDTO(deleted))
}

func (s *Server) handleCreateSnapshotVersion(w http.ResponseWriter, r *http.Request, p *principal) {
	snap, ok := s.loadSnapshot(r.Context(), w, p, r.PathValue("id"))
	if !ok {
		return
	}
	if !s.requireWritableSnapshot(w, p, snap) {
		return
	}
	key, ok := idempotencyKey(r)
	if !ok {
		writeProblem(w, "invalid_request", "Idempotency-Key is too long")
		return
	}
	ctx := r.Context()
	lookup := func() (store.SnapshotVersion, error) {
		return s.q.GetSnapshotVersionByIdempotencyKey(ctx, store.GetSnapshotVersionByIdempotencyKeyParams{
			SnapshotID: snap.ID, IdempotencyKey: key,
		})
	}
	// The idempotent answer is the version DTO.
	if existing, replayed, err := replayExisting(key, lookup); err != nil {
		s.internal(w, "look up idempotent SnapshotVersion", err)
		return
	} else if replayed {
		writeJSON(w, http.StatusOK, snapshotVersionToDTO(existing))
		return
	}
	var req createSnapshotVersionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if (req.Import == nil) == (req.Build == nil) {
		writeProblem(w, "invalid_request", "exactly one of import or build is required")
		return
	}
	if req.Build != nil {
		s.createBuild(w, r, p, snap, *req.Build, key)
		return
	}
	source, err := registry.ParseSourceRef(req.Import.Ref)
	if err != nil {
		writeProblem(w, "invalid_request", err.Error())
		return
	}
	// A ref naming the platform registry by its Node-reachable address is
	// reachable only through the Control Plane's push address.
	source, err = registry.RewriteRegistry(source, s.cfg.RegistryAddr, s.cfg.RegistryPushAddr)
	if err != nil {
		writeProblem(w, "invalid_request", "ref is not a valid image reference")
		return
	}
	if s.cfg.RegistryPushAddr == "" {
		writeProblem(w, "unavailable", "the platform registry is not configured; set STARS_REGISTRY_ADDR")
		return
	}
	sourceAuth, code, detail := s.sourceCredential(ctx, p.user.ID, req.Import.AuthSecret)
	if code != "" {
		writeProblem(w, code, detail)
		return
	}
	id, err := uuid.NewV7()
	if err != nil {
		s.internal(w, "generate SnapshotVersion id", err)
		return
	}
	version, err := s.q.CreateSnapshotVersion(ctx, store.CreateSnapshotVersionParams{
		ID: id, SnapshotID: snap.ID, State: string(SnapshotVersionBuilding),
		Source: "import", IdempotencyKey: key,
	})
	if err != nil {
		if existing, replayed := replayConflict(key, err, "snapshot_versions_idempotency", lookup); replayed {
			writeJSON(w, http.StatusOK, snapshotVersionToDTO(existing))
			return
		}
		s.internal(w, "create SnapshotVersion", err)
		return
	}
	s.audit(ctx, p, "snapshot.version.create", "snapshot", snap.ID.String(), map[string]any{
		"version": version.ID.String(), "source": version.Source, "ref": source.String(),
	})
	go s.runImport(version.ID, snap.ID, registry.Repo(snap.ID, platformName(snap)), source, sourceAuth)
	writeJSON(w, http.StatusCreated, snapshotVersionToDTO(version))
}

// platformName returns the registry repository name for a platform Snapshot,
// and "" for a user Snapshot (whose repository is keyed by id).
func platformName(snap store.Snapshot) string {
	if snap.OwnerID == nil {
		return snap.Name
	}
	return ""
}

// sourceCredential decrypts the caller's auth_secret: a Secret whose value is
// a Docker config.json for the source registry. No Secret means anonymous.
func (s *Server) sourceCredential(ctx context.Context, owner uuid.UUID, name *string) (*registry.Credential, string, string) {
	if name == nil || *name == "" {
		return nil, "", ""
	}
	if s.cfg.EncryptionKeys == nil {
		return nil, "unavailable", "Secret storage is not configured: set STARS_ENCRYPTION_KEYS_FILE"
	}
	row, err := s.q.GetSecretByName(ctx, store.GetSecretByNameParams{OwnerID: owner, Name: *name})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "not_found", "no such Secret"
	}
	if err != nil {
		s.log.Error("resolve auth Secret", "secret", *name, "error", err)
		return nil, "internal", "resolve auth Secret"
	}
	value, err := s.cfg.EncryptionKeys.Decrypt(row.Ciphertext, row.KeyID, []byte(owner.String()))
	if err != nil {
		s.log.Error("decrypt auth Secret", "secret", *name, "error", err)
		return nil, "internal", "decrypt auth Secret"
	}
	cred, err := registry.ParseCredential(string(value))
	if err != nil {
		return nil, "invalid_request", "auth_secret is not a Docker config.json"
	}
	// The source ref may have been rewritten to the Control Plane's push
	// address; accept the Node-reachable entry for it as well.
	cred.Alias(s.cfg.RegistryPushAddr, s.cfg.RegistryAddr)
	return cred, "", ""
}

// runImport copies the source index and records the outcome. The cancel
// handler may have moved the version out of building first; the conditional
// updates then leave it alone.
func (s *Server) runImport(versionID, snapshotID uuid.UUID, repo string, source name.Digest,
	sourceAuth *registry.Credential) {
	ctx, cancel := context.WithCancel(context.Background())
	s.snapshotMu.Lock()
	s.snapshotCancels[versionID] = cancel
	s.snapshotMu.Unlock()
	defer func() {
		s.snapshotMu.Lock()
		delete(s.snapshotCancels, versionID)
		s.snapshotMu.Unlock()
		cancel()
	}()

	target, err := name.NewDigest(registry.Ref(s.cfg.RegistryPushAddr, repo, source.DigestStr()))
	if err == nil {
		err = registry.Import(ctx, source, target, sourceAuth, s.cfg.RegistryCredentialDoc)
	}
	if err != nil {
		s.log.Error("snapshot import failed", "version", versionID, "source", source.String(), "error", err)
		if ctx.Err() != nil {
			// The cancel handler sets cancelled; this closes the race where
			// the import lost before the handler could move the row.
			_, _ = s.q.CancelSnapshotVersion(context.Background(), versionID)
			return
		}
		_, markErr := s.q.MarkSnapshotVersionFailed(context.Background(), store.MarkSnapshotVersionFailedParams{
			ID: versionID, Cause: strPtr("import_failed"),
		})
		if markErr != nil && !errors.Is(markErr, pgx.ErrNoRows) {
			s.log.Error("record failed Snapshot import", "version", versionID, "error", markErr)
		}
		return
	}

	digest := source.DigestStr()
	tx, err := s.pool.Begin(context.Background())
	if err != nil {
		s.log.Error("begin Snapshot import commit", "version", versionID, "error", err)
		return
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck // no-op after commit
	q := s.q.WithTx(tx)
	ready, err := q.MarkSnapshotVersionReady(context.Background(), store.MarkSnapshotVersionReadyParams{
		ID: versionID, Digest: &digest,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return // cancelled while copying
	}
	if err != nil {
		s.log.Error("mark Snapshot import ready", "version", versionID, "error", err)
		return
	}
	if _, err := q.SetSnapshotCurrentVersion(context.Background(), store.SetSnapshotCurrentVersionParams{
		ID: ready.SnapshotID, VersionID: &versionID,
	}); err != nil {
		s.log.Error("move Snapshot current pointer", "snapshot", ready.SnapshotID, "error", err)
		return
	}
	if err := tx.Commit(context.Background()); err != nil {
		s.log.Error("commit Snapshot import", "version", versionID, "error", err)
	}
}

func (s *Server) handleListSnapshotVersions(w http.ResponseWriter, r *http.Request, p *principal) {
	snap, ok := s.loadSnapshot(r.Context(), w, p, r.PathValue("id"))
	if !ok {
		return
	}
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
	rows, err := s.q.ListSnapshotVersions(r.Context(), store.ListSnapshotVersionsParams{
		SnapshotID: snap.ID, After: after, Limit: limit + 1,
	})
	if err != nil {
		s.internal(w, "list SnapshotVersions", err)
		return
	}
	page, next := trimPage(rows, limit, func(v store.SnapshotVersion) string { return EncodeCursor(v.ID) })
	items := make([]snapshotVersionDTO, 0, len(page))
	for _, row := range page {
		items = append(items, snapshotVersionToDTO(row))
	}
	writeJSON(w, http.StatusOK, snapshotVersionList{Items: items, NextCursor: next})
}

// loadVersion resolves {v} inside {id}, writing not_found when either is
// missing.
func (s *Server) loadVersion(ctx context.Context, w http.ResponseWriter, snap store.Snapshot, raw string) (store.SnapshotVersion, bool) {
	id, err := uuid.Parse(raw)
	if err != nil {
		writeProblem(w, "not_found", "no such SnapshotVersion")
		return store.SnapshotVersion{}, false
	}
	version, err := s.q.GetSnapshotVersion(ctx, store.GetSnapshotVersionParams{
		ID: id, SnapshotID: snap.ID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeProblem(w, "not_found", "no such SnapshotVersion")
		return store.SnapshotVersion{}, false
	}
	if err != nil {
		s.internal(w, "get SnapshotVersion", err)
		return store.SnapshotVersion{}, false
	}
	return version, true
}

func (s *Server) handleGetSnapshotVersion(w http.ResponseWriter, r *http.Request, p *principal) {
	snap, ok := s.loadSnapshot(r.Context(), w, p, r.PathValue("id"))
	if !ok {
		return
	}
	version, ok := s.loadVersion(r.Context(), w, snap, r.PathValue("v"))
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, snapshotVersionToDTO(version))
}

func (s *Server) handleDeleteSnapshotVersion(w http.ResponseWriter, r *http.Request, p *principal) {
	snap, ok := s.loadSnapshot(r.Context(), w, p, r.PathValue("id"))
	if !ok {
		return
	}
	if !s.requireWritableSnapshot(w, p, snap) {
		return
	}
	version, ok := s.loadVersion(r.Context(), w, snap, r.PathValue("v"))
	if !ok {
		return
	}
	if snap.CurrentVersionID != nil && *snap.CurrentVersionID == version.ID {
		writeProblem(w, "conflict", "the current SnapshotVersion cannot be deleted")
		return
	}
	if version.Source == "build" && version.State == string(SnapshotVersionBuilding) {
		// The build row and Exec reference the version; deleting it would
		// orphan a running build.
		writeProblem(w, "conflict", "cancel the build before deleting its SnapshotVersion")
		return
	}
	if version.Digest != nil {
		ref := registry.Repo(snap.ID, platformName(snap)) + "@" + *version.Digest
		count, err := s.q.CountSandboxSnapshotReferences(r.Context(), ref)
		if err != nil {
			s.internal(w, "count SnapshotVersion references", err)
			return
		}
		if count > 0 {
			writeProblem(w, "conflict", "a Sandbox references this SnapshotVersion")
			return
		}
	}
	// A copy in flight must not resurrect the row after it is gone.
	if cancel, ok := s.takeSnapshotCancel(version.ID); ok {
		cancel()
	}
	if err := s.q.DeleteSnapshotVersion(r.Context(), store.DeleteSnapshotVersionParams{
		ID: version.ID, SnapshotID: snap.ID,
	}); err != nil {
		s.internal(w, "delete SnapshotVersion", err)
		return
	}
	s.audit(r.Context(), p, "snapshot.version.delete", "snapshot", snap.ID.String(), map[string]any{
		"version": version.ID.String(),
	})
	writeJSON(w, http.StatusAccepted, snapshotVersionToDTO(version))
}

func (s *Server) handleSetSnapshotCurrent(w http.ResponseWriter, r *http.Request, p *principal) {
	snap, ok := s.loadSnapshot(r.Context(), w, p, r.PathValue("id"))
	if !ok {
		return
	}
	if !s.requireWritableSnapshot(w, p, snap) {
		return
	}
	var req setSnapshotCurrentRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	version, ok := s.loadVersion(r.Context(), w, snap, req.Version)
	if !ok {
		return
	}
	if version.State != string(SnapshotVersionReady) {
		writeProblem(w, "conflict", "only a ready SnapshotVersion can be current")
		return
	}
	updated, err := s.q.SetSnapshotCurrentVersion(r.Context(), store.SetSnapshotCurrentVersionParams{
		ID: snap.ID, VersionID: &version.ID,
	})
	if err != nil {
		s.internal(w, "set Snapshot current version", err)
		return
	}
	s.audit(r.Context(), p, "snapshot.current", "snapshot", snap.ID.String(), map[string]any{
		"version": version.ID.String(),
	})
	writeJSON(w, http.StatusOK, snapshotToDTO(updated))
}

func (s *Server) handleCancelSnapshotVersion(w http.ResponseWriter, r *http.Request, p *principal) {
	snap, ok := s.loadSnapshot(r.Context(), w, p, r.PathValue("id"))
	if !ok {
		return
	}
	if !s.requireWritableSnapshot(w, p, snap) {
		return
	}
	version, ok := s.loadVersion(r.Context(), w, snap, r.PathValue("v"))
	if !ok {
		return
	}
	switch version.State {
	case string(SnapshotVersionBuilding):
		updated, err := s.q.CancelSnapshotVersion(r.Context(), version.ID)
		if errors.Is(err, pgx.ErrNoRows) {
			// The copy finished between the read and the update.
			writeProblem(w, "conflict", "the SnapshotVersion is no longer building")
			return
		}
		if err != nil {
			s.internal(w, "cancel SnapshotVersion", err)
			return
		}
		if cancel, ok := s.takeSnapshotCancel(version.ID); ok {
			cancel()
		}
		// A build is stopped by the Reconciler through CancelExec (runtime.md
		// §12); the Exec's cancel intent is the durable form of that command.
		if version.Source == "build" && version.BuildExecID != nil {
			if _, err := s.q.CancelExec(r.Context(), *version.BuildExecID); err != nil && !errors.Is(err, pgx.ErrNoRows) {
				s.log.Warn("request build cancel", "version_id", version.ID, "error", err)
			}
			s.changed(r.Context())
		}
		s.audit(r.Context(), p, "snapshot.version.cancel", "snapshot", snap.ID.String(), map[string]any{
			"version": version.ID.String(),
		})
		writeJSON(w, http.StatusAccepted, snapshotVersionToDTO(updated))
	case string(SnapshotVersionCancelled):
		writeJSON(w, http.StatusAccepted, snapshotVersionToDTO(version))
	default:
		writeProblem(w, "conflict", "only a building SnapshotVersion can be cancelled")
	}
}

func (s *Server) takeSnapshotCancel(versionID uuid.UUID) (context.CancelFunc, bool) {
	s.snapshotMu.Lock()
	defer s.snapshotMu.Unlock()
	cancel, ok := s.snapshotCancels[versionID]
	if ok {
		delete(s.snapshotCancels, versionID)
	}
	return cancel, ok
}

// resolveSnapshotPin resolves a create request's `snapshot` (a
// SnapshotVersion id or a Snapshot name, own first and then platform) to the
// digest to pin. A missing or unusable Snapshot returns a problem code.
func resolveSnapshotPin(ctx context.Context, q *store.Queries, cfg Config, owner uuid.UUID, raw *string) (string, string, string) {
	ref := cfg.DefaultSnapshot
	platformOnly := true
	if raw != nil && *raw != "" {
		ref = *raw
		platformOnly = false
	}
	if ref == "" {
		return "", "internal", "no default Snapshot is configured; set STARS_DEFAULT_SNAPSHOT or pass snapshot"
	}
	if id, err := uuid.Parse(ref); err == nil && !platformOnly {
		version, err := q.GetSnapshotVersionByID(ctx, id)
		if err == nil {
			snap, err := q.GetSnapshot(ctx, version.SnapshotID)
			if err != nil || (snap.OwnerID != nil && *snap.OwnerID != owner) {
				return "", "not_found", "no such SnapshotVersion"
			}
			return pinVersion(cfg, snap, version)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return "", "internal", "resolve SnapshotVersion"
		}
	}
	if platformOnly {
		snap, err := q.GetPlatformSnapshotByName(ctx, ref)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", "not_found", "no such platform Snapshot"
		}
		if err != nil {
			return "", "internal", "resolve Snapshot"
		}
		return pinSnapshotCurrent(ctx, q, cfg, snap)
	}
	snap, err := q.GetSnapshotByName(ctx, store.GetSnapshotByNameParams{OwnerID: &owner, Name: ref})
	if errors.Is(err, pgx.ErrNoRows) {
		snap, err = q.GetPlatformSnapshotByName(ctx, ref)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "not_found", "no such Snapshot"
	}
	if err != nil {
		return "", "internal", "resolve Snapshot"
	}
	return pinSnapshotCurrent(ctx, q, cfg, snap)
}

// pinSnapshotCurrent pins the Snapshot's current version.
func pinSnapshotCurrent(ctx context.Context, q *store.Queries, cfg Config, snap store.Snapshot) (string, string, string) {
	if snap.CurrentVersionID == nil {
		return "", "conflict", "the Snapshot has no ready version"
	}
	version, err := q.GetSnapshotVersion(ctx, store.GetSnapshotVersionParams{
		ID: *snap.CurrentVersionID, SnapshotID: snap.ID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "conflict", "the Snapshot has no ready version"
	}
	if err != nil {
		return "", "internal", "resolve Snapshot current version"
	}
	return pinVersion(cfg, snap, version)
}

func pinVersion(cfg Config, snap store.Snapshot, version store.SnapshotVersion) (string, string, string) {
	if version.State != string(SnapshotVersionReady) || version.Digest == nil {
		return "", "conflict", "the SnapshotVersion is not ready"
	}
	if cfg.RegistryAddr == "" {
		return "", "internal", "the platform registry is not configured; set STARS_REGISTRY_ADDR"
	}
	return registry.Ref(cfg.RegistryAddr, registry.Repo(snap.ID, platformName(snap)), *version.Digest), "", ""
}
