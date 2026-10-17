package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/zibyn/stars-sandbox/internal/keyring"
	"github.com/zibyn/stars-sandbox/internal/store"
)

func testPrincipal() *principal {
	return &principal{user: store.User{ID: uuid.MustParse("0192f0a0-0000-7000-8000-000000000001")},
		token: store.GetTokenWithUserRow{Scope: "user"}}
}

func testKeyring(t *testing.T) *keyring.Keyring {
	t.Helper()
	encoded := base64.StdEncoding.EncodeToString(make([]byte, 32))
	k, err := keyring.Parse([]byte("test " + encoded + "\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return k
}

// PUT /secrets/{name} rejects reserved names and oversized values before it
// touches the database (ADR-0017).
func TestPutSecretRejections(t *testing.T) {
	s := &Server{cfg: Config{EncryptionKeys: testKeyring(t)}}
	for _, name := range []string{"GIT_TOKEN", "LD_PRELOAD", "STARS_SANDBOX_TOKEN", "PATH", "HOME"} {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPut, "/api/v1/secrets/"+name,
				strings.NewReader(`{"value":"x"}`))
			req.SetPathValue("name", name)
			s.handlePutSecret(rec, req, testPrincipal())
			assertProblem(t, rec, http.StatusBadRequest, "invalid_request")
		})
	}

	t.Run("oversized", func(t *testing.T) {
		body, _ := json.Marshal(map[string]string{"value": strings.Repeat("x", maxSecretBytes+1)})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, "/api/v1/secrets/BIG", bytes.NewReader(body))
		req.SetPathValue("name", "BIG")
		s.handlePutSecret(rec, req, testPrincipal())
		assertProblem(t, rec, http.StatusRequestEntityTooLarge, "payload_too_large")
	})

	t.Run("body too large", func(t *testing.T) {
		body, _ := json.Marshal(map[string]string{"value": strings.Repeat("x", 2<<20)})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, "/api/v1/secrets/BIG", bytes.NewReader(body))
		req.SetPathValue("name", "BIG")
		s.handlePutSecret(rec, req, testPrincipal())
		assertProblem(t, rec, http.StatusRequestEntityTooLarge, "payload_too_large")
	})
}

func TestSecretFeatureWithoutEncryptionKeys(t *testing.T) {
	s := &Server{}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/v1/secrets/A", strings.NewReader(`{"value":"x"}`))
	s.handlePutSecret(rec, req, testPrincipal())
	assertProblem(t, rec, http.StatusServiceUnavailable, "unavailable")
}

func TestSecretNameAndSizeLimits(t *testing.T) {
	for _, name := range []string{"GITHUB_TOKEN", "MY_PATH", "PATHX", "git_token", "STARS", "LD"} {
		if code, _ := secretUploadProblem(name, "x"); code != "" {
			t.Errorf("secretUploadProblem(%q) = %q, want allowed", name, code)
		}
	}
	for _, name := range []string{"", " padded", "GIT_", "LD_", "STARS_", "PATH", "HOME"} {
		if code, _ := secretUploadProblem(name, "x"); code == "" {
			t.Errorf("secretUploadProblem(%q) allowed a reserved name", name)
		}
	}
	if code, _ := secretUploadProblem("A", strings.Repeat("x", maxSecretBytes)); code != "" {
		t.Fatalf("24 KiB rejected: %q", code)
	}
	if code, _ := secretUploadProblem("A", strings.Repeat("x", maxSecretBytes+1)); code != "payload_too_large" {
		t.Fatalf("24 KiB + 1 = %q, want payload_too_large", code)
	}
}

func TestSecretQuota(t *testing.T) {
	for _, tt := range []struct {
		count int64
		want  bool
	}{
		{0, false},
		{49, false},
		{50, true},
		{60, true},
	} {
		if got := secretQuotaExceeded(tt.count); got != tt.want {
			t.Errorf("secretQuotaExceeded(%d) = %v, want %v", tt.count, got, tt.want)
		}
	}
}

// The keys of a Sandbox's secrets mapping follow the Secret name rules
// (ADR-0017): reserved prefixes and malformed environment names are refused.
func TestSecretEnvNameValidation(t *testing.T) {
	for _, name := range []string{"TOKEN", "GITHUB_TOKEN", "MY_PATH", "API_KEY"} {
		if code, _ := secretEnvNameProblem(name); code != "" {
			t.Errorf("secretEnvNameProblem(%q) = %q, want allowed", name, code)
		}
	}
	for _, name := range []string{"", " padded", "A=B", "A\x00B", "GIT_", "LD_", "STARS_", "PATH", "HOME"} {
		if code, _ := secretEnvNameProblem(name); code == "" {
			t.Errorf("secretEnvNameProblem(%q) allowed a reserved or malformed name", name)
		}
	}
}

// The snapshot keeps the Secret names and owner-bound ciphertexts; only the
// Control Plane decrypts them, and only for the owning User.
func TestSecretEnvSnapshot(t *testing.T) {
	keys := testKeyring(t)
	owner := uuid.New()
	ciphertext, keyID, err := keys.Encrypt([]byte("hunter2"), []byte(owner.String()))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	snapshot := secretEnvSnapshot{
		"TOKEN": {Name: "my-secret", KeyID: keyID, Ciphertext: ciphertext},
	}
	raw, err := encodeSecretEnv(snapshot)
	if err != nil {
		t.Fatalf("encodeSecretEnv: %v", err)
	}
	if !bytes.Contains(raw, []byte(`"name":"my-secret"`)) {
		t.Fatalf("snapshot lost the Secret name: %s", raw)
	}
	env, err := decryptSecretEnv(keys, owner, raw)
	if err != nil {
		t.Fatalf("decryptSecretEnv: %v", err)
	}
	if env["TOKEN"] != "hunter2" {
		t.Fatalf("decrypted TOKEN = %q, want hunter2", env["TOKEN"])
	}
	// The ciphertext is bound to its owner.
	if _, err := decryptSecretEnv(keys, uuid.New(), raw); err == nil {
		t.Fatal("decryptSecretEnv opened another User's ciphertext")
	}
	if _, err := decryptSecretEnv(keys, owner, []byte("not json")); err == nil {
		t.Fatal("decryptSecretEnv accepted a malformed snapshot")
	}
	if names := referencedSecretNames(snapshot); len(names) != 1 || names[0] != "my-secret" {
		t.Fatalf("referencedSecretNames = %v, want [my-secret]", names)
	}
	if raw, err := encodeSecretEnv(nil); err != nil || raw != nil {
		t.Fatalf("encodeSecretEnv(nil) = (%v, %v), want (nil, nil)", raw, err)
	}
	// Two environment names may reference one Secret; audit names are distinct.
	snapshot["TOKEN2"] = snapshot["TOKEN"]
	if names := referencedSecretNames(snapshot); len(names) != 1 {
		t.Fatalf("referencedSecretNames with a duplicate Secret = %v, want one name", names)
	}
}

func assertProblem(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d (body %s)", rec.Code, status, rec.Body)
	}
	var p struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	if p.Code != code {
		t.Fatalf("code = %q, want %q", p.Code, code)
	}
}
