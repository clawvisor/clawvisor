# Design Doc: Scope-Drift Continuation Menu

## Problem

Today, when an agent emits a tool call that falls outside the active task's authorized scope, Clawvisor's `TaskScopeEvaluator` returns `OutcomeHold` and the proxy surfaces an inline approval prompt to the user. The user is the only decision-maker: they say `yes`/`no`/`task` to grant the one call, deny it, or trigger an inline task-creation flow.

That puts every drift in front of the user as a binary, even when the agent could have handled it locally:

- The agent often knows the call is genuinely a new task and should POST `/control/tasks`. Asking the user "approve this one-off?" wastes their attention and misclassifies the work.
- The agent often knows the call is an obvious continuation of the active task's stated purpose and should `expand`. Same problem.
- The agent sometimes recognizes mid-thought that the call wasn't actually needed (the user clarified, the previous step returned what was needed, etc.). Today the only paths are "interrupt the user for approval" or "let the block stand and recover awkwardly."

We want the block to flow back to the **agent** first as a continuation. The agent has three explicit recovery options or can fall through implicitly to a different next action; only one of the explicit options surfaces to the user.

## Goals

- Turn a blocked tool call into an agent-side decision point with three explicit recovery options plus an implicit "do something else" fall-through.
- Two of the explicit options (new task, expand) surface to the user only as the existing inline task / expansion approval — not as "approve this drift?" prompts.
- The third explicit option (one-off approval) surfaces to the user with the agent's rationale attached.
- The implicit fall-through is silent: the agent takes a different next action and the drift expires via TTL.
- Keep one-shot enforcement: the agent cannot try multiple explicit options against the same block.
- Cache stability: the menu must not be reflected back into already-cached message turns. Drift records and menu text live in the live response only.
- Reuse existing primitives (`continuation.BuildContinuationBody`, `TaskScopeEvaluator`, the expand endpoint, `cvreason` intent verification) wherever possible.

## Non-Goals

- Replacing the existing user approval flow for hardcoded approval requirements (Layer 2 in `docs/ARCHITECTURE.md`). Those remain user-facing.
- Per-block telemetry redesign — initial cut reuses existing audit fact shapes with a `drift_id` field.
- Adding the `justify` option in v1. Reserved as a future addition (see "Open questions").

## Prior Art

The `broadleaf-statistic` workspace (`internal/runtime/llmproxy/scope_drift_*.go`) ships a near-complete implementation of this concept under the term **scope drift**. Its menu has four outcomes:

| broadleaf option | shape |
|---|---|
| (a) **expand** | `POST /control/tasks/<id>/expand` |
| (b) **new_task** | `POST /control/tasks?surface=inline` |
| (c) **one_off** | `<clawvisor:decision drift="..." option="one-off">rationale</clawvisor:decision>` |
| (d) **justify** | `<clawvisor:decision drift="..." option="justify">why this fits</clawvisor:decision>` re-runs the intent verifier with the justification threaded in |

Differences from what we're building here:

1. **No discard markup; implicit fall-through instead.** broadleaf's fourth option is `justify` (re-run the intent verifier with the agent's justification threaded in). The user's spec replaced that with `discard` (agent abandons the call). On reflection, neither needs to be an explicit menu option in v1: the agent can simply take a different next action and let the drift expire. The menu surfaces three explicit options (expand, new_task, one_off) and explains the fourth path in prose — "if none of these fit, do something else; the drift will not retry the blocked call on its own." See "Open questions" for whether to add `justify` later as a verifier-replay path.
2. **Source-aware behavior.** broadleaf distinguishes `task_scope` and `intent_verification` as block sources and lets `justify` re-run only the intent verifier. We can keep the source distinction for telemetry without supporting `justify` initially.
3. **In-band markup.** broadleaf encodes (c)/(d) as `<clawvisor:decision>` XML in the agent's assistant text, parsed server-side in the SSE postproc. Under this design only one-off uses markup; expand and new_task are normal control-plane POSTs, and the implicit fall-through has no markup at all.
4. **One-shot via registry.** broadleaf's `ScopeDriftRegistry.ClaimOption` enforces that a drift_id resolves exactly once. We adopt this directly. The implicit fall-through never calls `ClaimOption`; the drift just expires.
5. **Pre-clear retry.** On a successful resolution, broadleaf mints a one-shot pre-clear keyed by `(agent, fingerprint(tool_use))`. The agent re-emits the original tool call unchanged and it passes scope + intent verification once. We adopt this directly.

This design treats broadleaf as the starting point and lists the deltas explicitly rather than re-deriving each piece.

## Design

### The recovery options

A blocked tool call mints a `ScopeDrift` record. The proxy substitutes the tool call's result with a menu prompt containing the drift_id and three explicit recovery options:

| option | who decides | how it's expressed |
|---|---|---|
| (a) **expand** | user (inline approval prompt) | `POST /control/tasks/<id>/expand?surface=inline` |
| (b) **new_task** | user (inline approval prompt) | `POST /control/tasks?surface=inline` |
| (c) **one_off** | user (inline approval prompt with agent rationale) | `<clawvisor:decision drift="..." option="one-off">rationale</clawvisor:decision>` |

All three surface a single approval prompt to the user — but the agent has already classified the work, so the prompt is much more specific than "approve this one-off?".

Implicitly, there is a fourth path: the agent picks none of the above and does something else (asks the user a question, calls a different tool that's in scope, summarizes and stops). The menu names this path in prose and tells the agent that the proxy will NOT retry the blocked call on its own — the drift simply expires via TTL. No markup is required to take this path; the agent just emits its next turn normally.

### How the agent sees the block

When `TaskScopeEvaluator` or `IntentVerifyEvaluator` returns `OutcomeHold` for an out-of-scope tool_use:

1. The proxy registers a `ScopeDrift` record (id, agent, conversation, original `ToolUse`, active task at block time, source, reason).
2. Instead of substituting the conventional approval prompt, the proxy substitutes the **continuation body** built by `BuildContinuationBody`: the assistant's tool_use is preserved, and a synthetic tool_result with the menu text is appended.
3. The agent's next turn begins from a state where the menu is its most recent input. The menu carries the drift_id verbatim and tells the agent: "pick exactly one option. Each drift_id resolves exactly once."

This is a behavioral change from today: today the agent sees the user-role approval prompt and is taught to interpret it. Under this design, the agent sees a tool_result and treats the block as a structured continuation. The user-facing approval prompts still exist, but they are downstream of the agent's classification.

### Per-outcome resolution

**(a) expand and (b) new_task.** The agent issues a normal control-plane POST. Both endpoints already exist; both already accept `?surface=inline` for inline user approval. The handler tags the resulting approval record with the originating `drift_id`, and on user approval the drift's outcome is set to `succeeded` and a pre-clear is minted for `(agent, fingerprint(original tool_use))`. On user denial, outcome is `denied`. The agent re-emits the original tool call unchanged; the pre-clear lets it through once.

**(c) one_off.** The agent emits `<clawvisor:decision drift="..." option="one-off">rationale</clawvisor:decision>` in its assistant text. The proxy's SSE postproc (see `scope_drift_resolve.go` in broadleaf) parses the markup, calls `ClaimOption(driftID, "one_off")` on the registry, and substitutes the markup with a confirmation marker. Downstream, the user sees an approval prompt that surfaces the original tool, the block reason, AND the agent's rationale verbatim. On approve, drift outcome → `succeeded`, pre-clear minted, agent re-emits. On deny, drift outcome → `denied`, drift closed.

**Implicit fall-through.** The agent does nothing drift-shaped — no POST, no markup — and just emits its next turn. `ClaimOption` is never called. The drift sits in the registry until TTL expires, at which point it's evicted. No pre-clear is ever minted for this drift_id, so if the agent (or a future turn) re-emits the same blocked tool call, it gets a fresh block and a fresh menu — no terminal-deny lock-in. The agent is told in the menu prompt that this is a valid path, so picking it isn't an error condition.

### One-shot enforcement and cache stability

The `ScopeDriftRegistry.ClaimOption` enforces one resolution per drift_id (broadleaf's pattern). A second `<clawvisor:decision>` markup against the same drift_id is rejected with `ErrDriftAlreadyResolved` and the markup is replaced with an error marker the agent can read on its next turn. The implicit fall-through doesn't go through `ClaimOption`, so it doesn't compete with this guard — it just means no option was ever claimed and the drift expires unresolved.

The drift_id is opaque and minted per-call. It does NOT appear in any prior cached turn — it lives only in the live response. The substituted markers (one-off awaiting approval, etc.) are stable text that does not leak the drift_id, so cache fills are not invalidated by drift state. This matches the existing `feedback_reconstruction_immutability` constraint (see `MEMORY.md`).

### Pre-clear retry

On `succeeded`, the registry mints a one-shot pre-clear keyed by `(agent_id, fingerprint(tool_use))`. `fingerprint` includes conversation, tool/route shape, AND a hash of the input bytes, so a successful drift on one call cannot bypass scope checks for a sibling call on the same endpoint with different params. The agent re-emits the original tool call unchanged; `taskscope.go` checks pre-clears first and lets the call through once before consuming the entry. This is broadleaf's pattern, adopted unchanged.

The implicit fall-through and any `denied` outcome do not mint a pre-clear. If the agent later re-emits the same blocked call, a fresh drift is minted and a fresh menu rendered — the prior block leaves no residue.

### Lifecycle

```
   tool_use lands
        │
        ▼
TaskScopeEvaluator / IntentVerifyEvaluator → OutcomeHold
        │
        ▼
Mint ScopeDrift{id, agent, tool_use, task_at_block, source, reason}
        │
        ▼
BuildContinuationBody appends tool_result with menu text → response to agent
        │
        ▼
agent picks one of:
   ├── (a) POST /control/tasks/<id>/expand?surface=inline ──┐
   ├── (b) POST /control/tasks?surface=inline ──────────────├──→ ClaimOption
   ├── (c) <clawvisor:decision option="one-off">...         ┘
   │
   └── (implicit) agent does something else; ClaimOption never called.
                  Drift TTL-expires. No pre-clear. Re-emit later = fresh drift.
        │
        ▼
user approves/denies the (a)/(b)/(c) prompt
        │
        ▼
Outcome=Succeeded/Denied
        │
      success
        │
        ▼
Mint pre-clear (agent_id, fingerprint(tool_use))
        │
        ▼
agent re-emits original tool_use → pre-clear consumed → call passes
```

### Source-aware behavior

The `ScopeDriftSource` enum from broadleaf is preserved:

- `task_scope` — `TaskScopeEvaluator` denied the call.
- `intent_verification` — task scope passed but `IntentVerifyEvaluator` rejected.

The menu rendering and the three explicit options do not vary by source. The distinction shows up in:

- Audit/telemetry (`scope_drift.minted{source=...}`).
- The reason text the menu surfaces ("Reason: <intent verifier said X>" vs "Reason: <no authorized action covered service.action>").
- Future work: bringing back `justify` to re-run only the intent verifier would gate on this source — see "Open questions."

### Integration points

1. **`internal/runtime/llmproxy/taskscope.go`**: extend `Check()` to consult the pre-clear table before evaluating scope. Pre-clears live in a new `ScopeDriftRegistry` interface.
2. **`internal/runtime/llmproxy/policies/task_scope_evaluator.go`** and the intent-verify evaluator: on `OutcomeHold` for an out-of-scope call, mint a drift and route through the new menu instead of the existing approval prompt. Hardcoded approval requirements (Layer 2) still use the existing flow.
3. **`internal/runtime/llmproxy/continuation.go`**: reused as-is. The menu text is just another synthetic tool_result.
4. **`internal/api/handlers/tasks.go`**: `Expand()` and `Create()` accept an optional `drift_id` body field. On user approval, the handler calls `registry.SetOutcome(driftID, Succeeded)`. On denial, `SetOutcome(driftID, Denied)`. No-op when `drift_id` is empty (existing flows unchanged).
5. **`internal/runtime/llmproxy/controltool/control.go`**: system prompt gains a "SCOPE-DRIFT MENU" section that teaches the agent the three explicit options, the one-shot rule, AND that the implicit fall-through is a valid choice (not an error). The existing SCOPE DRIFT section in `control.go` already establishes terminology and the expand-vs-new-task heuristic; the menu section sits underneath it as the runtime contract.
6. **New files (mirroring broadleaf)**: `scope_drift_registry.go`, `scope_drift_prompt.go`, `scope_drift_resolve.go`, `scope_drift_one_off_prompt.go`, `scope_drift_reply.go` and their tests. No discard-specific files — the implicit fall-through is "the absence of any of these calls."

### Telemetry

Reuse broadleaf's counter shapes:

- `scope_drift.minted{source}`
- `scope_drift.option_chosen.{expand,new_task,one_off}`
- `scope_drift.outcome.{succeeded,denied}`
- `scope_drift.pre_clear_consumed`
- `scope_drift.expired_unresolved` — new; logged when TTL evicts a drift that never had an option claimed. This is the only telemetry signal for the implicit fall-through, and it conflates "agent classified as unnecessary" with "agent stalled / got distracted." We accept that ambiguity in v1.

## Decisions

1. **Drift TTL = 10 minutes, uniform.** The TTL bounds how long the registry holds a drift record in memory across three lifecycle phases:

   - **mint → ClaimOption** (agent reads menu and picks an explicit option). Usually a few seconds.
   - **ClaimOption → SetOutcome** (proxy waits for user inline approval after (a)/(b)/(c)). Can be minutes — the user may step away from the chat.
   - **SetOutcome(succeeded) → pre-clear consumption** (agent re-emits the original tool call). Usually one turn.

   broadleaf's 60s is sized for phase 1 only and is too short for phase 2: if the user lingers on the approval prompt past TTL, the drift expires, the pre-clear is never minted, and the work is lost. 10 minutes comfortably absorbs slow human-in-the-loop turns without letting stale records live forever. The TTL does NOT govern menu visibility — the menu lives in the continuation body for the conversation's lifetime; the TTL only matters for `ClaimOption` and `LookupPreClear` calls.

   The implicit fall-through is unaffected: it never calls `ClaimOption`, so it just rides the same 10-minute timer and emits `expired_unresolved` when evicted.

2. **Rollout: new menu activates for all live sessions on deploy.** The existing approval flow remains compiled-in as a fallback for:
   - Hardcoded approval requirements (Layer 2 in `docs/ARCHITECTURE.md`) — those are not scope drift and stay user-facing.
   - Defensive cases where a block somehow mints no drift record (registry unreachable, malformed evaluator output, etc.).

   For ordinary task-scope and intent-verification blocks on active sessions, the new menu takes over immediately — no opt-in flag, no per-session migration. Pre-deploy in-flight approval prompts predating the rollout retain their old behavior (user types `yes`/`no`/`task` as before) because they predate any drift record; new blocks land on the menu.

## Open Questions

1. **`justify` as a future addition.** Not in v1, but reserved for later. If telemetry shows recurrent false-positive `intent_verification`-sourced blocks where the agent has no good recovery (the call IS in the task's spirit but the verifier disagrees), we add a fourth explicit option: `<clawvisor:decision drift="..." option="justify">why this fits</clawvisor:decision>` re-runs the intent verifier with the agent's justification threaded in, accepting or rejecting on the second pass. broadleaf's implementation is the reference. Until then, the agent's only paths for an intent-verifier false positive are (b) new_task (wrong frame), (c) one_off (works but interrupts the user), or implicit fall-through (loses the work).

## Out of Scope

- Sliding back into "let the verifier re-run" without `justify` markup. Out-of-band verifier replay is risky and underspecified.
- Cross-agent drift sharing. Drifts are scoped to a single conversation; a sibling agent on the same task never sees them.
- Cross-conversation pre-clears. Fingerprint includes conversation_id, so a drift resolved in one conversation does NOT pre-clear the same call in another.
- Auto-classification ("Clawvisor predicts this is a new_task and just routes the agent there"). The agent classifies; the proxy enforces.

## Prior-Art Pointers

- broadleaf design (architecture layer): `broadleaf-statistic/docs/ARCHITECTURE.md` (Layer 3 description, `pending_scope_expansion`).
- broadleaf scope-drift implementation: `broadleaf-statistic/internal/runtime/llmproxy/scope_drift_*.go`.
- broadleaf inline state machine (predecessor design): `broadleaf-statistic/internal/runtime/llmproxy/inline_task_state_machine.md`.
- broadleaf telemetry: `broadleaf-statistic/internal/e2e/lite/scope_drift_counters.go`.
- This workspace's continuation primitive: `internal/runtime/llmproxy/continuation.go` (`BuildContinuationBody`, `PrependAssistantNotice`).
- This workspace's expand endpoint: `internal/api/handlers/tasks.go` (`TasksHandler.Expand`, `ApproveInlineExpansion`).
- This workspace's existing control-plane system prompt: `internal/runtime/llmproxy/controltool/control.go` (the SCOPE DRIFT and EXPAND vs NEW TASK sections are the user-facing baseline for the new menu language).
