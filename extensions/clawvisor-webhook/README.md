# Openclaw Clawvisor Webhook Plugin

Receives Clawvisor callback notifications (task approvals, request results) and routes them into the correct OpenClaw agent session via the Gateway WebSocket.

## How It Works

1. Clawvisor POSTs a signed JSON payload to `http://<gateway>/clawvisor/callback?session=<session_key>`
2. The plugin verifies the HMAC-SHA256 signature against the configured secret
3. It builds a wake message from the payload and delivers it to the agent session via `chat.send` over the Gateway WebSocket

## Setup

### 1. Register a callback secret with Clawvisor

```bash
curl -s -X POST "$CLAWVISOR_URL/api/callbacks/register" \
  -H "Authorization: Bearer $CLAWVISOR_AGENT_TOKEN"
```

Returns: `{"callback_secret": "cbsec_..."}`

**Important:** Calling this endpoint again rotates the secret. If you re-register, you must update the plugin config to match.

### 2. Configure the plugin in `openclaw.json`

Add the plugin to your `plugins` block:

```json
{
  "plugins": {
    "clawvisor-webhook": {
      "secret": "cbsec_..."
    }
  }
}
```

### 3. Set environment variables

Add to `~/.openclaw/.env`:

```
CLAWVISOR_URL=http://<clawvisor-host>:8080
CLAWVISOR_AGENT_TOKEN=<your-agent-bearer-token>
OPENCLAW_HOOKS_URL=http://localhost:18789
CLAWVISOR_CALLBACK_SECRET=cbsec_...
```

- `CLAWVISOR_URL` — the Clawvisor instance URL (not a secret, just a URL)
- `CLAWVISOR_AGENT_TOKEN` — agent bearer token from the Clawvisor dashboard (use `openclaw secrets configure` for secure storage)
- `OPENCLAW_HOOKS_URL` — the OpenClaw gateway URL that Clawvisor can reach for callbacks
- `CLAWVISOR_CALLBACK_SECRET` — same as the plugin `secret` above; used by the agent skill for signature verification

### 4. Restart the gateway

```bash
openclaw gateway restart
```

## Callback URL Format

When making Clawvisor gateway requests or creating tasks, set the `callback_url` to:

```
${OPENCLAW_HOOKS_URL}/clawvisor/callback?session=<session_key>
```

The `?session=` query parameter routes the callback to the correct agent session. Get your session key from `session_status` (shown as `🧵 Session: <key>`).

## Troubleshooting

### `401 Unauthorized: invalid signature`

The callback secret in the plugin config doesn't match what Clawvisor is signing with.

**Causes:**
- You re-registered the callback secret (`POST /api/callbacks/register`) but didn't update the plugin config in `openclaw.json`
- The plugin config has a stale/old secret

**Fix:**
1. Re-register: `POST $CLAWVISOR_URL/api/callbacks/register` to get the current secret
2. Update `openclaw.json` → `plugins.clawvisor-webhook.secret` with the new value
3. Also update `CLAWVISOR_CALLBACK_SECRET` in `~/.openclaw/.env` if set there
4. Restart the gateway: `openclaw gateway restart`

### `clawvisor-webhook: no secret configured`

The plugin loaded but found no `secret` in its config.

**Fix:** Add the `secret` field to the plugin config in `openclaw.json` (see Setup step 2).

### `clawvisor-webhook: no gateway token found`

The plugin can't find the gateway auth token to connect via WebSocket.

**Fix:** Ensure `gateway.auth.token` is set in `openclaw.json`.

### Callbacks not arriving at the agent session

- Verify `OPENCLAW_HOOKS_URL` is reachable from the Clawvisor instance
- Check that the `?session=` parameter in the callback URL matches your actual session key
- Look at Clawvisor logs for callback delivery errors (e.g. `non-success response`)
- Look at OpenClaw gateway logs for webhook handling errors

### Intent verification rejections (`status: restricted`)

Clawvisor's intent verification checks that your request params match the `expected_use` declared in the task. Common issues:

- **Missing date filters:** If `expected_use` says "from today" but params don't filter by date
- **Date format confusion:** The verification model may not recognize future-looking dates; use relative filters like `newer_than:1d` instead of absolute dates
- **Scope mismatch:** Params that are broader than what was declared (e.g. "all emails" vs "unread emails from today")

**Fix:** Make your params precisely match your declared intent, or adjust the `expected_use` to be broader.

## Architecture

```
Clawvisor ──POST──▶ OpenClaw Gateway ──▶ clawvisor-webhook plugin
                                              │
                                              ▼
                                     Verify HMAC signature
                                              │
                                              ▼
                                     Build wake message
                                              │
                                              ▼
                                     Gateway WS chat.send
                                              │
                                              ▼
                                     Agent session receives
                                     callback as a message
```

The plugin maintains a persistent WebSocket connection to the gateway, authenticating with an Ed25519 device keypair (auto-generated and stored in `device.json`).
