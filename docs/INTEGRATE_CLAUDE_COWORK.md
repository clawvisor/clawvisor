# Clawvisor — Claude Cowork Integration Guide

You are helping a user connect Claude Desktop (Cowork) to a running Clawvisor
server via the Cowork plugin marketplace. This gives Claude access to gated
API requests (Gmail, Calendar, Drive, GitHub, Slack, etc.) with task-scoped
authorization and human approval. Follow these instructions step by step.
Ask the user for clarification when the environment is ambiguous — do not
guess silently.

**Prerequisite:** Clawvisor must be running. If it isn't, set it up first —
see [SETUP.md](SETUP.md).

---

## Goal State

When setup is complete, the user should have:

1. Clawvisor running (locally or in the cloud)
2. The `clawvisor/cowork-plugins` marketplace added in Claude Desktop
3. The **Clawvisor** plugin (or **Clawvisor Local**) installed
4. The Clawvisor connector connected and authorized
5. Claude Desktop able to make gateway requests via Clawvisor MCP tools

---

## Step 1: Verify Clawvisor is running

Ask the user if Clawvisor is running and where:
- **Locally** — default URL is `http://localhost:25297`
- **Remote / cloud** — get the instance URL (e.g. `https://your-instance.run.app`)

If Clawvisor is not set up yet, point them to [SETUP.md](SETUP.md).

Do not proceed until the user confirms Clawvisor is reachable.

---

## Step 2: Open the plugin manager

Direct the user to:

1. Open **Claude Desktop**
2. Navigate to **Claude Cowork**
3. Click **Customize** in the sidebar
4. Press the **+** next to **Personal plugins** on the left

---

## Step 3: Add the marketplace

Under **Create plugin**, have the user select **Add marketplace** and paste:

```
clawvisor/cowork-plugins
```

---

## Step 4: Install the Clawvisor plugin

1. Switch to the **Personal** tab
2. Switch to the **cowork-plugins** tab
3. Select the plugin to install:
   - **Clawvisor** — for the hosted Clawvisor cloud account
   - **Clawvisor Local** — for a Clawvisor instance running on `localhost:25297`

---

## Step 5: Connect the Clawvisor connector

For the cloud (**Clawvisor**) plugin, the connector handles OAuth:

1. Under the **Clawvisor** plugin, select **Connectors**
2. Click the **clawvisor** connector and choose **Connect**
3. A browser window opens to the Clawvisor consent page
4. The user logs in and approves access

The agent appears in the Clawvisor dashboard under **Agents** (named
"Claude Desktop (Cowork)" or similar). The user can revoke access there at
any time.

For **Clawvisor Local**, no browser authorization is needed — the plugin
talks directly to the local daemon.

If the OAuth flow does not complete:
- Verify the user has a Clawvisor account (run `make setup` if needed)
- Check that `public_url` under `server:` in `config.yaml` matches the URL
  Claude is connecting to (needed for OAuth redirect validation)

---

## Step 6: Connect accounts

Before Claude can use an integration, it must be connected in Clawvisor. Direct
the user to:

1. Open the Clawvisor dashboard
2. Go to the **Accounts** tab
3. Connect the accounts they want Claude to access (Gmail, GitHub, Slack, etc.)

Each service has its own OAuth flow or API key configuration.

---

## Step 7: Verify

Have the user create a new **Claude Cowork** session and ask the agent to use
a connected account via Clawvisor — e.g. "check my Gmail" or "list my GitHub
issues." Claude should create a task, prompt for approval, and execute
through Clawvisor.

You can also call the `fetch_catalog` MCP tool directly to confirm the
connection is working. It should return the list of available tools. If
it returns an auth error, repeat Step 5. If it fails to connect, the
connector points at the wrong instance or Clawvisor isn't running — ask the
user to check.

---

## Step 8: Summary

Present the user with:

```
Clawvisor + Claude Cowork Setup Complete
────────────────────────────────────────
Clawvisor:  <CLAWVISOR_URL>
Plugin:     Clawvisor (cowork-plugins)
```

Explain how it works:
- Claude has access to six MCP tools: `fetch_catalog`, `create_task`,
  `get_task`, `complete_task`, `expand_task`, and `gateway_request`
- Claude creates tasks declaring what it needs, the user approves them in the
  dashboard (or via Telegram), and Claude executes actions under the approved
  scope
- In-scope actions with `auto_execute` run immediately; others queue for
  per-request approval
- All actions are logged in the audit trail. Credentials never leave Clawvisor.

Remind the user to:
- Connect accounts in the Clawvisor dashboard under the **Accounts** tab
  before asking Claude to use them
- Approve tasks in the dashboard (or via Telegram) when Claude requests them
- Optionally set restrictions in the dashboard to hard-block specific actions
