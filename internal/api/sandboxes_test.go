package api

import (
	"testing"

	"github.com/google/uuid"

	"github.com/zibyn/stars-sandbox/internal/store"
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
