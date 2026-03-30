// Package skills embeds the Clawvisor OpenClaw skill files so they can be
// served directly from the running server without needing the source tree.
package skills

import "embed"

//go:embed all:clawvisor
var FS embed.FS
