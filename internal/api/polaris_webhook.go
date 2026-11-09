package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/zibyn/orrery/internal/store"
)

// webhookTolerance is how far webhook-timestamp may be from now. Polaris
// defines none, so this is the Standard Webhooks default; Polaris re-stamps
// every retry, so it never blocks redelivery.
const webhookTolerance = 5 * time.Minute

// polarisWebhookEvent is a Polaris User event (Polaris protocol.md#webhook).
type polarisWebhookEvent struct {
	Type      string    `json:"type"`
	Timestamp time.Time `json:"timestamp"`
	Data      struct {
		Sub     string   `json:"sub"`
		Changed []string `json:"changed"`
	} `json:"data"`
}

// handlePolarisWebhook applies a Polaris User event to the Polaris-linked
// User it names (ADR-0029). Polaris retries any non-2xx, so everything Orrery
// will never act on (unknown sub or type, a stale event) is acknowledged.
func (s *Server) handlePolarisWebhook(w http.ResponseWriter, r *http.Request, _ *principal) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		writeProblem(w, "invalid_request", "the webhook body is unreadable")
		return
	}
	if !validWebhookSignature(s.cfg.OIDC.WebhookSecret, r.Header, body, time.Now()) {
		writeProblem(w, "unauthenticated", "the webhook signature is invalid")
		return
	}
	var e polarisWebhookEvent
	if err := json.Unmarshal(body, &e); err != nil || e.Data.Sub == "" || e.Timestamp.IsZero() {
		writeProblem(w, "invalid_request", "the webhook body is not a Polaris User event")
		return
	}
	params := store.ApplyPolarisEventParams{
		EventAt: pgtype.Timestamptz{Time: e.Timestamp, Valid: true},
		Issuer:  &s.cfg.OIDC.IssuerURL,
		Subject: &e.Data.Sub,
	}
	var action string
	switch e.Type {
	case "user.disabled":
		action, params.Disabled = "user.disable", new(true)
	case "user.enabled":
		action, params.Disabled = "user.enable", new(false)
	case "user.deleted":
		action = "user.delete"
	case "user.updated":
		// Events are thin: a Role change only says roles changed, so admin is
		// cleared and the next login re-grants it (fail closed).
		if !slices.Contains(e.Data.Changed, "roles") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		action, params.ClearAdmin = "user.update", true
	default:
		w.WriteHeader(http.StatusNoContent)
		return
	}
	ctx := r.Context()
	user, err := s.q.ApplyPolarisEvent(ctx, params)
	if errors.Is(err, pgx.ErrNoRows) { // unknown sub, machine User or stale event
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		s.internal(w, "apply Polaris event", err)
		return
	}
	if e.Type == "user.deleted" {
		if _, err := s.q.SoftDeleteUser(ctx, user.ID); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			s.internal(w, "delete user", err)
			return
		}
	}
	s.auditAs(ctx, nil, nil, action, "user", user.ID.String(), map[string]any{
		"via": "polaris_webhook", "event": e.Type, "is_admin": user.IsAdmin,
	})
	w.WriteHeader(http.StatusNoContent)
}

// validWebhookSignature checks a Standard Webhooks signature the way Polaris
// makes it: v1 HMAC-SHA256 over "id.timestamp.body", keyed by the base64
// after whsec_ or else by the secret's bytes.
func validWebhookSignature(secret string, h http.Header, body []byte, now time.Time) bool {
	id, ts := h.Get("webhook-id"), h.Get("webhook-timestamp")
	sent, err := strconv.ParseInt(ts, 10, 64)
	if secret == "" || id == "" || err != nil || now.Sub(time.Unix(sent, 0)).Abs() > webhookTolerance {
		return false
	}
	key := []byte(secret)
	if k, ok := strings.CutPrefix(secret, "whsec_"); ok {
		if raw, err := base64.StdEncoding.DecodeString(k); err == nil {
			key = raw
		}
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id + "." + ts + "."))
	mac.Write(body)
	want := mac.Sum(nil)
	// The header may list several signatures, space separated.
	for _, sig := range strings.Fields(h.Get("webhook-signature")) {
		if v, ok := strings.CutPrefix(sig, "v1,"); ok {
			if got, err := base64.StdEncoding.DecodeString(v); err == nil && hmac.Equal(got, want) {
				return true
			}
		}
	}
	return false
}
