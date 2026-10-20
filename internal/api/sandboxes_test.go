package api

import (
	"testing"

	"github.com/google/uuid"

	"github.com/zibyn/orrery/internal/store"
)

// GET /sandboxes filters by owner only for an admin; everyone else always sees
// their own (api.md §3).
func TestSandboxListOwner(t *testing.T) {
	own, other := uuid.New(), uuid.New()
	user := &principal{user: store.User{ID: own}, token: store.GetTokenWithUserRow{Scope: "user"}}
	admin := &principal{user: store.User{ID: own}, token: store.GetTokenWithUserRow{Scope: "admin", UserIsAdmin: true}}
	demoted := &principal{user: store.User{ID: own}, token: store.GetTokenWithUserRow{Scope: "admin"}}

	tests := []struct {
		name  string
		p     *principal
		raw   string
		want  uuid.UUID
		valid bool
	}{
		{"user without owner", user, "", own, true},
		{"user ignores owner", user, other.String(), own, true},
		{"admin without owner", admin, "", own, true},
		{"admin names another owner", admin, other.String(), other, true},
		{"admin with an invalid owner", admin, "not-a-uuid", uuid.Nil, false},
		{"demoted admin ignores owner", demoted, other.String(), own, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := sandboxListOwner(tt.p, tt.raw)
			if ok != tt.valid || got != tt.want {
				t.Fatalf("sandboxListOwner = (%v, %v), want (%v, %v)", got, ok, tt.want, tt.valid)
			}
		})
	}
}

// api.md §3: runtime is `container` (the default) or `local`; anything else is
// invalid. Local has no Snapshot and no Resources.
func TestSandboxRuntimeRequest(t *testing.T) {
	str := func(v string) *string { return &v }
	tests := []struct {
		name      string
		raw       *string
		runtime   string
		code      string
		snapshot  *string
		resources bool
		wantCode  string
	}{
		{name: "omitted defaults to container", raw: nil, runtime: "container"},
		{name: "empty defaults to container", raw: str(""), runtime: "container"},
		{name: "container", raw: str("container"), runtime: "container"},
		{name: "local", raw: str("local"), runtime: "local"},
		{name: "unknown", raw: str("podman"), code: "invalid_request"},
		{name: "local with a Snapshot", raw: str("local"), code: "unsupported", snapshot: str("default")},
		{name: "local with an empty Snapshot is fine", raw: str("local"), runtime: "local", snapshot: str("")},
		{name: "local with Resources", raw: str("local"), code: "unsupported", resources: true},
		{name: "container with a Snapshot and Resources", raw: str("container"), runtime: "container", snapshot: str("default"), resources: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var snapshot *string
			if tt.snapshot != nil {
				snapshot = tt.snapshot
			}
			req := createSandboxRequest{Runtime: tt.raw, Snapshot: snapshot}
			if tt.resources {
				req.Resources = []sandboxResourceRequest{{Resource: "r"}}
			}
			got, code, _ := sandboxRuntime(req)
			if tt.code != "" {
				if code != tt.code {
					t.Fatalf("sandboxRuntime problem = %q, want %q", code, tt.code)
				}
				return
			}
			if code != "" {
				t.Fatalf("sandboxRuntime rejected with %q: %s", code, got)
			}
			if got != tt.runtime {
				t.Fatalf("sandboxRuntime = %q, want %q", got, tt.runtime)
			}
		})
	}
}
