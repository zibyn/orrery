package keyring

import (
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeKeys writes a keys file and returns its path.
func writeKeys(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "keys")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func randomKey(t *testing.T) string {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(key)
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	k, err := LoadFile(writeKeys(t, "one "+randomKey(t)))
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	owner := []byte("0192f0a0-0000-7000-8000-000000000001")
	plaintext := []byte("hunter2")

	ciphertext, keyID, err := k.Encrypt(plaintext, owner)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if keyID != "one" {
		t.Fatalf("key id = %q, want one", keyID)
	}
	if strings.Contains(string(ciphertext), string(plaintext)) {
		t.Fatal("ciphertext contains the plaintext")
	}
	got, err := k.Decrypt(ciphertext, keyID, owner)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if string(got) != string(plaintext) {
		t.Fatalf("Decrypt = %q, want %q", got, plaintext)
	}
}

func TestDecryptRejectsWrongKeyAndAAD(t *testing.T) {
	first, err := LoadFile(writeKeys(t, "first "+randomKey(t)))
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadFile(writeKeys(t, "second "+randomKey(t)))
	if err != nil {
		t.Fatal(err)
	}
	owner := []byte("owner-a")
	ciphertext, keyID, err := first.Encrypt([]byte("secret"), owner)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := second.Decrypt(ciphertext, keyID, owner); err == nil {
		t.Fatal("decrypted with a different key")
	}
	if _, err := first.Decrypt(ciphertext, keyID, []byte("owner-b")); err == nil {
		t.Fatal("decrypted with a different AAD")
	}
	if _, err := first.Decrypt(ciphertext, "missing", owner); err == nil {
		t.Fatal("decrypted with an unknown key id")
	}
}

func TestFirstKeyEncryptsAndEveryKeyDecrypts(t *testing.T) {
	oldKey, newKey := randomKey(t), randomKey(t)
	k, err := LoadFile(writeKeys(t, "new "+newKey, "", "old "+oldKey))
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if _, keyID, err := k.Encrypt([]byte("x"), nil); err != nil || keyID != "new" {
		t.Fatalf("Encrypt used %q, %v; want new", keyID, err)
	}

	// A ciphertext written before rotation still decrypts with the old key.
	old, err := LoadFile(writeKeys(t, "old "+oldKey))
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, keyID, err := old.Encrypt([]byte("x"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k.Decrypt(ciphertext, keyID, nil); err != nil {
		t.Fatalf("Decrypt with rotated keyring: %v", err)
	}
}

func TestLoadFileRejectsMalformedKeys(t *testing.T) {
	for name, body := range map[string]string{
		"empty":        "",
		"one field":    "onlyid",
		"bad base64":   "id !!!",
		"short key":    "id " + base64.StdEncoding.EncodeToString(make([]byte, 16)),
		"long key":     "id " + base64.StdEncoding.EncodeToString(make([]byte, 33)),
		"duplicate id": "id " + randomKey(t) + "\nid " + randomKey(t),
		"three fields": "id " + randomKey(t) + " extra",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "keys")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadFile(path); err == nil {
				t.Fatalf("LoadFile accepted %q", body)
			}
		})
	}
}
