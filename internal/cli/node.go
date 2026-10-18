package cli

import (
	"fmt"
	"io"
	"net/http"
)

// runNode is the admin Node lifecycle surface (api.md §3, ADR-0014).
// declare-lost clears the Token and cascades; pause/resume is the placement
// switch; delete requires a paused, drained Node and leaves a tombstone.
func runNode(c *client, args []string, stderr io.Writer) error {
	const usage = "usage: stars-sandbox node pause|resume|delete|declare-lost <id>"
	if len(args) == 0 {
		return fmt.Errorf(usage)
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
	case "declare-lost":
		if len(args) != 2 {
			return fmt.Errorf("usage: stars-sandbox node declare-lost <id>")
		}
		return c.sendAndPrint(http.MethodPost, "/api/v1/nodes/"+args[1]+"/declare-lost", nil, "")
	default:
		return fmt.Errorf("unknown node command %q", args[0])
	}
}
