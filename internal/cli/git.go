package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// runGitCredential implements the helper git starts through
// `credential.helper` (gitcredentials(7)). The helper reads git's
// credential description on stdin; `get` answers with username/password
// lines, `store` and `erase` are no-ops because the platform never asks git to
// keep a credential (ADR-0017).
func runGitCredential(c *client, args []string, stdout, stderr io.Writer) error {
	input := readCredentialInput(os.Stdin)
	action := "get"
	if len(args) > 0 {
		action = args[0]
	}
	switch action {
	case "store", "erase":
		return nil
	case "get":
	default:
		return fmt.Errorf("unknown git-credential action %q", action)
	}

	body := map[string]string{}
	if host := input["host"]; host != "" {
		body["host"] = host
	}
	status, data, err := c.send(http.MethodPost, "/api/v1/git/credential", nil, body, "")
	if err != nil {
		return err
	}
	// A missing link is not a helper failure: name the console page on stderr
	// and let git try the next helper (api.md §3).
	if status == http.StatusNotFound {
		var p struct {
			AuthorizeURL string `json:"authorize_url"`
		}
		if json.Unmarshal(data, &p) == nil && p.AuthorizeURL != "" {
			fmt.Fprintf(stderr, "authorize this Sandbox's owner for Git at %s\n", p.AuthorizeURL)
		}
		return nil
	}
	if status < 200 || status >= 300 {
		return problemError(status, data)
	}
	var cred struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.Unmarshal(data, &cred); err != nil {
		return fmt.Errorf("decode Git credential: %w", err)
	}
	fmt.Fprintf(stdout, "username=%s\npassword=%s\n", cred.Username, cred.Password)
	return nil
}

// readCredentialInput parses git's "key=value" lines up to the blank line
// that ends the description.
func readCredentialInput(r io.Reader) map[string]string {
	values := map[string]string{}
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			break
		}
		if key, value, ok := strings.Cut(line, "="); ok {
			values[strings.TrimSpace(key)] = value
		}
	}
	return values
}
