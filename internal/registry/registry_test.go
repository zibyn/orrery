package registry

import (
	"encoding/json"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/uuid"
)

const testDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestRepo(t *testing.T) {
	id := uuid.MustParse("018f0000-0000-7000-8000-000000000001")
	for _, tc := range []struct {
		name     string
		id       uuid.UUID
		platform string
		want     string
	}{
		{"user", id, "", "snapshots/" + id.String()},
		{"platform", uuid.Nil, "default", "platform/default"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Repo(tc.id, tc.platform); got != tc.want {
				t.Fatalf("Repo = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRef(t *testing.T) {
	got := Ref("127.0.0.1:15000", "platform/default", testDigest)
	want := "127.0.0.1:15000/platform/default@" + testDigest
	if got != want {
		t.Fatalf("Ref = %q, want %q", got, want)
	}
}

func TestParseSourceRef(t *testing.T) {
	for _, tc := range []struct {
		ref  string
		ok   bool
		want string
	}{
		{testDigest, false, ""},
		{"alpine:3.22", false, ""},
		{"alpine@" + testDigest, true, "alpine@" + testDigest},
		{"ghcr.io/acme/app@" + testDigest, true, "ghcr.io/acme/app@" + testDigest},
		{"alpine@sha256:short", false, ""},
		{"alpine@sha512:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", false, ""},
	} {
		got, err := ParseSourceRef(tc.ref)
		if tc.ok && err != nil {
			t.Fatalf("ParseSourceRef(%q): %v", tc.ref, err)
		}
		if !tc.ok && err == nil {
			t.Fatalf("ParseSourceRef(%q) accepted an unpinned or non-sha256 ref", tc.ref)
		}
		if tc.ok && got.String() != tc.want {
			t.Fatalf("ParseSourceRef(%q) = %q, want %q", tc.ref, got.String(), tc.want)
		}
	}
}

func TestRewriteRegistry(t *testing.T) {
	source, err := ParseSourceRef("127.0.0.1:15000/snapshots/abc@" + testDigest)
	if err != nil {
		t.Fatal(err)
	}
	got, err := RewriteRegistry(source, "127.0.0.1:15000", "172.17.0.1:15000")
	if err != nil {
		t.Fatal(err)
	}
	want := "172.17.0.1:15000/snapshots/abc@" + testDigest
	if got.String() != want {
		t.Fatalf("RewriteRegistry = %q, want %q", got.String(), want)
	}
	// A foreign registry is left alone.
	external, _ := ParseSourceRef("ghcr.io/acme/app@" + testDigest)
	unchanged, err := RewriteRegistry(external, "127.0.0.1:15000", "172.17.0.1:15000")
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.String() != external.String() {
		t.Fatalf("RewriteRegistry changed a foreign ref: %q", unchanged.String())
	}
}

func TestCredentialResolvesPushHost(t *testing.T) {
	raw, err := json.Marshal(map[string]any{
		"auths": map[string]any{
			"127.0.0.1:15000":            map[string]string{"auth": "b3JyZXJ5OnNlY3JldA=="},
			"host.docker.internal:15000": map[string]string{"username": "orrery", "password": "secret"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	cred, err := ParseCredential(string(raw))
	if err != nil {
		t.Fatalf("ParseCredential: %v", err)
	}
	for _, host := range []string{"127.0.0.1:15000", "host.docker.internal:15000"} {
		if !cred.Has(host) {
			t.Fatalf("credential does not resolve %s", host)
		}
		registry, err := name.NewRegistry(host)
		if err != nil {
			t.Fatal(err)
		}
		auth, err := cred.Keychain().Resolve(registry)
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := auth.Authorization()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Username != "orrery" || cfg.Password != "secret" {
			t.Fatalf("%s resolved to %+v", host, cfg)
		}
	}
	if cred.Has("registry.example:5000") {
		t.Fatal("unknown host resolved")
	}
}

func TestCredentialAlias(t *testing.T) {
	cred, err := ParseCredential(`{"auths":{"127.0.0.1:15000":{"auth":"b3JyZXJ5OnNlY3JldA=="}}}`)
	if err != nil {
		t.Fatal(err)
	}
	cred.Alias("172.17.0.1:15000", "127.0.0.1:15000")
	if !cred.Has("172.17.0.1:15000") {
		t.Fatal("alias did not resolve")
	}
}

func TestValidPlatformName(t *testing.T) {
	for _, name := range []string{"default", "alpine-3.22", "team_a", "a"} {
		if !ValidPlatformName(name) {
			t.Fatalf("%q rejected", name)
		}
	}
	for _, name := range []string{"", "Default", "a/b", "../x", "a b", "-a", "a."} {
		if ValidPlatformName(name) {
			t.Fatalf("%q accepted", name)
		}
	}
}
