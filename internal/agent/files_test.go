package agent

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func newFilesEnv(t *testing.T, mode string) (*httptest.Server, uuid.UUID, string) {
	t.Helper()
	root := t.TempDir()
	ws := uuid.New()
	wsDir := workspacePath(root, ws)
	if err := os.MkdirAll(wsDir, 0o777); err != nil {
		t.Fatal(err)
	}
	sid := uuid.New()
	if err := os.MkdirAll(sandboxPath(root, sid), 0o755); err != nil {
		t.Fatal(err)
	}
	r := &Runtime{dataRoot: root}
	if err := r.writeWorkspaceRefFile(sid, workspaceRef{ID: ws.String(), Mode: mode}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(r.tunnelMux())
	t.Cleanup(srv.Close)
	return srv, sid, wsDir
}

func fileURL(base string, sid uuid.UUID, suffix, path string) string {
	u := base + "/sandboxes/" + sid.String() + "/files"
	if suffix != "" {
		u += "/" + suffix
	}
	if path != "" {
		u += "?path=" + url.QueryEscape(path)
	}
	return u
}

func doReq(t *testing.T, method, u string, body []byte, header map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, u, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestFileContentRoundTripAndRange(t *testing.T) {
	srv, sid, _ := newFilesEnv(t, "rw")
	u := fileURL(srv.URL, sid, "content", "dir/hello.txt")

	if resp := doReq(t, http.MethodPut, u, []byte("hello world"), nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT = %d, want 204", resp.StatusCode)
	}
	resp := doReq(t, http.MethodGet, u, nil, nil)
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(got) != "hello world" {
		t.Fatalf("GET = %d %q", resp.StatusCode, got)
	}
	resp = doReq(t, http.MethodGet, u, nil, map[string]string{"Range": "bytes=6-10"})
	got, _ = io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusPartialContent || string(got) != "world" {
		t.Fatalf("Range GET = %d %q", resp.StatusCode, got)
	}

	list := doReq(t, http.MethodGet, fileURL(srv.URL, sid, "", "dir"), nil, nil)
	var entries fileList
	if err := json.NewDecoder(list.Body).Decode(&entries); err != nil {
		t.Fatal(err)
	}
	if len(entries.Items) != 1 || entries.Items[0].Name != "hello.txt" || entries.Items[0].Type != "file" {
		t.Fatalf("list = %+v", entries.Items)
	}
	stat := doReq(t, http.MethodGet, fileURL(srv.URL, sid, "stat", "dir/hello.txt"), nil, nil)
	var entry fileEntry
	if err := json.NewDecoder(stat.Body).Decode(&entry); err != nil {
		t.Fatal(err)
	}
	if entry.Size != int64(len("hello world")) || entry.Path != "dir/hello.txt" {
		t.Fatalf("stat = %+v", entry)
	}
}

func TestFileMkdirMoveDelete(t *testing.T) {
	srv, sid, _ := newFilesEnv(t, "rw")
	base := srv.URL

	mkdir, _ := json.Marshal(pathRequest{Path: "a/b"})
	if resp := doReq(t, http.MethodPost, fileURL(base, sid, "mkdir", ""), mkdir, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("mkdir = %d", resp.StatusCode)
	}
	if resp := doReq(t, http.MethodPut, fileURL(base, sid, "content", "a/b/f.txt"), []byte("x"), nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT = %d", resp.StatusCode)
	}
	move, _ := json.Marshal(moveRequest{From: "a/b/f.txt", To: "a/f.txt"})
	if resp := doReq(t, http.MethodPost, fileURL(base, sid, "move", ""), move, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("move = %d", resp.StatusCode)
	}
	if resp := doReq(t, http.MethodGet, fileURL(base, sid, "content", "a/f.txt"), nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("moved file = %d", resp.StatusCode)
	}
	// A non-empty directory is a conflict, not a silent recursive wipe.
	if resp := doReq(t, http.MethodDelete, fileURL(base, sid, "", "a"), nil, nil); resp.StatusCode != http.StatusConflict {
		t.Fatalf("delete non-empty = %d, want 409", resp.StatusCode)
	}
	if resp := doReq(t, http.MethodDelete, fileURL(base, sid, "", "a/f.txt"), nil, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete = %d", resp.StatusCode)
	}
	if resp := doReq(t, http.MethodDelete, fileURL(base, sid, "", "a/b"), nil, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete dir = %d", resp.StatusCode)
	}
	// mkdir over an existing file is a type error, not success.
	if resp := doReq(t, http.MethodPut, fileURL(base, sid, "content", "plain"), []byte("x"), nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT = %d", resp.StatusCode)
	}
	mkdirFile, _ := json.Marshal(pathRequest{Path: "plain"})
	if resp := doReq(t, http.MethodPost, fileURL(base, sid, "mkdir", ""), mkdirFile, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("mkdir over file = %d, want 400", resp.StatusCode)
	}
}

func TestFilePathsStayInsideWorkspace(t *testing.T) {
	srv, sid, wsDir := newFilesEnv(t, "rw")
	base := srv.URL

	for _, p := range []string{"../escape", "/etc/passwd", "a/../../escape"} {
		u := fileURL(base, sid, "content", p)
		if resp := doReq(t, http.MethodPut, u, []byte("x"), nil); resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("PUT %q = %d, want 400", p, resp.StatusCode)
		}
	}

	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("s"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(wsDir, "link")); err != nil {
		t.Fatal(err)
	}
	resp := doReq(t, http.MethodGet, fileURL(base, sid, "content", "link/secret"), nil, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("escaping symlink = %d, want 400", resp.StatusCode)
	}
	// An absolute symlink target cannot be resolved inside the Workspace either.
	if err := os.Symlink("/etc", filepath.Join(wsDir, "abs")); err != nil {
		t.Fatal(err)
	}
	if resp := doReq(t, http.MethodGet, fileURL(base, sid, "content", "abs/passwd"), nil, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("absolute symlink = %d, want 400", resp.StatusCode)
	}
}

func TestFileWriteOnReadOnlyWorkspace(t *testing.T) {
	srv, sid, _ := newFilesEnv(t, "ro")
	if resp := doReq(t, http.MethodPut, fileURL(srv.URL, sid, "content", "x"), []byte("x"), nil); resp.StatusCode != http.StatusConflict {
		t.Fatalf("PUT on ro = %d, want 409", resp.StatusCode)
	}
	if resp := doReq(t, http.MethodGet, fileURL(srv.URL, sid, "", ""), nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("list on ro = %d, want 200", resp.StatusCode)
	}
}

func TestArchiveRoundTrip(t *testing.T) {
	srv, sid, _ := newFilesEnv(t, "rw")
	base := srv.URL
	if resp := doReq(t, http.MethodPut, fileURL(base, sid, "content", "src/a.txt"), []byte("alpha"), nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT = %d", resp.StatusCode)
	}
	if resp := doReq(t, http.MethodPut, fileURL(base, sid, "content", "src/sub/b.txt"), []byte("beta"), nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT = %d", resp.StatusCode)
	}
	archive := doReq(t, http.MethodGet, fileURL(base, sid, "archive", "src"), nil, nil)
	if archive.StatusCode != http.StatusOK {
		t.Fatalf("archive GET = %d", archive.StatusCode)
	}
	raw, _ := io.ReadAll(archive.Body)

	if resp := doReq(t, http.MethodPut, fileURL(base, sid, "archive", "dst"), raw, nil); resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("archive PUT = %d %s", resp.StatusCode, body)
	}
	// Archive entries keep their Workspace-relative paths, so unpacking into
	// dst restores src/ beneath it.
	for name, want := range map[string]string{"dst/src/a.txt": "alpha", "dst/src/sub/b.txt": "beta"} {
		resp := doReq(t, http.MethodGet, fileURL(base, sid, "content", name), nil, nil)
		got, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK || string(got) != want {
			t.Fatalf("%s = %d %q, want %q", name, resp.StatusCode, got, want)
		}
	}
}

func TestArchiveRejectsEscapingEntries(t *testing.T) {
	srv, sid, _ := newFilesEnv(t, "rw")
	cases := map[string]*tar.Header{
		"traversal name": {Name: "../escape.txt", Typeflag: tar.TypeReg, Mode: 0o644},
		// The write direction never creates links (research §9).
		"symlink":      {Name: "link", Typeflag: tar.TypeSymlink, Linkname: "../../etc/passwd", Mode: 0o777},
		"special file": {Name: "fifo", Typeflag: tar.TypeFifo, Mode: 0o644},
	}
	for name, hdr := range cases {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			tw := tar.NewWriter(&buf)
			if err := tw.WriteHeader(hdr); err != nil {
				t.Fatal(err)
			}
			if hdr.Typeflag == tar.TypeReg {
				_, _ = tw.Write([]byte("x"))
			}
			_ = tw.Close()
			resp := doReq(t, http.MethodPut, fileURL(srv.URL, sid, "archive", "out"), buf.Bytes(), nil)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("archive PUT = %d, want 400", resp.StatusCode)
			}
		})
	}
}

func TestFileListMissingWorkspaceRef(t *testing.T) {
	root := t.TempDir()
	r := &Runtime{dataRoot: root}
	srv := httptest.NewServer(r.tunnelMux())
	defer srv.Close()
	resp := doReq(t, http.MethodGet, fileURL(srv.URL, uuid.New(), "", ""), nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing ref = %d, want 404", resp.StatusCode)
	}
	if !strings.Contains(resp.Header.Get("Content-Type"), "problem+json") {
		t.Fatalf("content type = %q", resp.Header.Get("Content-Type"))
	}
}
