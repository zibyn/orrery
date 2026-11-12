package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/zibyn/orrery/internal/store"
)

// user-token --sub is the break-glass path: it mints a working token for an
// existing Polaris-linked User, refuses an unknown sub, and never touches
// is_admin (ADR-0029).
func TestUserTokenSub(t *testing.T) {
	_, op, pool := polarisTestServer(t)
	s := New(Config{OIDC: OIDCConfig{
		IssuerURL: op.URL, ClientID: op.ClientID, ClientSecret: "secret", Audience: op.Audience,
	}}, pool, slog.New(slog.DiscardHandler))
	ctx := context.Background()
	id, sub := uuid.New(), "sub-"+uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, name, oidc_issuer, oidc_subject, is_admin)
		VALUES ($1, $2, $3, $4, true)`, id, "linked-"+id.String(), s.cfg.OIDC.IssuerURL, sub); err != nil {
		t.Fatal(err)
	}

	if _, err := s.UserToken(ctx, UserTokenOptions{Sub: "no-such-" + sub, Scope: "user"}); err == nil {
		t.Fatal("unknown sub: want an error")
	}

	created, err := s.UserToken(ctx, UserTokenOptions{Sub: sub, Scope: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/users/me", nil)
	req.Header.Set("Authorization", "Bearer "+created.Token)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /users/me with the minted token: %d %s", rec.Code, rec.Body)
	}
	var me userDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil {
		t.Fatal(err)
	}
	if me.ID != id.String() || !me.IsAdmin || me.Machine {
		t.Fatalf("GET /users/me = %+v, want the linked admin User", me)
	}
}

// user-token --session mints a console Session for e2e: its cookie
// authenticates as the User, with the scope a login would give.
func TestUserTokenSession(t *testing.T) {
	h, op, pool := polarisTestServer(t)
	s := New(Config{OIDC: OIDCConfig{IssuerURL: op.URL}}, pool, slog.New(slog.DiscardHandler))
	name := "e2e-" + uuid.NewString()
	created, err := s.UserToken(context.Background(), UserTokenOptions{Name: name, Admin: true, Scope: "user", Session: true})
	if err != nil {
		t.Fatal(err)
	}
	if created.Kind != "session" || created.Scope != "admin" {
		t.Fatalf("created = %+v, want an admin-scoped Session", created.tokenDTO)
	}
	var got userDTO
	if code := asSession(t, h, newSessionCookie(created.Token), "/api/v1/users/me", &got); code != http.StatusOK || got.Name != name {
		t.Fatalf("GET /users/me with the Session cookie = %d %+v", code, got)
	}
}

// Promoting an existing machine User to admin writes only is_admin, so a
// quota change committed while the promotion waits survives it (#255).
func TestUserTokenAdminKeepsConcurrentQuota(t *testing.T) {
	s, q := purgeTestServer(t)
	ctx := context.Background()
	name := "machine-" + uuid.NewString()
	user, err := s.machineUser(ctx, name, false)
	if err != nil {
		t.Fatal(err)
	}
	// Hold the row lock with a quota change, so the promotion reads the old
	// row and then waits to write.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `UPDATE users SET gpu_quota = $2 WHERE id = $1`, user.ID, user.GpuQuota+7); err != nil {
		t.Fatal(err)
	}
	type result struct {
		user store.User
		err  error
	}
	done := make(chan result, 1)
	go func() {
		u, err := s.machineUser(ctx, name, true)
		done <- result{u, err}
	}()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		var waiting int
		if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND query LIKE '%UPDATE users%'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("promotion never waited on the row lock")
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	res := <-done
	if res.err != nil {
		t.Fatal(res.err)
	}
	if !res.user.IsAdmin {
		t.Fatalf("promoted = %+v, want an admin", res.user)
	}
	got, err := q.GetUserByName(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsAdmin || got.GpuQuota != user.GpuQuota+7 {
		t.Fatalf("after promotion is_admin %v gpu_quota %d, want true and %d", got.IsAdmin, got.GpuQuota, user.GpuQuota+7)
	}
}
