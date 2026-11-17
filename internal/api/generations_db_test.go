package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	nodev1 "github.com/zibyn/orrery/gen/node/v1"
	"github.com/zibyn/orrery/internal/generation"
	"github.com/zibyn/orrery/internal/manifest"
	"github.com/zibyn/orrery/internal/objectstore"
	"github.com/zibyn/orrery/internal/store"
)

// fakeGenerationStore is the object store as the Generation code sees it:
// CAS objects by owner and digest, Generation manifests by owner and id.
type fakeGenerationStore struct {
	mu          sync.Mutex
	objects     map[string]bool
	generations map[string][]byte
	content     map[string][]byte // CAS bytes served by server
	manifests   map[string][]byte // Resource manifests by owner/address
	server      *httptest.Server
}

func newFakeGenerationStore() *fakeGenerationStore {
	return &fakeGenerationStore{objects: map[string]bool{}, generations: map[string][]byte{}}
}

func (f *fakeGenerationStore) put(owner uuid.UUID, digest string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[owner.String()+"/"+digest] = true
}

func (f *fakeGenerationStore) HeadSHA256(_ context.Context, owner, digest string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.objects[owner+"/"+digest] {
		return "", objectstore.ErrNotFound
	}
	return digest, nil
}

func (f *fakeGenerationStore) GetGeneration(_ context.Context, owner, id string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.generations[owner+"/"+id]
	if !ok {
		return nil, objectstore.ErrNotFound
	}
	return data, nil
}

func (f *fakeGenerationStore) DeleteGeneration(_ context.Context, owner, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.generations, owner+"/"+id)
	return nil
}

// putContent stores data as a CAS object and returns its digest.
func (f *fakeGenerationStore) putContent(owner uuid.UUID, data []byte) string {
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[owner.String()+"/"+digest] = true
	if f.content == nil {
		f.content = map[string][]byte{}
	}
	f.content[owner.String()+"/"+digest] = data
	return digest
}

// PresignGetURL serves the object from an in-process HTTP server.
func (f *fakeGenerationStore) PresignGetURL(_ context.Context, owner, digest string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.server == nil {
		f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			f.mu.Lock()
			data, ok := f.content[strings.TrimPrefix(r.URL.Path, "/")]
			f.mu.Unlock()
			if !ok {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write(data)
		}))
	}
	return f.server.URL + "/" + owner + "/" + digest, nil
}

func (f *fakeGenerationStore) PutManifest(_ context.Context, owner, address string, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.manifests == nil {
		f.manifests = map[string][]byte{}
	}
	f.manifests[owner+"/"+address] = data
	return nil
}

type generationFixture struct {
	t       *testing.T
	ctx     context.Context
	s       *Server
	store   *fakeGenerationStore
	node    store.Node
	user    uuid.UUID
	sandbox uuid.UUID
}

func newGenerationFixture(t *testing.T) *generationFixture {
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
	fake := newFakeGenerationStore()
	f := &generationFixture{t: t, ctx: ctx, store: fake, user: uuid.New(), sandbox: uuid.New(),
		s: &Server{pool: pool, q: store.New(pool), log: slog.New(slog.NewTextHandler(io.Discard, nil)), generations: fake}}
	nodeID := uuid.New()
	f.exec(`INSERT INTO users (id, name) VALUES ($1, $2)`, f.user, "u-"+f.user.String())
	f.exec(`INSERT INTO nodes (id, name, token_hash) VALUES ($1, $2, '')`, nodeID, "n-"+nodeID.String())
	f.exec(`INSERT INTO sandboxes (id, owner_id, name, snapshot_digest, cpu, memory, state, desired_state, node_id)
		VALUES ($1, $2, 'sb', 'sha256:x', 1, 1, 'stopping', 'stopped', $3)`, f.sandbox, f.user, nodeID)
	if f.node, err = f.s.q.GetNode(ctx, nodeID); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *generationFixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.s.pool.Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
}

func (f *generationFixture) head() *uuid.UUID {
	f.t.Helper()
	sb, err := f.s.q.GetSandboxIncludingDeleted(f.ctx, f.sandbox)
	if err != nil {
		f.t.Fatal(err)
	}
	return sb.HeadGenerationID
}

// diff is a one-file diff manifest whose file is digest.
func diff(digest string) generation.Manifest {
	return generation.Manifest{Kind: generation.KindDiff, Entries: []generation.Entry{
		{Entry: manifest.Entry{Path: "workspace", Type: manifest.TypeDir}, Mode: 0o755},
		{Entry: manifest.Entry{Path: "workspace/f", Type: manifest.TypeFile, Size: ptr(int64(1)), SHA256: digest}, Mode: 0o644},
	}}
}

func ptr[T any](v T) *T { return &v }

// submit opens an upload for m at fence generation on base.
func (f *generationFixture) submit(m generation.Manifest, fence int64, base *uuid.UUID) (*nodev1.GenerationAck, error) {
	chunk := &nodev1.GenerationChunk{SandboxId: f.sandbox.String(), Generation: fence}
	if base != nil {
		chunk.BaseGenerationId = base.String()
	}
	for _, e := range m.Entries {
		chunk.Entries = append(chunk.Entries, &nodev1.ManifestEntry{Path: e.Path, Type: e.Type, Size: e.Size, Sha256: e.SHA256, Blocks: e.Blocks})
	}
	return f.s.submitGeneration(f.ctx, f.node, []*nodev1.GenerationChunk{chunk})
}

// upload does what the Agent does with the ack: PUT the missing objects and
// the full manifest.
func (f *generationFixture) upload(ack *nodev1.GenerationAck, m generation.Manifest) {
	f.t.Helper()
	for _, digest := range ack.GetMissingSha256() {
		f.store.put(f.user, digest)
	}
	data, err := generation.Encode(m)
	if err != nil {
		f.t.Fatal(err)
	}
	f.store.mu.Lock()
	f.store.generations[f.user.String()+"/"+ack.GetUploadId()] = data
	f.store.mu.Unlock()
}

func (f *generationFixture) commit(ack *nodev1.GenerationAck) (*nodev1.CommitGenerationResponse, error) {
	return f.s.commitGeneration(f.ctx, f.node, &nodev1.CommitGenerationRequest{UploadId: ack.GetUploadId()})
}

// commitOK runs the whole flow and returns the new head.
func (f *generationFixture) commitOK(m generation.Manifest, base *uuid.UUID) uuid.UUID {
	f.t.Helper()
	ack, err := f.submit(m, 1, base)
	if err != nil {
		f.t.Fatal(err)
	}
	f.upload(ack, m)
	resp, err := f.commit(ack)
	if err != nil {
		f.t.Fatal(err)
	}
	return uuid.MustParse(resp.GetGenerationId())
}

var (
	objA = strings.Repeat("a", 64)
	objB = strings.Repeat("b", 64)
	objC = strings.Repeat("c", 64)
)

func TestGenerationCommitAdvancesHead(t *testing.T) {
	f := newGenerationFixture(t)
	f.store.put(f.user, objA)
	ack, err := f.submit(diff(objB), 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := ack.GetMissingSha256(); len(got) != 1 || got[0] != objB {
		t.Fatalf("missing = %v, want [b]", got)
	}
	f.upload(ack, diff(objB))
	resp, err := f.commit(ack)
	if err != nil {
		t.Fatal(err)
	}
	first := uuid.MustParse(resp.GetGenerationId())
	if h := f.head(); h == nil || *h != first {
		t.Fatalf("head = %v, want %s", h, first)
	}
	second := f.commitOK(diff(objA), &first)
	if h := f.head(); h == nil || *h != second {
		t.Fatalf("head = %v, want %s", h, second)
	}
	// Committing the same upload again does not move the head back.
	if _, err := f.commit(ack); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("recommit = %v, want FailedPrecondition", err)
	}
}

// A commit whose reply was lost is retried; while it is the head, the retry
// succeeds instead of sending the Agent back to resubmit.
func TestGenerationCommitRetrySucceeds(t *testing.T) {
	f := newGenerationFixture(t)
	ack, err := f.submit(diff(objA), 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.upload(ack, diff(objA))
	for range 2 {
		if resp, err := f.commit(ack); err != nil || resp.GetGenerationId() != ack.GetUploadId() {
			t.Fatalf("commit = %v, %v", resp, err)
		}
	}
}

// Presign signs a Generation manifest PUT only for an open upload of this
// Node, and GET only for a head of a Sandbox on it.
func TestGenerationPresignAuthorization(t *testing.T) {
	f := newGenerationFixture(t)
	ack, err := f.submit(diff(objA), 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	allowed := func(id, method string) bool {
		t.Helper()
		_, ok, err := f.s.authorizeGeneration(f.ctx, f.node, id, method)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !allowed(ack.GetUploadId(), "PUT") || allowed(ack.GetUploadId(), "GET") {
		t.Fatal("an open upload must be signable for PUT only")
	}
	if owner, err := f.s.q.FindGenerationUploadObjectOwner(f.ctx, store.FindGenerationUploadObjectOwnerParams{
		NodeID: f.node.ID, Sha256: objA,
	}); err != nil || owner != f.user {
		t.Fatalf("missing object PUT owner = %v, %v", owner, err)
	}
	f.upload(ack, diff(objA))
	if _, err := f.commit(ack); err != nil {
		t.Fatal(err)
	}
	if allowed(ack.GetUploadId(), "PUT") || !allowed(ack.GetUploadId(), "GET") {
		t.Fatal("a head must be signable for GET only")
	}
	if allowed(uuid.NewString(), "GET") {
		t.Fatal("an unknown Generation was authorized")
	}
}

func TestGenerationCommitRefusesStaleFence(t *testing.T) {
	f := newGenerationFixture(t)
	if _, err := f.submit(diff(objA), 0, nil); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("submit with an old fence = %v, want FailedPrecondition", err)
	}
	ack, err := f.submit(diff(objA), 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.upload(ack, diff(objA))
	// A restart supersedes the committer before it commits.
	f.exec(`UPDATE sandboxes SET generation = generation + 1 WHERE id = $1`, f.sandbox)
	if _, err := f.commit(ack); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("commit with a stale fence = %v, want FailedPrecondition", err)
	}
	if h := f.head(); h != nil {
		t.Fatalf("head = %s after a refused commit", h)
	}
}

func TestGenerationCommitRefusesAnotherNode(t *testing.T) {
	f := newGenerationFixture(t)
	other := uuid.New()
	f.exec(`INSERT INTO nodes (id, name, token_hash) VALUES ($1, $2, '')`, other, "n-"+other.String())
	f.exec(`UPDATE sandboxes SET node_id = $2 WHERE id = $1`, f.sandbox, other)
	if _, err := f.submit(diff(objA), 1, nil); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("submit from a Node the Sandbox is not on = %v, want FailedPrecondition", err)
	}
}

func TestGenerationConcurrentCommitsAdvanceOnce(t *testing.T) {
	f := newGenerationFixture(t)
	acks := make([]*nodev1.GenerationAck, 2)
	for i, digest := range []string{objA, objB} {
		ack, err := f.submit(diff(digest), 1, nil)
		if err != nil {
			t.Fatal(err)
		}
		f.upload(ack, diff(digest))
		acks[i] = ack
	}
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range acks {
		wg.Go(func() { _, errs[i] = f.commit(acks[i]) })
	}
	wg.Wait()
	won := -1
	for i, err := range errs {
		switch {
		case err == nil:
			if won >= 0 {
				t.Fatal("both commits on the same base succeeded")
			}
			won = i
		case status.Code(err) != codes.FailedPrecondition:
			t.Fatalf("losing commit = %v, want FailedPrecondition", err)
		}
	}
	if won < 0 {
		t.Fatal("neither commit succeeded")
	}
	if h := f.head(); h == nil || h.String() != acks[won].GetUploadId() {
		t.Fatalf("head = %v, want the winner %s", h, acks[won].GetUploadId())
	}
}

func TestGenerationCommitRefusesMissingObject(t *testing.T) {
	f := newGenerationFixture(t)
	ack, err := f.submit(diff(objA), 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.upload(&nodev1.GenerationAck{UploadId: ack.GetUploadId()}, diff(objA)) // manifest only
	if _, err := f.commit(ack); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("commit with a missing object = %v, want FailedPrecondition", err)
	}
	if h := f.head(); h != nil {
		t.Fatalf("head = %s after a refused commit", h)
	}
}

// Only the head is retained: the superseded Generation's row goes, so an
// object only it referenced becomes sweepable while the head's stay.
func TestGenerationPurgeFreesSupersededObjects(t *testing.T) {
	f := newGenerationFixture(t)
	both := func(x, y string) generation.Manifest {
		m := diff(x)
		m.Entries = append(m.Entries, generation.Entry{
			Entry: manifest.Entry{Path: "workspace/g", Type: manifest.TypeFile, Size: ptr(int64(1)), SHA256: y}, Mode: 0o644})
		return m
	}
	first := f.commitOK(both(objA, objB), nil)
	second := f.commitOK(both(objA, objC), &first)

	if err := f.s.purgeGenerations(f.ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.q.GetGeneration(f.ctx, first); err == nil {
		t.Fatal("superseded Generation survived the purge")
	}
	if _, err := f.store.GetGeneration(f.ctx, f.user.String(), first.String()); err == nil {
		t.Fatal("superseded Generation manifest survived the purge")
	}
	if _, err := f.s.q.GetGeneration(f.ctx, second); err != nil {
		t.Fatalf("head Generation: %v", err)
	}

	rows, err := f.s.q.ListSweepableCASObjects(f.ctx, store.ListSweepableCASObjectsParams{
		Cutoff: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true}, Limit: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	var mine []string
	for _, row := range rows {
		if row.OwnerID == f.user {
			mine = append(mine, row.Sha256)
		}
	}
	sort.Strings(mine)
	if len(mine) != 1 || mine[0] != objB {
		t.Fatalf("sweepable = %v, want only the superseded Generation's own object", mine)
	}
}
