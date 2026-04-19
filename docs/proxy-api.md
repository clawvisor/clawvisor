# Clawvisor Proxy ↔ Server API Contract

**Status:** Draft (bootstrap)
**Purpose:** Define the wire-level HTTP/JSON contract between the Clawvisor Server (this repo) and the Clawvisor Proxy (separate repo, forked from Kumo, currently attached as a submodule for dev convenience).
**Audience:** Implementers on both sides. This document is normative — both repos implement against it; neither imports code from the other.

---

## 1. Why this document exists

The server and the proxy are developed in separate repositories with no shared code dependencies (see [design-proxy-stage1.md §2](./design-proxy-stage1.md)). Schema types are duplicated on each side. That means a single source of truth is required, and it's this document.

Compatibility is enforced by:
1. Both implementations reading this contract.
2. An integration test suite (runs nightly in both repos' CI) that exercises the actual wire protocol.
3. Explicit versioning rules (§3) so either side can evolve without silently breaking the other.

If this doc and an implementation disagree, **the doc wins** and the implementation is buggy. Fix the implementation, or update the doc first with review from both sides.

---

## 2. Scope

This contract covers the proxy-facing API the server exposes. It is organized by the roadmap stages in the design docs:

- **Stage 1 endpoints** — shipped together as the MVP. Implementers can target this set and have a working system.
- **Stage 2 endpoints** — credential injection, SSE invalidations. Additive; Stage 1 clients should not break when Stage 2 endpoints appear.
- **Stage 3 endpoints** — policy decisions, ban checks. Also additive.

Endpoints the proxy *doesn't* call (e.g. the dashboard's `/api/plugin/bridges/{id}/transcript` for user consumption) are not in this document. Only the proxy↔server plane.

---

## 3. Versioning and compatibility

### 3.1 Contract version

This document itself is versioned.

- Current: **v1-draft**.
- On any breaking change, the version bumps (`v2-draft`, then `v2` on first GA).
- The proxy reports the contract version it was built against via the `X-Clawvisor-Contract-Version` request header on every request. The server reports its contract version on every response via the same header. Either side can warn or refuse if the other is on an incompatible version.

### 3.2 Additive vs breaking

**Additive changes (no version bump):**
- New optional request fields (consumed if present, ignored if absent).
- New optional response fields (old clients ignore).
- New endpoints.
- New enum values — but only when both sides handle unknowns gracefully (spec the default handling explicitly per-endpoint).

**Breaking changes (requires version bump):**
- Removing fields.
- Changing the type of a field.
- Changing the semantics of an existing enum value.
- Changing response status codes.
- Requiring new mandatory fields.

### 3.3 Unknown-field tolerance

Both sides MUST tolerate unknown JSON fields on input — parse what's recognized, ignore the rest, do not error. Failing-closed on unknown fields would force lockstep deploys.

### 3.4 Schema duplication discipline

Because types are duplicated (server in Go, proxy in Go), both sides keep their own struct definitions. This is the price of repo independence. Guardrails:

- This doc is the normative source; struct definitions are implementations of it.
- When a field is added or renamed here, both repos update in the same release cycle, and the integration test suite catches drift.
- `pkg/proxyapi/` does NOT exist in either repo. There is no shared Go module.

---

## 4. Conventions

### 4.1 Base URL

The proxy is configured at startup with a server URL. All endpoints in this doc are relative to that URL (e.g., `GET /api/proxy/config` against base `http://clawvisor-server:25297` resolves to `http://clawvisor-server:25297/api/proxy/config`).

### 4.2 Authentication

Every request from the proxy includes the `cvisproxy_...` bearer token in the `Authorization` header:

```
Authorization: Bearer cvisproxy_abc123...
```

The server validates the token on every request. Unauthenticated requests return `401 UNAUTHORIZED`.

Token lifecycle:
- Issued at pair time (see Stage 1 §3.1).
- Persisted in the install artifact.
- Rotated by the user re-running the Installer. Old tokens remain valid until explicitly revoked via dashboard.

### 4.3 Content type

All request/response bodies are `application/json; charset=utf-8`. The server MAY return `text/event-stream` for specific SSE endpoints (clearly marked below).

### 4.4 Timestamps

RFC 3339 / ISO 8601, UTC, millisecond precision:

```
2026-04-19T03:14:22.512Z
```

Never Unix epoch seconds/milliseconds in this contract (even though the `TurnEvent` examples in the Stage 1 design use them for intra-proxy references; the wire format is always RFC 3339).

### 4.5 Identifiers

- `event_id` — ULID, proxy-generated, globally unique across proxies. Opaque string to the server.
- `bridge_id` — UUID v4, server-assigned.
- `agent_token_id` — the `cvis_...` token itself, used as identity. Opaque.
- `conversation_id` — provider-dependent format (e.g., `telegram:8536852960`). Opaque to both sides except for grouping.

### 4.6 Error response format

```json
{
  "error": "missing authorization header",
  "code": "UNAUTHORIZED",
  "hint": "Include 'Authorization: Bearer cvisproxy_...' on every request."
}
```

Fields:
- `error` — human-readable message. Freely formatted; not stable across versions.
- `code` — machine-readable constant. Stable. Documented per-endpoint.
- `hint` — optional, user-actionable guidance.

Status codes used:
- `200` — success
- `400` — bad request (malformed body, missing required field)
- `401` — missing/invalid auth
- `403` — authenticated but not authorized (e.g., bridge revoked)
- `404` — resource not found
- `409` — conflict (e.g., seq regression, duplicate event)
- `429` — rate limited
- `500` — server error, proxy should retry with backoff
- `503` — server temporarily unavailable (e.g., DB down), proxy should retry

### 4.7 Rate limiting

Server MAY rate-limit per `cvisproxy_...` token. On `429`, response includes:

```
Retry-After: 5
```

The proxy MUST respect `Retry-After`. Excessive 429s should be surfaced operationally (metrics, dashboard alert).

### 4.8 Request size limits

Default: 10 MB. Individual endpoints may document stricter limits (e.g., `POST /api/proxy/turns` caps at 5 MB per batch).

---

## 5. Stage 1 endpoints (MVP)

The minimum set for a working proxy↔server integration.

### 5.1 GET /api/proxy/config

**Purpose:** Proxy fetches its runtime configuration at startup and periodically (default every 60s) thereafter.

**Request:**
```
GET /api/proxy/config HTTP/1.1
Authorization: Bearer cvisproxy_...
X-Clawvisor-Contract-Version: v1-draft
```

**Response (200):**
```json
{
  "contract_version": "v1-draft",
  "proxy_instance_id": "01HXX...",
  "bridge_id": "7f953938-bd9f-43a2-a3e0-70bc2b3718c6",
  "agents": [
    {
      "agent_token_id": "cvis_abc123",
      "agent_label": "main"
    }
  ],
  "provider_parsers": ["anthropic", "telegram"],
  "server_time": "2026-04-19T03:14:22.512Z",
  "config_ttl_seconds": 60
}
```

Fields:
- `proxy_instance_id` — assigned by the server when the proxy first registers. Proxy persists and reuses it across restarts.
- `bridge_id` — which bridge this proxy serves.
- `agents` — list of agent tokens the proxy will accept via `Proxy-Authorization`. Tokens NOT in this list are rejected by the proxy with `407` (or forwarded unattributed if the proxy is configured to do so — Stage 1 rejects).
- `provider_parsers` — which provider-specific parsers are expected to be enabled. Proxy enables matching parsers; unknown values ignored with a warning.
- `server_time` — used by the proxy for clock-skew detection.
- `config_ttl_seconds` — proxy refetches config at least this often.

**Errors:**
- `401 UNAUTHORIZED` — token invalid.
- `403 BRIDGE_REVOKED` — bridge is revoked; proxy should stop forwarding traffic.

### 5.2 POST /api/proxy/turns

**Purpose:** Proxy ingests batched `TurnEvent`s captured from observed traffic.

**Request:**
```
POST /api/proxy/turns HTTP/1.1
Authorization: Bearer cvisproxy_...
Content-Type: application/json
X-Clawvisor-Contract-Version: v1-draft

{
  "events": [
    { /* TurnEvent, see §8.1 */ },
    { /* TurnEvent */ }
  ]
}
```

Limits:
- Max 1000 events per batch.
- Max 5 MB body size.
- Proxy SHOULD batch on time (default 1000ms window) or count (default 100 events), whichever first.

**Response (200):**
```json
{
  "accepted": 2,
  "rejected": [],
  "warnings": []
}
```

Partial-failure semantics: per-event rejections are reported in `rejected`; the other events are accepted. A full-batch failure returns `400` or `500` with no body-level per-event info.

```json
{
  "accepted": 1,
  "rejected": [
    {
      "event_id": "evt_01HYX...",
      "code": "SEQ_REGRESSION",
      "error": "seq 1234 is not strictly greater than last_seq 1235 for this bridge"
    }
  ],
  "warnings": [
    {
      "event_id": "evt_01HYX...",
      "code": "SIGNATURE_INVALID",
      "error": "Ed25519 verification failed"
    }
  ]
}
```

Stage 1: `SIGNATURE_INVALID` is a warning (audit-only, per Stage 1 design §4.3). Stage 2+: it becomes a `rejected` code.

**Error codes:**
- `DUPLICATE_EVENT` — same `event_id` previously ingested for this bridge. No-op.
- `SEQ_REGRESSION` — per-bridge seq not strictly monotonic. Rejected at Stage 1 just as at Stage 0 (this rule is inherited from the existing buffer ingest).
- `INVALID_REQUEST` — required field missing / malformed.
- `SIGNATURE_INVALID` — (warning at Stage 1; rejection at Stage 2+).
- `BRIDGE_REVOKED` — bridge is revoked; full batch rejected.

**Retry semantics:**
- `500`/`503`: retry with exponential backoff (starting 1s, max 60s, jitter ±20%).
- `429`: honor `Retry-After`.
- `400`: do not retry the same payload. Log and drop.
- Individual rejected events: do not retry (they'll be rejected again with the same code).
- Individual accepted events: forgotten by the proxy.

### 5.3 POST /api/proxy/signing-keys/rotate

**Purpose:** Proxy registers a new signing public key. Called at proxy startup and when rotating (default daily).

**Request:**
```
POST /api/proxy/signing-keys/rotate HTTP/1.1
Authorization: Bearer cvisproxy_...
Content-Type: application/json

{
  "key_id": "proxy-2026-04-19",
  "alg": "ed25519",
  "public_key": "MCowBQYDK2VwAyEA..."
}
```

Fields:
- `key_id` — opaque string, unique per proxy. Convention: `proxy-{YYYY-MM-DD}` but not required.
- `alg` — `"ed25519"` (only supported at Stage 1).
- `public_key` — base64-encoded DER (for Ed25519, the 32-byte public key wrapped in SubjectPublicKeyInfo).

**Response (200):**
```json
{
  "key_id": "proxy-2026-04-19",
  "registered_at": "2026-04-19T03:14:22.512Z"
}
```

**Error codes:**
- `DUPLICATE_KEY_ID` — this `key_id` already exists for this proxy. Proxy should treat as success (idempotent re-registration on restart).
- `INVALID_KEY` — key could not be parsed.

### 5.4 Idempotency

All POST endpoints in this contract MUST be idempotent on retry. This is explicit:

- `POST /api/proxy/turns` — idempotent per `event_id`. Retrying a batch re-processes dedup; no user-visible effect.
- `POST /api/proxy/signing-keys/rotate` — idempotent per `key_id`. Re-registering returns the original registration timestamp.

Proxy MAY include an `Idempotency-Key` header as defense in depth; server behavior is the same either way.

---

## 6. Stage 2 endpoints (credential injection)

Added when Stage 2 ships. Stage 1 proxies work without ever calling these; Stage 2 proxies use them for every injected request.

### 6.1 POST /api/proxy/credential-lookup

**Purpose:** Fetch a credential from the server's vault for injection into an outbound request.

**Request:**
```
POST /api/proxy/credential-lookup HTTP/1.1
Authorization: Bearer cvisproxy_...
Content-Type: application/json

{
  "agent_token_id": "cvis_abc123",
  "credential_ref": "vault:anthropic",
  "destination_host": "api.anthropic.com",
  "destination_path": "/v1/messages",
  "request_id": "req_01HXX..."
}
```

Fields:
- `destination_host` / `destination_path` — for server-side logging + rate-limit scoping; does not affect which credential is returned (the server trusts the proxy's injection rules for host matching, but logs the actual destination for audit).
- `request_id` — proxy-generated; correlates to the eventual `TurnEvent`.

**Response (200):**
```json
{
  "credential": "sk-ant-api03-...",
  "credential_id": "cred_01HZZ...",
  "ttl_seconds": 300,
  "cache_key": "vault:anthropic#v7"
}
```

Fields:
- `credential` — the plaintext credential to inject.
- `credential_id` — server-side identifier for this specific credential version. Changes on rotation.
- `ttl_seconds` — proxy caches for up to this long. Server MAY push an invalidation (see §6.2) before then.
- `cache_key` — opaque. Proxy uses this as the cache key; the key changes on rotation so a simple lookup self-invalidates when the cached `cache_key` mismatches a fresh response.

**Error codes:**
- `CREDENTIAL_NOT_FOUND` — the `credential_ref` is not configured.
- `CREDENTIAL_NOT_AUTHORIZED` — the agent's ACL does not permit this credential.
- `CREDENTIAL_REVOKED` — credential has been explicitly revoked; proxy should block the request (fail-closed).

**Security posture:** returned `credential` is plaintext. Proxy MUST NOT log, persist to disk, or transmit further than the outbound request it's building. See Stage 2 design §2.3 for cache discipline.

### 6.2 GET /api/proxy/invalidations (SSE)

**Purpose:** Server-initiated push to invalidate cached credentials on rotation/revocation.

**Request:**
```
GET /api/proxy/invalidations HTTP/1.1
Authorization: Bearer cvisproxy_...
Accept: text/event-stream
```

**Response (200):**
```
Content-Type: text/event-stream

event: invalidate
data: {"cache_key":"vault:anthropic#v7","reason":"rotated"}

event: invalidate
data: {"cache_key":"vault:openai#v3","reason":"revoked"}

event: heartbeat
data: {"server_time":"2026-04-19T03:15:22.512Z"}

```

Events:
- `invalidate` — proxy SHOULD evict the cache entry matching `cache_key`. Next lookup refetches.
- `heartbeat` — every 30s, or more often. Connection keepalive + clock sync.

**Reconnect semantics:**
- On disconnect, proxy reconnects with exponential backoff (1s → 60s).
- On reconnect, proxy MAY re-fetch all cached credentials to resync (belt-and-braces against missed invalidations during disconnect).
- Server MAY buffer invalidations for a disconnected proxy up to 5 min; beyond that, proxy re-fetches as above.

---

## 7. Stage 3 endpoints (policy enforcement)

Added when Stage 3 ships. Fast-rule evaluation happens inside the proxy using policy fetched via `/api/proxy/config`. These endpoints are for the cases where the proxy must defer to the server.

### 7.1 Policy delivery in /api/proxy/config

At Stage 3, `/api/proxy/config` response gains a `policy` field:

```json
{
  /* ...Stage 1 fields... */
  "policy": {
    "version": 7,
    "compiled": { /* PolicyDoc, see §8.3 */ }
  },
  "bans": [
    {
      "agent_token_id": "cvis_abc123",
      "rule_name": "block_repo_delete",
      "banned_until": "2026-04-19T04:14:22.512Z"
    }
  ]
}
```

Proxy applies `policy.compiled.rules.fast` inline for every request. On `flag` outcome, proxy calls §7.2 for a judge decision.

### 7.2 POST /api/proxy/policy-decision

**Purpose:** Proxy defers a `flag` outcome to the server-side judge for a contextual decision.

**Request:**
```
POST /api/proxy/policy-decision HTTP/1.1
Authorization: Bearer cvisproxy_...
Content-Type: application/json

{
  "agent_token_id": "cvis_abc123",
  "rule_name": "flag_unknown_post",
  "request": {
    "method": "POST",
    "host": "api.greenhouse.io",
    "path": "/v1/candidates/123/notes",
    "body_excerpt": "..."   /* truncated ≤ 2 KB */
  },
  "conversation_id": "telegram:8536852960",
  "request_id": "req_01HXX..."
}
```

**Response (200):**
```json
{
  "decision": "allow",
  "reason": "User explicitly requested the action.",
  "cached_until": "2026-04-19T03:19:22.512Z"
}
```

`decision` values: `"allow"`, `"block"`, `"flag_for_human_review"`.

Proxy MAY cache the decision per `(agent_token_id, rule_name, request_id_semantic_hash)` until `cached_until` — server decides the cache window.

**Latency:** typical 500-2000ms (LLM judge). Timeout: 5s default, configurable per-policy.

**Error codes:**
- `JUDGE_UNAVAILABLE` — judge LLM unreachable. Proxy applies policy's `on_error` rule (`block` by default).
- `POLICY_NOT_FOUND` — `rule_name` doesn't exist in the current policy; likely a stale proxy cache. Proxy SHOULD refetch `/api/proxy/config`.

### 7.3 POST /api/proxy/ban-check

**Purpose:** Proxy checks whether an agent is currently banned before processing a request.

In practice, the proxy uses the `bans` list from `/api/proxy/config` for the hot path. This endpoint is for the edge case where a proxy wants to verify freshness after a cold start or before a particularly destructive request.

**Request:**
```
POST /api/proxy/ban-check HTTP/1.1
Authorization: Bearer cvisproxy_...
Content-Type: application/json

{
  "agent_token_id": "cvis_abc123"
}
```

**Response (200):**
```json
{
  "banned": true,
  "rule_name": "block_repo_delete",
  "banned_until": "2026-04-19T04:14:22.512Z"
}
```

Or:
```json
{
  "banned": false
}
```

---

## 8. Shared types

### 8.1 TurnEvent

Used in §5.2 `POST /api/proxy/turns`. Produced by the proxy, consumed by the server.

```json
{
  "event_id": "evt_01HXX...",
  "ts": "2026-04-19T03:14:22.512Z",
  "source": "proxy",
  "source_version": "v1",
  "stream": "channel",
  "agent_token_id": "cvis_abc123",
  "bridge_id": "7f953938-bd9f-43a2-a3e0-70bc2b3718c6",
  "conversation_id": "telegram:8536852960",
  "provider": "telegram",
  "direction": "inbound",
  "role": "user",
  "turn": {
    "text": "Can you book me a ride to the airport?",
    "tool_calls": null,
    "tool_results": null
  },
  "raw_ref": {
    "traffic_log": "2026-04-19.jsonl#L4213",
    "request_body_hash": "sha256:9b4f7c..."
  },
  "signature": {
    "alg": "ed25519",
    "key_id": "proxy-2026-04-19",
    "sig": "base64..."
  }
}
```

**Field semantics:**

| Field | Type | Required | Semantics |
|---|---|---|---|
| `event_id` | string (ULID) | yes | Unique per proxy; server dedups on `(bridge_id, event_id)`. |
| `ts` | RFC3339 string | yes | Wall-clock capture time. Server MAY clamp far-future values (inherits Stage 0 rule: more than 60s ahead → drop). |
| `source` | string enum | yes | `"proxy"` for events from the proxy. `"plugin"` reserved for Stage 0 scavenger data (not emitted by the proxy). |
| `source_version` | string | yes | Schema version of the event. `"v1"` at Stage 1. |
| `stream` | enum | yes | `"llm"` \| `"channel"` \| `"action"`. See Stage 1 §4.1. |
| `agent_token_id` | string | yes | `cvis_...` token this event is attributed to. |
| `bridge_id` | UUID string | yes | Redundant with the authenticated proxy's bridge, but included for routing. |
| `conversation_id` | string \| null | no | Provider-native conversation key. Required for `stream: "channel"`. Nullable for `stream: "llm"` at Stage 1 (sessionization deferred; see Stage 1 §4.1). |
| `provider` | string | yes | `"anthropic"`, `"openai"`, `"telegram"`, etc. Proxy's parser identifier. |
| `direction` | enum | yes | `"inbound"` or `"outbound"`. User → agent / agent → user-or-service. |
| `role` | enum | yes | `"user"` \| `"assistant"` \| `"tool"` \| `"system"`. Normalized across providers. |
| `turn.text` | string | no | Extracted text content (already concatenated across blocks). Nullable for pure tool-call turns. |
| `turn.tool_calls` | array \| null | no | LLM-stream only. Array of `{id, name, input}` objects. |
| `turn.tool_results` | array \| null | no | LLM-stream only. Array of `{tool_use_id, content, is_error}` objects. |
| `raw_ref.traffic_log` | string | no | Pointer into proxy's own traffic log for audit. Server does not dereference; stores as-is. |
| `raw_ref.request_body_hash` | string | no | SHA-256 of the raw request body the TurnEvent was derived from. |
| `signature` | object | no (yes at Stage 2+) | Ed25519 signature over a canonical serialization of the event (sans signature block). |

**Canonical serialization for signing:** JSON with keys sorted lexically, no whitespace, UTF-8. The exact spec is documented in the proxy repo (`docs/signing.md`) and MUST match byte-for-byte on the server side for signature verification to succeed. Proxy repo owns this spec; server repo implements verification.

### 8.2 InjectionRule (Stage 2)

Served by the server, consumed by the proxy. Part of `/api/proxy/config` response at Stage 2.

```json
{
  "rule_id": "rule_01HYY...",
  "priority": 100,
  "match": {
    "host": "api.anthropic.com",
    "path_pattern": "/v1/*",
    "methods": ["POST"]
  },
  "inject": {
    "style": "header",
    "target": "x-api-key",
    "template": "{{credential}}",
    "credential_ref": "vault:anthropic"
  },
  "enabled": true
}
```

`inject.style` values: `"header"` \| `"path"` \| `"query"`. See Stage 2 §2.1.

`template` uses `{{credential}}` as the only substitution. Proxy implements a minimal substitution — no full template engine.

### 8.3 PolicyDoc (Stage 3)

Compiled form. Server compiles from user-authored YAML; proxy consumes the compiled JSON.

```json
{
  "version": 7,
  "bridge_id": "7f953938-...",
  "rules": {
    "fast": [
      {
        "name": "block_repo_delete",
        "action": "block",
        "match": {
          "hosts": ["api.github.com"],
          "methods": ["DELETE"],
          "paths": ["/repos/*"]
        },
        "message": "Repository deletion is not allowed."
      }
    ],
    "judge": {
      "enabled": true,
      "model": "claude-haiku-4-5",
      "timeout_ms": 5000,
      "on_error": "block"
    },
    "default": "flag"
  },
  "ban": {
    "enabled": true,
    "max_violations": 3,
    "window_seconds": 3600,
    "ban_duration_seconds": 3600,
    "scope": "per_rule"
  }
}
```

See Stage 3 §2.1 for authoring. Proxy applies `rules.fast` in order, first match wins.

---

## 9. Test vectors

For CI integration tests on both sides, a shared set of test vectors lives at `docs/proxy-api-vectors/` in this repo. Proxy repo pulls via its integration test harness (direct HTTP fetch at test time, not a code dep).

Initial vector set (Stage 1):

- `vectors/turn-events/user-telegram-inbound.json`
- `vectors/turn-events/assistant-anthropic-outbound.json`
- `vectors/turn-events/assistant-anthropic-streamed-reassembled.json`
- `vectors/errors/seq-regression.json`
- `vectors/errors/duplicate-event.json`

Each vector includes: the request body the proxy would send, the expected server response, and (for success cases) the expected database state after.

Maintenance: when either side adds or changes a behavior, it adds a test vector in a PR to this repo, referenced by the implementation PR on either side.

---

## 10. Example flows

### 10.1 Startup

```
proxy starts
  ├─ POST /api/proxy/signing-keys/rotate        { key_id: "proxy-2026-04-19", ... }
  │   ← 200 { registered_at: "..." }
  ├─ GET /api/proxy/config
  │   ← 200 { proxy_instance_id, bridge_id, agents, provider_parsers, ... }
  └─ ready to forward traffic
```

### 10.2 User message via Telegram (Stage 1)

```
agent → api.telegram.org/bot.../getUpdates
  proxy observes response body
  proxy parses → TurnEvent (channel, inbound, user)
  proxy batches with other events

POST /api/proxy/turns { events: [...] }
  ← 200 { accepted: N, rejected: [] }
```

### 10.3 Anthropic LLM call with credential injection (Stage 2)

```
agent → POST api.anthropic.com/v1/messages   (no x-api-key)
  proxy matches injection rule
  ├─ POST /api/proxy/credential-lookup { credential_ref: "vault:anthropic", ... }
  │   ← 200 { credential: "sk-ant-...", ttl_seconds: 300, cache_key: "...#v7" }
  ├─ proxy injects x-api-key header
  └─ proxy forwards to api.anthropic.com

proxy observes response
  proxy parses → TurnEvent (llm, outbound, assistant)
  POST /api/proxy/turns ...
```

### 10.4 Flagged request, judge defers (Stage 3)

```
agent → POST api.greenhouse.io/v1/candidates/123/notes
  proxy matches flag rule "flag_unknown_post"
  ├─ POST /api/proxy/policy-decision { agent, rule_name, request, conversation_id }
  │   ← 200 { decision: "allow", reason: "User requested notes creation." }
  ├─ proxy forwards (decision: allow)
  └─ proxy emits TurnEvent
```

---

## 11. Evolution rules

### 11.1 Adding a new provider parser

1. Proxy repo adds the parser; emits new `provider` value.
2. Server tolerates unknown `provider` values (stores string as-is, displays in dashboard).
3. This doc adds the provider to the enum list; no version bump.

### 11.2 Adding a new stream type

1. This doc adds to `stream` enum. No version bump.
2. Server tolerates unknown `stream` values (store, don't crash).
3. Both sides ship the new behavior in the release they deliberately name.

### 11.3 Removing a field

1. Version bump to `v2-draft`.
2. Deprecation period: both sides support old and new for at least one release.
3. After deprecation, server rejects old-format requests with `426 Upgrade Required` (or similar) including the contract version it expects.

### 11.4 Changing semantics

Treat as removal + addition: version bump, overlapping support, hard cutover. Do not silently re-interpret fields.

---

## 12. Known open questions

These are flagged in the design docs but have direct impact on this contract. Resolve before Stage 1 GA.

1. **`conversation_id` keying for LLM stream.** Stage 1 emits `null`; Stage 2 adds content-hash-based keying (Stage 2 §5.5). When Stage 2 ships, is this an additive change (new field values become populated) or does the field's semantics shift enough to warrant a version bump? Leaning additive; confirm with real implementation.

2. **`turn.text` for streaming tool-call-only responses.** For an LLM response that is pure `tool_use` with no text, is `turn.text` `""` or `null`? This doc says nullable; both sides must agree.

3. **Whether `raw_ref.traffic_log` is actually useful to the server.** Today it's stored as opaque string. If it's never dereferenced, consider dropping it to save bytes.

4. **Rate limits.** Not specified in this contract. Defer until we see real traffic patterns.

5. **Multi-proxy support.** Today one proxy per bridge. Multi-proxy-per-bridge (active/standby, or fleet for hosted) needs a consistency story for `proxy_instance_id` and event ordering. Deferred to hosted-mode design.

---

## 13. Changelog

- `v1-draft` (2026-04-19): Initial draft, Stage 1 endpoints with Stage 2+3 outlined.
