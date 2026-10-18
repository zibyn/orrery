// Package dockerfile holds the small Dockerfile helpers shared by the build
// request validator and the Agent's provenance fallback. LogicalLines is
// deliberately not a full Dockerfile parser: both callers only need image
// references, and anything they cannot resolve is rejected or ignored.
package dockerfile

import "strings"

// LogicalLines joins continuation lines and drops full-line comments.
func LogicalLines(text string) []string {
	var out []string
	var current strings.Builder
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSuffix(raw, "\r")
		if current.Len() == 0 {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
		}
		trimmed := strings.TrimRight(line, " \t")
		continuation := strings.HasSuffix(trimmed, "\\")
		if continuation {
			trimmed = strings.TrimRight(strings.TrimSuffix(trimmed, "\\"), " \t")
		}
		if current.Len() > 0 {
			current.WriteByte(' ')
		}
		current.WriteString(strings.TrimSpace(trimmed))
		if !continuation {
			out = append(out, current.String())
			current.Reset()
		}
	}
	if current.Len() > 0 {
		out = append(out, current.String())
	}
	return out
}
