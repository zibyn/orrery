//go:build !embed

// Package web carries the built Web console. Without -tags embed, which
// `make console` sets after building dist, the placeholder committed in the
// repo is served so that plain `go build` needs no Node toolchain (api.md §8).
package web

import (
	"embed"
	"io/fs"
)

//go:embed placeholder
var files embed.FS

// FS returns the placeholder console.
func FS() fs.FS {
	sub, err := fs.Sub(files, "placeholder")
	if err != nil {
		panic(err) // the embedded tree is fixed at build time
	}
	return sub
}
