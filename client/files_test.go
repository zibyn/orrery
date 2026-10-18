package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFileContentRoundTrip(t *testing.T) {
	var uploaded []byte
	var gotRange string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/api/v1/sandboxes/sb1/files/content":
			uploaded, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/sandboxes/sb1/files/content":
			gotRange = r.Header.Get("Range")
			_, _ = w.Write([]byte("download"))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "tok")
	if err := c.WriteFile(context.Background(), "sb1", "src/a.txt", bytes.NewReader([]byte("upload")), 6); err != nil {
		t.Fatal(err)
	}
	if string(uploaded) != "upload" {
		t.Fatalf("uploaded = %q", uploaded)
	}
	var dst bytes.Buffer
	if err := c.ReadFile(context.Background(), "sb1", "src/a.txt", &dst, "bytes=0-7"); err != nil {
		t.Fatal(err)
	}
	if dst.String() != "download" || gotRange != "bytes=0-7" {
		t.Fatalf("download = %q, Range = %q", dst.String(), gotRange)
	}
}

func TestListAndStatFiles(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/sandboxes/sb1/files":
			if got := r.URL.Query().Get("path"); got != "src" {
				t.Errorf("path = %q", got)
			}
			_ = json.NewEncoder(w).Encode(FileList{Items: []FileEntry{{Path: "src/a.txt", Type: "file", Size: 6}}})
		case "/api/v1/sandboxes/sb1/files/stat":
			_ = json.NewEncoder(w).Encode(FileEntry{Path: "src/a.txt", Type: "file", Size: 6})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "tok")
	list, err := c.ListFiles(context.Background(), "sb1", "src")
	if err != nil || len(list.Items) != 1 || list.Items[0].Path != "src/a.txt" {
		t.Fatalf("list = %+v, %v", list, err)
	}
	entry, err := c.StatFile(context.Background(), "sb1", "src/a.txt")
	if err != nil || entry.Type != "file" || entry.Size != 6 {
		t.Fatalf("stat = %+v, %v", entry, err)
	}
}

func TestFileMutationsAndArchive(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, fmt.Sprintf("%s %s", r.Method, r.URL.Path))
		switch r.URL.Path {
		case "/api/v1/sandboxes/sb1/files/archive":
			if r.Method == http.MethodGet {
				_, _ = w.Write([]byte("tar-bytes"))
				return
			}
			data, _ := io.ReadAll(r.Body)
			if string(data) != "tar-upload" {
				t.Errorf("archive upload = %q", data)
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	ctx := context.Background()
	c := New(srv.URL, "tok")
	if err := c.Mkdir(ctx, "sb1", "empty"); err != nil {
		t.Fatal(err)
	}
	if err := c.MoveFile(ctx, "sb1", "a", "b"); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteFile(ctx, "sb1", "b"); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := c.ReadArchive(ctx, "sb1", "tree", &archive); err != nil {
		t.Fatal(err)
	}
	if archive.String() != "tar-bytes" {
		t.Fatalf("archive = %q", archive.String())
	}
	if err := c.WriteArchive(ctx, "sb1", "unpacked", bytes.NewReader([]byte("tar-upload")), 10); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"POST /api/v1/sandboxes/sb1/files/mkdir",
		"POST /api/v1/sandboxes/sb1/files/move",
		"DELETE /api/v1/sandboxes/sb1/files",
		"GET /api/v1/sandboxes/sb1/files/archive",
		"PUT /api/v1/sandboxes/sb1/files/archive",
	}
	if len(calls) != len(want) {
		t.Fatalf("calls = %v", calls)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Fatalf("call %d = %q, want %q", i, calls[i], want[i])
		}
	}
}
