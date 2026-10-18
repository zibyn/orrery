package api

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/zibyn/stars-sandbox/internal/keyring"
	"github.com/zibyn/stars-sandbox/internal/registry"
)

// Config is the deployment configuration. Values that defaults.md marks as
// "deployment" live here; constants live in code.
type Config struct {
	DatabaseURL         string
	Addr                string
	NodeAddr            string
	NodeTLSCert         string
	NodeTLSKey          string
	EnrollmentPacketTTL time.Duration
	DefaultGPUQuota     int32
	DefaultSandboxQuota int32
	// PlatformSnapshots are the deployment's platform images (defaults.md,
	// STARS_PLATFORM_SNAPSHOTS). The Control Plane imports them at startup.
	PlatformSnapshots []PlatformSnapshot
	// DefaultSnapshot is the platform Snapshot name used when a Sandbox
	// create omits snapshot (defaults.md, STARS_DEFAULT_SNAPSHOT).
	DefaultSnapshot string
	// ExecLogCap bounds an Execution Log (defaults.md, 16 MiB).
	ExecLogCap int64
	// ExecLogRetention keeps an Execution Log this long after its Exec is
	// terminal (defaults.md, 30 days; 0 keeps it forever).
	ExecLogRetention time.Duration
	// AuditRetention keeps Audit Records this long (defaults.md, 0 = forever).
	AuditRetention time.Duration
	// ResourceVersionTTL expires a non-current, unreferenced ResourceVersion
	// this long after creation (defaults.md, 30 days; 0 keeps it forever).
	ResourceVersionTTL time.Duration
	// CASGrace keeps an unreferenced CAS object this long after upload
	// (defaults.md, 24 hours; 0 disables the sweep).
	CASGrace time.Duration
	// PurgeInterval is the periodic cleanup cadence. Production always uses the
	// 10-minute constant (defaults.md); STARS_TEST_PURGE_INTERVAL is an
	// explicit test hook so e2e can observe a sweep. It is not a deployment
	// knob.
	PurgeInterval time.Duration
	// PtyIdleSeconds is the deployment PTY idle reap (defaults.md, 300 s).
	PtyIdleSeconds int64
	// IdleStopSeconds is the deployment default for a Sandbox's Idle Stop
	// (defaults.md, 900 s; 0 disables).
	IdleStopSeconds int32
	// AdmissionTimeout is how long a queued Sandbox waits before it fails
	// with admission_timeout (defaults.md, 1 hour).
	AdmissionTimeout time.Duration
	// BuildCPU and BuildMemory are a Snapshot build's resource ceiling
	// (defaults.md, deployment; 4 CPU + 8 GiB). Builds do not count toward
	// User Quota (ADR-0021).
	BuildCPU    float64
	BuildMemory int64
	// BuildTimeout bounds a Snapshot build from admission to completion
	// (defaults.md, deployment; 1 hour).
	BuildTimeout time.Duration
	// AppsDomain is the wildcard domain port forwarding serves (api.md §6).
	// Empty means the Gateway is off; the CLI tunnel still works.
	AppsDomain string
	// PortTicketKey signs port-forward tickets and Gateway cookies. Empty
	// means a per-process random key, which only invalidates live cookies.
	PortTicketKey []byte
	// EncryptionKeys encrypts Secrets, Git tokens and Sandbox Tokens. Nil
	// means those features answer with an explicit error (defaults.md).
	EncryptionKeys *keyring.Keyring
	// AccessURL is the externally reachable `api` address. It becomes
	// STARS_URL in every Exec and the base of Git OAuth URLs; empty turns Git
	// account linking off (defaults.md).
	AccessURL string
	// GitProviders is the deployment's OAuth applications (ADR-0013).
	GitProviders []GitProvider
	// OIDC is the single external issuer (ADR-0020). An empty IssuerURL turns
	// OIDC off.
	OIDC OIDCConfig
	// S3 is the object store. An empty Endpoint turns object storage off, and
	// Presign then answers with an error.
	S3 S3Config
	// RegistryAddr is the Node-reachable address of the platform OCI registry
	// (ADR-0014), e.g. "127.0.0.1:15000". Empty means no registry is deployed.
	RegistryAddr string
	// RegistryPushAddr is the address the Control Plane pushes imports to.
	// It differs from RegistryAddr inside a container, where 127.0.0.1 is the
	// Control Plane itself. Empty means RegistryAddr.
	RegistryPushAddr string
	// RegistryCredential is the Docker config.json auth document handed to
	// Nodes in HelloAck, e.g. {"auths":{"host":{"auth":"base64(user:pass)"}}}.
	RegistryCredential string
	// RegistryCredentialDoc is the parsed credential. It must resolve
	// RegistryPushAddr as well as RegistryAddr (see ConfigFromEnv).
	RegistryCredentialDoc *registry.Credential
}

// PlatformSnapshot is one STARS_PLATFORM_SNAPSHOTS entry.
type PlatformSnapshot struct {
	Name string
	Ref  string // ref@sha256:…
}

// S3Config is the S3-compatible object store configuration (defaults.md).
type S3Config struct {
	// Endpoint is what the Control Plane reaches, e.g. http://rustfs:9000.
	Endpoint string
	// PublicEndpoint is what Nodes reach; presigned URLs use it. Empty means
	// Endpoint.
	PublicEndpoint string
	Bucket         string
	// Region defaults to us-east-1.
	Region    string
	AccessKey string
	SecretKey string
}

type OIDCConfig struct {
	IssuerURL    string
	ClientID     string
	ClientSecret string
	// AllowSignups defaults to false; the first User bypasses it.
	AllowSignups bool
	// EmailDomains, when non-empty, restricts logins to those email domains.
	EmailDomains []string
}

func (c OIDCConfig) Enabled() bool { return c.IssuerURL != "" }

func ConfigFromEnv() (Config, error) {
	cfg := Config{
		DatabaseURL:   os.Getenv("STARS_DATABASE_URL"),
		Addr:          envOr("STARS_API_ADDR", ":8080"),
		NodeAddr:      envOr("STARS_NODE_ADDR", ":8081"),
		NodeTLSCert:   os.Getenv("STARS_NODE_TLS_CERT"),
		NodeTLSKey:    os.Getenv("STARS_NODE_TLS_KEY"),
		AppsDomain:    os.Getenv("STARS_APPS_DOMAIN"),
		PortTicketKey: []byte(os.Getenv("STARS_PORT_TICKET_KEY")),
		AccessURL:     os.Getenv("STARS_ACCESS_URL"),
	}
	if cfg.DatabaseURL == "" {
		return Config{}, fmt.Errorf("STARS_DATABASE_URL is required")
	}
	var err error
	if cfg.EnrollmentPacketTTL, err = envDuration("STARS_ENROLLMENT_PACKET_TTL", time.Hour); err != nil {
		return Config{}, err
	}
	if cfg.DefaultGPUQuota, err = envInt32("STARS_DEFAULT_GPU_QUOTA", 0); err != nil {
		return Config{}, err
	}
	if cfg.DefaultSandboxQuota, err = envInt32("STARS_DEFAULT_SANDBOX_QUOTA", 0); err != nil {
		return Config{}, err
	}
	if cfg.ExecLogCap, err = envInt64("STARS_EXEC_LOG_CAP", 16<<20); err != nil {
		return Config{}, err
	}
	if cfg.ExecLogRetention, err = envRetention("STARS_EXEC_LOG_RETENTION", 30*24*time.Hour); err != nil {
		return Config{}, err
	}
	if cfg.AuditRetention, err = envRetention("STARS_AUDIT_RETENTION", 0); err != nil {
		return Config{}, err
	}
	if cfg.ResourceVersionTTL, err = envRetention("STARS_RESOURCE_VERSION_TTL", 30*24*time.Hour); err != nil {
		return Config{}, err
	}
	if cfg.CASGrace, err = envRetention("STARS_CAS_GRACE", 24*time.Hour); err != nil {
		return Config{}, err
	}
	// Test hook only: production uses the 10-minute constant (defaults.md).
	if cfg.PurgeInterval, err = envDuration("STARS_TEST_PURGE_INTERVAL", purgeInterval); err != nil {
		return Config{}, err
	}
	if cfg.PtyIdleSeconds, err = envInt64("STARS_PTY_IDLE_SECONDS", 300); err != nil {
		return Config{}, err
	}
	if cfg.IdleStopSeconds, err = envInt32("STARS_IDLE_STOP_SECONDS", 900); err != nil {
		return Config{}, err
	}
	if cfg.AdmissionTimeout, err = envDuration("STARS_ADMISSION_TIMEOUT", time.Hour); err != nil {
		return Config{}, err
	}
	if cfg.BuildCPU, err = envFloat("STARS_BUILD_CPU", 4); err != nil {
		return Config{}, err
	}
	if cfg.BuildMemory, err = envInt64("STARS_BUILD_MEMORY", 8<<30); err != nil {
		return Config{}, err
	}
	if cfg.BuildTimeout, err = envDuration("STARS_BUILD_TIMEOUT", time.Hour); err != nil {
		return Config{}, err
	}
	if err := cfg.loadOIDC(); err != nil {
		return Config{}, err
	}
	if path := os.Getenv("STARS_GIT_PROVIDERS_FILE"); path != "" {
		if cfg.GitProviders, err = loadGitProviders(path); err != nil {
			return Config{}, err
		}
	}
	if path := os.Getenv("STARS_ENCRYPTION_KEYS_FILE"); path != "" {
		if cfg.EncryptionKeys, err = keyring.LoadFile(path); err != nil {
			return Config{}, err
		}
	}
	cfg.S3 = S3Config{
		Endpoint:       os.Getenv("STARS_S3_ENDPOINT"),
		PublicEndpoint: os.Getenv("STARS_S3_PUBLIC_ENDPOINT"),
		Bucket:         os.Getenv("STARS_S3_BUCKET"),
		Region:         os.Getenv("STARS_S3_REGION"),
		AccessKey:      os.Getenv("STARS_S3_ACCESS_KEY"),
		SecretKey:      os.Getenv("STARS_S3_SECRET_KEY"),
	}
	if cfg.S3.Endpoint != "" || cfg.S3.Bucket != "" {
		if cfg.S3.Endpoint == "" || cfg.S3.Bucket == "" || cfg.S3.AccessKey == "" || cfg.S3.SecretKey == "" {
			return Config{}, fmt.Errorf("STARS_S3_ENDPOINT, STARS_S3_BUCKET, STARS_S3_ACCESS_KEY and STARS_S3_SECRET_KEY are required together")
		}
	}
	cfg.RegistryAddr = os.Getenv("STARS_REGISTRY_ADDR")
	cfg.RegistryPushAddr = envOr("STARS_REGISTRY_PUSH_ADDR", cfg.RegistryAddr)
	cfg.RegistryCredential = os.Getenv("STARS_REGISTRY_CREDENTIAL")
	if cfg.RegistryCredentialDoc, err = registry.ParseCredential(cfg.RegistryCredential); err != nil {
		return Config{}, err
	}
	// A single credential document serves Nodes and the Control Plane. When
	// the deployment lists only the Node-reachable host, let the push host
	// reuse that entry instead of failing the platform import at startup.
	cfg.RegistryCredentialDoc.Alias(cfg.RegistryPushAddr, cfg.RegistryAddr)
	if cfg.PlatformSnapshots, err = parsePlatformSnapshots(os.Getenv("STARS_PLATFORM_SNAPSHOTS")); err != nil {
		return Config{}, err
	}
	cfg.DefaultSnapshot = os.Getenv("STARS_DEFAULT_SNAPSHOT")
	if cfg.DefaultSnapshot != "" {
		found := false
		for _, snapshot := range cfg.PlatformSnapshots {
			if snapshot.Name == cfg.DefaultSnapshot {
				found = true
				break
			}
		}
		if !found {
			return Config{}, fmt.Errorf("STARS_DEFAULT_SNAPSHOT must name an entry of STARS_PLATFORM_SNAPSHOTS")
		}
	}
	return cfg, nil
}

// parsePlatformSnapshots reads `name=ref@sha256:…,…` (defaults.md). Names and
// refs are validated here so a broken deployment fails at startup rather than
// at the first Sandbox create.
func parsePlatformSnapshots(raw string) ([]PlatformSnapshot, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	seen := map[string]bool{}
	var snapshots []PlatformSnapshot
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		name, ref, ok := strings.Cut(entry, "=")
		if !ok || name == "" || ref == "" {
			return nil, fmt.Errorf("STARS_PLATFORM_SNAPSHOTS entries are name=ref@sha256:…, got %q", entry)
		}
		if !registry.ValidPlatformName(name) {
			return nil, fmt.Errorf("platform Snapshot name %q must be lowercase alphanumeric with . _ - separators", name)
		}
		if seen[name] {
			return nil, fmt.Errorf("platform Snapshot name %q is listed twice", name)
		}
		seen[name] = true
		if _, err := registry.ParseSourceRef(ref); err != nil {
			return nil, fmt.Errorf("platform Snapshot %q: %w", name, err)
		}
		snapshots = append(snapshots, PlatformSnapshot{Name: name, Ref: ref})
	}
	return snapshots, nil
}

// loadOIDC reads the deployment's one external issuer (defaults.md identity
// section). A configured issuer needs a client id and a secret file.
func (c *Config) loadOIDC() error {
	c.OIDC.IssuerURL = os.Getenv("STARS_OIDC_ISSUER_URL")
	c.OIDC.ClientID = os.Getenv("STARS_OIDC_CLIENT_ID")
	if !c.OIDC.Enabled() {
		return nil
	}
	if c.OIDC.ClientID == "" {
		return fmt.Errorf("STARS_OIDC_CLIENT_ID is required when STARS_OIDC_ISSUER_URL is set")
	}
	path := os.Getenv("STARS_OIDC_CLIENT_SECRET_FILE")
	if path == "" {
		return fmt.Errorf("STARS_OIDC_CLIENT_SECRET_FILE is required when STARS_OIDC_ISSUER_URL is set")
	}
	secret, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read OIDC client secret: %w", err)
	}
	c.OIDC.ClientSecret = strings.TrimSpace(string(secret))
	for _, domain := range strings.Split(os.Getenv("STARS_OIDC_EMAIL_DOMAINS"), ",") {
		if domain = strings.ToLower(strings.TrimSpace(domain)); domain != "" {
			c.OIDC.EmailDomains = append(c.OIDC.EmailDomains, domain)
		}
	}
	if raw := os.Getenv("STARS_OIDC_ALLOW_SIGNUPS"); raw != "" {
		allow, err := strconv.ParseBool(raw)
		if err != nil {
			return fmt.Errorf("STARS_OIDC_ALLOW_SIGNUPS must be true or false")
		}
		c.OIDC.AllowSignups = allow
	}
	return nil
}

func envDuration(key string, fallback time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", key)
	}
	return d, nil
}

// envRetention reads a duration where 0 means "keep forever" (defaults.md,
// purge configuration). Retention is applied in whole seconds, so a positive
// sub-second value would truncate to 0 and delete everything.
func envRetention(key string, fallback time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 || (d > 0 && d < time.Second) {
		return 0, fmt.Errorf("%s must be 0 or a duration of at least 1s", key)
	}
	return d, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt32(key string, fallback int32) (int32, error) {
	n, err := envInt(key, int64(fallback), 32, 0, "non-negative")
	return int32(n), err
}

func envInt64(key string, fallback int64) (int64, error) {
	return envInt(key, fallback, 64, 1, "positive")
}

// envFloat reads a positive floating-point value (a build's CPU ceiling).
func envFloat(key string, fallback float64) (float64, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f <= 0 {
		return 0, fmt.Errorf("%s must be a positive number", key)
	}
	return f, nil
}

// envInt reads a signed integer of the given bit width, rejecting values below
// min; kind names the lower bound in the error.
func envInt(key string, fallback int64, bits int, min int64, kind string) (int64, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.ParseInt(v, 10, bits)
	if err != nil || n < min {
		return 0, fmt.Errorf("%s must be a %s integer", key, kind)
	}
	return n, nil
}
