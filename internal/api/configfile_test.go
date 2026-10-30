package api

import (
	"bytes"
	"encoding/base64"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeConfigYAML writes a config file and points ORRERY_CONFIG_FILE at it.
func writeConfigYAML(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ORRERY_CONFIG_FILE", path)
	return path
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestLoadConfigFromYAML(t *testing.T) {
	writeConfigYAML(t, `
database:
  host: postgres
  port: 5433
  user: orrery
  password: secret
  name: orrery
access:
  url: https://cp.example.com:9443
  apps_domain: apps.example.com
  ticket_key: ticket
enrollment_packet_ttl: 2h
quotas: {default_gpu: 2, default_sandbox: 5}
logs: {exec_cap: 1024, exec_retention: 26h, audit_retention: 0,
       resource_version_ttl: 48h, cas_grace: 3h}
lifecycle: {pty_idle_seconds: 30, idle_stop_seconds: 60, admission_timeout: 5m}
builds: {cpu: 2, memory: 1073741824, timeout: 10m}
s3: {secret_key: s3-secret}
`)
	cfg, err := LoadConfig(discardLogger())
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.DatabaseURL != "postgres://orrery:secret@postgres:5433/orrery?sslmode=disable" {
		t.Fatalf("DatabaseURL = %q", cfg.DatabaseURL)
	}
	if cfg.AccessURL != "https://cp.example.com:9443" || cfg.AppsDomain != "apps.example.com" {
		t.Fatalf("access = %q, %q", cfg.AccessURL, cfg.AppsDomain)
	}
	if string(cfg.PortTicketKey) != "ticket" {
		t.Fatalf("PortTicketKey = %q", cfg.PortTicketKey)
	}
	if cfg.EnrollmentPacketTTL != 2*time.Hour || cfg.AdmissionTimeout != 5*time.Minute {
		t.Fatalf("durations = %v, %v", cfg.EnrollmentPacketTTL, cfg.AdmissionTimeout)
	}
	if cfg.DefaultGPUQuota != 2 || cfg.DefaultSandboxQuota != 5 {
		t.Fatalf("quotas = %d, %d", cfg.DefaultGPUQuota, cfg.DefaultSandboxQuota)
	}
	if cfg.ExecLogCap != 1024 || cfg.ExecLogRetention != 26*time.Hour ||
		cfg.AuditRetention != 0 || cfg.ResourceVersionTTL != 48*time.Hour || cfg.CASGrace != 3*time.Hour {
		t.Fatalf("logs = %+v", cfg)
	}
	if cfg.PtyIdleSeconds != 30 || cfg.IdleStopSeconds != 60 {
		t.Fatalf("lifecycle = %d, %d", cfg.PtyIdleSeconds, cfg.IdleStopSeconds)
	}
	if cfg.BuildCPU != 2 || cfg.BuildMemory != 1073741824 || cfg.BuildTimeout != 10*time.Minute {
		t.Fatalf("builds = %v, %d, %v", cfg.BuildCPU, cfg.BuildMemory, cfg.BuildTimeout)
	}
	// Values that never appear in the file keep their code defaults.
	if cfg.Addr != ":8080" || cfg.NodeAddr != ":8081" {
		t.Fatalf("addresses = %q, %q", cfg.Addr, cfg.NodeAddr)
	}
}

func TestLoadConfigYAMLDefaults(t *testing.T) {
	writeConfigYAML(t, `
database: {host: postgres, user: orrery, password: secret, name: orrery}
s3: {secret_key: s3-secret}
`)
	cfg, err := LoadConfig(discardLogger())
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.DatabaseURL != "postgres://orrery:secret@postgres:5432/orrery?sslmode=disable" {
		t.Fatalf("DatabaseURL = %q", cfg.DatabaseURL)
	}
	if cfg.EnrollmentPacketTTL != time.Hour || cfg.AdmissionTimeout != time.Hour ||
		cfg.BuildTimeout != time.Hour || cfg.PtyIdleSeconds != 300 || cfg.IdleStopSeconds != 900 {
		t.Fatalf("defaults = %+v", cfg)
	}
	if cfg.ExecLogCap != 16<<20 || cfg.ExecLogRetention != 720*time.Hour ||
		cfg.ResourceVersionTTL != 720*time.Hour || cfg.CASGrace != 24*time.Hour {
		t.Fatalf("log defaults = %+v", cfg)
	}
	if cfg.BuildCPU != 4 || cfg.BuildMemory != 8<<30 {
		t.Fatalf("build defaults = %v, %d", cfg.BuildCPU, cfg.BuildMemory)
	}
	if cfg.S3.Endpoint != "http://rustfs:9000" || cfg.S3.Bucket != "orrery" ||
		cfg.S3.Region != "us-east-1" || cfg.S3.AccessKey != "orrery" {
		t.Fatalf("s3 defaults = %+v", cfg.S3)
	}
	if cfg.S3.PublicEndpoint != "http://rustfs:9000" {
		t.Fatalf("S3.PublicEndpoint = %q, wants the internal endpoint", cfg.S3.PublicEndpoint)
	}
}

func TestLoadConfigYAMLStrict(t *testing.T) {
	for name, body := range map[string]string{
		"unknown key":        "database: {host: h, user: u, password: p, name: n}\nunknown: true\n",
		"unknown nested key": "database: {host: h, user: u, password: p, name: n, port: 5432, typo: 1}\n",
		"database url with structured fields": `
database: {url: "postgres://x", host: h, user: u, password: p, name: n}
`,
		"missing database host": "database: {user: u, password: p, name: n}\n",
		"missing database user": "database: {host: h, password: p, name: n}\n",
		"bad port":              "database: {host: h, user: u, password: p, name: n, port: 70000}\n",
		"bad duration":          "database: {host: h, user: u, password: p, name: n}\nenrollment_packet_ttl: soon\n",
		"zero duration":         "database: {host: h, user: u, password: p, name: n}\nbuilds: {timeout: 0s}\n",
		"negative retention":    "database: {host: h, user: u, password: p, name: n}\nlogs: {audit_retention: -1h}\n",
		"sub-second retention":  "database: {host: h, user: u, password: p, name: n}\nlogs: {exec_retention: 500ms}\n",
		"negative exec cap":     "database: {host: h, user: u, password: p, name: n}\nlogs: {exec_cap: -1}\n",
		"zero build cpu":        "database: {host: h, user: u, password: p, name: n}\nbuilds: {cpu: 0}\n",
	} {
		t.Run(name, func(t *testing.T) {
			writeConfigYAML(t, body)
			if _, err := LoadConfig(discardLogger()); err == nil {
				t.Fatal("LoadConfig accepted an invalid file")
			}
		})
	}
}

func TestLoadConfigYAMLEnvOverridesScalars(t *testing.T) {
	writeConfigYAML(t, `
database: {host: postgres, user: orrery, password: secret, name: orrery}
access: {url: https://file.example}
enrollment_packet_ttl: 2h
s3: {secret_key: s3-secret}
`)
	t.Setenv("ORRERY_DATABASE_HOST", "db.example")
	t.Setenv("ORRERY_ACCESS_URL", "https://env.example")
	t.Setenv("ORRERY_ENROLLMENT_PACKET_TTL", "3h")
	t.Setenv("ORRERY_API_ADDR", ":9999")

	var log bytes.Buffer
	cfg, err := LoadConfig(slog.New(slog.NewTextHandler(&log, nil)))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !strings.Contains(cfg.DatabaseURL, "@db.example:5432/") {
		t.Fatalf("DatabaseURL = %q", cfg.DatabaseURL)
	}
	if cfg.AccessURL != "https://env.example" || cfg.EnrollmentPacketTTL != 3*time.Hour || cfg.Addr != ":9999" {
		t.Fatalf("overrides = %q, %v, %q", cfg.AccessURL, cfg.EnrollmentPacketTTL, cfg.Addr)
	}
	for _, key := range []string{"database.host", "access.url", "enrollment_packet_ttl", "api_addr"} {
		if !strings.Contains(log.String(), "key="+key) {
			t.Fatalf("startup log does not name overridden key %q: %s", key, log.String())
		}
	}
}

func TestLoadConfigYAMLSecretsAndLists(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	writeConfigYAML(t, `
database: {host: postgres, user: orrery, password: secret, name: orrery}
s3: {secret_key: s3-secret}
encryption_keys:
  - {id: k1, key: `+key+`}
git_providers:
  - {id: gitea, type: gitea, client_id: cid, client_secret: inline-secret,
     base_url: https://git.example.com, scopes: [read:user]}
oidc: {issuer_url: https://idp.test, client_id: oidc-id, client_secret: oidc-secret,
       email_domains: [Example.COM], allow_signups: true}
registry: {addr: cp.example.com:9443, username: orrery, password: registry-secret}
platform_snapshots:
  - {name: default, ref: alpine@`+configTestDigest+`, default: true}
`)
	cfg, err := LoadConfig(discardLogger())
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.EncryptionKeys == nil {
		t.Fatal("EncryptionKeys is nil")
	}
	if _, _, err := cfg.EncryptionKeys.Encrypt([]byte("x"), nil); err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if len(cfg.GitProviders) != 1 || cfg.GitProviders[0].ClientSecret != "inline-secret" ||
		cfg.GitProviders[0].AuthURL != "https://git.example.com/login/oauth/authorize" {
		t.Fatalf("GitProviders = %+v", cfg.GitProviders)
	}
	if cfg.OIDC.ClientSecret != "oidc-secret" || !cfg.OIDC.AllowSignups ||
		len(cfg.OIDC.EmailDomains) != 1 || cfg.OIDC.EmailDomains[0] != "example.com" {
		t.Fatalf("OIDC = %+v", cfg.OIDC)
	}
	if cfg.RegistryAddr != "cp.example.com:9443" || cfg.RegistryPushAddr != "cp.example.com:9443" {
		t.Fatalf("registry = %q, %q", cfg.RegistryAddr, cfg.RegistryPushAddr)
	}
	if cfg.RegistryCredentialDoc == nil || !cfg.RegistryCredentialDoc.Has("cp.example.com:9443") {
		t.Fatal("registry credential does not resolve the registry address")
	}
	if len(cfg.PlatformSnapshots) != 1 || cfg.DefaultSnapshot != "default" {
		t.Fatalf("platform snapshots = %+v, default %q", cfg.PlatformSnapshots, cfg.DefaultSnapshot)
	}
}

func TestLoadConfigYAMLEgressRules(t *testing.T) {
	writeConfigYAML(t, `
database: {host: postgres, user: orrery, password: secret, name: orrery}
s3: {secret_key: s3-secret}
egress:
  allow: ["10.0.0.5/32:443", "10.0.0.2:53"]
  deny: [169.254.169.254/32, 10.0.0.0/8]
`)
	cfg, err := LoadConfig(discardLogger())
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	// Passed through as written (node-protocol.md HelloAck).
	if strings.Join(cfg.EgressAllow, ",") != "10.0.0.5/32:443,10.0.0.2:53" ||
		strings.Join(cfg.EgressDeny, ",") != "169.254.169.254/32,10.0.0.0/8" {
		t.Fatalf("egress = %v, %v", cfg.EgressAllow, cfg.EgressDeny)
	}

	// No entries are required.
	writeConfigYAML(t, "database: {host: postgres, user: orrery, password: secret, name: orrery}\ns3: {secret_key: s}\negress: {}\n")
	if cfg, err := LoadConfig(discardLogger()); err != nil || len(cfg.EgressAllow)+len(cfg.EgressDeny) != 0 {
		t.Fatalf("empty egress: %v, %+v %+v", err, cfg.EgressAllow, cfg.EgressDeny)
	}
}

func TestLoadConfigYAMLRejectsBadLists(t *testing.T) {
	base := "database: {host: h, user: u, password: p, name: n}\ns3: {secret_key: s}\n"
	for name, body := range map[string]string{
		"duplicate provider id": `
git_providers:
  - {id: g, type: github, client_id: c, client_secret: s, scopes: []}
  - {id: g, type: github, client_id: c, client_secret: s, scopes: []}
`,
		"provider without secret": `
git_providers:
  - {id: g, type: github, client_id: c, scopes: []}
`,
		"oidc without secret": `
oidc: {issuer_url: https://idp.test, client_id: c}
`,
		"registry username only": `
registry: {addr: r, username: u}
`,
		"s3 without secret": `
s3: {endpoint: http://s3}
`,
		"duplicate snapshot name": `
platform_snapshots:
  - {name: default, ref: alpine@` + configTestDigest + `}
  - {name: default, ref: busybox@` + configTestDigest + `}
`,
		"two default snapshots": `
platform_snapshots:
  - {name: one, ref: alpine@` + configTestDigest + `, default: true}
  - {name: two, ref: busybox@` + configTestDigest + `, default: true}
`,
		"unpinned snapshot ref": `
platform_snapshots:
  - {name: default, ref: alpine:3.22}
`,
		"egress deny without prefix": "egress: {deny: [example.com]}\n",
		"egress allow bad port":      "egress: {allow: [\"10.0.0.5/32:0\"]}\n",
		"egress IPv6 entry":          "egress: {deny: [\"fd00::/8\"]}\n",
		"egress unknown key":         "egress: {block: [10.0.0.0/8]}\n",
	} {
		t.Run(name, func(t *testing.T) {
			writeConfigYAML(t, base+body)
			if _, err := LoadConfig(discardLogger()); err == nil {
				t.Fatal("LoadConfig accepted an invalid file")
			}
		})
	}
}

func TestLoadConfigYAMLAcceptsDatabaseURLAlone(t *testing.T) {
	writeConfigYAML(t, `
database: {url: "postgres://u:p@db.example:6543/x?sslmode=require"}
s3: {endpoint: ""}
`)
	cfg, err := LoadConfig(discardLogger())
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.DatabaseURL != "postgres://u:p@db.example:6543/x?sslmode=require" {
		t.Fatalf("DatabaseURL = %q", cfg.DatabaseURL)
	}
	// s3.endpoint: "" turns object storage off, so no secret is required.
	if cfg.S3.Endpoint != "" {
		t.Fatalf("S3.Endpoint = %q", cfg.S3.Endpoint)
	}
}

func TestLoadConfigFallsBackToEnvironment(t *testing.T) {
	t.Setenv("ORRERY_CONFIG_FILE", "")
	t.Setenv("ORRERY_DATABASE_URL", "postgres://u:p@db.example:5432/x")
	cfg, err := LoadConfig(discardLogger())
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.DatabaseURL != "postgres://u:p@db.example:5432/x" {
		t.Fatalf("DatabaseURL = %q", cfg.DatabaseURL)
	}
}

func TestLoadConfigYAMLGitProviderScopesDefaultEmpty(t *testing.T) {
	writeConfigYAML(t, `
database: {host: h, user: u, password: p, name: n}
s3: {secret_key: s}
git_providers:
  - {id: g, type: github, client_id: c, client_secret: s}
`)
	cfg, err := LoadConfig(discardLogger())
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.GitProviders) != 1 || cfg.GitProviders[0].Scopes == nil || len(cfg.GitProviders[0].Scopes) != 0 {
		t.Fatalf("GitProviders = %+v", cfg.GitProviders)
	}
}
