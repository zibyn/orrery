// Package client is the public, hand-written Go SDK for the orrery
// REST API (api.md §9). It uses only the standard library.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client speaks the public REST API with one API Token.
type Client struct {
	baseURL   string
	token     string
	http      *http.Client
	transfers *http.Client
}

// waitForTimeout is the per-call deadline for GET ?wait_for=. The API
// long-polls for up to 60 seconds, so the default 30-second JSON timeout
// would cut a valid wait short.
const waitForTimeout = 90 * time.Second

// New creates a Client. baseURL is the API origin, e.g. https://orrery.example.
func New(baseURL, token string) *Client {
	return &Client{
		baseURL:   strings.TrimRight(baseURL, "/"),
		token:     token,
		http:      &http.Client{Timeout: 30 * time.Second},
		transfers: &http.Client{},
	}
}

// Error is a problem+json answer (api.md §2). Detail is the problem's
// detail, or the status line when the body was not a problem document.
type Error struct {
	Status int
	Code   string
	Detail string
	// AuthorizeURL is the extension POST /git/credential carries when the
	// owner has no linked Git account.
	AuthorizeURL string
}

// Error implements error. It prefers "code: detail", then whichever is set.
func (e *Error) Error() string {
	switch {
	case e.Code == "":
		return e.Detail
	case e.Detail == "":
		return e.Code
	default:
		return e.Code + ": " + e.Detail
	}
}

// Token returns the API Token the Client authenticates with.
func (c *Client) Token() string { return c.token }

// BaseURL returns the API origin, without a trailing slash.
func (c *Client) BaseURL() string { return c.baseURL }

// Origin returns the HTTP origin of the API, used as a WebSocket Origin.
func (c *Client) Origin() string {
	u, err := url.Parse(c.baseURL)
	if err != nil {
		return c.baseURL
	}
	return u.Scheme + "://" + u.Host
}

// WebSocketURL builds the ws:// or wss:// URL for an API path.
func (c *Client) WebSocketURL(path string) string {
	u, err := url.Parse(c.baseURL)
	if err != nil {
		return c.baseURL + path
	}
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	u.Path = path
	return u.String()
}

// PTYURL is the WebSocket endpoint for an Exec's terminal (api.md §4).
func (c *Client) PTYURL(execID string) string {
	return c.WebSocketURL("/api/v1/execs/" + url.PathEscape(execID) + "/pty")
}

// TunnelURL is the WebSocket endpoint for a Sandbox port tunnel (api.md §6).
func (c *Client) TunnelURL(sandboxID string, port int) string {
	return c.WebSocketURL("/api/v1/sandboxes/" + url.PathEscape(sandboxID) +
		"/ports/" + fmt.Sprint(port) + "/tunnel")
}

// do performs one JSON request and decodes a successful answer into out.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any, idempotencyKey string) error {
	return c.doWithHeaders(ctx, method, path, query, body, out, idempotencyKey, nil)
}

func (c *Client) sendHeaders(hc *http.Client, ctx context.Context, method, path string, query url.Values, body any, idempotencyKey string, headers map[string]string) (int, []byte, error) {
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		payload = bytes.NewReader(encoded)
	}
	req, err := c.newRequest(ctx, method, path, query, payload)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	for name, value := range headers {
		if value != "" {
			req.Header.Set(name, value)
		}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, data, nil
}

func (c *Client) newRequest(ctx context.Context, method, path string, query url.Values, body io.Reader) (*http.Request, error) {
	endpoint := c.baseURL + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	return req, nil
}

func parseError(status int, data []byte) *Error {
	var p struct {
		Code         string `json:"code"`
		Detail       string `json:"detail"`
		AuthorizeURL string `json:"authorize_url"`
	}
	if json.Unmarshal(data, &p) == nil && p.Code != "" {
		return &Error{Status: status, Code: p.Code, Detail: p.Detail, AuthorizeURL: p.AuthorizeURL}
	}
	return &Error{Status: status, Detail: fmt.Sprintf("HTTP %d: %s", status, strings.TrimSpace(string(data)))}
}
