package api

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/zibyn/stars-sandbox/internal/keyring"
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
	// SnapshotDigest is the deployment default when a create omits snapshot.
	SnapshotDigest string
	// ExecLogCap bounds an Execution Log (defaults.md, 16 MiB).
	ExecLogCap int64
	// ExecLogRetention keeps an Execution Log this long after its Exec is
	// terminal (defaults.md, 30 days; 0 keeps it forever).
	ExecLogRetention time.Duration
	// AuditRetention keeps Audit Records this long (defaults.md, 0 = forever).
	AuditRetention time.Duration
	// PtyIdleSeconds is the deployment PTY idle reap (defaults.md, 300 s).
	PtyIdleSeconds int64
	// IdleStopSeconds is the deployment default for a Sandbox's Idle Stop
	// (defaults.md, 900 s; 0 disables).
	IdleStopSeconds int32
	// AdmissionTimeout is how long a queued Sandbox waits before it fails
	// with admission_timeout (defaults.md, 1 hour).
	AdmissionTimeout time.Duration
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
	// STARS_URL in every Exec; empty leaves STARS_URL out (defaults.md).
	AccessURL string
	// OIDC is the single external issuer (ADR-0020). An empty IssuerURL turns
	// OIDC off.
	OIDC OIDCConfig
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
		DatabaseURL:    os.Getenv("STARS_DATABASE_URL"),
		Addr:           envOr("STARS_API_ADDR", ":8080"),
		NodeAddr:       envOr("STARS_NODE_ADDR", ":8081"),
		NodeTLSCert:    os.Getenv("STARS_NODE_TLS_CERT"),
		NodeTLSKey:     os.Getenv("STARS_NODE_TLS_KEY"),
		SnapshotDigest: os.Getenv("STARS_DEFAULT_SNAPSHOT_DIGEST"),
		AppsDomain:     os.Getenv("STARS_APPS_DOMAIN"),
		PortTicketKey:  []byte(os.Getenv("STARS_PORT_TICKET_KEY")),
		AccessURL:      os.Getenv("STARS_ACCESS_URL"),
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
	if cfg.PtyIdleSeconds, err = envInt64("STARS_PTY_IDLE_SECONDS", 300); err != nil {
		return Config{}, err
	}
	if cfg.IdleStopSeconds, err = envInt32("STARS_IDLE_STOP_SECONDS", 900); err != nil {
		return Config{}, err
	}
	if cfg.AdmissionTimeout, err = envDuration("STARS_ADMISSION_TIMEOUT", time.Hour); err != nil {
		return Config{}, err
	}
	if err := cfg.loadOIDC(); err != nil {
		return Config{}, err
	}
	if path := os.Getenv("STARS_ENCRYPTION_KEYS_FILE"); path != "" {
		if cfg.EncryptionKeys, err = keyring.LoadFile(path); err != nil {
			return Config{}, err
		}
	}
	return cfg, nil
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
