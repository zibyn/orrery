package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/zibyn/stars-sandbox/internal/keyring"
	"github.com/zibyn/stars-sandbox/internal/registry"
	"go.yaml.in/yaml/v3"
)

// fileConfig mirrors config.yaml (defaults.md, "部署配置"). Fields start at
// their defaults, so a decoded file only has to name what it changes.
type fileConfig struct {
	Access struct {
		URL        string `yaml:"url"`
		AppsDomain string `yaml:"apps_domain"`
		TicketKey  string `yaml:"ticket_key"`
	} `yaml:"access"`
	Database struct {
		URL      string `yaml:"url"`
		Host     string `yaml:"host"`
		Port     *int   `yaml:"port"`
		User     string `yaml:"user"`
		Password string `yaml:"password"`
		Name     string `yaml:"name"`
	} `yaml:"database"`
	EnrollmentPacketTTL string `yaml:"enrollment_packet_ttl"`
	Quotas              struct {
		DefaultGPU     int32 `yaml:"default_gpu"`
		DefaultSandbox int32 `yaml:"default_sandbox"`
	} `yaml:"quotas"`
	Lifecycle struct {
		PtyIdleSeconds   int64  `yaml:"pty_idle_seconds"`
		IdleStopSeconds  int32  `yaml:"idle_stop_seconds"`
		AdmissionTimeout string `yaml:"admission_timeout"`
	} `yaml:"lifecycle"`
	Builds struct {
		CPU     float64 `yaml:"cpu"`
		Memory  int64   `yaml:"memory"`
		Timeout string  `yaml:"timeout"`
	} `yaml:"builds"`
	Logs struct {
		ExecCap            int64  `yaml:"exec_cap"`
		ExecRetention      string `yaml:"exec_retention"`
		AuditRetention     string `yaml:"audit_retention"`
		ResourceVersionTTL string `yaml:"resource_version_ttl"`
		CASGrace           string `yaml:"cas_grace"`
	} `yaml:"logs"`
	EncryptionKeys []encryptionKeyFile `yaml:"encryption_keys"`
	GitProviders   []gitProviderYAML   `yaml:"git_providers"`
	OIDC           struct {
		IssuerURL    string   `yaml:"issuer_url"`
		ClientID     string   `yaml:"client_id"`
		ClientSecret string   `yaml:"client_secret"`
		EmailDomains []string `yaml:"email_domains"`
		AllowSignups bool     `yaml:"allow_signups"`
	} `yaml:"oidc"`
	S3 struct {
		Endpoint       string `yaml:"endpoint"`
		PublicEndpoint string `yaml:"public_endpoint"`
		Bucket         string `yaml:"bucket"`
		Region         string `yaml:"region"`
		AccessKey      string `yaml:"access_key"`
		SecretKey      string `yaml:"secret_key"`
	} `yaml:"s3"`
	Registry struct {
		Addr     string `yaml:"addr"`
		PushAddr string `yaml:"push_addr"`
		Username string `yaml:"username"`
		Password string `yaml:"password"`
	} `yaml:"registry"`
	PlatformSnapshots []platformSnapshotFile `yaml:"platform_snapshots"`
}

type encryptionKeyFile struct {
	ID  string `yaml:"id"`
	Key string `yaml:"key"`
}

// gitProviderYAML is one config.yaml git_providers entry; unlike the
// environment-mode JSON file, the client secret is inline (ADR-0024).
type gitProviderYAML struct {
	ID           string    `yaml:"id"`
	Type         string    `yaml:"type"`
	ClientID     string    `yaml:"client_id"`
	ClientSecret string    `yaml:"client_secret"`
	BaseURL      string    `yaml:"base_url"`
	AuthURL      string    `yaml:"auth_url"`
	TokenURL     string    `yaml:"token_url"`
	ValidateURL  string    `yaml:"validate_url"`
	PKCE         *bool     `yaml:"pkce"`
	Host         string    `yaml:"host"`
	Scopes       *[]string `yaml:"scopes"`
}

type platformSnapshotFile struct {
	Name    string `yaml:"name"`
	Ref     string `yaml:"ref"`
	Default bool   `yaml:"default"`
}

func defaultFileConfig() *fileConfig {
	f := &fileConfig{}
	f.EnrollmentPacketTTL = "1h"
	f.Lifecycle.PtyIdleSeconds = 300
	f.Lifecycle.IdleStopSeconds = 900
	f.Lifecycle.AdmissionTimeout = "1h"
	f.Builds.CPU = 4
	f.Builds.Memory = 8 << 30
	f.Builds.Timeout = "1h"
	f.Logs.ExecCap = 16 << 20
	f.Logs.ExecRetention = "720h"
	f.Logs.AuditRetention = "0"
	f.Logs.ResourceVersionTTL = "720h"
	f.Logs.CASGrace = "24h"
	f.S3.Endpoint = "http://rustfs:9000"
	f.S3.Bucket = "stars"
	f.S3.Region = "us-east-1"
	f.S3.AccessKey = "stars"
	return f
}

// LoadConfig reads STARS_CONFIG_FILE, applies environment scalar overrides and
// returns the deployment configuration (defaults.md). Without the variable it
// keeps the environment-only path used by tests and CI.
func LoadConfig(logger *slog.Logger) (Config, error) {
	path := os.Getenv("STARS_CONFIG_FILE")
	if path == "" {
		return ConfigFromEnv()
	}
	if logger == nil {
		logger = slog.Default()
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config file: %w", err)
	}
	f := defaultFileConfig()
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(f); err != nil && !errors.Is(err, io.EOF) {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return Config{}, fmt.Errorf("%s: %w", path, err)
		}
		return Config{}, fmt.Errorf("%s: must contain a single YAML document", path)
	}
	if err := (envOverride{log: logger}).apply(f); err != nil {
		return Config{}, err
	}
	return f.toConfig(logger)
}

// envOverride applies the documented scalar override variables. Lists and maps
// are file-only (ADR-0024).
type envOverride struct{ log *slog.Logger }

// get returns an override value and logs the key it replaced.
func (o envOverride) get(key, env string) (string, bool) {
	v := os.Getenv(env)
	if v == "" {
		return "", false
	}
	o.note(key, env)
	return v, true
}

// note logs key as overridden. get uses it for every scalar, and toConfig
// uses it for the keys that have no config.yaml form at all.
func (o envOverride) note(key, env string) {
	if os.Getenv(env) != "" {
		o.log.Info("config overridden by environment", "key", key, "env", env)
	}
}

func (o envOverride) apply(f *fileConfig) error {
	if v, ok := o.get("access.url", "STARS_ACCESS_URL"); ok {
		f.Access.URL = v
	}
	if v, ok := o.get("access.apps_domain", "STARS_APPS_DOMAIN"); ok {
		f.Access.AppsDomain = v
	}
	if v, ok := o.get("access.ticket_key", "STARS_PORT_TICKET_KEY"); ok {
		f.Access.TicketKey = v
	}
	if v, ok := o.get("database.url", "STARS_DATABASE_URL"); ok {
		f.Database.URL = v
	}
	if v, ok := o.get("database.host", "STARS_DATABASE_HOST"); ok {
		f.Database.Host = v
	}
	if v, ok := o.get("database.port", "STARS_DATABASE_PORT"); ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("STARS_DATABASE_PORT must be an integer")
		}
		f.Database.Port = &n
	}
	if v, ok := o.get("database.user", "STARS_DATABASE_USER"); ok {
		f.Database.User = v
	}
	if v, ok := o.get("database.password", "STARS_DATABASE_PASSWORD"); ok {
		f.Database.Password = v
	}
	if v, ok := o.get("database.name", "STARS_DATABASE_NAME"); ok {
		f.Database.Name = v
	}
	if v, ok := o.get("enrollment_packet_ttl", "STARS_ENROLLMENT_PACKET_TTL"); ok {
		f.EnrollmentPacketTTL = v
	}
	if v, ok := o.get("quotas.default_gpu", "STARS_DEFAULT_GPU_QUOTA"); ok {
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil {
			return fmt.Errorf("STARS_DEFAULT_GPU_QUOTA must be an integer")
		}
		f.Quotas.DefaultGPU = int32(n)
	}
	if v, ok := o.get("quotas.default_sandbox", "STARS_DEFAULT_SANDBOX_QUOTA"); ok {
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil {
			return fmt.Errorf("STARS_DEFAULT_SANDBOX_QUOTA must be an integer")
		}
		f.Quotas.DefaultSandbox = int32(n)
	}
	if v, ok := o.get("lifecycle.pty_idle_seconds", "STARS_PTY_IDLE_SECONDS"); ok {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return fmt.Errorf("STARS_PTY_IDLE_SECONDS must be an integer")
		}
		f.Lifecycle.PtyIdleSeconds = n
	}
	if v, ok := o.get("lifecycle.idle_stop_seconds", "STARS_IDLE_STOP_SECONDS"); ok {
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil {
			return fmt.Errorf("STARS_IDLE_STOP_SECONDS must be an integer")
		}
		f.Lifecycle.IdleStopSeconds = int32(n)
	}
	if v, ok := o.get("lifecycle.admission_timeout", "STARS_ADMISSION_TIMEOUT"); ok {
		f.Lifecycle.AdmissionTimeout = v
	}
	if v, ok := o.get("builds.cpu", "STARS_BUILD_CPU"); ok {
		n, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return fmt.Errorf("STARS_BUILD_CPU must be a number")
		}
		f.Builds.CPU = n
	}
	if v, ok := o.get("builds.memory", "STARS_BUILD_MEMORY"); ok {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return fmt.Errorf("STARS_BUILD_MEMORY must be an integer")
		}
		f.Builds.Memory = n
	}
	if v, ok := o.get("builds.timeout", "STARS_BUILD_TIMEOUT"); ok {
		f.Builds.Timeout = v
	}
	if v, ok := o.get("logs.exec_cap", "STARS_EXEC_LOG_CAP"); ok {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return fmt.Errorf("STARS_EXEC_LOG_CAP must be an integer")
		}
		f.Logs.ExecCap = n
	}
	if v, ok := o.get("logs.exec_retention", "STARS_EXEC_LOG_RETENTION"); ok {
		f.Logs.ExecRetention = v
	}
	if v, ok := o.get("logs.audit_retention", "STARS_AUDIT_RETENTION"); ok {
		f.Logs.AuditRetention = v
	}
	if v, ok := o.get("logs.resource_version_ttl", "STARS_RESOURCE_VERSION_TTL"); ok {
		f.Logs.ResourceVersionTTL = v
	}
	if v, ok := o.get("logs.cas_grace", "STARS_CAS_GRACE"); ok {
		f.Logs.CASGrace = v
	}
	if v, ok := o.get("oidc.issuer_url", "STARS_OIDC_ISSUER_URL"); ok {
		f.OIDC.IssuerURL = v
	}
	if v, ok := o.get("oidc.client_id", "STARS_OIDC_CLIENT_ID"); ok {
		f.OIDC.ClientID = v
	}
	if v, ok := o.get("oidc.client_secret", "STARS_OIDC_CLIENT_SECRET"); ok {
		f.OIDC.ClientSecret = v
	}
	if v, ok := o.get("oidc.allow_signups", "STARS_OIDC_ALLOW_SIGNUPS"); ok {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("STARS_OIDC_ALLOW_SIGNUPS must be true or false")
		}
		f.OIDC.AllowSignups = b
	}
	if v, ok := o.get("s3.endpoint", "STARS_S3_ENDPOINT"); ok {
		f.S3.Endpoint = v
	}
	if v, ok := o.get("s3.public_endpoint", "STARS_S3_PUBLIC_ENDPOINT"); ok {
		f.S3.PublicEndpoint = v
	}
	if v, ok := o.get("s3.bucket", "STARS_S3_BUCKET"); ok {
		f.S3.Bucket = v
	}
	if v, ok := o.get("s3.region", "STARS_S3_REGION"); ok {
		f.S3.Region = v
	}
	if v, ok := o.get("s3.access_key", "STARS_S3_ACCESS_KEY"); ok {
		f.S3.AccessKey = v
	}
	if v, ok := o.get("s3.secret_key", "STARS_S3_SECRET_KEY"); ok {
		f.S3.SecretKey = v
	}
	if v, ok := o.get("registry.addr", "STARS_REGISTRY_ADDR"); ok {
		f.Registry.Addr = v
	}
	if v, ok := o.get("registry.push_addr", "STARS_REGISTRY_PUSH_ADDR"); ok {
		f.Registry.PushAddr = v
	}
	if v, ok := o.get("registry.username", "STARS_REGISTRY_USERNAME"); ok {
		f.Registry.Username = v
	}
	if v, ok := o.get("registry.password", "STARS_REGISTRY_PASSWORD"); ok {
		f.Registry.Password = v
	}
	return nil
}

// toConfig validates the merged file and turns it into a Config.
func (f *fileConfig) toConfig(logger *slog.Logger) (Config, error) {
	cfg := Config{
		Addr:          envOr("STARS_API_ADDR", ":8080"),
		NodeAddr:      envOr("STARS_NODE_ADDR", ":8081"),
		NodeTLSCert:   os.Getenv("STARS_NODE_TLS_CERT"),
		NodeTLSKey:    os.Getenv("STARS_NODE_TLS_KEY"),
		AppsDomain:    f.Access.AppsDomain,
		AccessURL:     f.Access.URL,
		PortTicketKey: []byte(f.Access.TicketKey),
	}
	o := envOverride{log: logger}
	// These keys have no config.yaml form (defaults.md).
	for _, key := range [][2]string{
		{"api_addr", "STARS_API_ADDR"},
		{"node_addr", "STARS_NODE_ADDR"},
		{"node_tls_cert", "STARS_NODE_TLS_CERT"},
		{"node_tls_key", "STARS_NODE_TLS_KEY"},
	} {
		o.note(key[0], key[1])
	}
	databaseURL, err := f.databaseURL()
	if err != nil {
		return Config{}, err
	}
	cfg.DatabaseURL = databaseURL
	cfg.DefaultGPUQuota = f.Quotas.DefaultGPU
	cfg.DefaultSandboxQuota = f.Quotas.DefaultSandbox
	cfg.PtyIdleSeconds = f.Lifecycle.PtyIdleSeconds
	cfg.IdleStopSeconds = f.Lifecycle.IdleStopSeconds
	cfg.BuildCPU = f.Builds.CPU
	cfg.BuildMemory = f.Builds.Memory
	cfg.ExecLogCap = f.Logs.ExecCap
	if cfg.EnrollmentPacketTTL, err = positiveDuration("enrollment_packet_ttl", f.EnrollmentPacketTTL); err != nil {
		return Config{}, err
	}
	if cfg.AdmissionTimeout, err = positiveDuration("lifecycle.admission_timeout", f.Lifecycle.AdmissionTimeout); err != nil {
		return Config{}, err
	}
	if cfg.BuildTimeout, err = positiveDuration("builds.timeout", f.Builds.Timeout); err != nil {
		return Config{}, err
	}
	if cfg.ExecLogRetention, err = retention("logs.exec_retention", f.Logs.ExecRetention); err != nil {
		return Config{}, err
	}
	if cfg.AuditRetention, err = retention("logs.audit_retention", f.Logs.AuditRetention); err != nil {
		return Config{}, err
	}
	if cfg.ResourceVersionTTL, err = retention("logs.resource_version_ttl", f.Logs.ResourceVersionTTL); err != nil {
		return Config{}, err
	}
	if cfg.CASGrace, err = retention("logs.cas_grace", f.Logs.CASGrace); err != nil {
		return Config{}, err
	}
	// Test hook only: production uses the 10-minute constant (defaults.md).
	if cfg.PurgeInterval, err = envDuration("STARS_TEST_PURGE_INTERVAL", purgeInterval); err != nil {
		return Config{}, err
	}
	if err := f.validateScalars(); err != nil {
		return Config{}, err
	}
	if cfg.EncryptionKeys, err = f.keyring(); err != nil {
		return Config{}, err
	}
	if cfg.GitProviders, err = f.gitProviders(); err != nil {
		return Config{}, err
	}
	if err := f.oidc(&cfg); err != nil {
		return Config{}, err
	}
	if err := f.s3(&cfg); err != nil {
		return Config{}, err
	}
	if err := f.registryCredential(&cfg); err != nil {
		return Config{}, err
	}
	if err := f.platformSnapshots(&cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (f *fileConfig) databaseURL() (string, error) {
	structured := f.Database.Host != "" || f.Database.User != "" ||
		f.Database.Password != "" || f.Database.Name != "" || f.Database.Port != nil
	if f.Database.URL != "" {
		if structured {
			return "", fmt.Errorf("database.url cannot be combined with database.host, port, user, password or name")
		}
		return f.Database.URL, nil
	}
	if f.Database.Host == "" || f.Database.User == "" || f.Database.Password == "" || f.Database.Name == "" {
		return "", fmt.Errorf("database.host, user, password and name are required when database.url is empty")
	}
	port := 5432
	if f.Database.Port != nil {
		port = *f.Database.Port
	}
	if port < 1 || port > 65535 {
		return "", fmt.Errorf("database.port must be 1-65535")
	}
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(f.Database.User, f.Database.Password),
		Host:     net.JoinHostPort(f.Database.Host, strconv.Itoa(port)),
		Path:     "/" + f.Database.Name,
		RawQuery: "sslmode=disable",
	}
	return u.String(), nil
}

func (f *fileConfig) validateScalars() error {
	if f.Quotas.DefaultGPU < 0 || f.Quotas.DefaultSandbox < 0 {
		return fmt.Errorf("quotas must be non-negative")
	}
	if f.Lifecycle.PtyIdleSeconds <= 0 {
		return fmt.Errorf("lifecycle.pty_idle_seconds must be positive")
	}
	if f.Lifecycle.IdleStopSeconds < 0 {
		return fmt.Errorf("lifecycle.idle_stop_seconds must be non-negative")
	}
	if f.Builds.CPU <= 0 {
		return fmt.Errorf("builds.cpu must be a positive number")
	}
	if f.Builds.Memory <= 0 {
		return fmt.Errorf("builds.memory must be positive")
	}
	if f.Logs.ExecCap <= 0 {
		return fmt.Errorf("logs.exec_cap must be positive")
	}
	return nil
}

func (f *fileConfig) keyring() (*keyring.Keyring, error) {
	if len(f.EncryptionKeys) == 0 {
		return nil, nil
	}
	var b bytes.Buffer
	for _, k := range f.EncryptionKeys {
		fmt.Fprintf(&b, "%s %s\n", k.ID, k.Key)
	}
	return keyring.Parse(b.Bytes())
}

func (f *fileConfig) gitProviders() ([]GitProvider, error) {
	providers := make([]GitProvider, 0, len(f.GitProviders))
	seen := map[string]bool{}
	for n, e := range f.GitProviders {
		if e.ClientSecret == "" {
			return nil, fmt.Errorf("git_providers[%d]: client_secret is required", n)
		}
		// defaults.md lists the required fields; scopes is not one of them, so
		// an omitted list requests none.
		scopes := e.Scopes
		if scopes == nil {
			scopes = &[]string{}
		}
		provider, err := resolveGitProvider(gitProviderSpec{
			ID:           e.ID,
			Type:         e.Type,
			ClientID:     e.ClientID,
			ClientSecret: e.ClientSecret,
			BaseURL:      e.BaseURL,
			AuthURL:      e.AuthURL,
			TokenURL:     e.TokenURL,
			ValidateURL:  e.ValidateURL,
			PKCE:         e.PKCE,
			Host:         e.Host,
			Scopes:       scopes,
		})
		if err != nil {
			return nil, fmt.Errorf("git_providers[%d]: %w", n, err)
		}
		if seen[provider.ID] {
			return nil, fmt.Errorf("git_providers[%d]: duplicate id %q", n, provider.ID)
		}
		seen[provider.ID] = true
		providers = append(providers, provider)
	}
	return providers, nil
}

func (f *fileConfig) oidc(cfg *Config) error {
	cfg.OIDC.IssuerURL = f.OIDC.IssuerURL
	cfg.OIDC.ClientID = f.OIDC.ClientID
	cfg.OIDC.ClientSecret = f.OIDC.ClientSecret
	cfg.OIDC.AllowSignups = f.OIDC.AllowSignups
	for _, domain := range f.OIDC.EmailDomains {
		if domain = strings.ToLower(strings.TrimSpace(domain)); domain != "" {
			cfg.OIDC.EmailDomains = append(cfg.OIDC.EmailDomains, domain)
		}
	}
	if !cfg.OIDC.Enabled() {
		return nil
	}
	if cfg.OIDC.ClientID == "" || cfg.OIDC.ClientSecret == "" {
		return fmt.Errorf("oidc.client_id and oidc.client_secret are required when oidc.issuer_url is set")
	}
	return nil
}

func (f *fileConfig) s3(cfg *Config) error {
	cfg.S3 = S3Config{
		Endpoint:       f.S3.Endpoint,
		PublicEndpoint: f.S3.PublicEndpoint,
		Bucket:         f.S3.Bucket,
		Region:         f.S3.Region,
		AccessKey:      f.S3.AccessKey,
		SecretKey:      f.S3.SecretKey,
	}
	if cfg.S3.Endpoint != "" {
		if cfg.S3.Bucket == "" || cfg.S3.AccessKey == "" || cfg.S3.SecretKey == "" {
			return fmt.Errorf("s3.bucket, s3.access_key and s3.secret_key are required when s3.endpoint is set")
		}
	}
	if cfg.S3.PublicEndpoint == "" {
		cfg.S3.PublicEndpoint = cfg.S3.Endpoint
	}
	return nil
}

func (f *fileConfig) registryCredential(cfg *Config) error {
	cfg.RegistryAddr = f.Registry.Addr
	cfg.RegistryPushAddr = f.Registry.PushAddr
	if cfg.RegistryPushAddr == "" {
		cfg.RegistryPushAddr = cfg.RegistryAddr
	}
	raw := ""
	if f.Registry.Username != "" || f.Registry.Password != "" {
		if f.Registry.Username == "" || f.Registry.Password == "" {
			return fmt.Errorf("registry.username and registry.password are required together")
		}
		auth := base64.StdEncoding.EncodeToString([]byte(f.Registry.Username + ":" + f.Registry.Password))
		doc, err := json.Marshal(map[string]any{
			"auths": map[string]any{cfg.RegistryAddr: map[string]string{"auth": auth}},
		})
		if err != nil {
			return err
		}
		raw = string(doc)
	}
	cfg.RegistryCredential = raw
	cred, err := registry.ParseCredential(raw)
	if err != nil {
		return err
	}
	cred.Alias(cfg.RegistryPushAddr, cfg.RegistryAddr)
	cfg.RegistryCredentialDoc = cred
	return nil
}

func (f *fileConfig) platformSnapshots(cfg *Config) error {
	seen := map[string]bool{}
	defaults := 0
	for n, s := range f.PlatformSnapshots {
		if !registry.ValidPlatformName(s.Name) {
			return fmt.Errorf("platform_snapshots[%d]: name %q must be lowercase alphanumeric with . _ - separators", n, s.Name)
		}
		if seen[s.Name] {
			return fmt.Errorf("platform_snapshots[%d]: name %q is listed twice", n, s.Name)
		}
		seen[s.Name] = true
		if _, err := registry.ParseSourceRef(s.Ref); err != nil {
			return fmt.Errorf("platform_snapshots[%d] (%s): %w", n, s.Name, err)
		}
		if s.Default {
			defaults++
			cfg.DefaultSnapshot = s.Name
		}
		cfg.PlatformSnapshots = append(cfg.PlatformSnapshots, PlatformSnapshot{Name: s.Name, Ref: s.Ref})
	}
	if defaults > 1 {
		return fmt.Errorf("platform_snapshots: at most one entry may set default: true")
	}
	return nil
}

func positiveDuration(name, raw string) (time.Duration, error) {
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", name)
	}
	return d, nil
}

func retention(name, raw string) (time.Duration, error) {
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 || (d > 0 && d < time.Second) {
		return 0, fmt.Errorf("%s must be 0 or a duration of at least 1s", name)
	}
	return d, nil
}
