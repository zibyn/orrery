package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSameOrigin(t *testing.T) {
	plain := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:18080/api/v1/users/me", nil)
	secure := httptest.NewRequest(http.MethodGet, "https://api.example.com/api/v1/users/me", nil)

	for _, tc := range []struct {
		name   string
		req    *http.Request
		origin string
		want   bool
	}{
		{"same http origin", plain, "http://127.0.0.1:18080", true},
		{"other port", plain, "http://127.0.0.1:9999", false},
		{"other host", plain, "http://evil.example", false},
		{"port-forward subdomain", plain, "http://3000--sbx.127.0.0.1:18080", false},
		{"same https origin", secure, "https://api.example.com", true},
		{"https downgrade", secure, "http://api.example.com", false},
		{"https onto a plain listener", plain, "https://127.0.0.1:18080", false},
		{"missing origin", plain, "", false},
		{"garbage origin", plain, "://", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sameOrigin(tc.req, tc.origin); got != tc.want {
				t.Fatalf("sameOrigin(%q) = %v, want %v", tc.origin, got, tc.want)
			}
		})
	}

	proxied := httptest.NewRequest(http.MethodGet, "http://api.example.com/api/v1/users/me", nil)
	proxied.Header.Set("X-Forwarded-Proto", "https")
	if !sameOrigin(proxied, "https://api.example.com") {
		t.Fatal("sameOrigin rejected the proxy scheme reported by X-Forwarded-Proto")
	}
}

func TestIsWebSocketUpgrade(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers map[string]string
		want    bool
	}{
		{"upgrade", map[string]string{"Connection": "Upgrade", "Upgrade": "websocket"}, true},
		{"token list and case", map[string]string{"Connection": "keep-alive, Upgrade", "Upgrade": "WebSocket"}, true},
		{"no upgrade header", map[string]string{"Connection": "Upgrade"}, false},
		{"plain request", nil, false},
	} {
		r := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:18080/", nil)
		for k, v := range tc.headers {
			r.Header.Set(k, v)
		}
		if got := isWebSocketUpgrade(r); got != tc.want {
			t.Errorf("%s: isWebSocketUpgrade = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestSessionCookieAttributes(t *testing.T) {
	c := newSessionCookie("ssk_value")
	if c.Name != "stars_session" || c.Value != "ssk_value" {
		t.Fatalf("cookie = %q=%q", c.Name, c.Value)
	}
	if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode {
		t.Fatalf("cookie flags = HttpOnly:%v Secure:%v SameSite:%v", c.HttpOnly, c.Secure, c.SameSite)
	}
	if c.Path != "/" {
		t.Fatalf("path = %q, want /", c.Path)
	}
	if c.Domain != "" {
		t.Fatalf("domain = %q, want empty", c.Domain)
	}

	cleared := clearSessionCookie()
	if cleared.MaxAge >= 0 || cleared.Value != "" || cleared.Name != "stars_session" {
		t.Fatalf("cleared cookie = %+v", cleared)
	}
}
