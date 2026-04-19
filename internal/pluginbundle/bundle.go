// Package pluginbundle embeds the OpenClaw plugin tarball into the
// Clawvisor server binary and exposes accessors for serving it. The
// tarball is produced by `npm --prefix extensions/clawvisor run bundle`
// (or `make plugin-bundle`) and must be regenerated whenever the plugin
// source changes. CI runs the bundle step before `go build` so production
// binaries always ship a bundle matched to the plugin source at that
// commit.
package pluginbundle

import (
	"embed"
	"errors"
	"io/fs"
	"strings"
)

//go:embed all:embed
var embedFS embed.FS

// ErrUnavailable is returned when the build did not run the bundle step —
// e.g. a dev build with `go build ./...` straight out of a fresh clone.
// The handler surfaces this as 503 so the user knows to run `make
// plugin-bundle` (or let CI handle it).
var ErrUnavailable = errors.New("plugin bundle not produced for this build; run `make plugin-bundle`")

// Tarball returns the embedded plugin tarball bytes. Safe to call many
// times; the slice points at the binary's read-only embed.FS backing.
func Tarball() ([]byte, error) {
	return embedFS.ReadFile("embed/openclaw-plugin.tgz")
}

// SHA256 returns the embedded sha256 file contents (hex digest plus
// trailing filename, sha256sum-compatible format). The accessor exists
// so the HTTP handler can serve it directly as plain text.
func SHA256() ([]byte, error) {
	return embedFS.ReadFile("embed/openclaw-plugin.sha256")
}

// Version returns the version string the bundler wrote into the tarball
// (or "" if unavailable). Used by the pair endpoint to compare plugin
// compat against this build.
func Version() string {
	// The version is inside the tarball itself (clawvisor/VERSION) but
	// duplicating it as a top-level file at bundle time makes the check
	// a cheap read. If the bundle step hasn't run, return "".
	b, err := embedFS.ReadFile("embed/VERSION")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// Available reports whether a real tarball was embedded (vs only the
// NOTE.md sentinel from a non-bundle build).
func Available() bool {
	if _, err := fs.Stat(embedFS, "embed/openclaw-plugin.tgz"); err != nil {
		return false
	}
	return true
}
