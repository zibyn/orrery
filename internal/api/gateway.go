package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	nodev1 "github.com/zibyn/stars-sandbox/gen/node/v1"
	"github.com/zibyn/stars-sandbox/internal/store"
)

const (
	// gatewayCookieName carries the exchanged ticket on a port subdomain.
	gatewayCookieName = "stars_port"
	// portTicketTTL is the signed URL's life (defaults.md, 60 s).
	portTicketTTL = time.Minute
	// portGrantTTL keeps a browser's cookie valid while it uses the app; the
	// cookie is re-signed, so a ticket minted for the URL need not live that long.
	portGrantTTL = time.Hour
)

// portTicket is the signed grant behind a port URL and its cookie. The ticket
// is a bearer credential: minting has already authorized the caller (owner or
// admin), and the signature keeps it unforgeable afterwards.
type portTicket struct {
	SandboxID uuid.UUID `json:"sandbox_id"`
	UserID    uuid.UUID `json:"user_id"`
	Port      int       `json:"port"`
	ExpiresAt int64     `json:"expires_at"`
}

// parseAppHost recognizes `<port>--<sandbox_id>.<apps_domain>` (api.md §6).
func parseAppHost(host, appsDomain string) (uuid.UUID, int, bool) {
	if appsDomain == "" {
		return uuid.Nil, 0, false
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	prefix, ok := strings.CutSuffix(host, "."+strings.ToLower(appsDomain))
	if !ok {
		return uuid.Nil, 0, false
	}
	// A Sandbox id only contains single hyphens, so the first `--` separates
	// the port from the id.
	portRaw, idRaw, ok := strings.Cut(prefix, "--")
	if !ok {
		return uuid.Nil, 0, false
	}
	port, err := strconv.Atoi(portRaw)
	if err != nil || port < 1 || port > 65535 {
		return uuid.Nil, 0, false
	}
	id, err := uuid.Parse(idRaw)
	if err != nil {
		return uuid.Nil, 0, false
	}
	return id, port, true
}

func (s *Server) signPortTicket(t portTicket) string {
	payload, _ := json.Marshal(t)
	mac := hmac.New(sha256.New, s.cfg.PortTicketKey)
	mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *Server) verifyPortTicket(raw string) (portTicket, bool) {
	var t portTicket
	encoded, signature, ok := strings.Cut(raw, ".")
	if !ok {
		return t, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return t, false
	}
	got, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil {
		return t, false
	}
	mac := hmac.New(sha256.New, s.cfg.PortTicketKey)
	mac.Write(payload)
	if !hmac.Equal(got, mac.Sum(nil)) {
		return t, false
	}
	if json.Unmarshal(payload, &t) != nil || t.ExpiresAt == 0 {
		return portTicket{}, false
	}
	if time.Now().Unix() > t.ExpiresAt {
		return portTicket{}, false
	}
	return t, true
}

// handlePortTicket issues the signed URL a browser opens for a Sandbox port
// (api.md §6). The owner or an admin can ask for one; everyone else sees 404.
func (s *Server) handlePortTicket(w http.ResponseWriter, r *http.Request, p *principal) {
	id, ok := pathUUID(r, "id")
	if !ok {
		writeProblem(w, "invalid_request", "id must be a UUID")
		return
	}
	port, ok := pathPort(w, r)
	if !ok {
		return
	}
	sb, ok := s.getOwnedSandbox(w, r.Context(), p, id)
	if !ok {
		return
	}
	if sb.State != "ready" {
		writeProblem(w, "conflict", "the Sandbox is not ready")
		return
	}
	if s.cfg.AppsDomain == "" {
		writeProblem(w, "unavailable", "port forwarding is not configured: set STARS_APPS_DOMAIN")
		return
	}
	t := portTicket{
		SandboxID: id,
		UserID:    p.user.ID,
		Port:      port,
		ExpiresAt: time.Now().Add(portTicketTTL).Unix(),
	}
	host := strconv.Itoa(port) + "--" + id.String() + "." + s.cfg.AppsDomain
	writeJSON(w, http.StatusOK, map[string]string{
		"url": "https://" + host + "/?ticket=" + url.QueryEscape(s.signPortTicket(t)),
	})
}

func pathPort(w http.ResponseWriter, r *http.Request) (int, bool) {
	port, err := strconv.Atoi(r.PathValue("port"))
	if err != nil || port < 1 || port > 65535 {
		writeProblem(w, "invalid_request", "port must be an integer in 1-65535")
		return 0, false
	}
	return port, true
}

// servePortGateway is the Gateway side of a port subdomain (api.md §6). A
// ticket in the query is exchanged for a cookie, which then authorizes the
// browser; the platform credentials never reach the Sandbox.
func (s *Server) servePortGateway(w http.ResponseWriter, r *http.Request, sandboxID uuid.UUID, port int) {
	if raw := r.URL.Query().Get("ticket"); raw != "" {
		ticket, ok := s.verifyPortTicket(raw)
		if !ok || ticket.SandboxID != sandboxID || ticket.Port != port {
			writeProblem(w, "not_found", "no such Sandbox")
			return
		}
		if _, ok := s.gatewaySandbox(w, r, sandboxID); !ok {
			return
		}
		ticket.ExpiresAt = time.Now().Add(portGrantTTL).Unix()
		http.SetCookie(w, &http.Cookie{
			Name:     gatewayCookieName,
			Value:    s.signPortTicket(ticket),
			Path:     "/",
			HttpOnly: true,
			Secure:   requestIsSecure(r),
			SameSite: http.SameSiteLaxMode,
			MaxAge:   int(portGrantTTL.Seconds()),
		})
		// The ticket must not stay in the URL the app sees: send the browser
		// back with the cookie instead.
		target := *r.URL
		q := target.Query()
		q.Del("ticket")
		target.RawQuery = q.Encode()
		http.Redirect(w, r, target.String(), http.StatusSeeOther)
		return
	}

	cookie, err := r.Cookie(gatewayCookieName)
	if err != nil {
		writeProblem(w, "not_found", "no such Sandbox")
		return
	}
	ticket, ok := s.verifyPortTicket(cookie.Value)
	if !ok || ticket.SandboxID != sandboxID || ticket.Port != port {
		writeProblem(w, "not_found", "no such Sandbox")
		return
	}
	sb, ok := s.gatewaySandbox(w, r, sandboxID)
	if !ok {
		return
	}
	if sb.State != "ready" {
		writeProblem(w, "unavailable", "the Sandbox is not ready")
		return
	}
	if sb.NodeID == nil {
		writeProblem(w, "unavailable", "the Sandbox's Node is not connected")
		return
	}
	s.activity.begin(sandboxID)
	defer s.activity.end(sandboxID)
	conn, ok := s.openTunnel(w, r, *sb.NodeID, sandboxPortTarget(sandboxID, port))
	if !ok {
		return
	}
	defer conn.Close() //nolint:errcheck // one request per tunnel
	proxyPort(w, r, conn)
}

// gatewaySandbox resolves the ticket's Sandbox. The ticket was only minted
// after the caller passed the ownership check, so the signature is enough.
func (s *Server) gatewaySandbox(w http.ResponseWriter, r *http.Request, id uuid.UUID) (store.Sandbox, bool) {
	sb, err := s.q.GetSandbox(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeProblem(w, "not_found", "no such Sandbox")
		return store.Sandbox{}, false
	}
	if err != nil {
		s.internal(w, "get Sandbox", err)
		return store.Sandbox{}, false
	}
	return sb, true
}

func requestIsSecure(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// proxyPort forwards one Gateway request onto the Sandbox's port over the
// tunnel. Upgrades (WebSocket) are relayed raw; plain HTTP is re-framed so a
// failed dial can still answer 503 with a fresh response.
func proxyPort(w http.ResponseWriter, r *http.Request, conn net.Conn) {
	if isWebSocketUpgrade(r) {
		forward := forwardedRequest(r, true)
		dropTicketParam(forward)
		proxyUpgrade(w, conn, forward)
		return
	}
	forward := forwardedRequest(r, true)
	for k := range hopByHop {
		forward.Header.Del(k)
	}
	forward.Header.Del("Expect")
	forward.Header.Set("Connection", "close")
	dropTicketParam(forward)
	relayHTTP(w, conn, forward, "the Sandbox port did not answer")
}

func dropTicketParam(r *http.Request) {
	q := r.URL.Query()
	if q.Has("ticket") {
		q.Del("ticket")
		r.URL.RawQuery = q.Encode()
	}
}

// sandboxPortTarget builds the OpenTunnel target for an Agent that reaches the
// Sandbox's port inside its network namespace (node-protocol.md).
func sandboxPortTarget(sandboxID uuid.UUID, port int) *nodev1.OpenTunnel {
	return &nodev1.OpenTunnel{Target: &nodev1.OpenTunnel_SandboxPort{
		SandboxPort: &nodev1.SandboxPort{SandboxId: sandboxID.String(), Port: int32(port)},
	}}
}
