# Clawvisor Integration YAML Specification

Clawvisor integrations come in two formats:

1. **YAML adapters** (`*.yaml`) — declare a REST/GraphQL integration directly: auth, endpoints, params, response shape, risk classification. Hot-loaded from `~/.clawvisor/adapters/`. **This document, sections "Top-Level Structure" through "Complete Example", covers this format.**

2. **MCP adapters** (`*.mcp.yaml`) — declare that an integration is backed by a remote MCP (Model Context Protocol) server. Almost everything (actions, params, risk, auth endpoints, client credentials) is discovered at runtime; the YAML only needs the MCP endpoint and a few hooks. **See the "MCP Adapters" section at the end of this document.**

Prefer MCP adapters when the vendor ships a hosted MCP server (e.g. Notion, Supabase) — it's a few lines of YAML and you inherit dynamic tool discovery, OAuth client registration, and response sanitization for free. Use YAML adapters for direct REST/GraphQL integrations where no MCP server exists.

## Top-Level Structure

```yaml
service:
  id: <string>           # unique lowercase identifier (e.g. "jira", "stripe")
  display_name: <string> # human-readable name
  description: <string>  # one-line description
  setup_url: <string>          # optional: link to API key / OAuth app setup page
  key_hint: <string>           # optional: placeholder text for API key input (e.g. "Stripe secret key (sk_...)")
  key_display_name: <string>   # optional: label rendered above the API key input (e.g. "Connection string"). Defaults to no label.
  key_description: <string>    # optional: helper text rendered under the label. Newlines are preserved, so multi-line guidance is fine.
  icon_svg: <string>     # optional: inline SVG markup (good for small, self-contained glyphs)
  icon_url: <string>     # optional: URL to the icon (absolute or site-relative, e.g. "/logos/stripe.svg"). Prefer this for larger official brand assets already served as static files. If both are set, icon_url takes precedence.
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
  base_url: <string>  # e.g. "https://api.github.com" (supports {{.var.X}} interpolation)
  type: <"rest" | "graphql">

variables:           # optional: user-configurable variables collected at activation time
  <variable_name>:
    display_name: <string>
    description: <string>     # optional
    required: <bool>          # optional, default false
    default: <string>         # optional

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

For services where the username is a non-secret account identifier (e.g. Twilio's Account SID), use `user_var` to pull it from a variable instead of asking the user to paste `user:pass`. The vaulted credential is then just the password.

```yaml
auth:
  type: basic
  user_var: account_sid

variables:
  account_sid:
    display_name: "Account SID"
    required: true
```

`header` and `header_prefix` are ignored for basic auth.

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
    # Provider-specific overrides (for non-standard OAuth flows):
    scope_param: "user_scope"                       # override the authorize URL scope parameter name
    token_path: "authed_user.access_token"          # JSON path to access token in token response
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
- `scope_param` — override the query parameter name used for scopes in the authorize URL (default `"scope"`). Slack v2 OAuth requires `"user_scope"` for user token flows
- `token_path` — dot-delimited JSON path to extract the access token from the token response (default: top-level `"access_token"`). Slack v2 returns the user token at `"authed_user.access_token"`

### No Auth

For public APIs that don't require authentication.

```yaml
auth:
  type: none
```

`extra_headers` still works with `type: none` for APIs that need non-auth headers.

## Variables

Variables let each user provide instance-specific values (like a base URL) at activation time. They are stored in the database and interpolated into fields like `base_url` at runtime using `{{.var.<name>}}` placeholders.

```yaml
variables:
  instance_url:
    display_name: "Instance URL"
    description: "Your Atlassian Cloud URL (e.g. https://yourteam.atlassian.net)"
    required: true
  workspace_id:
    display_name: "Workspace ID"
    default: "default"
```

Reference variables in `base_url` (or any other field that supports interpolation):

```yaml
api:
  base_url: "{{.var.instance_url}}"
  type: rest
```

| Field | Type | Description |
|-------|------|-------------|
| `display_name` | string | Label shown to the user during activation |
| `description` | string | Optional help text explaining what to enter |
| `required` | bool | If true, activation fails without a value (unless `default` is set) |
| `default` | string | Pre-filled default value; satisfies `required` if the user leaves it blank |

Variables are collected in the TUI, daemon setup CLI, and web dashboard when the user connects the service. They are persisted per-user in a `service_configs` table, separate from credentials.

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
| `graphql_var` | bool | Pass as a top-level GraphQL variable. Combine with `map_to` to use a different variable name than the user-facing param (e.g. `issue_id` → `$id`). |
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
  meta:                                            # optional: extract top-level metadata (e.g. pagination)
    - { name: nextPageToken, rename: next_page_token }
    - { name: has_more }
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

### Response Metadata

The `meta` field extracts top-level response fields that live outside the `data_path` — typically pagination cursors or totals. These are returned in `result.meta` separately from `result.data`, so agents can discover how to fetch the next page.

```yaml
response:
  data_path: "channels"
  fields:
    - { name: id }
    - { name: name }
  meta:
    - { name: nextPageToken, rename: next_page_token }
    - { name: response_metadata.next_cursor, rename: next_cursor }  # dot paths supported
    - { name: has_more }
```

| Field | Description |
|-------|-------------|
| `name` | Field name in the raw API response (supports dot-delimited paths) |
| `rename` | Output key name in `result.meta` (defaults to `name`) |

Meta fields that are absent, null, or empty string in the response are silently omitted — `result.meta` is only present when at least one field has a value.

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
  key_hint: "Acme API key"
  identity:
    endpoint: "/me"
    field: "email"

auth:
  type: api_key
  header: "Authorization"
  header_prefix: "Bearer "

api:
  base_url: "{{.var.instance_url}}/v1"
  type: rest

variables:
  instance_url:
    display_name: "Instance URL"
    description: "Your Acme instance (e.g. https://mycompany.acme.com)"
    required: true

actions:
  list_widgets:
    display_name: "List widgets"
    risk: { category: read, sensitivity: low, description: "List all widgets" }
    method: GET
    path: "/widgets"
    params:
      status: { type: string, default: "active", location: query }
      limit: { type: int, default: 25, max: 100, location: query }
      cursor: { type: string, location: query }
    response:
      data_path: "data"
      fields:
        - { name: id }
        - { name: name }
        - { name: status }
        - { name: created_at }
      meta:
        - { name: next_cursor }
        - { name: has_more }
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

---

# MCP Adapters

An MCP adapter declares that a service is backed by a remote [Model Context Protocol](https://modelcontextprotocol.io) server. Clawvisor handles the protocol mechanics generically — the YAML spec only has to identify the server and (optionally) tell Clawvisor how to derive the user's identity.

MCP-adapter files live in `internal/adapters/definitions/mcp/` and use the `.mcp.yaml` suffix. They're embedded into the binary via `//go:embed *.mcp.yaml`, so adding a service requires a rebuild — there's no per-user hot-load path today.

## Top-Level Structure

```yaml
service:
  id: <string>           # canonical service identifier (e.g. "notion")
  display_name: <string>
  description: <string>
  setup_url: <string>    # optional
  icon_url: <string>     # optional
  icon_svg: <string>     # optional, inline SVG (mutually exclusive with icon_url)

mcp:
  transport: <"http" | "stdio">  # default: stdio
  endpoint: <string>             # MCP streamable-HTTP URL (http only)
  command: [<string>, ...]       # argv to launch the server (stdio only)
  credential_env: <string>       # env var the server reads for auth (stdio only)
  oauth: { ... }                 # optional, see "OAuth"
  whoami: { ... }                # optional, see "Whoami"
```

For HTTP transports (the production case for vendor-hosted MCP servers like `mcp.notion.com`), only `endpoint`, `oauth`, and `whoami` are needed. For stdio transports (local subprocess servers like `npx @notionhq/notion-mcp-server`), set `command` and `credential_env`.

## What Clawvisor handles automatically

Everything that a YAML adapter would have to declare explicitly is derived at runtime for MCP adapters:

| Concern | YAML adapter | MCP adapter |
| --- | --- | --- |
| Action list | hand-written `actions:` block | `tools/list` at activation; persisted to `mcp_tool_caches` |
| Param schemas | `params:` per action | tool's `inputSchema` JSON Schema |
| Risk classification | per-action `risk:` block | MCP `annotations` (`readOnlyHint`, `destructiveHint`) |
| HTTP / JSON-RPC framing | manual `path` + `method` + `body` mapping | handled by `mcpclient` (JSON + SSE) |
| Response sanitization | per-action `transforms:` | generic gateway middleware (HTML strip, truncation) |
| OAuth endpoints | declared inline | RFC 8414 discovery from the server |
| OAuth client provisioning | admin-pasted client_id/secret | RFC 7591 dynamic client registration, cached per deployment |
| Token refresh | manual via `pkce_flow` | `oauth2.TokenSource` wrapping the HTTP client |

The one piece you *do* configure per-service is identity resolution (whoami), and only because there's no MCP standard for it.

## OAuth

For MCP servers that require OAuth (most do), declare it with an empty mapping:

```yaml
mcp:
  transport: http
  endpoint: https://mcp.notion.com/mcp
  oauth: {}
```

On the first Connect click for a given service, the adapter:

1. Probes the MCP endpoint cold → reads `WWW-Authenticate: Bearer resource_metadata="..."`.
2. Fetches the protected-resource metadata (RFC 9728) → `authorization_servers`.
3. Fetches the AS metadata (`.well-known/oauth-authorization-server`, RFC 8414) → endpoints, supported grant types, supported auth methods, supported PKCE methods.
4. POSTs to the AS's `registration_endpoint` (RFC 7591) with Clawvisor's callback URL → receives `client_id` (+ optional `client_secret`).
5. Caches the registered client + discovered endpoints under `__system__/mcp.client.<serviceID>` in the vault.

All subsequent users on this Clawvisor deployment reuse that cached client. Discovery runs once per service per deployment.

### When dynamic registration isn't available

A few MCP servers don't implement RFC 7591. For those, an admin can pre-register a client (via the vendor's developer dashboard) and pin it through the settings UI at `/api/system/mcp-oauth`. The pinned credentials at `__system__/mcp.oauth.<serviceID>` take precedence over any cached dynamic-registration record.

You can also declare hardcoded authorize/token URLs in the spec as a fallback for servers that don't expose RFC 8414 metadata:

```yaml
mcp:
  transport: http
  endpoint: https://mcp.example.com/mcp
  oauth:
    authorize_url: https://example.com/oauth/authorize  # fallback, used if discovery fails
    token_url:     https://example.com/oauth/token
    scopes: [read, write]                               # optional, included in the authorize URL
```

These fields are escape hatches; the discovery path should cover almost every production MCP server.

## Whoami

Identity resolution determines the alias under which Clawvisor stores the user's credential (e.g. `notion:eric@clawvisor.com` instead of `notion:default`). MCP has no standard tool name for this, so the spec names a tool + describes how to extract the identifier from its response.

```yaml
mcp:
  whoami:
    tool: <string>           # MCP tool name to invoke (e.g. "notion-get-users")
    params: <map>            # optional, passed as `arguments` to the tool
    field: <string>          # dot-path into the JSON response with array indexing
```

The `field` path supports two array-indexing notations: `results[0].email` (JSONPath-style) and `results.0.email` (dot-number). Whoami results are then run through a normalization pass — whitespace becomes `-`, letters lowercase, characters outside the alias set (`a-z 0-9 _ - . @ +`) are dropped — so org names like `"Eric Levine's Org"` become `eric-levines-org`.

If `whoami` is omitted or the tool call fails, the credential is stored under the `default` alias.

## Examples

### Notion (OAuth, hosted MCP)

The MCP-driven Notion adapter ships with `id: notion-mcp` (display: `Notion`). The original REST-API adapter at `id: notion` is preserved as `Notion (Legacy)` for backward compatibility — new connections should use this MCP one.

```yaml
service:
  id: notion-mcp
  display_name: Notion
  description: "Notion workspace via MCP — read pages, query databases, create content."
  icon_url: "/logos/notion.svg"

mcp:
  transport: http
  endpoint: https://mcp.notion.com/mcp
  oauth: {}
  whoami:
    tool: notion-get-users
    params:
      user_id: self
    field: results[0].email
```

### Supabase (OAuth, hosted MCP, cross-domain AS)

```yaml
service:
  id: supabase
  display_name: Supabase
  description: "Supabase via MCP — query the database, manage projects, deploy edge functions."
  icon_url: "/logos/supabase.svg"

mcp:
  transport: http
  endpoint: https://mcp.supabase.com/mcp
  oauth: {}
  whoami:
    tool: list_organizations
    field: organizations[0].name
```

(Supabase's MCP advertises its authorization server on `api.supabase.com`, not `mcp.supabase.com`. Discovery handles that automatically via the protected-resource metadata.)

### Local stdio (no OAuth)

```yaml
service:
  id: local-thing
  display_name: "Local Thing"

mcp:
  transport: stdio
  command: ["npx", "-y", "@vendor/local-mcp-server"]
  credential_env: VENDOR_TOKEN   # the API key from vault is injected as this env var
```

## Trade-offs

What you give up by adopting MCP instead of a hand-written YAML adapter:

- **Schema authorship**: tool param schemas come from the vendor. If their schema overstates required-ness (e.g. Notion marks `parent` required when prose says it's optional for workspace-level pages), Clawvisor faithfully echoes that. The catalog can't second-guess the server.
- **Action stability**: when the vendor adds, removes, or renames tools, your users' restriction rules need to follow. The same risk exists for upstream API changes against a YAML adapter; MCP just makes the surface easier to query for diffs.
- **Trust boundary**: the MCP server is now part of the trust chain. Tool responses pass through the response-sanitization middleware before reaching the agent, but a compromised server can still return misleading-but-clean text. Pin server endpoints to vendor-controlled domains; don't proxy unknown MCP servers.

What you gain in exchange is large enough to be the default: zero per-service Go, automatic OAuth setup, dynamic tool discovery, consistent risk classification, and a sanitization layer that works the same for every server.
