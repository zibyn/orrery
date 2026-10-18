package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zibyn/stars-sandbox/internal/supervisor"
)

func TestReadBuildMetadata(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "metadata.json")
	raw := `{
	  "containerimage.digest": "sha256:1111111111111111111111111111111111111111111111111111111111111111",
	  "buildx.build.provenance": {
	    "materials": [
	      {"uri": "pkg:docker/alpine@3.22", "digest": {"sha256": "2222222222222222222222222222222222222222222222222222222222222222"}},
	      {"uri": "pkg:docker/alpine@3.22", "digest": {"sha256": "2222222222222222222222222222222222222222222222222222222222222222"}},
	      {"uri": "pkg:docker/busybox@1", "digest": {"sha256": "3333333333333333333333333333333333333333333333333333333333333333"}}
	    ]
	  }
	}`
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	meta, err := readBuildMetadata(path)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Digest != "sha256:"+strings.Repeat("1", 64) {
		t.Fatalf("digest = %q", meta.Digest)
	}
	if len(meta.FromDigests) != 2 {
		t.Fatalf("from digests = %v, want two unique entries", meta.FromDigests)
	}
	if meta.FromDigests[0] != "sha256:"+strings.Repeat("2", 64) ||
		meta.FromDigests[1] != "sha256:"+strings.Repeat("3", 64) {
		t.Fatalf("from digests = %v", meta.FromDigests)
	}
}

func TestBuildRecordFrames(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "output")
	record, err := newBuildRecord(path)
	if err != nil {
		t.Fatal(err)
	}
	buildStreamWriter{record: record, stream: supervisor.FrameStdout}.Write([]byte("hello "))
	buildStreamWriter{record: record, stream: supervisor.FrameStderr}.Write([]byte("oops\n"))
	buildStreamWriter{record: record, stream: supervisor.FrameStdout}.Write([]byte("world\n"))
	if err := record.Close(); err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var frames []string
	seq := int64(0)
	for {
		stream, data, err := supervisor.ReadFrame(f)
		if err != nil {
			break
		}
		frames = append(frames, supervisor.FrameName(stream)+":"+string(data))
		seq += supervisor.HeaderSize + int64(len(data))
	}
	if got := strings.Join(frames, "|"); got != "stdout:hello |stderr:oops\n|stdout:world\n" {
		t.Fatalf("frames = %q", got)
	}
	if seq == 0 {
		t.Fatal("record reported no bytes")
	}
}

func TestUserSliceParent(t *testing.T) {
	got := userSliceParent(1000, "stars-build-abc.slice")
	want := "user.slice/user-1000.slice/user@1000.service/stars.slice/stars-build.slice/stars-build-abc.slice"
	if got != want {
		t.Fatalf("userSliceParent = %q, want %q", got, want)
	}
	// systemd splits a UUID slice name at every '-', so the real unit is
	// nested under each prefix.
	got = userSliceParent(1000, "stars-build-01a1-70b1-984b-597d45716a42.slice")
	want = "user.slice/user-1000.slice/user@1000.service/stars.slice/stars-build.slice/" +
		"stars-build-01a1.slice/stars-build-01a1-70b1.slice/" +
		"stars-build-01a1-70b1-984b.slice/stars-build-01a1-70b1-984b-597d45716a42.slice"
	if got != want {
		t.Fatalf("userSliceParent = %q, want %q", got, want)
	}
}
