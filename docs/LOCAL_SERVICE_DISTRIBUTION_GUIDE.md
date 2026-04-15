# Publishing Remote-Installable Local Services

This guide explains how to publish a GitHub repo that `clawvisor-local` can
install from with commands like:

```bash
clawvisor-local list your-org/your-local-services
clawvisor-local inspect your-org/your-local-services your-service
clawvisor-local install your-org/your-local-services your-service
```

This is a separate concern from writing `service.yaml` files. The local adapter
guide covers how to author a service; this guide covers how to package and
publish services so users can install them from GitHub releases.

---

## Current model

Today, remote-managed service installation has a fixed v1 model:

- source: GitHub repos only
- release source: latest release only
- manifest asset name: `clawvisor-local-manifest.json`
- archive type: `tar.gz` only
- service bundle install root: `~/.clawvisor/local/services`
- runtime asset install root: `~/.clawvisor/bin`

The installer validates the manifest, downloads the matching release assets,
verifies each asset against the SHA-256 declared in the manifest, installs the
files atomically, updates `~/.clawvisor/local/state/installed-services.json`,
and then tries to reload the running daemon.

---

## Repo layout

Your distribution repo can be organized however you want, but each release must
publish a manifest plus the referenced archives. A typical source layout looks
like:

```text
local-integrations/
  services/
    apple.imessage/
      service.yaml
    apple.photos/
      service.yaml
  helpers/
    imessage/
      Clawvisor iMessage Helper.app/
    photos/
      Clawvisor Photos Helper.app/
  scripts/
    build-release.sh
```

The source tree does not matter to `clawvisor-local`. Only the contents of the
published release matter.

---

## Release assets

Each GitHub release should publish:

- `clawvisor-local-manifest.json`
- one `tar.gz` service bundle per service
- any runtime/helper archives referenced by those services

Example release asset set:

```text
clawvisor-local-manifest.json
service-apple.imessage.tar.gz
service-apple.photos.tar.gz
clawvisor-imessage-helper-darwin-arm64.tar.gz
clawvisor-imessage-helper-darwin-amd64.tar.gz
clawvisor-photos-helper-darwin-arm64.tar.gz
clawvisor-photos-helper-darwin-amd64.tar.gz
```

`clawvisor-local` resolves the latest release, fetches
`clawvisor-local-manifest.json`, and then downloads the assets referenced by the
manifest from that same release.

---

## Manifest format

The manifest must be JSON and currently uses schema version `1`.

```json
{
  "schema_version": 1,
  "repo": "your-org/your-local-services",
  "version": "v0.1.0",
  "min_clawvisor_local_version": "0.8.10",
  "services": [
    {
      "id": "apple.imessage",
      "name": "Apple Messages",
      "description": "Read local iMessage data",
      "aliases": ["imessage", "messages"],
      "platforms": ["darwin"],
      "service_schema_version": 1,
      "service_bundle": {
        "asset_name": "service-apple.imessage.tar.gz",
        "sha256": "…",
        "archive_type": "tar.gz",
        "install_to": "services/apple.imessage",
        "replace_mode": "atomic_dir_replace"
      },
      "runtime_assets": [
        {
          "asset_name": "clawvisor-imessage-helper-darwin-arm64.tar.gz",
          "sha256": "…",
          "archive_type": "tar.gz",
          "install_to": "bin/Clawvisor iMessage Helper.app",
          "replace_mode": "atomic_dir_replace",
          "os": "darwin",
          "arch": "arm64"
        }
      ],
      "post_install": {
        "permissions": ["Full Disk Access"],
        "apps_requiring_fda": ["Clawvisor iMessage Helper.app"],
        "notes": ["Grant Full Disk Access to the helper app if prompted."],
        "restart_required": false
      }
    }
  ]
}
```

Important rules enforced by `clawvisor-local`:

- `repo` must match the repo the user requested
- `schema_version` must be `1`
- `service_schema_version` must be `1`
- service IDs must be unique
- aliases must be unique after lowercase normalization
- `platforms` must be present
- `archive_type` must be `tar.gz`
- `install_to` must be relative and must start with `services/` or `bin/`
- the service bundle `install_to` must be exactly `services/<service-id>`
- `replace_mode` must be `atomic_dir_replace` or `atomic_file_replace`

If `min_clawvisor_local_version` is set, the installer rejects the manifest when
the local binary is too old.

---

## Building the archives

Each archive should unpack cleanly into the target path declared by
`install_to`.

For a service bundle:

- archive name: `service-apple.imessage.tar.gz`
- install target: `services/apple.imessage`
- extracted content must contain a valid `service.yaml` at the bundle root

For a helper/runtime asset:

- archive name: `clawvisor-imessage-helper-darwin-arm64.tar.gz`
- install target: `bin/Clawvisor iMessage Helper.app`
- extracted content should match the final directory or file being installed

The installer validates the extracted `service.yaml` before committing the
service bundle. It also rejects path collisions unless the existing installed
content has the same digest.

---

## Asset ownership and upgrades

`clawvisor-local` tracks installed service ownership in:

```text
~/.clawvisor/local/state/installed-services.json
```

That state records:

- which repo/version installed each service
- which installed paths belong to which service
- the source asset names and hashes
- a content digest used for collision detection

This enables:

- `clawvisor-local upgrade <service-id>`
- `clawvisor-local uninstall <service-id>`
- shared runtime assets to remain installed while another service still owns them

If two different services try to install different bytes to the same `bin/...`
or `services/...` path, installation fails.

---

## User-facing CLI flow

Once your repo is published, users interact with it through `clawvisor-local`:

```bash
clawvisor-local list your-org/your-local-services
clawvisor-local inspect your-org/your-local-services imessage
clawvisor-local install your-org/your-local-services imessage
clawvisor-local upgrade apple.imessage
clawvisor-local uninstall apple.imessage --prune-assets
```

Behavior to expect:

- `list` reads the latest release manifest and shows available services
- `inspect` shows the selected service, platform support, and matching runtime assets
- `install` downloads assets, installs them, and attempts a daemon reload
- `upgrade` reinstalls from the latest release for the recorded source repo
- `uninstall` removes the service bundle and optionally prunes unowned runtime assets

If the daemon is not running, filesystem changes still succeed but the user has
to run `clawvisor-local reload` or restart the daemon.

---

## Practical recommendations

- Keep your service authoring repo and your distribution repo separate if you
  want tighter control over release publishing.
- Use stable asset names across releases so diffs are easier to reason about.
- Set `min_clawvisor_local_version` whenever you rely on newer installer or
  service runtime behavior.
- Include clear `post_install.notes` and `permissions` for anything involving
  Full Disk Access, Accessibility, Automation, or TCC prompts on macOS.
- Test your release in a clean home directory before publishing:

```bash
export HOME="$(mktemp -d)"
~/.clawvisor/bin/clawvisor-local list your-org/your-local-services
~/.clawvisor/bin/clawvisor-local install your-org/your-local-services your-service
```

---

## Relationship to `service.yaml`

The service bundle still contains an ordinary local service:

- `service.yaml`
- scripts or server binaries it needs
- any service-local assets

This guide does not replace the authoring guide. Write and test the service
first, then package and publish it through a release manifest.
