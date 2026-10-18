package mcpserver_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const filePath = "/api/v1/sandboxes/sb/files/content"

func TestFileReadText(t *testing.T) {
	api := fakeAPI(t)
	api.handle("GET", filePath, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("path"); got != "hello.txt" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"code": "invalid_request", "detail": "path " + got})
			return
		}
		if got := r.Header.Get("Range"); got != "bytes=10-19" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"code": "invalid_request", "detail": "range " + got})
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "hello\n")
	})
	cs := connect(t, clientAPI(api))
	result := callTool(t, cs, "file_read", map[string]any{
		"sandbox_id": "sb", "path": "hello.txt", "offset": 10, "limit": 10,
	})
	body := structured(t, result)
	if body["encoding"] != "text" || body["content"] != "hello\n" {
		t.Fatalf("body = %#v", body)
	}
	if body["offset"] != float64(10) || body["size"] != float64(6) {
		t.Fatalf("body = %#v", body)
	}
}

func TestFileReadBinaryAndImage(t *testing.T) {
	api := fakeAPI(t)
	binary := []byte{0xff, 0xfe, 0x00, 0x01}
	api.handle("GET", filePath, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("path") == "pic.png" {
			w.Header().Set("Content-Type", "image/png")
		} else {
			w.Header().Set("Content-Type", "application/octet-stream")
		}
		_, _ = w.Write(binary)
	})
	cs := connect(t, clientAPI(api))

	bin := structured(t, callTool(t, cs, "file_read", map[string]any{
		"sandbox_id": "sb", "path": "blob.bin",
	}))
	if bin["encoding"] != "base64" {
		t.Fatalf("binary encoding = %v", bin["encoding"])
	}
	decoded, err := base64.StdEncoding.DecodeString(bin["content_base64"].(string))
	if err != nil || !bytes.Equal(decoded, binary) {
		t.Fatalf("content_base64 = %q, want %x", bin["content_base64"], binary)
	}

	image := callTool(t, cs, "file_read", map[string]any{
		"sandbox_id": "sb", "path": "pic.png",
	})
	imgBody := structured(t, image)
	if imgBody["encoding"] != "image" {
		t.Fatalf("image encoding = %v", imgBody["encoding"])
	}
	var imageContent *sdk.ImageContent
	for _, c := range image.Content {
		if ic, ok := c.(*sdk.ImageContent); ok {
			imageContent = ic
		}
	}
	if imageContent == nil {
		t.Fatalf("no image content in %#v", image.Content)
	}
	if imageContent.MIMEType != "image/png" || !bytes.Equal(imageContent.Data, binary) {
		t.Fatalf("image content = %+v", imageContent)
	}
}

func TestFileWriteBase64(t *testing.T) {
	api := fakeAPI(t)
	want := []byte{0xff, 0x00, 0x10}
	api.handle("PUT", filePath, func(w http.ResponseWriter, r *http.Request) {
		got, _ := io.ReadAll(r.Body)
		if !bytes.Equal(got, want) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"code": "invalid_request", "detail": "body"})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	cs := connect(t, clientAPI(api))
	result := callTool(t, cs, "file_write", map[string]any{
		"sandbox_id": "sb", "path": "out.bin",
		"content": base64.StdEncoding.EncodeToString(want), "encoding": "base64",
	})
	body := structured(t, result)
	if body["size"] != float64(len(want)) || body["path"] != "out.bin" {
		t.Fatalf("body = %#v", body)
	}
}

func TestFileWriteRejectsMoreThanOneMiB(t *testing.T) {
	cs := connect(t, clientAPI(fakeAPI(t)))
	result := callTool(t, cs, "file_write", map[string]any{
		"sandbox_id": "sb", "path": "big", "content": string(bytes.Repeat([]byte("a"), 1<<20+1)),
	})
	if code := errorCode(t, result); code != "invalid_request" {
		t.Fatalf("code = %q, want invalid_request", code)
	}
}

func TestFileMkdirMoveDelete(t *testing.T) {
	api := fakeAPI(t)
	made := false
	api.handle("POST", "/api/v1/sandboxes/sb/files/mkdir", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		made = body["path"] == "src"
		w.WriteHeader(http.StatusNoContent)
	})
	api.handle("POST", "/api/v1/sandboxes/sb/files/move", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	api.handle("DELETE", "/api/v1/sandboxes/sb/files", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	cs := connect(t, clientAPI(api))
	for _, call := range []struct {
		name string
		args map[string]any
	}{
		{"file_mkdir", map[string]any{"sandbox_id": "sb", "path": "src"}},
		{"file_move", map[string]any{"sandbox_id": "sb", "from": "src", "to": "dst"}},
		{"file_delete", map[string]any{"sandbox_id": "sb", "path": "dst"}},
	} {
		body := structured(t, callTool(t, cs, call.name, call.args))
		if body["ok"] != true {
			t.Errorf("%s = %#v", call.name, body)
		}
	}
	if !made {
		t.Error("file_mkdir did not send the path")
	}
}
