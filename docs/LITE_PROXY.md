# Lite-Proxy

> [!WARNING]
> The lite-proxy is in active development. Behavior, flags, and the on-wire surface may change in any release while it remains pre-1.0. Treat it as preview-quality.

The lite-proxy is an LLM termination + tool-mediation surface that runs inside the Clawvisor daemon. Agents point `ANTHROPIC_BASE_URL` or `OPENAI_BASE_URL` at the daemon's HTTP port, authenticate with their existing `cvis_…` agent token, and Clawvisor:

- Swaps the agent token for the real upstream API key from the vault before forwarding to Anthropic / OpenAI.
- Inspects every tool_use in the response, rewrites credentialed calls through Clawvisor's resolver, and gates them on task scope.
- Holds tool_uses that need user approval, returning a synthesized prompt the user replies to inline (`approve` / `deny` / `task`).
- Substitutes vaulted credentials into the outbound HTTP call at proxy time so the agent never holds the real secret.

It is an alternative to the [runtime proxy](RUNTIME_PROXY.md) (CONNECT/TLS-MITM). They solve overlapping problems with different tradeoffs:

| | Runtime Proxy | Lite-Proxy |
|---|---|---|
| Transport | CONNECT proxy, agent traffic routed via `HTTP_PROXY` | HTTPS endpoints at `BASE_URL` |
| Setup | Agent trusts a generated CA | Agent points env var at daemon |
| Sees | Every outbound HTTPS request the agent makes | Only the model's tool-use blocks + outbound calls the model emits via curl/fetch tools |
| Approval surfaces | Dashboard | Dashboard + inline chat |
| Best for | Local agents, IDE plugins | API-only setups, hosted/proxy-only sessions |

The two can coexist. The lite-proxy is the focus of this document.

## Enable the proxy

In `config.yaml`:

```yaml
proxy_lite:
  enabled: true

  # Optional: override upstream hosts (e.g. for staging deployments).
  # anthropic_base_url: https://api.anthropic.com
  # openai_base_url:    https://api.openai.com

  # Hostnames this Clawvisor instance serves itself on. The resolver
  # refuses target hosts matching any of these so an agent can't read
  # its own audit log via its own placeholder.
  self_hostnames:
    - clawvisor.example.com

  # Default false. Flip on for self-host development where you need
  # RFC1918 / loopback destinations to be reachable through the
  # resolver.
  allow_private_networks: false

  # Optional. Path to a JSON-lines decision-trace log used for
  # debugging "why was this tool_use blocked?". See "Debugging" below.
  trace_log_path: /var/log/clawvisor/lite-proxy-trace.jsonl
```

The proxy is gated by `proxy_lite.enabled`. When on, the daemon exposes:

| Route | Purpose |
|---|---|
| `POST /v1/messages` | Anthropic Messages API (Claude Code, Anthropic SDK). |
| `POST /v1/messages/count_tokens` | Anthropic token counter. |
| `POST /v1/chat/completions` | OpenAI Chat Completions (Codex, OpenAI SDK). |
| `POST /v1/responses` | OpenAI Responses API. |
| `POST /proxy/v1/…` | The resolver — agents call here when their tool_use uses a vault placeholder. The daemon swaps it in and forwards to the real target host. |
| `GET /control/skill` | Documents the control plane to the agent. |
| `POST /control/tasks` | Task creation. Agents POST here to request scope before doing non-trivial work. |
| `GET /control/tasks/{id}` | Status lookup. |
| `POST /control/tasks/{id}/expand` | Add scope to an existing task. |

### Split hosted deployments

Hosted environments can run the dashboard/API and lite-proxy as separate
services from the same binary by setting `server.route_set`:

```yaml
# Main app: dashboard + user APIs, but no public /v1, /proxy/v1, or /control surface.
server:
  route_set: app
proxy_lite:
  enabled: true

# Dedicated proxy service: health + /v1, /proxy/v1, and /control only.
server:
  route_set: proxy_lite
proxy_lite:
  enabled: true
```

Equivalent environment variables:

```bash
CLAWVISOR_ROUTE_SET=proxy_lite
CLAWVISOR_PROXY_LITE_ENABLED=true
CLAWVISOR_PROXY_LITE_SELF_HOSTNAMES=app.example.com,llm-proxy.example.com
CLAWVISOR_PROXY_LITE_ALLOW_PRIVATE_NETWORKS=false
```

For multi-instance proxy deployments, configure `REDIS_URL`. Redis backs both
resolver caller nonces and inline approval holds, so an `approve` / `deny`
reply can release a tool call held by another instance.

Restart the daemon after editing config:

```bash
clawvisor restart
```

## Connect an agent

Every agent has a `cvis_…` bearer token issued at registration. The lite-proxy authenticates by that token; no other secret is sent on the wire by the agent.

The simplest connection is via the bundled wrappers:

```bash
# One-time: register the agent. Pick a memorable alias.
clawvisor agent register --name dev

# Run Claude Code through the lite-proxy:
clawvisor agent claude --agent dev -- --print "what is 2+2"

# Run Codex through the lite-proxy:
clawvisor agent codex --agent dev -- exec "say hi"

# Auto-detect harness from the command:
clawvisor agent lite-run --agent dev -- claude --print "ping"
```

The wrappers inject the right environment variables for each harness:

| Variable | Claude Code | Codex |
|---|---|---|
| Endpoint | `ANTHROPIC_BASE_URL=<daemon>` | `OPENAI_BASE_URL=<daemon>/v1` |
| Auth | `ANTHROPIC_AUTH_TOKEN=<cvis_…>` | `OPENAI_API_KEY=<cvis_…>` |
| Mask | `ANTHROPIC_API_KEY=` (empty) | Codex `-c model_provider=clawvisor` injected |

If you need the raw exports for a custom invocation:

```bash
clawvisor agent lite-env claude --agent dev
# Prints `export ANTHROPIC_BASE_URL=… ANTHROPIC_AUTH_TOKEN=… …`
```

## Vault placeholders (`autovault_…`)

Real third-party credentials never enter the agent's context. Clawvisor stores them in the vault and gives the agent opaque references called **placeholders** in the form `autovault_<service>_<random>` (or `autovault_<service>_<account>_<random>` for multi-account installs).

The placeholder is what you paste into agent prompts and tool_uses:

```bash
# Inside the agent harness:
curl -sS https://api.github.com/user \
  -H "Authorization: Bearer autovault_github_xyz123"
```

What happens at runtime:

1. The model emits a Bash tool_use containing the curl above.
2. The lite-proxy's response postprocess inspects every tool_use. Because the bash command parses cleanly as `curl + headers + URL`, the inspector classifies it as a credentialed API call to `api.github.com`.
3. The boundary check confirms `api.github.com` is in the placeholder's bound-service allowlist (GitHub maps to `api.github.com` + `uploads.github.com`).
4. The decision evaluator checks that the agent has an approved task covering this call (per the task model — see "Tasks" below).
5. If all checks pass, the tool_use is **rewritten** before the harness sees it. The harness runs:

   ```bash
   curl -sS https://daemon.example/proxy/v1/user \
     -H "Authorization: Bearer autovault_github_xyz123" \
     -H "X-Clawvisor-Target-Host: api.github.com" \
     -H "X-Clawvisor-Caller: Bearer cv-nonce-…"
   ```

6. The resolver at `/proxy/v1/…` looks up the placeholder, fetches the real PAT from the vault, swaps the `Authorization` header value, and proxies the call to `api.github.com`. The harness gets the GitHub response.

The agent never sees the real PAT. The user pastes the placeholder once; Clawvisor handles substitution per call.

### Creating placeholders

Two paths:

- **Dashboard.** Open the Services page → connect the service (vaults the real credential). Then the "Shadow Tokens" panel → pick an agent + service → "Mint token". Copy the `autovault_…` string.
- **API.** `POST /api/runtime/placeholders/mint` with `{"agent_id": "...", "service": "github"}`. Returns the placeholder.

The `secret_vault` feature flag controls the Shadow Tokens UI visibility. Lite-proxy alone is enough to surface it; you don't need to also enable the runtime proxy:

```yaml
features:
  secret_vault: true  # surface the Shadow Tokens panel in the UI
```

(Either `proxy_lite.enabled` or `runtime_proxy.enabled` makes the panel visible.)

## Tasks

A **task** is a user-approved scope: a purpose statement plus a list of tools the agent expects to use. Once approved, the agent can do anything within that scope without per-call prompts.

The model is instructed to POST a task definition to the control plane at the start of any non-trivial request:

```bash
curl -sS -X POST 'https://clawvisor.local/control/tasks?wait=true&timeout=120' \
  -H 'Content-Type: application/json' \
  --data '{
    "purpose": "Build a polished interactive landing page in /tmp/landing-b",
    "expected_tools_json": [
      {"tool_name": "Bash",  "why": "Create the directory and run helper commands"},
      {"tool_name": "Write", "why": "Create HTML / CSS / JS files"},
      {"tool_name": "Edit",  "why": "Iterate on the design"}
    ],
    "intent_verification_mode": "strict",
    "expires_in_seconds": 600
  }'
```

`https://clawvisor.local` is a synthetic hostname — Clawvisor rewrites the curl to point at the real daemon and injects a one-shot nonce; the agent never types the daemon URL or auth headers directly.

The user approves the task in the dashboard. Subsequent tool_uses within the declared scope (matching tool name + boundary check) run without further prompts.

### Tool-name aliases across harnesses

`expected_tools_json` accepts either harness-specific tool names or canonical class names:

| Class | Aliases |
|---|---|
| `shell` | `Bash`, `bash`, `shell`, `exec_command` |
| `read_file` | `Read`, `read_file` |
| `edit_file` | `Edit`, `NotebookEdit`, `apply_patch`, `edit_file` |
| `write_file` | `Write`, `write_file` |
| `web_fetch` | `WebFetch`, `fetch`, `http_request`, `web_fetch` |

A task created in a Claude Code session declaring `Bash` covers a Codex session's `exec_command`, and vice versa.

## What runs without a task

Some operations don't need scope at all:

- **Pure local reads.** `Read`, `Glob`, `Grep`, `BashOutput`, `ToolSearch`, Codex's `read_file`. They don't change state, don't transmit credentials.
- **Harness-internal lifecycle / planning.** `TodoWrite`, `Skill`, `Agent`, `EnterPlanMode`, `ExitPlanMode`, `EnterWorktree`, `ExitWorktree`, `ScheduleWakeup`, `KillShell`. These mutate harness state, not anything user-observable.
- **Read-only bash via AST classifier.** `pwd`, `ls`, `cat`, `head`, `tail`, `find`, `grep`, `wc`, `whoami`, `id`, `printf`, `echo`, and pipelines of these. Refused: `sed -i`, `find -exec`, command substitution (`$(…)`), subshells, write redirects (except `> /dev/null`), and anything that calls another interpreter.

These run silently. Anything else — writes (Edit, Write, NotebookEdit), network (WebFetch), mutating bash (`mkdir`, `rm`, `git commit`, `curl https://…`) — requires an approved task or an inline approval.

## Approvals

When a tool_use needs approval, the daemon **holds** it and synthesizes a prompt visible inline in the agent's chat:

```
Clawvisor paused this tool call for approval.

Tool: Bash
Reason: no matching task scope
Input: mkdir -p /tmp/landing

Reply approve to run this tool call, deny to block it,
or task to instruct the agent to include this in a task definition for approval.
```

Three replies:

- `approve` — release the held tool_use. Daemon rewrites and re-emits it; harness runs it; model gets the result.
- `deny` — refuse it. Model sees a synthetic refusal tool_result and stops.
- `task` — daemon tells the model to POST a task definition that covers this work. Once the task is approved, the original tool_use re-emits within the new scope.

The dashboard approval surface lives in parallel. Async agents and multi-agent control panels still use it. Single-user interactive sessions can stay entirely in chat.

## Audit + observability

Every inspected tool_use produces audit rows:

- `lite_proxy.endpoint_call` — one per `/v1/*` request, with provider / model / streaming flag / available tools.
- `lite_proxy.tool_use_inspected` — one per tool_use that the inspector saw. Records: tool name, decision (`allow` / `block` / `rewrite`), outcome, reason, host/method/path, placeholder (when applicable).
- `lite_proxy.resolver_swap` — one per `/proxy/v1/…` resolver call, with the placeholder, bound service, target host/path.

Credential patterns (`ghp_…`, `sk-ant-…`, `Bearer …`, etc.) are redacted before audit values are stored. Placeholders are kept verbatim — they're references, not secrets.

## Debugging

When something gets refused unexpectedly, enable the **decision-trace log** to see exactly which gate fired:

```yaml
proxy_lite:
  trace_log_path: /var/log/clawvisor/lite-proxy-trace.jsonl
```

Or per-session:

```bash
export CLAWVISOR_PROXY_LITE_TRACE=/tmp/lite-trace.jsonl
clawvisor restart
```

Each tool_use produces a sequence of JSON-line events. Useful query patterns:

```bash
# See the full decision chain for the last tool_use:
tail -100 ~/.clawvisor/logs/lite-proxy-trace.jsonl | jq -c '
  {ts: .timestamp[11:23], event, tool: .tool_name,
   src: .source, kind, reason: (.reason // "")[0:80]}'

# Filter on tool_use_id to follow one specific call:
jq -c 'select(.tool_use_id == "toolu_…")' < lite-proxy-trace.jsonl

# Find every block + reason:
jq -c 'select(.event == "decision" and .kind == "needs_approval")' < lite-proxy-trace.jsonl
```

Events you'll see (one tool_use → several lines):

| Event | When |
|---|---|
| `tool_use_entry` | Tool_use arrived; preview of input + whether `TriggerHits` fired. |
| `inspect_verdict` | Parser/validator produced a verdict (`source: deterministic/validator/trigger_miss`). |
| `boundary_check` | Per-placeholder bound-service lookup result. |
| `decision` | Decision evaluator verdict (`kind: allow/deny/needs_approval`). A second `decision` event with `source: local_only_pass_through` / `shell_poll_pass_through` / `readonly_shell_pass_through` indicates an override fired. |
| `control_rewrite` | A control-plane tool_use was rewritten to the synthetic URL. |
| `rewrite_applied` | A credentialed tool_use was rewritten through the resolver. |
| `nonce_mint` | Caller nonce minted for the rewrite. |

If you're not seeing the events you expect, confirm the daemon picked up the config by checking startup logs for `lite-proxy: decision trace enabled`.

## Common gotchas

- **Daemon port collision.** If `server.port` is taken (another dev server, Vite, etc.), the daemon will fall back to an ephemeral port and your registered agent's `server_url` goes stale. `clawvisor agent register --url http://localhost:<actual-port>` to refresh, or kill the squatter so the daemon can claim its configured port.
- **`features.secret_vault: false` hides Shadow Tokens.** Either flip it on, or enable `proxy_lite.enabled` (which surfaces the panel as a side effect).
- **Placeholder ownership.** A placeholder is bound to a specific `(user, agent, service)`. If you mint it under agent A but run agent B, the boundary check fails. The error message includes a redacted prefix so you can tell which placeholder was looked up.
- **Multi-account services.** Service IDs can include an account suffix (`github:work`, `github:home`). The normalizer strips it for the bound-service host lookup, but the placeholder must still belong to the agent that's calling.
- **Nonces in conversation history.** The harness re-sends prior tool_uses on every turn. The inbound sanitizer reverts rewritten URLs and strips `cv-nonce-…` / `X-Clawvisor-*` headers before the model sees them, so the model never learns the rewrite shape. If you see those substrings in the model's emitted tool_uses, file a bug.

## Related docs

- [Architecture](ARCHITECTURE.md) — the daemon's component diagram.
- [Runtime Proxy](RUNTIME_PROXY.md) — the CONNECT-based alternative.
- [Integration: Claude Code](INTEGRATE_CLAUDE_CODE.md) — Claude Code specifics.
- [Task examples](TASK_EXAMPLES.md) — `expected_tools_json` patterns.
