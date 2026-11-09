package api

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/google/uuid"
)

// fakePolaris is an in-process Polaris OP: discovery, JWKS and /token. The
// Polaris SDK's own fake OP is test-only and cannot be imported (ADR-0029).
type fakePolaris struct {
	*httptest.Server
	ClientID, Audience string
	// WebhookSecret is the Application's webhook secret, in Polaris's
	// whsec_<base64> form.
	WebhookSecret string
	key           *rsa.PrivateKey

	mu     sync.Mutex
	logins map[string]polarisLogin // by authorization code
}

// polarisLogin is what Polaris answers for one authorization code.
type polarisLogin struct {
	Sub          string
	Sid          string
	Entitlements []string
	// Audience overrides the access token's aud; empty means fakePolaris.Audience.
	Audience string
	// Profile is extra ID token claims (email, preferred_username, name).
	Profile map[string]any
	nonce   string
}

func newFakePolaris(t *testing.T) *fakePolaris {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakePolaris{ClientID: "orrery", Audience: "api://orrery", key: key,
		WebhookSecret: "whsec_" + base64.StdEncoding.EncodeToString([]byte(uuid.NewString())), logins: map[string]polarisLogin{}}
	mux := http.NewServeMux()
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                f.URL,
			"authorization_endpoint":                f.URL + "/authorize",
			"token_endpoint":                        f.URL + "/token",
			"jwks_uri":                              f.URL + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
			{Key: &f.key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"},
		}})
	})
	mux.HandleFunc("POST /token", f.token)
	return f
}

func (f *fakePolaris) token(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	login, ok := f.logins[r.PostFormValue("code")]
	delete(f.logins, r.PostFormValue("code"))
	f.mu.Unlock()
	if !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
		return
	}
	aud := login.Audience
	if aud == "" {
		aud = f.Audience
	}
	now := time.Now().Unix()
	access := map[string]any{
		"iss": f.URL, "aud": aud, "sub": login.Sub, "client_id": f.ClientID, "sid": login.Sid,
		"iat": now, "exp": now + 600, "jti": uuid.NewString(),
	}
	if len(login.Entitlements) > 0 { // Polaris omits the claim when no Role is held
		access["entitlements"] = login.Entitlements
	}
	id := map[string]any{
		"iss": f.URL, "aud": f.ClientID, "sub": login.Sub, "sid": login.Sid, "nonce": login.nonce,
		"iat": now, "exp": now + 600,
	}
	for k, v := range login.Profile {
		id[k] = v
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": f.Sign("at+jwt", access), "token_type": "Bearer", "expires_in": 600,
		"id_token": f.Sign("JWT", id),
	})
}

// Sign signs claims with the fake's key and the given typ header, so later
// tests can mint logout tokens and forged variants.
func (f *fakePolaris) Sign(typ string, claims map[string]any) string {
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: f.key},
		(&jose.SignerOptions{}).WithType(jose.ContentType(typ)).WithHeader("kid", "k1"))
	if err != nil {
		panic(err)
	}
	raw, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		panic(err)
	}
	return raw
}

// Login runs the browser half of a Polaris login against h: /oidc/login,
// Polaris approving it as login, then /oidc/callback. It returns the callback
// response; on success it carries the Session cookie.
func (f *fakePolaris) Login(t *testing.T, h http.Handler, login polarisLogin) *http.Response {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/oidc/login", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("GET /oidc/login = %d: %s", rec.Code, rec.Body)
	}
	authorize, err := url.Parse(rec.Header().Get("Location"))
	if err != nil || !strings.HasPrefix(authorize.String(), f.URL+"/authorize") {
		t.Fatalf("/oidc/login redirected to %q, want the fake Polaris", rec.Header().Get("Location"))
	}
	code := uuid.NewString()
	login.nonce = authorize.Query().Get("nonce")
	f.mu.Lock()
	f.logins[code] = login
	f.mu.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/oidc/callback?"+url.Values{
		"code": {code}, "state": {authorize.Query().Get("state")},
	}.Encode(), nil)
	for _, c := range rec.Result().Cookies() {
		req.AddCookie(c)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Result()
}

// sessionCookie is the Session cookie a successful Login set.
func sessionCookie(t *testing.T, resp *http.Response) *http.Cookie {
	t.Helper()
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookieName && c.Value != "" {
			return c
		}
	}
	t.Fatalf("login answered %d with no Session cookie", resp.StatusCode)
	return nil
}

// webhookSignature mirrors Polaris's sign (internal/webhook/webhook.go): a
// Standard Webhooks v1 HMAC-SHA256 over "id.ts.body"; a whsec_ secret is
// base64 after the prefix, any other is used as its bytes.
func webhookSignature(secret, id, ts string, body []byte) string {
	key := []byte(secret)
	if k, ok := strings.CutPrefix(secret, "whsec_"); ok {
		if raw, err := base64.StdEncoding.DecodeString(k); err == nil {
			key = raw
		}
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id + "." + ts + "."))
	mac.Write(body)
	return "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// polarisEvent is one Polaris User event.
type polarisEvent struct {
	Type string
	Sub  string
	// At is the event's creation time; zero means now.
	At      time.Time
	Changed []string
}

// body is the exact JSON Polaris sends for e (its eventBody).
func (e polarisEvent) body() []byte {
	at := e.At
	if at.IsZero() {
		at = time.Now()
	}
	type data struct {
		Sub     string   `json:"sub"`
		Changed []string `json:"changed,omitempty"`
	}
	b, err := json.Marshal(struct {
		Type      string `json:"type"`
		Timestamp string `json:"timestamp"`
		Data      data   `json:"data"`
	}{e.Type, at.UTC().Format("2006-01-02T15:04:05Z"), data{e.Sub, e.Changed}})
	if err != nil {
		panic(err)
	}
	return b
}

// WebhookRequest is a delivery of e signed with secret at sendTime.
func (f *fakePolaris) WebhookRequest(e polarisEvent, secret string, sendTime time.Time) *http.Request {
	body := e.body()
	id, ts := "msg_"+uuid.NewString(), strconv.FormatInt(sendTime.Unix(), 10)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/polaris/webhook", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("webhook-id", id)
	req.Header.Set("webhook-timestamp", ts)
	req.Header.Set("webhook-signature", webhookSignature(secret, id, ts, body))
	return req
}

// Webhook delivers e to h signed with the shared secret and returns the
// status Polaris would see.
func (f *fakePolaris) Webhook(t *testing.T, h http.Handler, e polarisEvent) int {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, f.WebhookRequest(e, f.WebhookSecret, time.Now()))
	return rec.Code
}
