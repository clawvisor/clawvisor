// Package definitions embeds the built-in YAML adapter definitions.
package definitions

import "embed"

//go:embed *.yaml
var FS embed.FS
