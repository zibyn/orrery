package api

import (
	"testing"
)

const configTestDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestConfigParsesPlatformSnapshots(t *testing.T) {
	t.Setenv("ORRERY_DATABASE_URL", "postgres://x")
	t.Setenv("ORRERY_PLATFORM_SNAPSHOTS", "default=alpine@"+configTestDigest+", tools=ghcr.io/acme/tools@"+configTestDigest)
	t.Setenv("ORRERY_DEFAULT_SNAPSHOT", "default")
	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if len(cfg.PlatformSnapshots) != 2 {
		t.Fatalf("PlatformSnapshots = %+v", cfg.PlatformSnapshots)
	}
	if cfg.PlatformSnapshots[0].Name != "default" || cfg.PlatformSnapshots[0].Ref != "alpine@"+configTestDigest {
		t.Fatalf("first platform Snapshot = %+v", cfg.PlatformSnapshots[0])
	}
	if cfg.DefaultSnapshot != "default" {
		t.Fatalf("DefaultSnapshot = %q", cfg.DefaultSnapshot)
	}
}

func TestConfigRejectsBadPlatformSnapshots(t *testing.T) {
	for _, tc := range []struct {
		name     string
		platform string
		def      string
	}{
		{"missing equals", "default" + configTestDigest, ""},
		{"tag only", "default=alpine:3.22", ""},
		{"duplicate name", "default=alpine@" + configTestDigest + ",default=busybox@" + configTestDigest, ""},
		{"bad name", "Default=alpine@" + configTestDigest, ""},
		{"default not listed", "default=alpine@" + configTestDigest, "other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ORRERY_DATABASE_URL", "postgres://x")
			t.Setenv("ORRERY_PLATFORM_SNAPSHOTS", tc.platform)
			t.Setenv("ORRERY_DEFAULT_SNAPSHOT", tc.def)
			if _, err := ConfigFromEnv(); err == nil {
				t.Fatal("ConfigFromEnv accepted a bad platform Snapshot configuration")
			}
		})
	}
}

func TestConfigRegistryPushAddress(t *testing.T) {
	t.Setenv("ORRERY_DATABASE_URL", "postgres://x")
	t.Setenv("ORRERY_REGISTRY_ADDR", "127.0.0.1:15000")
	t.Setenv("ORRERY_REGISTRY_PUSH_ADDR", "")
	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RegistryPushAddr != "127.0.0.1:15000" {
		t.Fatalf("RegistryPushAddr = %q, defaults to ORRERY_REGISTRY_ADDR", cfg.RegistryPushAddr)
	}
	// The Node-reachable credential document must also resolve the address the
	// Control Plane pushes to, or the platform import cannot authenticate.
	t.Setenv("ORRERY_REGISTRY_PUSH_ADDR", "host.docker.internal:15000")
	t.Setenv("ORRERY_REGISTRY_CREDENTIAL", `{"auths":{"127.0.0.1:15000":{"auth":"b3JyZXJ5OnNlY3JldA=="}}}`)
	cfg, err = ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RegistryCredentialDoc == nil || !cfg.RegistryCredentialDoc.Has("host.docker.internal:15000") {
		t.Fatal("the credential does not resolve the push address")
	}
}
