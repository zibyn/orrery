package cli

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	starsclient "github.com/zibyn/stars-sandbox/client"
)

// runGit manages the caller's Git Account Links (ADR-0013).
func runGit(c *client, args []string, stderr io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: stars-sandbox git links|unlink <provider-id>")
	}
	switch args[0] {
	case "links":
		fs := flag.NewFlagSet("git links", flag.ContinueOnError)
		fs.SetOutput(stderr)
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return fmt.Errorf("usage: stars-sandbox git links")
		}
		links, err := c.ListGitLinks(ctx)
		if err != nil {
			return err
		}
		return c.print(links)
	case "unlink":
		fs := flag.NewFlagSet("git unlink", flag.ContinueOnError)
		fs.SetOutput(stderr)
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return fmt.Errorf("usage: stars-sandbox git unlink <provider-id>")
		}
		link, err := c.DeleteGitLink(ctx, fs.Arg(0))
		if err != nil {
			return err
		}
		return c.print(link)
	default:
		return fmt.Errorf("unknown git command %q", args[0])
	}
}

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

	body := starsclient.GitCredentialRequest{}
	if host := input["host"]; host != "" {
		body.Host = host
	}
	cred, err := c.GitCredential(ctx, body)
	if err != nil {
		// A missing link is not a helper failure: name the console page on
		// stderr and let git try the next helper (api.md §3).
		var problem *starsclient.Error
		if errors.As(err, &problem) && problem.Status == http.StatusNotFound {
			if problem.AuthorizeURL != "" {
				fmt.Fprintf(stderr, "authorize this Sandbox's owner for Git at %s\n", problem.AuthorizeURL)
			}
			return nil
		}
		return err
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
