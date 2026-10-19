package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/zibyn/stars-sandbox/internal/manifest"
	"github.com/zibyn/stars-sandbox/internal/objectstore"
	"github.com/zibyn/stars-sandbox/internal/store"
)

// uploadTTL is how long an upload session resumes (defaults.md, 24 hours).
const uploadTTL = 24 * time.Hour

// maxManifestBytes bounds the manifest body. One million entries need well
// over the shared 1 MiB JSON limit (defaults.md).
const maxManifestBytes = 256 << 20

type resourceDTO struct {
	ID               string     `json:"id"`
	Name             string     `json:"name"`
	CurrentVersionID *string    `json:"current_version_id"`
	CreatedAt        time.Time  `json:"created_at"`
	DeletedAt        *time.Time `json:"deleted_at,omitempty"`
}

type resourceList struct {
	Items      []resourceDTO `json:"items"`
	NextCursor *string       `json:"next_cursor"`
}

type resourceVersionDTO struct {
	ID         string          `json:"id"`
	ResourceID string          `json:"resource_id"`
	Address    *string         `json:"address"`
	Size       int64           `json:"size"`
	State      string          `json:"state"`
	Cause      *string         `json:"cause"`
	Provenance json.RawMessage `json:"provenance"`
	CreatedAt  time.Time       `json:"created_at"`
	ExpiresAt  *time.Time      `json:"expires_at"`
}

type resourceVersionList struct {
	Items      []resourceVersionDTO `json:"items"`
	NextCursor *string              `json:"next_cursor"`
}

type uploadPut struct {
	SHA256 string `json:"sha256"`
	URL    string `json:"url"`
}

type uploadDTO struct {
	UploadID  string      `json:"upload_id"`
	ExpiresAt time.Time   `json:"expires_at"`
	Puts      []uploadPut `json:"puts"`
}

// badObject is one object that failed publication verification; it is the
// problem's `objects` extension (api.md §3).
type badObject struct {
	SHA256 string `json:"sha256"`
	Reason string `json:"reason"`
}

func resourceToDTO(resource store.Resource) resourceDTO {
	return resourceDTO{
		ID:               resource.ID.String(),
		Name:             resource.Name,
		CurrentVersionID: uuidPtr(resource.CurrentVersionID),
		CreatedAt:        resource.CreatedAt.Time.UTC(),
		DeletedAt:        timePtr(resource.DeletedAt),
	}
}

func resourceVersionToDTO(version store.ResourceVersion) resourceVersionDTO {
	return resourceVersionDTO{
		ID:         version.ID.String(),
		ResourceID: version.ResourceID.String(),
		Address:    version.Address,
		Size:       version.Size,
		State:      version.State,
		Cause:      version.Cause,
		Provenance: version.Provenance,
		CreatedAt:  version.CreatedAt.Time.UTC(),
		ExpiresAt:  timePtr(version.ExpiresAt),
	}
}

func (s *Server) handleCreateResource(w http.ResponseWriter, r *http.Request, p *principal) {
	var request struct {
		Name string `json:"name"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	if !validDisplayName(request.Name) {
		writeProblem(w, "invalid_request", "name must be non-empty, trimmed and at most 255 characters")
		return
	}
	key, ok := idempotencyKey(r)
	if !ok {
		writeProblem(w, "invalid_request", "Idempotency-Key is too long")
		return
	}
	ctx := r.Context()
	lookup := func() (store.Resource, error) {
		return s.q.GetResourceByIdempotencyKey(ctx, store.GetResourceByIdempotencyKeyParams{
			OwnerID: p.user.ID, IdempotencyKey: key,
		})
	}
	if replayOr(s, w, "look up idempotent Resource", key, lookup, func(r store.Resource) {
		writeJSON(w, http.StatusOK, resourceToDTO(r))
	}) {
		return
	}
	id, err := uuid.NewV7()
	if err != nil {
		s.internal(w, "generate Resource id", err)
		return
	}
	resource, replayed, err := createIdempotent(key, lookup,
		func() (store.Resource, error) {
			return s.q.CreateResource(ctx, store.CreateResourceParams{
				ID: id, OwnerID: p.user.ID, Name: request.Name, IdempotencyKey: key,
			})
		})
	if err != nil {
		if writeConstraintProblem(w, err) {
			return
		}
		s.internal(w, "create Resource", err)
		return
	}
	if replayed {
		writeJSON(w, http.StatusOK, resourceToDTO(resource))
		return
	}
	s.audit(ctx, p, "resource.create", "resource", id.String(), map[string]any{"name": resource.Name})
	writeJSON(w, http.StatusCreated, resourceToDTO(resource))
}

func (s *Server) handleListResources(w http.ResponseWriter, r *http.Request, p *principal) {
	rows, next, ok := readPage(s, w, r, "list Resources", parseCursor,
		func(ctx context.Context, limit int32, after *uuid.UUID) ([]store.Resource, error) {
			return s.q.ListResources(ctx, store.ListResourcesParams{
				OwnerID: p.user.ID, After: after, Limit: limit,
			})
		},
		func(resource store.Resource) string { return EncodeCursor(resource.ID) })
	if !ok {
		return
	}
	list := resourceList{Items: make([]resourceDTO, 0, len(rows)), NextCursor: next}
	for _, row := range rows {
		list.Items = append(list.Items, resourceToDTO(row))
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleGetResource(w http.ResponseWriter, r *http.Request, p *principal) {
	id, ok := pathUUID(r, "id")
	if !ok {
		writeProblem(w, "invalid_request", "id must be a UUID")
		return
	}
	resource, ok := s.getLiveResource(w, r.Context(), p, id)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, resourceToDTO(resource))
}

func (s *Server) handleDeleteResource(w http.ResponseWriter, r *http.Request, p *principal) {
	id, ok := pathUUID(r, "id")
	if !ok {
		writeProblem(w, "invalid_request", "id must be a UUID")
		return
	}
	ctx := r.Context()
	resource, ok := s.getOwnedResource(w, ctx, p, id)
	if !ok {
		return
	}
	if resource.DeletedAt.Valid {
		writeJSON(w, http.StatusAccepted, resourceToDTO(resource))
		return
	}
	resource, err := s.q.SoftDeleteResource(ctx, id)
	if err != nil {
		s.internal(w, "delete Resource", err)
		return
	}
	s.audit(ctx, p, "resource.delete", "resource", id.String(), map[string]any{"name": resource.Name})
	writeJSON(w, http.StatusAccepted, resourceToDTO(resource))
}

// getOwnedResource hides another User's Resource behind 404 (api.md §1). It
// includes tombstones; callers decide whether one is visible.
func (s *Server) getOwnedResource(w http.ResponseWriter, ctx context.Context, p *principal, id uuid.UUID) (store.Resource, bool) {
	return loadVisible(s, w, ctx, p, id, "Resource",
		func(ctx context.Context, id uuid.UUID) (store.Resource, uuid.UUID, error) {
			resource, err := s.q.GetResource(ctx, id)
			return resource, resource.OwnerID, err
		})
}

// getLiveResource is the read/upload path: a tombstoned Resource is gone.
func (s *Server) getLiveResource(w http.ResponseWriter, ctx context.Context, p *principal, id uuid.UUID) (store.Resource, bool) {
	resource, ok := s.getOwnedResource(w, ctx, p, id)
	if !ok {
		return store.Resource{}, false
	}
	if resource.DeletedAt.Valid {
		writeProblem(w, "not_found", "no such Resource")
		return store.Resource{}, false
	}
	return resource, true
}

// decodeManifestRequest reads and normalizes a manifest body. The shared
// decoder's 1 MiB cap is too small for the 1,000,000-entry limit.
func (s *Server) decodeManifestRequest(w http.ResponseWriter, r *http.Request) (manifest.Manifest, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxManifestBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var m manifest.Manifest
	if err := dec.Decode(&m); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeProblem(w, "payload_too_large", "manifest body is too large")
			return manifest.Manifest{}, false
		}
		writeProblem(w, "invalid_request", "invalid manifest: "+err.Error())
		return manifest.Manifest{}, false
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeProblem(w, "invalid_request", "body must contain exactly one JSON object")
		return manifest.Manifest{}, false
	}
	normalized, err := manifest.Normalize(m)
	if err != nil {
		s.writeManifestError(w, err)
		return manifest.Manifest{}, false
	}
	return normalized, true
}

func (s *Server) writeManifestError(w http.ResponseWriter, err error) {
	if errors.Is(err, manifest.ErrTooLarge) || errors.Is(err, manifest.ErrTooManyEntries) {
		writeProblem(w, "payload_too_large", err.Error())
		return
	}
	writeProblem(w, "invalid_request", err.Error())
}

func (s *Server) handleCreateUpload(w http.ResponseWriter, r *http.Request, p *principal) {
	id, ok := pathUUID(r, "id")
	if !ok {
		writeProblem(w, "invalid_request", "id must be a UUID")
		return
	}
	ctx := r.Context()
	resource, ok := s.getLiveResource(w, ctx, p, id)
	if !ok {
		return
	}
	if s.objects == nil {
		writeProblem(w, "unavailable", "object storage is not configured")
		return
	}
	m, ok := s.decodeManifestRequest(w, r)
	if !ok {
		return
	}
	address, err := manifest.Address(m)
	if err != nil {
		s.writeManifestError(w, err)
		return
	}
	canonical, err := manifest.Canonical(m)
	if err != nil {
		s.writeManifestError(w, err)
		return
	}
	upload, err := s.q.ReusableUpload(ctx, store.ReusableUploadParams{
		ResourceID: resource.ID, Address: address,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		uploadID, idErr := uuid.NewV7()
		if idErr != nil {
			s.internal(w, "generate upload id", idErr)
			return
		}
		upload, err = s.q.CreateUpload(ctx, store.CreateUploadParams{
			ID: uploadID, ResourceID: resource.ID, Address: address,
			Manifest: canonical, ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(uploadTTL), Valid: true},
		})
	}
	if err != nil {
		s.internal(w, "create upload session", err)
		return
	}

	response := uploadDTO{
		UploadID:  upload.ID.String(),
		ExpiresAt: upload.ExpiresAt.Time.UTC(),
		Puts:      []uploadPut{},
	}
	for _, object := range manifest.FileObjects(m) {
		stored, err := s.objects.HeadSHA256(ctx, resource.OwnerID.String(), object.SHA256)
		if err == nil && stored == object.SHA256 {
			continue
		}
		if err != nil && !errors.Is(err, objectstore.ErrNotFound) {
			s.internal(w, "check object existence", err)
			return
		}
		// The blob is gone. A cas_objects row that still names it is stale;
		// drop it so the next publish does not assume the object exists
		// (api.md §3, schema.md).
		if _, err := s.q.GetCASObject(ctx, store.GetCASObjectParams{
			OwnerID: resource.OwnerID, Sha256: object.SHA256,
		}); err == nil {
			if err := s.q.DeleteCASObject(ctx, store.DeleteCASObjectParams{
				OwnerID: resource.OwnerID, Sha256: object.SHA256,
			}); err != nil {
				s.internal(w, "delete stale CAS row", err)
				return
			}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			s.internal(w, "look up CAS object", err)
			return
		}
		url, err := s.objects.PresignPutURL(ctx, resource.OwnerID.String(), object.SHA256)
		if err != nil {
			s.internal(w, "presign upload", err)
			return
		}
		response.Puts = append(response.Puts, uploadPut{SHA256: object.SHA256, URL: url})
	}
	writeJSON(w, http.StatusCreated, response)
}

func (s *Server) handleCreateResourceVersion(w http.ResponseWriter, r *http.Request, p *principal) {
	id, ok := pathUUID(r, "id")
	if !ok {
		writeProblem(w, "invalid_request", "id must be a UUID")
		return
	}
	ctx := r.Context()
	resource, ok := s.getLiveResource(w, ctx, p, id)
	if !ok {
		return
	}
	if s.objects == nil {
		writeProblem(w, "unavailable", "object storage is not configured")
		return
	}
	var request struct {
		UploadID string `json:"upload_id"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	uploadID, err := uuid.Parse(request.UploadID)
	if err != nil {
		writeProblem(w, "invalid_request", "upload_id must be a UUID")
		return
	}
	upload, err := s.q.GetUpload(ctx, uploadID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && upload.ResourceID != resource.ID) {
		writeProblem(w, "not_found", "no such upload session")
		return
	}
	if err != nil {
		s.internal(w, "get upload session", err)
		return
	}
	if time.Now().After(upload.ExpiresAt.Time) {
		writeProblem(w, "conflict", "upload session has expired; submit the manifest again")
		return
	}
	var m manifest.Manifest
	if err := json.Unmarshal(upload.Manifest, &m); err != nil {
		s.internal(w, "decode stored manifest", err)
		return
	}
	m, err = manifest.Normalize(m)
	if err != nil {
		s.internal(w, "normalize stored manifest", err)
		return
	}
	address, err := manifest.Address(m)
	if err != nil {
		s.internal(w, "address stored manifest", err)
		return
	}
	if address != upload.Address {
		s.internal(w, "stored manifest does not match its address", nil)
		return
	}
	canonical, err := manifest.Canonical(m)
	if err != nil {
		s.internal(w, "encode stored manifest", err)
		return
	}

	// Verify every object with HeadObject in checksum mode. A version is
	// published only when all of them exist with the declared digest
	// (ADR-0012).
	bad := []badObject{}
	validated := make([]string, 0, len(manifest.FileObjects(m)))
	for _, object := range manifest.FileObjects(m) {
		validated = append(validated, object.SHA256)
		stored, err := s.objects.HeadSHA256(ctx, resource.OwnerID.String(), object.SHA256)
		switch {
		case errors.Is(err, objectstore.ErrNotFound):
			bad = append(bad, badObject{SHA256: object.SHA256, Reason: "missing"})
		case err != nil:
			s.internal(w, "verify object", err)
			return
		case stored != object.SHA256:
			bad = append(bad, badObject{SHA256: object.SHA256, Reason: "checksum_mismatch"})
		}
	}
	if len(bad) > 0 {
		writeProblemWithExtras(w, "conflict", "some objects are missing or do not match their declared sha256", map[string]any{
			"objects": bad,
		})
		return
	}

	key, ok := idempotencyKey(r)
	if !ok {
		writeProblem(w, "invalid_request", "Idempotency-Key is too long")
		return
	}
	lookup := func() (store.ResourceVersion, error) {
		return s.q.GetResourceVersionByIdempotencyKey(ctx, store.GetResourceVersionByIdempotencyKeyParams{
			ResourceID: resource.ID, IdempotencyKey: key,
		})
	}
	if replayOr(s, w, "look up idempotent ResourceVersion", key, lookup, func(v store.ResourceVersion) {
		writeJSON(w, http.StatusOK, resourceVersionToDTO(v))
	}) {
		return
	}

	manifestKey := ""
	if address != "" {
		manifestKey = objectstore.ManifestKey(resource.OwnerID.String(), address)
	}
	if err := s.objects.PutManifest(ctx, resource.OwnerID.String(), address, canonical); err != nil {
		s.internal(w, "store manifest", err)
		return
	}
	versionID, err := uuid.NewV7()
	if err != nil {
		s.internal(w, "generate ResourceVersion id", err)
		return
	}
	provenance := json.RawMessage(`{"source":"upload"}`)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		s.internal(w, "begin publish", err)
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	q := s.q.WithTx(tx)
	version, replayed, err := createIdempotent(key, lookup,
		func() (store.ResourceVersion, error) {
			return q.CreateResourceVersion(ctx, store.CreateResourceVersionParams{
				ID: versionID, ResourceID: resource.ID,
				Address: &address, ManifestKey: &manifestKey, Size: manifest.TotalSize(m),
				Provenance: provenance, IdempotencyKey: key,
			})
		})
	if err != nil {
		s.internal(w, "create ResourceVersion", err)
		return
	}
	if replayed {
		writeJSON(w, http.StatusOK, resourceVersionToDTO(version))
		return
	}
	for _, object := range manifest.FileObjects(m) {
		if err := q.InsertResourceVersionObject(ctx, store.InsertResourceVersionObjectParams{
			VersionID: version.ID, Sha256: object.SHA256,
		}); err != nil {
			s.internal(w, "link version object", err)
			return
		}
		if err := q.UpsertCASObject(ctx, store.UpsertCASObjectParams{
			OwnerID: resource.OwnerID, Sha256: object.SHA256, Size: object.Size,
		}); err != nil {
			s.internal(w, "record CAS object", err)
			return
		}
	}
	if _, err := q.SetResourceCurrent(ctx, store.SetResourceCurrentParams{
		ID: resource.ID, CurrentVersionID: &version.ID,
	}); err != nil {
		s.internal(w, "move current pointer", err)
		return
	}
	if err := q.DeleteUpload(ctx, upload.ID); err != nil {
		s.internal(w, "finish upload session", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		s.internal(w, "commit publish", err)
		return
	}
	// The publish just proved these objects exist, so versions marked
	// unavailable by an earlier 404 may now be usable again (api.md §3).
	s.recoverUnavailableVersions(ctx, resource.OwnerID, validated)
	s.audit(ctx, p, "resource.version.publish", "resource_version", version.ID.String(), map[string]any{
		"resource_id": resource.ID.String(), "address": address, "size": version.Size,
	})
	s.audit(ctx, p, "resource.current.move", "resource", resource.ID.String(), map[string]any{
		"version": version.ID.String(),
	})
	writeJSON(w, http.StatusCreated, resourceVersionToDTO(version))
}

// recoverUnavailableVersions re-checks the owner's unavailable versions that
// reference a digest the publish just validated. An unavailable version
// returns to ready only when every one of its objects is again present and
// matching; the current pointer is untouched (api.md §3). Recovery is a repair
// after the fact, so a failure here is logged rather than failing the publish.
func (s *Server) recoverUnavailableVersions(ctx context.Context, ownerID uuid.UUID, digests []string) {
	if s.objects == nil || len(digests) == 0 {
		return
	}
	versions, err := s.q.ListUnavailableVersionsReferencing(ctx, store.ListUnavailableVersionsReferencingParams{
		OwnerID: ownerID, Sha256: digests,
	})
	if err != nil {
		s.log.Warn("list unavailable ResourceVersions", "error", err)
		return
	}
	for _, versionID := range versions {
		objects, err := s.q.ListResourceVersionObjects(ctx, versionID)
		if err != nil {
			s.log.Warn("list unavailable ResourceVersion objects", "version_id", versionID, "error", err)
			continue
		}
		complete := true
		for _, digest := range objects {
			stored, err := s.objects.HeadSHA256(ctx, ownerID.String(), digest)
			if err != nil || stored != digest {
				complete = false
				break
			}
		}
		if !complete {
			continue
		}
		if _, err := s.q.MarkResourceVersionAvailable(ctx, versionID); err != nil {
			s.log.Warn("mark ResourceVersion available", "version_id", versionID, "error", err)
		}
	}
}

func (s *Server) handleListResourceVersions(w http.ResponseWriter, r *http.Request, p *principal) {
	id, ok := pathUUID(r, "id")
	if !ok {
		writeProblem(w, "invalid_request", "id must be a UUID")
		return
	}
	ctx := r.Context()
	resource, ok := s.getLiveResource(w, ctx, p, id)
	if !ok {
		return
	}
	rows, next, ok := readPage(s, w, r, "list ResourceVersions", parseCursor,
		func(ctx context.Context, limit int32, after *uuid.UUID) ([]store.ResourceVersion, error) {
			return s.q.ListResourceVersions(ctx, store.ListResourceVersionsParams{
				ResourceID: resource.ID, After: after, Limit: limit,
			})
		},
		func(version store.ResourceVersion) string { return EncodeCursor(version.ID) })
	if !ok {
		return
	}
	list := resourceVersionList{Items: make([]resourceVersionDTO, 0, len(rows)), NextCursor: next}
	for _, row := range rows {
		list.Items = append(list.Items, resourceVersionToDTO(row))
	}
	writeJSON(w, http.StatusOK, list)
}

// ownedVersion resolves {id}/{v} to a version of the caller's Resource.
func (s *Server) ownedVersion(w http.ResponseWriter, r *http.Request, p *principal) (store.Resource, store.ResourceVersion, bool) {
	ctx := r.Context()
	resourceID, ok := pathUUID(r, "id")
	if !ok {
		writeProblem(w, "invalid_request", "id must be a UUID")
		return store.Resource{}, store.ResourceVersion{}, false
	}
	resource, ok := s.getLiveResource(w, ctx, p, resourceID)
	if !ok {
		return store.Resource{}, store.ResourceVersion{}, false
	}
	versionID, ok := pathUUID(r, "v")
	if !ok {
		writeProblem(w, "invalid_request", "version must be a UUID")
		return store.Resource{}, store.ResourceVersion{}, false
	}
	version, err := s.q.GetResourceVersion(ctx, versionID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && version.ResourceID != resource.ID) {
		writeProblem(w, "not_found", "no such ResourceVersion")
		return store.Resource{}, store.ResourceVersion{}, false
	}
	if err != nil {
		s.internal(w, "get ResourceVersion", err)
		return store.Resource{}, store.ResourceVersion{}, false
	}
	return resource, version, true
}

func (s *Server) handleGetResourceVersion(w http.ResponseWriter, r *http.Request, p *principal) {
	_, version, ok := s.ownedVersion(w, r, p)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, resourceVersionToDTO(version))
}

func (s *Server) handleGetResourceManifest(w http.ResponseWriter, r *http.Request, p *principal) {
	resource, version, ok := s.ownedVersion(w, r, p)
	if !ok {
		return
	}
	if s.objects == nil {
		writeProblem(w, "unavailable", "object storage is not configured")
		return
	}
	if version.Address == nil {
		writeProblem(w, "conflict", "the version has no manifest")
		return
	}
	data, err := s.objects.GetManifest(r.Context(), resource.OwnerID.String(), *version.Address)
	if errors.Is(err, objectstore.ErrNotFound) {
		writeProblem(w, "conflict", "the manifest is missing from the object store")
		return
	}
	if err != nil {
		s.internal(w, "read manifest", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (s *Server) handleCreateResourceDownloads(w http.ResponseWriter, r *http.Request, p *principal) {
	resource, version, ok := s.ownedVersion(w, r, p)
	if !ok {
		return
	}
	if s.objects == nil {
		writeProblem(w, "unavailable", "object storage is not configured")
		return
	}
	var request struct {
		SHA256 []string `json:"sha256"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	digests, err := s.q.ListResourceVersionObjects(r.Context(), version.ID)
	if err != nil {
		s.internal(w, "list version objects", err)
		return
	}
	known := make(map[string]struct{}, len(digests))
	for _, digest := range digests {
		known[digest] = struct{}{}
	}
	urls := make([]string, 0, len(request.SHA256))
	for _, digest := range request.SHA256 {
		if _, ok := known[digest]; !ok {
			writeProblem(w, "invalid_request", fmt.Sprintf("%q is not part of this version", digest))
			return
		}
		url, err := s.objects.PresignGetURL(r.Context(), resource.OwnerID.String(), digest)
		if err != nil {
			s.internal(w, "presign download", err)
			return
		}
		urls = append(urls, url)
	}
	writeJSON(w, http.StatusOK, map[string]any{"urls": urls})
}

func (s *Server) handleSetResourceCurrent(w http.ResponseWriter, r *http.Request, p *principal) {
	id, ok := pathUUID(r, "id")
	if !ok {
		writeProblem(w, "invalid_request", "id must be a UUID")
		return
	}
	var request struct {
		Version string `json:"version"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	versionID, err := uuid.Parse(request.Version)
	if err != nil {
		writeProblem(w, "invalid_request", "version must be a UUID")
		return
	}
	ctx := r.Context()
	resource, ok := s.getLiveResource(w, ctx, p, id)
	if !ok {
		return
	}
	version, err := s.q.GetResourceVersion(ctx, versionID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && version.ResourceID != resource.ID) {
		writeProblem(w, "not_found", "no such ResourceVersion")
		return
	}
	if err != nil {
		s.internal(w, "get ResourceVersion", err)
		return
	}
	if ResourceVersionState(version.State) != ResourceVersionReady {
		writeProblem(w, "conflict", "only a ready ResourceVersion can become current")
		return
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		s.internal(w, "begin current move", err)
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	q := s.q.WithTx(tx)
	locked, err := q.LockResource(ctx, resource.ID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeProblem(w, "not_found", "no such Resource")
			return
		}
		s.internal(w, "lock Resource", err)
		return
	}
	if expected, present := ifMatch(r); present {
		current := ""
		if locked.CurrentVersionID != nil {
			current = locked.CurrentVersionID.String()
		}
		if expected != "*" && expected != current {
			writeProblem(w, "precondition_failed", "If-Match does not match the current version")
			return
		}
	}
	updated, err := q.SetResourceCurrent(ctx, store.SetResourceCurrentParams{
		ID: resource.ID, CurrentVersionID: &versionID,
	})
	if err != nil {
		s.internal(w, "move current pointer", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		s.internal(w, "commit current move", err)
		return
	}
	s.audit(ctx, p, "resource.current.move", "resource", resource.ID.String(), map[string]any{
		"version": versionID.String(),
	})
	writeJSON(w, http.StatusOK, resourceToDTO(updated))
}

// ifMatch reads the If-Match header, accepting an optionally quoted version id.
func ifMatch(r *http.Request) (string, bool) {
	value := strings.TrimSpace(r.Header.Get("If-Match"))
	if value == "" {
		return "", false
	}
	return strings.Trim(value, `"`), true
}
