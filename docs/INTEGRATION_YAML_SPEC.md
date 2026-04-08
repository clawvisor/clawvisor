# Clawvisor Integration YAML Specification

This document describes the YAML format for declaring Clawvisor integrations (adapters). Each YAML file defines a single service integration — its authentication, API endpoints, parameters, and response handling.

Integration files are stored in `~/.clawvisor/adapters/` and hot-loaded on save.

## Top-Level Structure

```yaml
service:
  id: <string>           # unique lowercase identifier (e.g. "jira", "stripe")
  display_name: <string> # human-readable name
  description: <string>  # one-line description
  setup_url: <string>    # optional: link to API key / OAuth app setup page
  icon_svg: <string>     # optional: inline SVG markup
  identity:              # optional: auto-detect account identity after activation
    endpoint: <string>   # URL path to fetch identity (e.g. "/user")
    field: <string>      # dot-delimited JSON field (e.g. "login", "email")
    method: <string>     # optional: HTTP method, default "GET"
    body: <string>       # optional: request body (e.g. GraphQL query JSON)

auth:
  type: <"api_key" | "oauth2" | "basic" | "none">
  header: <string>         # e.g. "Authorization" (api_key only)
  header_prefix: <string>  # e.g. "Bearer " (api_key only)
  extra_headers:           # optional: additional headers on every request
    <key>: <value>
  # Include ONE of the following for OAuth flows:
  pkce_flow: ...    # PKCE authorization code (for public clients)
  device_flow: ...  # Device authorization grant (for CLI apps)
  oauth: ...        # Traditional OAuth2 (requires client secret)

api:
  base_url: <string>  # e.g. "https://api.github.com"
  type: <"rest" | "graphql">

# Optional: natural-language guidance for the intent verification system.
# Helps the verifier understand nuances of this service's actions.
verification_hints: <string>

actions:
  <action_name>:  # snake_case action identifier
    display_name: <string>
    risk:
      category: <"read" | "write" | "delete" | "search">
      sensitivity: <"low" | "medium" | "high">
      description: <string>  # what this action does
    method: <string>   # HTTP method (REST)
    path: <string>     # URL path with {{.param}} interpolation
    params: ...
    response: ...
```

## Authentication

### API Key / Bearer Token

The simplest auth type. The user provides a token that is sent in a header.

```yaml
auth:
  type: api_key
  header: "Authorization"
  header_prefix: "Bearer "
```

### Basic Auth

The credential is stored as a `user:pass` string. The runtime splits on `:` and uses Go's `SetBasicAuth()` to produce a standard `Authorization: Basic <base64>` header.

```yaml
auth:
  type: basic
```

No additional fields are needed — `header` and `header_prefix` are ignored for basic auth.

### PKCE Flow (Recommended for OAuth)

For APIs that support OAuth2. PKCE doesn't require a client secret — only a public client ID. The user configures their client ID on the Settings page or when connecting the service.

```yaml
auth:
  type: api_key
  header: "Authorization"
  header_prefix: "Bearer "
  pkce_flow:
    client_id_env: "SPOTIFY_CLIENT_ID"  # env var for client ID
    scopes: ["user-read-playback-state", "user-modify-playback-state"]
    authorize_url: "https://accounts.spotify.com/authorize"
    token_url: "https://accounts.spotify.com/api/token"
    token_path: "access_token"  # optional: JSON path to token in response
```

**Fields:**
- `client_id_env` — environment variable name for the client ID (recommended over hardcoding)
- `client_id` — hardcoded public client ID (use for well-known public apps)
- `scopes` — OAuth scopes needed by the actions
- `authorize_url` — OAuth2 authorization endpoint
- `token_url` — OAuth2 token endpoint
- `token_path` — optional JSON path to extract access token from token response (default: `access_token`)

### Device Flow

For CLI-friendly OAuth (no browser redirect needed). The user authorizes via a code displayed in the terminal.

```yaml
auth:
  type: api_key
  header: "Authorization"
  header_prefix: "token "
  device_flow:
    client_id: "Ov23lilVGK2hqWMGk9Qk"
    client_id_env: "GITHUB_CLIENT_ID"
    scopes: ["repo", "read:org"]
    device_code_url: "https://github.com/login/device/code"
    token_url: "https://github.com/login/oauth/access_token"
    grant_type: "urn:ietf:params:oauth:grant-type:device_code"  # optional: override grant_type in token exchange
```

**Fields:**
- `client_id` — hardcoded public client ID
- `client_id_env` — environment variable name for client ID override
- `scopes` — requested OAuth scopes
- `device_code_url` — device authorization endpoint
- `token_url` — token exchange endpoint
- `grant_type` — optional override for the `grant_type` parameter in the token request (default: `urn:ietf:params:oauth:grant-type:device_code`)

### Traditional OAuth2

For server-side OAuth2 flows that require a client secret. Use PKCE or device flow instead when possible.

```yaml
auth:
  type: oauth2
  oauth:
    endpoint: "google"                    # well-known provider ("google"), or omit for custom URLs
    vault_key: "google"                   # shared vault key across services using the same OAuth app
    scopes: ["https://www.googleapis.com/auth/gmail.readonly"]
    scope_merge: true                     # merge scopes with existing credential (for multi-service OAuth apps)
    conditional_scopes:                   # optional: scopes gated on environment variables
      - scope: "https://www.googleapis.com/auth/gmail.send"
        env_gate: "ENABLE_GMAIL_SEND"
        default: false                    # include if env var is unset?
    # Custom endpoint fields (used when `endpoint` is not a well-known provider):
    client_id_env: "ACME_CLIENT_ID"
    client_secret_env: "ACME_CLIENT_SECRET"
    authorize_url: "https://acme.com/oauth/authorize"
    token_url: "https://acme.com/oauth/token"
```

**Fields:**
- `endpoint` — well-known provider name (currently `"google"`), or omit for custom endpoints
- `vault_key` — vault key for credential storage; defaults to the service ID. Use a shared key (e.g. `"google"`) when multiple services share the same OAuth app
- `scopes` — requested OAuth scopes
- `scope_merge` — if true, new scopes are merged with the existing credential rather than replacing it
- `conditional_scopes` — scopes conditionally included based on environment variables
  - `scope` — the OAuth scope string
  - `env_gate` — environment variable name to check
  - `default` — whether to include the scope when the env var is unset
- `client_id` / `client_id_env` — client ID (inline or via env var)
- `client_secret` / `client_secret_env` — client secret (inline or via env var)
- `authorize_url` — OAuth2 authorization endpoint
- `token_url` — OAuth2 token endpoint

### No Auth

For public APIs that don't require authentication.

```yaml
auth:
  type: none
```

`extra_headers` still works with `type: none` for APIs that need non-auth headers.

## Actions

Each action maps to a single API operation.

### REST Actions

```yaml
actions:
  list_issues:
    display_name: "List issues"
    risk:
      category: read
      sensitivity: low
      description: "List repository issues"
    method: GET
    path: "/repos/{{.owner}}/{{.repo}}/issues"
    params:
      owner: { type: string, required: true, location: path }
      repo: { type: string, required: true, location: path }
      state: { type: string, default: "open", location: query }
      max_results: { type: int, default: 30, max: 100, location: query }
    response:
      fields:
        - { name: number }
        - { name: title, sanitize: true }
        - { name: state }
        - { name: html_url, rename: url }
      summary: "{{len .Data}} issue(s)"
```

### GraphQL Actions

```yaml
actions:
  list_issues:
    display_name: "List issues"
    risk: { category: read, sensitivity: low, description: "List issues" }
    query: |
      query($filter: IssueFilter, $first: Int) {
        issues(filter: $filter, first: $first) {
          nodes { id title state { name } }
        }
      }
    params:
      team_id:
        type: string
        required: true
        filter_path: "team.id.eq"  # builds nested filter object
      first: { type: int, default: 50, graphql_var: true }
    response:
      data_path: "data.issues.nodes"
      fields:
        - { name: id }
        - { name: title }
        - { name: state, path: "state.name" }
      summary: "{{len .Data}} issue(s)"
```

## Parameters

| Field | Type | Description |
|-------|------|-------------|
| `type` | string | `"string"`, `"int"`, `"bool"`, `"object"`, `"array"` |
| `required` | bool | Parameter is mandatory |
| `default` | any | Static default value |
| `location` | string | `"query"`, `"body"`, or `"path"` |
| `map_to` | string | API-side parameter name if different (e.g. `max_results` → `maxResults`) |
| `min` / `max` | int | Constraints for int params |
| `transform` | string | Expr-lang expression to transform value before sending |
| `default_expr` | string | Expr-lang expression for dynamic default (e.g. `"rfc3339(now())"`) |

### GraphQL-Specific Parameter Fields

| Field | Type | Description |
|-------|------|-------------|
| `graphql_var` | bool | Pass as a top-level GraphQL variable |
| `filter_path` | string | Dot-delimited path to build a nested filter object (e.g. `"team.id.eq"`) |
| `input_field` | string | Maps param to a field in the `$input` mutation variable (e.g. `"teamId"`) |

### Parameter Location

- `path` — interpolated into the URL path (e.g. `/repos/{{.owner}}`)
- `query` — appended as URL query parameters
- `body` — included in the JSON request body (default for POST/PUT/PATCH)

Path parameters also support credential field interpolation via `{{.credential.field}}` (e.g. `{{.credential.user}}`). For basic auth, `user` and `pass` are available; for API key, `token`.

### Body Mode

For PATCH endpoints where you only want to send provided fields:

```yaml
  update_event:
    method: PATCH
    path: "/events/{{.event_id}}"
    body_mode: sparse  # only include params that were actually provided
```

### Encoding

For APIs that expect form-encoded bodies instead of JSON:

```yaml
  send_message:
    method: POST
    path: "/chat.postMessage"
    encoding: form  # default is "json"
```

## Response Handling

```yaml
response:
  data_path: "data.items"  # dot path to the data array/object in the response
  fields:
    - { name: id }
    - { name: title, sanitize: true }           # strip HTML entities
    - { name: state, path: "state.name" }        # nested access
    - { name: url, rename: link }                 # rename output key
    - { name: amount, transform: "money" }        # cents → "100.00"
    - name: start                                 # expr-lang expression
      expr: "start.dateTime ?? start.date ?? ''"
    - { name: location, optional: true }          # omit if nil
    - { name: notes, nullable: true }             # return "" if nil
  summary: "{{len .Data}} item(s)"
```

### Field Options

| Field | Description |
|-------|-------------|
| `name` | JSON key to extract |
| `path` | Dot-delimited nested path (e.g. `"channel.name"`) |
| `rename` | Output key name (defaults to `name`) |
| `sanitize` | Strip HTML entities and truncate |
| `transform` | Named transform: `"money"`, `"upper"`, `"sanitize"` |
| `expr` | Expr-lang expression (takes precedence over path/name) |
| `optional` | Omit field from output if expr returns nil |
| `nullable` | Return empty string if nil instead of erroring |

### Summary Templates

Go `text/template` syntax with access to:
- `{{len .Data}}` — array length
- `{{.fieldname}}` — field value from a single-object response (e.g. `"Created #{{.number}}: {{.title}}"`)

## Error Envelope Checking

For APIs like Slack that return HTTP 200 with errors in the response body:

```yaml
  list_channels:
    method: GET
    path: "/conversations.list"
    error_check:
      success_path: "ok"     # path to boolean success field
      error_path: "error"    # path to error message
```

## Risk Classification

Every action must have a risk assessment:

- **category**: What the action does
  - `read` — retrieves data without modification
  - `search` — searches or queries data
  - `write` — creates or modifies data
  - `delete` — removes or destroys data

- **sensitivity**: Blast radius if misused
  - `low` — read-only on non-sensitive data
  - `medium` — writes on standard data, or reads on sensitive data
  - `high` — destructive ops, sensitive data, large blast radius

## Complete Example

```yaml
service:
  id: acme
  display_name: Acme API
  description: "Manage widgets and orders in the Acme platform."
  setup_url: "https://acme.com/settings/api-keys"
  identity:
    endpoint: "/me"
    field: "email"

auth:
  type: api_key
  header: "Authorization"
  header_prefix: "Bearer "

api:
  base_url: "https://api.acme.com/v1"
  type: rest

actions:
  list_widgets:
    display_name: "List widgets"
    risk: { category: read, sensitivity: low, description: "List all widgets" }
    method: GET
    path: "/widgets"
    params:
      status: { type: string, default: "active", location: query }
      limit: { type: int, default: 25, max: 100, location: query }
    response:
      data_path: "data"
      fields:
        - { name: id }
        - { name: name }
        - { name: status }
        - { name: created_at }
      summary: "{{len .Data}} widget(s)"

  create_widget:
    display_name: "Create widget"
    risk: { category: write, sensitivity: medium, description: "Create a new widget" }
    method: POST
    path: "/widgets"
    params:
      name: { type: string, required: true, location: body }
      description: { type: string, location: body }
      tags: { type: array, location: body }
    response:
      fields:
        - { name: id }
        - { name: name }
      summary: "Created widget: {{.name}}"

  delete_widget:
    display_name: "Delete widget"
    risk: { category: delete, sensitivity: high, description: "Permanently delete a widget" }
    method: DELETE
    path: "/widgets/{{.widget_id}}"
    params:
      widget_id: { type: string, required: true, location: path }
    response:
      summary: "Widget deleted"
```
