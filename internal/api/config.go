package api

import (
	"fmt"
	"os"
	"strconv"
)

// Config is the deployment configuration. Values that defaults.md marks as
// "deployment" live here; constants live in code.
type Config struct {
	DatabaseURL         string
	Addr                string
	DefaultGPUQuota     int32
	DefaultSandboxQuota int32
}

func ConfigFromEnv() (Config, error) {
	cfg := Config{
		DatabaseURL: os.Getenv("STARS_DATABASE_URL"),
		Addr:        envOr("STARS_API_ADDR", ":8080"),
	}
	if cfg.DatabaseURL == "" {
		return Config{}, fmt.Errorf("STARS_DATABASE_URL is required")
	}
	var err error
	if cfg.DefaultGPUQuota, err = envInt32("STARS_DEFAULT_GPU_QUOTA", 0); err != nil {
		return Config{}, err
	}
	if cfg.DefaultSandboxQuota, err = envInt32("STARS_DEFAULT_SANDBOX_QUOTA", 0); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt32(key string, fallback int32) (int32, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.ParseInt(v, 10, 32)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer", key)
	}
	return int32(n), nil
}
