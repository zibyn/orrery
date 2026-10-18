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

	"golang.org/x/term"
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
	// login writes the config, so an explicit path that does not exist yet is
	// fine for it and an error for every other command.
	required := *configPath != "" && rest[0] != "login"
	fc, err := loadConfig(path, required)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	base := firstNonEmpty(*apiURL, os.Getenv("STARS_SANDBOX_API_URL"), os.Getenv("STARS_URL"), fc.APIURL, "http://localhost:8080")
	if rest[0] == "login" {
		if err := runLogin(base, path, rest[1:], os.Stdin, stdout, stderr); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		return 0
	}
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
	case "secret":
		err = runSecret(c, rest[1:], stderr)
	case "audit":
		err = runAudit(c, rest[1:], stderr)
	case "exec":
		err = runExec(c, rest[1:], stderr)
	case "shell":
		err = runShell(c, rest[1:], stderr)
	case "files":
		err = runFiles(c, rest[1:], stderr)
	case "port-forward":
		err = runPortForward(c, rest[1:])
	case "git-credential":
		err = runGitCredential(c, rest[1:], stdout, stderr)
	case "git":
		err = runGit(c, rest[1:], stderr)
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
  login            log in and store an API Token (--username for a password login)
  user list|get|create|update|delete|quota
  token list|create|revoke
  token <provider>                  fetch a Git credential by provider id
  git-credential get|store|erase    git credential helper (Sandbox only)
  git links                         list linked Git providers
  git unlink <provider-id>          unlink a Git account
  secret set [--file PATH] <name>   write a value from stdin or a file
  secret list
  secret rm <name>
  audit list
  exec run|cancel
  shell <sandbox-id> [-- argv...]   open an interactive terminal
  files ls <sandbox-id> [path]      list a Workspace directory
  files cp <src> <dst>              copy a file to or from <sandbox-id>:<path>
  port-forward <sandbox-id> [LOCAL:]REMOTE
                                    tunnel a local TCP port to a Sandbox port

global flags:
  --config PATH    config file (default $STARS_SANDBOX_CONFIG or $XDG_CONFIG_HOME/stars-sandbox/config.json)
  --api-url URL    API base URL (default $STARS_SANDBOX_API_URL or the config file)
  --token TOKEN    API token (default $STARS_SANDBOX_TOKEN or the config file)

examples:
  stars-sandbox login --username alice
  stars-sandbox user list
  stars-sandbox user create --name alice --login-type password --password-stdin
  stars-sandbox token create --name ci --scope read_only
  stars-sandbox secret set --file npm-token.txt NPM_TOKEN
  stars-sandbox secret list
  stars-sandbox secret rm NPM_TOKEN
  stars-sandbox audit list --action token.create --limit 20
  stars-sandbox exec run <sandbox-id> -- uname -a
  stars-sandbox exec cancel <exec-id>
  stars-sandbox shell <sandbox-id> -- /bin/bash
  stars-sandbox files ls <sandbox-id> src
  stars-sandbox files cp ./config.toml <sandbox-id>:src/config.toml
  stars-sandbox files cp <sandbox-id>:out/report.txt .
  stars-sandbox git links
  stars-sandbox git unlink gitea
  stars-sandbox port-forward <sandbox-id> 8080:3000
`)
}

// runLogin gets the CLI a named API Token (ADR-0020). With --username it trades
// a password for one; without, the User signs in at the console's /cli-auth,
// which creates a token with the browser Session, and pastes it back.
func runLogin(base, configPath string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	fs.SetOutput(stderr)
	username := fs.String("username", "", "username; omit to finish the login in the Web console")
	name := fs.String("name", "", "token name (default cli-<UTC timestamp>; password login only)")
	scope := fs.String("scope", "user", "token scope (password login only): read_only, user, or admin")
	passwordStdin := fs.Bool("password-stdin", false, "read the password from stdin")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("usage: stars-sandbox login [--username NAME] [flags]")
	}
	if *username == "" {
		return consoleLogin(base, configPath, stdin, stdout, stderr)
	}
	password, err := readPassword(*passwordStdin, stdin, stderr)
	if err != nil {
		return err
	}
	if *name == "" {
		*name = "cli-" + time.Now().UTC().Format("20060102T150405Z")
	}
	c := &client{base: base, http: &http.Client{Timeout: 30 * time.Second}, out: stdout}
	data, err := c.do(http.MethodPost, "/api/v1/sessions", nil, map[string]any{
		"username": *username,
		"password": password,
		"token":    map[string]any{"name": *name, "scope": *scope},
	}, "")
	if err != nil {
		return fmt.Errorf("%w (if this User does not log in with a password, run again without --username and finish in the Web console)", err)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return fmt.Errorf("decode login response: %w", err)
	}
	if out.Token == "" {
		return fmt.Errorf("login response did not contain a token")
	}
	if err := writeConfig(configPath, fileConfig{APIURL: base, Token: out.Token}); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "logged in as %s; token %q written to %s\n", *username, *name, configPath)
	return nil
}

// consoleLogin handles a User who logs in at an IdP: the console creates the
// token with the browser Session, and the User pastes it here like `coder login`.
func consoleLogin(base, configPath string, stdin io.Reader, stdout, stderr io.Writer) error {
	consoleURL := strings.TrimRight(base, "/") + "/login?return=%2Fcli-auth"
	fmt.Fprintf(stderr, "Open %s, sign in, and create a token on the CLI authorization page.\n", consoleURL)
	fmt.Fprint(stderr, "Paste the token: ")
	token, err := readLine(stdin)
	if err != nil {
		return err
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return fmt.Errorf("no token pasted")
	}
	// Validate before storing: a typo must not replace a working config.
	c := &client{base: base, token: token, http: &http.Client{Timeout: 30 * time.Second}, out: stdout}
	data, err := c.do(http.MethodGet, "/api/v1/users/me", nil, nil, "")
	if err != nil {
		return fmt.Errorf("the token was rejected: %w", err)
	}
	var me struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(data, &me); err != nil {
		return fmt.Errorf("decode user: %w", err)
	}
	if err := writeConfig(configPath, fileConfig{APIURL: base, Token: token}); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "logged in as %s; token written to %s\n", me.Name, configPath)
	return nil
}

func readPassword(fromStdin bool, stdin io.Reader, stderr io.Writer) (string, error) {
	if fromStdin {
		return readLine(stdin)
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return "", fmt.Errorf("--password-stdin is required when stdin is not a terminal")
	}
	// The terminal reads from the process's stdin, not the injected reader.
	fmt.Fprint(stderr, "Password: ")
	password, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(stderr)
	if err != nil {
		return "", err
	}
	return string(password), nil
}

// readLine reads one trimmed line, accepting EOF as the end of the input.
func readLine(r io.Reader) (string, error) {
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func writeConfig(path string, fc fileConfig) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	data, err := json.MarshalIndent(fc, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write config %s: %w", path, err)
	}
	return os.Chmod(path, 0o600)
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
	status, data, err := c.send(method, path, query, body, idempotencyKey)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, problemError(status, data)
	}
	return data, nil
}

// send performs a request and returns its status and body even on failure, so
// callers that read problem extensions (the credential helper) can inspect it.
func (c *client) send(method, path string, query url.Values, body any, idempotencyKey string) (int, []byte, error) {
	endpoint := strings.TrimRight(c.base, "/") + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		payload = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, endpoint, payload)
	if err != nil {
		return 0, nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	resp, err := c.http.Do(req)
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

// problemError turns a problem+json body into an error, falling back to the
// status line for a body that is not one.
func problemError(status int, data []byte) error {
	var p struct {
		Code   string `json:"code"`
		Detail string `json:"detail"`
	}
	if json.Unmarshal(data, &p) == nil && p.Code != "" {
		if p.Detail == "" {
			return fmt.Errorf("%s", p.Code)
		}
		return fmt.Errorf("%s: %s", p.Code, p.Detail)
	}
	return fmt.Errorf("HTTP %d: %s", status, strings.TrimSpace(string(data)))
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
		return fmt.Errorf("usage: stars-sandbox token list|create|revoke|<provider>")
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
		// `token <provider>` fetches a Git credential for the Sandbox's owner.
		// The API Token verbs above keep their names.
		if len(args) == 1 && !strings.HasPrefix(args[0], "-") {
			return c.sendAndPrint(http.MethodPost, "/api/v1/git/credential",
				map[string]string{"provider": args[0]}, "")
		}
		return fmt.Errorf("unknown token command %q", args[0])
	}
}

func runSecret(c *client, args []string, stderr io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: stars-sandbox secret set|list|rm")
	}
	switch args[0] {
	case "set":
		fs := flag.NewFlagSet("secret set", flag.ContinueOnError)
		fs.SetOutput(stderr)
		file := fs.String("file", "", "read the value from this file instead of stdin")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return fmt.Errorf("usage: stars-sandbox secret set [--file PATH] <name>")
		}
		value, err := readSecretValue(*file)
		if err != nil {
			return err
		}
		return c.sendAndPrint(http.MethodPut, "/api/v1/secrets/"+url.PathEscape(fs.Arg(0)),
			map[string]string{"value": value}, "")
	case "list":
		fs := flag.NewFlagSet("secret list", flag.ContinueOnError)
		fs.SetOutput(stderr)
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		return c.getAndPrint("/api/v1/secrets", nil)
	case "rm":
		fs := flag.NewFlagSet("secret rm", flag.ContinueOnError)
		fs.SetOutput(stderr)
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return fmt.Errorf("usage: stars-sandbox secret rm <name>")
		}
		return c.sendAndPrint(http.MethodDelete, "/api/v1/secrets/"+url.PathEscape(fs.Arg(0)), nil, "")
	default:
		return fmt.Errorf("unknown secret command %q", args[0])
	}
}

// readSecretValue reads the value from a file or stdin. One trailing line
// ending is dropped, as with a value piped from echo; the value never comes
// from argv (#74).
func readSecretValue(path string) (string, error) {
	var (
		data []byte
		err  error
	)
	if path == "" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return "", err
	}
	value := strings.TrimSuffix(string(data), "\n")
	value = strings.TrimSuffix(value, "\r")
	return value, nil
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
	return readLine(os.Stdin)
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
