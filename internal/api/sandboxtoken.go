package api

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"

	"github.com/zibyn/orrery/internal/keyring"
	"github.com/zibyn/orrery/internal/store"
)

// errSandboxTokenStorage is the explicit answer when the deployment has no
// keyring (defaults.md).
var errSandboxTokenStorage = errors.New("Sandbox Token storage is not configured: set encryption_keys in the deployment configuration")

// requireSandboxTokens refuses a Sandbox create when tokens cannot be sealed.
func (s *Server) requireSandboxTokens(w http.ResponseWriter) bool {
	if s.cfg.EncryptionKeys == nil {
		writeProblem(w, "unavailable", errSandboxTokenStorage.Error())
		return false
	}
	return true
}

func newSandboxTokenSecret() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate Sandbox Token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// issueSandboxToken seals a fresh secret for the Sandbox's owner. Only the
// Control Plane can open it again, once per StartExec (ADR-0016).
func (s *Server) issueSandboxToken(ownerID uuid.UUID) ([]byte, *string, error) {
	if s.cfg.EncryptionKeys == nil {
		return nil, nil, errSandboxTokenStorage
	}
	secret, err := newSandboxTokenSecret()
	if err != nil {
		return nil, nil, err
	}
	ct, keyID, err := s.cfg.EncryptionKeys.Encrypt([]byte(secret), []byte(ownerID.String()))
	if err != nil {
		return nil, nil, fmt.Errorf("encrypt Sandbox Token: %w", err)
	}
	return ct, &keyID, nil
}

// sandboxTokenSecret opens the stored token for injection into a StartExec.
func sandboxTokenSecret(keys *keyring.Keyring, ownerID uuid.UUID, ct []byte, keyID *string) (string, error) {
	if keys == nil {
		return "", errSandboxTokenStorage
	}
	if len(ct) == 0 || keyID == nil {
		return "", errors.New("Sandbox has no Sandbox Token")
	}
	raw, err := keys.Decrypt(ct, *keyID, []byte(ownerID.String()))
	if err != nil {
		return "", fmt.Errorf("decrypt Sandbox Token: %w", err)
	}
	return string(raw), nil
}

// sandboxTokenLive reports whether a Sandbox Token may still authenticate. A
// Sandbox delete sets both desired_state and deleted_at, and invalidates the
// token immediately (schema.md).
func sandboxTokenLive(sb store.Sandbox) bool {
	return !sb.DeletedAt.Valid && sb.DesiredState != string(SandboxDesiredDeleted)
}
