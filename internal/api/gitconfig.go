package api

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
)

// GitProvider is one deployment-configured OAuth application (ADR-0013,
// defaults.md). Endpoints are resolved from base_url by type, so a self-hosted
// instance needs only its base URL and client credentials.
type GitProvider struct {
	ID           string
	Type         string
	ClientID     string
	ClientSecret string
	AuthURL      string
	TokenURL     string
	ValidateURL  string // empty skips validation (generic only)
	RevokeURL    string // empty means the provider has no revoke endpoint
	PKCE         bool
	HostPattern  *regexp.Regexp
	Scopes       []string
}

// gitProviderFileEntry is one element of ORRERY_GIT_PROVIDERS_FILE (defaults.md,
// environment mode only). PKCE is a pointer so an omitted field means true.
type gitProviderFileEntry struct {
	ID               string    `json:"id"`
	Type             string    `json:"type"`
	ClientID         string    `json:"client_id"`
	ClientSecretFile string    `json:"client_secret_file"`
	BaseURL          string    `json:"base_url"`
	AuthURL          string    `json:"auth_url"`
	TokenURL         string    `json:"token_url"`
	ValidateURL      string    `json:"validate_url"`
	PKCE             *bool     `json:"pkce"`
	Host             string    `json:"host"`
	Scopes           *[]string `json:"scopes"`
}

// gitProviderSpec is the provider fields shared by the environment-mode JSON
// file and config.yaml; the secret has already been read or inlined.
type gitProviderSpec struct {
	ID           string
	Type         string
	ClientID     string
	ClientSecret string
	BaseURL      string
	AuthURL      string
	TokenURL     string
	ValidateURL  string
	PKCE         *bool
	Host         string
	Scopes       *[]string
}

var gitProviderIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// loadGitProviders reads and resolves ORRERY_GIT_PROVIDERS_FILE. An unreadable
// or invalid file fails startup rather than silently disabling Git linking.
func loadGitProviders(path string) ([]GitProvider, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read Git providers: %w", err)
	}
	var entries []gitProviderFileEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("parse Git providers: %w", err)
	}
	providers := make([]GitProvider, 0, len(entries))
	seen := map[string]bool{}
	for n, entry := range entries {
		if entry.ClientSecretFile == "" {
			return nil, fmt.Errorf("Git provider %d: client_secret_file is required", n+1)
		}
		secret, err := os.ReadFile(entry.ClientSecretFile)
		if err != nil {
			return nil, fmt.Errorf("Git provider %d: read client_secret_file: %w", n+1, err)
		}
		provider, err := resolveGitProvider(gitProviderSpec{
			ID:           entry.ID,
			Type:         entry.Type,
			ClientID:     entry.ClientID,
			ClientSecret: strings.TrimSpace(string(secret)),
			BaseURL:      entry.BaseURL,
			AuthURL:      entry.AuthURL,
			TokenURL:     entry.TokenURL,
			ValidateURL:  entry.ValidateURL,
			PKCE:         entry.PKCE,
			Host:         entry.Host,
			Scopes:       entry.Scopes,
		})
		if err != nil {
			return nil, fmt.Errorf("Git provider %d: %w", n+1, err)
		}
		if seen[provider.ID] {
			return nil, fmt.Errorf("Git provider %d: duplicate id %q", n+1, provider.ID)
		}
		seen[provider.ID] = true
		providers = append(providers, provider)
	}
	return providers, nil
}

func resolveGitProvider(e gitProviderSpec) (GitProvider, error) {
	if !gitProviderIDPattern.MatchString(e.ID) {
		return GitProvider{}, fmt.Errorf("id must be non-empty and use letters, digits, '.', '_' or '-'")
	}
	switch e.Type {
	case "github", "gitlab", "gitea", "generic":
	default:
		return GitProvider{}, fmt.Errorf("type must be github, gitlab, gitea or generic")
	}
	// defaults.md: explicit endpoints exist only for generic, which has no
	// type to derive them from.
	if e.Type != "generic" && (e.AuthURL != "" || e.TokenURL != "" || e.ValidateURL != "") {
		return GitProvider{}, fmt.Errorf("auth_url, token_url and validate_url are only for generic")
	}
	// Scopes are deployment policy, not something the type can infer.
	if e.Scopes == nil {
		return GitProvider{}, fmt.Errorf("scopes is required; use an empty array for none")
	}
	if e.ClientID == "" {
		return GitProvider{}, fmt.Errorf("client_id is required")
	}
	p := GitProvider{
		ID:           e.ID,
		Type:         e.Type,
		ClientID:     e.ClientID,
		ClientSecret: e.ClientSecret,
		PKCE:         e.PKCE == nil || *e.PKCE,
		Scopes:       *e.Scopes,
	}
	if p.ClientSecret == "" {
		return GitProvider{}, fmt.Errorf("client_secret is empty")
	}
	base := strings.TrimSuffix(e.BaseURL, "/")
	switch e.Type {
	case "github":
		if base == "" {
			base = "https://github.com"
		}
		apiBase := "https://api.github.com"
		if base != "https://github.com" {
			apiBase = base + "/api/v3"
		}
		p.AuthURL = base + "/login/oauth/authorize"
		p.TokenURL = base + "/login/oauth/access_token"
		p.ValidateURL = apiBase + "/user"
		p.RevokeURL = apiBase + "/applications/" + url.PathEscape(e.ClientID) + "/grant"
	case "gitlab":
		if base == "" {
			base = "https://gitlab.com"
		}
		p.AuthURL = base + "/oauth/authorize"
		p.TokenURL = base + "/oauth/token"
		p.ValidateURL = base + "/api/v4/user"
		p.RevokeURL = base + "/oauth/revoke"
	case "gitea":
		if base == "" {
			return GitProvider{}, fmt.Errorf("base_url is required for gitea")
		}
		p.AuthURL = base + "/login/oauth/authorize"
		p.TokenURL = base + "/login/oauth/access_token"
		p.ValidateURL = base + "/api/v1/user"
	case "generic":
		if e.AuthURL == "" || e.TokenURL == "" {
			return GitProvider{}, fmt.Errorf("auth_url and token_url are required for generic")
		}
		p.AuthURL = e.AuthURL
		p.TokenURL = e.TokenURL
		p.ValidateURL = e.ValidateURL
	}
	hostSource := e.Host
	if hostSource == "" {
		hostSource = hostPatternFrom(e.BaseURL, p.AuthURL)
	}
	pattern, err := regexp.Compile(hostSource)
	if err != nil {
		return GitProvider{}, fmt.Errorf("host pattern %q: %w", hostSource, err)
	}
	p.HostPattern = pattern
	if err := validateGitEndpoint("auth_url", p.AuthURL); err != nil {
		return GitProvider{}, err
	}
	if err := validateGitEndpoint("token_url", p.TokenURL); err != nil {
		return GitProvider{}, err
	}
	return p, nil
}

// hostPatternFrom derives an exact-host match from the deployment URL, so a
// provider that names only base_url still matches its repositories.
func hostPatternFrom(baseURL, authURL string) string {
	raw := baseURL
	if raw == "" {
		raw = authURL
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "^$"
	}
	return "^" + regexp.QuoteMeta(u.Host) + "$"
}

func validateGitEndpoint(name, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("%s must be an absolute URL", name)
	}
	return nil
}

// gitProvider returns the configured provider with this id.
func (s *Server) gitProvider(id string) (GitProvider, bool) {
	for _, p := range s.cfg.GitProviders {
		if p.ID == id {
			return p, true
		}
	}
	return GitProvider{}, false
}
