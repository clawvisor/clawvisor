# Design Doc: Chain Context for Intent Verification

## Problem

The intent verifier is stateless — it sees one request at a time. For multi-step tasks (e.g., "read inbox then reply to urgent emails"), the verifier can't validate that a follow-up request is consistent with what the previous step actually returned. A compromised agent could read the inbox, then email an unrelated address, and the verifier wouldn't catch it.

## Constraints

- **No raw result storage.** Clawvisor should not store API response data (email bodies, file contents). The gateway's role is credential injection and audit, not data warehousing.
- **LLM trust boundary already exists.** Request bodies (which can contain sensitive data) are already sent to the LLM provider for verification, so sending results to an extraction LLM is not a new exposure.
- **Verify and extract can't be combined.** Verification must complete *before* execution (which has side effects), and extraction needs the result *after* execution. These are necessarily separate LLM calls.
- **Zero overhead for the common case.** Single-action tasks and terminal actions shouldn't pay any latency or cost penalty.

## Design

### Core Idea

The verification LLM already reasons about the full task scope (all authorized actions). It can determine whether the current action's result is likely to inform downstream actions. A new boolean field `extract_context` in the verification verdict signals this.

When `extract_context` is true, a separate LLM call runs *after* execution to extract compact structural facts (IDs, addresses, phone numbers, amounts — never content) from the result. These facts are stored and fed into subsequent verification prompts for the same task.

### Session Scoping

Chain context is scoped by an optional `session_id` that the agent provides on each request. Facts are keyed on `(task_id, session_id)`, so separate invocations of the same standing task maintain independent chain contexts.

If no `session_id` is provided, chain context is disabled for that request: no facts are loaded, and no extraction is triggered. This keeps existing behavior unchanged for agents that don't opt in.

There is no explicit session lifecycle (no open/close calls). The agent sends a consistent `session_id` across related requests and stops sending it when the flow is done. Facts accumulate up to the per-task cap (50), so an agent reusing a stale session ID only hurts its own verification accuracy. No cleanup job is needed.

### Request Lifecycle (Updated)

```
1. Agent sends request (with optional session_id)
2. Gateway authenticates, checks restrictions, checks task scope
3. If session_id: load chain facts for (task, session) from DB  ← NEW
4. Verify intent (LLM call #1, includes chain facts if present)
   → returns {allow, param_scope, reason_coherence, extract_context}
5. Execute adapter (side effects happen here)
6. Log audit entry, return result to agent
7. If session_id && extract_context: extract facts (LLM call #2)  ← NEW
   → store facts, available for step 3 on next request
```

Steps 3 and 7 are the only additions. Step 7 runs after the response is returned to the agent, but extraction must complete before the next request in the same session can load facts. If the agent sends a follow-up before extraction finishes, that request proceeds without the pending facts. This is acceptable: chain context is a best-effort enhancement, and fast-polling agents degrade gracefully to the existing stateless verification.

**Cost note:** Every non-terminal read step with a session becomes two LLM calls (verify + extract). For scan-heavy tasks, this can meaningfully increase LLM cost.

### What the Verifier Signals

The verification verdict gains one field:

```go
type VerificationVerdict struct {
    Allow           bool   `json:"allow"`
    ParamScope      string `json:"param_scope"`
    ReasonCoherence string `json:"reason_coherence"`
    ExtractContext  bool   `json:"extract_context"`   // NEW
    Explanation     string `json:"explanation"`
    // ... Model, LatencyMS, Cached unchanged
}
```

The system prompt instructs the LLM to set `extract_context: true` only when:
- The action reads/retrieves data (not a terminal write/send)
- The task scope includes downstream actions that would plausibly reference entities from this result
- The task involves multiple steps where data flows between actions

Default is `false` (Go zero value), so if the LLM omits it, nothing happens. Fail-safe.

### What Gets Extracted

The extractor LLM sees the task purpose, all authorized actions, the action that just executed, and the result. It produces a JSON array of facts:

```json
[
  {"fact_type": "email_address", "fact_value": "alice@company.com"},
  {"fact_type": "message_id", "fact_value": "msg-abc123"},
  {"fact_type": "phone_number", "fact_value": "+15551234567"}
]
```

The extraction prompt is aggressive about minimization:
- Extract only structural references (IDs, addresses, names, amounts, dates)
- Never extract message bodies, file contents, passwords, or tokens
- Cap at 20 facts per extraction
- Result payload is truncated to 4KB before sending to the LLM

After the LLM returns facts, each `fact_value` is validated by substring match against the raw result. Any fact whose value doesn't appear in the result is dropped. This catches LLM hallucination: a fabricated email address or ID that wasn't in the actual response can't make it into the chain context. Case-insensitive matching is used for fact types like `email_address` and `domain`; all others use exact match.

This is the key advantage over hardcoded per-adapter extractors: the LLM understands the task context. If the task is "find phone numbers in this email and text them," the extractor pulls phone numbers from the email result — something no static extractor would anticipate.

### How Facts Are Used in Verification

The verification request gains a new field:

```go
type VerifyRequest struct {
    // ... existing fields unchanged
    ChainFacts []ChainFact  // NEW
}
```

When chain facts exist, they appear in the verification prompt as a markdown table:

```
Chain context (facts extracted from prior actions in this task):

| Service | Action | Fact Type | Value |
|---------|--------|-----------|-------|
| google.gmail | list_messages | email_address | alice@company.com |
| google.gmail | list_messages | message_id | msg-abc123 |
```

The system prompt instructs the verifier: if chain context is provided and the current request targets an entity (email address, file ID, etc.), check whether that entity appeared in the chain context. If the agent is emailing `eve@company.com` but the inbox results only contained messages from `alice@company.com` and `bob@company.com`, that's a `param_scope: "violation"`.

Task purpose and expected use take precedence — if the task explicitly names a target, chain context doesn't override that.

### Storage

New `chain_facts` table:

```sql
CREATE TABLE chain_facts (
    id          TEXT PRIMARY KEY,
    task_id     TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    session_id  TEXT NOT NULL,
    audit_id    TEXT NOT NULL REFERENCES audit_log(id) ON DELETE CASCADE,
    service     TEXT NOT NULL,
    action      TEXT NOT NULL,
    fact_type   TEXT NOT NULL,
    fact_value  TEXT NOT NULL,
    created_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX idx_chain_facts_task_session ON chain_facts(task_id, session_id);
```

Facts are tied to the task + session (for retrieval) and the audit entry (for traceability). When a task is deleted, its facts cascade-delete.

#### Fact Cleanup

Chain facts are deleted when the associated task reaches a terminal status (`completed`, `expired`, `revoked`, `denied`). This happens inline in the task status transition (e.g., `UpdateTaskStatus` or the completion/revoke handlers). Facts serve no purpose after the task is done, and retaining them would accumulate PII-adjacent data (email addresses, phone numbers, IDs) unnecessarily.

As a safety net, a future TTL-based cleanup job will periodically scan for expired tasks, mark them as `expired` in the database, and delete their associated chain facts. This covers edge cases where a task's expiry time passes without an explicit status transition (e.g., the server was down when the task expired).

Store interface additions:

```go
SaveChainFacts(ctx context.Context, facts []*ChainFact) error
ListChainFacts(ctx context.Context, taskID, sessionID string, limit int) ([]*ChainFact, error)
DeleteChainFactsByTask(ctx context.Context, taskID string) error
```

`ListChainFacts` returns facts ordered by `created_at ASC`, capped at 50 to prevent unbounded prompt growth.

### Cache Key Update

The verification cache key must include chain facts, since the same request with different chain context should produce different verdicts:

```
taskID|service|action|sha256(params)[:8]|sha256(reason)[:8]|sha256(chainFacts)[:8]
```

When no `session_id` is provided (no chain facts), the key is unchanged from today.

### Wiring

The `LLMExtractor` reuses the same `VerificationConfig` (same LLM provider, model, API key) — no new config needed. It's constructed alongside the verifier in `internal/api/server.go` and passed to the gateway handler.

```go
var extractor intent.Extractor = intent.NoopExtractor{}
if cfg.Verification.Enabled {
    extractor = intent.NewLLMExtractor(cfg.Verification, logger)
}
```

The guard handler (`guard.go`) also loads chain facts for verification but does not perform extraction (it doesn't execute adapters).

### Extraction Error Handling

If the extraction LLM call fails (timeout, malformed JSON, rate limit), the failure is logged and the facts are silently dropped. The next request proceeds without those facts, falling back to stateless verification. This matches the fail-safe principle: chain context improves verification but is never required for it.

## Files Changed

| File | Change |
|------|--------|
| `internal/intent/verifier.go` | Add `ExtractContext` to verdict, `ChainFacts` to request, `ChainFact` type |
| `internal/intent/prompts.go` | Update system prompt (extract_context output + chain context input), update user message builder to render chain facts as markdown table |
| `internal/intent/extractor.go` | **New file.** `Extractor` interface, `LLMExtractor`, `NoopExtractor`, extraction prompt, response parser, substring validation |
| `internal/intent/cache.go` | Include chain facts hash in cache key (omitted when no session) |
| `pkg/store/store.go` | Add `ChainFact` model, `SaveChainFacts`, `ListChainFacts`, `DeleteChainFactsByTask` to Store interface |
| `pkg/store/sqlite/store.go` | Implement `SaveChainFacts`, `ListChainFacts`, `DeleteChainFactsByTask` |
| `pkg/store/postgres/store.go` | Implement `SaveChainFacts`, `ListChainFacts`, `DeleteChainFactsByTask` |
| `pkg/store/sqlite/migrations/012_chain_facts.sql` | **New file.** Create `chain_facts` table (with `session_id` column) |
| `pkg/store/postgres/migrations/012_chain_facts.sql` | **New file.** Create `chain_facts` table (with `session_id` column) |
| `internal/api/handlers/gateway.go` | Accept `session_id` from request, load chain facts before verify, extraction after execute, substring validation |
| `internal/api/handlers/guard.go` | Accept `session_id` from request, load chain facts before verify |
| `internal/api/handlers/tasks.go` | Delete chain facts on task completion/revocation |
| `internal/api/server.go` | Construct extractor, pass to gateway handler |
| `README.md` | Document chain context feature |
| `skills/clawvisor/SKILL.md` | Teach agent how to pass `session_id` for multi-step flows |

## What This Doesn't Cover (Future Work)

- **Expired task cleanup job.** A background goroutine that periodically scans for tasks past their `expires_at`, transitions them to `expired` status, and deletes their chain facts. Currently expired tasks are only caught when accessed. This job would ensure timely cleanup of both task state and associated chain facts.
- **Approval flow extraction.** When a pending approval is executed via the approvals handler, that result could also be extracted. Deferred because approved requests already went through human review.
- **Eval coverage.** The eval suite will need new chain-context test cases. These require a different testing approach since the current eval runner is single-request. A multi-step eval harness would be a separate effort.
