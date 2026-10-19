// Package buildinfo reports the version a binary was built from.
package buildinfo

import "runtime/debug"

// version is set with -X at build time. The Docker build leaves .git out of
// its context, so its build info has no VCS data to fall back on (ADR-0023).
var version string

// Version is the release tag when the build stamped one, otherwise the
// version Go reads from the build, otherwise "dev".
func Version() string {
	if version != "" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}
