//go:build e2e

package e2e

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zibyn/orrery/client"
)

// TestFilesAPI is the "files-api" acceptance scenario (#39 M1 item 15): content
// round-trips through the Tunnel, and escaping paths and symlinks are rejected.
// It also runs the Go SDK parity half (#108) on the same Sandbox under sdk*
// paths, checking each client package result against REST.
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
	content := []byte("hello sandboxes\n")
	if status := put("src/hello.txt", content); status != http.StatusNoContent {
		t.Fatalf("upload = %d, want 204", status)
	}
	status, got := get("src/hello.txt", nil)
	if status != http.StatusOK || !bytes.Equal(got, content) {
		t.Fatalf("download = %d %q", status, got)
	}
	status, got = get("src/hello.txt", map[string]string{"Range": "bytes=6-14"})
	if status != http.StatusPartialContent || string(got) != "sandboxes" {
		t.Fatalf("Range download = %d %q", status, got)
	}

	// List and stat report the file with its path relative to /workspace.
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

	// Paths that escape the root filesystem, or lie on a Resource mount, are
	// rejected, never rewritten; other absolute paths are in the root (api.md §5).
	for _, p := range []string{"../escape.txt", "/../escape.txt", "/resources/x", "src/../../escape.txt"} {
		if status := put(p, []byte("x")); status != http.StatusBadRequest {
			t.Fatalf("upload %q = %d, want 400", p, status)
		}
		if status, _ := get(p, nil); status != http.StatusBadRequest {
			t.Fatalf("download %q = %d, want 400", p, status)
		}
	}

	// A symlink inside /workspace may point outside it, but following it is
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

	// SDK parity: upload through the SDK, read back through REST and the SDK.
	ctx := context.Background()
	c := sdkClient(t)
	if err := c.WriteFile(ctx, sandboxID, "sdksrc/hello.txt", bytes.NewReader(content), int64(len(content))); err != nil {
		t.Fatalf("sdk WriteFile: %v", err)
	}
	status, got = get("sdksrc/hello.txt", nil)
	if status != http.StatusOK || !bytes.Equal(got, content) {
		t.Fatalf("REST download = %d %q", status, got)
	}
	var sdkDownloaded bytes.Buffer
	if err := c.ReadFile(ctx, sandboxID, "sdksrc/hello.txt", &sdkDownloaded, ""); err != nil {
		t.Fatalf("sdk ReadFile: %v", err)
	}
	if !bytes.Equal(sdkDownloaded.Bytes(), content) {
		t.Fatalf("sdk download = %q", sdkDownloaded.Bytes())
	}
	var ranged bytes.Buffer
	if err := c.ReadFile(ctx, sandboxID, "sdksrc/hello.txt", &ranged, "bytes=6-14"); err != nil {
		t.Fatalf("sdk ranged ReadFile: %v", err)
	}
	if ranged.String() != "sandboxes" {
		t.Fatalf("sdk range = %q", ranged.String())
	}

	// List and stat through the SDK agree with REST.
	sdkList, err := c.ListFiles(ctx, sandboxID, "sdksrc")
	if err != nil {
		t.Fatalf("sdk ListFiles: %v", err)
	}
	status, data = call(t, http.MethodGet, files+"?path=sdksrc", admin, nil)
	mustJSON(t, data, &list)
	if status != http.StatusOK || len(sdkList.Items) != len(list.Items) {
		t.Fatalf("sdk list = %+v, REST list = %+v", sdkList, list)
	}
	for i, item := range sdkList.Items {
		if item.Path != list.Items[i].Path || item.Type != list.Items[i].Type || item.Size != list.Items[i].Size {
			t.Fatalf("entry %d: sdk %+v, REST %+v", i, item, list.Items[i])
		}
	}
	entry, err := c.StatFile(ctx, sandboxID, "sdksrc/hello.txt")
	if err != nil {
		t.Fatalf("sdk StatFile: %v", err)
	}
	status, data = call(t, http.MethodGet, files+"/stat?path=sdksrc/hello.txt", admin, nil)
	if status != http.StatusOK || entry.Type != "file" || entry.Size != int64(len(content)) ||
		!strings.Contains(string(data), `"type":"file"`) {
		t.Fatalf("sdk stat = %+v, REST = %s", entry, data)
	}

	// SDK mkdir, move and delete.
	if err := c.Mkdir(ctx, sandboxID, "sdkempty"); err != nil {
		t.Fatalf("sdk Mkdir: %v", err)
	}
	status, data = call(t, http.MethodGet, files+"/stat?path=sdkempty", admin, nil)
	if status != http.StatusOK || !strings.Contains(string(data), `"type":"dir"`) {
		t.Fatalf("REST stat of mkdir = %d %s", status, data)
	}
	if err := c.MoveFile(ctx, sandboxID, "sdksrc/hello.txt", "sdksrc/moved.txt"); err != nil {
		t.Fatalf("sdk MoveFile: %v", err)
	}
	if status, _ := get("sdksrc/moved.txt", nil); status != http.StatusOK {
		t.Fatalf("sdk moved file = %d", status)
	}
	if err := c.DeleteFile(ctx, sandboxID, "sdksrc/moved.txt"); err != nil {
		t.Fatalf("sdk DeleteFile: %v", err)
	}
	if status, _ := get("sdksrc/moved.txt", nil); status != http.StatusNotFound {
		t.Fatalf("sdk deleted file = %d, want 404", status)
	}

	// The SDK surfaces escaping paths as 400 invalid_request.
	var problem *client.Error
	for _, p := range []string{"../escape.txt", "/../escape.txt", "/resources/x", "src/../../escape.txt"} {
		err := c.WriteFile(ctx, sandboxID, p, strings.NewReader("x"), 1)
		if !errors.As(err, &problem) || problem.Status != http.StatusBadRequest || problem.Code != "invalid_request" {
			t.Fatalf("sdk write %q = %v, want 400 invalid_request", p, err)
		}
		err = c.ReadFile(ctx, sandboxID, p, &bytes.Buffer{}, "")
		if !errors.As(err, &problem) || problem.Status != http.StatusBadRequest {
			t.Fatalf("sdk read %q = %v, want 400", p, err)
		}
	}

	// An escaping symlink is listed but never followed by the SDK either.
	e = createExec(t, admin, sandboxID, map[string]any{"argv": []string{"ln", "-s", "/etc", "sdkescape"}})
	assertExitedWith(t, waitExecFinished(t, admin, e.ID, 10*time.Second), "exited")
	if err := c.ReadFile(ctx, sandboxID, "sdkescape/passwd", &bytes.Buffer{}, ""); !errors.As(err, &problem) ||
		problem.Status != http.StatusBadRequest {
		t.Fatalf("sdk escaping symlink = %v, want 400", err)
	}
	link, err := c.StatFile(ctx, sandboxID, "sdkescape")
	if err != nil || link.Type != "symlink" {
		t.Fatalf("sdk symlink stat = %+v, %v", link, err)
	}

	// SDK archive download and upload preserve the subtree.
	if err := c.WriteFile(ctx, sandboxID, "sdktree/a.txt", strings.NewReader("alpha"), 5); err != nil {
		t.Fatalf("sdk WriteFile: %v", err)
	}
	var archive bytes.Buffer
	if err := c.ReadArchive(ctx, sandboxID, "sdktree", &archive); err != nil {
		t.Fatalf("sdk ReadArchive: %v", err)
	}
	if names := tarNames(t, archive.Bytes()); len(names) < 2 {
		t.Fatalf("sdk archive = %v", names)
	}
	if err := c.WriteArchive(ctx, sandboxID, "sdkunpacked", bytes.NewReader(archive.Bytes()), int64(archive.Len())); err != nil {
		t.Fatalf("sdk WriteArchive: %v", err)
	}
	if status, got := get("sdkunpacked/sdktree/a.txt", nil); status != http.StatusOK || string(got) != "alpha" {
		t.Fatalf("sdk unpacked = %d %q", status, got)
	}

	// Preconditions: a Sandbox that is not ready answers 409, over REST and the SDK.
	if status, data := call(t, http.MethodPost, "/api/v1/sandboxes/"+sandboxID+"/stop", admin, nil); status != http.StatusAccepted {
		t.Fatalf("stop = %d %s", status, data)
	}
	call(t, http.MethodGet, "/api/v1/sandboxes/"+sandboxID+"?wait_for=stopped", admin, nil)
	if status := put("stopped.txt", []byte("x")); status != http.StatusConflict {
		t.Fatalf("upload to stopped Sandbox = %d, want 409", status)
	}
	if err := c.WriteFile(ctx, sandboxID, "stopped.txt", strings.NewReader("x"), 1); !errors.As(err, &problem) ||
		problem.Status != http.StatusConflict || problem.Code != "conflict" {
		t.Fatalf("sdk write to stopped Sandbox = %v, want 409 conflict", err)
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
