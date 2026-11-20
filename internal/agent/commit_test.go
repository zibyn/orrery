package agent

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	nodev1 "github.com/zibyn/orrery/gen/node/v1"
	"github.com/zibyn/orrery/internal/generation"
	"github.com/zibyn/orrery/internal/nodeproto"
)

// fakeGenerationNode is the Control Plane side of a Generation commit: it
// stores what the Agent PUTs and advances a head, failing the first
// unavailable submits as an unreachable object store would.
type fakeGenerationNode struct {
	nodev1.NodeClient
	http *httptest.Server

	mu          sync.Mutex
	unavailable int
	refuse      error
	sendEOF     bool // Send fails as on a stream the server already ended
	head        string
	bases       []string
	submitted   [][]string // sha256 of the submitted file entries, per submit
	objects     map[string][]byte
	manifests   map[string][]byte
	puts        map[string]int // stored PUTs per object
	putDelay    time.Duration  // how long each object PUT takes
	failPut     string         // the object whose next PUT fails
	holdPuts    bool           // object PUTs but failPut's answer only once canceled
	inflight    int
	maxInflight int
}

func newFakeGenerationNode(t *testing.T) *fakeGenerationNode {
	f := &fakeGenerationNode{objects: map[string][]byte{}, manifests: map[string][]byte{}, puts: map[string]int{}}
	f.http = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		digest, object := strings.CutPrefix(r.URL.Path, "/objects/")
		data, err := io.ReadAll(r.Body) // read first: then r's context ends when the client gives up
		if err != nil {
			return // a canceled PUT stores nothing
		}
		if r.Method == http.MethodPut && object {
			f.mu.Lock()
			f.inflight++
			f.maxInflight = max(f.maxInflight, f.inflight)
			delay, hold := f.putDelay, f.holdPuts && digest != f.failPut
			f.mu.Unlock()
			defer func() { f.mu.Lock(); f.inflight--; f.mu.Unlock() }()
			time.Sleep(delay)
			if hold {
				select {
				case <-r.Context().Done():
					return // canceled: stores nothing
				case <-time.After(5 * time.Second): // never canceled: store it, for the test to see
				}
			}
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Method == http.MethodGet {
			if key, ok := strings.CutPrefix(r.URL.Path, "/generations/"); ok {
				w.Write(f.manifests[key]) //nolint:errcheck // test server
			} else {
				w.Write(f.objects[strings.TrimPrefix(r.URL.Path, "/objects/")]) //nolint:errcheck // test server
			}
			return
		}
		if key, ok := strings.CutPrefix(r.URL.Path, "/generations/"); ok {
			f.manifests[key] = data
		} else if digest == f.failPut {
			f.failPut = ""
			http.Error(w, "injected failure", http.StatusInternalServerError)
		} else {
			f.objects[digest] = data
			f.puts[digest]++
		}
	}))
	t.Cleanup(f.http.Close)
	return f
}

type fakeGenerationStream struct {
	grpc.ClientStreamingClient[nodev1.GenerationChunk, nodev1.GenerationAck]
	f      *fakeGenerationNode
	chunks []*nodev1.GenerationChunk
}

func (s *fakeGenerationStream) Send(c *nodev1.GenerationChunk) error {
	if s.f.sendEOF {
		return io.EOF
	}
	s.chunks = append(s.chunks, c)
	return nil
}

func (s *fakeGenerationStream) CloseAndRecv() (*nodev1.GenerationAck, error) {
	f := s.f
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.unavailable > 0 {
		f.unavailable--
		return nil, status.Error(codes.Unavailable, "object storage is unreachable")
	}
	if f.refuse != nil {
		return nil, f.refuse
	}
	f.bases = append(f.bases, s.chunks[0].GetBaseGenerationId())
	var digests, missing []string
	for _, c := range s.chunks {
		for _, e := range c.GetEntries() {
			digests = append(digests, e.GetSha256())
			for _, d := range append([]string{e.GetSha256()}, e.GetBlocks()...) {
				if _, ok := f.objects[d]; !ok && d != "" {
					missing = append(missing, d)
				}
			}
		}
	}
	f.submitted = append(f.submitted, digests)
	return &nodev1.GenerationAck{UploadId: uuid.NewString(), MissingSha256: missing}, nil
}

func (f *fakeGenerationNode) SubmitGeneration(context.Context, ...grpc.CallOption) (grpc.ClientStreamingClient[nodev1.GenerationChunk, nodev1.GenerationAck], error) {
	return &fakeGenerationStream{f: f}, nil
}

func (f *fakeGenerationNode) Presign(_ context.Context, req *nodev1.PresignRequest, _ ...grpc.CallOption) (*nodev1.PresignResponse, error) {
	resp := &nodev1.PresignResponse{}
	for _, d := range req.GetSha256() {
		resp.Urls = append(resp.Urls, f.http.URL+"/objects/"+d)
	}
	for _, id := range req.GetGenerations() {
		resp.Urls = append(resp.Urls, f.http.URL+"/generations/"+id)
	}
	return resp, nil
}

func (f *fakeGenerationNode) CommitGeneration(_ context.Context, req *nodev1.CommitGenerationRequest, _ ...grpc.CallOption) (*nodev1.CommitGenerationResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.manifests[req.GetUploadId()]; !ok {
		return nil, status.Error(codes.FailedPrecondition, "the Generation manifest is not uploaded")
	}
	f.head = req.GetUploadId()
	return &nodev1.CommitGenerationResponse{GenerationId: f.head}, nil
}

func fastCommitRetries(t *testing.T) {
	oldMin, oldMax := commitRetryMin, commitRetryMax
	commitRetryMin, commitRetryMax = time.Millisecond, 4*time.Millisecond
	t.Cleanup(func() { commitRetryMin, commitRetryMax = oldMin, oldMax })
}

// An unreachable object store keeps the stop retrying with commit_retrying
// from the third failure; once it is back the commit completes, and the next
// stop uploads only what changed (runtime.md §16, defaults.md).
func TestCommitRetriesUntilTheStoreIsBack(t *testing.T) {
	fastCommitRetries(t)
	f := newFakeGenerationNode(t)
	f.unavailable = 4
	rt := newRuntime(ProviderContainerd, t.TempDir(), "node")
	rt.client = f
	id := uuid.New()
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "etc/motd"), "hello", 0o644)
	writeTestFile(t, filepath.Join(root, "workspace/a"), "a", 0o644)
	target := commitTarget{root: root, kind: generation.KindDiff}

	retrying := 0
	if err := rt.commitWithRetry(context.Background(), id, 3, target, func() { retrying++ }); err != nil {
		t.Fatal(err)
	}
	if retrying != 2 { // failures 3 and 4
		t.Fatalf("reported retrying %d times, want 2", retrying)
	}
	first := f.head
	m, err := generation.Decode(f.manifests[first])
	if err != nil {
		t.Fatal(err)
	}
	if e := entriesByPath(m)["etc/motd"]; e.SHA256 != digestOf("hello") || string(f.objects[e.SHA256]) != "hello" {
		t.Fatalf("motd = %+v", e)
	}

	writeTestFile(t, filepath.Join(root, "workspace/b"), "b", 0o644)
	if err := rt.commitWithRetry(context.Background(), id, 3, target, func() { retrying++ }); err != nil {
		t.Fatal(err)
	}
	if got := f.submitted[len(f.submitted)-1]; len(got) != 1 || got[0] != digestOf("b") {
		t.Fatalf("second stop submitted %v, want only b", got)
	}
	if f.bases[len(f.bases)-1] != first {
		t.Fatalf("second commit based on %q, want %q", f.bases[len(f.bases)-1], first)
	}
	m, _ = generation.Decode(f.manifests[f.head])
	if _, ok := entriesByPath(m)["workspace/a"]; !ok {
		t.Fatal("the second Generation lost the unchanged file")
	}
}

// A holder the Control Plane refuses (deleted, moved, newer generation) has
// nothing left to commit to: the stop ends without retrying.
func TestCommitEndsWhenTheHolderIsRefused(t *testing.T) {
	fastCommitRetries(t)
	f := newFakeGenerationNode(t)
	f.refuse = status.Error(codes.FailedPrecondition, "the Sandbox is deleted")
	rt := newRuntime(ProviderContainerd, t.TempDir(), "node")
	rt.client = f
	target := commitTarget{root: t.TempDir(), kind: generation.KindDiff}
	if err := rt.commitWithRetry(context.Background(), uuid.New(), 3, target, func() {}); err != nil {
		t.Fatalf("refused commit = %v, want it to end", err)
	}
}

// A server that ends the stream before reading it fails Send with a bare
// io.EOF; the stream's status still decides, so a refused holder ends the stop.
func TestCommitReadsTheStatusBehindSendEOF(t *testing.T) {
	fastCommitRetries(t)
	f := newFakeGenerationNode(t)
	f.sendEOF = true
	f.refuse = status.Error(codes.FailedPrecondition, "the Sandbox is deleted")
	rt := newRuntime(ProviderContainerd, t.TempDir(), "node")
	rt.client = f
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	target := commitTarget{root: t.TempDir(), kind: generation.KindDiff}
	if err := rt.commitWithRetry(ctx, uuid.New(), 3, target, func() {}); err != nil {
		t.Fatalf("refused commit behind io.EOF = %v, want it to end", err)
	}
}

// An unreadable filesystem cannot be committed by retrying: commit_failed.
func TestCommitFailsOnAnUnreadableFilesystem(t *testing.T) {
	fastCommitRetries(t)
	rt := newRuntime(ProviderContainerd, t.TempDir(), "node")
	rt.client = newFakeGenerationNode(t)
	target := commitTarget{root: filepath.Join(t.TempDir(), "gone"), kind: generation.KindDiff}
	if err := rt.commitWithRetry(context.Background(), uuid.New(), 3, target, func() {}); err == nil || !isCommitFailed(err) {
		t.Fatalf("err = %v, want commit_failed", err)
	}
}

func TestUpperDirOfASnapshot(t *testing.T) {
	overlay := []mount.Mount{{Type: "overlay", Options: []string{"index=off", "workdir=/s/2/work", "upperdir=/s/2/fs", "lowerdir=/s/1/fs"}}}
	if got := upperDir(overlay); got != "/s/2/fs" {
		t.Fatalf("overlay upperdir = %q", got)
	}
	if got := upperDir([]mount.Mount{{Type: "bind", Source: "/s/1/fs"}}); got != "/s/1/fs" {
		t.Fatalf("bind upperdir = %q", got)
	}
	if got := upperDir([]mount.Mount{{Type: "overlay", Options: []string{"lowerdir=/a:/b"}}}); got != "" {
		t.Fatalf("a read-only view has upperdir %q", got)
	}
}

// stopRecorder is a driver that stops instantly and commits root.
type stopRecorder struct {
	runtimeDriver
	root    string
	dropped chan struct{}
}

func (d *stopRecorder) stopSandbox(context.Context, uuid.UUID) error { return nil }
func (d *stopRecorder) commitTarget(context.Context, uuid.UUID) (commitTarget, bool, error) {
	return commitTarget{root: d.root, kind: generation.KindDiff}, true, nil
}
func (d *stopRecorder) dropSandboxDisk(context.Context, uuid.UUID) error {
	close(d.dropped)
	return nil
}

func stopAndObserve(t *testing.T, rt *Runtime, id uuid.UUID, generation int64) chan *nodev1.SandboxObserved {
	observed := make(chan *nodev1.SandboxObserved, 10)
	runner := newCommandRunner(t.Context(), rt, func(msg *nodev1.AgentMessage) {
		if obs := msg.GetSandboxObserved(); obs != nil {
			observed <- obs
		}
	})
	runner.handle(&nodev1.ControlMessage{Message: &nodev1.ControlMessage_StopSandbox{
		StopSandbox: &nodev1.StopSandbox{CommandId: "c1", SandboxId: id.String(), Generation: generation},
	}})
	return observed
}

// StopSandbox reports committing, commit_retrying while the store is
// unreachable, and gone only after the commit; the disk stays as the Node's
// cache, and a later stop of it (a delete) has nothing to commit
// (node-protocol.md SandboxObserved, runtime.md §16).
func TestStopSandboxReportsGoneOnlyAfterTheCommit(t *testing.T) {
	fastCommitRetries(t)
	f := newFakeGenerationNode(t)
	f.unavailable = 3
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "etc/motd"), "hello", 0o644)
	driver := &stopRecorder{root: root, dropped: make(chan struct{})}
	rt := newRuntime(ProviderContainerd, t.TempDir(), uuid.NewString())
	rt.driver = driver
	rt.client = f
	id := uuid.New()
	observed := stopAndObserve(t, rt, id, 2)
	var phases []string
	for len(phases) == 0 || phases[len(phases)-1] != string(nodeproto.SandboxPhaseGone) {
		select {
		case obs := <-observed:
			phase := obs.GetPhase()
			if obs.GetCommitRetrying() {
				phase += "+retrying"
			}
			phases = append(phases, phase)
		case <-time.After(5 * time.Second):
			t.Fatalf("phases so far %v", phases)
		}
	}
	if got, want := strings.Join(phases, " "), "committing committing+retrying gone"; got != want {
		t.Fatalf("phases = %q, want %q", got, want)
	}
	f.mu.Lock()
	head := f.head
	f.mu.Unlock()
	if head == "" {
		t.Fatal("gone was reported without a commit")
	}
	select {
	case <-driver.dropped:
		t.Fatal("the committed disk was dropped instead of cached")
	default:
	}

	submits := len(f.submitted)
	again := stopAndObserve(t, rt, id, 3)
	select {
	case obs := <-again:
		if obs.GetPhase() != string(nodeproto.SandboxPhaseGone) {
			t.Fatalf("second stop reported %q first, want gone", obs.GetPhase())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the second stop reported nothing")
	}
	if len(f.submitted) != submits {
		t.Fatal("the second stop committed the cached diff again")
	}
}

// An unreadable upperdir ends the stop commit_failed and keeps the disk.
func TestStopSandboxReportsCommitFailed(t *testing.T) {
	driver := &stopRecorder{root: filepath.Join(t.TempDir(), "gone"), dropped: make(chan struct{})}
	rt := newRuntime(ProviderContainerd, t.TempDir(), uuid.NewString())
	rt.driver = driver
	rt.client = newFakeGenerationNode(t)
	observed := stopAndObserve(t, rt, uuid.New(), 2)
	for _, want := range []string{"committing", "commit_failed"} {
		select {
		case obs := <-observed:
			if obs.GetPhase() != want {
				t.Fatalf("phase = %q, want %q", obs.GetPhase(), want)
			}
			if want == "commit_failed" && obs.GetCause() != "commit_failed" {
				t.Fatalf("cause = %q", obs.GetCause())
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no %s", want)
		}
	}
	select {
	case <-driver.dropped:
		t.Fatal("a failed commit dropped the disk")
	default:
	}
}
