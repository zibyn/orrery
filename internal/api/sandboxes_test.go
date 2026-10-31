package api

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
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

// api.md §3 egress: omitted is open; mode is required and one of three;
// allowlist needs a non-empty allow; allow with any other mode, even empty, is
// invalid; each entry is CIDR[:port]; at most 64 entries (defaults.md).
func TestSandboxEgressRequest(t *testing.T) {
	many := make([]string, 65)
	for i := range many {
		many[i] = fmt.Sprintf("10.0.0.%d", i)
	}
	tooMany, _ := json.Marshal(many)
	maxOK, _ := json.Marshal(many[:64])
	tests := []struct {
		name    string
		body    string
		runtime string
		mode    string
		allow   []string
		code    string
	}{
		{name: "omitted is open", body: `{}`, mode: "open"},
		{name: "null is open", body: `{"egress":null}`, mode: "open"},
		{name: "open", body: `{"egress":{"mode":"open"}}`, mode: "open"},
		{name: "none", body: `{"egress":{"mode":"none"}}`, mode: "none"},
		{name: "allowlist", body: `{"egress":{"mode":"allowlist","allow":["10.1.2.3","10.2.0.0/16:443"]}}`,
			mode: "allowlist", allow: []string{"10.1.2.3", "10.2.0.0/16:443"}},
		{name: "allowlist at the limit", body: `{"egress":{"mode":"allowlist","allow":` + string(maxOK) + `}}`,
			mode: "allowlist", allow: many[:64]},
		{name: "allowlist over the limit", body: `{"egress":{"mode":"allowlist","allow":` + string(tooMany) + `}}`, code: "invalid_request"},
		{name: "egress without mode", body: `{"egress":{}}`, code: "invalid_request"},
		{name: "empty mode", body: `{"egress":{"mode":""}}`, code: "invalid_request"},
		{name: "unknown mode", body: `{"egress":{"mode":"closed"}}`, code: "invalid_request"},
		{name: "allowlist without allow", body: `{"egress":{"mode":"allowlist"}}`, code: "invalid_request"},
		{name: "allowlist with empty allow", body: `{"egress":{"mode":"allowlist","allow":[]}}`, code: "invalid_request"},
		{name: "none with allow", body: `{"egress":{"mode":"none","allow":["10.0.0.1"]}}`, code: "invalid_request"},
		{name: "none with empty allow", body: `{"egress":{"mode":"none","allow":[]}}`, code: "invalid_request"},
		{name: "open with empty allow", body: `{"egress":{"mode":"open","allow":[]}}`, code: "invalid_request"},
		{name: "bad entry", body: `{"egress":{"mode":"allowlist","allow":["example.com"]}}`, code: "invalid_request"},
		{name: "IPv6 entry", body: `{"egress":{"mode":"allowlist","allow":["::1"]}}`, code: "invalid_request"},
		{name: "port out of range", body: `{"egress":{"mode":"allowlist","allow":["10.0.0.1:0"]}}`, code: "invalid_request"},
		{name: "local with open", body: `{"egress":{"mode":"open"}}`, runtime: "local", mode: "open"},
		{name: "local with none", body: `{"egress":{"mode":"none"}}`, runtime: "local", code: "unsupported"},
		{name: "local with allowlist", body: `{"egress":{"mode":"allowlist","allow":["10.0.0.1"]}}`, runtime: "local", code: "unsupported"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var req createSandboxRequest
			dec := json.NewDecoder(strings.NewReader(tt.body))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&req); err != nil {
				t.Fatal(err)
			}
			runtime := tt.runtime
			if runtime == "" {
				runtime = "container"
			}
			mode, allow, code, detail := sandboxEgressMode(req, runtime)
			if code != tt.code {
				t.Fatalf("sandboxEgress problem = %q (%s), want %q", code, detail, tt.code)
			}
			if code != "" {
				return
			}
			if mode != tt.mode || !slices.Equal(allow, tt.allow) {
				t.Fatalf("sandboxEgress = %q %v, want %q %v", mode, allow, tt.mode, tt.allow)
			}
		})
	}
}

// api.md §3: `address` is the Sandbox's current IPv4 address and empty when it
// has none (not running, or reported by an older Agent).
func TestSandboxDTOAddress(t *testing.T) {
	addr := "10.88.0.7"
	for _, tt := range []struct {
		name string
		sb   store.Sandbox
		want string
	}{
		{"running", store.Sandbox{State: "ready", Address: &addr}, `"address":"10.88.0.7"`},
		{"stopped", store.Sandbox{State: "stopped"}, `"address":""`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := json.Marshal((&Server{}).sandboxToDTO(t.Context(), tt.sb))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(raw), tt.want) {
				t.Fatalf("sandbox JSON %s lacks %s", raw, tt.want)
			}
		})
	}
}
