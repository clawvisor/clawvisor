---
name: clawvisor
description: >
  Route tool requests through Clawvisor for policy enforcement, credential
  vaulting, and human approval flows. Use for Gmail, Calendar, Drive, Contacts,
  GitHub, and iMessage (macOS). Clawvisor enforces the user's policies and
  injects credentials — the agent never handles secrets directly.
version: 0.1.0
homepage: https://github.com/ericlevine/clawvisor-gatekeeper
metadata:
  openclaw:
    requires_env:
      - CLAWVISOR_URL          # e.g. http://localhost:8080 or https://your-instance.run.app
      - CLAWVISOR_AGENT_TOKEN  # agent bearer token from the Clawvisor dashboard
    user_setup:
      - "Set CLAWVISOR_URL to your Clawvisor instance URL"
      - "Create an agent in the Clawvisor dashboard, copy the token, then run: openclaw credentials set CLAWVISOR_AGENT_TOKEN"
      - "Activate any services you want the agent to use (Gmail, GitHub, etc.) in the dashboard under Services"
      - "Optionally create policies in the dashboard to control what the agent is allowed to do"
---

# Clawvisor Skill

Clawvisor is a gatekeeper between you and external services. Every action goes
through Clawvisor, which checks policy, injects credentials, optionally routes
to the user for approval, and returns a clean semantic result. You never hold
API keys.

---

## Getting Your Service Catalog

At the start of each session, fetch your personalized service catalog:

```
GET $CLAWVISOR_URL/api/skill/catalog
Authorization: Bearer $CLAWVISOR_AGENT_TOKEN
```

This returns the services available to you, their supported actions, which
actions require approval, and a list of services you can ask the user to
activate. Always fetch this before making gateway requests so you know what's
available and what will need human approval.

---

## Making a request

```bash
curl -s -X POST "$CLAWVISOR_URL/api/gateway/request" \
  -H "Authorization: Bearer $CLAWVISOR_AGENT_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "service": "<service_id>",
    "action": "<action_name>",
    "params": { ... },
    "reason": "One sentence explaining why",
    "request_id": "<unique ID you generate>",
    "context": {
      "source": "user_message",
      "data_origin": null,
      "callback_url": "<your session inbound URL if available>"
    }
  }'
```

### Required fields

| Field | Description |
|---|---|
| `service` | Service identifier (from your catalog) |
| `action` | Action to perform on that service |
| `params` | Action-specific parameters (from your catalog) |
| `reason` | One sentence explaining why you are making this request. Shown to the user in approvals and audit log. Be specific. |
| `request_id` | A unique ID you generate (e.g. UUID). Used to correlate callbacks. Must be unique across all your requests. |

### Context fields

Always include the `context` object. All fields are optional but strongly recommended:

| Field | Description |
|---|---|
| `callback_url` | Your session inbound URL. Clawvisor posts the result here after async approval. |
| `data_origin` | Source of any external data you are acting on (see below). |
| `source` | What triggered this request: `"user_message"`, `"scheduled_task"`, `"callback"`, etc. |

### data_origin — always populate when processing external content

`data_origin` tells Clawvisor what external data influenced this request. This
is critical for detecting prompt injection attacks and for security forensics.

**Set it to:**
- The Gmail message ID when acting on email content: `"gmail:msg-abc123"`
- The URL of a web page you fetched: `"https://example.com/page"`
- The GitHub issue URL you were reading: `"https://github.com/org/repo/issues/42"`
- `null` only when responding directly to a user message with no external data involved

**Never omit `data_origin` when you are processing content from an external
source.** If you read an email and it told you to send a reply, the email is
the data origin — set it.

---

## Task-Scoped Access (multi-step tasks)

For tasks involving multiple requests to the same service, declare a task scope
to get one approval instead of N:

```bash
curl -s -X POST "$CLAWVISOR_URL/api/tasks" \
  -H "Authorization: Bearer $CLAWVISOR_AGENT_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "purpose": "Review last 30 iMessage threads and classify reply status",
    "authorized_actions": [
      {"service": "apple.imessage", "action": "list_threads", "auto_execute": true},
      {"service": "apple.imessage", "action": "get_thread", "auto_execute": true}
    ],
    "expires_in_seconds": 1800
  }'
```

If all actions are already allowed by policy, the task goes straight to `active`
with no approval needed. Otherwise, the response status is `pending_approval` and
the user is notified to approve.

Once the task is active, include `"task_id": "<task-uuid>"` in each gateway
request to auto-execute in-scope actions without per-request approvals:

```json
{
  "task_id": "<task-uuid>",
  "service": "apple.imessage",
  "action": "get_thread",
  "params": {"thread_id": "+15551234567", "max_results": 5},
  "reason": "checking if thread needs a reply",
  "request_id": "...",
  "context": {...}
}
```

### Task response statuses

| Status | Meaning | What to do |
|---|---|---|
| `pending_task_approval` | Task declared but not yet approved | Tell the user and wait for callback or poll `GET /api/tasks/{id}`. |
| `pending_scope_expansion` | Request outside task scope | Call `POST /api/tasks/{id}/expand` with the new action. |
| `task_expired` | Task has passed its expiry | Expand the task to extend, or create a new task. |

### Scope expansion

If you need an action not in the original task scope:

```bash
curl -s -X POST "$CLAWVISOR_URL/api/tasks/<task-id>/expand" \
  -H "Authorization: Bearer $CLAWVISOR_AGENT_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "service": "apple.imessage",
    "action": "send_message",
    "auto_execute": false,
    "reason": "John Doe asked a question that warrants a reply"
  }'
```

The user will be notified to approve the expansion. On approval, the action is
added to the task scope and the expiry is reset.

### Completing a task

When you're done, mark the task as completed:

```bash
curl -s -X POST "$CLAWVISOR_URL/api/tasks/<task-id>/complete" \
  -H "Authorization: Bearer $CLAWVISOR_AGENT_TOKEN"
```

---

## Handling responses

Every response has a `status` field. Handle each case as follows:

| Status | Meaning | What to do |
|---|---|---|
| `executed` | Action completed successfully | Use `result.summary` and `result.data`. Report to the user. |
| `blocked` | A policy explicitly blocked this action | Tell the user: "I wasn't allowed to [action] — [reason]." Do **not** retry or attempt a workaround. |
| `pending` | Action is awaiting human approval | Tell the user: "I've requested approval for [action]. Check the Approvals panel in the Clawvisor dashboard to approve or deny it." Do **not** retry — wait for the callback. |
| `pending_activation` | Service not yet connected | The response includes an `activate_url` field. Tell the user: "[Service] isn't activated yet. Activate it here: [activate_url]" so they can connect it directly. |
| `error` (code `SERVICE_NOT_CONFIGURED`) | Same as `pending_activation` | Same as above — surface the `activate_url` from the response. |
| `error` (other) | Something went wrong | Report the error message to the user. Do not silently retry. |

---

## Receiving callbacks

When a `pending` request is resolved (approved, denied, or timed out),
Clawvisor POSTs a JSON payload to the `callback_url` you provided:

```json
{
  "request_id": "send-email-5678",
  "status": "executed",
  "result": { "summary": "Email sent to alice@example.com", "data": { ... } },
  "audit_id": "a8f3..."
}
```

`status` will be `executed`, `denied`, or `error`. Handle accordingly:
1. Find the pending task associated with `request_id`.
2. If `status` is `executed`, continue with the `result` data.
3. If `status` is `denied` or `error`, tell the user the outcome and stop.

**OpenClaw note:** When your `callback_url` is an OpenClaw `/hooks/agent`
endpoint, the JSON callback is delivered to your session as a text message
with a `[Clawvisor Result]` prefix. The content is the same — just formatted
as text instead of raw JSON.

### Polling when no callback_url is set

If you did not provide a `callback_url`, you can poll for the result by
re-sending the same gateway request with the same `request_id`. This is
idempotent — Clawvisor recognizes the duplicate `request_id` and returns
the current status without executing the action again.

---

## Troubleshooting

**"I get `401 Unauthorized`"**
Your agent token is invalid or missing. Check that `CLAWVISOR_AGENT_TOKEN` is
set correctly. Tokens are shown once at creation — generate a new one in the
dashboard if needed.

**"The service I need is `not_activated`"**
Connect the service in the Clawvisor dashboard under Services. For Google
services (Gmail, Calendar, Drive, Contacts), a single OAuth connection covers
all of them.

**"My request keeps returning `pending`"**
The user has a policy requiring approval for that action. They need to respond
via the Approvals panel in the dashboard.

**"I got an `EXECUTION_ERROR`"**
The action was allowed by policy but the adapter failed (e.g. invalid params,
upstream API error). The `error` field in the response has the details. Report
it to the user — do not silently retry.

**"I was blocked and I don't know why"**
The `reason` field in the response explains the policy rule that matched. Pass
it to the user verbatim — don't guess or try to work around it.

---

## Policy decision vocabulary

| YAML rule | `evaluate` decision | Gateway `status` |
|---|---|---|
| `allow: true` | `execute` | `executed` |
| `require_approval: true` | `approve` | `pending` |
| `allow: false` | `block` | `blocked` |
| (no matching rule) | `approve` | `pending` |
