package agent

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/zibyn/stars-sandbox/internal/registry"
)

func TestRegistryCredentialMatchesImageHost(t *testing.T) {
	auth := base64.StdEncoding.EncodeToString([]byte("stars:secret"))
	cred, err := registry.ParseCredential(`{"auths":{"127.0.0.1:15000":{"auth":"` + auth + `"}}}`)
	if err != nil {
		t.Fatal(err)
	}
	const ref = "127.0.0.1:15000/snapshots/0192@sha256:abc"
	header := authHeader(cred, ref)
	if header == "" {
		t.Fatalf("no auth for %s", ref)
	}
	raw, err := base64.URLEncoding.DecodeString(header)
	if err != nil {
		t.Fatalf("X-Registry-Auth is not base64url: %v", err)
	}
	var got struct {
		Username      string `json:"username"`
		Password      string `json:"password"`
		ServerAddress string `json:"serveraddress"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("X-Registry-Auth is not JSON: %v", err)
	}
	if got.Username != "stars" || got.Password != "secret" || got.ServerAddress != "127.0.0.1:15000" {
		t.Fatalf("auth config = %+v", got)
	}
}

func TestRegistryCredentialLeavesDockerHubUnauthenticated(t *testing.T) {
	cred, err := registry.ParseCredential(`{"auths":{"registry.example:5000":{"auth":"eDp5"}}}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{
		"alpine:3.22",
		"library/alpine",
		"docker.io/library/alpine:3.22",
		"registry.example:5001/foo",
	} {
		if header := authHeader(cred, ref); header != "" {
			t.Fatalf("%s got an auth header; only registry.example:5000 is configured", ref)
		}
	}
}

func TestRegistryCredentialAbsentOrMalformed(t *testing.T) {
	if header := authHeader(nil, "127.0.0.1:15000/x"); header != "" {
		t.Fatalf("nil credential returned auth %q", header)
	}
	if _, err := registry.ParseCredential("not json"); err == nil {
		t.Fatal("malformed credential was accepted")
	}
}
