package cli

import (
	"fmt"
	"io"
	"time"
)

// publishFollowTimeout bounds waiting for the Agent to finish a publish.
const publishFollowTimeout = 10 * time.Minute

func runWorkspace(c *client, args []string, stderr io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: stars-sandbox workspace publish|declare-lost|abandon ...")
	}
	switch args[0] {
	case "publish":
		if len(args) != 4 {
			return fmt.Errorf("usage: stars-sandbox workspace publish <id> <path> <name>")
		}
		return publishWorkspace(c, args[1], args[2], args[3])
	case "declare-lost":
		if len(args) != 2 {
			return fmt.Errorf("usage: stars-sandbox workspace declare-lost <id>")
		}
		workspace, err := c.DeclareWorkspaceLost(ctx, args[1])
		if err != nil {
			return err
		}
		return c.print(workspace)
	case "abandon":
		if len(args) != 2 {
			return fmt.Errorf("usage: stars-sandbox workspace abandon <id>")
		}
		workspace, err := c.AbandonWorkspace(ctx, args[1])
		if err != nil {
			return err
		}
		return c.print(workspace)
	default:
		return fmt.Errorf("unknown workspace command %q", args[0])
	}
}

// publishWorkspace starts a publish and follows the pending ResourceVersion
// until it is terminal (api.md §3).
func publishWorkspace(c *client, workspaceID, path, name string) error {
	version, err := c.PublishWorkspace(ctx, workspaceID, path, name)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(publishFollowTimeout)
	for version.State == "pending" {
		if time.Now().After(deadline) {
			return fmt.Errorf("publish %s did not finish within %s", version.ID, publishFollowTimeout)
		}
		time.Sleep(250 * time.Millisecond)
		version, err = c.GetResourceVersion(ctx, version.ResourceID, version.ID)
		if err != nil {
			return err
		}
	}
	if err := c.print(version); err != nil {
		return err
	}
	if version.State == "failed" {
		if version.Cause != nil {
			return fmt.Errorf("publish failed: %s", *version.Cause)
		}
		return fmt.Errorf("publish failed")
	}
	return nil
}
