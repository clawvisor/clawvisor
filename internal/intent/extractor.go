package intent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/clawvisor/clawvisor/internal/llm"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// maxExtractResultLen is the maximum length of the adapter result passed to the LLM.
const maxExtractResultLen = 4096

// maxExtractedFacts caps the number of facts persisted per extraction call.
// Set generously so a single list response (typically 50–200 items) doesn't
// have its tail truncated — that was a major source of chain-context misses.
// chain_facts rows are deleted on task completion, so per-task storage is
// bounded by the task's lifetime, not the cap.
const maxExtractedFacts = 500

// Extractor extracts structural chain facts from adapter results.
//
// Implementations support two-phase extraction so the gateway can persist
// builtin-regex facts immediately (sub-millisecond) and then add LLM-derived
// facts asynchronously. Extract is retained for callers that want both
// phases combined; gateway code should prefer ExtractBuiltins + ExtractLLM
// to minimize the window where downstream verifications race the LLM call.
type Extractor interface {
	// ExtractBuiltins runs only the builtin regex patterns against the full
	// result. Synchronous and fast (~ms). Safe to save the result immediately.
	ExtractBuiltins(req ExtractRequest) []*store.ChainFact

	// ExtractLLM calls the LLM extractor and returns facts beyond what was
	// already captured. `existing` is typically the output of ExtractBuiltins
	// for the same request and is used to dedupe so the returned slice
	// contains only NEW facts.
	ExtractLLM(ctx context.Context, req ExtractRequest, existing []*store.ChainFact) ([]*store.ChainFact, error)

	// Extract is the convenience combined call: ExtractBuiltins + ExtractLLM.
	Extract(ctx context.Context, req ExtractRequest) ([]*store.ChainFact, error)
}

// ExtractRequest contains the data needed for chain context extraction.
type ExtractRequest struct {
	TaskPurpose       string
	AuthorizedActions []store.TaskAction
	Service           string
	Action            string
	Result            string // raw adapter result (full; truncated internally for LLM, regexes run against full)
	TaskID            string
	SessionID         string
	AuditID           string
}

// NoopExtractor returns nil (extraction not configured).
type NoopExtractor struct{}

func (NoopExtractor) ExtractBuiltins(_ ExtractRequest) []*store.ChainFact { return nil }

func (NoopExtractor) ExtractLLM(_ context.Context, _ ExtractRequest, _ []*store.ChainFact) ([]*store.ChainFact, error) {
	return nil, nil
}

func (NoopExtractor) Extract(_ context.Context, _ ExtractRequest) ([]*store.ChainFact, error) {
	return nil, nil
}

// LLMExtractor uses an LLM to extract structural facts from adapter results.
type LLMExtractor struct {
	health *llm.Health
	logger *slog.Logger

	// geminiCacheNameFn returns the current Gemini cachedContents resource
	// name, or "" when no cache is registered. Set via StartGeminiCache.
	// Attached to every per-call llm.Client so the cache is referenced on
	// Gemini provider requests.
	geminiCacheNameFn func() string
	// geminiCacheMgr owns the cache lifecycle when StartGeminiCache was
	// used. Nil when no cache was set up.
	geminiCacheMgr *llm.GeminiCacheManager
}

// NewLLMExtractor creates an LLM-backed chain context extractor.
func NewLLMExtractor(health *llm.Health, logger *slog.Logger) *LLMExtractor {
	return &LLMExtractor{health: health, logger: logger}
}

// StartGeminiCache initializes the Gemini explicit context cache for the
// extractor's system prompt and registers it so per-request clients
// reference it automatically. cfg.SystemPrompt is filled in by the
// extractor and should be left empty by callers. On creation failure the
// extractor proceeds without caching (slower, but functional).
func (e *LLMExtractor) StartGeminiCache(ctx context.Context, cfg llm.GeminiCacheManagerConfig) error {
	cfg.SystemPrompt = extractionSystemPrompt
	if cfg.Logger == nil {
		cfg.Logger = e.logger
	}
	mgr, err := llm.NewGeminiCacheManager(cfg)
	if err != nil {
		return fmt.Errorf("extractor gemini cache: %w", err)
	}
	if err := mgr.Start(ctx); err != nil {
		return fmt.Errorf("extractor gemini cache start: %w", err)
	}
	e.geminiCacheMgr = mgr
	e.geminiCacheNameFn = mgr.CacheName
	return nil
}

// StopGeminiCache deletes the cache and stops the refresh goroutine.
// Safe to call when no cache is running.
func (e *LLMExtractor) StopGeminiCache(ctx context.Context) {
	if e.geminiCacheMgr != nil {
		e.geminiCacheMgr.Stop(ctx)
	}
}

const extractionSystemPrompt = `You extract structural references from API results for a security system.
These references become "chain context" — facts that future requests in
the same task can reference. The chain-context check uses these facts to
verify that an agent targeting an entity (a message_id, file_id, email,
etc.) is operating within scope of what was previously discovered.

Two extraction tracks run on every result:
  1. "facts": up to 50 entity values you observe directly in the
     truncated view of the result.
  2. "patterns": Go/RE2 regex patterns (one capture group) that we run
     against the FULL untruncated result to catch entities the
     truncation cut off.
Both tracks are required for every non-empty result. Do not skip
patterns just because you found facts in the truncated view — the
result may be much larger than what you see.

You complement a builtin regex pass.
The system runs builtin regex patterns over the FULL untruncated result
BEFORE this LLM call and saves those facts independently. Builtins
already capture common ID shapes for known services (e.g. Gmail 16-hex
message_id and threadId, Slack channel_id, Stripe charge_id/customer_id,
Linear issue_id, Calendar event_id including recurring instances, plus
a generic 8+ char "id" catch-all). You don't need to focus your output
on those; the system has them covered. Spend your direct-fact budget
and your custom regex patterns on the LONG TAIL: emails, person names,
dates, URLs, novel ID shapes that the generic patterns wouldn't match,
and any service-specific identifiers that don't follow an obvious
convention. If you do emit overlapping facts the system dedupes them,
so duplicates are harmless — but redirecting your effort to the long
tail makes your contribution more valuable.

OUTPUT FORMAT — strict.
Output a single raw JSON object only. The very first character of your
response must be the opening brace, and the very last character must be
the closing brace. Do NOT wrap the JSON in markdown code fences of any
kind (no triple-backtick blocks, no language tags). Do NOT include prose,
commentary, explanation, or notes before, after, or between the braces.
The parser extracts only the first balanced JSON value — any extra text
is wasted.

============================================================
WHAT TO EXTRACT — canonical fact_type vocabulary
============================================================
Cost asymmetry: an extra fact stored is cheap; a missed identifier blocks
a legitimate downstream request. Lean toward extracting any value that
looks like a routable identifier, even if it doesn't match a known name.

Common fact_type names — use these where they fit, snake_case:
  email_address, phone_number, person_name, url, domain, date,
  message_id, thread_id, file_id, event_id, contact_id, calendar_id,
  channel_id, channel_name, user_id, customer_id, issue_id, pr_id,
  commit_hash

If a service emits an identifier that doesn't fit any of those, invent a
descriptive snake_case name and use it. Don't drop a routable ID just
because it lacks a canonical entry. The name should describe the entity
type, not the source field name (use "issue_id" over "issueIdentifier").

Specific mappings the system depends on (these ARE load-bearing):
  - message_id is the SERVICE-INTERNAL API ID (Gmail 16-hex, iMessage
    GUID, etc.). NOT the RFC 5322 "<...@...>" header — drop those.
  - date is an ISO date (YYYY-MM-DD or full RFC 3339); NEVER a bare
    time-of-day ("10:00:00", "3:30 PM") — that's a value, not an entity.
  - Drop confirmation/booking/reservation codes, postal codes, and money
    amounts — these are values, not chain-routable references.

============================================================
WHAT NOT TO EXTRACT — content vs. structural references
============================================================
A structural reference is something a future API call in the same task
might TARGET (e.g. fetch by ID, send to address). Anything else is
content. Even if it appears as a JSON field, content goes nowhere.

NEVER extract — content, not entities:

  Titles, subjects, summaries, descriptions:
    event titles ("Coffee Mornings!", "Team Sync"),
    email subjects, calendar names, file/document names,
    issue/PR titles, meeting summaries, message bodies.

  Names of things (not people):
    company names, team / organization names, product names,
    bank names, location / property names, project names,
    repository names.

  Status / metadata / values:
    labels (Gmail's "INBOX", "UNREAD", "CATEGORY_UPDATES"),
    status strings ("active", "closed", "pending"),
    durations, percentages, version numbers,
    bare time-of-day fragments ("10:00:00", "3:30 PM"),
    counts ("3 events", "5 messages"),
    flags (is_unread, is_archived).

  Pagination state:
    page_token, next_page_token, cursor, pagination_token, sync_token.
    These are internal cursor state, not chain-context references.

  Postal addresses:
    street addresses, postal codes, full physical addresses. Drop them
    even if the API returns "address" as a field. We do not chain-route
    on postal addresses.

Test before emitting: "If a future request in this task TARGETED this
value, would the call shape be 'fetch X by this ID' or 'send to this
address'?" If yes, it's a structural reference. If the value is a label,
title, status, version, or summary, it is content — drop it.

============================================================
SENSITIVE CONTENT — never extract
============================================================
Beyond the existing rule (no message bodies, file contents, passwords,
tokens, API keys, HTML), the prohibition extends to:

  - one-time passwords (OTPs) and 2FA codes
  - email/SMS verification codes ("753269", "Your code is 581904")
  - personal identification numbers (PINs)
  - meeting passcodes (Zoom passcode, conference PIN, dial-in PIN)
  - banking secrets: routing numbers, SWIFT codes, full account numbers,
    full credit-card numbers, CVVs, "card_last_four"
  - any short numeric or alphanumeric code labeled "code", "passcode",
    "pin", "otp", "verification", "auth_code" — these are typically
    secrets, not chain-context references.

Example: if a result body says "Your verification code is 753269" — do
nothing with the code value. Extract the email_address of the sender
(if present), but never the code itself.

============================================================
QUALITY / FORMAT RULES
============================================================
1. JSON KEY vs. VALUE — never extract JSON KEY NAMES as fact values.
   Keys are the field names; values are what comes after the colon.
   In:  {"to":"alice@example.com","from":"bob@example.com"}
   The email_address values are "alice@example.com" and
   "bob@example.com" — NEVER "to", "from", "id", "subject", or any
   other key name.

2. ONE ENTITY per fact — never combine multiple entities.
   Wrong:  email_address = "Chris <chris@a.com>, Danny <danny@b.com>"
   Right:  email_address = "chris@a.com"
           email_address = "danny@b.com"

3. NO TRUNCATION — fact_value must be the complete entity. If the
   truncation cliff cuts an email mid-domain ("juan@"), DROP it; the
   regex patterns running against the full result will pick up the
   complete value. Same for partial IDs, partial URLs, partial phones.

4. NO SURROUNDING TEXT — fact_value is the entity alone, not wrapped
   in quotes, brackets, or display-name formatting.
   Wrong:  email_address = "<alice@example.com>"
           email_address = "Alice Chen <alice@example.com>"
   Right:  email_address = "alice@example.com"
           person_name   = "Alice Chen"

5. CROSS-SERVICE shape mismatch — never emit a value whose ID shape
   does not match the service.
   Wrong:  on google.calendar list_events, emitting
           message_id = "msg_11ldp32n" (iMessage-shape).
   Each service has its own ID shape; if the value doesn't match, it
   either belongs to a different fact_type or should be dropped.

6. VALIDATE shape before emitting:
   - email_address: matches local@domain.tld, no whitespace
   - message_id (Gmail): 16 hex chars, no angle brackets
   - phone_number: digits with optional "+", no embedded text
   - url: starts with http:// or https://
   - date: ISO 8601 (YYYY-MM-DD or full RFC 3339), not bare time

7. WHEN UNSURE, OMIT. Missing facts are recovered by regex patterns;
   bad facts pollute chain context and break downstream verification.

============================================================
REGEX PATTERN GUIDANCE
============================================================
Patterns must:
  - use Go RE2 syntax (no lookahead/lookbehind, no backreferences);
  - have EXACTLY ONE capture group around the value;
  - anchor on JSON structure (a key prefix like "id":) where possible
    to avoid catching the value inside body text or quoted email content.

Prefer permissive patterns over narrow ones when entity values vary in
shape. A pattern like "\"id\":\\s*\"([^\"]+)\"" that captures any quoted
value after an "id" key is fine — it catches recurring instance IDs,
opaque tokens, and unusual formats that a tighter regex would miss. The
runtime caps each pattern at 200 matches, so over-broad capture is
self-limiting; missing identifiers is the failure mode that costs us.

Examples of good patterns:
  email_address: "\"(?:email|emailAddress|address)\":\\s*\"([^\"]+@[^\"]+\\.[^\"]+)\""
  message_id (Gmail): "\"(?:id|message_id)\":\\s*\"([a-f0-9]{16})\""
  phone_number: "\"(?:phone|phoneNumber|number)\":\\s*\"(\\+?[0-9][0-9 ()-]{6,})\""
  file_id (Drive): "\"(?:id|fileId)\":\\s*\"([a-zA-Z0-9_-]{20,})\""
  event_id (Calendar): "\"id\":\\s*\"([^\"]+)\""  (catches both plain
    alphanumeric IDs and recurring-instance "<base>_<UTC_datetime>Z" form)

DO NOT:
  - match across the entire JSON without a key anchor — e.g. the bare
    pattern "([^\"]+@[^\"]+)" matches every email-shaped substring,
    including addresses inside quoted email body content;
  - include the surrounding key in the capture group — capture only
    the value.

============================================================
WORKED EXAMPLES — POSITIVE
============================================================
E1. Gmail list_messages (truncated). Result snippet:
  {"messages":[
    {"id":"19db3147fd3da575","threadId":"19db3147fd3da575",
     "from":"Alice Chen <alice@example.com>","subject":"...","date":"..."},
    {"id":"19db4f7c1c0ce3c6","threadId":"19db4f7c1c0ce3c6",
     "from":"Bob Lee <bob@example.com>",...
Output:
  {"facts":[
     {"fact_type":"message_id","fact_value":"19db3147fd3da575"},
     {"fact_type":"thread_id","fact_value":"19db3147fd3da575"},
     {"fact_type":"email_address","fact_value":"alice@example.com"},
     {"fact_type":"person_name","fact_value":"Alice Chen"},
     {"fact_type":"message_id","fact_value":"19db4f7c1c0ce3c6"},
     {"fact_type":"thread_id","fact_value":"19db4f7c1c0ce3c6"},
     {"fact_type":"email_address","fact_value":"bob@example.com"},
     {"fact_type":"person_name","fact_value":"Bob Lee"}],
   "patterns":[
     {"fact_type":"message_id","regex":"\"id\":\\s*\"([a-f0-9]{16})\""},
     {"fact_type":"thread_id","regex":"\"threadId\":\\s*\"([a-f0-9]{16})\""},
     {"fact_type":"email_address","regex":"\"from\":\\s*\"[^\"]*<?([^\"\\s<>]+@[^\"\\s<>]+)>?\""}]}
  Note: subject is omitted (content). The from field is parsed to
  extract just the address; the regex captures the email even when
  it's wrapped in display-name angle brackets.

E2. Calendar list_events. Result snippet:
  {"events":[
    {"id":"abc123_20260422","summary":"Team Sync",
     "start":"2026-04-22T15:00:00Z","end":"2026-04-22T16:00:00Z",
     "attendees":[{"email":"alice@example.com"},{"email":"bob@example.com"}],
     "location":"Conference Room B",
     "htmlLink":"https://calendar.google.com/event?eid=abc"},
    {"id":"def456_20260423","summary":"1:1 with Bob",...
Output:
  {"facts":[
     {"fact_type":"event_id","fact_value":"abc123_20260422"},
     {"fact_type":"date","fact_value":"2026-04-22"},
     {"fact_type":"email_address","fact_value":"alice@example.com"},
     {"fact_type":"email_address","fact_value":"bob@example.com"},
     {"fact_type":"url","fact_value":"https://calendar.google.com/event?eid=abc"},
     {"fact_type":"event_id","fact_value":"def456_20260423"},
     {"fact_type":"date","fact_value":"2026-04-23"}],
   "patterns":[
     {"fact_type":"event_id","regex":"\"id\":\\s*\"([A-Za-z0-9_]+_[0-9]{8}[A-Z0-9]*)\""},
     {"fact_type":"email_address","regex":"\"email\":\\s*\"([^\"]+@[^\"]+\\.[^\"]+)\""}]}
  Note: summary, location, end-time, and the bare time portion of
  start are content — omitted. Only the date portion is extracted.

E3. Stripe list_charges. Result snippet:
  {"data":[{"id":"ch_3NqBrpJ2eZvKYo4C","amount":5000,"currency":"usd",
    "customer":"cus_QYr1oFZdhWB2c","receipt_email":"customer@example.com",
    "billing_details":{"name":"Jane Doe","address":{...}},...
Output:
  {"facts":[
     {"fact_type":"charge_id","fact_value":"ch_3NqBrpJ2eZvKYo4C"},
     {"fact_type":"customer_id","fact_value":"cus_QYr1oFZdhWB2c"},
     {"fact_type":"email_address","fact_value":"customer@example.com"},
     {"fact_type":"person_name","fact_value":"Jane Doe"}],
   "patterns":[
     {"fact_type":"charge_id","regex":"\"id\":\\s*\"(ch_[A-Za-z0-9]+)\""},
     {"fact_type":"customer_id","regex":"\"customer\":\\s*\"(cus_[A-Za-z0-9]+)\""}]}
  Note: amount and address are dropped — they're values, not routable
  references. charge_id and customer_id are good names for the ch_* and
  cus_* prefixed identifiers Stripe uses.

E4. Slack list_channels (paginated). Result snippet:
  {"channels":[
    {"id":"C0K8L2M5N7Q","name":"general","is_archived":false},
    {"id":"C0R3S5T7U9X","name":"project-cascade","is_archived":false}],
   "response_metadata":{"next_cursor":"dGVhbTpDMEFBREw5NkI4WA=="}}
Output:
  {"facts":[
     {"fact_type":"channel_id","fact_value":"C0K8L2M5N7Q"},
     {"fact_type":"channel_name","fact_value":"general"},
     {"fact_type":"channel_id","fact_value":"C0R3S5T7U9X"},
     {"fact_type":"channel_name","fact_value":"project-cascade"}],
   "patterns":[
     {"fact_type":"channel_id","regex":"\"id\":\\s*\"(C[A-Z0-9]{8,})\""},
     {"fact_type":"channel_name","regex":"\"name\":\\s*\"([A-Za-z0-9_-]+)\""}]}
  Note: extract BOTH channel_id and channel_name — Slack agents reference
  channels by either. is_archived flag is metadata; next_cursor is pagination
  state. Drop both.

============================================================
WORKED EXAMPLES — NEGATIVE
============================================================
N1. Gmail get_message containing a verification code. Body:
  "Your verification code is 753269. It expires in 10 minutes."
  Headers: From: noreply@bank.com, To: alice@example.com,
  Subject: "Verify your account"
CORRECT extraction:
  facts: [
    {"fact_type":"email_address","fact_value":"noreply@bank.com"},
    {"fact_type":"email_address","fact_value":"alice@example.com"}]
WRONG extractions (must not appear):
  - {"fact_type":"verification_code","fact_value":"753269"} — sensitive
  - {"fact_type":"subject","fact_value":"Verify your account"} — content
  - {"fact_type":"message_id","fact_value":"<...@mail.example.com>"} —
    RFC 5322 header, not the API ID

N2. Calendar list_calendars (paginated). Result snippet:
  {"items":[{"id":"primary","summary":"My Calendar"},
            {"id":"family@group.calendar.google.com","summary":"Family"}],
   "nextPageToken":"EiEKCwjfyM2dBhDA..."}
CORRECT extraction:
  facts: [
    {"fact_type":"calendar_id","fact_value":"primary"},
    {"fact_type":"calendar_id","fact_value":"family@group.calendar.google.com"}]
WRONG extractions:
  - {"fact_type":"page_token","fact_value":"EiEK..."} — pagination state
  - {"fact_type":"calendar_summary","fact_value":"My Calendar"} — content
  - {"fact_type":"calendar_name","fact_value":"Family"} — content

N3. Gmail list_messages where the LLM grabbed JSON keys.
Result snippet:
  {"messages":[{"id":"19db3147fd3da575","from":"alice@example.com",
                "to":"bob@example.com"}, ... ]}
CORRECT extraction:
  facts: [
    {"fact_type":"message_id","fact_value":"19db3147fd3da575"},
    {"fact_type":"email_address","fact_value":"alice@example.com"},
    {"fact_type":"email_address","fact_value":"bob@example.com"}]
WRONG extractions (key names captured as values):
  - {"fact_type":"email_address","fact_value":"to"}
  - {"fact_type":"email_address","fact_value":"from"}
  - {"fact_type":"message_id","fact_value":"id"}

============================================================
SUMMARY CHECKLIST
============================================================
For every result:
  ☐ Use ONLY the canonical fact_type names listed above.
  ☐ Drop content (titles, summaries, names of things, labels, status,
    versions, durations, time fragments, counts, postal addresses).
  ☐ Drop pagination tokens (page_token, next_page_token, cursor, etc).
  ☐ Drop sensitive codes (OTPs, PINs, verification codes,
    banking secrets).
  ☐ For each value, capture the entity ALONE — no surrounding text,
    no JSON key names, no truncated fragments, no combined values.
  ☐ Always emit regex patterns with one capture group; anchor on JSON
    keys where possible.
  ☐ Empty result: return {"facts":[],"patterns":[]}.`

// maxRegexRuntime caps how long we spend running LLM-provided regexes.
const maxRegexRuntime = 2 * time.Second

// maxRegexMatches caps the number of matches per regex pattern to prevent
// runaway extraction from overly broad patterns. Sized to match
// maxExtractedFacts so a single tight pattern (e.g. Gmail's 16-hex
// message_id) can fully populate chain context for a large list response.
const maxRegexMatches = 500

// ExtractBuiltins runs only the builtin regex patterns against the full
// result. No LLM call. Returns immediately (microseconds to milliseconds).
// Safe to call before LLM extraction so high-confidence facts (Gmail
// 16-hex message_ids, Slack channel_ids, Stripe charge/customer prefixes,
// etc.) land in chain_facts without waiting for the LLM round trip.
func (e *LLMExtractor) ExtractBuiltins(req ExtractRequest) []*store.ChainFact {
	patterns := builtinPatterns(req.Service, req.Action)
	if len(patterns) == 0 {
		return nil
	}
	facts, _ := mergeExtractionResults(nil, patterns, nil, req, e.logger)
	if len(facts) > 0 {
		e.logger.Debug("chain context builtin extraction complete",
			"task_id", req.TaskID,
			"service", req.Service,
			"action", req.Action,
			"facts", len(facts),
		)
	}
	return facts
}

// ExtractLLM calls the LLM extractor and returns ONLY the facts that are
// new relative to `existing` (typically the output of ExtractBuiltins for
// the same request). On LLM failure returns nil (no error) — the builtin
// pass already saved high-confidence facts, so failure here just means we
// miss long-tail entities (emails, person names, novel IDs) for this call.
func (e *LLMExtractor) ExtractLLM(ctx context.Context, req ExtractRequest, existing []*store.ChainFact) ([]*store.ChainFact, error) {
	result := req.Result
	truncated := len(result) > maxExtractResultLen
	if truncated {
		result = result[:maxExtractResultLen]
	}

	userMsg := fmt.Sprintf("Service: %s\nAction: %s\n\nResult:\n%s", req.Service, req.Action, result)

	cfg := e.health.ChainContextConfig()
	// Extraction outputs are larger than verification (full facts array +
	// regex patterns) — bump max_tokens above the package default 1024 so
	// large list responses don't get truncated mid-JSON. Haiku is compact
	// enough that 1024 usually fits, but Gemini's output is more verbose
	// per fact emitted. Sized at 8192 — enough for any reasonable list
	// response without wasting tokens.
	client := llm.NewClient(cfg.LLMProviderConfig).WithMaxTokens(8192)
	if e.geminiCacheNameFn != nil {
		client.AttachGeminiCacheNameFn(e.geminiCacheNameFn)
	}
	messages := []llm.ChatMessage{
		{Role: "system", Content: extractionSystemPrompt, CacheControl: true},
		{Role: "user", Content: userMsg},
	}

	raw, usage, err := client.CompleteWithUsage(ctx, messages)
	if err != nil {
		if errors.Is(err, llm.ErrSpendCapExhausted) {
			e.health.SetSpendCapExhausted()
		}
		e.logger.Warn("chain context extraction LLM call failed", "err", err, "task_id", req.TaskID)
		return nil, nil
	}
	llm.LogUsage(e.logger, "chain_context_extraction", cfg.Model, usage)

	// Parse response — supports both old format (array) and new format
	// (object with facts+patterns). Models often wrap JSON in markdown fences
	// or add trailing prose; extract the first complete JSON value via
	// brace-matching to avoid those failure modes.
	extracted := extractFirstJSONValue(raw)
	if extracted == "" {
		e.logger.Warn("chain context extraction: no JSON value found in response", "task_id", req.TaskID)
		return nil, nil
	}
	directFacts, patterns := parseExtractionResponse(extracted, e.logger, req.TaskID)

	// Pre-seed seen with anything ExtractBuiltins already produced so this
	// pass returns only NEW facts and the gateway can save them with a
	// straight INSERT (no need for upsert semantics).
	seen := make(map[string]bool, len(existing))
	for _, f := range existing {
		seen[f.FactType+"|"+f.FactValue] = true
	}

	facts, dropped := mergeExtractionResults(directFacts, patterns, seen, req, e.logger)

	e.logger.Debug("chain context LLM extraction complete",
		"task_id", req.TaskID,
		"direct_facts", len(directFacts)-dropped,
		"dropped", dropped,
		"patterns", len(patterns),
		"new_facts", len(facts),
		"existing_facts", len(existing),
		"truncated", truncated,
	)

	return facts, nil
}

// Extract is a convenience wrapper that calls both phases and returns the
// combined result. Callers that want the latency benefit (saving builtin
// facts before the LLM call returns) should call ExtractBuiltins and
// ExtractLLM directly.
func (e *LLMExtractor) Extract(ctx context.Context, req ExtractRequest) ([]*store.ChainFact, error) {
	builtinFacts := e.ExtractBuiltins(req)
	llmFacts, err := e.ExtractLLM(ctx, req, builtinFacts)
	if err != nil {
		return builtinFacts, err
	}
	return append(builtinFacts, llmFacts...), nil
}

// mergeExtractionResults validates direct facts against the full result and
// augments them with regex-pattern matches, deduped by (fact_type, fact_value).
//
// `preSeen` is an optional pre-seeded dedupe set; pass non-nil to suppress
// facts already produced by an earlier extraction phase (e.g. ExtractLLM
// passes the keys of facts emitted by ExtractBuiltins). Pass nil for a
// fresh dedupe.
//
// Regex always runs when patterns exist. An earlier version gated regex on
// "truncated || llm_failed || no direct facts", but that caused silent
// misses on small list responses: the LLM's 20-direct-fact soft cap could
// omit an ID the response contained, and regex — the only mechanism that
// would have caught it — was skipped because nothing looked wrong. Facts
// are deduped and capped, so there's no double-counting or overflow cost.
func mergeExtractionResults(
	directFacts []extractedFact,
	patterns []extractionPattern,
	preSeen map[string]bool,
	req ExtractRequest,
	logger *slog.Logger,
) (facts []*store.ChainFact, dropped int) {
	seen := make(map[string]bool, len(preSeen))
	for k := range preSeen {
		seen[k] = true
	}

	appendFact := func(factType, factValue, source string) {
		key := factType + "|" + factValue
		if seen[key] {
			return
		}
		seen[key] = true
		facts = append(facts, &store.ChainFact{
			ID:        uuid.New().String(),
			TaskID:    req.TaskID,
			SessionID: req.SessionID,
			AuditID:   req.AuditID,
			Service:   req.Service,
			Action:    req.Action,
			FactType:  factType,
			FactValue: factValue,
			Source:    source,
		})
	}

	// Direct facts come from the LLM's explicit "facts" array.
	for _, ef := range directFacts {
		if ef.FactType == "" || ef.FactValue == "" {
			dropped++
			continue
		}
		if !substringMatch(req.Result, ef.FactValue, ef.FactType) {
			dropped++
			continue
		}
		appendFact(ef.FactType, ef.FactValue, "llm_direct")
	}

	// Regex matches inherit their source from the originating pattern
	// (builtin or llm_regex).
	if len(patterns) > 0 {
		regexFacts := runExtractionPatterns(patterns, req.Result, logger, req.TaskID)
		for _, rf := range regexFacts {
			source := rf.source
			if source == "" {
				source = "unknown"
			}
			appendFact(rf.factType, rf.factValue, source)
		}
	}

	if len(facts) > maxExtractedFacts {
		facts = facts[:maxExtractedFacts]
	}
	return facts, dropped
}

type extractedFact struct {
	FactType  string `json:"fact_type"`
	FactValue string `json:"fact_value"`
}

type extractionPattern struct {
	FactType string `json:"fact_type"`
	Regex    string `json:"regex"`
	// Source identifies which extractor produced this pattern. Set to
	// "builtin" for hardcoded per-service patterns and "llm_regex" for
	// patterns parsed out of an LLM response. Skipped on JSON marshal so
	// it doesn't leak into LLM responses or get clobbered by parsing.
	Source string `json:"-"`
}

// extractFirstJSONValue returns the first complete JSON object or array
// found in s, ignoring leading/trailing markdown fences, prose, or extra
// content. Returns "" if no balanced JSON value is found. The returned
// substring is suitable for direct json.Unmarshal.
func extractFirstJSONValue(s string) string {
	// Find the first { or [ that begins a value.
	start := -1
	var open, closeCh byte
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '{':
			start, open, closeCh = i, '{', '}'
		case '[':
			start, open, closeCh = i, '[', ']'
		}
		if start != -1 {
			break
		}
	}
	if start == -1 {
		return ""
	}

	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if escaped {
			escaped = false
			continue
		}
		if inString {
			switch c {
			case '\\':
				escaped = true
			case '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case open:
			depth++
		case closeCh:
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return "" // unterminated
}

// parseExtractionResponse handles both the new object format
// {"facts": [...], "patterns": [...]} and the legacy array format [...].
// Every returned pattern has Source="llm_regex" so facts captured by these
// patterns are distinguishable from builtin-pattern facts in chain_facts.
func parseExtractionResponse(raw string, logger *slog.Logger, taskID string) ([]extractedFact, []extractionPattern) {
	// Try new format first.
	var obj struct {
		Facts    []extractedFact     `json:"facts"`
		Patterns []extractionPattern `json:"patterns"`
	}
	if err := json.Unmarshal([]byte(raw), &obj); err == nil && (len(obj.Facts) > 0 || len(obj.Patterns) > 0) {
		for i := range obj.Patterns {
			obj.Patterns[i].Source = "llm_regex"
		}
		return obj.Facts, obj.Patterns
	}

	// Fall back to legacy array format (no patterns in this format).
	var arr []extractedFact
	if err := json.Unmarshal([]byte(raw), &arr); err != nil {
		logger.Warn("chain context extraction parse failed", "err", err, "task_id", taskID)
		return nil, nil
	}
	return arr, nil
}

type regexMatch struct {
	factType  string
	factValue string
	source    string // "builtin" or "llm_regex"
}

// runExtractionPatterns compiles and runs LLM-emitted regex patterns against
// the full result. Each pattern must have exactly one capture group.
// Patterns that fail to compile, take too long, or match too broadly are skipped.
func runExtractionPatterns(patterns []extractionPattern, fullResult string, logger *slog.Logger, taskID string) []regexMatch {
	deadline := time.Now().Add(maxRegexRuntime)
	var matches []regexMatch

	for _, p := range patterns {
		if time.Now().After(deadline) {
			logger.Warn("regex extraction deadline exceeded, skipping remaining patterns",
				"task_id", taskID,
			)
			break
		}
		if p.FactType == "" || p.Regex == "" {
			continue
		}

		re, err := regexp.Compile(p.Regex)
		if err != nil {
			logger.Debug("skipping invalid extraction regex",
				"task_id", taskID,
				"fact_type", p.FactType,
				"regex", p.Regex,
				"err", err,
			)
			continue
		}

		found := re.FindAllStringSubmatch(fullResult, maxRegexMatches)
		for _, m := range found {
			// Expect exactly one capture group.
			if len(m) < 2 {
				continue
			}
			val := strings.TrimSpace(m[1])
			if val == "" {
				continue
			}
			matches = append(matches, regexMatch{factType: p.FactType, factValue: val, source: p.Source})
		}
	}

	return matches
}

// builtinPatterns returns hardcoded extraction patterns for known service
// result structures. These run on every extraction call alongside any
// patterns the LLM emitted, providing defense-in-depth so common ID and
// email shapes are always captured. Dedupe in mergeExtractionResults
// handles overlap with LLM-emitted facts. Every returned pattern has
// Source="builtin" so facts produced by these patterns are auditable.
func builtinPatterns(service, action string) []extractionPattern {
	patterns := builtinPatternsRaw(service, action)
	for i := range patterns {
		patterns[i].Source = "builtin"
	}
	return patterns
}

// builtinPatternsRaw returns the per-service patterns without setting a
// Source — the public builtinPatterns wrapper handles that uniformly.
func builtinPatternsRaw(service, action string) []extractionPattern {
	// Generic patterns that apply to most JSON API responses. Includes a
	// permissive entity_id catch for any "id" field with an 8+ char value;
	// service-specific patterns below typically also extract these values
	// under more precise fact_types, and dedupe handles the overlap.
	generic := []extractionPattern{
		{FactType: "email_address", Regex: `"(?:email|emailAddress|from|to|sender|recipient)":\s*"([^"]+@[^"]+)"`},
		{FactType: "entity_id", Regex: `"id":\s*"([^"]{8,})"`},
	}

	svc := strings.SplitN(service, ":", 2)[0] // strip instance suffix

	switch svc {
	case "google.gmail":
		return append(generic,
			extractionPattern{FactType: "message_id", Regex: `"id":\s*"([a-f0-9]{16})"`},
			extractionPattern{FactType: "thread_id", Regex: `"threadId":\s*"([a-f0-9]{16})"`},
			extractionPattern{FactType: "email_address", Regex: `"email":\s*"([^"]+@[^"]+)"`},
		)
	case "google.drive":
		return append(generic,
			extractionPattern{FactType: "file_id", Regex: `"id":\s*"([^"]+)"`},
			extractionPattern{FactType: "email_address", Regex: `"emailAddress":\s*"([^"]+@[^"]+)"`},
		)
	case "google.calendar":
		// Google Calendar event IDs are lowercase base32 (5-1024 chars),
		// optionally prefixed with "_" for imported events, with an optional
		// "_<UTC_datetime>Z" suffix for recurring instances. Tight to avoid
		// labeling calendar IDs (e.g. "primary", "x@group.calendar.google.com")
		// as event_ids; those still get captured by the generic entity_id
		// fallback in the shared block above.
		return append(generic,
			extractionPattern{FactType: "event_id", Regex: `"id":\s*"(_?[a-z0-9]+(?:_[0-9]{8}T[0-9]{6}Z)?)"`},
			extractionPattern{FactType: "email_address", Regex: `"email":\s*"([^"]+@[^"]+)"`},
		)
	case "google.contacts":
		return append(generic,
			extractionPattern{FactType: "phone_number", Regex: `"value":\s*"(\+?[\d\s\-()]{7,})"`},
			extractionPattern{FactType: "email_address", Regex: `"value":\s*"([^"]+@[^"]+)"`},
		)
	case "github":
		return append(generic,
			extractionPattern{FactType: "issue_id", Regex: `"number":\s*(\d+)`},
			extractionPattern{FactType: "pr_id", Regex: `"number":\s*(\d+)`},
		)
	case "linear":
		return append(generic,
			extractionPattern{FactType: "issue_id", Regex: `"id":\s*"([a-f0-9-]{36})"`},
			extractionPattern{FactType: "issue_id", Regex: `"identifier":\s*"([A-Z]+-\d+)"`},
		)
	case "slack":
		return append(generic,
			extractionPattern{FactType: "channel_id", Regex: `"id":\s*"(C[A-Z0-9]+)"`},
			extractionPattern{FactType: "message_id", Regex: `"ts":\s*"([\d.]+)"`},
		)
	case "stripe":
		return append(generic,
			extractionPattern{FactType: "charge_id", Regex: `"id":\s*"(ch_[a-zA-Z0-9]+)"`},
			extractionPattern{FactType: "customer_id", Regex: `"(?:id|customer)":\s*"(cus_[a-zA-Z0-9]+)"`},
		)
	case "imessage":
		return append(generic,
			extractionPattern{FactType: "message_id", Regex: `"(?:id|guid)":\s*"([^"]+)"`},
			extractionPattern{FactType: "phone_number", Regex: `"(?:handle|sender|recipient)":\s*"(\+?[\d]{10,})"`},
		)
	default:
		// Unknown service: rely entirely on the generic block, which already
		// emits an entity_id pattern with an 8+ char minimum and the email
		// pattern. No service-specific additions.
		return generic
	}
}

// substringMatch checks that factValue appears in result.
// Case-insensitive for email_address and domain fact types; exact match for all others.
func substringMatch(result, factValue, factType string) bool {
	if factType == "email_address" || factType == "domain" {
		return strings.Contains(strings.ToLower(result), strings.ToLower(factValue))
	}
	return strings.Contains(result, factValue)
}
