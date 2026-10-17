//go:build embed

// Package web carries the built Web console. `make console` builds it into
// dist and compiles this binary with -tags embed; without the tag the
// placeholder committed in the repo is served instead (api.md §8).
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var files embed.FS

// FS returns the built console.
func FS() fs.FS {
	sub, err := fs.Sub(files, "dist")
	if err != nil {
		panic(err) // dist is fixed at build time
	}
	return sub
}
