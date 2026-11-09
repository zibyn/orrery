package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	statusCreated       = "created"
	statusLinked        = "linked"
	statusAlreadyLinked = "already linked"
	statusMachine       = "machine"
	statusFailed        = "failed"
)

// Polaris's own username rule (identity.CheckUsername). Names are checked as
// they are, not lowercased: a changed name is reported, never made up.
var usernameRE = regexp.MustCompile(`^[a-z0-9._-]{3,32}$`)

// Polaris reads such an identifier as a +86 phone number, not a username.
var phoneRE = regexp.MustCompile(`^(?:\+?86)?(1[3-9]\d{9})$`)

type outcome struct {
	Name, Status, Sub, Reason string
}

type report struct {
	Users  []outcome
	Admins []string
}

type user struct {
	id                     string
	name, loginType        string
	email, issuer, subject *string
	isAdmin                bool
}

// run links every live User to a Polaris User (ADR-0029). It only ever reads
// the pre-switch schema, and writes only oidc_issuer and oidc_subject.
// Raw SQL, not sqlc: login_type and password_hash are gone from the current
// schema (migration 000029), so the generated queries cannot name them.
func run(ctx context.Context, pool *pgxpool.Pool, client *http.Client, issuer string) (report, error) {
	rows, err := pool.Query(ctx, `SELECT id::text, name, login_type, email, oidc_issuer, oidc_subject, is_admin
		FROM users WHERE deleted_at IS NULL ORDER BY name`)
	if err != nil {
		return report{}, err
	}
	var users []user
	for rows.Next() {
		var u user
		if err := rows.Scan(&u.id, &u.name, &u.loginType, &u.email, &u.issuer, &u.subject, &u.isAdmin); err != nil {
			return report{}, err
		}
		users = append(users, u)
	}
	if err := rows.Err(); err != nil {
		return report{}, err
	}

	p := &mgmt{client: client, base: strings.TrimRight(issuer, "/") + "/v1/management"}
	var r report
	for _, u := range users {
		o := outcome{Name: u.name}
		switch {
		case u.loginType == "none":
			o.Status = statusMachine
		case u.issuer != nil && *u.issuer == issuer && u.subject != nil:
			o.Status, o.Sub = statusAlreadyLinked, *u.subject
		case u.loginType == "password":
			o = p.importPassword(ctx, pool, u)
		default:
			o = p.linkByEmail(ctx, u)
		}
		if o.Sub != "" && (o.Status == statusCreated || o.Status == statusLinked) {
			if _, err := pool.Exec(ctx, `UPDATE users SET oidc_issuer = $1, oidc_subject = $2, updated_at = now() WHERE id = $3`,
				issuer, o.Sub, u.id); err != nil {
				o = outcome{Name: u.name, Status: statusFailed, Sub: o.Sub, Reason: "write link: " + err.Error()}
			}
		}
		r.Users = append(r.Users, o)
		if u.isAdmin {
			who := u.name + " (machine User, keeps is_admin)"
			if o.Status != statusMachine {
				who = u.name + " -> " + o.Sub
			}
			r.Admins = append(r.Admins, who)
		}
	}
	return r, nil
}

type mgmt struct {
	client *http.Client
	base   string
}

func (p *mgmt) importPassword(ctx context.Context, pool *pgxpool.Pool, u user) outcome {
	fail := func(reason string) outcome { return outcome{Name: u.name, Status: statusFailed, Reason: reason} }
	if !usernameRE.MatchString(u.name) || phoneRE.MatchString(u.name) {
		return fail("name is not a valid Polaris username (^[a-z0-9._-]{3,32}$, not a phone number): rename the User, then rerun")
	}
	var hash *string
	if err := pool.QueryRow(ctx, `SELECT password_hash FROM users WHERE id = $1`, u.id).Scan(&hash); err != nil {
		return fail(err.Error())
	}
	if hash == nil {
		return fail("password User has no password hash")
	}
	// Email first, so a 409 finds a person Polaris already knows by email.
	identifier, withUsername := u.name, false
	if u.email != nil && *u.email != "" {
		identifier, withUsername = strings.ToLower(*u.email), true
	}
	status, body, err := p.do(ctx, http.MethodPost, "/users", map[string]string{"identifier": identifier, "password_hash": *hash})
	if err != nil {
		return fail(err.Error())
	}
	var resp struct{ Sub, Detail string }
	json.Unmarshal(body, &resp) //nolint:errcheck // an empty sub is handled below
	switch {
	case status == http.StatusConflict && resp.Sub != "":
		return outcome{Name: u.name, Status: statusLinked, Sub: resp.Sub}
	case status != http.StatusCreated || resp.Sub == "":
		return fail(fmt.Sprintf("POST /users %s: %d %s", identifier, status, resp.Detail))
	}
	o := outcome{Name: u.name, Status: statusCreated, Sub: resp.Sub}
	if withUsername {
		status, body, err := p.do(ctx, http.MethodPut, "/users/"+url.PathEscape(resp.Sub)+"/identifiers/username", map[string]string{"value": u.name})
		if err != nil || status != http.StatusNoContent {
			// The Polaris User exists and is linked; only the username is missing.
			o.Reason = fmt.Sprintf("username not set (%d %v %s): set it in Polaris", status, err, body)
		}
	}
	return o
}

// linkByEmail links an OIDC User from a previous issuer. Every Polaris email
// identifier is verified, so an exact match is a verified one.
func (p *mgmt) linkByEmail(ctx context.Context, u user) outcome {
	fail := func(reason string) outcome { return outcome{Name: u.name, Status: statusFailed, Reason: reason} }
	if u.email == nil || *u.email == "" {
		return fail("OIDC User has no email to match a Polaris User by")
	}
	email := strings.ToLower(*u.email)
	status, body, err := p.do(ctx, http.MethodGet, "/users?limit=200&q="+url.QueryEscape(email), nil)
	if err != nil {
		return fail(err.Error())
	}
	if status != http.StatusOK {
		return fail(fmt.Sprintf("GET /users: %d %s", status, body))
	}
	var list struct {
		Users []struct {
			Sub         string `json:"sub"`
			Identifiers []struct{ Kind, Value string }
		}
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return fail("GET /users: " + err.Error())
	}
	// q is a substring search: keep exact email identifiers only.
	var subs []string
	for _, pu := range list.Users {
		for _, id := range pu.Identifiers {
			if id.Kind == "email" && strings.ToLower(id.Value) == email {
				subs = append(subs, pu.Sub)
			}
		}
	}
	if len(subs) != 1 {
		return fail(fmt.Sprintf("%d Polaris Users hold email %s", len(subs), email))
	}
	return outcome{Name: u.name, Status: statusLinked, Sub: subs[0]}
}

func (p *mgmt) do(ctx context.Context, method, path string, in any) (int, []byte, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return 0, nil, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.base+path, body)
	if err != nil {
		return 0, nil, err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, b, err
}

func (r report) write(w io.Writer) {
	for _, status := range []string{statusCreated, statusLinked, statusAlreadyLinked, statusMachine, statusFailed} {
		var n int
		for _, u := range r.Users {
			if u.Status == status {
				n++
			}
		}
		fmt.Fprintf(w, "%s: %d\n", status, n)
		for _, u := range r.Users {
			if u.Status != status {
				continue
			}
			line := "  " + u.Name
			if u.Sub != "" {
				line += " -> " + u.Sub
			}
			if u.Reason != "" {
				line += ": " + u.Reason
			}
			fmt.Fprintln(w, line)
		}
	}
	fmt.Fprintf(w, "admins (assign them the Polaris Orrery admin Role): %d\n", len(r.Admins))
	for _, a := range r.Admins {
		fmt.Fprintln(w, "  "+a)
	}
}

func (r report) failed() bool {
	for _, u := range r.Users {
		if u.Status == statusFailed {
			return true
		}
	}
	return false
}
