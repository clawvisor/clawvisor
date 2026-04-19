# Clawvisor OpenClaw Plugin

Connects OpenClaw agents to a Clawvisor authorization gateway. The plugin pairs itself once (receiving a bridge token it never shares with agents) and mints per-agent tokens through a single dashboard approval.

## What it does

- **Tool proxy** — exposes 8 `clawvisor_*` tools to the agent (catalog, tasks, gateway, feedback), each authenticated with the agent's own Clawvisor token.
- **Message bridge** — forwards inbound user and outbound agent messages to Clawvisor's `/api/buffer/ingest` using a **bridge token** held only by the plugin runtime, so an agent cannot plant approval messages.
- **Auto-approval context** — when the user has enabled auto-approval on the bridge, Clawvisor consults the forwarded conversation for task approval decisions via LLM analysis. The plugin injects the current `conversation_id` into `clawvisor_create_task` calls on the agent's behalf — the agent never sets it.

No local sidecar. The plugin talks to a cloud (or self-hosted) Clawvisor instance over HTTPS.

## Two distinct tokens

The plugin deliberately uses two different bearer tokens:

| Token             | Prefix       | Held by          | Can call                                          |
| ----------------- | ------------ | ---------------- | ------------------------------------------------- |
| **Bridge token**  | `cvisbr_...` | Plugin runtime   | `/api/buffer/ingest`, `/api/plugin/agents`        |
| **Agent token**   | `cvis_...`   | Agent tool calls | `/api/tasks`, `/api/gateway/*`, `/api/feedback/*` |

A leaked bridge token cannot create tasks or execute gateway requests. A leaked agent token cannot write to the approval buffer. This split is what prevents the self-approval attack where an agent plants a fake "approved" message and then triggers a task against it.

## Config

```yaml
plugins:
  clawvisor:
    enabled: true
    url: https://clawvisor.com
    userId: <your Clawvisor user_id>   # set by the agent during /skill/setup-openclaw
    groupChatId: openclaw-default      # fallback buffer context when no conversationId
    agents: [main, researcher]         # OpenClaw agent IDs; tokens minted on pair
    # Written by the plugin after pairing — don't edit by hand:
    bridgeToken: cvisbr_xxxxxxxx
    agentTokens:
      main: cvis_xxxxxxxx
      researcher: cvis_yyyyyyyy
    installFingerprint: install_<uuid>
```

## Pairing

1. User generates a setup link from the Clawvisor dashboard (Agents page → Connect OpenClaw) and pastes it to any agent running under OpenClaw.
2. The agent fetches `GET {url}/skill/setup-openclaw?user_id=...` and follows the instructions: deposit `userId` in the plugin config and ask the user to reload the plugin.
3. On reload, the plugin detects `userId` but no `bridgeToken`, posts `POST /api/plugin/pair?wait=true` with an install fingerprint, hostname, and the configured agent IDs, and long-polls.
4. The user sees a **Plugin pairing** approval card in the Clawvisor dashboard alongside existing task approvals. The card shows the install fingerprint + hostname + agent list and has an auto-approval consent checkbox.
5. On approve, the server mints one bridge token and one agent token per agent ID and returns them in the plugin's long-poll response. The plugin writes them back to its config (if the host SDK supports `persistConfig`) or logs them for manual paste.

## Post-pair agent additions

To add a new agent after the initial pair, the plugin calls `POST /api/plugin/agents?wait=true` with its bridge token. A fresh pending approval appears in the dashboard; on approve, the new agent token is returned and written to `agentTokens`.

## Auto-approval

Auto-approval from forwarded conversations is per-bridge and off by default. Toggle it from the Agents page in the dashboard without re-pairing — the server reads `bridge_tokens.auto_approval_enabled` at task-creation time.
