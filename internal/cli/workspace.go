package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// publishFollowTimeout bounds waiting for the Agent to finish a publish.
const publishFollowTimeout = 10 * time.Minute

type workspacePublishVersion struct {
	ID         string  `json:"id"`
	ResourceID string  `json:"resource_id"`
	State      string  `json:"state"`
	Cause      *string `json:"cause"`
}

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
		return c.sendAndPrint(http.MethodPost, "/api/v1/workspaces/"+args[1]+"/declare-lost", nil, "")
	case "abandon":
		if len(args) != 2 {
			return fmt.Errorf("usage: stars-sandbox workspace abandon <id>")
		}
		return c.sendAndPrint(http.MethodPost, "/api/v1/workspaces/"+args[1]+"/abandon", nil, "")
	default:
		return fmt.Errorf("unknown workspace command %q", args[0])
	}
}

// publishWorkspace starts a publish and follows the pending ResourceVersion
// until it is terminal (api.md §3).
func publishWorkspace(c *client, workspaceID, path, name string) error {
	status, data, err := c.send(http.MethodPost, "/api/v1/workspaces/"+workspaceID+"/publish", nil,
		map[string]string{"path": path, "resource": name}, "")
	if err != nil {
		return err
	}
	if status != http.StatusAccepted {
		return problemError(status, data)
	}
	var version workspacePublishVersion
	if err := json.Unmarshal(data, &version); err != nil {
		return fmt.Errorf("decode ResourceVersion: %w", err)
	}
	deadline := time.Now().Add(publishFollowTimeout)
	for version.State == "pending" {
		if time.Now().After(deadline) {
			return fmt.Errorf("publish %s did not finish within %s", version.ID, publishFollowTimeout)
		}
		time.Sleep(250 * time.Millisecond)
		data, err = c.do(http.MethodGet, "/api/v1/resources/"+version.ResourceID+"/versions/"+version.ID, nil, nil, "")
		if err != nil {
			return err
		}
		if err := json.Unmarshal(data, &version); err != nil {
			return fmt.Errorf("decode ResourceVersion: %w", err)
		}
	}
	if err := c.print(data); err != nil {
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
