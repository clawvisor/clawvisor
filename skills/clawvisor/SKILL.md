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

Clawvisor is a gatekeeper that sits between you and external services. Every
action you take — sending an email, creating a calendar event, filing a GitHub
issue — goes through Clawvisor first. It checks policy, injects credentials,
optionally routes to the user for approval, and returns a clean semantic result.

**You never hold API keys.** Clawvisor does. This means:
- The user can revoke access at any time without giving you a new token.
- Every action is logged and auditable.
- The user can block, allow, or require approval for any action via policies.

---

## Before making any request

Check what services are available and activated on this instance:

```bash
curl -s "$CLAWVISOR_URL/api/services" \
  -H "Authorization: Bearer $CLAWVISOR_AGENT_TOKEN"
```

A service with `"status": "not_activated"` means the user has not yet connected
that service. You can still attempt a request — Clawvisor will escalate
activation to the user automatically and the response will be
`"status": "pending_activation"`.

---

## Making a request

```bash
curl -s -X POST "$CLAWVISOR_URL/api/gateway/request" \
  -H "Authorization: Bearer $CLAWVISOR_AGENT_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "service": "google.gmail",
    "action": "list_messages",
    "params": {"query": "is:unread", "max_results": 10},
    "reason": "Checking for urgent messages from the user",
    "request_id": "<unique ID you generate for this request>",
    "context": {
      "source": "user_message",
      "data_origin": null,
      "callback_url": "<your OpenClaw session inbound URL if available>"
    }
  }'
```

### Required fields

| Field | Description |
|---|---|
| `service` | Service identifier (see Available Services below) |
| `action` | Action to perform on that service |
| `params` | Action-specific parameters (see per-service docs below) |
| `reason` | One sentence explaining why you are making this request. Shown to the user in approvals and audit log. Be specific. |
| `request_id` | A unique ID you generate (e.g. UUID or `<task>-<timestamp>`). Used to correlate callbacks. Must be unique across all your requests. |

### Context fields

Always include the `context` object. All fields are optional but strongly recommended:

| Field | Description |
|---|---|
| `callback_url` | Your OpenClaw session inbound URL. Clawvisor posts the result here after async approval. Include whenever your session URL is available. |
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

## Handling responses

Every response has a `status` field. Handle each case as follows:

| Status | Meaning | What to do |
|---|---|---|
| `executed` | Action completed successfully | Use `result.summary` and `result.data`. Report to the user. |
| `blocked` | A policy explicitly blocked this action | Tell the user: "I wasn't allowed to [action] — [reason]." Do **not** retry or attempt a workaround. |
| `pending` | Action is awaiting human approval | Tell the user: "I've requested approval for [action]. You'll receive a Telegram message (or check the dashboard) to approve or deny it. I'll pick up where I left off once you respond." Do **not** retry — wait for the callback. |
| `pending_activation` | Service not yet connected | Tell the user: "[Service] isn't activated yet. Please activate it in the Clawvisor dashboard under Services, then I can retry this request." |
| `error` (code `SERVICE_NOT_CONFIGURED`) | Same as `pending_activation` | Same as above. |
| `error` (other) | Something went wrong | Report the error message to the user. Do not silently retry. |

### executed — example response

```json
{
  "status": "executed",
  "request_id": "list-emails-1234",
  "audit_id": "a8f3...",
  "result": {
    "summary": "3 unread emails found",
    "data": [
      {"id": "msg-abc", "from": "alice@example.com", "subject": "Q3 report", "snippet": "..."},
      ...
    ]
  }
}
```

Use `result.summary` for a concise description to tell the user. Use
`result.data` for the structured content. The exact shape of `data` depends
on the service and action.

### pending — saving context for the callback

When a request goes pending, note the `request_id` in your task context so you
can resume when the callback arrives:

> "Requested approval to send email to alice@example.com re: Q3 report.
> Waiting for callback on request_id: send-email-5678."

When the callback arrives (see below), look up what you were doing by
`request_id` and continue.

---

## Receiving callbacks

When Clawvisor delivers a result after approval, your OpenClaw session receives
an inbound message. Recognize it by the `[Clawvisor Result]` prefix:

```
[Clawvisor Result] request_id: send-email-5678
Status: executed
Summary: Email sent to alice@example.com
```

Or for a denial:

```
[Clawvisor Result] request_id: send-email-5678
Status: denied
Reason: User denied the approval request
```

When you see this pattern:
1. Find the pending task associated with `request_id`.
2. If `status` is `executed`, continue with the result data.
3. If `status` is `denied` or `error`, tell the user the outcome and stop.

If `callback_url` was not set when you made the request, poll for the result
by re-sending the same request with the same `request_id`. Clawvisor will
return the outcome if it has already been resolved.

---

## Available services and actions

### google.gmail
| Action | Key params | Description |
|---|---|---|
| `list_messages` | `query`, `max_results` | List messages matching a Gmail search query |
| `get_message` | `message_id` | Get full content of a message |
| `send_message` | `to`, `subject`, `body`, `cc` (opt) | Send an email |
| `create_draft` | `to`, `subject`, `body` | Create a draft without sending |
| `delete_message` | `message_id` | Move message to trash |

### google.calendar
| Action | Key params | Description |
|---|---|---|
| `list_events` | `time_min` / `from`, `time_max` / `to`, `calendar_id` (opt) | List events in a time range. Dates accepted as `"YYYY-MM-DD"` or RFC3339. |
| `get_event` | `event_id` | Get full event details |
| `create_event` | `summary`, `start`, `end`, `attendees` (opt) | Create a calendar event. `start`/`end` accept `"YYYY-MM-DD"` (all-day) or `"YYYY-MM-DDTHH:MM:SS[Z]"` (timed). |
| `update_event` | `event_id`, fields to update | Update an existing event |
| `delete_event` | `event_id` | Delete an event |

### google.drive
| Action | Key params | Description |
|---|---|---|
| `list_files` | `query`, `max_results` | List files matching a query |
| `get_file` | `file_id` | Get file metadata and content |
| `create_file` | `name`, `content`, `mime_type`, `parent_id` (opt) | Create a file |
| `update_file` | `file_id`, `content` | Update file content |
| `delete_file` | `file_id` | Move file to trash |

### google.contacts
| Action | Key params | Description |
|---|---|---|
| `list_contacts` | `query` (opt), `max_results` | List contacts |
| `get_contact` | `contact_id` | Get a contact's details |
| `create_contact` | `name`, `email`, `phone` (opt) | Create a new contact |
| `update_contact` | `contact_id`, fields to update | Update an existing contact |

### github
| Action | Key params | Description |
|---|---|---|
| `list_issues` | `owner`, `repo`, `state` (opt) | List issues on a repo |
| `get_issue` | `owner`, `repo`, `number` | Get issue details |
| `create_issue` | `owner`, `repo`, `title`, `body` | Create an issue |
| `comment_issue` | `owner`, `repo`, `number`, `body` | Add a comment to an issue |
| `list_prs` | `owner`, `repo`, `state` (opt) | List pull requests |
| `get_pr` | `owner`, `repo`, `number` | Get PR details |
| `list_repos` | `org` (opt) | List repositories |
| `search_code` | `query`, `repo` (opt) | Search code |

**Note:** GitHub uses a personal access token (PAT), not OAuth. Activate via the dashboard Services page — enter your PAT when prompted.

### apple.imessage *(macOS only)*
| Action | Key params | Description |
|---|---|---|
| `search_messages` | `query`, `contact` (opt), `days_back` (opt) | Search messages by content/sender |
| `list_threads` | `max_results` (opt) | List recent conversation threads |
| `get_thread` | `contact` or `thread_id`, `days_back` (opt) | Get messages from a conversation |
| `send_message` | `to`, `text` | Send an iMessage (always requires approval) |

**Note:** Only available on macOS with Messages.app configured. Requires Full Disk Access permission in System Settings → Privacy & Security. `send_message` always routes to approval regardless of policy.

---

## Example workflows

### Read unread email and summarise

```bash
# 1. List unread messages
curl -s -X POST "$CLAWVISOR_URL/api/gateway/request" \
  -H "Authorization: Bearer $CLAWVISOR_AGENT_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "service": "google.gmail",
    "action": "list_messages",
    "params": {"query": "is:unread", "max_results": 5},
    "reason": "Checking for unread messages at user request",
    "request_id": "list-unread-001",
    "context": {"source": "user_message", "data_origin": null, "callback_url": "..."}
  }'

# 2. Fetch a specific message (set data_origin to the message ID)
curl -s -X POST "$CLAWVISOR_URL/api/gateway/request" \
  -H "Authorization: Bearer $CLAWVISOR_AGENT_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "service": "google.gmail",
    "action": "get_message",
    "params": {"message_id": "msg-abc123"},
    "reason": "Reading email content to summarise for user",
    "request_id": "get-msg-002",
    "context": {"source": "user_message", "data_origin": "gmail:msg-abc123", "callback_url": "..."}
  }'
```

### Reply to an email (requires approval by default)

```bash
curl -s -X POST "$CLAWVISOR_URL/api/gateway/request" \
  -H "Authorization: Bearer $CLAWVISOR_AGENT_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "service": "google.gmail",
    "action": "send_message",
    "params": {
      "to": "alice@example.com",
      "subject": "Re: Q3 report",
      "body": "Thanks for sending this over. I'\''ve reviewed it and everything looks good."
    },
    "reason": "Replying to Q3 report email as requested by user",
    "request_id": "send-reply-003",
    "context": {
      "source": "user_message",
      "data_origin": "gmail:msg-abc123",
      "callback_url": "..."
    }
  }'
```

If the response is `"status": "pending"`, tell the user:

> "I've drafted a reply to Alice's Q3 report email and requested your approval.
> Check your Telegram or the Clawvisor dashboard to approve or deny it."

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
via Telegram or the dashboard. If no Telegram is configured, direct them to the
Approvals panel in the dashboard.

**"I was blocked and I don't know why"**
The `reason` field in the response explains the policy rule that matched. Pass
it to the user verbatim — don't guess or try to work around it.

---

## Policy decision vocabulary

The `/api/policies/evaluate` dry-run endpoint returns a `decision` field that
maps to the gateway response `status` as follows:

| YAML rule | `evaluate` decision | Gateway `status` |
|---|---|---|
| `allow: true` | `execute` | `executed` |
| `require_approval: true` | `approve` | `pending` |
| `allow: false` | `block` | `blocked` |
| (no matching rule) | `approve` | `pending` |

This vocabulary is also used in policy conflict reports (`opposing_decisions`,
`shadowed_rule`) and in the `reason` field returned by the gateway.
