# Runtime Capability Matrix

This matrix tracks the major runtime capability buckets called for by
`runtime-testing-strategy.md` and maps them to code paths, supported surfaces,
expected artifacts, expected runtime events, and the current automated/manual
coverage anchors in this repo.

`U` = unit coverage
`I` = integration coverage
`R` = replay/fixture coverage
`M` = manual acceptance coverage

| Capability | Code Path | Supported Surfaces | Modes | Persisted Artifacts | Runtime Events | Coverage |
| --- | --- | --- | --- | --- | --- | --- |
| Runtime bootstrap/session creation | `internal/runtime/proxy/manager.go`, `cmd/clawvisor/cmd_agent_runtime.go` | `agent run`, `runtime-env`, durable agent-token proxy auth | observe, enforce | `runtime_sessions` | session lifecycle only indirectly | U: `cmd/clawvisor/cmd_agent_runtime_test.go`; I: `internal/runtime/proxy/manager_test.go`, `internal/runtime/proxy/auth_test.go`; M: runtime manual pack 1/5 |
| Observe vs enforce parity | `internal/runtime/proxy/policy_hook.go`, `internal/runtime/proxy/tooluse_runtime.go` | proxy egress, tool-use | observe, enforce | audit entries, approvals | `runtime.observe.*`, runtime allow/review events | I: `internal/runtime/proxy/policy_hook_test.go`, `tooluse_runtime_test.go`; M: manual pack 1/4/5 |
| Anthropic tool-use interception | `internal/runtime/conversation/anthropic_response.go`, `internal/runtime/proxy/tooluse_runtime.go` | `api.anthropic.com /v1/messages` | observe, enforce | approvals, leases | `runtime.tool_use.*`, `runtime.observe.would_prompt_inline`, `runtime.lease.*` | U: `internal/runtime/conversation/*`; I: `internal/runtime/proxy/tooluse_runtime_test.go`; R: `internal/runtime/conversation/testdata/providers/anthropic_messages/*`; M: manual pack 6 |
| OpenAI Responses function-call interception | `internal/runtime/conversation/openai_response.go`, `internal/runtime/proxy/tooluse_runtime.go` | `api.openai.com /v1/responses` | observe, enforce | approvals, leases | `runtime.tool_use.*`, `runtime.lease.*` | U: `internal/runtime/conversation/*`; I: `internal/runtime/proxy/tooluse_runtime_test.go`; R: `internal/runtime/conversation/testdata/providers/openai_responses/*`; M: manual pack 6 |
| OpenAI Chat tool-call interception | `internal/runtime/conversation/openai_response.go` | `api.openai.com /v1/chat/completions` | observe, enforce | approvals | `runtime.tool_use.*`, `runtime.observe.would_prompt_inline` | U: `internal/runtime/conversation/response_test.go`; R: `internal/runtime/conversation/testdata/providers/openai_chat/*`; M: manual pack 6 |
| Codex responses interception | `internal/runtime/conversation/parser.go`, `openai_response.go` | `chatgpt.com /backend-api/codex/responses` | observe, enforce | approvals, leases | `runtime.tool_use.*`, `runtime.lease.*` | U: `internal/runtime/conversation/parser_test.go`; R: `internal/runtime/conversation/testdata/providers/codex_responses/*`; M: manual pack 6 |
| Held tool-use approval/release | `internal/runtime/proxy/tooluse_runtime.go`, `internal/runtime/review/cache.go` | Anthropic, OpenAI function-style, Codex function-style | observe, enforce | `approval_records`, review cache, leases | `runtime.tool_use.held`, `released`, `denied`, `review_failed` | I: `internal/runtime/proxy/tooluse_runtime_test.go`; M: manual pack 6 |
| One-off egress approval/retry | `internal/runtime/proxy/policy_hook.go`, `internal/api/handlers/runtime.go` | HTTP egress | observe, enforce | `approval_records`, `one_off_approvals` | `runtime.egress.review_required`, `one_off_created`, `one_off_consumed` | I: `internal/runtime/proxy/policy_hook_test.go`, `internal/api/handlers/runtime_test.go`; M: manual pack 7 |
| Runtime task promotion | `internal/api/handlers/runtime.go`, `runtime_controls.go` | egress approvals, event promotion | enforce | `tasks`, `active_task_sessions` | approval-created and task-promotion lineage | I: `internal/api/handlers/runtime_test.go`, `runtime_controls_test.go`; M: manual pack 7/10 |
| Runtime context-judge task binding | `internal/runtime/policy/context_judge.go`, `internal/runtime/proxy/policy_hook.go` | unmatched egress, unmatched tool-use | observe, enforce | approvals or matched task attribution | runtime allow/review events plus audit attribution | U: `internal/runtime/policy/context_judge_test.go`; I: `internal/runtime/proxy/policy_hook_test.go`; M: manual pack 7 |
| Global runtime policy allow/deny/review | `internal/runtime/policy/*`, `internal/runtime/proxy/policy_hook.go`, `tooluse_runtime.go` | egress, tool-use | observe, enforce | `runtime_policy_rules` | `runtime.policy.allow_matched`, `deny_matched`, `review_matched`, `runtime.observe.would_*` | I: proxy scenario suite; M: manual pack 10 |
| Active task binding start/end | `internal/api/handlers/tasks.go`, `internal/mcp/tools_exec.go` | runtime sessions | n/a | `active_task_sessions` | no dedicated runtime event today | I: `internal/api/handlers/tasks_runtime_start_test.go`, `internal/mcp/tools_test.go`; M: manual pack 7 |
| Lease open/close/correlation | `internal/runtime/leases`, `tooluse_runtime.go` | held/released tool execution | observe, enforce | `tool_execution_leases` | `runtime.lease.opened`, `runtime.lease.closed` | I: `internal/runtime/proxy/tooluse_runtime_test.go`; M: manual pack 12 |
| Inbound secret capture | `internal/runtime/proxy/inbound_secret_runtime.go` | supported provider request bodies | observe, enforce | `runtime_placeholders` | `runtime.autovault.captured`, `observed`, unsupported-surface event | I: `internal/runtime/proxy/inbound_secret_runtime_test.go`; M: manual pack 8 |
| Outbound placeholder swap | `internal/runtime/proxy/placeholder_runtime.go` | credential-bearing headers | observe, enforce | vault lookups, placeholders | `runtime.autovault.authorized` | I: `internal/runtime/proxy/placeholder_runtime_test.go`; M: manual pack 8/9 |
| Outbound raw credential detection | `internal/runtime/autovault/detect.go`, `placeholder_runtime.go` | outbound headers only | observe, strict | none by default | `runtime.autovault.observed` | U: detector helpers; I: `internal/runtime/proxy/placeholder_runtime_test.go`; M: manual pack 9 |
| Strict outbound credential review | `internal/runtime/proxy/placeholder_runtime.go`, `internal/api/handlers/runtime.go` | outbound headers only | strict | `approval_records`, `credential_authorizations` | `runtime.autovault.review_required`, `authorization_created` | I: `internal/runtime/proxy/placeholder_runtime_test.go`, `internal/api/handlers/runtime_test.go`; M: manual pack 9 |
| Runtime event emission | `internal/runtime/proxy/runtime_events.go`, handlers | all runtime flows | observe, enforce | `runtime_events` | all runtime event types | I: asserted across runtime proxy and handler tests; M: manual pack 4/12 |
| Unsupported provider/path observability | request-context carrier, inbound secret capture, parser matchers | unsupported provider paths, unsupported item types | observe, enforce | runtime events only | unsupported-surface event | U/R: provider fixture corpus; I: `internal/runtime/proxy/inbound_secret_runtime_test.go`; M: manual pack 4 |

## Known Current Boundaries

- OpenAI built-in Responses tool item families such as `shell_call`,
  `apply_patch_call`, `computer_call`, and `mcp_call` are intentionally locked
  as unsupported in the provider fixture corpus until first-class interception
  is added.
- Policy mutation / forensic runtime events are only partially implemented
  today. The testing strategy should still track these matrix cells so they are
  obvious when the product surfaces land.
- Replay coverage currently uses representative anonymized traces checked into
  `internal/runtime/conversation/testdata/replay/`. Expand these with real
  harness captures over time.
