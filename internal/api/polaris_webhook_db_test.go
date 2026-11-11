package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The fake signs exactly as Polaris does: Polaris's own test vector.
func TestFakePolarisWebhookSignatureMatchesPolaris(t *testing.T) {
	got := webhookSignature("whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw", "msg_p5jXN8AQM9LWM0D4loKWxJek", "1614265330", []byte(`{"test": 2432232314}`))
	if want := "v1,g0hM9SsE+OTPJTGt/tmIKtSyZlE3uFJELVlNIOLJ1OE="; got != want {
		t.Fatalf("signature = %s, want %s", got, want)
	}
}

// A webhook that is not genuinely from Polaris is refused and changes nothing.
func TestPolarisWebhookRejectsBadSignature(t *testing.T) {
	h, op, _ := polarisTestServer(t)
	sub := "sig-" + uuid.NewString()
	session := sessionCookie(t, op.Login(t, h, polarisLogin{Sub: sub}))
	disable := polarisEvent{Type: "user.disabled", Sub: sub}

	tampered := op.WebhookRequest(disable, op.WebhookSecret, time.Now())
	tampered.Header.Set("webhook-id", "msg_other")
	for name, req := range map[string]*http.Request{
		"wrong secret": op.WebhookRequest(disable, "whsec_d3Jvbmc=", time.Now()),
		"tampered":     tampered,
		"replayed":     op.WebhookRequest(disable, op.WebhookSecret, time.Now().Add(-10*time.Minute)),
		"unsigned":     httptest.NewRequest(http.MethodPost, "/api/v1/polaris/webhook", nil),
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s webhook = %d, want 401", name, rec.Code)
		}
	}
	if code := asSession(t, h, session, "/api/v1/users/me", nil); code != http.StatusOK {
		t.Fatalf("Session after rejected webhooks = %d, want 200", code)
	}
}

// scopedToken mints an API Token of scope through the Session.
func scopedToken(t *testing.T, h http.Handler, session *http.Cookie, scope string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/users/me/tokens",
		strings.NewReader(`{"name":"t-`+uuid.NewString()[:8]+`","scope":"`+scope+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.AddCookie(session)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var created createdTokenDTO
	if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &created) != nil {
		t.Fatalf("POST /users/me/tokens = %d: %s", rec.Code, rec.Body)
	}
	return created.Token
}

// callBearer calls the API with a Bearer token and returns the status.
func callBearer(h http.Handler, token, method, path, body string) int {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

// sandboxToken gives the User a Sandbox and returns its Sandbox Token.
func sandboxToken(t *testing.T, pool *pgxpool.Pool, owner uuid.UUID) string {
	t.Helper()
	s := &Server{cfg: Config{EncryptionKeys: testKeyring(t)}}
	ct, keyID, err := s.issueSandboxToken(owner)
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	if _, err := pool.Exec(context.Background(), `INSERT INTO sandboxes
		(id, owner_id, name, snapshot_digest, cpu, memory, token_ct, token_key_id)
		VALUES ($1, $2, $3, 'sha256:x', 1, 1, $4, $5)`, id, owner, "sb-"+id.String()[:8], ct, keyID); err != nil {
		t.Fatal(err)
	}
	secret, err := sandboxTokenSecret(s.cfg.EncryptionKeys, owner, ct, keyID)
	if err != nil {
		t.Fatal(err)
	}
	return SandboxToken(id, secret)
}

func noSessionCookie(t *testing.T, resp *http.Response) {
	t.Helper()
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookieName && c.Value != "" {
			t.Error("login set a Session cookie")
		}
	}
}

// Disabling a person in Polaris rejects every Orrery credential of theirs and
// refuses a new login; enabling them restores their API Token.
func TestPolarisWebhookDisableAndEnable(t *testing.T) {
	h, op, pool := polarisTestServer(t)
	sub := "dis-" + uuid.NewString()
	session := sessionCookie(t, op.Login(t, h, polarisLogin{Sub: sub}))
	user := me(t, h, session)
	token := scopedToken(t, h, session, "user")
	sandbox := sandboxToken(t, pool, uuid.MustParse(user.ID))
	// An authenticated Sandbox Token gets 404 for an unknown provider.
	credential := func() int {
		return callBearer(h, sandbox, http.MethodPost, "/api/v1/git/credential", `{"provider":"none"}`)
	}
	if code := credential(); code != http.StatusNotFound {
		t.Fatalf("Sandbox Token before disable = %d, want 404", code)
	}

	if code := op.Webhook(t, h, polarisEvent{Type: "user.disabled", Sub: sub}); code != http.StatusNoContent {
		t.Fatalf("user.disabled webhook = %d, want 204", code)
	}
	if code := asSession(t, h, session, "/api/v1/users/me", nil); code != http.StatusUnauthorized {
		t.Errorf("Session of a disabled User = %d, want 401", code)
	}
	if code := callBearer(h, token, http.MethodGet, "/api/v1/users/me", ""); code != http.StatusUnauthorized {
		t.Errorf("API Token of a disabled User = %d, want 401", code)
	}
	if code := credential(); code != http.StatusUnauthorized {
		t.Errorf("Sandbox Token of a disabled User = %d, want 401", code)
	}
	resp := op.Login(t, h, polarisLogin{Sub: sub})
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("login of a disabled User = %d, want 403", resp.StatusCode)
	}
	noSessionCookie(t, resp)

	// A later second: at the same second the disable would win.
	if code := op.Webhook(t, h, polarisEvent{Type: "user.enabled", Sub: sub, At: time.Now().Add(time.Second)}); code != http.StatusNoContent {
		t.Fatalf("user.enabled webhook = %d, want 204", code)
	}
	if code := callBearer(h, token, http.MethodGet, "/api/v1/users/me", ""); code != http.StatusOK {
		t.Fatalf("API Token after enable = %d, want 200", code)
	}
}

// Deleting a person in Polaris deletes their User as DELETE /users/{id} does.
func TestPolarisWebhookDeleteRemovesUser(t *testing.T) {
	h, op, pool := polarisTestServer(t)
	admin := sessionCookie(t, op.Login(t, h, polarisLogin{Sub: "adm-" + uuid.NewString(), Entitlements: []string{adminEntitlement}}))
	sub := "del-" + uuid.NewString()
	session := sessionCookie(t, op.Login(t, h, polarisLogin{Sub: sub}))
	user := me(t, h, session)
	sandbox := sandboxToken(t, pool, uuid.MustParse(user.ID))
	credential := func() int {
		return callBearer(h, sandbox, http.MethodPost, "/api/v1/git/credential", `{"provider":"none"}`)
	}
	if code := credential(); code != http.StatusNotFound {
		t.Fatalf("Sandbox Token before delete = %d, want 404", code)
	}

	if code := op.Webhook(t, h, polarisEvent{Type: "user.deleted", Sub: sub}); code != http.StatusNoContent {
		t.Fatalf("user.deleted webhook = %d, want 204", code)
	}
	if code := asSession(t, h, admin, "/api/v1/users/"+user.ID, nil); code != http.StatusNotFound {
		t.Errorf("GET /users/{id} of a deleted User = %d, want 404", code)
	}
	if code := asSession(t, h, session, "/api/v1/users/me", nil); code != http.StatusUnauthorized {
		t.Errorf("Session of a deleted User = %d, want 401", code)
	}
	// The deleted User's Sandbox row outlives them; its token must not.
	if code := credential(); code != http.StatusUnauthorized {
		t.Errorf("Sandbox Token of a deleted User = %d, want 401", code)
	}
}

// A Role change in Polaris removes admin authority at once, from the Session
// and from an admin-scoped API Token; the next login decides it again.
func TestPolarisWebhookRolesChangeClearsAdmin(t *testing.T) {
	h, op, _ := polarisTestServer(t)
	sub := "roles-" + uuid.NewString()
	session := sessionCookie(t, op.Login(t, h, polarisLogin{Sub: sub, Entitlements: []string{adminEntitlement}}))
	token := scopedToken(t, h, session, "admin")

	// Other profile changes leave admin alone.
	if code := op.Webhook(t, h, polarisEvent{Type: "user.updated", Sub: sub, Changed: []string{"email"}}); code != http.StatusNoContent {
		t.Fatalf("user.updated{email} webhook = %d, want 204", code)
	}
	if code := callBearer(h, token, http.MethodGet, "/api/v1/users", ""); code != http.StatusOK {
		t.Fatalf("admin token after an email change = %d, want 200", code)
	}

	if code := op.Webhook(t, h, polarisEvent{Type: "user.updated", Sub: sub, Changed: []string{"profile", "roles"}}); code != http.StatusNoContent {
		t.Fatalf("user.updated{roles} webhook = %d, want 204", code)
	}
	if code := asSession(t, h, session, "/api/v1/users", nil); code != http.StatusForbidden {
		t.Errorf("Session GET /users after a Role change = %d, want 403", code)
	}
	if code := callBearer(h, token, http.MethodGet, "/api/v1/users", ""); code != http.StatusForbidden {
		t.Errorf("admin token GET /users after a Role change = %d, want 403", code)
	}
}

// Polaris stamps events to the second and delivers them in any order, so at
// one second a disable beats an enable whichever arrives first (ADR-0029).
func TestPolarisWebhookSameSecondDisableBeatsEnable(t *testing.T) {
	h, op, _ := polarisTestServer(t)
	at := time.Now().Truncate(time.Second)
	for name, order := range map[string][]string{
		"disable first": {"user.disabled", "user.enabled"},
		"enable first":  {"user.enabled", "user.disabled"},
	} {
		sub := "tie-" + uuid.NewString()
		token := scopedToken(t, h, sessionCookie(t, op.Login(t, h, polarisLogin{Sub: sub})), "user")
		for _, typ := range order {
			if code := op.Webhook(t, h, polarisEvent{Type: typ, Sub: sub, At: at}); code != http.StatusNoContent {
				t.Fatalf("%s: %s webhook = %d, want 204", name, typ, code)
			}
		}
		if code := callBearer(h, token, http.MethodGet, "/api/v1/users/me", ""); code != http.StatusUnauthorized {
			t.Errorf("%s: API Token = %d, want 401 (disabled)", name, code)
		}
	}
}

// auditActions lists the actions of the User's Audit Records.
func auditActions(t *testing.T, h http.Handler, admin *http.Cookie, userID string) []string {
	t.Helper()
	var list auditList
	if code := asSession(t, h, admin, "/api/v1/audit?target_type=user&target="+userID, &list); code != http.StatusOK {
		t.Fatalf("GET /audit = %d", code)
	}
	var actions []string
	for _, a := range list.Items {
		actions = append(actions, a.Action)
	}
	return actions
}

// A redelivery (the same webhook-id) changes nothing, even while its event is
// still current: it leaves no second Audit Record.
func TestPolarisWebhookRedeliveryChangesNothing(t *testing.T) {
	h, op, _ := polarisTestServer(t)
	admin := sessionCookie(t, op.Login(t, h, polarisLogin{Sub: "adm-" + uuid.NewString(), Entitlements: []string{adminEntitlement}}))
	sub := "redeliver-" + uuid.NewString()
	user := me(t, h, sessionCookie(t, op.Login(t, h, polarisLogin{Sub: sub, Entitlements: []string{adminEntitlement}})))
	roles := polarisEvent{Type: "user.updated", Sub: sub, Changed: []string{"roles"}, At: time.Now(), ID: "msg_" + uuid.NewString()}

	if code := op.Webhook(t, h, roles); code != http.StatusNoContent {
		t.Fatalf("user.updated webhook = %d, want 204", code)
	}
	if code := op.Webhook(t, h, roles); code != http.StatusNoContent {
		t.Fatalf("redelivered webhook = %d, want 204", code)
	}
	if got := auditActions(t, h, admin, user.ID); !slices.Equal(got, []string{"user.update", "user.create"}) {
		t.Errorf("Audit Records = %v, want one user.update after user.create", got)
	}
}

// A user.deleted whose delete fails leaves no trace, so Polaris's retry (the
// same webhook-id) still deletes the User.
func TestPolarisWebhookDeleteRetryAfterFailure(t *testing.T) {
	h, op, pool := polarisTestServer(t)
	admin := sessionCookie(t, op.Login(t, h, polarisLogin{Sub: "adm-" + uuid.NewString(), Entitlements: []string{adminEntitlement}}))
	sub := "delretry-" + uuid.NewString()
	user := me(t, h, sessionCookie(t, op.Login(t, h, polarisLogin{Sub: sub})))
	ctx := context.Background()
	// The trigger fails the soft delete of this one User only.
	suffix := strings.ReplaceAll(user.ID, "-", "")
	fn, trg := "fail_delete_"+suffix, "fail_delete_"+suffix
	for _, sql := range []string{
		`CREATE FUNCTION ` + fn + `() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
			IF NEW.id = '` + user.ID + `' AND NEW.deleted_at IS NOT NULL THEN RAISE EXCEPTION 'injected'; END IF;
			RETURN NEW; END $$`,
		`CREATE TRIGGER ` + trg + ` BEFORE UPDATE ON users FOR EACH ROW EXECUTE FUNCTION ` + fn + `()`,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	drop := func() {
		pool.Exec(ctx, `DROP TRIGGER IF EXISTS `+trg+` ON users`) //nolint:errcheck // test cleanup
		pool.Exec(ctx, `DROP FUNCTION IF EXISTS `+fn+`()`)        //nolint:errcheck // test cleanup
	}
	t.Cleanup(drop)

	del := polarisEvent{Type: "user.deleted", Sub: sub, At: time.Now(), ID: "msg_" + uuid.NewString()}
	if code := op.Webhook(t, h, del); code != http.StatusInternalServerError {
		t.Fatalf("failing user.deleted webhook = %d, want 500", code)
	}
	drop()
	if code := op.Webhook(t, h, del); code != http.StatusNoContent {
		t.Fatalf("retried user.deleted webhook = %d, want 204", code)
	}
	if code := asSession(t, h, admin, "/api/v1/users/"+user.ID, nil); code != http.StatusNotFound {
		t.Errorf("GET /users/{id} after the retried delete = %d, want 404", code)
	}
}

// Redelivered, reordered and unknown events, unknown subs and machine Users
// are acknowledged and change nothing (ADR-0029).
func TestPolarisWebhookIgnoresWhatItWillNeverApply(t *testing.T) {
	h, op, pool := polarisTestServer(t)
	sub := "stale-" + uuid.NewString()
	token := scopedToken(t, h, sessionCookie(t, op.Login(t, h, polarisLogin{Sub: sub})), "user")
	now := time.Now()

	for _, e := range []polarisEvent{
		{Type: "user.disabled", Sub: sub, At: now},
		{Type: "user.disabled", Sub: sub, At: now},                             // duplicate
		{Type: "user.enabled", Sub: sub, At: now.Add(-time.Minute)},            // stale
		{Type: "user.enabled.v2", Sub: sub, At: now.Add(time.Minute)},          // unknown type
		{Type: "user.enabled", Sub: "nobody-" + sub, At: now.Add(time.Minute)}, // unknown sub
	} {
		if code := op.Webhook(t, h, e); code != http.StatusNoContent {
			t.Fatalf("%+v webhook = %d, want 204", e, code)
		}
	}
	if code := callBearer(h, token, http.MethodGet, "/api/v1/users/me", ""); code != http.StatusUnauthorized {
		t.Fatalf("API Token after a stale enable = %d, want 401 (still disabled)", code)
	}

	// A machine User has no Polaris identity, so no event can name it, not
	// even by its name or id.
	s := New(Config{OIDC: OIDCConfig{IssuerURL: op.URL}}, pool, slog.New(slog.DiscardHandler))
	name := "bot-" + uuid.NewString()[:8]
	machine, err := s.UserToken(context.Background(), UserTokenOptions{Name: name, Scope: "user"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/users/me", nil)
	req.Header.Set("Authorization", "Bearer "+machine.Token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var bot userDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &bot); err != nil {
		t.Fatal(err)
	}
	for _, sub := range []string{name, bot.ID} {
		if code := op.Webhook(t, h, polarisEvent{Type: "user.disabled", Sub: sub}); code != http.StatusNoContent {
			t.Fatalf("webhook naming a machine User = %d, want 204", code)
		}
	}
	if code := callBearer(h, machine.Token, http.MethodGet, "/api/v1/users/me", ""); code != http.StatusOK {
		t.Fatalf("machine User's token after webhooks = %d, want 200", code)
	}
}
