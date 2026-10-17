//go:build e2e

package e2e

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestFilesAPI is the "files-api" acceptance scenario (#39 M1 item 15): content
// round-trips through the Tunnel, and escaping paths and symlinks are rejected.
func TestFilesAPI(t *testing.T) {
	admin := adminToken(t)
	raiseSandboxQuota(t, admin, 10)
	sandboxID := readyExecSandbox(t, admin, 128<<20)
	files := "/api/v1/sandboxes/" + sandboxID + "/files"

	put := func(path string, body []byte) int {
		t.Helper()
		status, _ := fileCall(t, admin, http.MethodPut, files+"/content?path="+url.QueryEscape(path), body)
		return status
	}
	get := func(path string, header map[string]string) (int, []byte) {
		t.Helper()
		return fileCallHeader(t, admin, http.MethodGet, files+"/content?path="+url.QueryEscape(path), nil, header)
	}

	// Upload and download agree byte for byte.
	content := []byte("hello workspace\n")
	if status := put("src/hello.txt", content); status != http.StatusNoContent {
		t.Fatalf("upload = %d, want 204", status)
	}
	status, got := get("src/hello.txt", nil)
	if status != http.StatusOK || !bytes.Equal(got, content) {
		t.Fatalf("download = %d %q", status, got)
	}
	status, got = get("src/hello.txt", map[string]string{"Range": "bytes=6-14"})
	if status != http.StatusPartialContent || string(got) != "workspace" {
		t.Fatalf("Range download = %d %q", status, got)
	}

	// List and stat report the file with its Workspace-relative path.
	status, data := call(t, http.MethodGet, files+"?path=src", admin, nil)
	var list struct {
		Items []struct {
			Path string `json:"path"`
			Type string `json:"type"`
			Size int64  `json:"size"`
		} `json:"items"`
	}
	mustJSON(t, data, &list)
	if status != http.StatusOK || len(list.Items) != 1 || list.Items[0].Path != "src/hello.txt" ||
		list.Items[0].Type != "file" || list.Items[0].Size != int64(len(content)) {
		t.Fatalf("list = %d %+v", status, list.Items)
	}
	status, data = call(t, http.MethodGet, files+"/stat?path=src/hello.txt", admin, nil)
	if status != http.StatusOK || !strings.Contains(string(data), `"type":"file"`) {
		t.Fatalf("stat = %d %s", status, data)
	}

	// mkdir, move and delete.
	status, _ = call(t, http.MethodPost, files+"/mkdir", admin, map[string]string{"path": "empty"})
	if status != http.StatusNoContent {
		t.Fatalf("mkdir = %d", status)
	}
	status, _ = call(t, http.MethodPost, files+"/move", admin, map[string]string{"from": "src/hello.txt", "to": "src/moved.txt"})
	if status != http.StatusNoContent {
		t.Fatalf("move = %d", status)
	}
	if status, _ := get("src/moved.txt", nil); status != http.StatusOK {
		t.Fatalf("moved file = %d", status)
	}
	status, _ = call(t, http.MethodDelete, files+"?path="+url.QueryEscape("src/moved.txt"), admin, nil)
	if status != http.StatusNoContent {
		t.Fatalf("delete = %d", status)
	}
	if status, _ := get("src/moved.txt", nil); status != http.StatusNotFound {
		t.Fatalf("deleted file = %d, want 404", status)
	}

	// Paths that leave /workspace are rejected, never rewritten.
	for _, p := range []string{"../escape.txt", "/etc/passwd", "src/../../escape.txt"} {
		if status := put(p, []byte("x")); status != http.StatusBadRequest {
			t.Fatalf("upload %q = %d, want 400", p, status)
		}
		if status, _ := get(p, nil); status != http.StatusBadRequest {
			t.Fatalf("download %q = %d, want 400", p, status)
		}
	}

	// A symlink inside the Workspace may point outside it, but following it is
	// refused: only in-tree access is allowed.
	e := createExec(t, admin, sandboxID, map[string]any{"argv": []string{"ln", "-s", "/etc", "escape"}})
	assertExitedWith(t, waitExecFinished(t, admin, e.ID, 10*time.Second), "exited")
	if status, _ := get("escape/passwd", nil); status != http.StatusBadRequest {
		t.Fatalf("escaping symlink = %d, want 400", status)
	}
	// The link itself is still listed and statable.
	status, data = call(t, http.MethodGet, files+"/stat?path=escape", admin, nil)
	if status != http.StatusOK || !strings.Contains(string(data), `"type":"symlink"`) {
		t.Fatalf("symlink stat = %d %s", status, data)
	}

	// Archive download and upload preserve the subtree.
	if status := put("tree/a.txt", []byte("alpha")); status != http.StatusNoContent {
		t.Fatalf("upload = %d", status)
	}
	status, raw := fileCall(t, admin, http.MethodGet, files+"/archive?path=tree", nil)
	if status != http.StatusOK {
		t.Fatalf("archive download = %d", status)
	}
	if names := tarNames(t, raw); len(names) < 2 {
		t.Fatalf("archive = %v", names)
	}
	status, _ = fileCall(t, admin, http.MethodPut, files+"/archive?path=unpacked", raw)
	if status != http.StatusNoContent {
		t.Fatalf("archive upload = %d", status)
	}
	if status, got := get("unpacked/tree/a.txt", nil); status != http.StatusOK || string(got) != "alpha" {
		t.Fatalf("unpacked = %d %q", status, got)
	}

	// A malicious archive is refused and writes nothing.
	var bad bytes.Buffer
	tw := tar.NewWriter(&bad)
	_ = tw.WriteHeader(&tar.Header{Name: "../outside.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1})
	_, _ = tw.Write([]byte("x"))
	_ = tw.Close()
	if status, _ := fileCall(t, admin, http.MethodPut, files+"/archive?path=evil", bad.Bytes()); status != http.StatusBadRequest {
		t.Fatalf("escaping archive = %d, want 400", status)
	}

	// The CLI uses the same API: upload, list and download.
	dir := t.TempDir()
	local := filepath.Join(dir, "cli.txt")
	if err := os.WriteFile(local, []byte("cli round trip"), 0o644); err != nil {
		t.Fatal(err)
	}
	cli := newCLIRunner(t, admin)
	cli.run(t, "", "files", "cp", local, sandboxID+":cli/cli.txt")
	if out := cli.run(t, "", "files", "ls", sandboxID, "cli"); !strings.Contains(string(out), "cli.txt") {
		t.Fatalf("files ls = %s", out)
	}
	downloaded := filepath.Join(dir, "back.txt")
	cli.run(t, "", "files", "cp", sandboxID+":cli/cli.txt", downloaded)
	back, err := os.ReadFile(downloaded)
	if err != nil || string(back) != "cli round trip" {
		t.Fatalf("cli download = %q %v", back, err)
	}

	// Preconditions: a Sandbox that is not ready answers 409.
	if status, data := call(t, http.MethodPost, "/api/v1/sandboxes/"+sandboxID+"/stop", admin, nil); status != http.StatusAccepted {
		t.Fatalf("stop = %d %s", status, data)
	}
	call(t, http.MethodGet, "/api/v1/sandboxes/"+sandboxID+"?wait_for=stopped", admin, nil)
	if status := put("stopped.txt", []byte("x")); status != http.StatusConflict {
		t.Fatalf("upload to stopped Sandbox = %d, want 409", status)
	}
}

func fileCall(t *testing.T, token, method, path string, body []byte) (int, []byte) {
	t.Helper()
	return fileCallHeader(t, token, method, path, body, nil)
}

func fileCallHeader(t *testing.T, token, method, path string, body []byte, header map[string]string) (int, []byte) {
	t.Helper()
	var payload io.Reader
	if body != nil {
		payload = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, apiURL()+path, payload)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, data
}

func tarNames(t *testing.T, raw []byte) []string {
	t.Helper()
	var names []string
	tr := tar.NewReader(bytes.NewReader(raw))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return names
		}
		if err != nil {
			t.Fatalf("read archive %q: %v", fmt.Sprint(raw[:min(len(raw), 200)]), err)
		}
		names = append(names, hdr.Name)
	}
}
