package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProblemError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"code":"not_found","detail":"no such thing","authorize_url":"https://stars.example/git"}`)
	}))
	defer srv.Close()

	_, err := New(srv.URL, "tok").Me(context.Background())
	var problem *Error
	if !errors.As(err, &problem) {
		t.Fatalf("error = %v, want *client.Error", err)
	}
	if problem.Status != http.StatusNotFound || problem.Code != "not_found" || problem.Detail != "no such thing" {
		t.Fatalf("problem = %+v", problem)
	}
	if problem.AuthorizeURL != "https://stars.example/git" {
		t.Fatalf("authorize_url = %q", problem.AuthorizeURL)
	}
	if got := problem.Error(); got != "not_found: no such thing" {
		t.Fatalf("Error() = %q", got)
	}
}

func TestNonProblemErrorKeepsTheStatusLine(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "upstream is down\n")
	}))
	defer srv.Close()

	_, err := New(srv.URL, "tok").Me(context.Background())
	if got := err.Error(); got != "HTTP 502: upstream is down" {
		t.Fatalf("error = %q", got)
	}
}

func TestSendCarriesAuthBodyAndIdempotencyKey(t *testing.T) {
	var (
		gotAuth, gotKey, gotContentType string
		gotBody                         CreateSandboxRequest
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotKey = r.Header.Get("Idempotency-Key")
		gotContentType = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(Sandbox{ID: "sb1", State: "pending"})
	}))
	defer srv.Close()

	name, cpu := "box", 0.5
	sb, err := New(srv.URL, "ssk_test").CreateSandbox(context.Background(), CreateSandboxRequest{
		Name: &name, CPU: &cpu,
	}, "key-1")
	if err != nil {
		t.Fatal(err)
	}
	if sb.ID != "sb1" || sb.State != "pending" {
		t.Fatalf("Sandbox = %+v", sb)
	}
	if gotAuth != "Bearer ssk_test" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if gotKey != "key-1" {
		t.Fatalf("Idempotency-Key = %q", gotKey)
	}
	if !strings.HasPrefix(gotContentType, "application/json") {
		t.Fatalf("Content-Type = %q", gotContentType)
	}
	if gotBody.Name == nil || *gotBody.Name != name || gotBody.CPU == nil || *gotBody.CPU != cpu {
		t.Fatalf("request body = %+v", gotBody)
	}
}

func TestCreatedTokenEmbedsTokenAndDecodesPlaintext(t *testing.T) {
	var got CreatedToken
	body := `{"id":"t1","kind":"api","name":"ci","scope":"user",` +
		`"created_at":"2026-01-01T00:00:00Z","token":"sst_secret"}`
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != "t1" || got.Name != "ci" || got.Scope != "user" || got.Plaintext != "sst_secret" {
		t.Fatalf("CreatedToken = %+v", got)
	}
}

func TestGetSandboxQueriesWaitFor(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("wait_for"); got != "ready" {
			t.Errorf("wait_for = %q", got)
		}
		_ = json.NewEncoder(w).Encode(Sandbox{ID: "sb1", State: "ready"})
	}))
	defer srv.Close()

	sb, err := New(srv.URL, "tok").GetSandbox(context.Background(), "sb1", "ready")
	if err != nil {
		t.Fatal(err)
	}
	if sb.State != "ready" {
		t.Fatalf("State = %q", sb.State)
	}
}
