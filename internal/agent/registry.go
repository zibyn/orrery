package agent

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// registryCredential is the deployment's Docker config.json auth document,
// delivered in HelloAck.registry_credential (ADR-0014). It is the Node's only
// way to authenticate to the platform registry.
type registryCredential struct {
	auths map[string]string // registry host -> base64(user:password)
	// raw is the original Docker config.json, written to a temporary
	// DOCKER_CONFIG for the build CLI (buildx reads the standard document).
	raw string
}

func parseRegistryCredential(raw string) (*registryCredential, error) {
	if raw == "" {
		return nil, nil
	}
	var doc struct {
		Auths map[string]struct {
			Auth string `json:"auth"`
		} `json:"auths"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return nil, fmt.Errorf("parse registry credential: %w", err)
	}
	cred := &registryCredential{auths: make(map[string]string, len(doc.Auths)), raw: raw}
	for host, entry := range doc.Auths {
		cred.auths[host] = entry.Auth
	}
	return cred, nil
}

// authHeader is the X-Registry-Auth value for an image reference, or "" when
// the image's registry is not in the credential document. Docker Hub pulls
// must keep working unauthenticated.
func (c *registryCredential) authHeader(ref string) string {
	if c == nil {
		return ""
	}
	host := registryHost(ref)
	encoded, ok := c.auths[host]
	if !ok || encoded == "" {
		return ""
	}
	user, pass, ok := strings.Cut(decodeBase64(encoded), ":")
	if !ok {
		return ""
	}
	payload, err := json.Marshal(map[string]string{
		"username":      user,
		"password":      pass,
		"serveraddress": host,
	})
	if err != nil {
		return ""
	}
	return base64.URLEncoding.EncodeToString(payload)
}

// registryHost applies Docker's reference rule: the first component names a
// registry when it contains "." or ":" or is "localhost"; otherwise the image
// comes from Docker Hub.
func registryHost(ref string) string {
	first, _, _ := strings.Cut(ref, "/")
	if strings.ContainsAny(first, ".:") || first == "localhost" {
		return first
	}
	return "docker.io"
}

// decodeBase64 accepts the standard and URL alphabets Docker configs use.
func decodeBase64(value string) string {
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.URLEncoding} {
		if raw, err := enc.DecodeString(value); err == nil {
			return string(raw)
		}
	}
	return ""
}
