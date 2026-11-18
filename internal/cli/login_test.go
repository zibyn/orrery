package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// Login is a device flow (ADR-0033): the CLI prints the console link with the
// user code, polls without a token until approved, validates the token it is
// handed and stores it.
func TestLoginPollsUntilApproved(t *testing.T) {
	const token = "orr_0123456789abcdef_secret"
	polls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "POST /api/v1/cli-logins":
			if r.Header.Get("Authorization") != "" {
				t.Errorf("start sent Authorization %q", r.Header.Get("Authorization"))
			}
			io.WriteString(w, `{"device_code":"dev","user_code":"BCDF-GHJK","verification_uri":"https://o.test/cli-auth","expires_in":60,"interval":0}`)
		case "POST /api/v1/cli-logins/token":
			var body map[string]string
			if json.NewDecoder(r.Body).Decode(&body) != nil || body["device_code"] != "dev" {
				t.Errorf("poll body = %v", body)
			}
			if polls++; polls < 3 {
				io.WriteString(w, `{"status":"pending"}`)
				return
			}
			io.WriteString(w, `{"status":"approved","token":"`+token+`"}`)
		case "GET /api/v1/users/me":
			if r.Header.Get("Authorization") != "Bearer "+token {
				t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
			}
			io.WriteString(w, `{"id":"u1","name":"alice","is_admin":false}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "config.json")
	var stdout, stderr bytes.Buffer
	if err := runLogin(srv.URL, path, nil, &stdout, &stderr); err != nil {
		t.Fatalf("login: %v", err)
	}
	if !strings.Contains(stderr.String(), "https://o.test/cli-auth?code=BCDF-GHJK") {
		t.Fatalf("stderr = %q, want the approval link", stderr.String())
	}
	if polls != 3 {
		t.Fatalf("polls = %d, want 3", polls)
	}
	fc, err := loadConfig(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if fc.Token != token || fc.APIURL != srv.URL {
		t.Fatalf("config = %+v", fc)
	}
}

// An expired login (404 on poll) stops with the server's message and writes
// no config.
func TestLoginStopsWhenTheLoginExpires(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/cli-logins" {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"device_code":"dev","user_code":"BCDF-GHJK","verification_uri":"https://o.test/cli-auth","expires_in":60,"interval":0}`)
			return
		}
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"code":"not_found","detail":"the CLI login expired"}`)
	}))
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "config.json")
	err := runLogin(srv.URL, path, nil, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("error = %v, want the expiry", err)
	}
	if _, err := loadConfig(path, true); err == nil {
		t.Fatal("a config was written")
	}
}

// There is no password login (ADR-0029): --username is not a flag.
func TestLoginHasNoPasswordFlags(t *testing.T) {
	err := runLogin("http://127.0.0.1:1", filepath.Join(t.TempDir(), "config.json"),
		[]string{"--username", "bob"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "username") {
		t.Fatalf("error = %v, want --username rejected", err)
	}
}
