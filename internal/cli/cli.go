package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/term"

	starsclient "github.com/zibyn/stars-sandbox/client"
)

// The external CLI speaks only the public REST API (ADR-0017).
type fileConfig struct {
	APIURL string `json:"api_url"`
	Token  string `json:"token"`
}

// ctx is the process-wide context for API calls; the CLI has no cancellation.
var ctx = context.Background()

// client pairs the SDK with the writer command output goes to.
type client struct {
	*starsclient.Client
	out io.Writer
}

func newClient(base, token string, out io.Writer) *client {
	return &client{Client: starsclient.New(base, token), out: out}
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
	c := newClient(base, tok, stdout)

	switch rest[0] {
	case "user":
		err = runUser(c, rest[1:], stderr)
	case "token":
		err = runToken(c, rest[1:], stderr)
	case "secret":
		err = runSecret(c, rest[1:], stderr)
	case "snapshot":
		err = runSnapshot(c, rest[1:], stderr)
	case "audit":
		err = runAudit(c, rest[1:], stderr)
	case "exec":
		err = runExec(c, rest[1:], stderr)
	case "shell":
		err = runShell(c, rest[1:], stderr)
	case "files":
		err = runFiles(c, rest[1:], stderr)
	case "resource":
		err = runResource(c, rest[1:], stderr)
	case "workspace":
		err = runWorkspace(c, rest[1:], stderr)
	case "node":
		err = runNode(c, rest[1:], stderr)
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
  snapshot import <name> <ref@sha256:…> [--auth-secret NAME]
                                    import an image and follow it to a terminal state
  snapshot build <name> -f Dockerfile [--base SNAPSHOT]
                   [--context name=resource[@version] ...] [--secret id=secret ...]
                                    build a Snapshot and follow its log
  snapshot ls
  snapshot rm <name-or-id>
  snapshot versions <name-or-id>
  snapshot current <name-or-id> [<version-id>]
                                    show or move the current version
  audit list
  exec run|cancel
  shell <sandbox-id> [-- argv...]   open an interactive terminal
  files ls <sandbox-id> [path]      list a Workspace directory
  files cp <src> <dst>              copy a file to or from <sandbox-id>:<path>
  resource create <name>
  resource ls
  resource rm <name|id>
  resource versions <name|id>
  resource current <name|id>
  resource push <dir> <name>        upload and publish a directory
  resource pull <name>[@version] <dir>
                                    download a version into a directory
  workspace publish <id> <path> <name>
                                    publish a Workspace path as a Resource version
  workspace declare-lost <id>       declare a Workspace lost (admin)
  workspace abandon <id>            abandon a stuck Workspace deletion (admin)
  node pause|resume|delete <id>     admin: pause, resume or delete a Node
  node declare-lost <id>            declare an unreachable Node lost (admin)
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
	c := starsclient.New(base, "")
	out, err := c.CreateSession(ctx, starsclient.SessionRequest{
		Username: *username,
		Password: password,
		Token:    &starsclient.TokenRequest{Name: *name, Scope: *scope},
	})
	if err != nil {
		return fmt.Errorf("%w (if this User does not log in with a password, run again without --username and finish in the Web console)", err)
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
	c := starsclient.New(base, token)
	me, err := c.Me(ctx)
	if err != nil {
		return fmt.Errorf("the token was rejected: %w", err)
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

// print writes v as indented JSON, matching the server's own shape.
func (c *client) print(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	var decoded any
	if err := json.Unmarshal(data, &decoded); err != nil {
		_, writeErr := c.out.Write(data)
		return writeErr
	}
	enc := json.NewEncoder(c.out)
	enc.SetIndent("", "  ")
	return enc.Encode(decoded)
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
		page, err := c.ListUsers(ctx, *limit, *cursor)
		if err != nil {
			return err
		}
		return c.print(page)
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
		var (
			user *starsclient.User
			err  error
		)
		if id == "me" {
			user, err = c.Me(ctx)
		} else {
			user, err = c.GetUser(ctx, id)
		}
		if err != nil {
			return err
		}
		return c.print(user)
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
		req := starsclient.CreateUserRequest{Name: *name, LoginType: *loginType, IsAdmin: *isAdmin}
		if *email != "" {
			req.Email = email
		}
		if pw != "" {
			req.Password = &pw
		}
		user, err := c.CreateUser(ctx, req, *idem)
		if err != nil {
			return err
		}
		return c.print(user)
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
		var req starsclient.UpdateUserRequest
		set := map[string]bool{}
		fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
		if set["name"] {
			req.Name = name
		}
		if set["email"] {
			req.Email = email
		}
		if set["login-type"] {
			req.LoginType = loginType
		}
		if set["is-admin"] {
			req.IsAdmin = isAdmin
		}
		if set["password"] || set["password-stdin"] {
			pw, err := resolvePassword(*password, *passwordStdin)
			if err != nil {
				return err
			}
			req.Password = &pw
		}
		user, err := c.UpdateUser(ctx, fs.Arg(0), req)
		if err != nil {
			return err
		}
		return c.print(user)
	case "delete":
		fs := flag.NewFlagSet("user delete", flag.ContinueOnError)
		fs.SetOutput(stderr)
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return fmt.Errorf("usage: stars-sandbox user delete <id>")
		}
		user, err := c.DeleteUser(ctx, fs.Arg(0))
		if err != nil {
			return err
		}
		return c.print(user)
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
		user, err := c.SetUserQuota(ctx, fs.Arg(0), int32(*gpus), int32(*sandboxes))
		if err != nil {
			return err
		}
		return c.print(user)
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
		page, err := c.ListTokens(ctx, *limit, *cursor)
		if err != nil {
			return err
		}
		return c.print(page)
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
		req := starsclient.TokenRequest{Name: *name, Scope: *scope}
		if *expiresAt != "" {
			t, err := time.Parse(time.RFC3339, *expiresAt)
			if err != nil {
				return fmt.Errorf("--expires-at must be RFC 3339: %w", err)
			}
			req.ExpiresAt = &t
		}
		token, err := c.CreateToken(ctx, req, *idem)
		if err != nil {
			return err
		}
		return c.print(token)
	case "revoke":
		fs := flag.NewFlagSet("token revoke", flag.ContinueOnError)
		fs.SetOutput(stderr)
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return fmt.Errorf("usage: stars-sandbox token revoke <id>")
		}
		token, err := c.RevokeToken(ctx, fs.Arg(0))
		if err != nil {
			return err
		}
		return c.print(token)
	default:
		// `token <provider>` fetches a Git credential for the Sandbox's owner.
		// The API Token verbs above keep their names. Only the token goes to
		// stdout, so GH_TOKEN=$(stars-sandbox token github) works (api.md §3).
		if len(args) == 1 && !strings.HasPrefix(args[0], "-") {
			cred, err := c.GitCredential(ctx, starsclient.GitCredentialRequest{Provider: args[0]})
			if err != nil {
				var problem *starsclient.Error
				if errors.As(err, &problem) && problem.Status == http.StatusNotFound && problem.AuthorizeURL != "" {
					return fmt.Errorf("no Git account is linked for %s; authorize at %s",
						args[0], problem.AuthorizeURL)
				}
				return err
			}
			if cred.Password == "" {
				return fmt.Errorf("the API returned an empty Git token")
			}
			fmt.Fprintln(c.out, cred.Password)
			return nil
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
		secret, err := c.PutSecret(ctx, fs.Arg(0), value)
		if err != nil {
			return err
		}
		return c.print(secret)
	case "list":
		fs := flag.NewFlagSet("secret list", flag.ContinueOnError)
		fs.SetOutput(stderr)
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		page, err := c.ListSecrets(ctx, 0, "")
		if err != nil {
			return err
		}
		return c.print(page)
	case "rm":
		fs := flag.NewFlagSet("secret rm", flag.ContinueOnError)
		fs.SetOutput(stderr)
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return fmt.Errorf("usage: stars-sandbox secret rm <name>")
		}
		secret, err := c.DeleteSecret(ctx, fs.Arg(0))
		if err != nil {
			return err
		}
		return c.print(secret)
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
	page, err := c.ListAudit(ctx, starsclient.AuditParams{
		Actor: *actor, Action: *action, Target: *target, TargetType: *targetType,
		From: *from, To: *to, Limit: *limit, Cursor: *cursor,
	})
	if err != nil {
		return err
	}
	return c.print(page)
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
