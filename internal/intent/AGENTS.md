# internal/intent — LLM Intent Verification

## OVERVIEW
Intent verification checks that gateway request params match the approved task purpose. Chain context extraction pulls structural facts (IDs, emails, phones) from adapter results so subsequent requests can be verified against what the agent actually saw, not just what it claimed.

## STRUCTURE
```
verifier.go       — LLMVerifier, NoopVerifier, VerifyRequest/VerificationVerdict types
extractor.go      — Extractor interface, builtin regex + LLM two-phase extraction (912 lines)
prompts.go        — System/user prompt templates for verification and extraction
approval.go       — Group-chat approval check (LLM reads messages for pre-approval signals)
cache.go          — In-memory verdict cache with TTL
cache_iface.go    — VerdictCacher interface (in-memory + Redis impls)
cache_redis.go    — Redis-backed verdict cache for multi-instance deployments
testdata/
  eval_cases.json         — 215 intent verification eval cases
  extract_eval_cases.json — 34 chain context extraction eval cases
```

## WHERE TO LOOK
| Task | File | Notes |
|------|------|-------|
| Change verification prompt logic | `prompts.go` | System prompt, extraction prompt, approval prompt |
| Add a new chain fact type | `extractor.go` | Builtin regex patterns near top, then LLM extraction |
| Change verdict structure | `verifier.go` | `VerificationVerdict` struct |
| Change caching behavior | `cache.go`, `cache_redis.go` | TTL, eviction, Redis key format |
| Add eval cases | `testdata/eval_cases.json` | Run `make eval-intent` after |
| Change group-chat approval | `approval.go` | `CheckApproval` + prompt |

## CONVENTIONS
- Two-phase extraction: `ExtractBuiltins` (regex, sub-ms) runs synchronously in the gateway path; `ExtractLLM` runs async and appends facts afterward.
- `maxExtractedFacts = 500` per call — generous to avoid truncating list results; facts are deleted on task completion.
- `maxExtractResultLen = 4096` — adapter results longer than this are truncated before LLM extraction.
- Verdict cache keyed by (task_id, service, action, params hash). In-memory by default; Redis when `CLAWVISOR_REDIS_URL` is set.
- Gemini context caching: `LLMVerifier` supports `SetGeminiCacheNameFn` / `StartGeminiCache` to reuse cached system prompts across calls.
- `Lenient` flag on `VerifyRequest` switches to a softer prompt that gives the agent benefit of the doubt (used for standing tasks without session_id).

## ANTI-PATTERNS
- NEVER emit `null` or `[]` for `missing_chain_values` — omit the field or return an empty object.
- NEVER extract JSON key names as `fact_value` — extract the actual value the key maps to.
- NEVER truncate `fact_value` — if it exceeds length limits, drop the entire fact rather than cropping it.
- NEVER extract sensitive content as chain facts: OTPs, PINs, banking secrets, auth tokens.
- Dates in extracted facts must be ISO format (YYYY-MM-DD or RFC 3339). NEVER emit bare time-of-day without a date.
- Do not add new builtin regex patterns without corresponding eval cases.
- Do not modify prompts without running `make eval-intent` to check for regressions.