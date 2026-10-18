package api

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestParseAppHost(t *testing.T) {
	id := uuid.New()
	tests := []struct {
		name     string
		host     string
		domain   string
		wantID   uuid.UUID
		wantPort int
		wantOK   bool
	}{
		{"plain", "8080--" + id.String() + ".apps.test", "apps.test", id, 8080, true},
		{"host port", "8080--" + id.String() + ".apps.test:443", "apps.test", id, 8080, true},
		{"upper case", "8080--" + strings.ToUpper(id.String()) + ".APPS.TEST", "apps.test", id, 8080, true},
		{"trailing dot", "8080--" + id.String() + ".apps.test.", "apps.test", id, 8080, true},
		{"no domain configured", "8080--" + id.String() + ".apps.test", "", uuid.Nil, 0, false},
		{"other domain", "8080--" + id.String() + ".evil.test", "apps.test", uuid.Nil, 0, false},
		{"suffix trap", "8080--" + id.String() + ".apps.test.evil", "apps.test", uuid.Nil, 0, false},
		{"missing separator", id.String() + ".apps.test", "apps.test", uuid.Nil, 0, false},
		{"port zero", "0--" + id.String() + ".apps.test", "apps.test", uuid.Nil, 0, false},
		{"port too large", "70000--" + id.String() + ".apps.test", "apps.test", uuid.Nil, 0, false},
		{"port not numeric", "http--" + id.String() + ".apps.test", "apps.test", uuid.Nil, 0, false},
		{"not a uuid", "8080--nope.apps.test", "apps.test", uuid.Nil, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotID, gotPort, ok := parseAppHost(tt.host, tt.domain)
			if ok != tt.wantOK || gotID != tt.wantID || gotPort != tt.wantPort {
				t.Fatalf("parseAppHost(%q, %q) = %v, %d, %v; want %v, %d, %v",
					tt.host, tt.domain, gotID, gotPort, ok, tt.wantID, tt.wantPort, tt.wantOK)
			}
		})
	}
}

func TestPortTicket(t *testing.T) {
	s := &Server{cfg: Config{PortTicketKey: []byte("test-key")}}
	want := portTicket{
		SandboxID: uuid.New(),
		UserID:    uuid.New(),
		Port:      8080,
		ExpiresAt: time.Now().Add(time.Minute).Unix(),
	}
	raw := s.signPortTicket(want)

	got, ok := s.verifyPortTicket(raw)
	if !ok || got != want {
		t.Fatalf("round trip = %+v, %v; want %+v", got, ok, want)
	}

	payload, signature, _ := strings.Cut(raw, ".")
	if _, ok := s.verifyPortTicket(payload + "x." + signature); ok {
		t.Fatal("tampered payload verified")
	}
	// Flip the first character. The last base64 character carries unused low
	// bits, so some replacements decode to the same signature bytes.
	replacement := "A"
	if strings.HasPrefix(signature, replacement) {
		replacement = "B"
	}
	if _, ok := s.verifyPortTicket(payload + "." + replacement + signature[1:]); ok {
		t.Fatal("tampered signature verified")
	}
	if _, ok := s.verifyPortTicket(payload); ok {
		t.Fatal("unsigned ticket verified")
	}
	if _, ok := (&Server{cfg: Config{PortTicketKey: []byte("other")}}).verifyPortTicket(raw); ok {
		t.Fatal("ticket verified under another key")
	}

	expired := want
	expired.ExpiresAt = time.Now().Add(-time.Second).Unix()
	if _, ok := s.verifyPortTicket(s.signPortTicket(expired)); ok {
		t.Fatal("expired ticket verified")
	}
}
