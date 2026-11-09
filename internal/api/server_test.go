package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUnknownEndpointsReturnProblemJSON(t *testing.T) {
	s := &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	h := s.Handler()

	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/v1/nope"},
		{http.MethodPost, "/api/v1/users/me/quota"},
		// Polaris is the only identity provider (ADR-0029): no password login,
		// no password changes, no admin-created Users.
		{http.MethodPost, "/api/v1/sessions"},
		{http.MethodPut, "/api/v1/users/me/password"},
		{http.MethodPut, "/api/v1/users/0197a6c0-0000-7000-8000-000000000000/password"},
		{http.MethodPost, "/api/v1/users"},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s %s = %d, want 404", tc.method, tc.path, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
			t.Fatalf("%s %s content type = %q", tc.method, tc.path, ct)
		}
		var problem struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil {
			t.Fatalf("%s %s: %v", tc.method, tc.path, err)
		}
		if problem.Code != "not_found" {
			t.Fatalf("%s %s code = %q, want not_found", tc.method, tc.path, problem.Code)
		}
	}
}

func TestOpenAPIHasNoPasswordIdentity(t *testing.T) {
	s := &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	doc, err := json.Marshal(s.OpenAPIDocument())
	if err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"createSession", "changeMyPassword", "resetUserPassword", "createUser", "login_type", "ChangePasswordRequest"} {
		if strings.Contains(string(doc), gone) {
			t.Errorf("OpenAPI still mentions %q", gone)
		}
	}
}
