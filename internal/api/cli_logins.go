package api

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/zibyn/orrery/internal/store"
)

// CLI login is a device authorization flow (ADR-0033, defaults.md).
const (
	cliLoginTTL        = 10 * time.Minute
	cliLoginInterval   = 5 * time.Second
	cliLoginMaxPending = 1000
)

// userCodeAlphabet has no vowels (no words) and no look-alike characters.
const userCodeAlphabet = "BCDFGHJKLMNPQRSTVWXZ"

type cliLoginDTO struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

type cliLoginTokenRequest struct {
	DeviceCode string `json:"device_code"`
}

type cliLoginTokenDTO struct {
	Status string `json:"status"`
	Token  string `json:"token,omitempty"`
}

type cliLoginApproveRequest struct {
	UserCode string `json:"user_code"`
}

func newUserCode() (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	out := make([]byte, 0, 9)
	for i, b := range buf {
		if i == 4 {
			out = append(out, '-')
		}
		// 256 % 20 != 0 skews a few letters slightly; 20^8 codes is plenty.
		out = append(out, userCodeAlphabet[int(b)%len(userCodeAlphabet)])
	}
	return string(out), nil
}

// normalizeUserCode accepts what a person types: any case, with or without
// the dash or spaces.
func normalizeUserCode(in string) (string, bool) {
	var b strings.Builder
	for _, r := range strings.ToUpper(in) {
		switch {
		case r == '-' || r == ' ':
		case strings.ContainsRune(userCodeAlphabet, r):
			b.WriteRune(r)
		default:
			return "", false
		}
	}
	s := b.String()
	if len(s) != 8 {
		return "", false
	}
	return s[:4] + "-" + s[4:], true
}

func (s *Server) handleCreateCLILogin(w http.ResponseWriter, r *http.Request, _ *principal) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		s.internal(w, "cli login device code", err)
		return
	}
	device := base64.RawURLEncoding.EncodeToString(buf)
	expires := time.Now().Add(cliLoginTTL)
	for range 3 { // a user_code collision is retried with a fresh code
		code, err := newUserCode()
		if err != nil {
			s.internal(w, "cli login user code", err)
			return
		}
		rows, err := s.q.CreateCLILogin(r.Context(), store.CreateCLILoginParams{
			DeviceHash: HashSecret(device), UserCode: code,
			ExpiresAt: timestamptzPtr(&expires), MaxPending: cliLoginMaxPending,
		})
		if isUniqueViolation(err) {
			continue
		}
		if err != nil {
			s.internal(w, "create cli login", err)
			return
		}
		if rows == 0 {
			writeProblem(w, "quota_exceeded", "too many CLI logins are pending; retry in a few minutes")
			return
		}
		writeJSON(w, http.StatusCreated, cliLoginDTO{
			DeviceCode:      device,
			UserCode:        code,
			VerificationURI: strings.TrimRight(s.cfg.AccessURL, "/") + "/cli-auth",
			ExpiresIn:       int(cliLoginTTL / time.Second),
			Interval:        int(cliLoginInterval / time.Second),
		})
		return
	}
	s.internal(w, "create cli login", errors.New("user code collisions"))
}

func (s *Server) handleApproveCLILogin(w http.ResponseWriter, r *http.Request, p *principal) {
	var req cliLoginApproveRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	code, ok := normalizeUserCode(req.UserCode)
	if !ok {
		writeProblem(w, "invalid_request", "user_code must be the 8-letter code the CLI printed")
		return
	}
	rows, err := s.q.ApproveCLILogin(r.Context(), store.ApproveCLILoginParams{UserCode: code, UserID: &p.user.ID})
	if err != nil {
		s.internal(w, "approve cli login", err)
		return
	}
	if rows == 0 {
		writeProblem(w, "not_found", "no pending CLI login has that code; it may have expired or been approved already")
		return
	}
	s.audit(r.Context(), p, "cli_login.approve", "user", p.user.ID.String(), map[string]any{"user_code": code})
	w.WriteHeader(http.StatusNoContent)
}

// handleCLILoginToken is the CLI's poll. The token is created only here, once,
// so an approval never leaves a plaintext token anywhere.
func (s *Server) handleCLILoginToken(w http.ResponseWriter, r *http.Request, _ *principal) {
	var req cliLoginTokenRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.DeviceCode == "" {
		writeProblem(w, "invalid_request", "device_code is required")
		return
	}
	ctx := r.Context()
	hash := HashSecret(req.DeviceCode)
	userID, err := s.q.CollectCLILogin(ctx, hash)
	if errors.Is(err, pgx.ErrNoRows) {
		if _, err := s.q.GetCLILogin(ctx, hash); errors.Is(err, pgx.ErrNoRows) {
			writeProblem(w, "not_found", "the CLI login expired or does not exist; run orrery login again")
			return
		} else if err != nil {
			s.internal(w, "get cli login", err)
			return
		}
		writeJSON(w, http.StatusOK, cliLoginTokenDTO{Status: "pending"})
		return
	}
	if err != nil {
		s.internal(w, "collect cli login", err)
		return
	}
	now := time.Now().UTC()
	name := fmt.Sprintf("cli-%s-%s", now.Format("20060102T150405"), uuid.NewString()[:8])
	token, raw, _, err := s.issueNamedToken(ctx, *userID, createTokenRequest{Name: name, Scope: "user"}, nil)
	if err != nil {
		// The row is gone: the CLI reports the failure and the User logs in again.
		s.internal(w, "create cli token", err)
		return
	}
	s.auditAs(ctx, userID, nil, "token.create", "token", token.ID.String(), map[string]any{
		"name": token.Name, "scope": token.Scope, "via": "cli_login",
	})
	writeJSON(w, http.StatusOK, cliLoginTokenDTO{Status: "approved", Token: raw})
}
