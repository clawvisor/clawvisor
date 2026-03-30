// Package coworkplugin embeds the Clawvisor Cowork plugin files so they can
// be installed into Claude Desktop's plugin directory during integration.
package coworkplugin

import "embed"

//go:embed all:plugin
var FS embed.FS
