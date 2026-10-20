package cli

import (
	"fmt"
	"io"

	orreryclient "github.com/zibyn/orrery/client"
)

// runNode is the admin Node lifecycle surface (api.md §3, ADR-0014).
// declare-lost clears the Token and cascades; pause/resume is the placement
// switch; delete requires a paused, drained Node and leaves a tombstone.
func runNode(c *client, args []string, stderr io.Writer) error {
	const usage = "usage: orrery node pause|resume|delete|declare-lost <id>"
	if len(args) == 0 {
		return fmt.Errorf(usage)
	}
	switch args[0] {
	case "pause", "resume":
		if len(args) != 2 {
			return fmt.Errorf("usage: orrery node %s <id>", args[0])
		}
		var (
			node *orreryclient.Node
			err  error
		)
		if args[0] == "pause" {
			node, err = c.PauseNode(ctx, args[1])
		} else {
			node, err = c.ResumeNode(ctx, args[1])
		}
		if err != nil {
			return err
		}
		return c.print(node)
	case "delete":
		if len(args) != 2 {
			return fmt.Errorf("usage: orrery node delete <id>")
		}
		node, err := c.DeleteNode(ctx, args[1])
		if err != nil {
			return err
		}
		return c.print(node)
	case "declare-lost":
		if len(args) != 2 {
			return fmt.Errorf("usage: orrery node declare-lost <id>")
		}
		node, err := c.DeclareNodeLost(ctx, args[1])
		if err != nil {
			return err
		}
		return c.print(node)
	default:
		return fmt.Errorf("unknown node command %q", args[0])
	}
}
