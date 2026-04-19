# Plugin bundle embed directory

This directory is populated by `npm --prefix extensions/clawvisor run bundle`
(or, more usually, `make plugin-bundle`). Expected outputs:

- `openclaw-plugin.tgz` — the plugin tarball that the Clawvisor server
  serves at `GET /skill/openclaw-plugin.tgz`. Embedded into the server
  binary via `//go:embed` at build time.
- `openclaw-plugin.sha256` — content hash of the tarball, served at
  `GET /skill/openclaw-plugin.sha256` for integrity verification.
- `openclaw-plugin-v<VERSION>.tgz` — version-pinned duplicate, same bytes,
  for stable URL references.

Those files are `.gitignore`d; this `NOTE.md` is committed so `go:embed` is
always satisfied even in a fresh checkout. CI runs the bundle step before
`go test` / `go build`, so production binaries always carry a real tarball.

If you see a 503 `PLUGIN_BUNDLE_UNAVAILABLE` from a running server, the
bundle wasn't generated for that build — run `make plugin-bundle` and
rebuild.
