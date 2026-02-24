# Dynamic Skill Catalog — Implementation Spec

## Overview

The current static `GET /skill/SKILL.md` teaches agents the Clawvisor protocol. This spec
adds a complementary authenticated endpoint, `GET /api/skill/catalog`, that returns a
personalized service catalog scoped to the requesting agent's activated services and policies.

The two files serve different purposes and are fetched at different times:

| | `GET /skill/SKILL.md` | `GET /api/skill/catalog` |
|---|---|---|
| Auth required | No | Yes (agent bearer token) |
| Content | Protocol, request format, error codes | Personalized service docs |
| When fetched | Install time / first session | Start of every session |
| Changes | Rarely | When services/policies change |

---

## Changes to Static SKILL.md

The current SKILL.md is ~14KB. Most of that bulk is the "Available services and actions"
tables and the example workflows — both of which are service-specific content that belongs
in the dynamic catalog, not the static bootstrap.

### What to remove from SKILL.md

- **"Before making any request"** — the curl check for `/api/services` is superseded by the
  catalog endpoint. Remove it.
- **"Available services and actions"** — the entire per-service action tables section.
  This is the biggest chunk of the file and moves entirely to the catalog.
- **"Example workflows"** — service-specific curl examples. Remove or reduce to one minimal
  example showing request/response shape only (no service-specific params).

### What stays in SKILL.md

Everything that's service-agnostic and protocol-level:
- Frontmatter / skill metadata
- What Clawvisor is (intro — keep to 2–3 sentences)
- "Getting your service catalog" (new — see below)
- Making a request (field definitions, context fields, `data_origin` guidance)
- Handling responses (status table — `executed`, `blocked`, `pending`, etc.)
- Receiving callbacks
- Troubleshooting
- Policy decision vocabulary table

Target size after trimming: **~400–500 tokens** (roughly 3KB).

### New section to add near the top

```markdown
## Getting Your Service Catalog

At the start of each session, fetch your personalized service catalog:

    GET /api/skill/catalog
    Authorization: Bearer <your_agent_token>

This returns the services available to you, their supported actions, which actions require
approval, and a list of services you can ask the user to activate. Always fetch this before
making gateway requests so you know what's available and what will need human approval.
```

---

## New Endpoint: GET /api/skill/catalog

### Auth

Agent bearer token. Returns 401 if missing or invalid. The token identifies both the agent
and its associated user, which is enough to determine activated services and applicable policies.

### Response

`Content-Type: text/markdown; charset=utf-8`

---

### Catalog Document Structure

```markdown
# Your Clawvisor Service Catalog

## Active Services

[one section per activated service — see format below]

---

## Available Services (not activated)

[one line per supported-but-not-activated service]

To activate a service, direct the user to the Clawvisor dashboard. You can also surface the
`activate_url` field included in any `SERVICE_NOT_CONFIGURED` response — it links directly to
the activation flow for that service.
```

---

### Active Services Section Format

For each service where `status == "activated"` for this user, emit a section. Run each action
through the policy evaluator to determine what to include:

- **`execute` decision** — include with full param docs, no label
- **`approve` decision** — include with full param docs, mark `⚠️ requires approval`
- **`block` decision** — omit entirely

Example output:

```markdown
### google.gmail

**list_messages** — List and search emails
  Params: query (string, optional), max_results (int, default 10)

**get_message** — Fetch a single email by ID
  Params: message_id (string, required)

**send_message** ⚠️ requires approval — Send an email
  Params: to (string, required), subject (string, required), body (string, required),
          cc (string, optional)
```

Blocked actions are not mentioned. Agents don't need to know they exist.

---

### Available Services Section Format

For each service in the Clawvisor catalog where `status != "activated"` for this user,
emit a single line:

```markdown
- **google.drive** — Google Drive file access (list, read, upload)
- **apple.imessage** — iMessage read and send
- **github** — GitHub issues, PRs, and code review
- **slack** — Slack messaging and channel management
- **stripe** — Stripe payments, charges, and refunds
```

No action docs. No param details. Just enough for the agent to know the capability exists
and pitch it to the user if it would help them accomplish something.

---

## Implementation Notes

### Handler

Add route: `GET /api/skill/catalog` — auth middleware same as other `/api/` routes (agent
bearer token). The handler:

1. Resolves the agent and user from the token
2. Fetches the user's activated services from the services store
3. For each activated service, iterates its known actions and calls `registry.Evaluate()`
   for each to determine include/label/omit
4. Fetches the full service catalog and subtracts activated services to get the available list
5. Renders the markdown document and returns it

### Policy Evaluation

Reuse `registry.Evaluate()` exactly as the gateway handler does. No new logic needed. Pass
the agent ID so role-targeted policies are applied correctly.

### Caching

Cache the rendered document per `(user_id, agent_id)` with a TTL of 60 seconds. Invalidate
on policy create/update/delete and on service activation change for that user.

If caching is deferred, a fresh render per request is fine for now — the operation is cheap
(a handful of policy evaluations and a string render).

### Token count target

A user with 3 activated services (e.g. Gmail, Calendar, Drive) should produce a catalog
under ~800 tokens. The available services list adds ~10 tokens per service regardless of
how many are in the catalog.

---

## Example Full Output

```markdown
# Your Clawvisor Service Catalog

## Active Services

### google.gmail

**list_messages** — List and search emails
  Params: query (string, optional), max_results (int, default 10)

**get_message** — Fetch a single email by ID
  Params: message_id (string, required)

**send_message** ⚠️ requires approval — Send an email
  Params: to (string, required), subject (string, required), body (string, required),
          cc (string, optional)

### google.calendar

**list_events** — List upcoming calendar events
  Params: calendar_id (string, default "primary"), from (date YYYY-MM-DD, optional),
          to (date YYYY-MM-DD, optional), max_results (int, default 10)

**list_calendars** — List all calendars
  Params: (none)

**get_event** — Fetch a single event by ID
  Params: calendar_id (string, required), event_id (string, required)

**create_event** ⚠️ requires approval — Create a calendar event
  Params: calendar_id (string, required), summary (string, required),
          start (RFC3339 datetime, required), end (RFC3339 datetime, required),
          description (string, optional), attendees ([]string, optional)

**delete_event** ⚠️ requires approval — Delete a calendar event
  Params: calendar_id (string, required), event_id (string, required)

---

## Available Services (not activated)

- **google.drive** — Google Drive file access (list, read, upload)
- **apple.imessage** — iMessage read and send
- **github** — GitHub issues, PRs, and code review
- **slack** — Slack messaging and channel management
- **stripe** — Stripe payments, charges, and refunds
- **linear** — Linear issue and project tracking
- **notion** — Notion pages and databases

To activate a service, direct the user to the Clawvisor dashboard. You can also surface the
`activate_url` field included in any `SERVICE_NOT_CONFIGURED` response — it links directly
to the activation flow for that service.
```
