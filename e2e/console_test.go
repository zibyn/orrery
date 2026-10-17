//go:build e2e

package e2e

import (
	"net/http"
	"regexp"
	"strings"
	"testing"
)

// TestWebConsole checks the deployed console: the SPA is served at / with a
// client-route fallback, the real build is embedded rather than the
// placeholder, and the API prefix is untouched (#77, api.md §8).
func TestWebConsole(t *testing.T) {
	for _, path := range []string{"/", "/login?return=%2Fcli-auth", "/cli-auth"} {
		resp, body := request(t, http.MethodGet, path, nil, nil, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200: %s", path, resp.StatusCode, body)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Fatalf("GET %s content type = %q, want text/html", path, ct)
		}
		if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
			t.Fatalf("GET %s Cache-Control = %q, want no-store", path, cc)
		}
	}

	_, index := request(t, http.MethodGet, "/", nil, nil, nil)
	match := regexp.MustCompile(`src="(/assets/[^"]+\.js)"`).FindSubmatch(index)
	if match == nil {
		t.Fatalf("index.html has no module script; the placeholder was embedded instead of the build:\n%s", index)
	}
	resp, _ := request(t, http.MethodGet, string(match[1]), nil, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", match[1], resp.StatusCode)
	}
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Fatalf("asset Cache-Control = %q, want immutable", cc)
	}

	// The console fallback must not swallow the API's problem+json.
	resp, body := request(t, http.MethodGet, "/api/v1/nope", nil, nil, nil)
	if resp.StatusCode != http.StatusNotFound || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/problem+json") {
		t.Fatalf("GET /api/v1/nope = %d %q: %s", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
}
