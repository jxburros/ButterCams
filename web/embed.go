// Package web embeds the built settings UI into the ButterCams binary.
//
// Run "npm install && npm run build" in this folder before "go build" to
// include the UI; without it the binary still builds and serves a short page
// explaining that the UI was not built.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// Dist returns the built UI files (the contents of web/dist).
func Dist() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err) // the embedded tree always has a dist folder
	}
	return sub
}

// Built reports whether the UI was built before the binary was compiled.
func Built() bool {
	_, err := fs.Stat(Dist(), "index.html")
	return err == nil
}
