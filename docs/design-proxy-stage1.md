# Design: Clawvisor Network Proxy — Stage 1

**Status:** Draft for review
**Author:** Eric Levine (with Claude)
**Target audience:** Clawvisor contributors + collaborators reviewing the direction before implementation
**Scope lock:** Stage 1 only, self-hosted, OpenClaw as first framework, Anthropic as first LLM provider, channel-stream transcripts as first signal.

---

## 1. Context

### 1.1 Where we are today

Clawvisor is an application-layer gateway for AI agents. It vaults third-party credentials (GitHub PATs, Gmail OAuth tokens, etc.), exposes those credentials to agents under task-scoped authorization, and provides LLM-based auto-approval for sensitive actions. Agents integrate via framework-specific plugins — today, an OpenClaw plugin that registers Clawvisor tools, handles pairing, and (recently) scrapes assistant replies out of OpenClaw's session JSONL to give auto-approval enough conversation context to make decisions.

That scavenger is fragile. Not unreliable — it's been hardened with watermarks, backfill windows, and timestamp normalization — but structurally tamperable. A compromised agent running with write access to its own session files can forge user↔assistant exchanges before the plugin reads them. For 99% of users this is irrelevant. For the auto-approval property Clawvisor *sells*, it's a real concern: the feature's trustworthiness is bounded by the least-trusted component in the transcript pipeline.

### 1.2 What this document proposes

Introduce a sovereign TLS-intercepting HTTP proxy as an *optional* enhancement layer. Forked from [Kumo](https://github.com/garrytan/kumo), deployed alongside the Clawvisor server, the proxy:

- Sits on the network path between the agent and the outside world.
- Terminates TLS using a CA cert the agent trusts but cannot read.
- Observes all HTTP(S) traffic the agent emits.
- Parses known API schemas (Anthropic initially, channel APIs like Telegram incrementally) to emit structured, signed `TurnEvent`s.
- Forwards the (possibly modified) request upstream and returns the response.

The proxy becomes a second, higher-trust source of the same transcript data the plugin scavenger produces today. Downstream consumers (auto-approval, dashboard, audit) can prefer proxy-sourced data when it's available and fall back to plugin data when it isn't.

This is Stage 1 of a progressive trust ladder. Stages 2 (credential injection — agent never sees API keys) and 3 (network policy enforcement — proxy can block disallowed requests) are out of scope for this spec but are informed by the architecture choices made here.

### 1.3 Non-goals

The following are explicitly *not* in scope for Stage 1:

- **Credential injection.** The proxy observes; it does not inject API keys into agent requests. LLM API keys remain in agent config for now. (Stage 2.)
- **Policy enforcement.** The proxy does not block or rewrite requests based on rules. Observe-only. (Stage 3.)
- **Additional LLM providers** beyond Anthropic. OpenAI, Gemini, Bedrock, xAI, etc. come later.
- **Additional channels** beyond whatever OpenClaw's Telegram adapter produces. Slack, Discord, email, webhooks — later.
- **Additional agent frameworks** beyond OpenClaw. Claude Code, Cursor, Aider, custom agents come later.
- **Hosted deployment.** This spec is for the self-hosted repo. A hosted-mode design doc forks from this one.
- **BYOK / customer-managed KMS.** Enterprise concern, deferred.
- **Windows support.** macOS + Linux only for Stage 1.
- **HTTP/2 / gRPC interception.** HTTP/1.1 + SSE is sufficient for Anthropic's API today.
- **WebSocket frame inspection.** Handshake passes through, individual frames are tunneled. (Matches Kumo's current behavior.)

### 1.4 Success criteria

Stage 1 ships successfully when:

1. A user running OpenClaw in Docker can opt into the proxy via a single user-run install command (see §3.2 on the Clawvisor Installer model), and thereafter every Anthropic API call + every Telegram API call their agent makes *via the proxy* flows through and ingests into the Clawvisor buffer. Stage 1 defaults to Posture A (proxy by convention, no egress enforcement); Posture B (egress enforced) is offered as an opt-in in the install artifact, documented as the hardened path.
2. The auto-approval engine consumes proxy-sourced **channel-stream** transcripts (marked `source: "proxy"`) in preference to scavenger-sourced transcripts (`source: "plugin"`) for channels the proxy can see (Telegram). For channels the proxy does not see (webchat, as in-container WebSockets), plugin-sourced data remains authoritative. LLM-stream events are captured but not consumed by auto-approval (see §4.1 on why — sessionization is a Stage 2 deliverable).
3. A cross-check anomaly detector flags discrepancies between proxy-observed and plugin-reported channel-stream conversations. (Tamper detection is free once both sources exist.)
4. Users who do not opt into the proxy continue to work exactly as they do today. No regressions at Stage 0.
5. Dashboard + marketing language accurately reflects the default posture: "tamper-proof transcripts for the traffic the proxy sees." The stronger claim ("agent structurally cannot bypass") is available only for Posture B users and is labeled as such in the setup flow.

---

## 2. Architecture

### 2.1 Process topology

Two sovereign processes on the host, communicating over a local authenticated channel.

```
┌──────────────────────────────────────────────────────────────────┐
│  Host                                                             │
│                                                                   │
│  ┌────────────────────────────────────────────────────────────┐  │
│  │  clawvisor-server   (existing binary)                       │  │
│  │    · identity + auth (bridge + agent tokens)                │  │
│  │    · credential vault (unchanged)                           │  │
│  │    · buffer / transcript store (extended)                   │  │
│  │    · dashboard (extended)                                   │  │
│  │    · proxy config API (NEW)                                 │  │
│  │    · transcript ingest from proxy (NEW)                     │  │
│  └────────────────────────────────────────────────────────────┘  │
│                       ▲                                           │
│                       │ localhost HTTP, mTLS or                   │
│                       │ pre-shared token                          │
│                       ▼                                           │
│  ┌────────────────────────────────────────────────────────────┐  │
│  │  clawvisor-proxy    (NEW binary, forked from Kumo)          │  │
│  │    · TLS MITM                                                │  │
│  │    · per-agent gateway_token auth                            │  │
│  │    · provider-aware parsers (Anthropic for Stage 1)         │  │
│  │    · signed TurnEvent emission                               │  │
│  │    · traffic log (JSONL, local)                              │  │
│  └────────────────────────────────────────────────────────────┘  │
│                       ▲                                           │
│                       │ HTTP_PROXY                                │
│                       │                                           │
│  ┌────────────────────────────────────────────────────────────┐  │
│  │  agent container (OpenClaw)                                 │  │
│  │    · clawvisor plugin (tool exposure only — scavenger dies) │  │
│  │    · egress forced through proxy via HTTP_PROXY + iptables  │  │
│  │    · trusts Clawvisor-minted CA cert                         │  │
│  └────────────────────────────────────────────────────────────┘  │
└──────────────────────────────────────────────────────────────────┘
```

Three container-level identities:

- **Agent container** (untrusted). Gets a public CA cert (for trust) and an HTTP_PROXY env var. Has no access to the proxy's CA key, no access to the Clawvisor server except through the pair-flow endpoints and the tool-call endpoints.
- **Proxy container** (sovereign). Holds the CA private key. Has outbound network access. Talks to the server over localhost.
- **Server container** (sovereign). Holds the vault + buffer. Unchanged perimeter.

The agent has no path to tamper with either sovereign process — not the proxy's CA key, not the server's vault, not the transcript stream between them.

**Isolation unit = one agent per process/container.** Because HTTP_PROXY and its embedded Proxy-Authorization are process-scoped (not per-logical-agent), the attribution model assumes exactly one agent identity per proxy client. Users who conceptually run multiple agents from one Clawvisor install run them as separate processes (or containers), each with its own HTTP_PROXY env and its own `cvis_…` token. Multi-agent-per-process *might* be supportable via framework-specific per-tool-client proxy-auth injection (OpenClaw's plugin could do this for its internal tool clients), but it is explicitly **out of scope for Stage 1**; we support the common case and document the limitation.

### 2.2 Trust boundaries

Two deployment postures have materially different guarantees. Be explicit about which one a given claim applies to:

- **Posture A — "proxy by convention."** HTTP_PROXY is set, CA cert is installed, no egress enforcement at the network level. An honest-but-buggy or well-behaved agent routes through the proxy; a deliberately malicious agent can unset HTTP_PROXY or use its own CA bundle and bypass. **This is the default Stage 1 posture** because egress enforcement requires privileges (iptables, Docker network policy) that many prosumer deployments don't grant.
- **Posture B — "proxy enforced."** Posture A *plus* network-level egress enforcement: iptables on native deployments, Docker network policy in Compose deployments, or Kubernetes NetworkPolicy in K8s deployments. All agent egress is blocked except to the proxy. A malicious agent cannot bypass. **This is opt-in at Stage 1** via extra rules in the install artifact; the user applies them consciously and at root/admin privilege.

The trust-boundary table below labels which mitigations apply to which posture:

| Boundary | Threat | Mitigation (Posture A) | Mitigation (Posture B) |
|---|---|---|---|
| Agent → proxy | Agent unsets `HTTP_PROXY` and makes direct calls | **None** — compromised/malicious agent can bypass. Detectable via comparison against expected traffic volume, but not preventable. | iptables / Docker network rules block egress except to proxy; agent physically cannot reach the outside world any other way. |
| Agent → proxy | Agent tries to bypass TLS by using its own CA bundle | Proxy's CA cert is in the system trust store (installed by the Clawvisor Installer, not the plugin). Agent runs as non-root and cannot overwrite the system trust store. Agent could still set `SSL_CERT_FILE` in its own env and provide its own bundle — partial bypass. | Same as A, plus all egress still blocked, so even if the agent bypasses trust-store its traffic can't escape. |
| Agent → proxy | Agent sends garbage to proxy to crash it | Proxy is a separate process; agent can't escalate into it. | Same. |
| Proxy → server | Proxy forwards forged `TurnEvent`s on behalf of agents it never saw | Proxy authenticates to server with pre-shared token (rotated by pair flow); per-event signatures are recorded + verified as audit metadata (see §4.3) but are not the ingest gate at Stage 1. | Same. |
| Server → agent | Server leaks credentials agent shouldn't see | Unchanged from today. | Unchanged. |

**What this means for the product narrative.** Stage 1 *by default* gives you observability against honest agents and detection of deviation by malicious ones, not prevention. The tamper-proofing property we advertise for transcripts is real, but it holds *conditional on the agent actually going through the proxy* — for Posture A deployments, that is a behavioral assumption, not a structural guarantee. Posture B closes this by making bypass structurally impossible, at the cost of more setup and more privileges.

Dashboard and marketing language should reflect this: "tamper-proof transcripts for the traffic the proxy sees." For users who need the stronger claim, Posture B instructions are prominently offered as the hardened path.

The proxy is a trusted party under either posture. The server trusts the proxy to forward faithful observations. This is correct in the single-tenant prosumer deployment: both sovereign processes belong to the same user. It is not correct in a multi-tenant future; that's an enterprise problem.

### 2.3 Deployment shape (self-hosted)

- `clawvisor` (existing binary) — runs the server.
- `clawvisor-proxy` (new binary) — runs the proxy.
- Both distributed as part of the Clawvisor release. Installed via Homebrew / Docker image / native binary bundle.
- Recommended deployment: two Docker containers, shared network namespace with the agent container(s), isolated from the agent at the process level.
- Single-binary mode where `clawvisor serve --with-proxy` runs both in-process, for the "I don't care about sovereignty, just want the observability" user. Documented but not the default.

**Why expose the insecure single-binary option at all?** Because a non-trivial fraction of the prosumer segment wants to run Clawvisor as a single native process on macOS, and asking them to spin up a second container is a real bounce risk. We give them the option to deploy less-securely with an honest label attached; "real security nerds" (user's phrase) opt into the two-container path.

---

## 3. Identity and auth

### 3.1 Token model

Stage 1 does not introduce new token types. It reuses the existing ones with a slight widening of their scope:

| Token | Format | Today's scope | Stage 1 scope |
|---|---|---|---|
| Bridge token | `cvisbr_…` | Plugin install identity; authorizes buffer ingest + agent management | Also: authorizes the proxy config API (proxy fetches its config at startup) |
| Agent token | `cvis_…` | Per-agent identity; authorizes tool calls | Also: acts as the proxy's `gateway_token` when authenticating outbound requests |
| Pair code | `cvpc_…` | Single-use bootstrap | Unchanged; now provisions proxy config in addition to plugin config |
| Proxy-to-server token | — | — | NEW: `cvisproxy_…` — pre-shared, issued at pair time, proxy authenticates to server with this |

The `cvisproxy_…` token is scoped extremely narrowly: it can post `TurnEvent`s to the server and fetch its own runtime config. It cannot read vault contents, cannot act on behalf of any user. This is the only new token type and it exists purely for proxy↔server auth.

### 3.2 Pair flow — the privilege boundary

**Clarification up front: the plugin runs unprivileged inside the agent container.** It cannot install CA certs into the system trust store, cannot write env vars that affect already-started processes, cannot apply iptables rules, and cannot restart the agent. Any spec language suggesting the plugin "provisions" these things is wrong. Privileged work is performed by a separate **Clawvisor Installer**, not the plugin.

**Three actors in the Stage 1 setup flow:**

1. **Dashboard (server)** — mints tokens, generates a per-bridge *install artifact*, stores it for retrieval.
2. **Clawvisor Installer** — a privileged helper that applies the install artifact to the user's environment. Runs outside the agent container, with whatever privileges are needed (docker-socket access, root on the host, etc.). Supplied as:
   - A `docker-compose.yml` snippet the user composes in (for Docker deployments). The snippet brings up the proxy container, mounts the CA cert into the agent container, configures networking, sets env vars.
   - A native install script (`curl | bash` style) for bare-metal / macOS native deployments. Runs as the user, with sudo prompts for the trust-store mutation and iptables steps.
   - Eventually (Stage 2+), a daemon process that talks to the Docker socket and applies artifacts automatically. Out of scope for Stage 1.
3. **Plugin** — runs inside the agent container once everything is set up. Reads tokens from a read-only mount, exposes tools, handles webchat-style in-container channels that the proxy can't see.

**Install artifact shape:**

```yaml
# returned by GET /api/plugin/bridges/{id}/install-artifact
# scoped to a single bridge + opt-in-to-Stage-1 request
version: 1
bridge_id: "..."
components:
  ca_cert_pem: |
    -----BEGIN CERTIFICATE-----
    ...
  ca_cert_fingerprint: "sha256:..."
  proxy:
    image: "clawvisor/proxy:v0.x.y"
    bind: "0.0.0.0:8080"
    token: "cvisproxy_..."     # proxy authenticates to server with this
    server_url: "http://clawvisor-server:25297"
  agent_env_additions:
    HTTP_PROXY: "http://cvis_abc123:@clawvisor-proxy:8080"
    HTTPS_PROXY: "http://cvis_abc123:@clawvisor-proxy:8080"
    NODE_EXTRA_CA_CERTS: "/etc/clawvisor/ca.crt"
    SSL_CERT_FILE: "/etc/clawvisor/ca.crt"
  trust_store:
    mount_path: "/etc/clawvisor/ca.crt"
    system_paths_to_update:
      debian: "/usr/local/share/ca-certificates/clawvisor.crt"
      alpine: "/usr/local/share/ca-certificates/clawvisor.crt"
  iptables_rules:           # optional; only if user wants egress enforcement
    - "OUTPUT -d clawvisor-proxy -j ACCEPT"
    - "OUTPUT -j DROP"
  plugin_secrets:
    bridge_token: "cvisbr_..."
    agent_tokens: { "main": "cvis_..." }
    mount_path: "/etc/clawvisor/secrets.json"
  docker_compose_snippet: |
    # ready-to-paste compose file for this install
    ...
  install_script: |
    # ready-to-run script for native installs
    ...
```

**Two user flows:**

*Flow A: "At pair time" (new installs, Docker-based).* User runs `clawvisor pair openclaw` (a new CLI command). The command hits the dashboard pair endpoint, receives the install artifact, writes a `docker-compose.yml` to the user's directory, and prints "Review and run `docker compose up -d`." User inspects, applies. Proxy starts, agent container starts with env + CA in place, plugin starts last and reads its token mount. Single human-in-the-loop step (the `docker compose up`).

*Flow B: "Opt-in after" (existing Stage 0 users).* User clicks "Enable Network Proxy" in the dashboard. Server generates the install artifact. Dashboard shows: "Run this to enable: `curl https://.../artifact.sh | bash`" (for native installs) or "Update your docker-compose.yml with this snippet" (for Docker). User applies. Same outcome.

Either way, the *plugin does no privileged work*. The plugin can signal readiness ("I see my mounts, I'm operational") but the actual mutation is done by the Clawvisor Installer acting on the user's behalf at the appropriate privilege level.

**Why not automate it end-to-end?** Because the automation requires privileges that belong to the user, not to Clawvisor. A daemon that manipulates the Docker socket on the user's behalf is possible (and is in scope for Stage 2's "one-click" hosted-mode flow) but requires the user to install and trust that daemon. For Stage 1, the honest answer is: **the user runs one command at pair time, under their own authority.** Call it "one-command opt-in" rather than "one-click."

**Server endpoints for the install artifact:**

- `POST /api/plugin/bridges/{id}/enable-proxy` — user JWT; marks the bridge as opted-in to Stage 1, generates a new install artifact, returns it.
- `GET /api/plugin/bridges/{id}/install-artifact` — user JWT; fetches the current artifact. Used by the `clawvisor pair` CLI and the dashboard "show me my install snippet" UI.
- `POST /api/plugin/bridges/{id}/disable-proxy` — user JWT; marks bridge as Stage 0, revokes the `cvisproxy_…` token. User is responsible for reverting their compose file / env / iptables rules on their end.

The plugin's runtime endpoint (`GET /api/plugin/proxy-setup`) is removed from the design — the plugin no longer performs provisioning. The plugin just reads its own secrets file at startup (same as Stage 0) and trusts that whatever Installer-provided environment it finds is correct.

### 3.3 Gateway token usage

The agent container's `HTTP_PROXY` env var includes the per-agent `cvis_…` token as proxy auth:

```
HTTP_PROXY=http://cvis_abc123:@localhost:8080
HTTPS_PROXY=http://cvis_abc123:@localhost:8080
```

Standard RFC 7235 proxy auth. The proxy strips `Proxy-Authorization` before forwarding upstream (Kumo already does this). The proxy uses the agent token to:

- Identify which agent this traffic belongs to (for attribution in the transcript).
- Look up the agent's configured providers in the proxy's config cache.
- (Stage 2) find the right injected credential for the destination host.

Per §2.1, **one agent per process/container is the supported attribution model**. The `HTTP_PROXY` env var and its embedded Proxy-Authorization apply to the whole process; generic HTTP clients do not split proxy auth per logical agent. Users who run multiple agents from one Clawvisor install do so as separate processes/containers with separate `cvis_…` tokens in their respective `HTTP_PROXY` env vars. Attribution is trivial: `Proxy-Authorization → cvis_token → agent identity`.

Framework-specific per-tool-client proxy-auth injection is possible (OpenClaw's plugin could override per-tool-client config to use a different token for a different internal agent) but is an optional enhancement, not part of the Stage 1 attribution guarantee. Stage 2's credential ACLs inherit this isolation unit: "per-agent" in the credential ACL means "per-container-identity," which in practice equals "per `cvis_…` token."

### 3.4 CA cert trust

The CA cert is generated *once* at pair time, scoped to this install, and stored in the vault (private key) and distributed (public cert) to the agent container at setup. Proxy holds the private key in its own process memory; agent has only the public cert.

Agent container trust-store install paths (by platform):

- Debian/Ubuntu: `/usr/local/share/ca-certificates/clawvisor.crt` + `update-ca-certificates`
- Alpine: `/usr/local/share/ca-certificates/clawvisor.crt` + `update-ca-certificates`
- Node.js (for OpenClaw specifically): `NODE_EXTRA_CA_CERTS=/path/to/clawvisor.crt`

Install script covers Debian/Alpine (dominant base images for OpenClaw Docker) and sets `NODE_EXTRA_CA_CERTS` as a belt-and-braces for the Node runtime.

If the user re-pairs, a new CA is minted. Old CA is revoked. (Mechanism: server keeps a revoked-CA list, proxy honors it. Stage 1 just implements the mint side; revoke happens in the dashboard.)

---

## 4. Transcript schema

### 4.1 Sources and streams

Three signal streams the proxy could theoretically emit. Stage 1 ships #1 (observability only) and #2 (the auto-approval source); #3 is raw-logged but not parsed.

1. **LLM stream** (Anthropic API calls in Stage 1).
   Captures the raw LLM input/output: user prompts, tool_use blocks, tool_result blocks, assistant responses. This is what the *agent* saw and produced.

   **Critical caveat: Anthropic's Messages API is stateless.** Every request body contains the full `messages[]` array of prior turns. If the proxy naively emits a `TurnEvent` per entry in every request's `messages[]`, it will duplicate previously-emitted turns on every subsequent request in the same conversation.

   For Stage 1, the LLM stream is therefore an **observability-and-audit-only** signal. It is NOT consumed by auto-approval. Duplicates are accepted as a known limitation; the dashboard transcript view deduplicates client-side by `turn.id` where possible, or displays with explicit "duplicate of earlier turn" markers. The production-grade sessionization algorithm (conversation keying + delta emission) is designed in Stage 2, where it becomes load-bearing for proxy-only-mode auto-approval.

2. **Channel stream** (Telegram API calls in Stage 1, because that's what OpenClaw uses most).
   Captures user-facing messages — what the user actually typed and what the agent actually delivered. Inbound polls (`getUpdates`) give us user messages; outbound `sendMessage` gives us agent replies. These APIs are not stateless: each message is emitted exactly once by the platform, so deduplication is straightforward (key on the platform's `message_id`).

   This is the auto-approval source at Stage 1. Because every message is emitted once, the channel stream yields a clean, monotonic transcript of the user-facing conversation, without the sessionization complexity of the LLM stream.

3. **Action stream** (other outbound API calls — GitHub, Gmail, etc.).
   Captures attempted actions. Logged as raw HTTP (like Kumo already does). Stage 3 uses this for policy enforcement; Stage 1 just records it.

**Stage 1 scope lock on auto-approval:** auto-approval consumes the **channel stream only**. The LLM stream is stored and displayed, not used for decisions. This avoids the sessionization problem entirely at Stage 1 and keeps the architecture honest about what's known to work versus what's still a research problem.

### 4.2 `TurnEvent` schema

Unified event shape across streams. Producer (proxy) emits, consumer (server) ingests.

```json
{
  "event_id": "evt_01HYXXXX",
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
    "request_body_hash": "sha256:..."
  },
  "signature": {
    "alg": "ed25519",
    "key_id": "proxy-2026-04-19",
    "sig": "..."
  }
}
```

Field notes:

- `event_id`: ULID, generated by proxy at capture time. Globally unique across proxies.
- `source`: `"proxy"` (Stage 1 — also `"plugin"` for existing scavenger data, for cross-check).
- `stream`: `"llm"` | `"channel"` | `"action"`.
- `agent_token_id`: the `cvis_…` identity that produced the request.
- `conversation_id`: whatever the stream natively uses for conversation keying. For Telegram: `telegram:{chat_id}`. For Anthropic: derived from the agent's session, or `null` if not identifiable (the LLM API is stateless).
- `direction`: `"inbound"` (user → agent) or `"outbound"` (agent → user/action).
- `role`: normalized turn role — `"user"`, `"assistant"`, `"tool"`, `"system"`.
- `turn.text`: parsed, user-facing text content. Tool blocks are in the structured fields.
- `turn.tool_calls` / `turn.tool_results`: populated for LLM-stream events that contain tool interaction; null otherwise.
- `raw_ref`: pointer into the proxy's traffic log for audit. The server does not store raw bodies; it stores pointers.
- `signature`: Ed25519 signature over a canonical serialization of the event. Key rotated per-day, public keys published in the dashboard for verification.

### 4.3 Signing

The proxy signs every `TurnEvent` at capture time. Signing happens in the proxy container, using a key the proxy generated locally and holds in memory.

**Stage 1 posture: audit metadata, not an enforcement gate.**

The server *records* each event's signature and *verifies* the signature as a health check, but does **not** reject events with bad or missing signatures at Stage 1. An invalid signature produces a logged warning and a dashboard-visible flag on the event; the event is still persisted and consumed by auto-approval. This is the consistent contract for Stage 1 everywhere — the ingest path in §4.5 and the Stage 1 narrative must not claim signature-based rejection until Stage 2.

Rationale: at Stage 1 the proxy and server are on the same host and trust each other via the pre-shared `cvisproxy_…` token. Signature verification in this topology is useful as evidence-of-origin but is not the gate that keeps bad data out (the token check is). Converting signatures into a hard rejection at Stage 1 means any key-rotation bug or clock-skew issue silently drops data for unclear reasons; not worth the operational pain when the token check already prevents forgery in this topology.

**Stage 2+ tightens this.** When the proxy may run remotely (hosted) or multiple proxies may ingest to a shared server, signatures become the primary origin gate. Stage 2's ingest path rejects events with invalid signatures and uses signing-key identity for per-proxy rate limits, attribution, and key revocation. The data model and on-the-wire format are deliberately the same as Stage 1 so this is a policy flip, not a schema migration.

**What signing buys us (at Stage 1):**

- **Tamper-evident audit trail.** Signed events can be independently replayed and verified later, including after a possible server compromise. Analysts can reject forged events ex post even if the ingest path accepted them.
- **Forward-compatible trust primitive.** Stage 2+ (hosted, multi-tenant, federation) inherits this signing format and enforces it. Shipping Stage 1 unsigned and adding signatures later would be a painful migration.

**What signing does NOT buy us at Stage 1:**

- End-to-end proof that the *agent* saw what the proxy saw. (The agent and proxy could theoretically disagree; the proxy's observation is authoritative by definition.)
- Automatic rejection of forged events at ingest time. That's Stage 2's job.

Signing key management: generated at proxy startup, written to a file readable only by the proxy process, rotated daily. Public keys registered with the server via `POST /api/proxy/signing-keys/rotate` and published to `GET /api/proxy/signing-keys` for ex-post verification. Not wired to KMS in Stage 1.

### 4.4 Provider parsers

For Stage 1 we ship parsers for:

1. **Anthropic Messages API** (`api.anthropic.com/v1/messages`).
   - Handle both non-streaming (`application/json` response) and streaming (`text/event-stream`).
   - Extract `messages[]` from request body (user turn, prior assistant turns, tool_results).
   - Extract `content` blocks from response body (assistant text, tool_use).
   - Reassemble streamed SSE events into a single logical turn. (Critical: Anthropic ships text via `content_block_delta`, tool calls via `content_block_start` + `input_json_delta`.)
   - Emit `TurnEvent`s for every entry in `messages[]` on every request, plus one for the response. This *intentionally over-emits* because of the stateless-API issue in §4.1 — Stage 1 accepts duplicates as an observability cost. Stage 2 designs the sessionization that suppresses them. Consumers (auto-approval in particular) must not consume LLM-stream events at Stage 1.

2. **Telegram Bot API** (`api.telegram.org/bot{token}/…`).
   - `getUpdates` response → emit inbound `TurnEvent` per message in the update list.
   - `sendMessage` request → emit outbound `TurnEvent` with the sent text.
   - Skip administrative endpoints (`getMe`, `setMyCommands`, etc.).

Parsers live in `internal/proxy/parsers/{anthropic,telegram}.go`. Adding a provider later means dropping a new file that implements the `Parser` interface. Unknown providers fall through to action-stream raw logging (Kumo's existing behavior).

### 4.5 Ingest path

Proxy → server: `POST /api/proxy/turns` with a batch of `TurnEvent`s.

- Batched for efficiency (up to N events or T milliseconds, whichever first).
- Idempotent via `event_id` — server dedupes, batches can be retried safely.
- Authenticated with `cvisproxy_…` token in the Authorization header.
- Server records + verifies signatures as audit metadata only at Stage 1 (see §4.3). Invalid signatures produce a warning and flag the event but do not reject it. Ingest-time rejection on bad signature is a Stage 2+ contract.
- Server persists `TurnEvent`s to a new `transcript_events` table. Populates the existing buffer (backwards-compat) for auto-approval consumption.

Error handling:

- Transient server error: proxy retries with exponential backoff, up to a buffer cap (disk-backed queue, bounded at, say, 100MB).
- Server unavailable for extended period: proxy keeps capturing but alerts that ingest is lagging. Agent traffic is not blocked.
- Malformed event: server rejects with detail, proxy logs + continues. No fail-closed at Stage 1.

---

## 5. Server changes

### 5.1 New schema

Two new tables (Postgres + SQLite migrations). Names subject to bikeshedding.

**`transcript_events`** — structured `TurnEvent`s.

```sql
CREATE TABLE transcript_events (
    event_id           TEXT PRIMARY KEY,
    source             TEXT NOT NULL,          -- 'proxy' | 'plugin'
    source_version     TEXT,
    stream             TEXT NOT NULL,          -- 'llm' | 'channel' | 'action'
    bridge_id          TEXT NOT NULL REFERENCES bridge_tokens(id),
    agent_token_id     TEXT,                   -- nullable; not all events tied to a single agent
    conversation_id    TEXT,
    provider           TEXT,
    direction          TEXT,                   -- 'inbound' | 'outbound'
    role               TEXT,
    text               TEXT,
    tool_calls         JSONB,                  -- or TEXT in SQLite
    tool_results       JSONB,
    raw_ref            JSONB,
    signature          JSONB,
    ts                 TIMESTAMPTZ NOT NULL,
    ingested_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    INDEX (bridge_id, ts DESC),
    INDEX (conversation_id, ts DESC)
);
```

**`proxy_instances`** — registered proxies and their metadata.

```sql
CREATE TABLE proxy_instances (
    id                    TEXT PRIMARY KEY,          -- ULID
    bridge_id             TEXT NOT NULL REFERENCES bridge_tokens(id),
    token_hash            TEXT NOT NULL UNIQUE,      -- hash of cvisproxy_ token
    signing_key_pub       TEXT,                      -- current public key, rotated
    ca_cert_fingerprint   TEXT NOT NULL,
    last_seen_at          TIMESTAMPTZ,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    revoked_at            TIMESTAMPTZ
);
```

### 5.2 New endpoints

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `POST` | `/api/plugin/enable-proxy` | user JWT | Opt this bridge into Stage 1. Returns the proxy-setup blob. |
| `GET` | `/api/plugin/proxy-setup` | bridge token | Plugin fetches current proxy setup for this bridge. Returns 404 if not opted in. |
| `GET` | `/api/proxy/config` | cvisproxy token | Proxy fetches its runtime config. |
| `POST` | `/api/proxy/turns` | cvisproxy token | Proxy ingests batched `TurnEvent`s. |
| `POST` | `/api/proxy/signing-keys/rotate` | cvisproxy token | Proxy registers new signing key. |
| `GET` | `/api/plugin/bridges/{id}/transcript` | user JWT | Dashboard dump — returns `transcript_events` for a bridge (replaces the debug buffer endpoint we added recently). |

The existing `POST /api/buffer/ingest` endpoint is NOT removed. Plugin continues to use it at Stage 0. At Stage 1, the plugin's scavenger is disabled (feature flag set by the proxy-setup blob), but the endpoint remains for non-proxy users.

### 5.3 Buffer consumer changes

The auto-approval engine reads from the buffer today via `h.msgBuffer.Messages(key)`. Stage 1 introduces a new source-ranked read:

```go
// Preferred: read transcript_events where source='proxy' and bridge_id=X
// Fallback: read buffer (legacy, plugin-sourced) if transcript_events is empty
```

New consumer function: `transcript.Recent(ctx, bridgeID, conversationID, window)` that encapsulates the source preference. Auto-approval calls this; existing `msgBuffer.Messages` callers migrate over time.

### 5.4 Cross-check for tamper detection

When both `proxy` and `plugin` data exist for the same conversation, a background job compares them. Discrepancies (plugin has a user message the proxy didn't see; proxy has an assistant turn the plugin doesn't) emit an anomaly record. Dashboard surfaces "potential transcript tampering detected" for the user to review.

Stage 1 implements the comparison + anomaly record; acting on the anomaly (revoking, alerting) is Stage 3 policy work.

---

## 6. Plugin changes

The plugin's role at Stage 1 shrinks but does not vanish. Per §3.2, the plugin does **not** perform any privileged provisioning — that work belongs to the Clawvisor Installer, acting under the user's authority outside the agent container. The plugin's job is passive.

| Responsibility | Stage 0 | Stage 1 (proxy enabled) |
|---|---|---|
| Pair flow (redeem pair code, POST `/api/plugin/pair`) | ✓ | ✓ (unchanged) |
| Provision trust store / env / iptables / container restarts | ✗ | ✗ (Installer's job, not plugin's) |
| Tool exposure (`api.registerTool`) | ✓ | ✓ (unchanged) |
| Secret storage (read-only mount of bridge/agent tokens) | ✓ | ✓ (plus `cvisproxy_…` if present in the mount) |
| Inbound `message_received` hook | ✓ (forwards to buffer) | ✓ (only for channels the proxy can't see — e.g., webchat) |
| Outbound scavenger (JSONL scrape) | ✓ | ✗ (disabled via a server-side config flag read on startup) |
| Traffic forwarding / transcript ingest | ✓ (HTTP to server) | ✗ (done by proxy) |
| Heartbeat / runtime polling to server | minimal | unchanged (no new polling endpoints at Stage 1) |

**How the scavenger is disabled at Stage 1.** When the plugin starts, it fetches its own runtime config from the server via its existing bridge-token-authenticated endpoint and reads a `scavenger_enabled: bool` field. If `false`, the scavenger code path is skipped. No proxy-setup blob is involved; the plugin has no need to know about proxy provisioning beyond "am I in a deployment that has a proxy running." The server sets `scavenger_enabled = !bridge.proxy_active`.

**The scavenger stays in the codebase** for users who revert to Stage 0 or never opt into Stage 1. Removing it would be wrong — it serves the non-proxy segment indefinitely.

**Webchat is a wrinkle.** OpenClaw's webchat is a direct browser↔container WebSocket that never leaves the internal Docker network. The MITM can't see it. For webchat, the plugin's `message_received` hook remains the source of truth. The `source` field on `transcript_events` distinguishes per-channel data: Telegram events have `source:"proxy"`, webchat events have `source:"plugin"` even at Stage 1.

This is honest and correct: not all channels are MITM-observable. The architecture acknowledges this instead of papering over it.

---

## 7. Dashboard changes

### 7.1 Progressive disclosure

Stage 0 dashboard stays the same. Stage 1 adds:

- **"Enable Network Proxy" card** on the bridge detail page. One dashboard click marks the bridge as opted-in and generates the install artifact (§3.2); completing setup then requires the user to run the generated command on their host at appropriate privilege (root for trust-store + iptables; user-level for Docker Compose with a working Docker daemon). The card explains this two-step flow and what the user is consenting to (decrypted traffic visibility).
- **Setup instructions panel** post-opt-in: displays the CA cert, the `HTTP_PROXY` env var to set, the iptables rules to apply, with platform-specific tabs (Docker, Docker Compose, Kubernetes, bare native).
- **Proxy health indicator**: green if the proxy has checked in recently, red if not. Quick diagnostics on failure modes.
- **Transcript view**: replaces the current buffer-dump debug endpoint with a first-class dashboard view. Filter by conversation, by time range, by source. Side-by-side diff view when both proxy and plugin data exist for the same conversation (for cross-check).
- **Anomaly feed**: per-bridge list of transcript-tampering anomalies (Stage 1 detection, Stage 3 response).

### 7.2 Not in Stage 1

- Policy authoring UI (Stage 3).
- Traffic explorer (browse all raw HTTP the agent has made). Probably Stage 2-ish.
- Analytics / cost breakdown by LLM provider. Nice-to-have, not near-term.

---

## 8. Pair flow, end to end

Walkthrough of a new user opting in at pair time.

### 8.1 Stage 0 path (unchanged, for reference)

1. Dashboard: user clicks "Connect OpenClaw," mints `cvpc_...` pair code.
2. Dashboard: shows setup snippet with pair code.
3. User: pastes snippet into agent.
4. Agent: plugin runs setup, POSTs to `/api/plugin/pair` with `cvpc_...`.
5. Dashboard: pair request appears, user approves.
6. Plugin: receives `{bridge_token, agent_tokens}`, writes to secrets file.
7. Plugin: starts normal operation — tool exposure, `message_received` scavenger, etc.

### 8.2 Stage 1 path — at pair time (new installs)

1. User runs `clawvisor pair openclaw` on their host (or equivalent GUI-driven dashboard flow for non-CLI users).
2. CLI calls the dashboard, user authenticates, selects "enable network proxy."
3. Server mints bridge + agent tokens + proxy token, generates install artifact (§3.2), returns it.
4. CLI writes `docker-compose.yml` (and/or an install script) to the user's current directory. Shows a diff / summary of what will be applied: "This will start a `clawvisor-proxy` container, mount a CA cert into your OpenClaw container, set HTTP_PROXY env vars, and optionally apply iptables rules."
5. User reviews, runs `docker compose up -d` (or `bash install.sh` for native).
6. Installer runs: trust store updated, env vars written into compose, proxy container started, agent container restarted with the new env. Plugin's secrets mount is populated.
7. Agent container starts. Plugin reads secrets mount, starts normal operation. Proxy sees agent's traffic, emits `TurnEvent`s.

Single human-in-the-loop step (the `docker compose up` / `bash install.sh` command), all privileged mutations done by the Installer acting under the user's authority.

### 8.3 Opt-in-after path — existing Stage 0 users

1. User: dashboard → bridge detail → "Enable Network Proxy." Server generates a fresh install artifact.
2. Dashboard displays: "Run this command on your host: `curl -sL https://.../artifact.sh | bash`" (for native installs) OR "Merge this snippet into your docker-compose.yml and re-apply" (for Docker).
3. User applies. Installer performs the same mutations as §8.2 step 6.
4. Agent container restarts. Plugin picks up the new secrets mount. Proxy is now in the path.

No plugin polling, no heartbeat required. The user is the trigger for the privileged action, and the user's next step after enabling in the dashboard is clear.

### 8.4 Opt-out

1. User: dashboard → bridge detail → "Disable Network Proxy." Server marks `proxy_instances.revoked_at` and generates a teardown artifact (optional — most users will just remove the compose changes manually).
2. User: removes the proxy container / install script output / iptables rules from their deployment, restarts the agent.
3. Agent returns to direct connections. Plugin's scavenger re-enables via a config flag pulled from server at startup (Stage 0 behavior restored).

Existing `transcript_events` remain queryable in the dashboard. The bridge's `revoked_at` timestamp lets auto-approval fall back to plugin-only data for this bridge going forward.

---

## 9. Migration

There is no Stage 0 → Stage 1 migration that happens automatically. Every user is Stage 0 on upgrade; they opt into Stage 1 per-bridge.

**For existing deployments installed before Stage 1 ships:**

- Existing bridges continue to work exactly as they do today.
- Plugin scavenger continues to run.
- Auto-approval reads from buffer as today.
- Dashboard adds the "Enable Network Proxy" option. No action required.

**For the Clawvisor codebase:**

- New migrations (`033_transcript_events.sql`, `034_proxy_instances.sql`).
- Existing `groupchat.Buffer` interface unchanged — it's a legacy path now.
- New `transcript.Store` interface wraps the new table.
- Auto-approval gets a new read path via `transcript.Recent(...)`; old path remains.

Backwards compatibility: Stage 0 works forever. Stage 1 is purely additive.

---

## 10. Security considerations

### 10.1 What the proxy can do

Sees decrypted LLM traffic, including any API keys in transit (Anthropic key in `Authorization` header). Sees Telegram bot tokens in URLs. Sees channel content — user prompts, assistant responses.

Mitigations:
- Proxy runs in its own container. Agent can't read proxy memory, can't read CA key, can't see traffic logs.
- Proxy logs redact `Authorization` headers (as Kumo already does).
- Traffic log file is readable only by the proxy user.
- Proxy does not phone home. Does not emit any network traffic itself except to the server (over localhost).

### 10.2 What a compromised proxy can do

If an attacker gets code execution in the proxy container: they see everything. They can forge `TurnEvent`s (but the forgery is *their own* — it's authoritative in the same way a compromised signer's signature is authoritative).

Mitigations:
- Proxy has minimal dependencies; fewer attack surfaces.
- Proxy has no write access to the server's vault. It reads config; it does not modify anything the agent can act on.
- Proxy doesn't run arbitrary user code. No plugin system, no scripting, no eval.
- Proxy is upgraded together with Clawvisor itself; standard supply-chain hygiene (signed releases, checksum verification).

### 10.3 What a compromised agent can do

Given the proxy in the path: it can produce convincing-looking traffic, but the proxy records everything. Auto-approval sees the ground truth; tampering with the plugin-side JSONL doesn't affect the proxy-side transcript.

What the agent *can still* do: generate legitimate-looking requests that happen to be malicious (e.g., `POST /api/v1/orders` with a valid payload to exfiltrate). The proxy sees it, logs it, but doesn't block it at Stage 1. Stage 3 (policy enforcement) closes this.

### 10.4 TLS certificate lifecycle

CA cert lives forever until revoked. Revocation via dashboard updates `bridge_tokens.ca_revoked_at`; proxy checks on startup + every N minutes; agents trust lists are *not* updated automatically (this is a known limitation — the CA cert persists in the agent's trust store until re-provisioned).

For Stage 1, operational guidance: "revoke the bridge and repair" is the full rotation path. Not great, but acceptable for a product at this stage.

### 10.5 Traffic log retention

Proxy's raw traffic log (JSONL, one file/day): retained per server config (default 7d at Stage 1). Structured `transcript_events` retained per user config (default 30d). Retention is a per-bridge setting, overridable in the dashboard, described in §11.

Raw bodies for LLM/channel traffic: stored in the JSONL with 10KB truncation (Kumo's existing behavior). Not stored in `transcript_events` — only parsed text + tool blocks are.

---

## 11. Retention

Per-install default, per-bridge override. No per-org yet (not multi-tenant at Stage 1).

**Defaults:**

| Data class | Default retention | User-configurable range |
|---|---|---|
| Proxy raw traffic log (JSONL) | 7 days | 1–90 days |
| `transcript_events` (parsed) | 30 days | 7–365 days |
| Anomaly records | 90 days | 30–365 days |
| Signing public keys | forever | forever (for audit) |

**Implementation:**
- Background job sweeps daily, deletes by `ingested_at < now() - retention`.
- Proxy prunes its own traffic log with its own retention setting (served via `/api/proxy/config`).
- Dashboard surfaces current retention per bridge; change applies immediately to new writes, within 24h to existing data.

Not enterprise-grade yet: no legal hold, no per-conversation retention, no compliance export. Those land when paying enterprise customers ask for them.

---

## 12. Open questions

Things to resolve during implementation or immediately prior:

1. **CA cert install inside the agent container.** OpenClaw's official Docker image base — what is it? Debian, Alpine, distroless? The install artifact's trust-store mutation depends on this. If distroless, we need a different approach (bake the cert into the container at build, or rely on `NODE_EXTRA_CA_CERTS` only and skip system trust store). The Clawvisor Installer (see §3.2) handles this, but needs to know the agent container's base image — either detect at install time or require the user to say.

2. **What exactly does the Clawvisor Installer ship as?** Three candidates: (a) static `docker-compose.yml` snippet generated per-bridge, (b) a per-bridge install script for native/bare-metal, (c) a long-running daemon with Docker-socket access for automatic provisioning. Stage 1 probably ships (a) + (b); (c) is a Stage 2 "hosted UX" thing. Need to pick what's in scope for Stage 1 M4.

3. **Proxy-to-server auth transport.** Localhost HTTP with bearer token is simplest. Unix socket is tighter (no network exposure). mTLS is most-secure but more moving parts. Default to localhost HTTP with bearer token for Stage 1 unless there's a reason not to.

4. **Telegram webhook mode.** If a user configures OpenClaw to receive Telegram messages via webhook (inbound HTTP to their server) instead of `getUpdates` polling, the proxy doesn't see the inbound messages. Only outbound `sendMessage`. Partially-observable. Document this; maybe offer webhook forwarding as a future feature.

5. **Multi-agent containers and proxy port.** If a user runs OpenClaw + AlphaClaw + a custom agent all in one container (unusual but possible), they all share the `HTTP_PROXY` setting. Distinct agent tokens in the env let the proxy attribute correctly, but verify this works with how Node/Go/Python HTTP clients handle proxy auth.

6. **Proxy single-binary mode opt-in.** How insecure-mode is flagged in the dashboard so users know what they opted into. Needs a clear label.

7. **Signing key backup.** If the proxy's signing key is lost, past events become unverifiable. Is that OK for Stage 1? (Probably yes — signatures are best-effort.) If not, we need key escrow. Flag decision.

8. **LLM request/response body retention in traffic log.** 10KB truncation breaks on long conversations (a 30-turn chat can exceed 10KB easily). Either raise the truncation, compress, or store bodies separately (content-addressed?). Needs a call before implementation.

9. **OpenClaw config-change reloads.** We observed this session that OpenClaw hot-reloads the plugin on config changes, which reset scavenger state. Does the proxy have a similar problem? (No — it's in a separate process.) Does the plugin's Stage 1 role depend on state that survives reloads? (Only token config, which persists on disk.)

10. **Rate limits.** Proxy makes decisions at request frequency. If auto-approval check fires on every message, and we hit a chatty conversation, we could overload the approval LLM. Rate limit the approval path, not the observation path. Flag for Stage 1 testing.

---

## 13. Milestones

Each milestone is a shippable chunk. The "demo-able" criterion matters — at any point, progress should be showable to someone.

### M1: Proxy spike (2 weeks)

- Fork Kumo into `clawvisor/proxy` (or an internal path under this repo — decide).
- Port the core TLS MITM + HTTP_PROXY handling.
- Add Anthropic parser (non-streaming first, then SSE).
- Emit `TurnEvent`s over stdout (not yet to server — local testing only).
- No auth yet, no signing yet.

Demo: run the proxy, curl through it to Anthropic, see structured events printed.

### M2: Proxy ↔ server integration (1 week)

- New server endpoints: `/api/proxy/turns`, `/api/proxy/config`.
- `transcript_events` table + migrations.
- Proxy authenticates with `cvisproxy_…` token, ingests events.
- Wire up `transcript.Recent(...)` read path. Auto-approval prefers it when data present.
- Dashboard "Enable Network Proxy" card (basic form).

Demo: enable proxy in dashboard, send a message through OpenClaw, see the proxy-sourced transcript appear in the dashboard alongside the plugin-sourced one.

### M3: Cutover (1 week)

- Plugin config flag to disable scavenger for Stage 1 bridges.
- Cross-check background job.
- Anomaly dashboard feed (basic).
- Signing (Ed25519 key at proxy, verification at server).

Demo: induce a transcript tamper on the agent side, anomaly appears in dashboard.

### M4: Install artifact + Clawvisor Installer (2 weeks)

- Install artifact schema (§3.2) + server endpoints to generate/fetch it.
- `clawvisor pair openclaw` CLI command that writes `docker-compose.yml` + optional install script to the user's host.
- One-shot Docker Compose template for two-container deployment (pre-filled with tokens from the artifact).
- Native-install script for macOS / Linux bare-metal deployments.
- Opt-in-after path via dashboard — generates artifact, user applies.
- UX polish: setup instructions panel, platform tabs, platform auto-detect where possible.

Explicit non-goal for M4: a long-running supervisor daemon with Docker-socket access. That's a Stage 2 hosted-mode feature; Stage 1 ships the "one command, under the user's authority" UX.

Demo: a new user can go from "never heard of Clawvisor" to a working Stage 1 install in under 10 minutes, with one `docker compose up -d` (or equivalent) step.

### M5: Polish + docs + release (1-2 weeks)

- Documentation: architecture, user guide, threat model.
- Retention config UI.
- Stability: 24h soak test with a real OpenClaw + Telegram deployment.
- Honest release-note "Stage 1 is beta; here's what works, here's what doesn't."

Total: 7-8 weeks. Single engineer, focused. Slip factor 1.5x realistic → 10-12 weeks wall-clock. That's "Stage 1 lands before end of Q3" territory.

---

## 14. What this spec leaves for Stage 2

Notes for the Stage 2 design doc that will come later:

- **Credential injection.** Proxy-side: destination-host-based credential lookup, vault IPC, caching. Agent-side: stripping API keys from config, using Clawvisor-identity-only config. UX: "move my Anthropic key to Clawvisor" button in dashboard.
- **Multi-provider LLM support.** OpenAI, Gemini, Bedrock, xAI parsers. Shared `LLMParser` interface.
- **Framework-agnostic install.** Claude Code, Cursor, Aider via proxy-only (no plugin). Proxy becomes the universal integration surface.
- **Hosted proxy.** Clawvisor cloud runs the proxy; user's agent points at it. Trust-and-convenience tradeoff for the prosumer mass market.
- **Traffic-log browsing UI.** Raw HTTP request explorer for debugging.
- **Basic policy rules.** Block by host/path pattern. Not yet the full Kumo policy engine.

## 15. What this spec leaves for Stage 3

- **Full policy engine.** Kumo's `kumo summarize` → policy YAML flow. Fast rules + LLM judge.
- **Network enforcement.** Block mode active. Bans. Rate limits. 403 responses to agent.
- **Anomaly response** (not just detection). Auto-revoke, auto-notify, auto-quarantine.
- **Enterprise surface.** Multi-tenant, orgs, per-team policies, audit export.

---

## 16. Explicit rejections / things considered and NOT doing

- **Merge proxy + server into a single process.** Considered. Rejected because it violates sovereignty for the "real security nerds" user segment and doesn't save enough complexity to matter. The single-binary opt-in mode gives the convenience without forcing the architecture.
- **Use eBPF / kernel-level interception instead of HTTP_PROXY.** Considered. Rejected — more complex, platform-specific (Linux only), harder UX, no clear benefit over HTTP_PROXY with iptables for Stage 1.
- **Sidecar pattern** (proxy in the same pod as agent but different container). Kubernetes-friendly. Considered. Deferred to Stage 2 — prosumer ICP doesn't care about K8s.
- **User-managed CA cert** (user generates their own, uploads to Clawvisor). Considered. Rejected — adds friction for negligible security benefit. Clawvisor-minted CA is fine for Stage 1.
- **Make the plugin go away entirely at Stage 1.** Considered. Rejected — tool exposure is the plugin's durable job, plus webchat observability requires it. The plugin's role shrinks but doesn't disappear.
- **Sign `TurnEvent`s with a KMS key instead of a proxy-local key.** Considered. Rejected for Stage 1 — KMS integration on the proxy side adds complexity and cost. Stage 2+ re-evaluates if/when hosted proxies emit to customer servers.

---

## Appendix A: Example `TurnEvent` payloads

### A.1 User message via Telegram (inbound channel)

```json
{
  "event_id": "evt_01HTTL...",
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
  "signature": { "alg": "ed25519", "key_id": "proxy-2026-04-19", "sig": "..." }
}
```

### A.2 Assistant reply via Anthropic Messages API (LLM stream, outbound)

```json
{
  "event_id": "evt_01HTTL...",
  "ts": "2026-04-19T03:14:25.841Z",
  "source": "proxy",
  "source_version": "v1",
  "stream": "llm",
  "agent_token_id": "cvis_abc123",
  "bridge_id": "7f953938-bd9f-43a2-a3e0-70bc2b3718c6",
  "conversation_id": null,
  "provider": "anthropic",
  "direction": "outbound",
  "role": "assistant",
  "turn": {
    "text": "Sure — I'll check Uber for a ride from your home to SFO.",
    "tool_calls": [
      {
        "id": "toolu_01...",
        "name": "uber_get_price_estimate",
        "input": { "start": "home", "end": "SFO" }
      }
    ],
    "tool_results": null
  },
  "raw_ref": { "traffic_log": "2026-04-19.jsonl#L4214", "request_body_hash": "sha256:..." },
  "signature": { "alg": "ed25519", "key_id": "proxy-2026-04-19", "sig": "..." }
}
```

### A.3 Assistant reply delivered via Telegram (channel stream, outbound)

```json
{
  "event_id": "evt_01HTTL...",
  "ts": "2026-04-19T03:14:26.203Z",
  "source": "proxy",
  "source_version": "v1",
  "stream": "channel",
  "agent_token_id": "cvis_abc123",
  "bridge_id": "7f953938-bd9f-43a2-a3e0-70bc2b3718c6",
  "conversation_id": "telegram:8536852960",
  "provider": "telegram",
  "direction": "outbound",
  "role": "assistant",
  "turn": {
    "text": "Checking Uber pricing for you now — one moment!",
    "tool_calls": null,
    "tool_results": null
  },
  "raw_ref": { "traffic_log": "2026-04-19.jsonl#L4215", "request_body_hash": "sha256:..." },
  "signature": { "alg": "ed25519", "key_id": "proxy-2026-04-19", "sig": "..." }
}
```

Note how events A.2 and A.3 both represent "the assistant's response" but from different streams. A.2 is the full LLM output including the tool call. A.3 is just the text the user actually saw (delivered verbatim to Telegram). Auto-approval reads A.3 (channel ground truth); incident review correlates A.2 + A.3 + the tool-call result.

---

## Appendix B: Comparison to alternatives considered

| Approach | Pros | Cons | Decision |
|---|---|---|---|
| Stay with plugin scavenger + harden further | Smallest code change | Doesn't solve tampering; ceiling on auto-approval trust | Rejected — we're at the ceiling |
| Require agent frameworks to emit signed transcripts | Clean attribution | Requires every framework to cooperate; glacial adoption | Rejected — doesn't work for Claude Code et al. |
| Filesystem hooks (watch session JSONL with kernel-level monitoring) | Less architectural disruption | Still tamperable if agent can modify before we see; doesn't scale to non-JSONL frameworks | Rejected — same root problem |
| Full TLS MITM (this proposal) | Solves tampering; framework-agnostic; architecture substrate for Stages 2+3 | CA cert install friction; per-provider parsers; separate process management | **Chosen** |
| eBPF-based traffic capture | No CA cert install | Linux-only, kernel-version-sensitive, operationally harder | Deferred — revisit if CA install becomes a real adoption blocker |

---

## Appendix C: Glossary

- **Agent** — the LLM-driven process making API calls (OpenClaw + skill, Claude Code, etc.).
- **Bridge** — a Clawvisor install identity, one per OpenClaw install. Identified by `cvisbr_…` token.
- **Agent identity** — per-agent identity within a bridge. Identified by `cvis_…` token.
- **Pair code** — one-shot bootstrap token (`cvpc_…`) that mints bridge + agent tokens.
- **Gateway token** — Kumo's term for per-agent proxy auth. In our model, the `cvis_…` agent token.
- **Channel stream** — user-facing message traffic (Telegram, Slack, webchat).
- **LLM stream** — LLM API traffic (Anthropic, OpenAI, etc.).
- **Action stream** — third-party tool API traffic (GitHub, Gmail, etc.).
- **TurnEvent** — unified structured event emitted by the proxy.
- **Sovereignty** (container sovereignty) — property that a process runs in an isolation domain the agent cannot violate.
