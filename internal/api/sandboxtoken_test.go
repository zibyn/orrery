package api

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/zibyn/stars-sandbox/internal/store"
)

// A Sandbox Token is "sst_<sandbox id>_<secret>" and never parses as an API
// Token (api.md §1).
func TestSandboxTokenWireForm(t *testing.T) {
	id := uuid.MustParse("0192f0a0-0000-7000-8000-000000000002")
	raw := SandboxToken(id, "secret-part")

	parts, ok := ParseSandboxToken(raw)
	if !ok || parts.ID != id || parts.Secret != "secret-part" {
		t.Fatalf("ParseSandboxToken(%q) = %+v, %v", raw, parts, ok)
	}
	if _, ok := ParseToken(raw); ok {
		t.Fatal("a Sandbox Token parsed as an API Token")
	}
	apiRaw := apiTokenPrefix + id.String() + "_secret-part"
	if _, ok := ParseSandboxToken(apiRaw); ok {
		t.Fatal("an API Token parsed as a Sandbox Token")
	}
}

// The Control Plane can recover the plaintext secret it stored for a Sandbox.
func TestIssueAndOpenSandboxToken(t *testing.T) {
	s := &Server{cfg: Config{EncryptionKeys: testKeyring(t)}}
	id, owner := uuid.New(), uuid.New()
	ct, keyID, err := s.issueSandboxToken(owner)
	if err != nil {
		t.Fatalf("issueSandboxToken: %v", err)
	}
	if len(ct) == 0 || keyID == nil || *keyID != "test" {
		t.Fatalf("issueSandboxToken = (%d bytes, %v), want ciphertext under key test", len(ct), keyID)
	}
	secret, err := sandboxTokenSecret(s.cfg.EncryptionKeys, owner, ct, keyID)
	if err != nil {
		t.Fatalf("sandboxTokenSecret: %v", err)
	}
	if parts, ok := ParseSandboxToken(SandboxToken(id, secret)); !ok || parts.ID != id {
		t.Fatalf("recovered token does not parse: %+v, %v", parts, ok)
	}
	// The ciphertext is bound to the owner.
	if _, err := sandboxTokenSecret(s.cfg.EncryptionKeys, uuid.New(), ct, keyID); err == nil {
		t.Fatal("token decrypted under a different owner")
	}
}

func TestSandboxTokenWithoutKeyring(t *testing.T) {
	s := &Server{}
	if _, _, err := s.issueSandboxToken(uuid.New()); err == nil {
		t.Fatal("issued a Sandbox Token without encryption keys")
	}
	if _, err := sandboxTokenSecret(nil, uuid.New(), []byte("x"), nil); err == nil {
		t.Fatal("opened a Sandbox Token without encryption keys")
	}
}

// A deleted Sandbox's token stops authenticating immediately, including while
// the logical delete is still tearing down (schema.md).
func TestSandboxTokenLive(t *testing.T) {
	live := store.Sandbox{DesiredState: "running", State: "ready"}
	if !sandboxTokenLive(live) {
		t.Fatal("a running Sandbox's token is not live")
	}
	stopped := store.Sandbox{DesiredState: "stopped", State: "stopped"}
	if !sandboxTokenLive(stopped) {
		t.Fatal("a stopped Sandbox's token is not live")
	}
	deleting := store.Sandbox{DesiredState: "deleted", State: "deleting",
		DeletedAt: pgtype.Timestamptz{Time: stopped.CreatedAt.Time, Valid: true}}
	if sandboxTokenLive(deleting) {
		t.Fatal("a logically deleted Sandbox's token is still live")
	}
	if sandboxTokenLive(store.Sandbox{State: "deleting", DeletedAt: pgtype.Timestamptz{Valid: true}}) {
		t.Fatal("a Sandbox with deleted_at is still live")
	}
}
