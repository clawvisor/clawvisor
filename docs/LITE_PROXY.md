# Lite-Proxy

> [!WARNING]
> The lite-proxy is in active development. Behavior, flags, and the on-wire surface may change in any release while it remains pre-1.0. Treat it as preview-quality.

The lite-proxy is an LLM termination + tool-mediation surface that runs inside the Clawvisor daemon. Agents point `ANTHROPIC_BASE_URL` or `OPENAI_BASE_URL` at the daemon's HTTP port, authenticate with their existing `cvis_…` agent token, and Clawvisor:

- Swaps the agent token for the real upstream API key from the vault before forwarding to Anthropic / OpenAI.
- Inspects every tool_use in the response, rewrites credentialed calls through Clawvisor's resolver, and gates them on task scope.
- Holds tool_uses that need user approval, returning a synthesized prompt the user replies to inline (`y`/`yes` / `n`/`no` / `task`).
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

**Fresh installs enable the lite-proxy in the Observe posture by default** — the setup wizard recommends it and the per-harness install scripts route through it (skill-gateway-only is the explicit opt-out: choose it in the wizard, or pass `route=skill-only` to an install script). The switch is **writer-side**: the compiled default of `proxy_lite.enabled` stays `false`, so **an existing config that never set the key is never silently enabled at upgrade** — it keeps behaving exactly as before. All routes listed below, the dashboard panels, and the CLI helpers refuse to operate until `proxy_lite.enabled` is `true` (which fresh wizard/daemon configs now write explicitly).

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
| `POST /api/v1/messages` | Anthropic Messages API (Claude Code, Anthropic SDK). |
| `POST /api/v1/messages/count_tokens` | Anthropic token counter. |
| `POST /api/v1/chat/completions` | OpenAI Chat Completions (Codex, OpenAI SDK). |
| `POST /api/v1/responses` | OpenAI Responses API. |
| `POST /api/proxy/…` | The resolver — agents call here when their tool_use uses a vault placeholder. The daemon swaps it in and forwards to the real target host. |
| `GET /api/control/skill` | Documents the control plane to the agent. |
| `POST /api/control/tasks` | Task creation. Agents POST here to request scope before doing non-trivial work. |
| `GET /api/control/tasks/{id}` | Status lookup. |
| `POST /api/control/tasks/{id}/expand` | Add scope to an existing task. |
| `POST /api/control/tasks/{id}/complete` | Close out a task whose work is done. Releases scope and clears chain-fact context. Unilateral (no user approval); accepts `active` or `expired` tasks. |

Agents should be prompted to use the shorter synthetic URL
`https://clawvisor.local/control/...`. Proxy-lite rewrites that model-facing
path to the real daemon route under `/api/control/...`; harnesses and direct
HTTP clients should use the real `/api/control` routes.

### Split hosted deployments

Hosted environments can run the dashboard/API and lite-proxy as separate
services from the same binary by setting `server.route_set`:

```yaml
# Main app: dashboard + user APIs, but no public /api/v1, /api/proxy, or /api/control surface.
server:
  route_set: app
proxy_lite:
  enabled: true

# Dedicated proxy service: health + /api/v1, /api/proxy, and /api/control only.
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
clawvisor-server restart
```

## Connect an agent

Every agent has a `cvis_…` bearer token issued at registration. The lite-proxy authenticates by that token; no other secret is sent on the wire by the agent.

The simplest connection is via the bundled wrappers:

```bash
# One-time: register the agent. Pick a memorable alias.
clawvisor-server agent register dev
```

In an interactive terminal, registration also asks whether this agent should
forward to Anthropic or OpenAI, then stores that upstream API key in the
agent-scoped vault entry used by proxy-lite. For automation, pass
`--provider anthropic --api-key "$ANTHROPIC_API_KEY"` or
`--provider openai --api-key "$OPENAI_API_KEY"`; use `--skip-llm-key` to only
register the agent token.

```bash
# Run Claude Code through the lite-proxy:
clawvisor-server agent claude --agent dev -- --print "what is 2+2"

# Run Codex through the lite-proxy:
clawvisor-server agent codex --agent dev -- exec "say hi"

# Auto-detect harness from the command. This is the default `agent run` path.
clawvisor-server agent run --agent dev -- claude --print "ping"
```

The wrappers inject the right environment variables for each harness:

| Variable | Claude Code | Codex |
|---|---|---|
| Endpoint | `ANTHROPIC_BASE_URL=<daemon>/api` | `OPENAI_BASE_URL=<daemon>/api/v1` |
| Auth | `ANTHROPIC_CUSTOM_HEADERS="X-Clawvisor-Agent-Token: <cvis_…>"` | `X-Clawvisor-Agent-Token: <cvis_…>` via Codex provider config |
| Mask | `ANTHROPIC_API_KEY=` (empty) | Codex `-c model_provider=clawvisor` injected |

If you need the raw exports for a custom invocation:

```bash
clawvisor-server agent lite-env claude --agent dev
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
   curl -sS https://daemon.example/api/proxy/user \
     -H "Authorization: Bearer autovault_github_xyz123" \
     -H "X-Clawvisor-Target-Host: api.github.com" \
     -H "X-Clawvisor-Caller: Bearer cv-nonce-…"
   ```

6. The resolver at `/api/proxy/…` looks up the placeholder, fetches the real PAT from the vault, swaps the `Authorization` header value, and proxies the call to `api.github.com`. The harness gets the GitHub response.

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

The model is instructed to POST a task definition to the control plane before any tool call that is not on the read-only allowlist (writes, deletes, non-default CLIs, network calls, credential use, multi-step work):

```bash
curl -sS -X POST 'https://clawvisor.local/control/tasks?wait=true&timeout=120' \
  -H 'Content-Type: application/json' \
  --data '{
    "purpose": "Build a polished interactive landing page in /tmp/landing-b",
    "expected_tools": [
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

`expected_tools` accepts either harness-specific tool names or canonical class names:

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

Reply yes or y to run this tool call, no or n to block it,
or task to instruct the agent to include this in a task definition for approval.
```

Three replies:

- `y` / `yes` — release the held tool_use. Daemon rewrites and re-emits it; harness runs it; model gets the result.
- `n` / `no` — refuse it. Model sees a synthetic refusal tool_result and stops.
- `task` — daemon tells the model to POST a task definition that covers this work. Once the task is approved, the original tool_use re-emits within the new scope.

The dashboard approval surface lives in parallel. Async agents and multi-agent control panels still use it. Single-user interactive sessions can stay entirely in chat.

## Audit + observability

Every inspected tool_use produces audit rows:

- `lite_proxy.endpoint_call` — one per `/api/v1/*` request, with provider / model / streaming flag / available tools.
- `lite_proxy.tool_use_inspected` — one per tool_use that the inspector saw. Records: tool name, decision (`allow` / `block` / `rewrite`), outcome, reason, host/method/path, placeholder (when applicable).
- `lite_proxy.resolver_swap` — one per `/api/proxy/…` resolver call, with the placeholder, bound service, target host/path.

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
clawvisor-server restart
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

- **Daemon port collision.** If `server.port` is taken (another dev server, Vite, etc.), the daemon will fall back to an ephemeral port and your registered agent's `server_url` goes stale. `clawvisor-server agent register --url http://localhost:<actual-port>` to refresh, or kill the squatter so the daemon can claim its configured port.
- **`features.secret_vault: false` hides Shadow Tokens.** Either flip it on, or enable `proxy_lite.enabled` (which surfaces the panel as a side effect).
- **Placeholder ownership.** A placeholder is bound to a specific `(user, agent, service)`. If you mint it under agent A but run agent B, the boundary check fails. The error message includes a redacted prefix so you can tell which placeholder was looked up.
- **Multi-account services.** Service IDs can include an account suffix (`github:work`, `github:home`). The normalizer strips it for the bound-service host lookup, but the placeholder must still belong to the agent that's calling.
- **Nonces in conversation history.** The harness re-sends prior tool_uses on every turn. The inbound sanitizer reverts rewritten URLs and strips `cv-nonce-…` / `X-Clawvisor-*` headers before the model sees them, so the model never learns the rewrite shape. If you see those substrings in the model's emitted tool_uses, file a bug.

## Security caveats

The lite-proxy is a defense-in-depth surface, not a panacea. Operators enabling it should understand its limits:

- **Placeholders are visible to the model.** The model sees `autovault_…` strings inside its own prior tool_uses (the rewrite happens *after* the model emits the call). An instruction-following adversary can be tricked into copying a placeholder into a non-credentialed location (a comment, a log line, a description field). The inbound sanitizer redacts `cv-nonce-…` and `X-Clawvisor-*` headers from history before the next turn, but it cannot scrub placeholders embedded in arbitrary text — those are meant to be there.
- **`allow_private_networks: false` is the default.** Flipping it on permits the resolver to dial RFC1918 / loopback. That re-introduces SSRF risk; it should only be used for self-host development where a private destination is the legitimate target.
- **Passthrough mode disables the proxy's safety properties for the configured agent.** When an operator enables a passthrough rule (`Agents → break glass`), inbound bodies are sent upstream without autovault placeholder redaction and outbound tool_uses are sent to the harness without inspector mediation. Use sparingly and audit the resulting traffic.
- **Adjudicator is a hint, not a gate.** The LLM-driven `SecretAdjudicator` decides whether an ambiguous candidate looks like a real credential. Prompt injection inside the candidate's surrounding context could in principle steer it; the adjudicator response is canary-verified to detect drift but a clever attacker can still produce verdicts that fail closed (no decision) rather than open (false positive). When in doubt, the proxy redacts.

## Related docs

- [Architecture](ARCHITECTURE.md) — the daemon's component diagram.
- [Runtime Proxy](RUNTIME_PROXY.md) — the CONNECT-based alternative.
- [Integration: Claude Code](INTEGRATE_CLAUDE_CODE.md) — Claude Code specifics.
- [Task examples](TASK_EXAMPLES.md) — `expected_tools` patterns.
