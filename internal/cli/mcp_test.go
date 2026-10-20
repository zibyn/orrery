package cli

import (
	"bytes"
	"strings"
	"testing"
)

// The MCP server runs over stdio, so anything on stdout would corrupt the
// protocol; refusals are plain errors on stderr.
func TestMCPRejectsSandboxTokensAndArguments(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"sandbox token", []string{"--token", "ors_0123456789abcdef_secret", "mcp"}, "Sandbox Token"},
		{"extra argument", []string{"--token", "orr_0123456789abcdef_secret", "mcp", "start"}, "takes no arguments"},
	} {
		var stdout, stderr bytes.Buffer
		code := Run(tc.args, &stdout, &stderr)
		if code == 0 {
			t.Errorf("%s: exit 0, want failure", tc.name)
		}
		if !strings.Contains(stderr.String(), tc.want) {
			t.Errorf("%s: stderr = %q, want %q", tc.name, stderr.String(), tc.want)
		}
		if stdout.Len() != 0 {
			t.Errorf("%s: wrote %q to stdout", tc.name, stdout.String())
		}
	}
}
