package api

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/zibyn/orrery/internal/generation"
	"github.com/zibyn/orrery/internal/manifest"
	"github.com/zibyn/orrery/internal/objectstore"
	"github.com/zibyn/orrery/internal/store"
)

// volumeFileEntry is §5's FileEntry, read from a Generation manifest.
type volumeFileEntry struct {
	Path       string    `json:"path"`
	Name       string    `json:"name"`
	Type       string    `json:"type"`
	Size       int64     `json:"size"`
	Mode       uint32    `json:"mode"`
	ModTime    time.Time `json:"mod_time"`
	LinkTarget *string   `json:"link_target,omitempty"`
}

type volumeFileList struct {
	Items []volumeFileEntry `json:"items"`
}

// volumeTree is a Volume's head Generation as a path index. A Volume never
// committed is an empty directory (api.md §3).
type volumeTree struct {
	owner   string
	entries map[string]generation.Entry
}

// errVolumePath marks a path that escapes the Volume.
var errVolumePath = errors.New("path must stay inside the Volume")

// volumeLocalPath is a request path relative to the Volume root; a leading
// "/" names the root too. Paths that escape are refused, never rewritten.
func volumeLocalPath(raw string) (string, error) {
	rel, ok := cleanLocalPath(strings.TrimPrefix(raw, "/"))
	if !ok {
		return "", errVolumePath
	}
	return rel, nil
}

// loadVolumeTree reads the {id} Volume's head manifest for a file request,
// writing the problem itself when it cannot.
func (s *Server) loadVolumeTree(w http.ResponseWriter, r *http.Request, p *principal) (volumeTree, string, bool) {
	v, ok := s.volumeFromPath(w, r, p)
	if !ok {
		return volumeTree{}, "", false
	}
	if v.State == VolumeDeleted {
		writeProblem(w, "conflict", "the Volume is deleted")
		return volumeTree{}, "", false
	}
	rel, err := volumeLocalPath(r.URL.Query().Get("path"))
	if err != nil {
		writeProblem(w, "invalid_request", err.Error())
		return volumeTree{}, "", false
	}
	tree, err := s.readVolumeTree(r.Context(), v.OwnerID, v.HeadGenerationID)
	if err != nil {
		if errors.Is(err, errNoObjectStore) {
			writeProblem(w, "unavailable", err.Error())
		} else {
			s.internal(w, "read Volume Generation", err)
		}
		return volumeTree{}, "", false
	}
	return tree, rel, true
}

var errNoObjectStore = errors.New("object storage is not configured")

func (s *Server) readVolumeTree(ctx context.Context, owner uuid.UUID, head *uuid.UUID) (volumeTree, error) {
	tree := volumeTree{owner: owner.String(), entries: map[string]generation.Entry{}}
	if head == nil {
		return tree, nil
	}
	if s.generations == nil {
		return tree, errNoObjectStore
	}
	data, err := s.generations.GetGeneration(ctx, tree.owner, head.String())
	if err != nil {
		return tree, err
	}
	m, err := generation.Decode(data)
	if err != nil {
		return tree, err
	}
	for _, e := range m.Entries {
		tree.entries[e.Path] = e
	}
	return tree, nil
}

// lookup returns rel's entry; the root is an implicit directory.
func (t volumeTree) lookup(rel string) (generation.Entry, bool) {
	if rel == "." {
		return generation.Entry{Entry: manifest.Entry{Path: ".", Type: manifest.TypeDir}, Mode: 0o755}, true
	}
	e, ok := t.entries[rel]
	return e, ok
}

// file resolves a hard link to the file entry holding its content.
func (t volumeTree) file(e generation.Entry) generation.Entry {
	if e.Type == generation.TypeHardlink {
		if target, ok := t.entries[e.Target]; ok {
			return target
		}
	}
	return e
}

func (t volumeTree) fileEntry(e generation.Entry) volumeFileEntry {
	content := t.file(e)
	out := volumeFileEntry{
		Path: e.Path, Name: path.Base(e.Path), Type: e.Type,
		Mode: content.Mode & 0o777, ModTime: time.Unix(0, content.Mtime).UTC(),
	}
	switch e.Type {
	case generation.TypeHardlink:
		out.Type = manifest.TypeFile
		fallthrough
	case manifest.TypeFile:
		if content.Size != nil {
			out.Size = *content.Size
		}
	case manifest.TypeSymlink:
		target := e.Target
		out.LinkTarget = &target
	}
	return out
}

// children lists dir's direct entries in path order.
func (t volumeTree) children(dir string) []volumeFileEntry {
	items := []volumeFileEntry{}
	for p, e := range t.entries {
		parent := path.Dir(p)
		if parent == dir {
			items = append(items, t.fileEntry(e))
		}
	}
	sortFileEntries(items)
	return items
}

func sortFileEntries(items []volumeFileEntry) {
	sort.Slice(items, func(i, j int) bool { return items[i].Path < items[j].Path })
}

func (s *Server) handleListVolumeFiles(w http.ResponseWriter, r *http.Request, p *principal) {
	tree, rel, ok := s.loadVolumeTree(w, r, p)
	if !ok {
		return
	}
	e, found := tree.lookup(rel)
	if !found {
		writeProblem(w, "not_found", "no such file or directory")
		return
	}
	if e.Type != manifest.TypeDir {
		writeProblem(w, "invalid_request", "path is not a directory")
		return
	}
	writeJSON(w, http.StatusOK, volumeFileList{Items: tree.children(rel)})
}

func (s *Server) handleStatVolumeFile(w http.ResponseWriter, r *http.Request, p *principal) {
	tree, rel, ok := s.loadVolumeTree(w, r, p)
	if !ok {
		return
	}
	e, found := tree.lookup(rel)
	if !found {
		writeProblem(w, "not_found", "no such file or directory")
		return
	}
	writeJSON(w, http.StatusOK, tree.fileEntry(e))
}

// handleVolumeFileContent streams a file's objects from the owner's CAS
// over the store's internal endpoint, without a Node (api.md §3).
func (s *Server) handleVolumeFileContent(w http.ResponseWriter, r *http.Request, p *principal) {
	tree, rel, ok := s.loadVolumeTree(w, r, p)
	if !ok {
		return
	}
	e, found := tree.lookup(rel)
	if !found {
		writeProblem(w, "not_found", "no such file or directory")
		return
	}
	if e.Type != manifest.TypeFile && e.Type != generation.TypeHardlink {
		writeProblem(w, "invalid_request", "path is not a regular file")
		return
	}
	content := tree.file(e)
	f := &objectFile{ctx: r.Context(), s: s, owner: tree.owner, digests: content.Blocks, block: -1}
	if len(f.digests) == 0 {
		f.digests = []string{content.SHA256}
	}
	if content.Size != nil {
		f.size = *content.Size
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	// ServeContent answers Range (api.md §5); an object that fails mid-body
	// just ends it short, as the headers are sent.
	http.ServeContent(w, r, "", time.Time{}, f)
	if f.err != nil {
		s.log.Warn("stream Volume file", "path", rel, "error", f.err)
	}
}

// objectFile is a file entry's content as an io.ReadSeeker: a read fetches
// and verifies the whole object it falls in (a block is at most
// manifest.BlockSize), so any range costs at most one extra block per end.
type objectFile struct {
	ctx       context.Context
	s         *Server
	owner     string
	digests   []string
	size, off int64
	block     int // index of buf in digests, -1 before the first fetch
	buf       []byte
	err       error
}

func (f *objectFile) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekCurrent:
		offset += f.off
	case io.SeekEnd:
		offset += f.size
	}
	if offset < 0 {
		return 0, errors.New("seek before the start")
	}
	f.off = offset
	return offset, nil
}

func (f *objectFile) Read(p []byte) (int, error) {
	if f.off >= f.size {
		return 0, io.EOF
	}
	i, start := 0, f.off // a file of one object, legacy ones included
	if len(f.digests) > 1 {
		i, start = int(f.off/manifest.BlockSize), f.off%manifest.BlockSize
	}
	if i != f.block {
		if i >= len(f.digests) {
			return 0, io.ErrUnexpectedEOF
		}
		buf, err := f.s.generations.GetObject(f.ctx, f.owner, f.digests[i])
		if err != nil {
			f.err = err
			return 0, err
		}
		f.buf, f.block = buf, i
	}
	if start >= int64(len(f.buf)) {
		return 0, io.ErrUnexpectedEOF
	}
	n := copy(p, f.buf[start:])
	f.off += int64(n)
	return n, nil
}

// copyObjects writes a file entry's content: its one object, or its blocks
// in order.
func (s *Server) copyObjects(ctx context.Context, w io.Writer, owner string, e generation.Entry) error {
	if e.Size != nil && *e.Size == 0 {
		return nil
	}
	digests := e.Blocks
	if len(digests) == 0 {
		digests = []string{e.SHA256}
	}
	for _, digest := range digests {
		data, err := s.generations.GetObject(ctx, owner, digest)
		if err != nil {
			return err
		}
		if _, err := w.Write(data); err != nil {
			return err
		}
	}
	return nil
}

// handleVolumeArchive streams rel's subtree as tar, with names relative to
// the Volume root like the Sandbox archive.
func (s *Server) handleVolumeArchive(w http.ResponseWriter, r *http.Request, p *principal) {
	tree, rel, ok := s.loadVolumeTree(w, r, p)
	if !ok {
		return
	}
	if _, found := tree.lookup(rel); !found {
		writeProblem(w, "not_found", "no such file or directory")
		return
	}
	var entries []volumeFileEntry
	for pth, e := range tree.entries {
		if rel == "." || pth == rel || strings.HasPrefix(pth, rel+"/") {
			entries = append(entries, tree.fileEntry(e))
		}
	}
	sortFileEntries(entries)
	w.Header().Set("Content-Type", "application/x-tar")
	tw := tar.NewWriter(w)
	for _, fe := range entries {
		hdr := &tar.Header{Name: fe.Path, Mode: int64(fe.Mode), ModTime: fe.ModTime, Format: tar.FormatPAX}
		switch fe.Type {
		case manifest.TypeDir:
			hdr.Typeflag, hdr.Name = tar.TypeDir, fe.Path+"/"
		case manifest.TypeSymlink:
			hdr.Typeflag, hdr.Linkname = tar.TypeSymlink, *fe.LinkTarget
		default:
			hdr.Typeflag, hdr.Size = tar.TypeReg, fe.Size
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return
		}
		if hdr.Typeflag == tar.TypeReg {
			if err := s.copyObjects(r.Context(), tw, tree.owner, tree.file(tree.entries[fe.Path])); err != nil {
				s.log.Warn("stream Volume archive", "path", fe.Path, "error", err)
				return // the stream just ends short
			}
		}
	}
	_ = tw.Close()
}

// volumeProvenance is `source: volume{id, path, generation_id}` (schema.md);
// a pending publish keeps its Generation from the purge.
type volumeProvenance struct {
	Source string `json:"source"`
	Volume struct {
		ID           string  `json:"id"`
		Path         string  `json:"path"`
		GenerationID *string `json:"generation_id"`
	} `json:"volume"`
}

// handlePublishVolume creates a pending ResourceVersion of a path of the
// Volume's head Generation (api.md §3). The Control Plane writes its
// manifest from the Generation; no data moves and no Node is involved.
func (s *Server) handlePublishVolume(w http.ResponseWriter, r *http.Request, p *principal) {
	key, ok := idempotencyKey(r)
	if !ok {
		writeProblem(w, "invalid_request", "Idempotency-Key is too long")
		return
	}
	v, ok := s.volumeFromPath(w, r, p)
	if !ok {
		return
	}
	ctx := r.Context()
	lookup := func() (store.ResourceVersion, error) {
		return s.q.GetResourceVersionByVolumePublishIdempotencyKey(ctx, store.GetResourceVersionByVolumePublishIdempotencyKeyParams{
			PublishVolumeID: &v.ID, PublishIdempotencyKey: key,
		})
	}
	if replayOr(s, w, "look up idempotent publish", key, lookup, func(version store.ResourceVersion) {
		writeJSON(w, http.StatusAccepted, resourceVersionToDTO(version))
	}) {
		return
	}
	if v.State == VolumeDeleted {
		writeProblem(w, "conflict", "the Volume is deleted")
		return
	}
	var request publishRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	rel, err := volumeLocalPath(request.Path)
	if err != nil {
		writeProblem(w, "invalid_request", err.Error())
		return
	}
	if !validDisplayName(request.Resource) {
		writeProblem(w, "invalid_request", "resource must be a non-empty, trimmed name of at most 255 characters")
		return
	}
	var prov volumeProvenance
	prov.Source, prov.Volume.ID, prov.Volume.Path = "volume", v.ID.String(), rel
	if v.HeadGenerationID != nil {
		head := v.HeadGenerationID.String()
		prov.Volume.GenerationID = &head
	}
	raw, err := json.Marshal(prov)
	if err != nil {
		s.internal(w, "encode provenance", err)
		return
	}
	s.createPendingPublish(w, ctx, v.OwnerID, request.Resource, key, lookup, raw,
		store.CreatePendingResourceVersionParams{PublishVolumeID: &v.ID})
}

type publishRequest struct {
	Path     string `json:"path"`
	Resource string `json:"resource"`
}

// createPendingPublish resolves (or creates) the owner's Resource and inserts
// the pending version; params carries the source column.
func (s *Server) createPendingPublish(w http.ResponseWriter, ctx context.Context, owner uuid.UUID, resourceName string, key *string,
	lookup func() (store.ResourceVersion, error), provenance json.RawMessage, params store.CreatePendingResourceVersionParams) {
	resource, err := s.ensureOwnedResource(ctx, owner, resourceName)
	if err != nil {
		s.internal(w, "resolve Resource", err)
		return
	}
	versionID, err := uuid.NewV7()
	if err != nil {
		s.internal(w, "generate ResourceVersion id", err)
		return
	}
	params.ID, params.ResourceID, params.Provenance, params.PublishIdempotencyKey = versionID, resource.ID, provenance, key
	version, _, err := createIdempotent(key, lookup, func() (store.ResourceVersion, error) {
		return s.q.CreatePendingResourceVersion(ctx, params)
	})
	if err != nil {
		s.internal(w, "create pending ResourceVersion", err)
		return
	}
	s.changed(ctx)
	writeJSON(w, http.StatusAccepted, resourceVersionToDTO(version))
}

// publishVolumes finishes pending Volume publishes: each becomes ready with
// a manifest taken from its Generation, or failed when the path is missing
// or the manifest cannot be built.
func (s *Server) publishVolumes(ctx context.Context) {
	if s.generations == nil {
		return
	}
	rows, err := s.q.ListPendingVolumePublishes(ctx, gcBatch)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Warn("list pending Volume publishes", "error", err)
		}
		return
	}
	for _, row := range rows {
		m, err := s.volumePublishManifest(ctx, row)
		if errors.Is(err, manifest.ErrInvalid) {
			s.log.Warn("Volume publish failed", "version_id", row.ID, "error", err)
			s.markPublishFailed(ctx, row.ID)
			continue
		}
		if err != nil {
			s.log.Warn("Volume publish", "version_id", row.ID, "error", err)
			continue // retried on the next pass
		}
		s.finishPublish(ctx, row.ID, row.ResourceID, row.OwnerID, m, true)
	}
}

// volumePublishManifest is the Resource manifest of the provenance path in
// its Generation. Mode, owner, mtime and xattrs are dropped: a Resource keeps
// only the executable bit (ADR-0012). A hard link becomes a copy of its file.
func (s *Server) volumePublishManifest(ctx context.Context, row store.ListPendingVolumePublishesRow) (manifest.Manifest, error) {
	var prov volumeProvenance
	if err := json.Unmarshal(row.Provenance, &prov); err != nil || prov.Source != "volume" {
		return manifest.Manifest{}, fmt.Errorf("%w: not a Volume provenance", manifest.ErrInvalid)
	}
	var head *uuid.UUID
	if prov.Volume.GenerationID != nil {
		id, err := uuid.Parse(*prov.Volume.GenerationID)
		if err != nil {
			return manifest.Manifest{}, fmt.Errorf("%w: generation_id", manifest.ErrInvalid)
		}
		head = &id
	}
	tree, err := s.readVolumeTree(ctx, row.OwnerID, head)
	if errors.Is(err, objectstore.ErrNotFound) {
		return manifest.Manifest{}, fmt.Errorf("%w: the Generation is gone", manifest.ErrInvalid)
	}
	if err != nil {
		return manifest.Manifest{}, err
	}
	base := prov.Volume.Path
	if _, ok := tree.lookup(base); !ok {
		return manifest.Manifest{}, fmt.Errorf("%w: %q is not in the Volume", manifest.ErrInvalid, base)
	}
	var out manifest.Manifest
	for p, e := range tree.entries {
		var relative string
		switch {
		case base == ".":
			relative = p
		case p == base:
			if e.Type == manifest.TypeDir {
				continue // a directory's own entry is the root
			}
			relative = path.Base(p)
		case strings.HasPrefix(p, base+"/"):
			relative = strings.TrimPrefix(p, base+"/")
		default:
			continue
		}
		content := tree.file(e)
		entry := manifest.Entry{Path: relative, Type: e.Type, Target: e.Target}
		if e.Type == manifest.TypeFile || e.Type == generation.TypeHardlink {
			entry = manifest.Entry{
				Path: relative, Type: manifest.TypeFile, Size: content.Size, SHA256: content.SHA256,
				Blocks: content.Blocks, Executable: content.Mode&0o111 != 0,
			}
		}
		out.Entries = append(out.Entries, entry)
	}
	return manifest.Normalize(out)
}
