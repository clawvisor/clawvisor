# Clawvisor — Claude Cowork Integration Guide

You are helping a user connect Claude Desktop (Cowork) to a running Clawvisor
server via MCP. This gives Claude access to gated API requests (Gmail,
Calendar, Drive, GitHub, Slack, etc.) with task-scoped authorization and human
approval. Follow these instructions step by step. Ask the user for
clarification when the environment is ambiguous — do not guess silently.

**Prerequisite:** Clawvisor must be running. If it isn't, set it up first —
see [SETUP.md](SETUP.md).

---

## Goal State

When setup is complete, the user should have:

1. Clawvisor running (locally or in the cloud)
2. The Clawvisor cowork plugin installed in Claude Desktop
3. The MCP server configured to point to their Clawvisor instance
4. OAuth authorization completed (agent token created automatically)
5. Claude Desktop able to make gateway requests via MCP tools

---

## Step 1: Verify Clawvisor is running

Ask the user if Clawvisor is running and where:
- **Locally** — default URL is `http://localhost:25297`
- **Remote server** — get the instance URL (e.g. `https://your-instance.run.app`)

If Clawvisor is not set up yet, point them to [SETUP.md](SETUP.md).

Do not proceed until the user confirms Clawvisor is reachable.

---

## Step 2: Install the Clawvisor plugin

Direct the user to install the cowork plugin in Claude Desktop:

1. Download the plugin zip:
   [clawvisor/cowork-plugin](https://github.com/clawvisor/cowork-plugin/archive/refs/heads/main.zip)

2. In Claude Desktop, go to **Settings** → **Plugins** → **Install from local source**

3. Drag the downloaded zip file into the upload area

---

## Step 3: Configure the MCP server

The plugin points to the hosted Clawvisor instance by default. Determine how
the user is running Clawvisor and configure accordingly:

### Local server

If Clawvisor is running locally, add the MCP server to Claude Desktop's
config. The user will need to give you access to edit the file:

- **macOS**: `~/Library/Application Support/Claude/claude_desktop_config.json`
  (the `Library` folder is hidden by default — the user may need to press
  **Cmd+Shift+.** in Finder to reveal it, then grant access)

Add the Clawvisor MCP server to `claude_desktop_config.json`:

```json
{
  "mcpServers": {
    "clawvisor-local": {
      "command": "npx",
      "args": ["mcp-remote", "http://localhost:25297/mcp"]
    }
  }
}
```

3. Restart Claude Desktop to pick up the new config.

### Remote server

If Clawvisor is running on a remote server (Cloud Run, etc.), update the
plugin to point to the user's instance:

1. Go to **Settings** → **Plugins** → **Manage Plugins** → **Clawvisor** → **Customize**
2. Update the MCP server URL to the user's instance
   (e.g. `https://your-instance.run.app/mcp`)

---

## Step 4: Authorize Claude

When Claude Desktop first connects to the MCP server, it triggers an OAuth
2.1 authorization flow:

1. Claude Desktop registers itself as an OAuth client with Clawvisor
2. A browser window opens showing the Clawvisor consent page
3. The user logs in and approves access
4. Claude Desktop receives an agent token and is ready to use

The agent appears in the Clawvisor dashboard under **Agents** (named
"Claude Desktop (MCP)" or similar). The user can revoke access there at any
time.

If the OAuth flow does not complete:
- Verify the user has a Clawvisor account (run `make setup` if needed)
- Check that `base_url` in `config.yaml` matches the URL Claude is
  connecting to (needed for OAuth redirect validation)

---

## Step 5: Connect services

Before Claude can use a service, it must be activated in Clawvisor. Direct
the user to:

1. Open the Clawvisor dashboard
2. Go to the **Services** tab
3. Connect the services they want Claude to access (Gmail, GitHub, Slack, etc.)

Each service has its own OAuth flow or API key configuration.

---

## Step 6: Verify

Use the `fetch_catalog` MCP tool to confirm the connection is working. It
should return the list of available services. If it returns an auth error, the
OAuth flow needs to be repeated. If it fails to connect, the MCP server URL
is wrong or Clawvisor isn't running — ask the user to check.

---

## Step 7: Summary

Present the user with:

```
Clawvisor + Claude Cowork Setup Complete
────────────────────────────────────────
Clawvisor:  <CLAWVISOR_URL>
Plugin:     clawvisor (cowork)
MCP:        <MCP server URL>
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
- Connect services in the Clawvisor dashboard under the **Services** tab
  before asking Claude to use them
- Approve tasks in the dashboard (or via Telegram) when Claude requests them
- Optionally set restrictions in the dashboard to hard-block specific actions
