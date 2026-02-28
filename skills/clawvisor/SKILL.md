---
name: clawvisor
description: >
  Route tool requests through Clawvisor for credential vaulting, task-scoped
  authorization, and human approval flows. Use for Gmail, Calendar, Drive,
  Contacts, GitHub, and iMessage (macOS). Clawvisor enforces restrictions,
  manages task scopes, and injects credentials — the agent never handles
  secrets directly.
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
      - "Optionally create restrictions in the dashboard to block specific actions"
---

# Clawvisor Skill

Clawvisor is a gatekeeper between you and external services. Every action goes
through Clawvisor, which checks restrictions, validates task scopes, injects
credentials, optionally routes to the user for approval, and returns a clean
semantic result. You never hold API keys.

The authorization model has three layers:
1. **Restrictions** — hard blocks the user sets. If a restriction matches, the action is blocked immediately.
2. **Tasks** — pre-authorized scopes you declare. If the action is in scope with `auto_execute`, it runs without approval.
3. **Per-request approval** — the default. Any action without a covering task goes to the user for approval.

---

## Setup

### Register for callback notifications (optional)

If you use `callback_url` in your requests and want to verify that callbacks
are genuinely from Clawvisor, register a dedicated callback signing secret:

```
POST $CLAWVISOR_URL/api/callbacks/register
Authorization: Bearer $CLAWVISOR_AGENT_TOKEN
```

Response: `{"callback_secret": "cbsec_..."}`

Store this as `CLAWVISOR_CALLBACK_SECRET`. Use it to verify the
`X-Clawvisor-Signature` header on incoming callbacks (HMAC-SHA256 of the
request body). Calling this endpoint again rotates the secret.

---

## Getting Your Service Catalog

At the start of each session, fetch your personalized service catalog:

```
GET $CLAWVISOR_URL/api/skill/catalog
Authorization: Bearer $CLAWVISOR_AGENT_TOKEN
```

This returns the services available to you, their supported actions, which
actions are restricted (blocked), and a list of services you can ask the user
to activate. Always fetch this before making gateway requests so you know
what's available and what is restricted.

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
      {"service": "apple.imessage", "action": "list_threads", "auto_execute": true, "expected_use": "List recent iMessage threads to find ones needing replies"},
      {"service": "apple.imessage", "action": "get_thread", "auto_execute": true, "expected_use": "Read individual thread messages to classify reply status"}
    ],
    "expires_in_seconds": 1800,
    "callback_url": "<your session inbound URL>"
  }'
```

Include `callback_url` to receive a callback when the task is approved, denied,
or expires — otherwise you must poll `GET /api/tasks/{id}` for status changes.

#### `expected_use` — declare your intent per action

Each action in `authorized_actions` can include an `expected_use` string that
describes how you intend to use that action. This is shown to the user when
they review the task for approval, and — when intent verification is enabled —
Clawvisor checks that your actual request parameters and per-request `reason`
are consistent with your declared `expected_use` and the task's `purpose`.

Always provide `expected_use` when you can. Be specific: "Fetch today's calendar
events" is better than "Use the calendar API."

All tasks start as `pending_approval` — the user is notified to approve the task
scope before it becomes active.

#### Standing tasks

For recurring workflows, you can create a **standing task** that does not expire:

```bash
curl -s -X POST "$CLAWVISOR_URL/api/tasks" \
  -H "Authorization: Bearer $CLAWVISOR_AGENT_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "purpose": "Ongoing email triage",
    "lifetime": "standing",
    "authorized_actions": [
      {"service": "google.gmail", "action": "list_messages", "auto_execute": true, "expected_use": "List recent emails to identify ones needing attention"},
      {"service": "google.gmail", "action": "get_message", "auto_execute": true, "expected_use": "Read individual emails to triage and summarize"}
    ]
  }'
```

Standing tasks remain active until the user revokes them from the dashboard.
Session tasks (the default) expire after the configured TTL.

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
| `pending_task_approval` | Task declared but not yet approved | Tell the user and wait for the `approved` or `denied` callback (or poll `GET /api/tasks/{id}`). |
| `pending_scope_expansion` | Request outside task scope | Call `POST /api/tasks/{id}/expand` with the new action. Wait for the `scope_expanded` or `scope_expansion_denied` callback. |
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
| `blocked` | A restriction blocks this action | Tell the user: "I wasn't allowed to [action] — [reason]." Do **not** retry or attempt a workaround. |
| `restricted` | Intent verification rejected the request | Your params or reason were inconsistent with the task's approved purpose. Tell the user: "My request was restricted — [reason from response]." Adjust your params or reason and retry with a new `request_id`. |
| `pending` | Action is awaiting human approval | Tell the user: "I've requested approval for [action]. Check the Approvals panel in the Clawvisor dashboard to approve or deny it." Do **not** retry — wait for the callback. |
| `error` (code `SERVICE_NOT_CONFIGURED`) | Service not yet connected | Tell the user: "[Service] isn't activated yet. Connect it in the Clawvisor dashboard." Do **not** retry. |
| `error` (other) | Something went wrong | Report the error message to the user. Do not silently retry. |

---

## Receiving callbacks

Clawvisor sends callbacks for two categories of events: **gateway request
resolutions** and **task lifecycle changes**. Each callback includes a `type`
field (`"request"` or `"task"`) so you can distinguish them.

When a `pending` gateway request is resolved (approved, denied, or timed out),
Clawvisor POSTs a JSON payload to the `callback_url` you provided in the
gateway request:

```json
{
  "type": "request",
  "request_id": "send-email-5678",
  "status": "executed",
  "result": { "summary": "Email sent to alice@example.com", "data": { ... } },
  "audit_id": "a8f3..."
}
```

Gateway request callback statuses: `executed`, `denied`, `timeout`, or `error`.

When a task is approved, denied, expanded, or expires, Clawvisor POSTs to the
`callback_url` you provided when creating the task. The `task_id` field
contains the task ID:

```json
{
  "type": "task",
  "task_id": "<task-id>",
  "status": "approved"
}
```

Task callback statuses:

| Status | Meaning |
|---|---|
| `approved` | Task was approved and is now active |
| `denied` | Task or scope expansion was denied |
| `scope_expanded` | Scope expansion was approved; new action added |
| `scope_expansion_denied` | Scope expansion was denied; task unchanged |
| `expired` | Task expired without being completed |

All callbacks are signed when you have registered a callback secret.

Handle callbacks as follows:
1. If you registered a callback secret, verify the `X-Clawvisor-Signature`
   header: it should equal `sha256=` + HMAC-SHA256(body, CLAWVISOR_CALLBACK_SECRET).
2. Check the `type` field: `"request"` → match `request_id` to your pending
   request. `"task"` → match `task_id` to your task.
3. For request callbacks: if `status` is `executed`, continue with the `result`
   data. If `denied`, `timeout`, or `error`, tell the user the outcome and stop.
4. For task callbacks: if `approved` or `scope_expanded`, proceed with your
   task. If `denied`, `scope_expansion_denied`, or `expired`, inform the user.

### OpenClaw callback setup

If you are running as an OpenClaw agent with the `clawvisor-webhook` extension
installed, build your `callback_url` using the `hooks_url` and `session_key`
from `session_status`:

```
callback_url: "<hooks_url>/clawvisor/callback?session=<session_key>"
```

The `?session=` query parameter routes the callback to the originating
conversation so the result is delivered back to the correct session.

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
The action requires human approval. Either create a task with `auto_execute` to
skip per-request approval, or ask the user to respond via the Approvals panel
in the dashboard.

**"I got an `EXECUTION_ERROR`"**
The action was approved but the adapter failed (e.g. invalid params, upstream
API error). The `error` field in the response has the details. Report it to
the user — do not silently retry.

**"My request was `restricted`"**
Intent verification determined your request parameters or reason don't match
the task's approved purpose. Review the `reason` in the response — it explains
what was inconsistent. Adjust your params or provide a more specific reason,
then retry with a new `request_id`.

**"I was blocked and I don't know why"**
The `reason` field in the response explains the restriction that matched. Pass
it to the user verbatim — don't guess or try to work around it.

---

## Authorization model summary

| Condition | Gateway `status` |
|---|---|
| Restriction matches | `blocked` |
| Task in scope + `auto_execute` + verification passes | `executed` |
| Task in scope + `auto_execute` + verification fails | `restricted` |
| Task in scope + no `auto_execute` | `pending` (per-request approval) |
| No task | `pending` (per-request approval) |
