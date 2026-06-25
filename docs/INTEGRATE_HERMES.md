# Clawvisor - Hermes Integration Guide

You are connecting Hermes to a running Clawvisor server via Clawvisor's HTTP
MCP endpoint. This gives Hermes access to gated API requests (Gmail, Calendar,
Drive, GitHub, Slack, etc.) with task-scoped authorization and human approval.
Follow these instructions step by step.

**Prerequisite:** Clawvisor must be running. If it isn't, set it up first -
see [SETUP.md](SETUP.md).

---

## Goal State

When setup is complete, the user should have:

1. Clawvisor running (locally or in the cloud)
2. A `clawvisor` MCP server entry in the Hermes config
3. Hermes authorized to Clawvisor through MCP OAuth
4. Hermes able to make gateway requests via Clawvisor MCP tools

---

## Step 1: Verify Clawvisor is running

```bash
curl -sf http://localhost:25297/ready 2>/dev/null && echo "RUNNING" || echo "NOT RUNNING"
```

If not running, the user needs to set it up first - see
[SETUP.md](SETUP.md).

Store the URL as `$CLAWVISOR_URL` (default `http://localhost:25297`). If the
user has a remote instance, get the URL and verify with:

```bash
curl -sf "$CLAWVISOR_URL/ready"
```

---

## Step 2: Configure Hermes

Add Clawvisor to the Hermes config file, usually `~/.hermes/config.yaml`:

```yaml
mcp_servers:
  clawvisor:
    url: "http://localhost:25297/mcp"
    auth: oauth
    tools:
      include:
        - fetch_catalog
        - create_task
        - get_task
        - expand_task
        - gateway_request
        - complete_task
      resources: false
```

Replace the URL if Clawvisor is not running locally. On first connection,
Hermes should open a browser to authorize access to Clawvisor. Clawvisor will
mint a bounded MCP agent token after the user approves the OAuth consent flow.

If Hermes runs in a separate container or remote environment, remember that
`localhost` refers to the Hermes environment. Use a URL that Hermes can reach,
for example `http://host.docker.internal:25297` from Docker Desktop, or a
public/tunneled Clawvisor URL for remote Hermes.

---

## Step 3: Reload Hermes MCP servers

Restart Hermes, or ask Hermes to reload MCP configuration:

```text
/reload-mcp
```

Hermes registers MCP tools with the server name in the tool name. For the
server name above, tools are expected to appear as names like:

- `mcp_clawvisor_fetch_catalog`
- `mcp_clawvisor_create_task`
- `mcp_clawvisor_gateway_request`

---

## Step 4: Connect services

Before Hermes can use a service, it must be activated in Clawvisor. Direct the
user to:

1. Open the Clawvisor dashboard
2. Go to the **Services** tab
3. Connect the services they want Hermes to access (Gmail, GitHub, Slack, etc.)

Each service has its own OAuth flow or API key configuration.

---

## Step 5: Verify

Ask Hermes:

```text
Use Clawvisor to fetch my available service catalog.
```

Or ask it to list available MCP tools and confirm the Clawvisor tools are
present. The `fetch_catalog` call should return the list of services activated
in Clawvisor. If it returns an auth error, repeat the MCP OAuth authorization
flow. If it fails to connect, check the Clawvisor URL from the Hermes runtime
environment.

For a real workflow, ask Hermes to use a connected account via Clawvisor, for
example:

```text
Use Clawvisor to check my Gmail for unread messages.
```

Hermes should create a task, prompt the user to approve it in the Clawvisor
dashboard, and then execute gateway requests under the approved scope.

---

## Optional: bearer-token fallback

If OAuth is unavailable or you need a simple debugging path, you can configure
Hermes with a raw Clawvisor agent token instead.

Create the token:

```bash
clawvisor-server agent create hermes --replace --json
```

If running in Docker instead:

```bash
docker exec <APP_CONTAINER> /clawvisor-server agent create hermes --replace --json
```

Then configure Hermes with the token:

```yaml
mcp_servers:
  clawvisor:
    url: "http://localhost:25297/mcp"
    headers:
      Authorization: "Bearer <token from agent create>"
```

Important: use the standard `Authorization: Bearer ...` header. Clawvisor's
MCP endpoint reads bearer auth from `Authorization`, not
`X-Clawvisor-Agent-Token`.

---

## Step 6: Summary

Present the user with:

```text
Clawvisor + Hermes Setup Complete
---------------------------------
Clawvisor:  <CLAWVISOR_URL>
MCP server: clawvisor
Config:     ~/.hermes/config.yaml
Auth:       MCP OAuth
```

Explain how it works:

- Hermes has access to Clawvisor MCP tools: `fetch_catalog`, `create_task`,
  `get_task`, `complete_task`, `expand_task`, and `gateway_request`
- Hermes creates tasks declaring what it needs, the user approves them in the
  dashboard (or via Telegram), and Hermes executes actions under the approved
  scope
- In-scope actions with `auto_execute` run immediately; others queue for
  per-request approval
- All actions are logged in the audit trail. Credentials never leave Clawvisor.

Remind the user to:

- Connect services in the Clawvisor dashboard under the **Services** tab before
  asking Hermes to use them
- Approve tasks in the dashboard (or via Telegram) when Hermes requests them
- Optionally set restrictions in the dashboard to hard-block specific actions
