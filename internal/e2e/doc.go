// Package e2e is an in-process LLM-driven end-to-end harness for the runtime
// proxy.
//
// A scenario YAML names a goal, persona, scenario fixture (vault entries +
// runtime policy rules + httptest upstream mocks), an approver script, and
// expectations. The harness boots a real runtime proxy (matching the
// production wiring), rewires its outbound dialer to route registered hosts to
// in-process httptest servers, and runs four LLM roles against it:
//
//   - user-sim   — pursues the goal, terminates on <DONE>
//   - responder  — real Claude with a single http_request tool that goes
//                  through the proxy under test
//   - approver   — watches for pending runtime approvals and resolves each
//                  according to the scenario's script (allow_once,
//                  allow_session, allow_always, deny)
//   - judge      — grades soft expectations against the transcript and a
//                  snapshot of runtime_events / approvals
//
// Run with: go test ./internal/e2e -run TestE2E
// Requires: CLAWVISOR_E2E_ANTHROPIC_KEY (else the LLM tests skip).
//
// A no-API-key smoke test in internal/e2e/harness exercises the wiring on
// every CI invocation.
//
// # Mission coverage
//
// Each scenario declares a Mission — the runtime/policy or task/gateway code
// path it is designed to exercise.
//
// Egress-policy paths (runtime proxy → in-process httptest upstream):
//
//	morning_standup     — allow rule matched (runtime.policy.allow_matched)
//	forbidden_host      — deny rule matched (runtime.policy.deny_matched)
//	notify_with_review  — fall-through review + allow_once (one_off_consumed)
//	denied_by_approver  — fall-through review + approver denies
//	session_authorize   — fall-through review + allow_session + task promotion
//	always_authorize    — fall-through review + allow_always + standing task
//	mixed_hosts         — multi-host: allow on one, review+approve on the other
//
// Task / gateway paths (agent → in-process Clawvisor API mux at
// api.clawvisor.test → handlers.{Tasks,Gateway} → test.echo adapter):
//
//	create_task_then_call    — POST /api/tasks (kind=task_create) →
//	                           approver allow_session → POST
//	                           /api/gateway/request with task_id →
//	                           adapter echoes params back
//	task_creation_denied     — task_create + deny → agent reports failure
//	standing_task            — task_create + allow_always → standing-
//	                           lifetime task, two sequential gateway
//	                           calls under the same task
//	expand_task              — task_create allow_session, then
//	                           POST /api/tasks/{id}/expand → kind=task_expand
//	                           approval → allow_session → use the new action
//	gateway_fetches_upstream — task_create + allow_session →
//	                           gateway invokes test.echo:fetch_url which
//	                           HTTP GETs a registered upstream through the
//	                           same proxy → response body comes back through
//	                           the gateway response (full chain: agent →
//	                           gateway → adapter → proxy → upstream)
//	gateway_without_task     — POST /api/gateway/request with no task_id →
//	                           gateway classifies, returns 202 pending +
//	                           request_id (kind=request_once,
//	                           transport=execute_pending_request) → approver
//	                           allow_once → agent POSTs /execute to claim
//	per_call_review          — task with auto_execute:false → each gateway
//	                           call still gates (kind=request_once,
//	                           transport=execute_pending_request) even
//	                           though the task scope already covers it
//	task_revoke              — create → approve → use successfully → POST
//	                           /api/tasks/{id}/revoke → next call gets
//	                           409 INVALID_STATE
//
// Paths NOT yet exercised here, with the harness change each would need:
//
//	runtime.policy.review_matched      — explicit "review" rule re-reviews on
//	                                     every retry and one-offs don't apply
//	                                     on that path; useful only with a
//	                                     responder that gives up gracefully.
//	runtime.observe.would_*            — needs the runtime in observation mode
//	                                     (ObservationMode flag plumbed in
//	                                     harness/server.go).
//	runtime.tool_use.* / runtime.lease.* — needs InstallToolUseInterceptors
//	                                     wired in harness/server.go and the
//	                                     responder switched off http_request
//	                                     onto a tool-use surface.
//	runtime.autovault.*                 — needs InstallPlaceholderSwap and/or
//	                                     InstallInboundSecretCapture wired in
//	                                     harness/server.go.
//
// The probabilistic decider has unit-test coverage in
// scenario/approver_test.go (deterministic-by-seed, weighted distribution
// honored, fallback to default and to static Resolution). An end-to-end
// probabilistic scenario would need either a degenerate distribution (and
// thus duplicate a scripted scenario) or expectations tolerant to either
// outcome — not added.
package e2e
