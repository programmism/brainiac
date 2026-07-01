// Package webui serves the read-only WebUI — a single embedded static page that
// calls the REST API (SYSTEM.md §6.2, ADR 0001). It is a client, not a home for
// logic: all "smarts" stay in core, reached via /api.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static
var files embed.FS

// Handler serves the embedded static assets (index.html at "/").
func Handler() http.Handler {
	sub, err := fs.Sub(files, "static")
	if err != nil {
		panic(err) // embedded at build time; cannot fail at runtime
	}
	return http.FileServer(http.FS(sub))
}
