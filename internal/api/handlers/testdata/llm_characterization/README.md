# LLM proxy characterization fixtures

Golden audit-row snapshots captured by `llm_characterization_test.go`. Each
file pins one observable scenario; the refactor described in
`.context/llmproxy-refactor-plan.md` must preserve these or explicitly
update them with a §10 behavior-change entry.

## Regenerating

After an intentional behavior change:

    UPDATE_LLM_CHARACTERIZATION=1 go test ./internal/api/handlers/ -run TestCharacterization

Review the diff before committing.

## Adding scenarios

When migrating a policy in Phase 3 or 4 of the refactor plan, the playbook's
step 1 is "confirm corpus coverage." If the migrated policy's observable
behavior isn't pinned by an existing scenario here, add one in the same PR.

Coverage today:

| Scenario | Pins |
|---|---|
| `passthrough_clean_anthropic` | Anthropic Messages clean baseline |
| `passthrough_clean_openai_chat` | OpenAI Chat conversation_id minting + first-turn detection |
| `control_notice_injected` | `control_notice_injected` audit flag when tools[] is declared |
| `malformed_request` | Short-circuit deny path before any policy runs |

Gaps that should be filled as their owning policies migrate:

- `secret_hold` — `preprocessLiteSecretBody` returns held=true
- `inline_task_create` — `RewriteInlineTaskApprovalReply` path
- `inline_task_approve` / `inline_task_deny`
- `tool_use_rewrite` — Inspector rewrites an autovault tool_use
- `tool_use_held` — Tool use awaiting approval
- `coalesced_approval` — multi-tool turn with shared approval_id
- `continuation` — multi-turn local intercept
