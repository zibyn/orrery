package api

import (
	"context"
	"errors"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/zibyn/orrery/internal/store"
)

// Volume states (schema.md volumes). A Volume needs no Node, so it has no
// provisioning or deleting.
const (
	VolumeReady   = "ready"
	VolumeDeleted = "deleted"
)

// maxSandboxVolumes is api.md §3's limit on `volumes`.
const maxSandboxVolumes = 16

type volumeDTO struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	State       string     `json:"state"`
	HeldBy      *string    `json:"held_by"`
	CommittedAt *time.Time `json:"committed_at"`
	Size        int64      `json:"size"`
	CreatedAt   time.Time  `json:"created_at"`
	DeletedAt   *time.Time `json:"deleted_at"`
}

type volumeList struct {
	Items      []volumeDTO `json:"items"`
	NextCursor *string     `json:"next_cursor"`
}

// sandboxVolumeDTO is one entry of a Sandbox's `volumes`, as requested (with
// the Volume resolved to its id) and as returned.
type sandboxVolumeDTO struct {
	Volume string `json:"volume"`
	Path   string `json:"path"`
}

func (s *Server) handleCreateVolume(w http.ResponseWriter, r *http.Request, p *principal) {
	key, ok := idempotencyKey(r)
	if !ok {
		writeProblem(w, "invalid_request", "Idempotency-Key is too long")
		return
	}
	ctx := r.Context()
	lookup := func() (store.Volume, error) {
		return s.q.GetVolumeByIdempotencyKey(ctx, store.GetVolumeByIdempotencyKeyParams{
			OwnerID: p.user.ID, IdempotencyKey: key,
		})
	}
	if replayOr(s, w, "look up idempotent Volume", key, lookup, func(v store.Volume) {
		writeJSON(w, http.StatusOK, s.volumeToDTO(ctx, v))
	}) {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if !validDisplayName(req.Name) {
		writeProblem(w, "invalid_request", "name must be non-empty, trimmed, and at most 255 characters")
		return
	}
	id, err := uuid.NewV7()
	if err != nil {
		s.internal(w, "generate Volume id", err)
		return
	}
	v, replayed, err := createIdempotent(key, lookup, func() (store.Volume, error) {
		return s.q.CreateVolume(ctx, store.CreateVolumeParams{ID: id, OwnerID: p.user.ID, Name: req.Name, IdempotencyKey: key})
	})
	if err != nil {
		if writeConstraintProblem(w, err) {
			return
		}
		s.internal(w, "create Volume", err)
		return
	}
	if replayed {
		writeJSON(w, http.StatusOK, s.volumeToDTO(ctx, v))
		return
	}
	s.audit(ctx, p, "volume.create", "volume", id.String(), map[string]any{"name": v.Name})
	writeJSON(w, http.StatusCreated, s.volumeToDTO(ctx, v))
}

func (s *Server) handleListVolumes(w http.ResponseWriter, r *http.Request, p *principal) {
	rows, next, ok := readPage(s, w, r, "list Volumes", parseCursor,
		func(ctx context.Context, limit int32, after *uuid.UUID) ([]store.Volume, error) {
			return s.q.ListVolumes(ctx, store.ListVolumesParams{OwnerID: p.user.ID, After: after, Limit: limit})
		},
		func(v store.Volume) string { return EncodeCursor(v.ID) })
	if !ok {
		return
	}
	list := volumeList{Items: make([]volumeDTO, 0, len(rows)), NextCursor: next}
	for _, row := range rows {
		list.Items = append(list.Items, s.volumeToDTO(r.Context(), row))
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleGetVolume(w http.ResponseWriter, r *http.Request, p *principal) {
	v, ok := s.volumeFromPath(w, r, p)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.volumeToDTO(r.Context(), v))
}

// handleDeleteVolume deletes at once, without a Node; the CAS sweep collects
// the objects once the purge drops the head (api.md §3).
func (s *Server) handleDeleteVolume(w http.ResponseWriter, r *http.Request, p *principal) {
	v, ok := s.volumeFromPath(w, r, p)
	if !ok {
		return
	}
	ctx := r.Context()
	if v.State == VolumeDeleted {
		writeJSON(w, http.StatusAccepted, s.volumeToDTO(ctx, v))
		return
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		s.internal(w, "begin transaction", err)
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	q := s.q.WithTx(tx)
	// The row lock waits out a create or start that is taking the Volume, so
	// the hold check below sees it.
	if _, err := q.LockVolume(ctx, v.ID); err != nil {
		s.internal(w, "lock Volume", err)
		return
	}
	deleted, err := q.MarkVolumeDeleted(ctx, v.ID)
	if err != nil {
		s.internal(w, "delete Volume", err)
		return
	}
	if deleted == 0 {
		if current, err := q.GetVolume(ctx, v.ID); err == nil && current.State == VolumeDeleted {
			writeJSON(w, http.StatusAccepted, s.volumeToDTO(ctx, current))
			return
		}
		writeProblem(w, "conflict", "a Sandbox holds the Volume")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		s.internal(w, "commit Volume deletion", err)
		return
	}
	s.audit(ctx, p, "volume.delete", "volume", v.ID.String(), nil)
	s.changed(ctx)
	if v, err = s.q.GetVolume(ctx, v.ID); err != nil {
		s.internal(w, "get Volume", err)
		return
	}
	writeJSON(w, http.StatusAccepted, s.volumeToDTO(ctx, v))
}

// volumeFromPath loads the {id} Volume, hiding another User's behind 404.
func (s *Server) volumeFromPath(w http.ResponseWriter, r *http.Request, p *principal) (store.Volume, bool) {
	id, ok := pathUUID(r, "id")
	if !ok {
		writeProblem(w, "invalid_request", "id must be a UUID")
		return store.Volume{}, false
	}
	return loadVisible(s, w, r.Context(), p, id, "Volume",
		func(ctx context.Context, id uuid.UUID) (store.Volume, uuid.UUID, error) {
			v, err := s.q.GetVolume(ctx, id)
			return v, v.OwnerID, err
		})
}

func (s *Server) volumeToDTO(ctx context.Context, v store.Volume) volumeDTO {
	dto := volumeDTO{
		ID: v.ID.String(), Name: v.Name, State: v.State,
		CreatedAt: v.CreatedAt.Time.UTC(), DeletedAt: timePtr(v.DeletedAt),
	}
	if holder, err := s.q.GetVolumeHolder(ctx, v.ID); err == nil {
		id := holder.String()
		dto.HeldBy = &id
	}
	if v.HeadGenerationID != nil {
		if g, err := s.q.GetGeneration(ctx, *v.HeadGenerationID); err == nil {
			committed := g.CreatedAt.Time.UTC()
			dto.CommittedAt, dto.Size = &committed, g.Size
		}
	}
	return dto
}

// mountPathProblem checks one `volumes[].path` (api.md §3). The path must
// already be in normal form: a rewritten path would mount somewhere the
// caller did not write.
func mountPathProblem(p string) string {
	if !strings.HasPrefix(p, "/") || path.Clean(p) != p || strings.ContainsRune(p, 0) {
		return "volumes[].path must be an absolute, normalized path"
	}
	if p == "/" {
		return "volumes[].path cannot be /"
	}
	for _, reserved := range []string{"/resources", "/.orrery", "/proc", "/sys", "/dev"} {
		if pathWithin(p, reserved) {
			return "volumes[].path cannot lie under " + reserved
		}
	}
	return ""
}

// pathWithin reports whether p is dir or lies under it; both are clean and
// absolute.
func pathWithin(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, dir+"/")
}

// resolveSandboxVolumes validates and resolves a create request's `volumes`
// inside its transaction. Each Volume is locked FOR SHARE so a concurrent
// delete waits for this create, then sees its hold.
func (s *Server) resolveSandboxVolumes(w http.ResponseWriter, ctx context.Context, q *store.Queries, p *principal, mounts []sandboxVolumeDTO) ([]store.CreateSandboxVolumeParams, bool) {
	if len(mounts) > maxSandboxVolumes {
		writeProblem(w, "invalid_request", "at most 16 volumes")
		return nil, false
	}
	for i, m := range mounts {
		if problem := mountPathProblem(m.Path); problem != "" {
			writeProblem(w, "invalid_request", problem)
			return nil, false
		}
		for _, other := range mounts[:i] {
			if pathWithin(m.Path, other.Path) || pathWithin(other.Path, m.Path) {
				writeProblem(w, "invalid_request", "volumes[].path "+m.Path+" overlaps "+other.Path)
				return nil, false
			}
		}
	}
	out := make([]store.CreateSandboxVolumeParams, 0, len(mounts))
	seen := map[uuid.UUID]bool{}
	for _, m := range mounts {
		var v store.Volume
		var err error
		if id, parseErr := uuid.Parse(m.Volume); parseErr == nil {
			v, err = q.LockLiveVolumeShare(ctx, id)
		} else {
			v, err = q.GetLiveVolumeByNameShare(ctx, store.GetLiveVolumeByNameShareParams{OwnerID: p.user.ID, Name: m.Volume})
		}
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && v.OwnerID != p.user.ID) {
			writeProblem(w, "not_found", "no such Volume: "+m.Volume)
			return nil, false
		}
		if err != nil {
			s.internal(w, "resolve Volume", err)
			return nil, false
		}
		if seen[v.ID] {
			writeProblem(w, "invalid_request", "a Volume can be mounted only once")
			return nil, false
		}
		seen[v.ID] = true
		out = append(out, store.CreateSandboxVolumeParams{VolumeID: v.ID, Path: m.Path})
	}
	return out, true
}

// sandboxVolumes is a Sandbox's `volumes` response field.
func (s *Server) sandboxVolumes(ctx context.Context, sandboxID uuid.UUID) []sandboxVolumeDTO {
	if s.q == nil { // a Server without a database, in DTO unit tests
		return []sandboxVolumeDTO{}
	}
	rows, err := s.q.ListSandboxVolumes(ctx, sandboxID)
	if err != nil {
		s.log.Warn("list Sandbox Volumes", "sandbox_id", sandboxID, "error", err)
	}
	out := make([]sandboxVolumeDTO, 0, len(rows))
	for _, row := range rows {
		out = append(out, sandboxVolumeDTO{Volume: row.VolumeID.String(), Path: row.Path})
	}
	return out
}

// lockStartVolumes refuses a start whose Volume was deleted while the
// Sandbox was stopped. The FOR SHARE locks make a concurrent delete wait.
func lockStartVolumes(ctx context.Context, q *store.Queries, sandboxID uuid.UUID) (string, error) {
	volumes, err := q.LockSandboxVolumes(ctx, sandboxID)
	if err != nil {
		return "", err
	}
	for _, v := range volumes {
		if v.State == VolumeDeleted {
			return "the Sandbox's Volume " + v.Name + " is deleted", nil
		}
	}
	return "", nil
}
