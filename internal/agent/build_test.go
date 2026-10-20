package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moby/buildkit/client"

	"github.com/zibyn/orrery/internal/supervisor"
)

func TestCollectFromDigests(t *testing.T) {
	from := "sha256:" + strings.Repeat("2", 64)
	set := map[string]struct{}{}
	collectFromDigests(&client.SolveStatus{
		Vertexes: []*client.Vertex{
			{Name: "[1/2] FROM docker.io/library/alpine:3.22@" + from},
			{Name: "[2/2] RUN echo probe"},
		},
		Logs: []*client.VertexLog{
			{Data: []byte("# resolve docker.io/library/alpine:3.22 0.0s done")},
		},
	}, set)
	if len(set) != 1 {
		t.Fatalf("digests = %v, want the resolved FROM digest", set)
	}
	if _, ok := set[from]; !ok {
		t.Fatalf("digests = %v, want %s", set, from)
	}
}

func TestPinnedFromDigests(t *testing.T) {
	base := "sha256:" + strings.Repeat("1", 64)
	alpine := "sha256:" + strings.Repeat("2", 64)
	busybox := "sha256:" + strings.Repeat("3", 64)
	dockerfile := "FROM base\n" +
		"COPY --from=ctx /x /x\n" +
		"FROM alpine@" + alpine + "\n" +
		"RUN --mount=type=bind,from=busybox@" + busybox + ",target=/c true\n"
	got := pinnedFromDigests(dockerfile, "127.0.0.1:15000/platform/default@"+base)
	if len(got) != 3 || got[0] != base || got[1] != alpine || got[2] != busybox {
		t.Fatalf("pinnedFromDigests = %v, want base, alpine and busybox", got)
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
	got := userSliceParent(1000, "orrery-build-abc.slice")
	want := "user.slice/user-1000.slice/user@1000.service/orrery.slice/orrery-build.slice/orrery-build-abc.slice"
	if got != want {
		t.Fatalf("userSliceParent = %q, want %q", got, want)
	}
	// systemd splits a UUID slice name at every '-', so the real unit is
	// nested under each prefix.
	got = userSliceParent(1000, "orrery-build-01a1-70b1-984b-597d45716a42.slice")
	want = "user.slice/user-1000.slice/user@1000.service/orrery.slice/orrery-build.slice/" +
		"orrery-build-01a1.slice/orrery-build-01a1-70b1.slice/" +
		"orrery-build-01a1-70b1-984b.slice/orrery-build-01a1-70b1-984b-597d45716a42.slice"
	if got != want {
		t.Fatalf("userSliceParent = %q, want %q", got, want)
	}
}
