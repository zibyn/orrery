package api

import (
	"fmt"
	"os"
	"strconv"
	"time"
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
	// PtyIdleSeconds is the deployment PTY idle reap (defaults.md, 300 s).
	PtyIdleSeconds int64
	// AppsDomain is the wildcard domain port forwarding serves (api.md §6).
	// Empty means the Gateway is off; the CLI tunnel still works.
	AppsDomain string
	// PortTicketKey signs port-forward tickets and Gateway cookies. Empty
	// means a per-process random key, which only invalidates live cookies.
	PortTicketKey []byte
}

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
	if cfg.PtyIdleSeconds, err = envInt64("STARS_PTY_IDLE_SECONDS", 300); err != nil {
		return Config{}, err
	}
	return cfg, nil
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
