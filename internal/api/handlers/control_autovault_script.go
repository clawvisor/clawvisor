package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/internal/intent"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
	"github.com/clawvisor/clawvisor/pkg/store"
	"github.com/google/uuid"
)

// scriptSessionHardLimits captures the v1 conservative defaults from the
// plan. Exposed so tests can inspect them; not configurable at runtime
// to avoid silently widening blast radius on cap drift.
const (
	scriptSessionMaxTTLSeconds     = 120
	scriptSessionMaxUses           = 200
	scriptSessionMaxRequestBytes = 1 << 20 // 1 MiB per response
	// scriptSessionMinTotalBytes is the floor for the aggregate byte
	// cap. We scale MaxTotalBytes with MaxUses (= max_uses * per-
	// request cap) so a 200-call fan-out can have every response near
	// the per-request ceiling without the aggregate cap binding
	// arbitrarily. The floor keeps small sessions (max_uses = 3) from
	// being trivially throttled — even a tiny session gets at least
	// 10 MiB of aggregate headroom, which covers any reasonable workflow.
	scriptSessionMinTotalBytes = 10 << 20 // 10 MiB
	scriptSessionMaxPathPrefixes   = 5
)

// AllowedScriptSessionMethods lists the HTTP methods a script session
// may bind to in v1. GET-only mirrors the plan; broader scopes (POST/
// PATCH/PUT/DELETE) wait on action-level scope mapping in the verifier.
var AllowedScriptSessionMethods = []string{"GET"}

// mintScriptSessionRequest is the wire shape POSTed to
// /api/control/autovault/script-session.
type mintScriptSessionRequest struct {
	Placeholder  string   `json:"placeholder"`
	TargetHost   string   `json:"target_host"`
	Methods      []string `json:"methods"`
	PathPrefixes []string `json:"path_prefixes"`
	MaxUses      int      `json:"max_uses"`
	TTLSeconds   int      `json:"ttl_seconds"`
	Why          string   `json:"why"`
}

// MintScriptSession handles POST /api/control/autovault/script-session.
//
// The flow:
//  1. Authenticate the agent via the existing caller-nonce middleware.
//  2. Validate deterministic constraints (placeholder ownership,
//     bound-host allowlist, method/path/use/TTL caps).
//  3. Resolve the placeholder's task and consult the intent verifier on
//     the derived capability.
//  4. Mint a short-lived ScriptSession token in the cache.
//  5. Emit a mint audit row and return the token + bounds.
func (h *LLMControlHandler) MintScriptSession(w http.ResponseWriter, r *http.Request) {
	agent := middleware.AgentFromContext(r.Context())
	if agent == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error":   "unauthorized",
			"message": "missing agent context",
		})
		return
	}
	if h.Store == nil || h.ScriptSessions == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error":   "script_session_unavailable",
			"message": "script session cache or store is not configured",
		})
		return
	}

	// Cap the request body. The mint payload is small (a placeholder,
	// a host, a few methods, a few prefixes, a max_uses, a ttl, a
	// `why`); 64 KiB is generous and prevents an authenticated agent
	// from forcing the daemon to allocate large buffers by POSTing
	// arbitrary garbage. 64 KiB allows for a long `why` plus comments.
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var body mintScriptSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		h.auditMintDeny(r.Context(), agent, llmproxy.ScriptSession{}, http.StatusBadRequest, "invalid_json", err.Error())
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":   "invalid_json",
			"message": err.Error(),
		})
		return
	}

	body.Placeholder = strings.TrimSpace(body.Placeholder)
	body.TargetHost = strings.ToLower(strings.TrimSpace(body.TargetHost))
	body.Why = strings.TrimSpace(body.Why)

	// partialMintSess captures whatever request fields have been
	// validated so far; deny paths pass it to LogScriptSessionMint so
	// the audit row reflects what the agent ASKED for, even on the
	// earliest rejections (placeholder/target_host/methods unset, etc.).
	partialMintSess := func() llmproxy.ScriptSession {
		return llmproxy.ScriptSession{
			UserID:      agent.UserID,
			AgentID:     agent.ID,
			Placeholder: body.Placeholder,
			TargetHost:  body.TargetHost,
			Why:         body.Why,
		}
	}

	if body.Placeholder == "" {
		h.denyMint(w, r.Context(), agent, partialMintSess(), http.StatusBadRequest, "placeholder_required", "placeholder is required")
		return
	}
	if body.TargetHost == "" {
		h.denyMint(w, r.Context(), agent, partialMintSess(), http.StatusBadRequest, "target_host_required", "target_host is required")
		return
	}
	if body.Why == "" {
		h.denyMint(w, r.Context(), agent, partialMintSess(), http.StatusBadRequest, "why_required", "why is required and must describe the script's purpose")
		return
	}
	methods, err := normalizeMintMethods(body.Methods)
	if err != nil {
		h.denyMint(w, r.Context(), agent, partialMintSess(), http.StatusBadRequest, "invalid_methods", err.Error())
		return
	}
	prefixes, err := normalizeMintPrefixes(body.PathPrefixes)
	if err != nil {
		h.denyMint(w, r.Context(), agent, partialMintSess(), http.StatusBadRequest, "invalid_path_prefixes", err.Error())
		return
	}
	maxUses, err := clampMintMaxUses(body.MaxUses)
	if err != nil {
		h.denyMint(w, r.Context(), agent, partialMintSess(), http.StatusBadRequest, "invalid_max_uses", err.Error())
		return
	}
	ttl, err := clampMintTTL(body.TTLSeconds)
	if err != nil {
		h.denyMint(w, r.Context(), agent, partialMintSess(), http.StatusBadRequest, "invalid_ttl", err.Error())
		return
	}

	// From here on the partial sess can carry the normalized
	// methods/prefixes/maxUses/ttl too — those are useful in the audit
	// row when later checks (placeholder, task, verifier) deny.
	partialMintSess = func() llmproxy.ScriptSession {
		return llmproxy.ScriptSession{
			UserID:       agent.UserID,
			AgentID:      agent.ID,
			Placeholder:  body.Placeholder,
			TargetHost:   body.TargetHost,
			Methods:      methods,
			PathPrefixes: prefixes,
			MaxUses:      maxUses,
			Why:          body.Why,
		}
	}

	// Placeholder ownership + bound-host check.
	ph, err := h.Store.GetRuntimePlaceholder(r.Context(), body.Placeholder)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			h.denyMint(w, r.Context(), agent, partialMintSess(), http.StatusUnauthorized, "unknown_placeholder", "placeholder not registered")
			return
		}
		h.denyMint(w, r.Context(), agent, partialMintSess(), http.StatusServiceUnavailable, "placeholder_lookup_failed", "placeholder lookup failed")
		return
	}
	if ph.UserID != agent.UserID || (ph.AgentID != "" && ph.AgentID != agent.ID) {
		h.denyMint(w, r.Context(), agent, partialMintSess(), http.StatusForbidden, "placeholder_ownership", "placeholder does not belong to the calling agent")
		return
	}
	now := time.Now().UTC()
	if reason, ok := llmproxy.ValidateRuntimePlaceholderAccess(r.Context(), h.Store, ph, agent.UserID, agent.ID, now); !ok {
		h.denyMint(w, r.Context(), agent, partialMintSess(), http.StatusForbidden, "placeholder_rejected", reason)
		return
	}
	hosts, hostReason := llmproxy.RuntimePlaceholderBoundHosts(r.Context(), h.Store, ph)
	if len(hosts) == 0 {
		// Distinct outcome from "host present but out of scope" — an
		// operator reading audit shouldn't need to parse the reason
		// string to tell "placeholder has no bound service" from
		// "request asked for a non-allowlisted host."
		h.denyMint(w, r.Context(), agent, partialMintSess(), http.StatusForbidden, "placeholder_has_no_bound_service", "placeholder has no bound-service hosts configured: "+hostReason)
		return
	}
	// Strip the port before BoundaryCheck to match the resolver's
	// path (proxy_resolver.go's swap path does the same). Allowlist
	// entries are bare hostnames (e.g. "api.github.com"), so a port-
	// bearing target_host like "api.github.com:443" would be rejected
	// here even though the resolver would accept the equivalent
	// request — leaving the mint endpoint stricter than the runtime
	// boundary. normalizeScriptSession (called inside Mint) strips
	// the port from the stored session too, so the post-mint
	// snapshot uses the bare host.
	boundaryHost := body.TargetHost
	if hostOnly, _, err := net.SplitHostPort(boundaryHost); err == nil {
		boundaryHost = hostOnly
	}
	if ok, reason := inspector.BoundaryCheck(inspector.Verdict{IsAPICall: true, Host: boundaryHost}, hosts); !ok {
		h.denyMint(w, r.Context(), agent, partialMintSess(), http.StatusForbidden, "target_host_not_bound", "target host not in placeholder's bound-service allowlist: "+reason)
		return
	}

	// Task scope: a placeholder bound to a task carries the
	// authoritative purpose / expected-use that the verifier should
	// evaluate against. A placeholder without a task short-circuits to
	// "deterministic checks only" — the agent's `why` is still recorded.
	var task *store.Task
	if ph.TaskID != "" {
		task, err = h.Store.GetTask(r.Context(), ph.TaskID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				h.denyMint(w, r.Context(), agent, partialMintSess(), http.StatusForbidden, "task_not_found", "placeholder is bound to a task that no longer exists")
				return
			}
			h.denyMint(w, r.Context(), agent, partialMintSess(), http.StatusServiceUnavailable, "task_lookup_failed", "task lookup failed")
			return
		}
		if task.UserID != agent.UserID || task.AgentID != agent.ID || task.Status != "active" {
			h.denyMint(w, r.Context(), agent, partialMintSess(), http.StatusForbidden, "task_not_active", "task bound to placeholder is not active for this agent")
			return
		}
		if task.ExpiresAt != nil && !task.ExpiresAt.After(now) {
			h.denyMint(w, r.Context(), agent, partialMintSess(), http.StatusForbidden, "task_expired", "task bound to placeholder has expired")
			return
		}
	}

	// Intent verification on the derived capability. Skip when no
	// verifier is configured; the deterministic checks above are still
	// authoritative. The verifier evaluates the *structured* scope —
	// `why` is supporting context and must not rescue overly broad
	// methods, hosts, prefixes, or use counts.
	if h.IntentVerifier != nil && task != nil {
		req := intent.VerifyRequest{
			TaskPurpose: task.Purpose,
			ExpectedUse: taskExpectedUse(task),
			Service:     ph.ServiceID,
			// Action names the credentialed FAN-OUT the agent is about
			// to perform, not "minting" — script-session mint is not a
			// new credential grant, it's a wrapper that batches calls
			// the task ALREADY authorized as direct credentialed curls.
			// Naming it as a mint operation confuses the verifier into
			// rejecting it as out-of-scope credential issuance even
			// when the underlying upstream calls are clearly in scope.
			Action: "autovault.credentialed_fanout",
			Params: map[string]any{
				"placeholder":   body.Placeholder,
				"target_host":   body.TargetHost,
				"methods":       methods,
				"path_prefixes": prefixes,
				"max_uses":      maxUses,
				"ttl_seconds":   int(ttl / time.Second),
			},
			Reason: body.Why,
			TaskID: task.ID,
			// ProxyLite flag selects the lite-proxy verifier variant
			// which is calibrated for per-call rationale shapes that
			// aren't a full natural-language reason (e.g. structured
			// capability descriptions like this one).
			ProxyLite: true,
			// ServiceHints tells the verifier how to evaluate this
			// request. Without this it falls back to general action-
			// scope rules and treats "autovault.*" as credential
			// issuance, rejecting workflows the task already covers.
			//
			// IMPORTANT: this verifier is the MEANINGFUL gate for
			// script-session safety. The runtime resolver enforces
			// per-request scope mechanically, but the verifier is
			// where we judge whether the agent's stated intent is
			// consistent with the task — including whether the
			// fetched data is likely to be exfiltrated to non-
			// Clawvisor destinations. Once the verifier approves,
			// the agent has wide latitude in HOW they execute the
			// fan-out (bash loops, Python scripts, pipelines, local
			// file writes), bounded only by the resolver's per-call
			// scope check.
			ServiceHints: "This request mints a Clawvisor script session — a short-lived, capped envelope that batches credentialed proxy calls under the bound credential. " +
				"It is NOT a new credential grant: the placeholder, vault item, and task scope were approved by the user separately and Clawvisor's resolver enforces those bounds on every proxied request. " +
				"Treat this verification as if the agent were about to issue up to `max_uses` direct `" + strings.Join(methods, "/") + "` calls to https://" + body.TargetHost + " under path prefix(es) " + strings.Join(prefixes, ", ") + " using the same placeholder. " +
				"\n\nALLOW when the requested host + methods + path prefixes are within what the task purpose would authorize as direct credentialed calls (e.g. a Gmail-triage task authorizing reads under /gmail/v1/users/me/messages, or a GitHub-issue-triage task authorizing reads under /repos/<owner>/<repo>/issues). " +
				"\n\nREJECT (param_scope=violation) when ANY of these apply:\n" +
				"  - The reason/`why` text mentions sending, uploading, forwarding, posting, transmitting, sharing, or otherwise routing the fetched data to a non-Clawvisor destination — third-party APIs, external storage, remote servers, other agents, email/messaging, webhooks, etc. The user did not approve data leaving their environment through this task.\n" +
				"  - The requested scope (host + methods + path_prefixes) is genuinely outside what the task purpose authorizes — e.g. write/send paths on a read-only triage task, an unrelated host, prefixes the task's stated workflow doesn't cover.\n" +
				"  - max_uses or ttl_seconds is far in excess of what the described workflow plausibly needs. The conservative baseline for a session is ~50 uses; requests well above that (especially anywhere near the hard ceiling of 200) MUST be backed by a concrete count in the agent's `why` (e.g. \"I just listed 120 thread IDs and need 120 + buffer\"). If `max_uses` exceeds 50 and the `why` doesn't state a discovered N that justifies it, reject as scope_denied.\n" +
				"\n\nDo NOT reject based on:\n" +
				"  - Anticipated execution language or tool surface (bash, python, node, etc.). The resolver enforces scope regardless of language.\n" +
				"  - Local file writes or local processing of fetched data (` > /tmp/out.jsonl`, `>> file.json`, `jq … | awk …`, in-process summarization). Writing to local files is NOT exfiltration — the data stays inside the user's environment under their existing trust boundary.\n" +
				"  - High request volume within the configured caps when the workflow plausibly involves that fan-out (e.g. 25 calls to read 25 message IDs).\n" +
				"  - Use of shell features like loops, pipelines, variable expansion. These are normal execution shapes, not security signals.",
		}
		verdict, vErr := h.IntentVerifier.Verify(r.Context(), req)
		if vErr != nil {
			h.denyMint(w, r.Context(), agent, partialMintSess(), http.StatusServiceUnavailable, "verifier_error", "intent verifier failed: "+vErr.Error())
			return
		}
		if verdict != nil && !verdict.Allow {
			// Audit only (don't go through denyMint which would write
			// a generic JSON body) — the response shape here carries
			// the full verifier verdict so the agent has actionable
			// context for what to retry with.
			h.auditMintDeny(r.Context(), agent, partialMintSess(), http.StatusForbidden, "scope_denied", "intent verifier rejected: "+verdict.Explanation)
			writeJSON(w, http.StatusForbidden, map[string]any{
				"error":             "scope_denied",
				"message":           "intent verifier rejected the requested script-session scope",
				"verifier_decision": verdict,
			})
			return
		}
	}

	sess := llmproxy.ScriptSession{
		ID:              uuid.NewString(),
		UserID:          agent.UserID,
		AgentID:         agent.ID,
		TaskID:          ph.TaskID,
		Placeholder:     body.Placeholder,
		ServiceID:      ph.ServiceID,
		TargetHost:      body.TargetHost,
		Methods:         methods,
		PathPrefixes:    prefixes,
		MaxUses:         maxUses,
		MaxRequestBytes: scriptSessionMaxRequestBytes,
		// Scale aggregate cap with max_uses so a 200-call fan-out
		// isn't artificially throttled by a 10 MiB ceiling that only
		// makes sense for small sessions. Per-request cap remains the
		// authoritative bound on any single response; aggregate
		// just sums up to what the session was sized for.
		MaxTotalBytes: scriptSessionTotalBytesFor(maxUses),
		Why:           body.Why,
		ExpiresAt:     now.Add(ttl),
	}
	token, err := h.ScriptSessions.Mint(r.Context(), sess)
	if err != nil {
		h.denyMint(w, r.Context(), agent, sess, http.StatusServiceUnavailable, "mint_failed", "could not mint script session: "+err.Error())
		return
	}

	if h.Audit != nil {
		h.Audit.LogScriptSessionMint(r.Context(), agent, sess, http.StatusOK, "allow", "minted", "")
	}

	resolverBase := strings.TrimRight(h.BaseURL, "/") + "/api/proxy"
	// Concrete copy-paste shape for the next request. Agents that
	// only see "use caller_token in X-Clawvisor-Caller" sometimes
	// fall back to calling the UPSTREAM URL directly
	// (https://gmail.googleapis.com/...) with just the placeholder
	// header — that routes through the one-shot rewrite path,
	// re-mints a nonce per call, breaks on multi-statement bash,
	// and rejects --config / loops / pipelines. Showing the
	// resolver URL + all three headers literally in the mint
	// response makes the right shape unmissable.
	examplePath := "<upstream-path>"
	if len(prefixes) > 0 {
		examplePath = strings.TrimRight(prefixes[0], "/") + "/<id-or-suffix>"
	}
	exampleCurl := "curl -sS '" + resolverBase + examplePath + "'" +
		" -H 'Authorization: Bearer " + body.Placeholder + "'" +
		" -H 'X-Clawvisor-Target-Host: " + body.TargetHost + "'" +
		" -H 'X-Clawvisor-Caller: Bearer " + token + "'"
	resp := map[string]any{
		"script_session_id":  sess.ID,
		"base_url":           resolverBase,
		"target_host":        body.TargetHost,
		"target_host_header": "X-Clawvisor-Target-Host",
		"caller_header":      "X-Clawvisor-Caller",
		"caller_token":       token,
		"placeholder":        body.Placeholder,
		"methods":            methods,
		"path_prefixes":      prefixes,
		"max_uses":           maxUses,
		"expires_at":         sess.ExpiresAt.UTC().Format(time.RFC3339),
		"max_request_bytes":  scriptSessionMaxRequestBytes,
		"max_total_bytes":    scriptSessionTotalBytesFor(maxUses),
		"example_request":    exampleCurl,
		"next_step": "Write any shape of script you want for the fan-out — `while read id; do … done`, `xargs`, Python, jq pipelines, parallel curls, multi-statement bash, etc. — as long as EVERY credentialed request inside it has the SAME shape as `example_request`: route through " + resolverBase + " (NOT the upstream host directly), include all three headers above, and substitute the upstream path under one of your `path_prefixes`. The script-session passthrough is permissive on script shape because the resolver enforces scope on every actual request. Calling the upstream URL with just the placeholder bypasses this session entirely and falls back to the one-shot rewrite path, which IS shape-restricted (no loops, no --config, no pipelines). See GET /api/control/autovault/script for full schema + error recovery.",
	}
	// Surface the task's approved tool surface so the agent stays
	// within it when executing the fan-out. Without this hint, agents
	// sometimes write the curl loop to a file via Write/Edit and try
	// to execute it from there — the inspector's boundary check
	// rejects that because the credentialed call is being emitted
	// from a tool the task didn't authorize. Echoing the names back
	// after mint nudges the agent to run the curls inline via the
	// same tool the task already covers (typically Bash).
	if names := taskApprovedToolNames(task); len(names) > 0 {
		resp["reminder_approved_tools"] = "Approved tools for this task: " + strings.Join(names, ", ") + ". Execute the credentialed curl(s) directly via one of these — emitting them from a different tool surface will fail the credential boundary check."
	}
	writeJSON(w, http.StatusOK, resp)
}

// AutovaultScriptDocs handles GET /api/control/autovault/script. Returns
// a static help payload that explains the flow. Linked from the
// injected control notice; safe to fetch outside an authenticated tool
// call (no agent-specific state).
func (h *LLMControlHandler) AutovaultScriptDocs(w http.ResponseWriter, r *http.Request) {
	resolverBase := strings.TrimRight(h.BaseURL, "/") + "/api/proxy"
	writeJSON(w, http.StatusOK, map[string]any{
		"name":        "clawvisor-autovault-script",
		"description": "Credentialed scripts call Clawvisor's proxy with a short-lived script session token, not the upstream API directly. Clawvisor mints the token after verifying the requested scope is within the active task; the resolver enforces scope on every request.",
		"when_to_use": "≥ 3 credentialed requests to the SAME host using the SAME placeholder under the SAME task (e.g. listing then fetching N Gmail message metadatas, paging through GitHub issues). One-shot credentialed calls (single message fetch, one-off API hit) DO NOT need a session — use direct `curl` with the placeholder in Authorization and let Clawvisor rewrite it.",
		"flow": []string{
			"1. Create a task that declares the credential and the tool you intend to use (see /control/skill). The task is what the verifier evaluates the session's scope against.",
			"2. Run the DISCOVERY/LIST call first as a normal one-shot credentialed curl (e.g. GET /messages?q=…) to learn the actual fan-out size N. Don't guess N — read it from the list response.",
			"3. POST /api/control/autovault/script-session with `{placeholder, target_host, methods, path_prefixes, max_uses, ttl_seconds, why}`. Set `max_uses` to N + a small retry buffer (~2-3), not the hard cap. Set `path_prefixes` to the narrowest prefix that covers the follow-up calls.",
			"4. Take the returned `caller_token`. Send it in `X-Clawvisor-Caller: Bearer <caller_token>` on every follow-up request.",
			"5. Send the `autovault_…` placeholder in `Authorization: Bearer <placeholder>` as usual; Clawvisor swaps it server-side.",
			"6. Send `X-Clawvisor-Target-Host: <target_host>` on every follow-up request so the resolver knows which upstream to dial.",
		},
		"sizing_guidance": map[string]string{
			"too_small": "Mid-flight `SCRIPT_SESSION_EXHAUSTED`. You'll have to mint a fresh session, which costs another verifier round-trip and may surface another approval prompt to the user.",
			"too_large": "The verifier evaluates `max_uses` against the task's stated workflow and the count you state in `why`. Asking for many more than your workflow needs — or asking for >50 without naming a discovered N in `why` — triggers `scope_denied`. Match the request to the discovered N (plus a small retry buffer).",
			"right_size": "N (the count from the discovery call) plus 2-3 for retries. If you don't know N, you're not ready to mint yet.",
		},
		"hard_limits": map[string]any{
			"ttl_seconds":       scriptSessionMaxTTLSeconds,
			"max_uses":          scriptSessionMaxUses,
			"max_request_bytes": scriptSessionMaxRequestBytes,
			"max_total_bytes_formula": "max(" + strconv.Itoa(scriptSessionMinTotalBytes) + ", max_uses × max_request_bytes) — aggregate cap scales with your requested fan-out so a 200-call session isn't artificially throttled by a small static ceiling.",
			"max_total_bytes_floor": scriptSessionMinTotalBytes,
			"methods":           AllowedScriptSessionMethods,
			"target_hosts_per_session": 1,
			"placeholders_per_session": 1,
		},
		"endpoints": map[string]any{
			"mint": map[string]string{
				"method": "POST",
				"path":   "/api/control/autovault/script-session",
			},
		},
		"example_request": map[string]any{
			// Agent-facing URL convention: the rewriter intercepts
			// https://clawvisor.local/control/... and forwards to the
			// real /api/control/... handler. The agent emits the
			// synthetic URL; it should never type the real /api path.
			"url":     "https://" + llmproxy.ControlSyntheticHost + llmproxy.ControlSyntheticPath + "/autovault/script-session",
			"method":  "POST",
			"headers": map[string]string{"Content-Type": "application/json"},
			"body": map[string]any{
				"placeholder":   "autovault_google_gmail_eric_clawvisor_com_xxxxx",
				"target_host":   "gmail.googleapis.com",
				"methods":       []string{"GET"},
				"path_prefixes": []string{"/gmail/v1/users/me/messages"},
				"max_uses":      30,
				"ttl_seconds":   120,
				"why":           "Fetch message metadata and snippets for recent Gmail inbox triage.",
			},
		},
		"example_script_request": map[string]any{
			"url":    resolverBase + "/gmail/v1/users/me/messages/<id>?format=metadata",
			"method": "GET",
			"headers": map[string]string{
				"Authorization":          "Bearer autovault_google_gmail_eric_clawvisor_com_xxxxx",
				"X-Clawvisor-Target-Host": "gmail.googleapis.com",
				"X-Clawvisor-Caller":     "Bearer cv-script-…",
			},
		},
		"error_recovery": map[string]string{
			"SCRIPT_SESSION_EXPIRED":        "TTL elapsed. Mint a new session.",
			"SCRIPT_SESSION_EXHAUSTED":      "max_uses reached. Mint a new session with appropriate budget; do not loop minting back-to-back without re-justifying scope.",
			"SCRIPT_SESSION_SCOPE_MISMATCH": "Request host/method/path/placeholder is outside the session scope. Mint a session that includes the new shape, or use the existing rewrite path for a one-off call.",
			"SCRIPT_SESSION_NOT_FOUND":      "Token unknown or revoked. Mint a fresh session.",
		},
	})
}

// taskApprovedToolNames returns the deduplicated tool_name list from
// task.ExpectedTools. Surfaced in the mint response so the agent can
// see "the task that approved this scope expects you to run via X" —
// useful when the inspector boundary check rejects a credentialed
// call emitted from a different tool surface (e.g. Write) than the
// task declared. Best-effort parse: malformed JSON or missing field
// returns nil, callers decide whether to omit the field.
func taskApprovedToolNames(task *store.Task) []string {
	if task == nil || len(task.ExpectedTools) == 0 {
		return nil
	}
	var raw []struct {
		ToolName string `json:"tool_name"`
	}
	if err := json.Unmarshal(task.ExpectedTools, &raw); err != nil {
		return nil
	}
	out := make([]string, 0, len(raw))
	seen := map[string]struct{}{}
	for _, t := range raw {
		name := strings.TrimSpace(t.ToolName)
		if name == "" {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out
}

// taskExpectedUse extracts a flat string describing the task's
// authorized actions for the verifier prompt. Mirrors envelope.go's
// shape but keeps this handler independent of the inline-task plumbing.
func taskExpectedUse(task *store.Task) string {
	if task == nil || len(task.AuthorizedActions) == 0 {
		return ""
	}
	parts := make([]string, 0, len(task.AuthorizedActions))
	for _, a := range task.AuthorizedActions {
		service := strings.TrimSpace(a.Service)
		action := strings.TrimSpace(a.Action)
		switch {
		case service != "" && action != "":
			parts = append(parts, service+":"+action)
		case service != "":
			parts = append(parts, service)
		case action != "":
			parts = append(parts, action)
		}
	}
	return strings.Join(parts, ", ")
}

func normalizeMintMethods(in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, errors.New("methods is required (allowed: " + strings.Join(AllowedScriptSessionMethods, ", ") + ")")
	}
	allowed := map[string]struct{}{}
	for _, m := range AllowedScriptSessionMethods {
		allowed[m] = struct{}{}
	}
	out := make([]string, 0, len(in))
	seen := map[string]struct{}{}
	for _, m := range in {
		m = strings.ToUpper(strings.TrimSpace(m))
		if m == "" {
			continue
		}
		if _, ok := allowed[m]; !ok {
			return nil, errors.New("method " + m + " is not allowed; permitted: " + strings.Join(AllowedScriptSessionMethods, ", "))
		}
		if _, dup := seen[m]; dup {
			continue
		}
		seen[m] = struct{}{}
		out = append(out, m)
	}
	if len(out) == 0 {
		return nil, errors.New("methods is required")
	}
	sort.Strings(out)
	return out, nil
}

func normalizeMintPrefixes(in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, errors.New("path_prefixes is required")
	}
	if len(in) > scriptSessionMaxPathPrefixes {
		return nil, errors.New("too many path_prefixes; max " + strconv.Itoa(scriptSessionMaxPathPrefixes))
	}
	out := make([]string, 0, len(in))
	seen := map[string]struct{}{}
	for _, p := range in {
		normalized, err := llmproxy.NormalizeScriptSessionPathPrefix(p)
		if err != nil {
			return nil, errors.New("invalid path prefix " + p + ": " + err.Error())
		}
		if _, dup := seen[normalized]; dup {
			continue
		}
		seen[normalized] = struct{}{}
		out = append(out, normalized)
	}
	// Reject redundant prefix sets — if one prefix is already covered
	// by another, the agent should submit only the broader one. Keeps
	// scope unambiguous in the audit row and verifier prompt.
	//
	// Sort first so the pair scan visits items in a deterministic
	// order and the error message names the broader/narrower pair
	// consistently across runs (cubic round-4 P2 #2). Without the
	// pre-sort, iteration order over the map-deduped slice was
	// arbitrary and the error message could flip the (covered_by,
	// covered) pair.
	//
	// O(N²) pair scan. Acceptable because scriptSessionMaxPathPrefixes
	// caps N at 5 (validated above), so worst-case 20 comparisons per
	// mint. If the cap ever grows, switch to a trie or a sort-then-
	// linear-scan: the rule is "no prefix is the prefix of another,"
	// which becomes O(N log N) once `out` is sorted.
	sort.Strings(out)
	for i, a := range out {
		for j, b := range out {
			if i == j {
				continue
			}
			if llmproxy.ScriptSessionPathPrefixMatch(a, b) {
				return nil, errors.New("redundant path_prefix " + b + " is already covered by " + a)
			}
		}
	}
	return out, nil
}

// scriptSessionTotalBytesFor scales the aggregate byte cap with the
// requested max_uses: each call may consume up to
// scriptSessionMaxRequestBytes, so the natural ceiling on aggregate
// bytes is the product. We floor at scriptSessionMinTotalBytes so a
// 3-use session still has reasonable aggregate headroom for any
// individual response that exceeds expectation.
func scriptSessionTotalBytesFor(maxUses int) int64 {
	scaled := int64(maxUses) * int64(scriptSessionMaxRequestBytes)
	if scaled < int64(scriptSessionMinTotalBytes) {
		return int64(scriptSessionMinTotalBytes)
	}
	return scaled
}

func clampMintMaxUses(raw int) (int, error) {
	if raw <= 0 {
		return 0, errors.New("max_uses must be > 0")
	}
	if raw > scriptSessionMaxUses {
		return 0, errors.New("max_uses must be <= " + strconv.Itoa(scriptSessionMaxUses))
	}
	return raw, nil
}

func clampMintTTL(raw int) (time.Duration, error) {
	if raw <= 0 {
		return 0, errors.New("ttl_seconds must be > 0")
	}
	if raw > scriptSessionMaxTTLSeconds {
		return 0, errors.New("ttl_seconds must be <= " + strconv.Itoa(scriptSessionMaxTTLSeconds))
	}
	return time.Duration(raw) * time.Second, nil
}

// auditMintDeny emits a script-session mint audit row with decision=deny
// without writing the HTTP response. Used by the verifier-denial branch
// which needs a custom response body carrying the verdict explanation.
// `sess` may be partially populated — empty fields just render as empty
// strings in the audit params, which is intentional for early-deny rows
// that don't yet have a full session shape.
func (h *LLMControlHandler) auditMintDeny(ctx context.Context, agent *store.Agent, sess llmproxy.ScriptSession, status int, outcome, reason string) {
	if h.Audit == nil {
		return
	}
	h.Audit.LogScriptSessionMint(ctx, agent, sess, status, "deny", outcome, reason)
}

// denyMint is the standard mint-handler rejection path: emits a mint
// audit row with decision=deny + the given outcome code, then writes
// a generic JSON error body to the client. Use it from every deny site
// so the audit trail is complete (cubic P2 #2). The verifier-denial
// branch uses auditMintDeny + writeJSON directly because it needs a
// richer response shape.
func (h *LLMControlHandler) denyMint(w http.ResponseWriter, ctx context.Context, agent *store.Agent, sess llmproxy.ScriptSession, status int, code, msg string) {
	h.auditMintDeny(ctx, agent, sess, status, code, msg)
	writeMintErr(w, status, code, msg)
}

func writeMintErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error":   code,
		"message": msg,
	})
}
