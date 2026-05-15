# Inline Task Approval State Machine

This document describes the conversational approval flow that lets a
user turn a blocked tool approval into an inline-approved task.

## Stages

`StageTool`

- Standard pending tool approval.
- The user may reply `approve`, `deny`, or `task`.
- `approve`/`deny` are handled by `TryReleasePendingApproval`.
- `task` is handled by `RewriteTaskApprovalReply`.

`StageAwaitingTaskApproval`

- The model emitted `POST /control/tasks?surface=inline`, and the proxy
  substituted a task approval prompt back into the conversation.
- The user may reply `approve` or `deny`.
- `approve`/`deny` are handled by `RewriteInlineTaskApprovalReply`.
- If this stage reaches `TryReleasePendingApproval`, release fails
  closed with `inline_task_preprocess_missing` and leaves the hold in
  cache for retry.

## Routing

`resolveApprovalReplyAction` owns approval reply routing.

Rules:

- Explicit approval IDs target only that hold.
- Bare replies target the newest visible hold across all stages.
- `approve`/`deny` on a `StageAwaitingTaskApproval` hold route to the
  inline task approval transition.
- `approve`/`deny` on any other hold route to regular tool release.
- `task` routes to the inline task-definition transition for the
  targeted hold.

The routing function only peeks and classifies. Transition helpers own
state mutation.

## Transitions

`RewriteTaskApprovalReply`

```text
StageTool + task -> startInlineTaskDefinition
```

Effects:

- Consumes the targeted tool hold.
- Rewrites the user's `task` reply into a deterministic
  `/control/tasks?surface=inline` task-creation prompt.
- Does not create a task.
- Does not retain an orphan tool hold that could later be approved.

`Postprocess` inline task intercept

```text
model POST /control/tasks?surface=inline -> StageAwaitingTaskApproval
```

Effects:

- Parses the task definition.
- Creates an inner pending hold at `StageAwaitingTaskApproval`.
- Substitutes the inline task approval prompt into the model response.

`RewriteInlineTaskApprovalReply`

```text
StageAwaitingTaskApproval + approve -> resolveInlineTaskApproval(success or failure)
StageAwaitingTaskApproval + deny    -> resolveInlineTaskApproval(denied)
```

Effects:

- Preflights request-body rewriting before mutating cache state.
- Consumes the inner inline approval hold.
- Drops the linked outer tool hold, if present.
- On approve, attempts task creation through `InlineTaskCreator`.
- Records one `InlineApprovalOutcome` resolution record.
- Rewrites the user's reply into the canonical inline task context.

`TryReleasePendingApproval`

```text
StageTool + approve -> release tool
StageTool + deny    -> deny tool
```

Effects:

- Resolves the same hold that routing peeked by explicit ID, closing the
  peek/resolve race where a newer hold could otherwise be consumed.
- Revalidates the held tool before release.
- Emits the synthetic allow/deny provider response.

## Invariants

- Bare replies always target the newest visible hold.
- Explicit IDs target only the matching hold.
- Older inline holds cannot steal newer regular tool approvals.
- Older tool holds cannot steal newer inline approvals.
- `task` on a tool hold never releases that tool.
- `approve` on an inline task hold never releases the original raw tool.
- `approve` on a tool hold never creates an inline task.
- Unsupported body rewrite shapes do not consume inline approval holds.
- Failed inline task creation is never augmented as success in later
  conversation history.
- Already-augmented historical approval messages must not leave stale
  bare approval text behind.

## Layer Ownership

- `approval_body_editor.go`: provider request-body parsing, user text
  replacement, text flattening, and history augmentation shape edits.
- `approval_reply_resolver.go`: pending-hold routing and action
  classification.
- `inline_task_transitions.go`: named transition side effects.
- `inline_task_rewrite.go`: inline approval preprocessing orchestration
  and provider-independent augmentation context.
- `task_reply.go`: `task` reply preprocessing.
- `release.go`: regular tool approval release and fail-closed guard.
- `inline_approval_outcome.go`: canonical in-memory resolution records
  used by history augmentation and diagnostics.
