package api

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestConsoleServesSPA covers the / console surface: every non-API GET falls
// back to index.html so client-side routes work on a reload, while the API
// prefix keeps answering with problem+json (api.md §1, §8).
func TestConsoleServesSPA(t *testing.T) {
	s := &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	h := s.Handler()

	for _, path := range []string{"/", "/login?return=%2Fcli-auth", "/cli-auth", "/assets/missing.js"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", path, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Fatalf("GET %s content type = %q, want text/html", path, ct)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
			t.Fatalf("GET %s Cache-Control = %q, want no-store", path, cc)
		}
	}

	// A console path is not an endpoint: an unsafe method answers like any
	// unknown route.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/login", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("POST /login = %d, want 404", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Fatalf("POST /login content type = %q, want application/problem+json", ct)
	}

	// The API prefix is unaffected by the console fallback.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/nope", nil))
	if rec.Code != http.StatusNotFound || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/problem+json") {
		t.Fatalf("GET /api/v1/nope = %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}

	// /metrics belongs to the API listener, not the SPA (api.md §1).
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("GET /metrics served the console: %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
}
