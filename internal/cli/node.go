package cli

import (
	"fmt"
	"net/http"
)

// runNode is the admin Node lifecycle surface (api.md §3, ADR-0014). Delete
// is refused unless the Node is paused with no non-terminal Workspace or
// Sandbox, because it leaves a tombstone the Control Plane never purges.
func runNode(c *client, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: stars-sandbox node pause|resume|delete <id>")
	}
	switch args[0] {
	case "pause", "resume":
		if len(args) != 2 {
			return fmt.Errorf("usage: stars-sandbox node %s <id>", args[0])
		}
		return c.sendAndPrint(http.MethodPost, "/api/v1/nodes/"+args[1]+"/"+args[0], nil, "")
	case "delete":
		if len(args) != 2 {
			return fmt.Errorf("usage: stars-sandbox node delete <id>")
		}
		return c.sendAndPrint(http.MethodDelete, "/api/v1/nodes/"+args[1], nil, "")
	default:
		return fmt.Errorf("unknown node command %q", args[0])
	}
}
