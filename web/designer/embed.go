// Package designerui embeds the built template-designer SPA so the
// service ships as one binary. `npm run build` in this directory
// regenerates dist/; the committed placeholder keeps `go build` working
// before the first UI build.
package designerui

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var distFS embed.FS

// Dist is the built SPA rooted at its index.html.
func Dist() (fs.FS, error) {
	return fs.Sub(distFS, "dist")
}
