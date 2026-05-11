# Runtime Proxy

TLS-terminating MITM proxy (preview) that observes model API calls, intercepts tool-use, and holds inline approvals for AI agent sessions.

## Structure

```
server.go                  — Server struct, goproxy wiring, listener lifecycle, bounded TTL cache
manager.go                 — Manager: session creation, revocation, proxy URL/CA cert helpers
auth.go                    — Bearer/Basic auth, session lookup, Proxy-Authorization header parsing
state.go                   — RequestState, RuntimeRequestContext, SecretScanSummary
session_guard.go           — InstallSessionGuard: authenticates every request, attaches session to context
inbound_secret_runtime.go  — Inbound secret capture: scans request bodies, replaces credentials with placeholders via LLM adjudication
tooluse_runtime.go         — Tool-use interceptor: blocks/holds/approves tool calls in model responses, inline approval release
tooluse_stream.go          — Streaming SSE tool-use rewriting (Anthropic/OpenAI)
policy_hook.go             — Policy hooks: request classification, approval creation, context-judge integration
observe_notice.go          — Observe-mode notice injection/scrubbing into model conversations
request_context.go         — Request body parsing, conversation turn extraction, provider detection
session_settings.go        — Per-session settings (inline approval toggle, observation mode)
tool_defaults.go           — Session-scoped tool default matching (auto-allow known-safe tools)
timing_hooks.go            — Timing instrumentation for proxy request/response pipeline
timing_transport.go        — Custom http.RoundTripper with timing spans
log_redaction.go           — Response body redaction for secrets in outbound traffic
ca.go                      — MITM CA load-or-generate, cert storage on disk
certcache.go               — LeafCertCache: on-demand TLS leaf certs forged from MITM CA
edge_cert.go               — EdgeCertProvider: cloud-mode TLS cert from disk with hot reload
multitenant_cache.go       — Cross-session secret/verdict caches backed by Redis for multi-instance
redis_cache.go             — Redis shared cache client for secret verdicts and captured secrets
placeholder_runtime.go     — Placeholder lookup/swap for outbound response rewriting
runtime_events.go          — emitRuntimeEvent: SSE event emission for dashboard real-time updates
```

## Where to Look

| Task | File(s) | Notes |
|------|---------|-------|
| Add a new model provider surface | `request_context.go`, `internal/runtime/conversation/` | Add parser + register in DefaultRegistry |
| Change secret detection logic | `inbound_secret_runtime.go` | Known prefixes in `knownPrefixSpecs`, heuristic filters in `looksObviouslyNonSecret` |
| Change tool-use blocking/approval flow | `tooluse_runtime.go`, `tooluse_stream.go` | Evaluator function, held approval release, synthetic responses |
| Change policy rule matching | `policy_hook.go`, `internal/runtime/policy/` | RuntimePolicyRule CRUD, context judge integration |
| Add observe-mode behavior | `observe_notice.go` | Notice injection into model responses, scrubbing on replay |
| Change session auth | `auth.go` | Bearer/Basic extraction, session validation |
| Change CA/cert behavior | `ca.go`, `certcache.go`, `edge_cert.go` | MITM CA generation, leaf cert caching, cloud-mode edge certs |
| Add timing instrumentation | `timing_hooks.go`, `timing_transport.go` | Span recording, file sink |
| Change inline approval UX | `tooluse_runtime.go`, `session_settings.go` | `sessionInlineApprovalEnabled`, approval reply parsing |
| Debug secret adjudication | `inbound_secret_runtime.go` | Set `CLAWVISOR_RUNTIME_PROXY_ADJUDICATION_DEBUG_DIR` (owner-only, never in prod) |

## Conventions

- goproxy handler chain: `InstallSessionGuard` → `InstallInboundSecretCapture` → `InstallToolUseInterceptors` → `InstallObserveNoticeRequestScrubber`. Order matters.
- `internalBypassHeader` (`X-Clawvisor-Internal-Bypass`) skips all interceptors for synthetic responses.
- Observation mode (`session.ObservationMode`): all tool calls pass through, events logged as `would_review`/`would_deny` instead of blocking.
- Secret adjudication uses LLM with bounded concurrency (8 goroutines) and a two-level cache (in-process `sync.Map` + Redis for multi-instance).
- Tool-use decisions: policy rules → session-scoped defaults → task matching → context judge → held approval.
- Streaming responses are rewritten in-place via SSE; non-streaming via buffered body rewrite.
- `RequestState` is stored on `goproxy.ProxyCtx.UserData` and threaded through the handler chain.

## Anti-Patterns

- NEVER log raw secret values, candidate strings, or full request/response bodies outside the adjudication debug dir.
- NEVER skip the session guard. Every request must authenticate before any interceptor runs.
- NEVER emit `null` or `[]` for `missing_chain_values` in intent extraction (handled upstream, but proxy must not reintroduce).
- NEVER truncate `fact_value` in chain context — drop the fact instead.
- NEVER set `CLAWVISOR_RUNTIME_PROXY_ADJUDICATION_DEBUG_DIR` in production. It writes plaintext candidate values to disk.
- NEVER assume a single proxy instance. Secret verdicts and captured secrets must go through the shared Redis cache for correctness.
- NEVER modify the goproxy handler order. Session guard must run first.