// Package registry is the Control Plane's client for the platform OCI
// registry (ADR-0014, ADR-0021). Nodes pull by digest; the Control Plane is
// the only writer, importing whole indexes from external refs while keeping
// their digest.
package registry

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/uuid"
)

// Repo is a Snapshot's repository inside the platform registry (defaults.md,
// "对象存储布局"): user Snapshots are keyed by id, platform Snapshots by name.
func Repo(snapshotID uuid.UUID, platformName string) string {
	if platformName != "" {
		return "platform/" + platformName
	}
	return "snapshots/" + snapshotID.String()
}

// Ref joins a registry address, repository and digest into the pull reference
// pinned in a Sandbox.
func Ref(addr, repo, digest string) string {
	return strings.TrimSuffix(addr, "/") + "/" + repo + "@" + digest
}

// ParseSourceRef validates an import source. Only a repository reference
// pinned by a sha256 digest is importable; tags could move under the import.
func ParseSourceRef(raw string) (name.Digest, error) {
	ref, err := name.NewDigest(strings.TrimSpace(raw))
	if err != nil {
		return name.Digest{}, fmt.Errorf("ref must be ref@sha256:…: %w", err)
	}
	if !strings.HasPrefix(ref.DigestStr(), "sha256:") || len(ref.DigestStr()) != len("sha256:")+64 {
		return name.Digest{}, fmt.Errorf("ref must be pinned by a sha256 digest")
	}
	return ref, nil
}

// RewriteRegistry returns ref with its registry host replaced when it is the
// deployment's Node-reachable address. A Control Plane in a container cannot
// reach its own 127.0.0.1, but importing an image that is already in the
// platform registry should still work.
func RewriteRegistry(ref name.Digest, from, to string) (name.Digest, error) {
	from, to = normalizeHost(from), normalizeHost(to)
	if from == "" || to == "" || from == to || normalizeHost(ref.Context().RegistryStr()) != from {
		return ref, nil
	}
	return name.NewDigest(to + "/" + ref.Context().RepositoryStr() + "@" + ref.DigestStr())
}

// ValidPlatformName reports whether a configured platform Snapshot name can
// also be used as a registry repository component.
func ValidPlatformName(value string) bool {
	return platformName.MatchString(value)
}

var platformName = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*$`)

// Credential is a Docker config.json auth document. The same document serves
// Nodes (pull host) and the Control Plane (push host); a deployment lists
// both hosts in `auths`.
type Credential struct {
	auths map[string]authn.AuthConfig
}

// ParseCredential reads a docker config.json document. An empty document is
// valid and means "no authentication".
func ParseCredential(raw string) (*Credential, error) {
	cred := &Credential{auths: map[string]authn.AuthConfig{}}
	if strings.TrimSpace(raw) == "" {
		return cred, nil
	}
	var doc struct {
		Auths map[string]struct {
			Auth     string `json:"auth"`
			Username string `json:"username"`
			Password string `json:"password"`
		} `json:"auths"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return nil, fmt.Errorf("parse registry credential: %w", err)
	}
	for host, entry := range doc.Auths {
		cfg := authn.AuthConfig{Username: entry.Username, Password: entry.Password}
		if entry.Auth != "" {
			user, password, ok := strings.Cut(decodeBase64(entry.Auth), ":")
			if !ok {
				return nil, fmt.Errorf("registry credential for %s has a malformed auth field", host)
			}
			cfg.Username, cfg.Password = user, password
		}
		cred.auths[normalizeHost(host)] = cfg
	}
	return cred, nil
}

// decodeBase64 accepts the standard and URL alphabets Docker configs use.
func decodeBase64(value string) string {
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.URLEncoding,
		base64.RawStdEncoding, base64.RawURLEncoding,
	} {
		if raw, err := enc.DecodeString(value); err == nil {
			return string(raw)
		}
	}
	return ""
}

func normalizeHost(host string) string {
	return strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(host, "https://"), "http://"), "/")
}

// Alias makes host resolve to from's credentials when it has none of its own.
// A deployment whose push address is not listed still authenticates with the
// Node-reachable entry.
func (c *Credential) Alias(host, from string) {
	if c == nil {
		return
	}
	host, from = normalizeHost(host), normalizeHost(from)
	if _, ok := c.auths[host]; ok {
		return
	}
	if cfg, ok := c.auths[from]; ok {
		c.auths[host] = cfg
	}
}

// Has reports whether the document holds credentials for host.
func (c *Credential) Has(host string) bool {
	if c == nil {
		return false
	}
	_, ok := c.auths[normalizeHost(host)]
	return ok
}

// Keychain resolves a registry host to its credentials.
func (c *Credential) Keychain() authn.Keychain { return keychain{c} }

type keychain struct{ c *Credential }

func (k keychain) Resolve(res authn.Resource) (authn.Authenticator, error) {
	if k.c == nil {
		return authn.Anonymous, nil
	}
	cfg, ok := k.c.auths[normalizeHost(res.RegistryStr())]
	if !ok {
		return authn.Anonymous, nil
	}
	return authn.FromConfig(cfg), nil
}

// Import copies a source image or the whole index to target, preserving the
// source digest, and verifies that the registry now serves that digest.
func Import(ctx context.Context, source name.Digest, target name.Digest, sourceAuth, targetAuth *Credential) error {
	sourceOpts := []remote.Option{remote.WithContext(ctx)}
	if sourceAuth != nil {
		sourceOpts = append(sourceOpts, remote.WithAuthFromKeychain(sourceAuth.Keychain()))
	}
	desc, err := remote.Get(source, sourceOpts...)
	if err != nil {
		return fmt.Errorf("read source %s: %w", source, err)
	}
	if desc.Digest.String() != source.DigestStr() {
		return fmt.Errorf("source served %s, requested %s", desc.Digest, source.DigestStr())
	}
	targetOpts := []remote.Option{remote.WithContext(ctx)}
	if targetAuth != nil {
		targetOpts = append(targetOpts, remote.WithAuthFromKeychain(targetAuth.Keychain()))
	}
	if desc.MediaType.IsIndex() {
		index, err := desc.ImageIndex()
		if err != nil {
			return fmt.Errorf("read source index: %w", err)
		}
		if err := remote.WriteIndex(target, index, targetOpts...); err != nil {
			return fmt.Errorf("write index to %s: %w", target, err)
		}
	} else {
		image, err := desc.Image()
		if err != nil {
			return fmt.Errorf("read source image: %w", err)
		}
		if err := remote.Write(target, image, targetOpts...); err != nil {
			return fmt.Errorf("write image to %s: %w", target, err)
		}
	}
	written, err := remote.Head(target, targetOpts...)
	if err != nil {
		return fmt.Errorf("verify %s: %w", target, err)
	}
	if written.Digest.String() != source.DigestStr() {
		return fmt.Errorf("wrote %s, requested %s", written.Digest, source.DigestStr())
	}
	return nil
}
