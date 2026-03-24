# Clawvisor — Setup Guide

You are helping the user set up Clawvisor. Setup has two phases: getting
Clawvisor running, then connecting an agent. Follow these instructions step
by step. Ask the user for clarification when their intent is ambiguous — do
not guess silently.

---

## Step 1: Determine how to run Clawvisor

Ask the user: **"How would you like to run Clawvisor?"**

| Option | Guide | When to recommend |
|---|---|---|
| **Local** | [SETUP_LOCAL.md](SETUP_LOCAL.md) | They have Go and Node installed, want the simplest native setup (SQLite, no Docker) |
| **Docker** | [SETUP_DOCKER.md](SETUP_DOCKER.md) | They have Docker installed, don't want to install Go/Node on the host |
| **Cloud** | [SETUP_CLOUD.md](SETUP_CLOUD.md) | They want to deploy to a remote server, VPS, or container platform |

If the user already has Clawvisor running, verify it:

```bash
curl -sf http://localhost:25297/ready 2>/dev/null && echo "RUNNING" || echo "NOT RUNNING"
```

If running, skip to Step 2. If they say it's on a different host/port, get
the URL and verify with `curl -sf $CLAWVISOR_URL/ready`.

Otherwise, follow the appropriate guide above. Return here when Clawvisor
is running and the user has dashboard access.

---

## Step 2: Connect an agent

Ask the user: **"Which agent are you connecting to Clawvisor?"**

| Option | Guide | When to recommend |
|---|---|---|
| **Claude Code** | [INTEGRATE_CLAUDE_CODE.md](INTEGRATE_CLAUDE_CODE.md) | They're using Claude Code and want to install the Clawvisor skill |
| **Claude Desktop** | [INTEGRATE_CLAUDE_COWORK.md](INTEGRATE_CLAUDE_COWORK.md) | They're using Claude Desktop and want MCP integration |
| **OpenClaw** | [INTEGRATE_OPENCLAW.md](INTEGRATE_OPENCLAW.md) | They're running OpenClaw and want webhook callbacks |
| **Other / generic** | [INTEGRATE_GENERIC.md](INTEGRATE_GENERIC.md) | Any HTTP-capable agent — covers token creation and points to the API protocol |

If the context already makes the agent choice obvious (e.g. they mentioned
Claude Code, or they're in an OpenClaw workspace), skip the question and
follow the matching guide directly.

Follow the appropriate guide. When it completes, the user has a working
Clawvisor setup with their agent connected.
