package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zibyn/orrery/internal/store"
)

const hash = "$argon2id$v=19$m=65536,t=3,p=4$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA"

// preSwitchVersion is the last migration before ADR-0029 drops login_type and
// password_hash: the script only ever runs against that schema.
const preSwitchVersion = 26

func testDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("ORRERY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ORRERY_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	// A database of its own: the script walks every User.
	name := "polaris_migrate_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		a, err := pgxpool.New(context.Background(), dsn)
		if err == nil {
			a.Exec(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)") //nolint:errcheck
			a.Close()
		}
	})
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Database = name
	src, err := iofs.New(store.Migrations, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	u := fmt.Sprintf("pgx5://%s:%s@%s:%d/%s?sslmode=disable",
		cfg.ConnConfig.User, cfg.ConnConfig.Password, cfg.ConnConfig.Host, cfg.ConnConfig.Port, name)
	m, err := migrate.NewWithSourceInstance("iofs", src, u)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Migrate(preSwitchVersion); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatal(err)
	}
	m.Close()
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// fakePolaris is the slice of the Management API the script uses.
type fakePolaris struct {
	mu     sync.Mutex
	users  map[string][]ident // sub -> identifiers
	writes int
}

type ident struct{ Kind, Value string }

func (f *fakePolaris) holder(value string) string {
	for sub, ids := range f.users {
		for _, id := range ids {
			if id.Value == value {
				return sub
			}
		}
	}
	return ""
}

func (f *fakePolaris) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer m2m" {
		http.Error(w, "no token", http.StatusUnauthorized)
		return
	}
	problem := func(status int, extra map[string]any) {
		body := map[string]any{"status": status, "title": http.StatusText(status), "detail": "fake"}
		for k, v := range extra {
			body[k] = v
		}
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(body) //nolint:errcheck
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/management/users":
		f.writes++
		var in struct {
			Identifier   string `json:"identifier"`
			PasswordHash string `json:"password_hash"`
		}
		json.NewDecoder(r.Body).Decode(&in) //nolint:errcheck
		if !strings.HasPrefix(in.PasswordHash, "$argon2id$") || strings.HasPrefix(in.Identifier, "reject") {
			problem(422, nil)
			return
		}
		if sub := f.holder(in.Identifier); sub != "" {
			problem(409, map[string]any{"sub": sub})
			return
		}
		kind := "username"
		if strings.Contains(in.Identifier, "@") {
			kind = "email"
		}
		sub := "sub-" + uuid.NewString()
		f.users[sub] = []ident{{kind, in.Identifier}}
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"sub": sub}) //nolint:errcheck
	case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/v1/management/users/"):
		f.writes++
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/management/users/"), "/")
		var in struct{ Value string }
		json.NewDecoder(r.Body).Decode(&in) //nolint:errcheck
		if h := f.holder(in.Value); h != "" && h != parts[0] {
			problem(409, nil)
			return
		}
		f.users[parts[0]] = append(f.users[parts[0]], ident{parts[2], in.Value})
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/management/users":
		q := r.URL.Query().Get("q")
		type idJSON struct {
			Kind  string `json:"kind"`
			Value string `json:"value"`
		}
		out := []map[string]any{}
		for sub, ids := range f.users {
			hit := strings.Contains(sub, q)
			var js []idJSON
			for _, id := range ids {
				hit = hit || strings.Contains(id.Value, q)
				js = append(js, idJSON{id.Kind, id.Value})
			}
			if hit {
				out = append(out, map[string]any{"sub": sub, "identifiers": js})
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"users": out, "hasMore": false}) //nolint:errcheck
	default:
		http.NotFound(w, r)
	}
}

type bearer struct{}

func (bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer m2m")
	return http.DefaultTransport.RoundTrip(r)
}

func addUser(t *testing.T, pool *pgxpool.Pool, name, email, loginType string, admin bool, issuer, subject string) {
	t.Helper()
	var pw any
	if loginType == "password" {
		pw = hash
	}
	nul := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO users (id, name, email, login_type, password_hash, is_admin, oidc_issuer, oidc_subject) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		uuid.New(), name, nul(email), loginType, pw, admin, nul(issuer), nul(subject)); err != nil {
		t.Fatal(err)
	}
}

func identity(t *testing.T, pool *pgxpool.Pool, name string) (issuer, subject *string) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), `SELECT oidc_issuer, oidc_subject FROM users WHERE name = $1`, name).Scan(&issuer, &subject); err != nil {
		t.Fatal(err)
	}
	return issuer, subject
}

func statuses(r report) map[string]string {
	m := map[string]string{}
	for _, u := range r.Users {
		m[u.Name] = u.Status
	}
	return m
}

func TestMigrate(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	fake := &fakePolaris{users: map[string][]ident{
		"sub-existing": {{"email", "bob@example.com"}},
		"sub-carol":    {{"email", "carol@example.com"}, {"username", "carol"}},
		"sub-near":     {{"email", "xdave@example.com"}},
	}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	issuer := srv.URL
	client := &http.Client{Transport: bearer{}}

	addUser(t, pool, "alice", "Alice@Example.com", "password", true, "", "")
	addUser(t, pool, "bob", "bob@example.com", "password", false, "", "")
	addUser(t, pool, "reject-me", "", "password", false, "", "")
	addUser(t, pool, "nomail", "", "password", false, "", "")
	addUser(t, pool, "Bad Name", "bad@example.com", "password", false, "", "")
	addUser(t, pool, "carol", "carol@example.com", "oidc", false, "https://old-idp", "old-carol")
	addUser(t, pool, "dave", "dave@example.com", "oidc", false, "https://old-idp", "old-dave")
	addUser(t, pool, "erin", "", "oidc", false, "https://old-idp", "old-erin")
	addUser(t, pool, "ci-bot", "", "none", true, "", "")

	r, err := run(ctx, pool, client, issuer)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"alice":     statusCreated,
		"bob":       statusLinked,
		"reject-me": statusFailed,
		"nomail":    statusCreated,
		"Bad Name":  statusFailed,
		"carol":     statusLinked,
		"dave":      statusFailed, // only a substring match: not guessed
		"erin":      statusFailed, // no email to match by
		"ci-bot":    statusMachine,
	}
	if got := statuses(r); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("statuses = %v, want %v", got, want)
	}
	for _, u := range r.Users {
		if u.Status == statusFailed && u.Reason == "" {
			t.Errorf("%s failed without a reason", u.Name)
		}
	}
	if fake.holder("Bad Name") != "" || fake.holder("bad@example.com") != "" {
		t.Error("an invalid username reached Polaris")
	}

	// Created and linked Users now carry Polaris's issuer and their sub.
	if iss, sub := identity(t, pool, "bob"); iss == nil || *iss != issuer || *sub != "sub-existing" {
		t.Errorf("bob = %v %v", iss, sub)
	}
	if iss, sub := identity(t, pool, "carol"); *iss != issuer || *sub != "sub-carol" {
		t.Errorf("carol = %v %v", *iss, *sub)
	}
	_, aliceSub := identity(t, pool, "alice")
	if fake.holder("alice@example.com") != *aliceSub || fake.holder("alice") != *aliceSub {
		t.Errorf("alice not created with email and username: %v", fake.users[*aliceSub])
	}
	if fake.holder("nomail") != *identity2(t, pool, "nomail") {
		t.Error("nomail not created by username")
	}
	if iss, sub := identity(t, pool, "ci-bot"); iss != nil || sub != nil {
		t.Error("machine User was touched")
	}
	if iss, sub := identity(t, pool, "dave"); *iss != "https://old-idp" || *sub != "old-dave" {
		t.Error("unmatched OIDC User was changed")
	}
	if len(r.Admins) != 2 || r.Admins[0] != "alice -> "+*aliceSub || !strings.HasPrefix(r.Admins[1], "ci-bot (machine") {
		t.Errorf("admins = %v", r.Admins)
	}

	// A rerun writes nothing to Polaris and reports the linked ones as already linked.
	writes := fake.writes
	r, err = run(ctx, pool, client, issuer)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alice", "bob", "nomail", "carol"} {
		if s := statuses(r)[name]; s != statusAlreadyLinked {
			t.Errorf("rerun %s = %s", name, s)
		}
	}
	if fake.writes != writes+1 { // only reject-me is retried and refused again
		t.Errorf("rerun made %d writes", fake.writes-writes)
	}
}

func identity2(t *testing.T, pool *pgxpool.Pool, name string) *string {
	_, sub := identity(t, pool, name)
	return sub
}
