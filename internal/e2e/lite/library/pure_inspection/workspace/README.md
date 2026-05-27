# Foo Service

The Foo Service handles synchronous Foo requests, batches Foo events
overnight, and emits Bar updates downstream.

## Quick start

    foo init
    foo serve --port 8080

## Layout

- `cmd/foo/` — the daemon entrypoint
- `internal/foo/` — request handling, batching, retry loop
- `internal/bar/` — Bar emitter and downstream client

Maintainer: foo-team@example.com
