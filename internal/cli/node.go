package cli

import (
	"fmt"
	"io"
	"net/http"
)

func runNode(c *client, args []string, stderr io.Writer) error {
	const usage = "usage: stars-sandbox node declare-lost <id>"
	if len(args) == 0 {
		return fmt.Errorf(usage)
	}
	switch args[0] {
	case "declare-lost":
		if len(args) != 2 {
			return fmt.Errorf(usage)
		}
		return c.sendAndPrint(http.MethodPost, "/api/v1/nodes/"+args[1]+"/declare-lost", nil, "")
	default:
		return fmt.Errorf("unknown node command %q", args[0])
	}
}
