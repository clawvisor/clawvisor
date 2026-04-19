# Design: Clawvisor Network Proxy — Stage 2 (Credential Custodian)

**Status:** Draft for review
**Author:** Eric Levine (with Claude)
**Target audience:** Clawvisor contributors reviewing the direction before implementation.
**Prerequisites:** Stage 1 shipped ([design-proxy-stage1.md](./design-proxy-stage1.md)).
**Scope lock:** The agent holds zero real credentials. All outbound secrets flow through the proxy, injected from Clawvisor's vault. Adds multi-LLM-provider support (Anthropic + OpenAI initially) and proxy-only install for frameworks beyond OpenClaw. Hosted deployment becomes available in this stage.

---

## 1. Context

### 1.1 What Stage 1 gave us

Stage 1 put a sovereign TLS-intercepting proxy in the network path between the agent and the outside world. Every LLM request, every channel API call, every outbound action is observable and attributable. Transcripts are tamper-proof for auto-approval. This is already a meaningful product beyond Stage 0.

But the agent still holds its own credentials. The Anthropic API key sits in its config. The Telegram bot token is in environment variables. The GitHub PAT the agent uses for its own business lives on its filesystem. Stage 1 solved the *observation* problem but left the *custody* problem exactly where Stage 0 left it.

### 1.2 What Stage 2 adds

The agent becomes credential-free. Clawvisor's vault holds every outbound secret. The proxy retrieves the right credential at request time based on destination host + agent identity, injects it into the outgoing request, and forwards. The agent process's environment, config files, and memory contain *zero* third-party credentials — only a Clawvisor identity token.

This is the capstone that turns Clawvisor into a **credential custodian of last resort**. The threat model shifts meaningfully:

| Attack | Stage 1 | Stage 2 |
|---|---|---|
| Agent process compromised, creds stolen | Attacker steals Anthropic key + all third-party creds | Attacker steals only the Clawvisor identity token (which is revokable in one click) |
| Agent process exfiltrates API keys via HTTP | Attacker succeeds (keys in agent's memory) | Agent doesn't have any keys to exfil |
| Insider with filesystem access | Can read every service credential | Can read only Clawvisor identity tokens |
| Credential rotation | Requires config change + restart | One-click in dashboard, instant |
| Credential revocation | Requires config change + restart (and hoping the agent isn't using them right now) | One-click in dashboard, immediate |

The product narrative shifts from "Clawvisor sees everything your agent does" to "**Clawvisor holds everything your agent needs, and your agent owns nothing that can be stolen.**"

### 1.3 New trust assumption: hosted mode is a product-boundary change

Stage 2 introduces **hosted deployment** as a first-class option (see §6). This is not a neutral repackaging of the self-hosted product — it is a qualitatively different trust posture that deserves to be surfaced at the top of this spec, not buried in the deploy section.

- In self-hosted mode (Stage 1 and the self-hosted flavor of Stage 2), decrypted LLM traffic, decrypted channel traffic, and plaintext credentials during injection never leave the user's machine.
- In **hosted mode**, Clawvisor Cloud's proxy fleet sees every user's decrypted LLM traffic, sees every user's channel API content, holds every user's injected credentials in memory during the injection window, and stores every user's transcripts and metadata in Clawvisor Cloud's database (KMS-encrypted at rest, plaintext in the proxy at use).
- That is a substantially larger trust surface than any previous stage of the product. It is justifiable — it enables the prosumer mass market — but the user is making a meaningfully larger bet on Clawvisor the company than they made at Stage 0, 1, or self-hosted Stage 2.

**Reviewers of this spec should evaluate Stage 2 self-hosted and Stage 2 hosted as two distinct offerings with distinct trust stories**, sharing an implementation but not a product-boundary. The success criteria in §1.5 calls out both.

Separately, if hosted mode ships first or loudest in marketing, self-hosted users who conflate the two may feel misled. Stage 2 comms need to distinguish clearly, every time.

### 1.4 Non-goals

- **Policy enforcement.** Observe + inject. Do not block, rewrite, or modify request content. (Stage 3.)
- **All LLM providers.** Anthropic + OpenAI for Stage 2. Gemini, Bedrock, xAI, Ollama, Groq come later as trivial follow-ups (just more parsers).
- **All third-party services.** The GitHub PAT / Gmail OAuth story already exists in Stage 0 (Clawvisor vault, task-scoped). Stage 2 extends this to credentials the *agent itself* needs (LLM keys, Telegram bot tokens, database URLs) — not replacing the task-scoped auth flow for user-facing tools.
- **Enterprise multi-tenancy / BYOK.** Single-user-per-install assumption still holds.
- **Cross-host deployments** (self-hosted). Proxy and server still run on the same host.
- **WebSocket support, HTTP/2 support.** Same limitations as Stage 1.

### 1.5 Success criteria

Stage 2 ships when:

*Self-hosted Stage 2:*
1. A user can move their Anthropic API key from agent config into Clawvisor's vault via the dashboard, and the agent continues to operate with no code change beyond the env var being removed.
2. The same works for OpenAI API keys and Telegram bot tokens.
3. **Claude Code works with the proxy, no plugin required.** This validates that the proxy is a framework-agnostic integration surface. Auto-approval for Claude Code is gated on M7 (LLM-stream sessionization).
4. Stage 1 users can opt into Stage 2 per-credential — e.g., move Anthropic to the vault now, keep GitHub where it is until later. Progressive trust extends at credential granularity, not just per-bridge.

*Hosted Stage 2 (evaluated independently):*
5. A hosted Clawvisor deployment (Clawvisor Cloud) runs the proxy remotely; a user can route their local agent through it with a single config change. Prosumer install friction drops to "install a CA cert, set `HTTP_PROXY`, and go."
6. Hosted mode ships with a clear, user-facing "what Clawvisor Cloud sees and stores" statement (see §1.3) that is linked from the signup flow, not just in the docs. A reasonable first-time user who opts into hosted understands what changed in the trust posture without having to read the spec.
7. Retention, data residency, and incident-response commitments are documented and operationally delivered before general availability.

---

## 2. Architecture

### 2.1 Injection mechanism

The proxy gains a new per-request responsibility: before forwarding, look up the destination host, match it to an **injection rule**, fetch the current credential from the vault, and rewrite the outbound request with that credential.

Injection rules are declarative:

```yaml
# stored in the server, delivered to the proxy at config time
injection_rules:
  - match:
      host: "api.anthropic.com"
    inject:
      header: "x-api-key"
      credential_ref: "vault:anthropic"

  - match:
      host: "api.openai.com"
      path: "/v1/*"
    inject:
      header: "Authorization"
      template: "Bearer {{ credential }}"
      credential_ref: "vault:openai"

  - match:
      host: "api.telegram.org"
      path_template: "/bot{{ credential }}/*"
    inject:
      path_substitute: true
      credential_ref: "vault:telegram-bot"
```

Three injection styles:

1. **Header injection** — set or replace a request header (most common: `Authorization` or `x-api-key`).
2. **Path substitution** — for APIs like Telegram that embed the secret in the URL path.
3. **Query param injection** — for APIs that use `?api_key=…` (rare and bad practice, but supported for completeness).

The agent makes requests *without* the credential (or with a placeholder). The proxy recognizes the destination, injects the real value, and forwards. Upstream sees a valid request; the agent never held the secret.

### 2.2 Proxy ↔ vault IPC

Credentials live in the server's vault (unchanged from today — envelope-encrypted with KMS in cloud, local key in self-hosted). The proxy needs to look them up at request time.

New server endpoint: `POST /api/proxy/credential-lookup`

Request:
```json
{
  "agent_token_id": "cvis_abc123",
  "credential_ref": "vault:anthropic"
}
```

Response:
```json
{
  "credential": "sk-ant-api03-...",
  "ttl_seconds": 300
}
```

- Authenticated with `cvisproxy_…` token (same as Stage 1 ingest).
- Server checks: does `cvis_abc123` have permission to use `vault:anthropic`? (Per-agent credential ACL — see §3.)
- Server decrypts, returns plaintext with TTL.
- Proxy caches in memory for the TTL, then refetches on next use.

**TTL strategy.** Short enough that rotation propagates quickly, long enough that we're not KMS-hitting every request.

- Default: 300 seconds (5 min). Standard for credential-caching systems.
- Server can push invalidations via a streaming SSE endpoint (`GET /api/proxy/invalidations`) — proxy subscribes, evicts entries when the server tells it to.
- On vault rotation, server sends invalidation → proxy evicts → next request refetches → new credential used.
- On revocation, same path.

### 2.3 Cache discipline

Proxy keeps credentials in memory only. Never writes to disk. Uses:

- A bounded LRU (e.g., 10,000 entries — generous for any sensible deployment).
- Per-entry TTL with jitter (±10%) so refreshes don't stampede on rotation boundaries.
- Explicit eviction on invalidation.
- Secure wiping on eviction (overwrite the credential bytes before GC, to the extent Go allows).

No persistence, no snapshots, no leaking via crash dumps (Go panic output scrubs credential cache).

### 2.4 Credential scope + attribution

Each credential in the vault has an ACL: which agent identities can use it.

```
vault:anthropic   → usable by: cvis_abc123, cvis_def456
vault:openai      → usable by: cvis_abc123
vault:github-pat  → usable by: cvis_def456 (not shared)
```

Per-request, the proxy identifies the caller via `Proxy-Authorization` (same `cvis_…` token as Stage 1's `gateway_token`), looks up the injection rule for the destination, verifies the ACL, and either injects or rejects (`403 Forbidden` from the proxy).

**Attribution unit, made explicit.** The ACL check is only as fine-grained as the proxy's attribution, which per Stage 1 §2.1 / §3.3 is **per container / process / token**, not per logical agent within a shared process. ACLs therefore enforce at this granularity by default:

- A `cvis_…` token identifies one agent-process-worth of HTTP traffic.
- A credential marked `usable by: cvis_abc123` means "usable by the process that holds `cvis_abc123` as its Proxy-Authorization."
- If a user runs multiple logical agents in one container/process, they all share `cvis_abc123` and therefore all share the same credential visibility. We do not advertise stricter separation than that.

**Users who need per-logical-agent ACLs within a single container must either:**

1. **Deploy one agent per container** (cleanest; always supported).
2. **Use the OpenClaw plugin's per-agent HTTP-client injection** (Stage 2 opt-in; OpenClaw only). The plugin wraps the tool-HTTP-client factory and hands each logical agent its own client configured with a distinct `Proxy-Authorization`. This makes per-connection attribution reliable for any traffic that flows through plugin-mediated clients — tool calls, LLM calls made through the plugin's Anthropic wrapper, etc. It does *not* cover traffic made through HTTP libraries the plugin doesn't mediate (raw `fetch` calls from user-installed OpenClaw extensions, for example), which still appear under the container's shared token.

Option (2) is framework-cooperative and partial. It gives reliable attribution for the common paths in OpenClaw specifically; it does not generalize to Claude Code / Cursor / arbitrary Python agents. When enabled, per-agent ACLs become meaningful within an OpenClaw container — but only for the plugin-mediated traffic surface. Dashboard must make this scope visible; users can't be misled into thinking ACLs cover everything when they only cover what the plugin hooks.

**Heuristic attribution is not used for ACL enforcement.** System-prompt fingerprinting and similar content-based heuristics are useful for observability attribution (labeling `TurnEvent`s with a best-guess agent identity) but are not authoritative enough for a security check. The credential ACL path only accepts reliable attribution — Proxy-Authorization from an isolated process, or plugin-mediated per-connection auth.

**ACL management UX.** Managed in the dashboard with language that matches the attribution reality:

- For users on one-agent-per-container deployments: straightforward per-agent checkboxes.
- For OpenClaw users with the plugin's per-agent mode enabled: per-logical-agent checkboxes, plus a visible note about what the plugin can and can't see.
- For multi-agent-per-container deployments without plugin mediation: checkboxes are grouped at the container level, and the UI explains why finer-grained control isn't available in this deployment shape.

Default for Stage 2: all credentials of a bridge usable by all `cvis_…` tokens of that bridge. Users opt into tighter ACLs, with the UX above.

---

## 3. Data model changes

### 3.1 Vault extensions

Existing vault schema stores credentials by `(user_id, service_id)`. Stage 2 extends with:

- `credential_ref`: stable string identifier used by injection rules (`vault:anthropic`, `vault:telegram-bot`). Unique per user. Can map to multiple providers of the same type (e.g., two separate Anthropic orgs → `vault:anthropic-personal` and `vault:anthropic-work`).
- `usable_by_agents`: array of `cvis_…` agent token IDs, or `null` meaning "all agents of this bridge."
- `injection_metadata`: optional per-credential override for the default injection rule (most credentials use the default for their type).

### 3.2 New tables

**`injection_rules`** — global + per-user rules for which credentials to inject for which destinations.

```sql
CREATE TABLE injection_rules (
    id                 TEXT PRIMARY KEY,
    user_id            TEXT REFERENCES users(id),  -- NULL for built-in rules
    host_pattern       TEXT NOT NULL,
    path_pattern       TEXT,
    method             TEXT,
    inject_style       TEXT NOT NULL,              -- 'header' | 'path' | 'query'
    inject_target      TEXT NOT NULL,              -- header name, path template, query key
    inject_template    TEXT,                       -- for templated injection (e.g., "Bearer {{credential}}")
    credential_ref     TEXT NOT NULL,
    priority           INTEGER NOT NULL DEFAULT 100,
    enabled            BOOLEAN NOT NULL DEFAULT TRUE,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
```

Built-in rules (shipped with Clawvisor) cover common services: Anthropic, OpenAI, Gemini, Telegram, GitHub, Slack, Stripe, etc. User-level rules override for custom or less-common services.

**`credential_usage_log`** — audit trail of credential lookups (who asked for what, when).

```sql
CREATE TABLE credential_usage_log (
    id                    BIGSERIAL PRIMARY KEY,
    ts                    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    agent_token_id        TEXT NOT NULL,
    credential_ref        TEXT NOT NULL,
    destination_host      TEXT NOT NULL,
    destination_path      TEXT,
    decision              TEXT NOT NULL,           -- 'granted' | 'denied_acl' | 'denied_revoked'
    request_id            TEXT,                    -- correlates to transcript_events
    INDEX (agent_token_id, ts DESC),
    INDEX (credential_ref, ts DESC)
);
```

Every lookup logged. This is a feature: users want to see which agent used which credential when, especially for sensitive keys. Dashboard surfaces this as a "credential activity" view.

Retention: same per-bridge config as other audit data (default 90d for usage logs).

---

## 4. UX: moving credentials to the vault

### 4.1 Per-credential opt-in flow

From the dashboard, under each bridge:

```
┌─ Credentials ─────────────────────────────────────┐
│                                                    │
│  Anthropic API    [agent-held]  [Move to vault →] │
│  OpenAI API       [agent-held]  [Move to vault →] │
│  Telegram bot     [in vault]    [Active]          │
│  GitHub PAT       [not set]     [Add credential]  │
│                                                    │
└────────────────────────────────────────────────────┘
```

Clicking "Move to vault" opens a short flow:

1. User pastes their Anthropic key into a one-time-view field.
2. Dashboard encrypts and stores in vault.
3. Dashboard generates a setup snippet for the agent:
   - "Remove `ANTHROPIC_API_KEY=sk-ant-...` from your agent's env."
   - "Verify your agent is already routing through Clawvisor's proxy (Stage 1)."
   - "Restart your agent."
4. On next request, proxy sees the Anthropic traffic without an API key, consults injection rule, fetches from vault, injects, forwards.

The user's experience: enter the key once, never see it again, get rotation + revocation in the dashboard.

### 4.2 Migration from Stage 0 task-scoped credentials

Stage 0 already has vaulted credentials for user-facing tools (GitHub, Gmail, etc.). Those stay as they are — the agent requests them via Clawvisor SDK per-task, and they never live on the agent's filesystem (today's model works).

Stage 2 introduces credentials the **agent itself** uses as infrastructure (LLM keys, bot tokens, database URLs). These weren't previously in Clawvisor's scope because there was no way to inject them without agent-side cooperation. Now the proxy enables it.

Clear separation in the dashboard:

- **User services** (Stage 0+): GitHub, Gmail, etc. Task-scoped auth. Agent sees these briefly, per-task, via SDK.
- **Agent infrastructure** (Stage 2+): Anthropic, OpenAI, Telegram bot, DB URLs. Never seen by agent. Injected by proxy.

### 4.3 Agent-holds-cred fallback

Not every credential needs to be moved. Users may have legacy configs or services Clawvisor doesn't yet have injection rules for. Agent-held credentials continue to work through the proxy exactly as at Stage 1 — the proxy sees them in transit and doesn't touch them. There's no forced migration.

This is important for DX: Stage 2 is not "you must move everything." It's "for each credential, you can choose to."

---

## 5. Framework-agnostic install (no plugin)

### 5.1 Proxy as universal integration surface

Once credentials flow through the proxy and transcripts flow through the proxy, the only remaining job of a framework-specific plugin is **tool exposure** — adding Clawvisor-managed tools to the agent's tool menu.

For Claude Code, Cursor, Aider, and "any script that calls Anthropic's API," there is no tool menu. The agent just makes HTTP calls. They don't need Clawvisor's tool catalog; they work directly with their own providers. The proxy is the *only* integration surface they need.

Stage 2 ships a **proxy-only integration mode** that:

- Does not require a plugin.
- Does not require pair flow.
- Does not require framework cooperation.
- Uses just the CA cert + `HTTP_PROXY` env var.

### 5.2 The proxy-only pair flow

For framework-agnostic install, the user's experience:

1. Dashboard → "Add agent" → "Without plugin (Claude Code / Cursor / custom)."
2. Dashboard mints: a bridge (or reuses one), an agent identity token, a CA cert bundle, an `HTTP_PROXY` env var.
3. User runs Clawvisor's setup script: it installs the CA cert into their system trust store, sets env vars.
4. User starts Claude Code (or whatever). It now routes through the proxy.
5. Transcripts + traffic logs flow into Clawvisor. User sees activity in the dashboard immediately.

**No OpenClaw plugin involved.** No framework-specific code required.

The plugin continues to be the right answer for OpenClaw specifically, because it handles tool exposure, pair flow UX, and the webchat observability gap. But the plugin is now *one* integration path, not the only one.

### 5.3 What the proxy-only mode loses

Honest accounting:

- **Tool exposure.** Claude Code can't use Clawvisor-managed tools (GitHub via Clawvisor's credential vault, etc.) without its own integration. That's fine — Claude Code has its own tool model (MCP).
- **Webchat-style in-process channels.** N/A; the frameworks in scope here don't have in-process channels.
- **Auto-approval via user-message context.** Proxy-only frameworks (Claude Code, Cursor) have no channel API — the "user" is the developer talking to the LLM in the terminal. The only transcript signal available is the LLM stream, which requires the sessionization design in §5.5 before auto-approval can safely consume it. Until that is built and validated, **proxy-only auto-approval is not promised by Stage 2**; it becomes available only when both §5.5 is shipped and the deduplication has been measured to be reliable on real Claude Code / Cursor traffic.

Net result: proxy-only install at Stage 2 reliably delivers observability, credential injection, and full activity audit. Auto-approval for proxy-only frameworks is deferred until the LLM-stream sessionization design (§5.5) ships and is validated.

### 5.5 LLM-stream sessionization (load-bearing for proxy-only auto-approval)

Stage 1 intentionally deferred this — the LLM stream there is observability-only. Stage 2 makes it load-bearing for proxy-only frameworks (Claude Code, Cursor, raw Python agents) where the LLM stream is the *only* transcript signal. This section specifies how the proxy derives a deduplicated, ordered turn stream from stateless LLM API traffic.

#### 5.5.1 The problem

Every Anthropic / OpenAI chat request body contains the full `messages[]` array of prior conversation turns. Two requests in the same conversation look like:

```
request N:     messages=[user1, assistant1, user2]
response N:    assistant2
request N+1:   messages=[user1, assistant1, user2, assistant2, user3]
response N+1:  assistant3
```

Naive emission (one `TurnEvent` per entry in each `messages[]`) over-emits: `user1`, `assistant1`, `user2` all appear twice. No stable conversation identifier, no obvious "this is turn N+1 of the conversation that had turn N."

#### 5.5.2 Conversation keying

The proxy derives a **conversation key** from request content:

```
conversation_key = hash(agent_token_id, first_user_message_content, model, system_prompt_hash)
```

- `agent_token_id` — from `Proxy-Authorization`. Per §3.3, attribution unit is the process.
- `first_user_message_content` — the text of `messages[0]` where `messages[0].role == "user"`. For conversations with a system prompt, that's `messages[0]`; for those without, same.
- `model` — from the request (`model` field). Conversations that switch models mid-stream get re-keyed, which is accepted.
- `system_prompt_hash` — if present, included so two conversations with same first user message but different system prompts get distinct keys.

This key is stable across all requests in a conversation as long as the conversation prefix doesn't change (which is the normal case — prefixes grow, they don't mutate). It's unstable if an agent rewrites the conversation prefix (trims history, summarizes earlier turns, etc.). That re-keying is accepted; it just produces a new conversation from the proxy's point of view, which is a tolerable loss-of-linkage.

**Collision risk:** Two unrelated conversations with the exact same first user message + same model + same system prompt + same agent would collide. In practice this is rare (conversations start differently) but possible. Acceptable at Stage 2; revisit if incident data shows collisions matter.

#### 5.5.3 Delta emission algorithm

Per request the proxy handles:

1. Compute `conversation_key`.
2. Look up `last_seen_prefix` for this key in a short-lived per-proxy cache (say, 1-hour TTL).
3. Let `current_prefix = messages[...]` (excluding the just-received current user turn if the API puts it there — both Anthropic and OpenAI do).
4. Let `delta = longest_strict_suffix(current_prefix) not in last_seen_prefix`.
   Equivalently: starting from the end of `current_prefix`, walk backwards until a turn matches the end of `last_seen_prefix` by content; emit only turns after that match.
5. Emit `TurnEvent`s for each delta entry, in order.
6. Emit a `TurnEvent` for the response body once the response arrives (assistant reply, with SSE reassembly).
7. Update `last_seen_prefix` to `current_prefix + [response_assistant_turn]`.

**Matching by content:** Two turns match if their text (and tool_calls, if present) are byte-equal after whitespace normalization. LLM providers return consistent content for prior turns, so byte-equality is fine; fuzzy matching is unnecessary and introduces false positives.

**What this gets right:**

- First request for a conversation: no cache entry → delta = full `messages[]` → emit everything, start cache.
- Second request: delta = new turn(s) since last request → emit only new. No duplicates.
- Agent retries the same LLM request: delta = empty → no emission.
- Agent rewrites history: cache miss at matching step → re-key as a new conversation, emit full prefix. Acceptable.
- Network partition between proxy and LLM (request lost, retried): dedup via delta = empty.

**What this gets wrong (accepted limitations):**

- Two concurrent conversations with identical prefixes and the same agent: they merge into one conversation key. Rare, visible in dashboard as "two separate Telegram chats merged into one transcript." Fix: add more entropy to the key (e.g., a conversation-ID cookie the agent passes) — but that requires framework cooperation we said we wouldn't require.
- The proxy must remember `last_seen_prefix` across plugin/proxy restarts for the dedup to work across restarts. On proxy restart, the cache is cold → first request re-emits the full prefix → server dedups via `event_id` (each `TurnEvent` gets a deterministic `event_id` derived from `(conversation_key, turn_index, content_hash)`, so replays are identified server-side).

#### 5.5.4 `event_id` stability for LLM-stream turns

Replacing Stage 1's scheme (`oc_asst:{node_id}:{hash}`) with a proxy-native scheme that works across restarts:

```
event_id = hash(conversation_key, turn_index, role, content_hash)
```

- `turn_index` = position of this turn in the conversation (0-indexed). Stable across requests in the same conversation.
- `role` + `content_hash` = guards against the unlikely turn-index collision.

Two calls to the same LLM with the same conversation history will produce the same `event_id` for each replayed turn → server dedups on ingest → no duplicates land.

#### 5.5.5 Validation plan

Before Stage 2 GA promises proxy-only auto-approval:

1. **Shadow mode in Stage 1+.** The proxy runs the sessionization algorithm against real Anthropic traffic from OpenClaw users, emits LLM-stream events with `experimental_sessionization: true`. Results stored but not consumed by auto-approval.
2. **Metrics collection.** Per-conversation: emitted turn count, duplicate count, dropped count, re-keying count. Ground truth from OpenClaw's session JSONL (the plugin still sees it) gives a comparison baseline.
3. **Accuracy target for GA.** 99%+ of conversations produce a turn stream indistinguishable from the JSONL-ground-truth transcript. If we fall short, iterate on the algorithm (possibly adding semi-fuzzy matching for edge cases) or defer proxy-only auto-approval further.
4. **Cut-over.** Once the algorithm clears the accuracy bar, Stage 2 proxy-only frameworks can consume the LLM stream for auto-approval.

### 5.6 MCP bridge (optional follow-on)

For Claude Code and any MCP-compliant agent, Clawvisor can optionally expose its managed services as an MCP server. The user adds `clawvisor-mcp://...` to their Claude Code MCP config; Clawvisor serves tool definitions + handles tool calls.

This lets Claude Code users get Clawvisor-managed tools (GitHub, Gmail) on top of the observability + injection layer. Strictly optional — proxy-only works fine without it.

Whether this ships in Stage 2 or as a follow-on: TBD. Probably follow-on unless it's trivial, because the proxy architecture is the priority.

---

## 6. Hosted deployment (Clawvisor Cloud)

### 6.1 Why hosted is a Stage 2 deliverable

Stage 1 is self-hosted-only because the value (tamper-proof transcripts) is easiest to explain locally and the trust tradeoff is minimal. Stage 2 is where hosted makes sense because:

- The "one-install" UX becomes possible: user installs nothing locally except a CA cert; everything else is in the cloud.
- The credential-holding story is already "trust Clawvisor with your secrets" — in hosted mode, that trust extends one level (Clawvisor Cloud holds decrypted credentials briefly during injection; Clawvisor self-hosted does too, just on your machine).
- Prosumer adoption goes up 10x when install is a single curl command and a config env var.

### 6.2 Hosted architecture

```
User's machine                        Clawvisor Cloud
┌─────────────────────┐              ┌──────────────────────┐
│  agent container    │              │  Clawvisor cloud      │
│  HTTP_PROXY=cloud   │─────TLS─────▶│    ├── proxy fleet   │
│  CA: Clawvisor Cloud│              │    ├── vault (KMS)   │
└─────────────────────┘              │    ├── server         │
                                      │    └── dashboard      │
                                      └──────────────────────┘
```

The proxy runs in Clawvisor's infrastructure. The user's agent routes all outbound HTTPS through `proxy.clawvisor.com` (or similar). Clawvisor decrypts, injects, forwards, stores transcripts.

**Trust commitment:** Clawvisor Cloud sees decrypted LLM API traffic, decrypted channel API traffic, and holds customer credentials under KMS. Customers explicitly opt into this.

### 6.3 Self-hosted vs hosted as deployments of the same product

Same binary, different deployment:

| | Self-hosted | Hosted |
|---|---|---|
| Proxy runs | User's machine | Clawvisor Cloud |
| Vault | Local SQLite | KMS-backed Postgres (Clawvisor Cloud) |
| CA cert | User-trust | Clawvisor Cloud-issued |
| Transcripts | Local | Clawvisor Cloud |
| Who sees plaintext | Nobody but user | Clawvisor (brief, during injection) |
| Install steps | Two containers, CA install | One CA install, one env var |
| Price | Free (OSS) | Subscription / usage-based |

The proxy binary doesn't know which mode it's running in. The server binary may have slightly different config in cloud mode (KMS-only vault, no local-file mode) but is the same codebase.

### 6.4 Cloud-specific concerns

- **Latency.** Adding a network hop between user and Anthropic adds 20-200ms per LLM request. Regional proxy deployments mitigate this but can't eliminate it. Document clearly.
- **Availability.** Clawvisor Cloud outage = user's agent stops working. Offer a "fail-open" degraded mode (proxy routes through without injection, agent fills in keys from env if present). Not ideal, but survives outages.
- **Data residency.** EU users probably want EU-only proxies + storage. US users don't care (but later will).
- **Abuse prevention.** Clawvisor becomes an open proxy if not authenticated. Strong auth + rate limiting + abuse detection from day one.

These are real engineering problems but not architectural blockers — standard "run a SaaS" concerns.

---

## 7. Multi-provider LLM support

### 7.1 Why Stage 2, not Stage 1

Stage 1 scope-locked to Anthropic to prove the architecture. Stage 2 is when "works with my LLM provider" becomes a real friction for adoption. Ship the top two at Stage 2, defer the long tail.

### 7.2 Parser additions

**OpenAI Chat Completions + Responses API.**

- `POST /v1/chat/completions` — standard. Streaming + non-streaming.
- `POST /v1/responses` — newer API, similar structure.
- Tool call format differs from Anthropic (OpenAI uses `tool_calls` array with `function` objects).
- Parser lives in `internal/proxy/parsers/openai.go`. Similar shape to `anthropic.go`.

**Emit the same `TurnEvent` schema.** The internal event model is provider-agnostic; parsers normalize.

### 7.3 Provider detection

Based on destination host:

- `api.anthropic.com` → Anthropic parser.
- `api.openai.com` → OpenAI parser.
- `generativelanguage.googleapis.com` → Gemini parser (future).
- Anything else (`host.docker.internal`, localhost services, unknown cloud APIs) → raw HTTP logging only, no parsing.

User can add custom rules in config for self-hosted LLMs (Ollama, LocalAI, etc.) — a stretch goal.

### 7.4 What breaks with multi-provider

Nothing, by design. The `TurnEvent` schema is provider-agnostic. Auto-approval consumes parsed turns; it doesn't care whether they came from Anthropic or OpenAI. Dashboard shows the provider as metadata.

The one edge case: a user with an agent that uses multiple LLMs (rare but possible — "main" uses Anthropic, "summarizer" uses OpenAI). Both work simultaneously; attribution is per-request.

---

## 8. Plugin deprecation path

The plugin is *not* deprecated in Stage 2. It remains the correct integration for OpenClaw because:

- It handles tool exposure (Clawvisor's managed services appear in OpenClaw's tool menu).
- It handles the pair-flow UX (dashboard → pair code → plugin).
- It handles the webchat observability gap (in-container WebSocket that the MITM doesn't see).

**What gets removed from the plugin at Stage 2:**

- Nothing beyond what Stage 1 already disabled (scavenger is already off when proxy is active).

**What gets added to the plugin at Stage 2:**

- Optional: "credential-free mode" — checks at startup that env vars for known services (Anthropic, OpenAI) are *not* set, warns if they are (reminding the user they can be moved to the vault).

### 8.1 Longer-term: plugin as optional

By Stage 2's end, the plugin is valuable but not *required* for OpenClaw users. An OpenClaw user who doesn't need tool exposure + is fine with webchat not being observed can skip the plugin entirely and just use the proxy.

That's a deliberate architectural position: Clawvisor becomes framework-independent at Stage 2. Plugins become enhancements, not prerequisites.

---

## 9. Security considerations

### 9.1 Threat: compromised proxy

Worst case: attacker gets code execution in the proxy. Sees all decrypted traffic, reads the credential cache, potentially lifts active credentials.

Mitigations:

- Proxy process has minimal dependencies. Audit regularly.
- Credential cache TTL is short (5 min) and bounded — blast radius is the credentials active in the last 5 minutes.
- Credentials are marked in the cache with their `credential_ref` so revocation via server invalidation is fast.
- No credential-to-disk leakage: memory-only cache, explicit wipe on eviction, core dumps disabled on proxy binary (`ulimit -c 0`).
- Proxy runs as unprivileged user (not root, even in container).
- Release binaries signed + published with checksums; image pulls verify digests.

### 9.2 Threat: compromised server

Worst case: attacker gets code execution in the Clawvisor server. Can decrypt vault contents (the server has the vault keys; in hosted mode, KMS access).

Mitigations:

- Standard server security hygiene. This is the same threat as any password manager's backend.
- In hosted mode, KMS access logs show decryption events — suspicious patterns alert.
- Per-credential ACLs mean a compromised server can decrypt anything, but a compromised *ACL-less* access path shouldn't exist.
- BYOK (Stage 4+) is the enterprise answer to "I don't even trust Clawvisor's KMS access." Not in scope here.

### 9.3 Threat: proxy-to-server IPC compromise

If an attacker gets on the proxy-to-server network path (e.g., a co-tenant on the host in hosted mode), they could MITM the credential lookup.

Mitigations:

- Self-hosted: localhost-only IPC, bearer token auth. Low risk if host is uncompromised.
- Hosted: mTLS between proxy fleet and server. Certificate rotation. VPC-internal traffic, never touches public internet.

### 9.4 Threat: credential exfiltration via request manipulation

Could a compromised agent trick the proxy into injecting a credential into a request going to an attacker-controlled server? For example, making a request to `http://evil.com/anthropic-stealer` and hoping the proxy injects the Anthropic key?

Mitigations:

- Injection rules are host-specific. A request to `evil.com` doesn't match `api.anthropic.com`. No injection.
- Rules cannot wildcard-match against arbitrary hosts. Built-in rules list specific domains.
- User-defined rules exist but dashboard warns prominently when rules cover hosts the user doesn't own.

### 9.5 Threat: agent making requests to legitimate hosts with forged injection intent

Agent makes `GET /api/v1/foo` on `api.anthropic.com` with path `/api/v1/foo/exfiltrate?data=sensitive`. Proxy injects the key. Anthropic rejects the unrecognized endpoint, but the request was made *through us*, so we logged it. Auto-approval flags? Dashboard shows? Audit surface catches?

This isn't really a "credential exfil" — the API key was only used for a real API that rejected the request. But it's a reminder that injection doesn't restrict *what* the agent asks of upstream, only *that* the auth is applied by us. Stage 3's policy enforcement closes this.

---

## 10. Retention (Stage 2 additions)

New data class:

| Data | Default retention | Configurable range |
|---|---|---|
| `credential_usage_log` | 90 days | 30–365 days |

Credential lookups are logged but only metadata (what, when, who). The actual credential value is never logged, anywhere.

---

## 11. Open questions

1. **Per-request vs per-session credential caching.** Current design caches per-`credential_ref` with TTL. Alternative: cache per-`(credential_ref, agent_token_id)`. Latter is slightly more flexible for per-agent invalidation; former is simpler. Default to the simpler one.

2. **Injection rules — where do they live?** Built-in rules compiled into the binary (easy to update but requires redeploy) vs. fetched from server on startup (more flexible but adds a dependency). Probably both: built-ins compiled, user rules from server.

3. **"Agent-held credential" grace period.** If a user moves their Anthropic key to the vault, the agent's env var might still be set briefly. Do we want the proxy to strip agent-provided credentials when an injection rule exists for that destination? (i.e., "if Clawvisor is authoritative for this, override whatever the agent sent"?) That's safer but surprising if debugging.

4. **Rate limiting credential lookups.** A malicious proxy or bug could spam `credential-lookup`. Server should rate-limit per `cvisproxy_` token. Threshold needs tuning.

5. **What happens when the vault is unreachable?** Proxy cache exists for TTL, then expires. Next request finds no cached cred, fetches, fails, request dies. Should we fail closed (block) or fail open (forward without injection, hoping agent has the key)? **Fail closed is correct for Stage 2.** Log prominently.

6. **Multi-hop requests.** If the agent's request goes through a redirect chain, injection applies only to the initial request. If the redirect target is a credential-injectable host, do we inject on the redirect? (Unusual but possible.) Likely: yes, with a security note in the dashboard about redirect-based requests.

7. **Claude Code's native proxy support.** Claude Code respects `HTTPS_PROXY` natively. Does it handle `Proxy-Authorization`? Does it handle a custom CA? If not, user has to set `NODE_EXTRA_CA_CERTS` or similar. Worth testing early and documenting platform-specifically.

8. **Mobile / browser-based agents.** Out of scope for Stage 2. But: how does this story eventually work if the "agent" is a ChatGPT iOS app calling Anthropic? It doesn't — those don't route through a proxy. Note as a natural limit.

9. **OpenClaw per-agent HTTP-client injection scope.** The plugin enhancement in §2.4 gives per-agent attribution for plugin-mediated clients only. What fraction of a realistic OpenClaw deployment's outbound traffic actually goes through plugin-mediated clients vs. user-installed extensions that use their own raw `fetch`? If the uncovered surface is non-trivial, the dashboard needs to be even more explicit about which traffic is attributed per-agent vs. per-container. Worth measuring during M6 shadow-mode.

10. **Heuristic attribution for observability `TurnEvent`s.** Stage 2 explicitly rules out heuristic attribution for ACL enforcement (§2.4). But heuristics (system-prompt fingerprinting, timing correlation) could still be useful to *label* `TurnEvent`s with a best-guess agent id for dashboard display, clearly marked as heuristic. Whether to ship this in Stage 2 or defer to Stage 3 — TBD. Value is mainly nicer dashboard UX for multi-agent-per-container deployments, not a security property.

---

## 12. Milestones

### M1: Credential injection mechanism (2 weeks)

- Injection rule schema, engine, unit tests.
- `credential-lookup` server endpoint.
- Vault-to-proxy IPC + memory cache with TTL.
- End-to-end test: proxy injects Anthropic key into outgoing request, request succeeds.
- No UX yet.

### M2: UX for moving credentials (1-2 weeks)

- Dashboard "Credentials" view for bridges.
- "Move to vault" flow per credential.
- Per-credential ACL UI.
- `credential_usage_log` + dashboard view.

### M3: OpenAI parser + second provider (1 week)

- `internal/proxy/parsers/openai.go`.
- OpenAI-specific injection rules (built-in).
- End-to-end test: user with OpenAI-powered agent moves key to vault, agent continues working.

### M4: Proxy-only install for framework-agnostic (2 weeks)

- Dashboard flow for "add agent without plugin."
- Setup script (CA cert install + env vars — leverages the Clawvisor Installer introduced in Stage 1 M4).
- Validated against: Claude Code, Cursor, a raw Python `anthropic` script.
- Documentation for each.
- **Observability + credential injection only at this milestone.** Auto-approval for proxy-only frameworks is blocked on M7 (sessionization validation).

### M7: LLM-stream sessionization (3 weeks)

- Implement the algorithm in §5.5: conversation keying, delta emission, stable `event_id`s.
- Ship as `experimental_sessionization: true` in proxy config — off by default.
- Run shadow mode for 2+ weeks against real OpenClaw Anthropic traffic; compare against ground-truth JSONL transcripts.
- Metrics: duplicate rate, dropped turn rate, re-keying rate, collision rate.
- Threshold for GA: 99%+ accuracy vs ground truth. If we fall short, iterate.
- On GA, turn on by default for proxy-only auto-approval.

Adds ~3 weeks to the Stage 2 timeline but removes a load-bearing unknown from the architecture.

### M5: Hosted proxy (3-4 weeks)

- Clawvisor Cloud proxy fleet deployment.
- User-facing setup: single config change to use cloud proxy.
- Trust + data story published (what Clawvisor Cloud sees, retention, etc.).
- Rate limiting, abuse prevention, multi-region.

### M6: Polish + release (1-2 weeks)

- Revocation invalidation (SSE-based).
- Failure mode hardening (vault down, proxy-server partition).
- Documentation updates.
- Soak testing.

### M8: OpenClaw per-agent HTTP-client injection (optional, 2 weeks)

Opt-in enhancement for users who run multiple logical agents inside a single OpenClaw container and need per-agent credential ACLs.

- Plugin wraps OpenClaw's tool-HTTP-client factory; each logical agent gets a client with its own `Proxy-Authorization`.
- Per-connection attribution validated end-to-end — proxy sees distinct tokens, ACL enforcement becomes meaningful at the logical-agent level.
- Dashboard UX: "plugin mediates attribution" indicator on the bridge page, showing what percentage of recent traffic was plugin-mediated vs. container-shared.
- Explicitly scoped: works for plugin-mediated traffic only. User-installed OpenClaw extensions that use raw `fetch` are not covered.
- Deferred from the critical path because the default (one-agent-per-container) is sufficient for the prosumer ICP.

Total: 13-16 weeks of focused work with M7 included (M8 optional, adds 2 weeks if pursued). Hosted proxy is the heaviest chunk (M5); sessionization (M7) is the next heaviest and is mandatory for proxy-only auto-approval. If you want to ship self-hosted Stage 2 separately from hosted, that's 10-12 weeks for the self-hosted + sessionization piece only. Skipping M7 drops to 7-9 weeks but leaves Claude Code / Cursor with observability + injection but no auto-approval.

---

## 13. What's left for Stage 3

Stage 3 is about moving from *observation + custody* to *enforcement*:

- Policy engine (block/allow/flag based on destination, path, method, LLM content).
- LLM judge for contextual decisions.
- Ban mechanism for repeated violations.
- Policy authoring UX in dashboard.
- `kumo summarize`-style observe-to-policy generation.
- Integration with auto-approval (unified approval + enforcement model).

Stage 3 is where the "bounded blast radius" property emerges and where Clawvisor can plausibly sell to enterprises concerned about agent misbehavior.

---

## 14. Explicit rejections

- **Forced credential migration.** "You can't use Clawvisor Stage 2 unless you move all your keys to the vault" — no. Per-credential opt-in keeps the prosumer friction low.
- **Embedding Clawvisor auth directly into each LLM SDK.** We're not shipping patches to the Anthropic SDK. Proxy intercept is cleaner.
- **Storing credentials in the agent container's ephemeral secret store (e.g., Docker secrets).** Defeats the point — agent still sees the credential. Vault-held + injected is the invariant.
- **Injection for protocols other than HTTP.** gRPC, TCP-level protocols (Postgres, Redis, etc.) are all in scope for "the agent shouldn't hold DB credentials either" but require per-protocol proxy support. Defer. Maybe Stage 4.
- **Provisioning long-lived agent-side certificates for TLS auth to specific services.** Services that authenticate clients via mTLS (some enterprise APIs) are not in scope for Stage 2. Defer.

---

## Appendix A: Example injection rule evaluation

Agent makes request: `POST https://api.anthropic.com/v1/messages` with empty `x-api-key` header.

Proxy sees request, looks up rules:

```yaml
- match:
    host: "api.anthropic.com"
  inject:
    header: "x-api-key"
    credential_ref: "vault:anthropic"
```

Match. Proxy:
1. Authenticates agent via `Proxy-Authorization` → `cvis_abc123`.
2. Calls server: `POST /api/proxy/credential-lookup { agent_token_id: "cvis_abc123", credential_ref: "vault:anthropic" }`.
3. Server checks ACL: `vault:anthropic` usable by `cvis_abc123` ✓.
4. Server returns: `{credential: "sk-ant-api03-...", ttl: 300}`.
5. Proxy caches, injects header, forwards to Anthropic.
6. Server logs the lookup in `credential_usage_log`.

Response comes back normally. Agent sees a normal response. Agent never saw the key.

---

## Appendix B: Trust posture change summary

| | Stage 0 | Stage 1 | Stage 2 |
|---|---|---|---|
| Third-party service creds (GitHub, Gmail) | Vault (task-scoped via SDK) | Vault (unchanged) | Vault (unchanged) |
| LLM API keys | Agent-held | Agent-held | Vault, injected |
| Channel tokens (Telegram bot) | Agent-held | Agent-held | Vault, injected |
| Transcripts (channel stream) | Plugin scavenger (tamperable) | Proxy (tamper-proof) | Proxy (unchanged) |
| Transcripts (LLM stream) | Plugin scavenger | Proxy (observability only, duplicates accepted) | Proxy (deduplicated via M7 sessionization) |
| Auto-approval source | Plugin-sourced | Channel stream (proxy) | Channel stream + LLM stream post-M7 |
| Attribution unit | Per agent (plugin knows) | Per container / `cvis_…` token | Per container / `cvis_…` token (optional per-logical-agent for OpenClaw via M8) |
| Install complexity | Low (plugin only) | Medium (plugin + proxy + installer) | Medium (hosted: low) |
| Trust commitment | Service creds only | + network traffic visibility | + all agent credentials |

Each stage is a proportional trade: more trust ceded, more security gained, no forced migration. Attribution-granularity improvements are opt-in enhancements, not default behavior.
