package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
)

// snapshot import|ls|rm|versions|current (api.md §3). Imports are
// asynchronous, so `import` follows the version to its terminal state.
func runSnapshot(c *client, args []string, stderr io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: stars-sandbox snapshot build|import|ls|rm|versions|current")
	}
	switch args[0] {
	case "build":
		return runSnapshotBuild(c, args[1:], stderr)
	case "import":
		return runSnapshotImport(c, args[1:])
	case "ls":
		fs := flag.NewFlagSet("snapshot ls", flag.ContinueOnError)
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
		return c.getAndPrint("/api/v1/snapshots", q)
	case "rm":
		fs := flag.NewFlagSet("snapshot rm", flag.ContinueOnError)
		fs.SetOutput(stderr)
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return fmt.Errorf("usage: stars-sandbox snapshot rm <name-or-id>")
		}
		id, err := resolveSnapshotID(c, fs.Arg(0))
		if err != nil {
			return err
		}
		return c.sendAndPrint(http.MethodDelete, "/api/v1/snapshots/"+url.PathEscape(id), nil, "")
	case "versions":
		fs := flag.NewFlagSet("snapshot versions", flag.ContinueOnError)
		fs.SetOutput(stderr)
		limit := fs.Int("limit", 0, "page size (1-200)")
		cursor := fs.String("cursor", "", "page cursor")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return fmt.Errorf("usage: stars-sandbox snapshot versions <name-or-id>")
		}
		id, err := resolveSnapshotID(c, fs.Arg(0))
		if err != nil {
			return err
		}
		q := url.Values{}
		if *limit > 0 {
			q.Set("limit", fmt.Sprint(*limit))
		}
		if *cursor != "" {
			q.Set("cursor", *cursor)
		}
		return c.getAndPrint("/api/v1/snapshots/"+url.PathEscape(id)+"/versions", q)
	case "current":
		fs := flag.NewFlagSet("snapshot current", flag.ContinueOnError)
		fs.SetOutput(stderr)
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() < 1 || fs.NArg() > 2 {
			return fmt.Errorf("usage: stars-sandbox snapshot current <name-or-id> [<version-id>]")
		}
		id, err := resolveSnapshotID(c, fs.Arg(0))
		if err != nil {
			return err
		}
		if fs.NArg() == 2 {
			return c.sendAndPrint(http.MethodPut, "/api/v1/snapshots/"+url.PathEscape(id)+"/current",
				map[string]string{"version": fs.Arg(1)}, "")
		}
		return showSnapshotCurrent(c, id)
	default:
		return fmt.Errorf("unknown snapshot command %q", args[0])
	}
}

func runSnapshotBuild(c *client, args []string, stderr io.Writer) error {
	// The command takes the name first (`snapshot build <name> -f …`), so the
	// standard flag package would stop at the first positional. Parse the
	// small option set by hand, as `snapshot import` does.
	var file, base, idem string
	var noFollow bool
	var contextFlags, secretFlags stringList
	var positional []string
	value := func(i *int, inline, name string) (string, error) {
		if inline != "" {
			return inline, nil
		}
		if *i+1 >= len(args) {
			return "", fmt.Errorf("%s needs a value", name)
		}
		*i++
		return args[*i], nil
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		var inline string
		if key, val, ok := strings.Cut(arg, "="); ok && strings.HasPrefix(key, "--") {
			arg, inline = key, val
		}
		var err error
		switch arg {
		case "-f", "--file":
			file, err = value(&i, inline, arg)
		case "--base":
			base, err = value(&i, inline, arg)
		case "--context":
			var v string
			if v, err = value(&i, inline, arg); err == nil {
				contextFlags = append(contextFlags, v)
			}
		case "--secret":
			var v string
			if v, err = value(&i, inline, arg); err == nil {
				secretFlags = append(secretFlags, v)
			}
		case "--idempotency-key":
			idem, err = value(&i, inline, arg)
		case "--no-follow":
			noFollow = true
		default:
			positional = append(positional, args[i])
		}
		if err != nil {
			return err
		}
	}
	if len(positional) != 1 || file == "" {
		return fmt.Errorf("usage: stars-sandbox snapshot build <name> -f Dockerfile [--base SNAPSHOT] [--context name=resource[@version] ...] [--secret id=secret ...]")
	}
	dockerfile := []byte(nil)
	if file == "-" {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		dockerfile = data
	} else {
		data, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		dockerfile = data
	}
	build := map[string]any{"dockerfile": string(dockerfile)}
	if base != "" {
		build["base"] = base
	}
	contexts := map[string]string{}
	for _, raw := range contextFlags {
		name, value, ok := strings.Cut(raw, "=")
		if !ok || name == "" || value == "" {
			return fmt.Errorf("--context must be name=resource[@version], got %q", raw)
		}
		contexts[name] = value
	}
	if len(contexts) > 0 {
		build["contexts"] = contexts
	}
	secrets := map[string]string{}
	for _, raw := range secretFlags {
		id, value, ok := strings.Cut(raw, "=")
		if !ok || id == "" || value == "" {
			return fmt.Errorf("--secret must be id=secret_name, got %q", raw)
		}
		secrets[id] = value
	}
	if len(secrets) > 0 {
		build["secrets"] = secrets
	}
	snapshotID, err := ensureSnapshot(c, positional[0])
	if err != nil {
		return err
	}
	status, data, err := c.send(http.MethodPost,
		"/api/v1/snapshots/"+url.PathEscape(snapshotID)+"/versions", nil,
		map[string]any{"build": build}, idem)
	if err != nil {
		return err
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return problemError(status, data)
	}
	var version snapshotVersionView
	if err := json.Unmarshal(data, &version); err != nil {
		return err
	}
	if noFollow {
		return c.print(data)
	}
	if version.BuildExecID != nil {
		// The build log is a normal Exec log (api.md §3).
		if _, err := c.followExec(*version.BuildExecID, 0, c.out, stderr); err != nil {
			return err
		}
	}
	final, raw, err := pollSnapshotVersion(c, snapshotID, version.ID)
	if err != nil {
		return err
	}
	if err := c.print(raw); err != nil {
		return err
	}
	if final.State != "ready" {
		cause := ""
		if final.Cause != nil {
			cause = ": " + *final.Cause
		}
		return fmt.Errorf("SnapshotVersion %s ended %s%s", final.ID, final.State, cause)
	}
	return nil
}

// pollSnapshotVersion waits for a terminal state and returns the final
// version, with the raw response for printing.
func pollSnapshotVersion(c *client, snapshotID, versionID string) (snapshotVersionView, []byte, error) {
	deadline := time.Now().Add(time.Hour)
	for {
		status, data, err := c.send(http.MethodGet,
			"/api/v1/snapshots/"+url.PathEscape(snapshotID)+"/versions/"+url.PathEscape(versionID),
			nil, nil, "")
		if err != nil {
			return snapshotVersionView{}, nil, err
		}
		if status != http.StatusOK {
			return snapshotVersionView{}, nil, problemError(status, data)
		}
		var version snapshotVersionView
		if err := json.Unmarshal(data, &version); err != nil {
			return snapshotVersionView{}, nil, err
		}
		if version.State != "building" {
			return version, data, nil
		}
		if time.Now().After(deadline) {
			return snapshotVersionView{}, nil, fmt.Errorf("SnapshotVersion %s is still building", versionID)
		}
		time.Sleep(time.Second)
	}
}

func runSnapshotImport(c *client, args []string) error {
	authSecret := ""
	var positional []string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--auth-secret":
			if i+1 >= len(args) {
				return fmt.Errorf("--auth-secret needs a Secret name")
			}
			authSecret = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--auth-secret="):
			authSecret = strings.TrimPrefix(args[i], "--auth-secret=")
		default:
			positional = append(positional, args[i])
		}
	}
	if len(positional) != 2 {
		return fmt.Errorf("usage: stars-sandbox snapshot import <name> <ref@sha256:…> [--auth-secret NAME]")
	}
	name, ref := positional[0], positional[1]
	snapshotID, err := ensureSnapshot(c, name)
	if err != nil {
		return err
	}
	body := map[string]any{"import": map[string]any{"ref": ref}}
	if authSecret != "" {
		body["import"].(map[string]any)["auth_secret"] = authSecret
	}
	status, data, err := c.send(http.MethodPost, "/api/v1/snapshots/"+url.PathEscape(snapshotID)+"/versions", nil, body, "")
	if err != nil {
		return err
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return problemError(status, data)
	}
	var version snapshotVersionView
	if err := json.Unmarshal(data, &version); err != nil {
		return err
	}
	return waitForSnapshotVersion(c, snapshotID, version)
}

type snapshotView struct {
	ID               string  `json:"id"`
	OwnerID          *string `json:"owner_id"`
	Name             string  `json:"name"`
	CurrentVersionID *string `json:"current_version_id"`
}

type snapshotVersionView struct {
	ID          string  `json:"id"`
	State       string  `json:"state"`
	Cause       *string `json:"cause"`
	Digest      *string `json:"digest"`
	BuildExecID *string `json:"build_exec_id"`
}

// ensureSnapshot finds the caller's Snapshot by name or creates it.
func ensureSnapshot(c *client, name string) (string, error) {
	if id, err := findSnapshotByName(c, name, true); err == nil {
		return id, nil
	}
	status, data, err := c.send(http.MethodPost, "/api/v1/snapshots", nil, map[string]string{"name": name}, "")
	if err != nil {
		return "", err
	}
	if status == http.StatusCreated {
		var snap snapshotView
		if err := json.Unmarshal(data, &snap); err != nil {
			return "", err
		}
		return snap.ID, nil
	}
	if status == http.StatusConflict {
		return findSnapshotByName(c, name, true)
	}
	return "", problemError(status, data)
}

// resolveSnapshotID accepts an id or a name. A name prefers the caller's own
// Snapshot, then falls back to a platform one.
func resolveSnapshotID(c *client, ref string) (string, error) {
	if _, err := uuid.Parse(ref); err == nil {
		return ref, nil
	}
	if id, err := findSnapshotByName(c, ref, true); err == nil {
		return id, nil
	}
	return findSnapshotByName(c, ref, false)
}

func findSnapshotByName(c *client, name string, ownOnly bool) (string, error) {
	cursor := ""
	for {
		q := url.Values{"limit": []string{"200"}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		data, err := c.do(http.MethodGet, "/api/v1/snapshots", q, nil, "")
		if err != nil {
			return "", err
		}
		var page struct {
			Items      []snapshotView `json:"items"`
			NextCursor *string        `json:"next_cursor"`
		}
		if err := json.Unmarshal(data, &page); err != nil {
			return "", err
		}
		for _, snap := range page.Items {
			if snap.Name != name {
				continue
			}
			if ownOnly && snap.OwnerID == nil {
				continue
			}
			if !ownOnly && snap.OwnerID != nil {
				continue
			}
			return snap.ID, nil
		}
		if page.NextCursor == nil {
			return "", fmt.Errorf("no such Snapshot %q", name)
		}
		cursor = *page.NextCursor
	}
}

func showSnapshotCurrent(c *client, snapshotID string) error {
	data, err := c.do(http.MethodGet, "/api/v1/snapshots/"+url.PathEscape(snapshotID), nil, nil, "")
	if err != nil {
		return err
	}
	var snap snapshotView
	if err := json.Unmarshal(data, &snap); err != nil {
		return err
	}
	if snap.CurrentVersionID == nil {
		return c.print(data)
	}
	return c.getAndPrint("/api/v1/snapshots/"+url.PathEscape(snapshotID)+"/versions/"+url.PathEscape(*snap.CurrentVersionID), nil)
}

// waitForSnapshotVersion polls until the import reaches a terminal state.
func waitForSnapshotVersion(c *client, snapshotID string, version snapshotVersionView) error {
	deadline := time.Now().Add(time.Hour)
	for version.State == "building" {
		if time.Now().After(deadline) {
			return fmt.Errorf("SnapshotVersion %s is still building", version.ID)
		}
		time.Sleep(time.Second)
		data, err := c.do(http.MethodGet,
			"/api/v1/snapshots/"+url.PathEscape(snapshotID)+"/versions/"+url.PathEscape(version.ID), nil, nil, "")
		if err != nil {
			return err
		}
		if err := json.Unmarshal(data, &version); err != nil {
			return err
		}
	}
	status, data, err := c.send(http.MethodGet,
		"/api/v1/snapshots/"+url.PathEscape(snapshotID)+"/versions/"+url.PathEscape(version.ID), nil, nil, "")
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return problemError(status, data)
	}
	if err := c.print(data); err != nil {
		return err
	}
	if version.State != "ready" {
		cause := ""
		if version.Cause != nil {
			cause = ": " + *version.Cause
		}
		return fmt.Errorf("SnapshotVersion %s ended %s%s", version.ID, version.State, cause)
	}
	return nil
}
