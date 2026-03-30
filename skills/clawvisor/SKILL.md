---
name: clawvisor
description: >
  Route tool requests through Clawvisor for credential vaulting, task-scoped
  authorization, and human approval flows. Use for Gmail, Calendar, Drive,
  Contacts, GitHub, and iMessage (macOS). Clawvisor enforces restrictions,
  manages task scopes, and injects credentials — the agent never handles
  secrets directly.
version: 0.7.0
homepage: https://github.com/clawvisor/clawvisor
metadata:
  {
    "openclaw":
      {
        "emoji": "🔐",
        "requires": { "env": ["CLAWVISOR_URL", "CLAWVISOR_AGENT_TOKEN", "OPENCLAW_HOOKS_URL"] },
        "primaryEnv": "CLAWVISOR_AGENT_TOKEN",
      },
  }
---

# Clawvisor Skill

## Setup

### Option A: Setup URL (recommended)

If you were given a setup URL (e.g. `https://relay.clawvisor.com/d/<id>/skill/setup`), fetch it — it contains step-by-step instructions with your `CLAWVISOR_URL` pre-filled, including how to register, authenticate, and set up E2E encryption.

### Option B: Manual setup

1. Set `CLAWVISOR_URL` to your Clawvisor instance URL (e.g. `http://localhost:25297`)
2. Create an agent in the Clawvisor dashboard, copy the token, then run: `openclaw credentials set CLAWVISOR_AGENT_TOKEN`
3. Set `OPENCLAW_HOOKS_URL` to your OpenClaw gateway's reachable URL (default `http://localhost:18789`)
4. Activate any services you want the agent to use (Gmail, GitHub, etc.) in the dashboard under Services
5. Set dashboard policies to require approval for write/send/delete actions — only enable `auto_execute` for read-only actions you trust the agent to perform unsupervised
6. If connecting through the cloud relay, use the bundled `e2e.mjs` helper or `cvis-e2e` binary — all requests through the relay require E2E encryption

> ⚠️ **`CLAWVISOR_AGENT_TOKEN` is a high-privilege credential.** It grants the agent access to every service activated in Clawvisor. Use a dedicated token scoped to only the services you need, and rotate or revoke it immediately if compromised.

---

## Overview

Clawvisor is a gatekeeper between you and external services. Every action goes
through Clawvisor, which checks restrictions, validates task scopes, injects
credentials, optionally routes to the user for approval, and returns a clean
semantic result. You never hold API keys.

The authorization model has two layers — applied in order:
1. **Restrictions** — hard blocks the user sets. If a restriction matches, the action is blocked immediately.
2. **Tasks** — scopes you declare. Every request must be attached to an approved task. If the action is in scope with `auto_execute`, it runs without approval. Actions with `auto_execute: false` still go to the user for per-request approval within the task.

---

## Typical Flow

1. Fetch the catalog — confirm the service is active and the action isn't restricted
2. Create a task with `POST /api/tasks?wait=true` — this blocks until the user approves
3. Make gateway requests with `POST /api/gateway/request?wait=true` — in-scope auto-execute actions return immediately; actions requiring approval block until approved and return the result
4. Mark the task complete when done

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

## Task-Scoped Access

Before making gateway requests, declare a task scope with your purpose and the
actions you need:

```bash
curl -s -X POST "$CLAWVISOR_URL/api/tasks?wait=true" \
  -H "Authorization: Bearer $CLAWVISOR_AGENT_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "purpose": "Review last 30 iMessage threads and classify reply status",
    "authorized_actions": [
      {"service": "apple.imessage", "action": "list_threads", "auto_execute": true, "expected_use": "List recent iMessage threads to find ones needing replies"},
      {"service": "apple.imessage", "action": "get_thread", "auto_execute": true, "expected_use": "Read individual thread messages to classify reply status"}
    ],
    "expires_in_seconds": 1800
  }'
```

- **`purpose`** — shown to the user during approval and used by intent verification to ensure requests stay consistent with declared intent. Be specific.
- **`expected_use`** — per-action description of how you'll use it. Shown during approval and checked by intent verification against your actual request params. Be specific: "Fetch today's calendar events" is better than "Use the calendar API."
- **`auto_execute`** — `true` runs in-scope requests immediately; `false` still requires per-request approval (use for destructive actions like `send_message`).
- **`expires_in_seconds`** — task TTL. Omit and set `"lifetime": "standing"` for a task that persists until the user revokes it (see below).
- **`callback_url`** — *(optional, only if callbacks are configured)* Clawvisor posts task lifecycle events here. Otherwise, long-poll with `GET /api/tasks/{id}?wait=true`.

All tasks start as `pending_approval` — the user is notified to approve the
scope before it becomes active. **Always use `?wait=true`** on `POST /api/tasks`
to block until the task is approved or denied in a single round-trip. If the
timeout elapses while still pending, long-poll `GET /api/tasks/{id}?wait=true`
until `status` changes to `active` (or `denied`).

### Standing tasks

For recurring workflows, create a **standing task** that does not expire:

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

### Chain context verification

Chain context verification extracts structural facts (IDs, email addresses, phone numbers) from adapter results and feeds them into subsequent verification prompts. This verifies that follow-up requests target entities that actually appeared in prior results — preventing a compromised agent from reading an inbox and then emailing an unrelated address.

**Ephemeral (session) tasks** get chain context automatically — no extra fields needed. The task ID is used to scope facts.

**Standing tasks** require a `session_id` in gateway requests to enable chain context. Use a consistent `session_id` (e.g., a UUID you generate once per workflow) across all related requests in a single invocation. This scopes facts to one invocation and prevents unrelated facts from prior invocations from mixing together.

If you omit `session_id` on a standing task, chain context is disabled and intent verification will apply stricter scrutiny to entity references — any specific targets (email addresses, IDs, etc.) must be justified by the task purpose or expected use alone, not by prior results.

- Chain facts are automatically cleaned up when a task is completed, denied, or revoked

### Scope expansion

If you need an action not in the original task scope:

```bash
curl -s -X POST "$CLAWVISOR_URL/api/tasks/<task-id>/expand?wait=true" \
  -H "Authorization: Bearer $CLAWVISOR_AGENT_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "service": "apple.imessage",
    "action": "send_message",
    "auto_execute": false,
    "reason": "John Doe asked a question that warrants a reply"
  }'
```

The user will be notified to approve the expansion. With `?wait=true`, the
request blocks until approved or denied. On approval, the action is added to
the task scope and the expiry is reset.

### Completing a task

When you're done, mark the task as completed:

```bash
curl -s -X POST "$CLAWVISOR_URL/api/tasks/<task-id>/complete" \
  -H "Authorization: Bearer $CLAWVISOR_AGENT_TOKEN"
```

---

## Gateway Requests

Every gateway request must include a `task_id` from an approved task.

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
    "task_id": "<task-uuid>",
    "session_id": "<consistent UUID for multi-step flows>",
    "context": {
      "source": "user_message",
      "data_origin": null
    }
  }'
```

### Required fields

| Field | Description |
|---|---|
| `service` | Service identifier (from your catalog) |
| `action` | Action to perform on that service |
| `params` | Action-specific parameters (from your catalog) |
| `reason` | One sentence explaining why. Shown in approvals and audit log. Be specific. |
| `request_id` | A unique ID you generate (e.g. UUID). Must be unique across all your requests. |
| `task_id` | The approved task ID this request belongs to. |
| `session_id` | *(Standing tasks only)* A consistent UUID across related requests in a single invocation. Required for chain context on standing tasks. Not needed for ephemeral tasks (chain context is automatic). |

### Context fields

Always include the `context` object. All fields are optional but strongly recommended:

| Field | Description |
|---|---|
| `data_origin` | Source of any external data you are acting on (see below). |
| `source` | What triggered this request: `"user_message"`, `"scheduled_task"`, `"callback"`, etc. |
| `callback_url` | *(Only if callbacks are configured.)* URL where Clawvisor posts the result after async approval. |

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

## Handling Responses

Every response has a `status` field. Handle each case as follows:

| Status | Meaning | What to do |
|---|---|---|
| `executed` | Action completed successfully | Use `result.summary` and `result.data`. Report to the user. |
| `pending` | Awaiting human approval | Tell the user: "I've requested approval for [action]." If you used `?wait=true` on the original POST, the request is already blocking and will return the result once approved. Otherwise, call `POST /api/gateway/request/{request_id}/execute?wait=true` to block until approved and get the result. Do **not** send a new request. |
| `blocked` | A restriction blocks this action | Tell the user: "I wasn't allowed to [action] — [reason]." Do **not** retry or attempt a workaround. |
| `restricted` | Intent verification rejected the request | Your params or reason were inconsistent with the task's approved purpose. Adjust and retry with a new `request_id`. |
| `pending_task_approval` | Task not yet approved | Tell the user and long-poll `GET /api/tasks/{id}?wait=true` until approved. |
| `pending_scope_expansion` | Request outside task scope | Call `POST /api/tasks/{id}/expand` with the new action. |
| `task_expired` | Task has passed its expiry | Expand the task to extend, or create a new task. |
| `error` (`SERVICE_NOT_CONFIGURED`) | Service not yet connected | Tell the user: "[Service] isn't activated yet. Connect it in the Clawvisor dashboard." |
| `error` (`EXECUTION_ERROR`) | Adapter failed | Report the error to the user. Do not silently retry. |
| `error` (other) | Something went wrong | Report the error message to the user. Do not silently retry. |

---

## Waiting for approval

**Always use `?wait=true`** on requests that require approval. This is the
simplest and most efficient pattern — the server holds the connection until the
user decides, then returns the resolved result in a single round-trip.

### Tasks

Use `?wait=true` on `POST /api/tasks` and `POST /api/tasks/{id}/expand`. The
request blocks until the task is approved or denied. Add `&timeout=N` to control
the wait (default & max 120 seconds).

```
POST /api/tasks?wait=true&timeout=120
```

If the timeout elapses while still pending, long-poll `GET /api/tasks/{id}?wait=true`
until `status` changes — just call again to keep waiting.

### Gateway requests

Use `?wait=true` on `POST /api/gateway/request`. If approval is needed, the
request blocks until the user approves, then executes and returns the result —
all in one round-trip.

```
POST /api/gateway/request?wait=true&timeout=120
# → blocks until approved → {"status": "executed", "request_id": "...", "result": {...}}
```

If the timeout elapses while still pending, the response has `"status": "pending"`.
Call `POST /api/gateway/request/{request_id}/execute?wait=true` to resume waiting
and get the result once approved.

### Fallback endpoints

These are available if you didn't use `?wait=true` on the original POST:

- **Execute after approval:** `POST /api/gateway/request/{request_id}/execute?wait=true` blocks until approved, then executes and returns the result.
- **Read-only status:** `GET /api/gateway/request/{request_id}` returns the current status without executing. Supports `?wait=true` to block until the request leaves `pending` state.
- **Legacy dedup:** Re-sending the same gateway request with the same `request_id` returns the current status without re-executing.

---

## Callbacks (optional)

If callbacks have been configured (e.g. via OpenClaw), Clawvisor can push
status changes instead of requiring polling. **Only include `callback_url` in
requests if your environment supports receiving callbacks.**

Callbacks cover two categories: **gateway request resolutions** and **task
lifecycle changes**. Each includes a `type` field (`"request"` or `"task"`).

**Request approved (waiting for agent to execute):**
```json
{
  "type": "request",
  "request_id": "send-email-5678",
  "status": "approved",
  "audit_id": "a8f3..."
}
```
When you receive this, call `POST /api/gateway/request/{request_id}/execute` to
execute the request and get the result.

Request callback statuses: `approved`, `denied`, `timeout`, or `error`.

**Task lifecycle change:**
```json
{
  "type": "task",
  "task_id": "<task-id>",
  "status": "approved"
}
```
Task callback statuses: `approved`, `denied`, `scope_expanded`, `scope_expansion_denied`, `expired`.

### Handling callbacks

1. If you registered a callback secret, verify the `X-Clawvisor-Signature`
   header: it should equal `sha256=` + HMAC-SHA256(body, CLAWVISOR_CALLBACK_SECRET).
2. Check the `type` field: `"request"` → match `request_id` to your pending
   request. `"task"` → match `task_id` to your task.
3. For request callbacks: if `status` is `approved`, call
   `POST /api/gateway/request/{request_id}/execute` to get the result.
   If `denied`, `timeout`, or `error`, tell the user the outcome and stop.
4. For task callbacks: if `approved` or `scope_expanded`, proceed with your
   task. If `denied`, `scope_expansion_denied`, or `expired`, inform the user.

### Callback verification

Register a signing secret to verify callbacks are genuinely from Clawvisor:

```
POST $CLAWVISOR_URL/api/callbacks/register
Authorization: Bearer $CLAWVISOR_AGENT_TOKEN
```

Response: `{"callback_secret": "cbsec_..."}`

Store this as `CLAWVISOR_CALLBACK_SECRET`. Calling this endpoint again rotates
the secret.

### OpenClaw callback URL

Build your `callback_url` using the `OPENCLAW_HOOKS_URL` environment variable
and your session key (shown in `session_status` as the Session field):

```
${OPENCLAW_HOOKS_URL}/clawvisor/callback?session=<session_key>
```

---

## Authorization Model Summary

| Condition | Gateway `status` |
|---|---|
| Restriction matches | `blocked` |
| Task in scope + `auto_execute` + verification passes | `executed` |
| Task in scope + `auto_execute` + verification fails | `restricted` |
| Task in scope + `auto_execute: false` | `pending` (per-request approval) |
| Action not in task scope | `pending_scope_expansion` |
