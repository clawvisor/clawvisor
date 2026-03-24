// Package web embeds the built frontend (dist/) so the dashboard
// can be served from a single binary without external files.
package web

import "embed"

//go:embed dist
var DistFS embed.FS
