// Package keyring encrypts secrets at rest with AES-256-GCM and a
// deployment-supplied keyring (defaults.md, encryption_keys).
package keyring

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
)

const keySize = 32

// Keyring holds every configured key. The first line of the file is the
// encryption key; all keys decrypt, so rotating a key does not make existing
// ciphertext unreadable (ADR-0017).
type Keyring struct {
	primary string
	keys    map[string][]byte
}

// LoadFile reads "<key_id> <base64 32 bytes>" per line.
func LoadFile(path string) (*Keyring, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read encryption keys: %w", err)
	}
	return Parse(data)
}

// Parse builds a Keyring from the file content.
func Parse(data []byte) (*Keyring, error) {
	k := &Keyring{keys: map[string][]byte{}}
	for n, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf("encryption key line %d: want <key_id> <base64 key>", n+1)
		}
		id := fields[0]
		if _, ok := k.keys[id]; ok {
			return nil, fmt.Errorf("encryption key line %d: duplicate key id %q", n+1, id)
		}
		raw, err := base64.StdEncoding.DecodeString(fields[1])
		if err != nil || len(raw) != keySize {
			return nil, fmt.Errorf("encryption key line %d: key must be base64 of %d bytes", n+1, keySize)
		}
		if k.primary == "" {
			k.primary = id
		}
		k.keys[id] = raw
	}
	if k.primary == "" {
		return nil, errors.New("encryption keys file has no keys")
	}
	return k, nil
}

// Encrypt seals plaintext with the primary key and returns the key id needed
// to decrypt it later. aad binds the ciphertext to its owner.
func (k *Keyring) Encrypt(plaintext, aad []byte) (ciphertext []byte, keyID string, err error) {
	gcm, err := newGCM(k.keys[k.primary])
	if err != nil {
		return nil, "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, "", fmt.Errorf("generate nonce: %w", err)
	}
	out := make([]byte, 0, len(nonce)+len(plaintext)+gcm.Overhead())
	out = append(out, nonce...)
	return gcm.Seal(out, nonce, plaintext, aad), k.primary, nil
}

// Decrypt opens ciphertext with the named key.
func (k *Keyring) Decrypt(ciphertext []byte, keyID string, aad []byte) ([]byte, error) {
	key, ok := k.keys[keyID]
	if !ok {
		return nil, fmt.Errorf("unknown encryption key %q", keyID)
	}
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	if len(ciphertext) < gcm.NonceSize() {
		return nil, errors.New("ciphertext is too short")
	}
	nonce, sealed := ciphertext[:gcm.NonceSize()], ciphertext[gcm.NonceSize():]
	plaintext, err := gcm.Open(nil, nonce, sealed, aad)
	if err != nil {
		return nil, fmt.Errorf("decrypt: %w", err)
	}
	return plaintext, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
