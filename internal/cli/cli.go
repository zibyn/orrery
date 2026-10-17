package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The external CLI speaks only the public REST API (ADR-0017).
type fileConfig struct {
	APIURL string `json:"api_url"`
	Token  string `json:"token"`
}

type client struct {
	base  string
	token string
	http  *http.Client
	out   io.Writer
}

func Run(args []string, stdout, stderr io.Writer) int {
	global := flag.NewFlagSet("stars-sandbox", flag.ContinueOnError)
	global.SetOutput(stderr)
	configPath := global.String("config", "", "path to the config file")
	apiURL := global.String("api-url", "", "API base URL")
	token := global.String("token", "", "API token")
	if err := global.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			usage(stderr)
			return 0
		}
		return 2
	}
	rest := global.Args()
	if len(rest) == 0 || rest[0] == "help" {
		usage(stderr)
		return 0
	}

	path := resolveConfigPath(*configPath)
	fc, err := loadConfig(path, *configPath != "")
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	base := firstNonEmpty(*apiURL, os.Getenv("STARS_SANDBOX_API_URL"), fc.APIURL, "http://localhost:8080")
	tok := firstNonEmpty(*token, os.Getenv("STARS_SANDBOX_TOKEN"), fc.Token)
	if tok == "" {
		fmt.Fprintf(stderr, "error: no API token configured; set STARS_SANDBOX_TOKEN, pass --token, or write one to %s\n", path)
		return 1
	}
	c := &client{base: base, token: tok, http: &http.Client{Timeout: 30 * time.Second}, out: stdout}

	switch rest[0] {
	case "user":
		err = runUser(c, rest[1:], stderr)
	case "token":
		err = runToken(c, rest[1:], stderr)
	case "audit":
		err = runAudit(c, rest[1:], stderr)
	default:
		err = fmt.Errorf("unknown command %q", rest[0])
	}
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	return 0
}

func usage(w io.Writer) {
	fmt.Fprint(w, `usage: stars-sandbox [global flags] <command> [flags]

commands:
  user list|get|create|update|delete|quota
  token list|create|revoke
  audit list

global flags:
  --config PATH    config file (default $STARS_SANDBOX_CONFIG or $XDG_CONFIG_HOME/stars-sandbox/config.json)
  --api-url URL    API base URL (default $STARS_SANDBOX_API_URL or the config file)
  --token TOKEN    API token (default $STARS_SANDBOX_TOKEN or the config file)

examples:
  stars-sandbox user list
  stars-sandbox user create --name alice --login-type password --password-stdin
  stars-sandbox token create --name ci --scope read_only
  stars-sandbox audit list --action token.create --limit 20
`)
}

func resolveConfigPath(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if v := os.Getenv("STARS_SANDBOX_CONFIG"); v != "" {
		return v
	}
	if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
		return filepath.Join(v, "stars-sandbox", "config.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "stars-sandbox.json"
	}
	return filepath.Join(home, ".config", "stars-sandbox", "config.json")
}

func loadConfig(path string, required bool) (fileConfig, error) {
	var fc fileConfig
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && !required {
			return fc, nil
		}
		return fc, fmt.Errorf("read config %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &fc); err != nil {
		return fc, fmt.Errorf("parse config %s: %w", path, err)
	}
	return fc, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func (c *client) do(method, path string, query url.Values, body any, idempotencyKey string) ([]byte, error) {
	endpoint := strings.TrimRight(c.base, "/") + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		payload = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, endpoint, payload)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var p struct {
			Code   string `json:"code"`
			Detail string `json:"detail"`
		}
		if json.Unmarshal(data, &p) == nil && p.Code != "" {
			if p.Detail == "" {
				return nil, fmt.Errorf("%s", p.Code)
			}
			return nil, fmt.Errorf("%s: %s", p.Code, p.Detail)
		}
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return data, nil
}

func (c *client) print(data []byte) error {
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		_, writeErr := c.out.Write(data)
		return writeErr
	}
	enc := json.NewEncoder(c.out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func runUser(c *client, args []string, stderr io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: stars-sandbox user list|get|create|update|delete|quota")
	}
	switch args[0] {
	case "list":
		fs := flag.NewFlagSet("user list", flag.ContinueOnError)
		fs.SetOutput(stderr)
		limit := fs.Int("limit", 0, "page size (1-200)")
		cursor := fs.String("cursor", "", "page cursor")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		q := url.Values{}
		if *limit > 0 {
			q.Set("limit", fmt.Sprint(*limit))
		}
		if *cursor != "" {
			q.Set("cursor", *cursor)
		}
		return c.getAndPrint("/api/v1/users", q)
	case "get":
		fs := flag.NewFlagSet("user get", flag.ContinueOnError)
		fs.SetOutput(stderr)
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return fmt.Errorf("usage: stars-sandbox user get <id|me>")
		}
		id := fs.Arg(0)
		if id == "me" {
			return c.getAndPrint("/api/v1/users/me", nil)
		}
		return c.getAndPrint("/api/v1/users/"+url.PathEscape(id), nil)
	case "create":
		fs := flag.NewFlagSet("user create", flag.ContinueOnError)
		fs.SetOutput(stderr)
		name := fs.String("name", "", "username")
		email := fs.String("email", "", "email")
		loginType := fs.String("login-type", "password", "password, oidc, or none")
		password := fs.String("password", "", "password (prefer --password-stdin)")
		passwordStdin := fs.Bool("password-stdin", false, "read the password from stdin")
		isAdmin := fs.Bool("is-admin", false, "make the User an admin")
		idem := fs.String("idempotency-key", "", "idempotency key")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *name == "" {
			return fmt.Errorf("--name is required")
		}
		pw, err := resolvePassword(*password, *passwordStdin)
		if err != nil {
			return err
		}
		body := map[string]any{"name": *name, "login_type": *loginType, "is_admin": *isAdmin}
		if *email != "" {
			body["email"] = *email
		}
		if pw != "" {
			body["password"] = pw
		}
		return c.sendAndPrint(http.MethodPost, "/api/v1/users", body, *idem)
	case "update":
		fs := flag.NewFlagSet("user update", flag.ContinueOnError)
		fs.SetOutput(stderr)
		name := fs.String("name", "", "username")
		email := fs.String("email", "", "email (empty string clears it)")
		loginType := fs.String("login-type", "", "password, oidc, or none")
		password := fs.String("password", "", "password (prefer --password-stdin)")
		passwordStdin := fs.Bool("password-stdin", false, "read the password from stdin")
		isAdmin := fs.Bool("is-admin", false, "make the User an admin")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return fmt.Errorf("usage: stars-sandbox user update <id> [flags]")
		}
		body := map[string]any{}
		set := map[string]bool{}
		fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
		if set["name"] {
			body["name"] = *name
		}
		if set["email"] {
			body["email"] = *email
		}
		if set["login-type"] {
			body["login_type"] = *loginType
		}
		if set["is-admin"] {
			body["is_admin"] = *isAdmin
		}
		if set["password"] || set["password-stdin"] {
			pw, err := resolvePassword(*password, *passwordStdin)
			if err != nil {
				return err
			}
			body["password"] = pw
		}
		return c.sendAndPrint(http.MethodPatch, "/api/v1/users/"+url.PathEscape(fs.Arg(0)), body, "")
	case "delete":
		fs := flag.NewFlagSet("user delete", flag.ContinueOnError)
		fs.SetOutput(stderr)
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return fmt.Errorf("usage: stars-sandbox user delete <id>")
		}
		return c.sendAndPrint(http.MethodDelete, "/api/v1/users/"+url.PathEscape(fs.Arg(0)), nil, "")
	case "quota":
		fs := flag.NewFlagSet("user quota", flag.ContinueOnError)
		fs.SetOutput(stderr)
		gpus := fs.Int("gpus", -1, "GPU quota")
		sandboxes := fs.Int("sandboxes", -1, "Sandbox quota")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return fmt.Errorf("usage: stars-sandbox user quota <id> --gpus N --sandboxes N")
		}
		if *gpus < 0 || *sandboxes < 0 {
			return fmt.Errorf("--gpus and --sandboxes are required and must be non-negative")
		}
		return c.sendAndPrint(http.MethodPut, "/api/v1/users/"+url.PathEscape(fs.Arg(0))+"/quota", map[string]any{
			"gpus": *gpus, "sandboxes": *sandboxes,
		}, "")
	default:
		return fmt.Errorf("unknown user command %q", args[0])
	}
}

func runToken(c *client, args []string, stderr io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: stars-sandbox token list|create|revoke")
	}
	switch args[0] {
	case "list":
		fs := flag.NewFlagSet("token list", flag.ContinueOnError)
		fs.SetOutput(stderr)
		limit := fs.Int("limit", 0, "page size (1-200)")
		cursor := fs.String("cursor", "", "page cursor")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		q := url.Values{}
		if *limit > 0 {
			q.Set("limit", fmt.Sprint(*limit))
		}
		if *cursor != "" {
			q.Set("cursor", *cursor)
		}
		return c.getAndPrint("/api/v1/users/me/tokens", q)
	case "create":
		fs := flag.NewFlagSet("token create", flag.ContinueOnError)
		fs.SetOutput(stderr)
		name := fs.String("name", "", "token name")
		scope := fs.String("scope", "user", "read_only, user, or admin")
		expiresAt := fs.String("expires-at", "", "RFC 3339 expiry")
		idem := fs.String("idempotency-key", "", "idempotency key")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *name == "" {
			return fmt.Errorf("--name is required")
		}
		body := map[string]any{"name": *name, "scope": *scope}
		if *expiresAt != "" {
			t, err := time.Parse(time.RFC3339, *expiresAt)
			if err != nil {
				return fmt.Errorf("--expires-at must be RFC 3339: %w", err)
			}
			body["expires_at"] = t
		}
		return c.sendAndPrint(http.MethodPost, "/api/v1/users/me/tokens", body, *idem)
	case "revoke":
		fs := flag.NewFlagSet("token revoke", flag.ContinueOnError)
		fs.SetOutput(stderr)
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return fmt.Errorf("usage: stars-sandbox token revoke <id>")
		}
		return c.sendAndPrint(http.MethodDelete, "/api/v1/users/me/tokens/"+url.PathEscape(fs.Arg(0)), nil, "")
	default:
		return fmt.Errorf("unknown token command %q", args[0])
	}
}

func runAudit(c *client, args []string, stderr io.Writer) error {
	if len(args) > 0 && args[0] == "list" {
		args = args[1:]
	}
	fs := flag.NewFlagSet("audit list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	actor := fs.String("actor", "", "User id or name")
	action := fs.String("action", "", "action, e.g. user.create")
	target := fs.String("target", "", "target id")
	targetType := fs.String("target-type", "", "target type, e.g. user")
	from := fs.String("from", "", "RFC 3339 lower bound")
	to := fs.String("to", "", "RFC 3339 upper bound")
	limit := fs.Int("limit", 0, "page size (1-200)")
	cursor := fs.String("cursor", "", "page cursor")
	if err := fs.Parse(args); err != nil {
		return err
	}
	q := url.Values{}
	for name, value := range map[string]string{
		"actor": *actor, "action": *action, "target": *target,
		"target_type": *targetType, "from": *from, "to": *to,
	} {
		if value != "" {
			q.Set(name, value)
		}
	}
	if *limit > 0 {
		q.Set("limit", fmt.Sprint(*limit))
	}
	if *cursor != "" {
		q.Set("cursor", *cursor)
	}
	return c.getAndPrint("/api/v1/audit", q)
}

func resolvePassword(value string, fromStdin bool) (string, error) {
	if value != "" && fromStdin {
		return "", fmt.Errorf("use either --password or --password-stdin, not both")
	}
	if !fromStdin {
		return value, nil
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func (c *client) getAndPrint(path string, query url.Values) error {
	data, err := c.do(http.MethodGet, path, query, nil, "")
	if err != nil {
		return err
	}
	return c.print(data)
}

func (c *client) sendAndPrint(method, path string, body any, idempotencyKey string) error {
	data, err := c.do(method, path, nil, body, idempotencyKey)
	if err != nil {
		return err
	}
	return c.print(data)
}
