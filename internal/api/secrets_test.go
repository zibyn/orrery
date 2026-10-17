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
