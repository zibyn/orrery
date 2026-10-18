//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zibyn/stars-sandbox/internal/manifest"
)

// TestResourceUploadChecksum is the "upload-checksum" acceptance scenario
// (#85): push and pull preserve bytes, the executable bit and symlinks; an
// unchanged re-push issues zero PUTs; a manifest with a wrong object is
// rejected at publication; and an over-5-GiB entry is refused before upload.
func TestResourceUploadChecksum(t *testing.T) {
	admin := adminToken(t)
	cli := newCLIRunner(t, admin)
	name := fmt.Sprintf("resource-%d", time.Now().UnixNano())

	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "hello.txt"), []byte("hello stars\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(src, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(src, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "bin", "run.sh"), []byte("#!/bin/sh\necho run\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(src, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "nested", "data.bin"), []byte{0, 1, 2, 3, 255}, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("hello.txt", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}

	var version struct {
		ID         string  `json:"id"`
		ResourceID string  `json:"resource_id"`
		Address    *string `json:"address"`
		State      string  `json:"state"`
	}
	mustJSON(t, cli.run(t, "", "resource", "push", src, name), &version)
	if version.State != "ready" || version.Address == nil {
		t.Fatalf("published version = %+v, want a ready version with an address", version)
	}

	dst := filepath.Join(t.TempDir(), "pulled")
	cli.run(t, "", "resource", "pull", name+"@"+version.ID, dst)
	assertTreesEqual(t, src, dst)

	// The canonical manifest is addressable and a second upload of it needs
	// no PUTs: every object is already in the owner's CAS.
	status, manifestBytes := call(t, http.MethodGet,
		"/api/v1/resources/"+version.ResourceID+"/versions/"+version.ID+"/manifest", admin, nil)
	if status != http.StatusOK {
		t.Fatalf("GET manifest: %d %s", status, manifestBytes)
	}
	status, body := call(t, http.MethodPost,
		"/api/v1/resources/"+version.ResourceID+"/uploads", admin, json.RawMessage(manifestBytes))
	if status != http.StatusCreated {
		t.Fatalf("re-submit manifest: %d %s", status, body)
	}
	var resumed struct {
		UploadID string `json:"upload_id"`
		Puts     []struct {
			SHA256 string `json:"sha256"`
			URL    string `json:"url"`
		} `json:"puts"`
	}
	mustJSON(t, body, &resumed)
	if len(resumed.Puts) != 0 {
		t.Fatalf("re-submitting identical content issued %d PUTs, want 0", len(resumed.Puts))
	}

	// A manifest that claims an object which was never uploaded is refused at
	// publication, and the problem lists the bad object.
	var m manifest.Manifest
	mustJSON(t, manifestBytes, &m)
	const missing = "0000000000000000000000000000000000000000000000000000000000000000"
	for i := range m.Entries {
		if m.Entries[i].Type == manifest.TypeFile {
			m.Entries[i].SHA256 = missing
			break
		}
	}
	badBody, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	status, body = call(t, http.MethodPost,
		"/api/v1/resources/"+version.ResourceID+"/uploads", admin, json.RawMessage(badBody))
	if status != http.StatusCreated {
		t.Fatalf("open upload for the bad manifest: %d %s", status, body)
	}
	var badUpload struct {
		UploadID string `json:"upload_id"`
		Puts     []struct {
			SHA256 string `json:"sha256"`
		} `json:"puts"`
	}
	mustJSON(t, body, &badUpload)
	if len(badUpload.Puts) != 1 || badUpload.Puts[0].SHA256 != missing {
		t.Fatalf("bad manifest puts = %+v, want exactly %s", badUpload.Puts, missing)
	}
	status, body = call(t, http.MethodPost,
		"/api/v1/resources/"+version.ResourceID+"/versions", admin, map[string]string{
			"upload_id": badUpload.UploadID,
		})
	if status != http.StatusConflict {
		t.Fatalf("publish with a missing object: %d %s, want 409", status, body)
	}
	var problem struct {
		Code    string `json:"code"`
		Objects []struct {
			SHA256 string `json:"sha256"`
			Reason string `json:"reason"`
		} `json:"objects"`
	}
	mustJSON(t, body, &problem)
	if problem.Code != "conflict" {
		t.Fatalf("problem code = %q, want conflict", problem.Code)
	}
	if len(problem.Objects) != 1 || problem.Objects[0].SHA256 != missing || problem.Objects[0].Reason != "missing" {
		t.Fatalf("problem objects = %+v, want the missing digest listed", problem.Objects)
	}

	// A file entry over 5 GiB is refused before any PUT is signed.
	big := map[string]any{"entries": []map[string]any{{
		"path": "big.bin", "type": "file",
		"size":   int64(5<<30) + 1,
		"sha256": missing,
	}}}
	status, body = call(t, http.MethodPost,
		"/api/v1/resources/"+version.ResourceID+"/uploads", admin, big)
	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("over-5-GiB manifest: %d %s, want 413", status, body)
	}
	requireProblemCode(t, body, "payload_too_large")
}

// assertTreesEqual compares two directory trees by type, bytes, the
// executable bit and symlink targets. Timestamps and other permission bits
// are not content (ADR-0012).
func assertTreesEqual(t *testing.T, wantRoot, gotRoot string) {
	t.Helper()
	type record struct {
		kind       byte // d, f, l
		executable bool
		content    []byte
		target     string
	}
	read := func(root string) map[string]record {
		records := map[string]record{}
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if path == root {
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			info, err := os.Lstat(path)
			if err != nil {
				return err
			}
			name := filepath.ToSlash(rel)
			switch {
			case info.Mode()&fs.ModeSymlink != 0:
				target, err := os.Readlink(path)
				if err != nil {
					return err
				}
				records[name] = record{kind: 'l', target: target}
			case info.IsDir():
				records[name] = record{kind: 'd'}
			default:
				content, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				records[name] = record{
					kind: 'f', executable: info.Mode().Perm()&0o111 != 0, content: content,
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
		return records
	}
	want, got := read(wantRoot), read(gotRoot)
	for name, expected := range want {
		actual, ok := got[name]
		if !ok {
			t.Fatalf("%s is missing after pull", name)
		}
		if actual.kind != expected.kind {
			t.Fatalf("%s: kind = %c, want %c", name, actual.kind, expected.kind)
		}
		switch expected.kind {
		case 'f':
			if !bytes.Equal(actual.content, expected.content) {
				t.Fatalf("%s: content changed (%q vs %q)", name, actual.content, expected.content)
			}
			if actual.executable != expected.executable {
				t.Fatalf("%s: executable = %v, want %v", name, actual.executable, expected.executable)
			}
		case 'l':
			if actual.target != expected.target {
				t.Fatalf("%s: target = %q, want %q", name, actual.target, expected.target)
			}
		}
	}
	for name := range got {
		if _, ok := want[name]; !ok {
			t.Fatalf("%s appeared after pull", name)
		}
	}
}
