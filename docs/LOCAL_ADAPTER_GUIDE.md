# Building a Local Adapter

This guide explains how the Clawvisor local daemon (`cmd/clawvisor-local`) works
and how to write a local adapter (called a "service") that bridges any local
tool to the cloud.

---

## What is the local daemon?

The local daemon is a lightweight process that runs on the user's machine. It:

1. Discovers **services** — directories containing a `service.yaml` manifest.
2. Pairs with Clawvisor Cloud via a time-limited code.
3. Maintains a persistent WebSocket tunnel to the cloud.
4. Receives action requests from the cloud and dispatches them to the
   appropriate local service.
5. Returns structured results back through the tunnel.

The daemon never stores API keys or credentials — it is a "dumb executor." The
cloud handles authorization, credential injection, and audit logging.

## Install

Install the latest released `clawvisor-local` binary with:

```bash
curl -fsSL https://raw.githubusercontent.com/clawvisor/clawvisor/main/scripts/install-local.sh | sh
```

This downloads the latest published `clawvisor-local` binary for your platform,
verifies it against the release `checksums.txt`, installs it into
`~/.clawvisor/bin`, and runs `clawvisor-local install-service`.

```
┌─────────────────────────────────────────────────────────┐
│                   Clawvisor Cloud                       │
│                                                         │
│  Agent ──► Gateway ──► Intent Check ──► Tunnel Server   │
└────────────────────────────────────────────┬────────────┘
                                             │ WebSocket
┌────────────────────────────────────────────▼────────────┐
│                   Local Daemon                          │
│                                                         │
│  Tunnel Client ──► Dispatcher ──► Exec / Server Runner  │
│                                                         │
│  Service Registry ◄── Discovery (scans service.yaml)    │
└─────────────────────────────────────────────────────────┘
```

---

## Service types

A local service is one of two types:

| Type | How it runs | Best for |
|------|------------|----------|
| **exec** | Spawns a fresh process per action request | Scripts, CLI tools, one-shot commands |
| **server** | Long-running HTTP server on a Unix socket | Stateful services, fast responses, persistent connections |

---

## Quick start: exec service

Create a directory under `~/.clawvisor/local/services/` with a `service.yaml`
and any scripts it references.

```
~/.clawvisor/local/services/
  my-tool/
    service.yaml
    run.sh
```

**`service.yaml`:**

```yaml
name: My Tool
description: Does something useful on the local machine

actions:
  - id: greet
    name: Say hello
    description: Greets the user by name
    run: ./run.sh
    timeout: 10s
    params:
      - name: person
        type: string
        required: true
        description: Name of the person to greet
```

**`run.sh`:**

```bash
#!/bin/bash
echo "Hello, $PARAM_PERSON!"
```

That's it. The daemon discovers the service on startup (or when reloaded via
`POST /api/services/reload`), and the action becomes available to agents through
the cloud.

### How exec dispatch works

1. The daemon resolves parameters (applies defaults for missing optional params).
2. It builds an environment that includes:
   - The system environment
   - Global env vars from `config.yaml`
   - Service-level `env` entries
   - Action-level `env` entries
   - `PARAM_<UPPER_NAME>` for each parameter (e.g. `PARAM_PERSON`)
   - Magic variables: `CLAWVISOR_SERVICE_ID`, `CLAWVISOR_ACTION_ID`,
     `CLAWVISOR_SERVICE_DIR`, `CLAWVISOR_REQUEST_ID`
3. It runs the command with a timeout (default 30s, configurable per action).
4. stdout and stderr are captured (up to 1 MB by default).
5. Exit code 0 → `success: true`. Non-zero → `success: false`, stderr returned
   as the error message.
6. On timeout, the process group receives SIGTERM, then SIGKILL after 3 seconds.

### Reading parameters in exec services

Parameters are available in two ways:

- **Environment variables**: `PARAM_<NAME>` with the name uppercased and hyphens
  replaced with underscores. E.g. parameter `file-path` → `PARAM_FILE_PATH`.
- **Stdin template**: If the action has a `stdin` field, the daemon interpolates
  `{{param_name}}` placeholders and pipes the result to the process's stdin.

```yaml
actions:
  - id: process_json
    name: Process JSON input
    run: ./handler.sh
    stdin: '{"name": "{{person}}", "count": {{count}}}'
    params:
      - name: person
        type: string
        required: true
      - name: count
        type: number
        required: true
```

---

## Quick start: server service

A server service is a long-running HTTP server. The daemon starts it, waits for
a health check, and routes requests to it over a Unix domain socket.

```
~/.clawvisor/local/services/
  my-server/
    service.yaml
    server.py
```

**`service.yaml`:**

```yaml
name: My Server
type: server
start: ./server.py
startup_timeout: 15
startup: lazy
health_check: /health

actions:
  - id: search
    name: Search records
    description: Full-text search over local records
    method: GET
    path: /api/search
    params:
      - name: query
        type: string
        required: true
        description: Search query

  - id: create_record
    name: Create a record
    method: POST
    path: /api/records
    body: '{"title": "{{title}}", "content": "{{content}}"}'
    body_format: json
    params:
      - name: title
        type: string
        required: true
      - name: content
        type: string
        required: true
```

**`server.py`:** (any language works — just listen on the Unix socket)

```python
#!/usr/bin/env python3
import os, json
from http.server import HTTPServer, BaseHTTPRequestHandler
import socket

class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == "/health":
            self.send_response(200)
            self.end_headers()
            return
        # ... handle other routes
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(json.dumps({"results": []}).encode())

    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        body = json.loads(self.rfile.read(length)) if length else {}
        # ... handle the request
        self.send_response(201)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(json.dumps({"id": "123"}).encode())

sock_path = os.environ["CLAWVISOR_SOCKET"]
server = HTTPServer(("localhost", 0), Handler)
server.socket.close()
server.socket = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
server.socket.bind(sock_path)
server.socket.listen(5)
server.serve_forever()
```

### How server dispatch works

1. On first request (lazy startup) or at daemon start (eager startup), the
   daemon starts the server process with `CLAWVISOR_SOCKET` set to a Unix
   socket path under `~/.clawvisor/local/run/`.
2. The daemon polls `GET <health_check>` (default `/health`) until it gets a
   200 response, or until `startup_timeout` seconds elapse.
3. Once healthy, the daemon routes action requests as HTTP calls over the socket:
   - `method` and `path` from the action definition.
   - `{{param_name}}` placeholders in `body` are interpolated with parameter
     values (JSON-escaped when `body_format` is `json`).
   - Parameters not referenced in the body template are appended as query
     parameters.
   - Service-level and action-level `headers` are merged (action takes
     precedence).
4. HTTP 2xx → `success: true`. Other status codes → `success: false`.
5. If the server process exits unexpectedly, the daemon marks it unhealthy and
   restarts it on the next request with exponential backoff (1s, 2s, 4s, 8s,
   16s, 30s cap). After 3 consecutive start failures, the service is marked
   as permanently failed until the daemon is restarted.

---

## `service.yaml` reference

### Top-level fields

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `name` | string | **required** | Display name |
| `id` | string | derived from path | Unique service identifier. If omitted, derived from the directory path relative to the service directory (e.g. `my-tool` or `vendor.tool`). |
| `description` | string | | Human-readable description |
| `icon` | string | | Icon identifier |
| `type` | string | `exec` | `exec` or `server` |
| `platform` | string | | Restrict to `linux`, `darwin`, or `windows`. Skipped on other platforms. |
| `env` | map | | Service-level environment variables, inherited by all actions. Supports `{{param}}` interpolation. |
| `working_dir` | string | service directory | Working directory for commands. Relative paths resolve against the service directory. |

### Server-specific fields

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `start` | string or list | **required** | Command to start the server. Supports shell-style strings (`"./server --port 8080"`) or explicit argv (`["./server", "--port", "8080"]`). |
| `startup_timeout` | int | `10` | Seconds to wait for the health check to pass. |
| `startup` | string | `lazy` | `lazy` (start on first request) or `eager` (start immediately). |
| `health_check` | string | `/health` | HTTP path for health checks. Must return 200 when ready. |
| `headers` | map | | Service-level HTTP headers, sent with every action request. |

### Action fields

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `id` | string | **required** | Unique action identifier within the service |
| `name` | string | same as `id` | Display name |
| `description` | string | | Human-readable description |
| `timeout` | string | `30s` | Go duration (e.g. `10s`, `2m`). Overrides the global default. |
| `env` | map | | Action-level environment variables (exec mode). Supports `{{param}}` interpolation. |
| `params` | list | | Parameter declarations (see below) |

**Exec-specific action fields:**

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `run` | string or list | **required** | Command to execute. Relative paths resolve against the service directory. |
| `stdin` | string | | Template piped to stdin. Supports `{{param}}` placeholders. |

**Server-specific action fields:**

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `method` | string | `GET` | HTTP method |
| `path` | string | **required** | Request path on the Unix socket |
| `body` | string | | Request body template. Supports `{{param}}` placeholders. |
| `body_format` | string | `json` if body is set, else `text` | `json` (values are JSON-escaped) or `text` (raw interpolation) |
| `headers` | map | | Action-level HTTP headers (merged over service-level headers) |

### Parameter fields

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `name` | string | **required** | Parameter name |
| `type` | string | `string` | `string`, `number`, `boolean`, or `json` |
| `required` | bool | `false` | Whether the parameter is mandatory |
| `default` | string | | Default value. Cannot be set together with `required`. |
| `description` | string | | Human-readable description |

---

## Service discovery

The daemon scans directories listed in `config.yaml` under `service_dirs`
(default: `~/.clawvisor/local/services`). It walks each directory looking for
`service.yaml` files.

**Service ID derivation:** If a manifest doesn't set `id`, the ID is derived
from the directory path relative to the service root. Path separators become
dots. For example:

```
~/.clawvisor/local/services/
  acme/
    widgets/
      service.yaml    → ID: acme.widgets
  my-tool/
    service.yaml      → ID: my-tool
```

**Duplicate IDs** are rejected — all services with the same ID are excluded
with a conflict error.

**Platform filtering:** If `platform` is set in the manifest and doesn't match
the current OS, the service is silently skipped.

**Reloading:** Services can be reloaded without restarting the daemon by calling
`POST http://localhost:25299/api/services/reload`.

---

## Template interpolation

Both exec and server modes support `{{param_name}}` placeholders in several
fields:

- `stdin` (exec mode)
- `body` (server mode)
- `env` values (both modes)

Only declared parameters are interpolated — unknown placeholders are left
as-is. When the format is `json`, values are JSON-escaped (special characters
like `"` and `\n` are properly escaped). When the format is `text`, values are
inserted raw.

---

## Daemon configuration

The daemon reads `~/.clawvisor/local/config.yaml`:

```yaml
name: my-machine              # Display name (defaults to hostname)
port: 25299                    # Pairing server port
log_level: info                # debug, info, warn, error
keep_awake: false              # macOS toolbar mode: prevent idle sleep when enabled

service_dirs:
  - ~/.clawvisor/local/services   # Tilde expansion supported

scan_interval: 0               # Seconds between auto-scans (0 = disabled)
default_timeout: 30s           # Default action timeout
max_output_size: 1048576       # 1 MB output limit per action
max_concurrent_requests: 10    # Max actions running in parallel

env:                           # Global env vars injected into all actions
  MY_GLOBAL_VAR: some-value

allowed_cloud_origins:
  - https://app.clawvisor.com  # CORS whitelist for the pairing server
```

On macOS, `clawvisor-local toolbar` runs the local daemon as a menu bar app.
`clawvisor-local install-service` now installs that toolbar mode as a LaunchAgent
so the daemon stays visible in the menu bar and can toggle `keep_awake`.
Remove it with `clawvisor-local uninstall-service`.

---

## Pairing and connection lifecycle

1. The daemon starts a local HTTP server on `localhost:25299`.
2. It generates a 6-digit pairing code (valid for 10 minutes, rotates every 5
   minutes).
3. The user enters the code in the Clawvisor Cloud dashboard (or scans a QR
   code).
4. The cloud calls `POST /api/pairing/complete` on the local daemon with a
   64-character hex connection token.
5. The daemon stores the token in `~/.clawvisor/local/state.json` (permissions
   0600) and establishes a WebSocket tunnel to the cloud.
6. On the tunnel, the daemon sends an **auth** frame (with the token), then a
   **capabilities** frame listing all discovered services and actions.
7. The tunnel is persistent — it reconnects with exponential backoff (1s → 60s
   cap). If the token is revoked by the cloud, reconnection stops.

---

## Concurrency and timeouts

- The daemon processes up to `max_concurrent_requests` actions in parallel
  (default 10). Additional requests queue for up to 30 seconds before timing
  out.
- Each action has its own `timeout` (default 30s). On timeout, exec processes
  receive SIGTERM → SIGKILL (3s grace).
- Output (stdout/stderr for exec, response body for server) is capped at
  `max_output_size` (default 1 MB). Larger output is truncated and the response
  includes a `truncated` flag.

---

## Examples

### Shell script adapter (exec)

A service that queries a local SQLite database:

```yaml
name: Local DB
description: Query the local app database

actions:
  - id: recent_users
    name: Recent users
    description: List users created in the last N days
    run: sqlite3 -json /path/to/app.db
    stdin: "SELECT * FROM users WHERE created_at > datetime('now', '-{{days}} days') LIMIT {{limit}};"
    timeout: 5s
    params:
      - name: days
        type: number
        required: true
        description: Number of days to look back
      - name: limit
        type: number
        default: "50"
        description: Maximum number of results
```

### Python server adapter

A service that wraps a local ML model:

```yaml
name: Local Model
type: server
start: python3 ./serve.py
startup_timeout: 30
startup: eager
health_check: /health

env:
  MODEL_PATH: /path/to/model.bin

actions:
  - id: predict
    name: Run prediction
    method: POST
    path: /predict
    body: '{"text": "{{text}}"}'
    body_format: json
    timeout: 60s
    params:
      - name: text
        type: string
        required: true
        description: Input text for prediction
```

### Multi-action service

A file management service with multiple actions:

```yaml
name: File Manager
description: Local file operations

actions:
  - id: list_files
    name: List files
    run: "ls -la {{directory}}"
    params:
      - name: directory
        type: string
        required: true
        description: Directory path to list

  - id: read_file
    name: Read file contents
    run: "cat {{path}}"
    timeout: 5s
    params:
      - name: path
        type: string
        required: true
        description: File path to read

  - id: search
    name: Search files
    run: "grep -r {{pattern}} {{directory}}"
    timeout: 30s
    params:
      - name: pattern
        type: string
        required: true
      - name: directory
        type: string
        required: true
```

---

## How local services differ from cloud YAML adapters

Clawvisor has two adapter systems. Choose the right one for your use case:

| | Cloud YAML adapter (`INTEGRATION_YAML_SPEC.md`) | Local service (`service.yaml`) |
|---|---|---|
| **Runs where** | On the Clawvisor server | On the user's machine |
| **Talks to** | External APIs over HTTPS | Local processes, files, databases |
| **Auth** | OAuth, API keys (managed by cloud vault) | None (daemon is trusted locally) |
| **Defined with** | REST/GraphQL endpoint declarations | Shell commands or local HTTP servers |
| **When to use** | Wrapping third-party SaaS APIs | Wrapping local tools, CLIs, databases, hardware |
