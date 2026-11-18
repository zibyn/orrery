package api

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/zibyn/orrery/internal/generation"
	"github.com/zibyn/orrery/internal/manifest"
	"github.com/zibyn/orrery/internal/store"
)

// volumeAPI drives the REST API against the test database as one machine
// User, with the object store faked.
type volumeAPI struct {
	t     *testing.T
	ctx   context.Context
	s     *Server
	h     http.Handler
	store *fakeGenerationStore
	user  uuid.UUID
	token string
}

func newVolumeAPI(t *testing.T) *volumeAPI {
	t.Helper()
	dsn := os.Getenv("ORRERY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ORRERY_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := Migrate(ctx, pool, dsn, ""); err != nil {
		t.Fatal(err)
	}
	s := New(Config{EncryptionKeys: testKeyring(t), DefaultSandboxQuota: 100}, pool, slog.New(slog.DiscardHandler))
	fake := newFakeGenerationStore()
	s.generations = fake
	a := &volumeAPI{t: t, ctx: ctx, s: s, h: s.Handler(), store: fake}
	a.user, a.token = a.newUser()
	return a
}

// newUser mints a machine User and its token.
func (a *volumeAPI) newUser() (uuid.UUID, string) {
	a.t.Helper()
	created, err := a.s.UserToken(a.ctx, UserTokenOptions{Name: "u-" + uuid.NewString()[:12], Scope: "user"})
	if err != nil {
		a.t.Fatal(err)
	}
	var id uuid.UUID
	if err := a.s.pool.QueryRow(a.ctx, `SELECT user_id FROM api_tokens WHERE id = $1`, uuid.MustParse(created.ID)).Scan(&id); err != nil {
		a.t.Fatal(err)
	}
	return id, created.Token
}

// resp is one recorded response.
type resp struct {
	code int
	body []byte
}

func (a *volumeAPI) doAs(token, method, path string, body any) resp {
	a.t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			a.t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, "/api/v1"+path, reader)
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	a.h.ServeHTTP(rec, req)
	return resp{rec.Code, rec.Body.Bytes()}
}

func (a *volumeAPI) do(method, path string, body any) resp {
	a.t.Helper()
	return a.doAs(a.token, method, path, body)
}

// want decodes a response that must have status code.
func want[T any](t *testing.T, r resp, status int) T {
	t.Helper()
	r.want(t, status)
	var out T
	if err := json.Unmarshal(r.body, &out); err != nil {
		t.Fatalf("decode %s: %v", r.body, err)
	}
	return out
}

func (r resp) want(t *testing.T, status int) {
	t.Helper()
	if r.code != status {
		t.Fatalf("status = %d, want %d: %s", r.code, status, r.body)
	}
}

func problemCode(body []byte) string {
	var p struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(body, &p)
	return p.Code
}

func (a *volumeAPI) createVolume(name string) volumeDTO {
	a.t.Helper()
	return want[volumeDTO](a.t, a.do(http.MethodPost, "/volumes", map[string]any{"name": name}), http.StatusCreated)
}

type mountReq struct {
	Volume string `json:"volume"`
	Path   string `json:"path"`
}

func (a *volumeAPI) createSandbox(mounts ...mountReq) resp {
	a.t.Helper()
	return a.do(http.MethodPost, "/sandboxes", map[string]any{
		"runtime": "local", "cpu": 1, "memory": 1 << 20, "volumes": mounts,
	})
}

// stop takes a Sandbox to stopped the way the Reconciler does.
func (a *volumeAPI) stop(id string) {
	a.t.Helper()
	a.do(http.MethodPost, "/sandboxes/"+id+"/stop", nil).want(a.t, http.StatusAccepted)
	if n, err := a.s.q.SetSandboxStopped(a.ctx, uuid.MustParse(id)); err != nil || n != 1 {
		a.t.Fatalf("SetSandboxStopped = %d, %v", n, err)
	}
}

func TestVolumeCRUD(t *testing.T) {
	a := newVolumeAPI(t)
	v := a.createVolume("data")
	if v.State != "ready" || v.HeldBy != nil || v.CommittedAt != nil || v.Size != 0 || v.Name != "data" {
		t.Fatalf("created Volume = %+v, want ready, unheld, never committed, empty", v)
	}
	a.do(http.MethodPost, "/volumes", map[string]any{"name": "data"}).want(t, http.StatusConflict)
	a.do(http.MethodPost, "/volumes", map[string]any{"name": " padded"}).want(t, http.StatusBadRequest)
	list := want[volumeList](t, a.do(http.MethodGet, "/volumes", nil), http.StatusOK)
	if len(list.Items) != 1 || list.Items[0].ID != v.ID {
		t.Fatalf("list = %+v, want just %s", list.Items, v.ID)
	}
	got := want[volumeDTO](t, a.do(http.MethodGet, "/volumes/"+v.ID, nil), http.StatusOK)
	if got.ID != v.ID {
		t.Fatalf("get = %+v", got)
	}

	_, other := a.newUser()
	for _, path := range []string{"/volumes/" + v.ID, "/volumes/" + v.ID + "/files"} {
		a.doAs(other, http.MethodGet, path, nil).want(t, http.StatusNotFound)
	}
	a.doAs(other, http.MethodDelete, "/volumes/"+v.ID, nil).want(t, http.StatusNotFound)

	deleted := want[volumeDTO](t, a.do(http.MethodDelete, "/volumes/"+v.ID, nil), http.StatusAccepted)
	if deleted.State != "deleted" || deleted.DeletedAt == nil {
		t.Fatalf("deleted Volume = %+v", deleted)
	}
	if list := want[volumeList](t, a.do(http.MethodGet, "/volumes", nil), http.StatusOK); len(list.Items) != 0 {
		t.Fatalf("list after delete = %+v, want empty", list.Items)
	}
	a.createVolume("data") // the name is free again
}

func TestSandboxVolumePathRules(t *testing.T) {
	a := newVolumeAPI(t)
	v := a.createVolume("v1")
	w := a.createVolume("v2")
	for _, path := range []string{
		"data", "", "/", "/data/", "/a/../data", "/a//b", "/./a",
		"/resources", "/resources/x", "/proc", "/proc/x", "/sys/x", "/dev", "/dev/shm", "/.orrery/x",
	} {
		t.Run(path, func(t *testing.T) {
			a.createSandbox(mountReq{v.ID, path}).want(t, http.StatusBadRequest)
		})
	}
	t.Run("nested mounts", func(t *testing.T) {
		a.createSandbox(mountReq{v.ID, "/data"}, mountReq{w.ID, "/data/sub"}).want(t, http.StatusBadRequest)
	})
	t.Run("one Volume twice", func(t *testing.T) {
		a.createSandbox(mountReq{v.ID, "/a"}, mountReq{v.ID, "/b"}).want(t, http.StatusBadRequest)
	})
	t.Run("more than 16", func(t *testing.T) {
		mounts := make([]mountReq, 17)
		for i := range mounts {
			mounts[i] = mountReq{a.createVolume("many-" + uuid.NewString()[:8]).ID, "/m/" + uuid.NewString()[:8]}
		}
		a.createSandbox(mounts...).want(t, http.StatusBadRequest)
	})
	t.Run("unknown Volume", func(t *testing.T) {
		a.createSandbox(mountReq{"no-such-volume", "/data"}).want(t, http.StatusNotFound)
	})
	t.Run("/workspace and siblings, by id and name", func(t *testing.T) {
		sb := want[sandboxDTO](t, a.createSandbox(mountReq{v.ID, "/workspace"}, mountReq{"v2", "/workspaces"}), http.StatusCreated)
		sort.Slice(sb.Volumes, func(i, j int) bool { return sb.Volumes[i].Path < sb.Volumes[j].Path })
		if len(sb.Volumes) != 2 || sb.Volumes[0] != (sandboxVolumeDTO{v.ID, "/workspace"}) || sb.Volumes[1] != (sandboxVolumeDTO{w.ID, "/workspaces"}) {
			t.Fatalf("volumes = %+v", sb.Volumes)
		}
	})
}

func TestVolumeHeldByOneSandbox(t *testing.T) {
	a := newVolumeAPI(t)
	v := a.createVolume("held")
	first := want[sandboxDTO](t, a.createSandbox(mountReq{v.ID, "/data"}), http.StatusCreated)
	if got := want[volumeDTO](t, a.do(http.MethodGet, "/volumes/"+v.ID, nil), http.StatusOK); got.HeldBy == nil || *got.HeldBy != first.ID {
		t.Fatalf("held_by = %v, want %s", got.HeldBy, first.ID)
	}

	second := a.createSandbox(mountReq{v.ID, "/other"})
	second.want(t, http.StatusConflict)
	if code := problemCode(second.body); code != "conflict" {
		t.Fatalf("second holder code = %q", code)
	}
	a.do(http.MethodDelete, "/volumes/"+v.ID, nil).want(t, http.StatusConflict)

	a.stop(first.ID)
	if got := want[volumeDTO](t, a.do(http.MethodGet, "/volumes/"+v.ID, nil), http.StatusOK); got.HeldBy != nil {
		t.Fatalf("held_by after stop = %v, want null", *got.HeldBy)
	}
	next := want[sandboxDTO](t, a.createSandbox(mountReq{v.ID, "/data"}), http.StatusCreated)

	// The stopped first Sandbox cannot take the Volume back.
	a.do(http.MethodPost, "/sandboxes/"+first.ID+"/start", nil).want(t, http.StatusConflict)
	// Deleting it from stopped does not either.
	a.do(http.MethodDelete, "/sandboxes/"+first.ID, nil).want(t, http.StatusAccepted)

	// A deleting holder keeps the Volume until the deletion finishes.
	a.do(http.MethodDelete, "/sandboxes/"+next.ID, nil).want(t, http.StatusAccepted)
	a.createSandbox(mountReq{v.ID, "/data"}).want(t, http.StatusConflict)
	if n, err := a.s.q.MarkSandboxDeleted(a.ctx, uuid.MustParse(next.ID)); err != nil || n != 1 {
		t.Fatalf("MarkSandboxDeleted = %d, %v", n, err)
	}
	a.do(http.MethodDelete, "/volumes/"+v.ID, nil).want(t, http.StatusAccepted)

	// A start whose Volume was deleted meanwhile is refused.
	w := a.createVolume("gone")
	sb := want[sandboxDTO](t, a.createSandbox(mountReq{w.ID, "/data"}), http.StatusCreated)
	a.stop(sb.ID)
	a.do(http.MethodDelete, "/volumes/"+w.ID, nil).want(t, http.StatusAccepted)
	a.do(http.MethodPost, "/sandboxes/"+sb.ID+"/start", nil).want(t, http.StatusConflict)
	a.createSandbox(mountReq{w.ID, "/data"}).want(t, http.StatusNotFound)
}

// commitVolume records m as the Volume's new head, as a Volume commit would.
func (a *volumeAPI) commitVolume(volumeID string, m generation.Manifest) uuid.UUID {
	a.t.Helper()
	data, err := generation.Encode(m)
	if err != nil {
		a.t.Fatal(err)
	}
	id := uuid.New()
	a.store.mu.Lock()
	a.store.generations[a.user.String()+"/"+id.String()] = data
	a.store.mu.Unlock()
	if _, err := a.s.pool.Exec(a.ctx, `INSERT INTO generations (id, owner_id, volume_id, seq, kind, manifest_key, size)
		VALUES ($1, $2, $3, (SELECT COALESCE(MAX(seq), 0) + 1 FROM generations WHERE volume_id = $3), 'tree', 'k', $4)`,
		id, a.user, uuid.MustParse(volumeID), generation.Size(m)); err != nil {
		a.t.Fatal(err)
	}
	if _, err := a.s.pool.Exec(a.ctx, `UPDATE volumes SET head_generation_id = $1 WHERE id = $2`, id, uuid.MustParse(volumeID)); err != nil {
		a.t.Fatal(err)
	}
	return id
}

// fixtureTree is a Volume with a directory of a file, an executable, a
// symlink and a hard link, an empty directory and a top-level file.
func (a *volumeAPI) fixtureTree() generation.Manifest {
	file := func(p, content string, mode uint32) generation.Entry {
		digest := a.store.putContent(a.user, []byte(content))
		return generation.Entry{Entry: manifest.Entry{Path: p, Type: manifest.TypeFile, Size: ptr(int64(len(content))), SHA256: digest}, Mode: mode, Mtime: 1e18}
	}
	dir := func(p string) generation.Entry {
		return generation.Entry{Entry: manifest.Entry{Path: p, Type: manifest.TypeDir}, Mode: 0o755}
	}
	return generation.Manifest{Kind: generation.KindTree, Entries: []generation.Entry{
		dir("data"),
		file("data/a.txt", "hello", 0o644),
		file("data/run.sh", "#!/bin/sh\n", 0o755),
		{Entry: manifest.Entry{Path: "data/link", Type: manifest.TypeSymlink, Target: "a.txt"}, Mode: 0o777},
		{Entry: manifest.Entry{Path: "data/hard", Type: generation.TypeHardlink, Target: "data/a.txt"}},
		dir("empty"),
		file("top", "x", 0o600),
	}}
}

func names(items []volumeFileEntry) []string {
	out := []string{}
	for _, e := range items {
		out = append(out, e.Type+":"+e.Path)
	}
	return out
}

func TestVolumeFilesFromHeadGeneration(t *testing.T) {
	a := newVolumeAPI(t)
	v := a.createVolume("files")
	if list := want[volumeFileList](t, a.do(http.MethodGet, "/volumes/"+v.ID+"/files", nil), http.StatusOK); len(list.Items) != 0 {
		t.Fatalf("never-committed Volume lists %v, want an empty directory", names(list.Items))
	}

	a.commitVolume(v.ID, a.fixtureTree())
	got := want[volumeDTO](t, a.do(http.MethodGet, "/volumes/"+v.ID, nil), http.StatusOK)
	if got.CommittedAt == nil || got.Size != int64(len("hello")+len("#!/bin/sh\n")+1) {
		t.Fatalf("committed Volume = %+v, want committed_at and the files' size", got)
	}
	files := "/volumes/" + v.ID + "/files"
	root := want[volumeFileList](t, a.do(http.MethodGet, files+"?path=/", nil), http.StatusOK)
	if got := strings.Join(names(root.Items), " "); got != "dir:data dir:empty file:top" {
		t.Fatalf("root = %s", got)
	}
	data := want[volumeFileList](t, a.do(http.MethodGet, files+"?path=data", nil), http.StatusOK)
	if got := strings.Join(names(data.Items), " "); got != "file:data/a.txt file:data/hard symlink:data/link file:data/run.sh" {
		t.Fatalf("data = %s", got)
	}
	stat := want[volumeFileEntry](t, a.do(http.MethodGet, files+"/stat?path=data/hard", nil), http.StatusOK)
	if stat.Size != 5 || stat.Mode != 0o644 || stat.Name != "hard" {
		t.Fatalf("stat of the hard link = %+v", stat)
	}
	link := want[volumeFileEntry](t, a.do(http.MethodGet, files+"/stat?path=data/link", nil), http.StatusOK)
	if link.LinkTarget == nil || *link.LinkTarget != "a.txt" {
		t.Fatalf("stat of the symlink = %+v", link)
	}
	for _, p := range []string{"data/a.txt", "data/hard"} {
		if r := a.do(http.MethodGet, files+"/content?path="+p, nil); r.code != http.StatusOK || string(r.body) != "hello" {
			t.Fatalf("content of %s = %d %q", p, r.code, r.body)
		}
	}
	a.do(http.MethodGet, files+"/content?path=data", nil).want(t, http.StatusBadRequest)
	a.do(http.MethodGet, files+"/stat?path=nope", nil).want(t, http.StatusNotFound)
	a.do(http.MethodGet, files+"?path=../x", nil).want(t, http.StatusBadRequest)
	a.do(http.MethodGet, files+"?path=top", nil).want(t, http.StatusBadRequest)

	archive := a.do(http.MethodGet, files+"/archive?path=data", nil)
	archive.want(t, http.StatusOK)
	tr := tar.NewReader(bytes.NewReader(archive.body))
	var members []string
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(tr)
		members = append(members, hdr.Name+"="+string(body)+hdr.Linkname)
	}
	if got := strings.Join(members, " "); got != "data/="+" data/a.txt=hello data/hard=hello data/link=a.txt data/run.sh=#!/bin/sh\n" {
		t.Fatalf("archive = %q", got)
	}
}

func TestVolumePublishWritesOnlyAManifest(t *testing.T) {
	a := newVolumeAPI(t)
	v := a.createVolume("pub")
	head := a.commitVolume(v.ID, a.fixtureTree())
	objectsBefore := len(a.store.objects)

	version := want[resourceVersionDTO](t, a.do(http.MethodPost, "/volumes/"+v.ID+"/publish",
		map[string]any{"path": "data", "resource": "out"}), http.StatusAccepted)
	if version.State != "pending" {
		t.Fatalf("publish = %+v, want pending", version)
	}
	var prov volumeProvenance
	if err := json.Unmarshal(version.Provenance, &prov); err != nil || prov.Volume.GenerationID == nil || *prov.Volume.GenerationID != head.String() || prov.Volume.Path != "data" {
		t.Fatalf("provenance = %s", version.Provenance)
	}
	missing := want[resourceVersionDTO](t, a.do(http.MethodPost, "/volumes/"+v.ID+"/publish",
		map[string]any{"path": "nope", "resource": "out"}), http.StatusAccepted)

	// The publish holds its Generation while pending, even past a newer head.
	a.commitVolume(v.ID, generation.Manifest{Kind: generation.KindTree})
	if garbage(t, a.s.q, head) {
		t.Fatal("a pending publish's Generation is garbage")
	}

	a.s.publishVolumes(a.ctx)
	got, err := a.s.q.GetResourceVersion(a.ctx, uuid.MustParse(version.ID))
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "ready" || got.Address == nil {
		t.Fatalf("published version = %s %v, want ready with an address", got.State, got.Address)
	}
	if len(a.store.objects) != objectsBefore {
		t.Fatalf("objects %d -> %d: publishing from a Generation uploaded objects", objectsBefore, len(a.store.objects))
	}
	raw := a.store.manifests[a.user.String()+"/"+*got.Address]
	var m manifest.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("stored manifest %q: %v", raw, err)
	}
	var entries []string
	for _, e := range m.Entries {
		entries = append(entries, e.Path+":"+e.Type+":"+e.Target+map[bool]string{true: ":x"}[e.Executable])
	}
	if got := strings.Join(entries, " "); got != "a.txt:file: hard:file: link:symlink:a.txt run.sh:file::x" {
		t.Fatalf("manifest = %s", got)
	}
	resource, err := a.s.q.GetResource(a.ctx, got.ResourceID)
	if err != nil || resource.CurrentVersionID == nil || *resource.CurrentVersionID != got.ID {
		t.Fatalf("current = %v, %v, want %s", resource.CurrentVersionID, err, got.ID)
	}
	if failed, err := a.s.q.GetResourceVersion(a.ctx, uuid.MustParse(missing.ID)); err != nil || failed.State != "failed" {
		t.Fatalf("publish of a missing path = %s, %v, want failed", failed.State, err)
	}
	if !garbage(t, a.s.q, head) {
		t.Fatal("a superseded Generation no publish reads is kept")
	}
}

func TestVolumeHeadRetention(t *testing.T) {
	a := newVolumeAPI(t)
	v := a.createVolume("kept")
	head := a.commitVolume(v.ID, a.fixtureTree())
	if garbage(t, a.s.q, head) {
		t.Fatal("a live Volume's head is garbage")
	}
	a.do(http.MethodDelete, "/volumes/"+v.ID, nil).want(t, http.StatusAccepted)
	if !garbage(t, a.s.q, head) {
		t.Fatal("a deleted Volume's head is kept")
	}
	if err := a.s.purgeGenerations(a.ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := a.s.q.GetGeneration(a.ctx, head); err == nil {
		t.Fatal("the purge kept a deleted Volume's head")
	}
}

func TestSandboxPublishRules(t *testing.T) {
	a := newVolumeAPI(t)
	v := a.createVolume("under")
	sb := want[sandboxDTO](t, a.createSandbox(mountReq{v.ID, "/workspace/data"}), http.StatusCreated)
	publish := "/sandboxes/" + sb.ID + "/publish"
	for _, p := range []string{"data", "/workspace/data/x", "/resources/model", "/../etc"} {
		a.do(http.MethodPost, publish, map[string]any{"path": p, "resource": "r"}).want(t, http.StatusBadRequest)
	}
	// Valid paths, relative or in the root filesystem; the Sandbox is not ready yet.
	for _, p := range []string{"src", "/etc/app", "/workspace/datax"} {
		a.do(http.MethodPost, publish, map[string]any{"path": p, "resource": "r"}).want(t, http.StatusConflict)
	}
}

// garbage reports whether the purge would collect Generation id.
func garbage(t *testing.T, q *store.Queries, id uuid.UUID) bool {
	t.Helper()
	rows, err := q.ListGarbageGenerations(context.Background(), 100000)
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range rows {
		if g.ID == id {
			return true
		}
	}
	return false
}

// The workspace field is gone without an alias period; the refusal names its
// replacement (ADR-0031).
func TestSandboxRemovedFieldNamesVolumes(t *testing.T) {
	a := newVolumeAPI(t)
	r := a.do(http.MethodPost, "/sandboxes", map[string]any{
		"runtime": "local", "cpu": 1, "memory": 1 << 20, "workspace": map[string]any{"id": uuid.NewString()},
	})
	r.want(t, http.StatusBadRequest)
	if code := problemCode(r.body); code != "invalid_request" || !strings.Contains(string(r.body), "volumes") {
		t.Fatalf("workspace field = %s %s, want invalid_request naming volumes", code, r.body)
	}
}

// A stop whose Volume commit failed leaves the writes on the Node: the
// failed Sandbox keeps the Volume until it is deleted, so nobody mounts the
// older head meanwhile (runtime.md §17).
func TestCommitFailedKeepsTheVolume(t *testing.T) {
	a := newVolumeAPI(t)
	v := a.createVolume("stranded")
	sb := want[sandboxDTO](t, a.createSandbox(mountReq{v.ID, "/data"}), http.StatusCreated)
	id := uuid.MustParse(sb.ID)
	cause := "commit_failed"
	if n, err := a.s.q.MarkSandboxFailed(a.ctx, store.MarkSandboxFailedParams{ID: id, Generation: a.generation(id), Cause: &cause}); err != nil || n != 1 {
		t.Fatalf("MarkSandboxFailed = %d, %v", n, err)
	}
	if got := want[volumeDTO](t, a.do(http.MethodGet, "/volumes/"+v.ID, nil), http.StatusOK); got.HeldBy == nil || *got.HeldBy != sb.ID {
		t.Fatalf("held_by after commit_failed = %v, want %s", got.HeldBy, sb.ID)
	}
	a.createSandbox(mountReq{v.ID, "/data"}).want(t, http.StatusConflict)
	a.do(http.MethodDelete, "/volumes/"+v.ID, nil).want(t, http.StatusConflict)

	a.do(http.MethodDelete, "/sandboxes/"+sb.ID, nil).want(t, http.StatusAccepted)
	if n, err := a.s.q.MarkSandboxDeleted(a.ctx, id); err != nil || n != 1 {
		t.Fatalf("MarkSandboxDeleted = %d, %v", n, err)
	}
	want[sandboxDTO](t, a.createSandbox(mountReq{v.ID, "/data"}), http.StatusCreated)

	// Any other failure releases the Volume.
	w := a.createVolume("released")
	other := want[sandboxDTO](t, a.createSandbox(mountReq{w.ID, "/data"}), http.StatusCreated)
	cause = "runtime_create_failed"
	if _, err := a.s.q.MarkSandboxFailed(a.ctx, store.MarkSandboxFailedParams{ID: uuid.MustParse(other.ID), Generation: a.generation(uuid.MustParse(other.ID)), Cause: &cause}); err != nil {
		t.Fatal(err)
	}
	if got := want[volumeDTO](t, a.do(http.MethodGet, "/volumes/"+w.ID, nil), http.StatusOK); got.HeldBy != nil {
		t.Fatalf("held_by after create failure = %v, want null", *got.HeldBy)
	}
}

func (a *volumeAPI) generation(id uuid.UUID) int64 {
	a.t.Helper()
	sb, err := a.s.q.GetSandbox(a.ctx, id)
	if err != nil {
		a.t.Fatal(err)
	}
	return sb.Generation
}

// files/content answers Range like the Sandbox file API (api.md §5), also
// across the blocks of a large file.
func TestVolumeFileContentRange(t *testing.T) {
	a := newVolumeAPI(t)
	v := a.createVolume("ranged")
	first := bytes.Repeat([]byte("a"), int(manifest.BlockSize))
	first[len(first)-1] = 'b'
	second := []byte("cde")
	size := int64(len(first) + len(second))
	a.commitVolume(v.ID, generation.Manifest{Kind: generation.KindTree, Entries: []generation.Entry{
		{Entry: manifest.Entry{Path: "big", Type: manifest.TypeFile, Size: &size, Blocks: []string{
			a.store.putContent(a.user, first), a.store.putContent(a.user, second),
		}}, Mode: 0o644},
	}})
	get := func(rng string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/volumes/"+v.ID+"/files/content?path=big", nil)
		req.Header.Set("Authorization", "Bearer "+a.token)
		if rng != "" {
			req.Header.Set("Range", rng)
		}
		rec := httptest.NewRecorder()
		a.h.ServeHTTP(rec, req)
		return rec
	}
	if rec := get("bytes=4194302-4194304"); rec.Code != http.StatusPartialContent || rec.Body.String() != "abc" ||
		rec.Header().Get("Content-Range") != "bytes 4194302-4194304/4194307" {
		t.Fatalf("range across blocks = %d %.20q %q", rec.Code, rec.Body.String(), rec.Header().Get("Content-Range"))
	}
	if rec := get("bytes=-2"); rec.Code != http.StatusPartialContent || rec.Body.String() != "de" {
		t.Fatalf("suffix range = %d %.20q", rec.Code, rec.Body.String())
	}
	if rec := get("bytes=9999999-"); rec.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("range past the end = %d", rec.Code)
	}
	if rec := get(""); rec.Code != http.StatusOK || int64(rec.Body.Len()) != size || rec.Header().Get("Accept-Ranges") != "bytes" {
		t.Fatalf("whole file = %d, %d bytes, Accept-Ranges %q", rec.Code, rec.Body.Len(), rec.Header().Get("Accept-Ranges"))
	}
}

// A Volume migrated from a Workspace whose directory is still on its Node
// says so instead of looking empty (runtime.md §16 upgrade migration).
func TestMigratedVolumePendingOnItsNode(t *testing.T) {
	a := newVolumeAPI(t)
	v := a.createVolume("workspace-0123abcd")
	if v.PendingMigration {
		t.Fatal("a new Volume is pending migration")
	}
	node := uuid.New()
	for _, sql := range []string{
		`INSERT INTO nodes (id, name, token_hash) VALUES ($1, $2::text, '')`,
		`INSERT INTO legacy_dirs (id, node_id, volume_id) VALUES ($2, $1, $2)`,
	} {
		if _, err := a.s.pool.Exec(a.ctx, sql, node, uuid.MustParse(v.ID)); err != nil {
			t.Fatal(err)
		}
	}
	if got := want[volumeDTO](t, a.do(http.MethodGet, "/volumes/"+v.ID, nil), http.StatusOK); !got.PendingMigration {
		t.Fatalf("Volume with its directory on a Node = %+v, want pending_migration", got)
	}
	if list := want[volumeList](t, a.do(http.MethodGet, "/volumes", nil), http.StatusOK); len(list.Items) != 1 || !list.Items[0].PendingMigration {
		t.Fatalf("list = %+v, want pending_migration", list.Items)
	}
}
