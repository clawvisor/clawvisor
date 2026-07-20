# PagerDuty Setup

This guide walks you through generating a PagerDuty REST API token and connecting
PagerDuty to Clawvisor. Once configured, agents can list and read incidents, see
who is on call, browse services, and (with explicit approval) acknowledge,
resolve, or create incidents.

## Which kind of token to create

PagerDuty has **two** kinds of REST API keys:

| Token type | Where it's created | Works with Clawvisor? |
|---|---|---|
| **User token** | My Profile → User Settings → **Create API User Token** | ✅ **Use this one** |
| Account / General Access key | Integrations → API Access Keys (admin only) | ⚠️ Not recommended |

**Use a User token.** PagerDuty requires write requests (acknowledge, resolve,
create) to identify the user performing the action. A User token carries that
identity implicitly, so these actions work out of the box.

An account-level key does **not** carry a user identity — PagerDuty then requires
a `From: <email>` header on every write. Clawvisor's declarative adapter sends a
fixed set of headers and cannot inject a per-user `From` header, so write actions
will fail with an account-level key. Read actions would still work, but you'd lose
acknowledge/resolve/create. Stick with a User token.

## 1. Generate a User REST API token

1. Sign in to PagerDuty.
2. Click your avatar (top-right) → **My Profile**.
3. Open the **User Settings** tab.
4. Click **Create API User Token**.
   - If you don't see this button, your account administrator has disabled
     user tokens. Ask an admin to enable them, or have a service account user
     create the token.
5. Give it a label (e.g. `Clawvisor`) and click **Create Token**.
6. **Copy the token immediately** — PagerDuty shows it only once. It's a
   20-character string that looks like `u+ABCDEFGHIJKLMNOPQR`.

> The token inherits **your** permissions. If you can only view incidents in the
> PagerDuty UI, the token can only view them through the API. To acknowledge,
> resolve, or create incidents, your PagerDuty role must allow it (Responder or
> above on the relevant services).

## 2. Connect PagerDuty in Clawvisor

### Dashboard

1. Open the Clawvisor dashboard → **Services**.
2. Find **PagerDuty** and click **Connect**.
3. Paste your User REST API token and save.

Clawvisor verifies the token by calling `GET /users/me` and stores your account
under your PagerDuty display name.

### CLI

```bash
clawvisor-server services add pagerduty
```

Paste the token when prompted.

## 3. Available actions

| Action | Type | Notes |
|---|---|---|
| `list_incidents` | read | Filter by `statuses`, `urgencies`, `service_ids`, `since`/`until`. |
| `get_incident` | read | Pass the incident `id`. |
| `list_oncalls` | read | Who is currently on call; filter by `schedule_ids`, `user_ids`, `escalation_policy_ids`. |
| `list_services` | read | Browse services; `query` to search by name. |
| `acknowledge_incident` | write | Pass the incident `id`. Stops escalation. |
| `resolve_incident` | write | Pass the incident `id`. |
| `create_incident` | write | Pass `title` and `service_id` (from `list_services`); optional `urgency` (`high`/`low`, default `high`) and `details`. Pages the service's on-call responders. |

The four read actions are classified `read` / `low` and are safe to authorize for
auto-execution under a task. The three write actions are classified `write` /
`medium`; declare them with `auto_execute: false` so each one goes to explicit
human approval before it changes incident state or pages responders.

### Example: create an incident

The agent passes friendly fields; Clawvisor assembles PagerDuty's nested
`incident` payload for you:

```json
{
  "service": "pagerduty",
  "action": "create_incident",
  "params": {
    "title": "Checkout latency spike",
    "service_id": "PXXXXXX",
    "urgency": "high",
    "details": "p99 over 2s for 10 minutes on checkout-api"
  }
}
```

## Troubleshooting

- **`401 Unauthorized`** — the token is wrong, was revoked, or was truncated when
  pasted. Generate a fresh User token and reconnect.
- **`403 Forbidden` on a write action** — your PagerDuty role doesn't permit that
  action on that service. Ask an admin to grant Responder (or higher) access.
- **`400` mentioning a `From` header** — you connected an account-level key
  instead of a User token. Disconnect and reconnect with a User REST API token
  (see step 1).
- **Identity shows as `default`** — `GET /users/me` only resolves for User
  tokens. This is cosmetic; the connection still works for reads, but write
  actions will fail unless you switch to a User token.

## Reference

- [PagerDuty: Generate a User Token (REST API key)](https://support.pagerduty.com/docs/api-access-keys#section-generate-a-user-token-rest-api-key)
- [PagerDuty REST API reference](https://developer.pagerduty.com/api-reference/)
