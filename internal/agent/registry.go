package agent

import (
	"encoding/base64"
	"encoding/json"
	"strings"

	"github.com/zibyn/stars-sandbox/internal/registry"
)

// authHeader is the X-Registry-Auth value for an image reference, or "" when
// the image's registry is not in the credential document. Docker Hub pulls
// must keep working unauthenticated.
func authHeader(cred *registry.Credential, ref string) string {
	host := registryHost(ref)
	cfg, ok := cred.Get(host)
	if !ok || (cfg.Username == "" && cfg.Password == "") {
		return ""
	}
	payload, err := json.Marshal(map[string]string{
		"username":      cfg.Username,
		"password":      cfg.Password,
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
