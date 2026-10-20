package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"

	orreryclient "github.com/zibyn/orrery/client"
)

// snapshot import|ls|rm|versions|current (api.md §3). Imports are
// asynchronous, so `import` follows the version to its terminal state.
func runSnapshot(c *client, args []string, stderr io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: orrery snapshot build|import|ls|rm|versions|current")
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
		page, err := c.ListSnapshots(ctx, *limit, *cursor)
		if err != nil {
			return err
		}
		return c.print(page)
	case "rm":
		fs := flag.NewFlagSet("snapshot rm", flag.ContinueOnError)
		fs.SetOutput(stderr)
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return fmt.Errorf("usage: orrery snapshot rm <name-or-id>")
		}
		id, err := resolveSnapshotID(c, fs.Arg(0))
		if err != nil {
			return err
		}
		snapshot, err := c.DeleteSnapshot(ctx, id)
		if err != nil {
			return err
		}
		return c.print(snapshot)
	case "versions":
		fs := flag.NewFlagSet("snapshot versions", flag.ContinueOnError)
		fs.SetOutput(stderr)
		limit := fs.Int("limit", 0, "page size (1-200)")
		cursor := fs.String("cursor", "", "page cursor")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return fmt.Errorf("usage: orrery snapshot versions <name-or-id>")
		}
		id, err := resolveSnapshotID(c, fs.Arg(0))
		if err != nil {
			return err
		}
		page, err := c.ListSnapshotVersions(ctx, id, *limit, *cursor)
		if err != nil {
			return err
		}
		return c.print(page)
	case "current":
		fs := flag.NewFlagSet("snapshot current", flag.ContinueOnError)
		fs.SetOutput(stderr)
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() < 1 || fs.NArg() > 2 {
			return fmt.Errorf("usage: orrery snapshot current <name-or-id> [<version-id>]")
		}
		id, err := resolveSnapshotID(c, fs.Arg(0))
		if err != nil {
			return err
		}
		if fs.NArg() == 2 {
			snapshot, err := c.SetSnapshotCurrent(ctx, id, fs.Arg(1))
			if err != nil {
				return err
			}
			return c.print(snapshot)
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
		return fmt.Errorf("usage: orrery snapshot build <name> -f Dockerfile [--base SNAPSHOT] [--context name=resource[@version] ...] [--secret id=secret ...]")
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
	build := &orreryclient.SnapshotBuild{Dockerfile: string(dockerfile)}
	if base != "" {
		build.Base = &base
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
		build.Contexts = contexts
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
		build.Secrets = secrets
	}
	snapshotID, err := ensureSnapshot(c, positional[0])
	if err != nil {
		return err
	}
	version, err := c.CreateSnapshotVersion(ctx, snapshotID,
		orreryclient.SnapshotVersionRequest{Build: build}, idem)
	if err != nil {
		return err
	}
	if noFollow {
		return c.print(version)
	}
	if version.BuildExecID != nil {
		// The build log is a normal Exec log (api.md §3).
		if _, err := c.FollowOutput(ctx, *version.BuildExecID, c.out, stderr); err != nil {
			return err
		}
	}
	final, err := pollSnapshotVersion(c, snapshotID, version.ID)
	if err != nil {
		return err
	}
	return printSnapshotVersion(c, final)
}

// pollSnapshotVersion waits for a terminal state and returns the final
// version.
func pollSnapshotVersion(c *client, snapshotID, versionID string) (*orreryclient.SnapshotVersion, error) {
	deadline := time.Now().Add(time.Hour)
	for {
		version, err := c.GetSnapshotVersion(ctx, snapshotID, versionID)
		if err != nil {
			return nil, err
		}
		if version.State != "building" {
			return version, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("SnapshotVersion %s is still building", versionID)
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
		return fmt.Errorf("usage: orrery snapshot import <name> <ref@sha256:…> [--auth-secret NAME]")
	}
	name, ref := positional[0], positional[1]
	snapshotID, err := ensureSnapshot(c, name)
	if err != nil {
		return err
	}
	imp := &orreryclient.SnapshotImport{Ref: ref}
	if authSecret != "" {
		imp.AuthSecret = &authSecret
	}
	version, err := c.CreateSnapshotVersion(ctx, snapshotID,
		orreryclient.SnapshotVersionRequest{Import: imp}, "")
	if err != nil {
		return err
	}
	final, err := pollSnapshotVersion(c, snapshotID, version.ID)
	if err != nil {
		return err
	}
	return printSnapshotVersion(c, final)
}

// ensureSnapshot finds the caller's Snapshot by name or creates it.
func ensureSnapshot(c *client, name string) (string, error) {
	if id, err := findSnapshotByName(c, name, true); err == nil {
		return id, nil
	}
	snapshot, err := c.CreateSnapshot(ctx, name)
	if err == nil {
		return snapshot.ID, nil
	}
	var problem *orreryclient.Error
	if errors.As(err, &problem) && problem.Status == http.StatusConflict {
		return findSnapshotByName(c, name, true)
	}
	return "", err
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
		page, err := c.ListSnapshots(ctx, 200, cursor)
		if err != nil {
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
	snapshot, err := c.GetSnapshot(ctx, snapshotID)
	if err != nil {
		return err
	}
	if snapshot.CurrentVersionID == nil {
		return c.print(snapshot)
	}
	version, err := c.GetSnapshotVersion(ctx, snapshotID, *snapshot.CurrentVersionID)
	if err != nil {
		return err
	}
	return c.print(version)
}

// printSnapshotVersion prints the final version and fails when it did not
// become ready.
func printSnapshotVersion(c *client, version *orreryclient.SnapshotVersion) error {
	if err := c.print(version); err != nil {
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
