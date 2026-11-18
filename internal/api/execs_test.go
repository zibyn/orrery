package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	nodev1 "github.com/zibyn/orrery/gen/node/v1"
	"github.com/zibyn/orrery/internal/store"
)

// api.md §4: cwd is relative to /workspace. Absolute paths and traversal are
// rejected; a path that cleans back inside is accepted.
func TestValidExecCwd(t *testing.T) {
	for _, cwd := range []string{"", ".", "sub", "sub/dir", "sub/../dir"} {
		if !validExecCwd(cwd) {
			t.Errorf("validExecCwd(%q) = false, want true", cwd)
		}
	}
	for _, cwd := range []string{"/etc", "/workspace/../etc", "..", "../etc", "sub/../../etc"} {
		if validExecCwd(cwd) {
			t.Errorf("validExecCwd(%q) = true, want false", cwd)
		}
	}
}

// An Exec's own secrets name the Sandbox owner's Secrets, resolve once at
// creation, and override the Sandbox's of the same name in StartExec
// (api.md §4, ADR-0032).
func TestCreateExecSecrets(t *testing.T) {
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
	s := New(Config{EncryptionKeys: testKeyring(t)}, pool, slog.New(slog.DiscardHandler))

	ownerID, nodeID, sandboxID := uuid.New(), uuid.New(), uuid.New()
	owner := &principal{user: store.User{ID: ownerID}, token: store.GetTokenWithUserRow{Scope: "user"}}
	admin := &principal{user: store.User{ID: uuid.New()}, token: store.GetTokenWithUserRow{Scope: "admin", UserIsAdmin: true}}
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users (id, name) VALUES ($1, $2)`, []any{ownerID, "u-" + ownerID.String()}},
		{`INSERT INTO nodes (id, name, token_hash) VALUES ($1, $2, '')`, []any{nodeID, "n-" + nodeID.String()}},
	} {
		if _, err := pool.Exec(ctx, stmt.sql, stmt.args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	putSecret := func(name, value string) {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"value": value})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, "/api/v1/secrets/"+name, strings.NewReader(string(body)))
		req.SetPathValue("name", name)
		s.handlePutSecret(rec, req, owner)
		if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
			t.Fatalf("put Secret %s: %d %s", name, rec.Code, rec.Body)
		}
	}
	putSecret("sandbox-tok", "from-sandbox")
	putSecret("sandbox-only", "kept")
	putSecret("exec-tok", "from-exec")
	sandboxSnapshot, missing, err := resolveSecretEnv(ctx, s.q, ownerID, secretEnvRefs(map[string]string{
		"TOK": "sandbox-tok", "ONLY_SANDBOX": "sandbox-only",
	}))
	if err != nil || len(missing) > 0 {
		t.Fatalf("resolve the Sandbox's Secrets: %v %v", missing, err)
	}
	sandboxCt, err := encodeSecretEnv(sandboxSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sandboxes (id, owner_id, name, snapshot_digest, cpu, memory, node_id, state, secret_env_ct)
		VALUES ($1, $2, 'sb', 'sha256:x', 1, 1, $3, 'ready', $4)`, sandboxID, ownerID, nodeID, sandboxCt); err != nil {
		t.Fatalf("seed Sandbox: %v", err)
	}

	createExec := func(p *principal, key string, secrets map[string]string) *httptest.ResponseRecorder {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"argv": []string{"true"}, "secrets": secrets})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/sandboxes/"+sandboxID.String()+"/execs", strings.NewReader(string(body)))
		req.SetPathValue("id", sandboxID.String())
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		s.handleCreateExec(rec, req, p)
		return rec
	}
	conn := &agentConn{nodeID: nodeID, send: make(chan *nodev1.ControlMessage, 1)}
	s.reconcile.hub.register(conn)
	// startedSecretEnv runs the Reconciler's StartExec for the created Exec and
	// returns the secret_env it sends the Node.
	startedSecretEnv := func(rec *httptest.ResponseRecorder) map[string]string {
		t.Helper()
		var e execDTO
		if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
			t.Fatalf("decode Exec: %v (%s)", err, rec.Body)
		}
		rows, err := s.q.ListActiveExecs(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			if row.Exec.ID.String() != e.ID {
				continue
			}
			s.reconcile.startExec(ctx, row)
			select {
			case msg := <-conn.send:
				return msg.GetStartExec().GetSecretEnv()
			case <-time.After(5 * time.Second):
				t.Fatal("no StartExec sent")
			}
		}
		t.Fatalf("Exec %s is not active", e.ID)
		return nil
	}

	t.Run("invalid name", func(t *testing.T) {
		for _, env := range []string{"LD_PRELOAD", "PATH", "A=B", ""} {
			assertProblem(t, createExec(owner, "", map[string]string{env: "exec-tok"}), http.StatusBadRequest, "invalid_request")
		}
	})

	t.Run("unknown Secret", func(t *testing.T) {
		assertProblem(t, createExec(owner, "", map[string]string{"TOK": "no-such-secret"}), http.StatusBadRequest, "invalid_request")
	})

	// The requester is an admin, yet the owner's Secret is the one used.
	t.Run("Exec overrides Sandbox", func(t *testing.T) {
		rec := createExec(admin, "", map[string]string{"TOK": "exec-tok"})
		if rec.Code != http.StatusCreated {
			t.Fatalf("create Exec: %d %s", rec.Code, rec.Body)
		}
		got := startedSecretEnv(rec)
		want := map[string]string{"TOK": "from-exec", "ONLY_SANDBOX": "kept"}
		if !maps.Equal(got, want) {
			t.Fatalf("secret_env = %v, want %v", got, want)
		}
	})

	t.Run("replay does not re-resolve", func(t *testing.T) {
		putSecret("replay-tok", "v1")
		first := createExec(owner, "replay", map[string]string{"TOK": "replay-tok"})
		if first.Code != http.StatusCreated {
			t.Fatalf("create Exec: %d %s", first.Code, first.Body)
		}
		putSecret("replay-tok", "v2")
		replay := createExec(owner, "replay", map[string]string{"TOK": "replay-tok"})
		if replay.Code != http.StatusOK || replay.Body.String() != first.Body.String() {
			t.Fatalf("replay = %d %s, want 200 %s", replay.Code, replay.Body, first.Body)
		}
		if got := startedSecretEnv(replay)["TOK"]; got != "v1" {
			t.Fatalf("replayed Exec's TOK = %q, want v1 from its creation", got)
		}
	})

	// auditOf reads, through GET /audit, the sandbox.exec.create Records of
	// the Exec that rec created.
	auditOf := func(rec *httptest.ResponseRecorder) []auditDTO {
		t.Helper()
		if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
			t.Fatalf("create Exec: %d %s", rec.Code, rec.Body)
		}
		var e execDTO
		if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
			t.Fatalf("decode Exec: %v (%s)", err, rec.Body)
		}
		list := httptest.NewRecorder()
		s.handleListAudit(list, httptest.NewRequest(http.MethodGet,
			"/api/v1/audit?action=sandbox.exec.create&target_type=sandbox&target="+sandboxID.String(), nil), admin)
		var page auditList
		if err := json.Unmarshal(list.Body.Bytes(), &page); err != nil {
			t.Fatalf("decode audit: %v (%s)", err, list.Body)
		}
		var out []auditDTO
		for _, a := range page.Items {
			var d struct {
				Exec string `json:"exec"`
			}
			if json.Unmarshal(a.Detail, &d) == nil && d.Exec == e.ID {
				out = append(out, a)
			}
		}
		return out
	}

	// The owner's Secrets reach whoever creates the Exec, so its creation is
	// audited with the caller, the owner and the Secret names, never a value
	// (ADR-0013). Concurrent requests with one key are one Exec: the others
	// replay it, through the lookup or the insert's conflict, and write nothing.
	t.Run("audit", func(t *testing.T) {
		putSecret("audit-tok", "audit-value")
		secrets := map[string]string{"TOK": "audit-tok", "ALSO": "audit-tok", "OTHER": "exec-tok"}
		for _, caller := range []struct {
			name string
			p    *principal
		}{{"owner", owner}, {"admin", admin}} {
			recs := make([]*httptest.ResponseRecorder, 8)
			var wg sync.WaitGroup
			for i := range recs {
				wg.Go(func() { recs[i] = createExec(caller.p, "audit-"+caller.name, secrets) })
			}
			wg.Wait()
			for _, rec := range recs[1:] {
				auditOf(rec)
			}
			records := auditOf(recs[0])
			if len(records) != 1 {
				t.Fatalf("%s: %d Audit Records, want 1", caller.name, len(records))
			}
			a := records[0]
			if a.ActorUserID == nil || *a.ActorUserID != caller.p.user.ID.String() {
				t.Fatalf("%s: actor = %v, want %s", caller.name, a.ActorUserID, caller.p.user.ID)
			}
			var d struct {
				Owner   string   `json:"owner"`
				Secrets []string `json:"secrets"`
			}
			if err := json.Unmarshal(a.Detail, &d); err != nil {
				t.Fatal(err)
			}
			if d.Owner != ownerID.String() || !slices.Equal(d.Secrets, []string{"audit-tok", "exec-tok"}) {
				t.Fatalf("%s: detail = %s, want owner %s and secrets [audit-tok exec-tok]", caller.name, a.Detail, ownerID)
			}
			for _, value := range []string{"audit-value", "from-exec"} {
				if strings.Contains(string(a.Detail), value) {
					t.Fatalf("%s: detail %s leaks a Secret value", caller.name, a.Detail)
				}
			}
		}
		if got := auditOf(createExec(owner, "", nil)); len(got) != 0 {
			t.Fatalf("an Exec without secrets wrote %d Audit Records, want none", len(got))
		}
	})
}
