# Clawvisor — Claude Code Integration Guide

You are installing the Clawvisor skill into a user's project so that Claude
Code can make gated API requests (Gmail, Calendar, Drive, GitHub, Slack, etc.)
through bash/curl — with task-scoped authorization and human approval. Follow
these instructions step by step. Ask the user for clarification when the
environment is ambiguous — do not guess silently.

---

## Goal State

When setup is complete, the user should have:

1. Clawvisor running (locally or in the cloud)
2. An agent token created for Claude Code
3. The Clawvisor skill installed globally at `~/.claude/skills/clawvisor/SKILL.md`
4. Environment variables (`CLAWVISOR_URL`, `CLAWVISOR_AGENT_TOKEN`) in `~/.claude/settings.json`
5. Claude Code able to make gateway requests via the skill

---

## Step 1: Verify Clawvisor is running

Check if Clawvisor is reachable:

```bash
curl -sf http://localhost:25297/ready 2>/dev/null && echo "RUNNING" || echo "NOT RUNNING"
```

If running, store the URL as `$CLAWVISOR_URL` (default `http://localhost:25297`).

If not running, ask the user:
- **"Is Clawvisor running somewhere else?"** — they may have a cloud instance
  or a different port. If so, get the URL and verify with
  `curl -sf $CLAWVISOR_URL/ready`.
- **"Would you like to set it up now?"** — point them to
  [SETUP.md](SETUP.md) for setup options.

Do not proceed until Clawvisor is reachable.

---

## Step 2: Create an agent token

Check if the user already has a token. If they provide one, skip to Step 3.

Otherwise, create one:

```bash
clawvisor agent create claude-code --replace --json
```

If running in Docker instead:

```bash
docker exec clawvisor /clawvisor agent create claude-code --replace --json
```

Save the `token` value from the JSON output — it is shown only once.

---

## Step 3: Install the skill globally

The skill is installed globally to `~/.claude/skills/clawvisor/SKILL.md` during
`clawvisor setup` (or `clawvisor connect-agent`). Verify it's present:

```bash
ls ~/.claude/skills/clawvisor/SKILL.md
```

If missing, re-run `clawvisor connect-agent claude-code` to install it
automatically.

The YAML frontmatter must be preserved — Claude Code uses it to recognize the
skill name, description, and required environment variables.

---

## Step 4: Set environment variables

Claude Code needs two environment variables:

| Variable | Value |
|---|---|
| `CLAWVISOR_URL` | The Clawvisor instance URL (e.g. `http://localhost:25297`) |
| `CLAWVISOR_AGENT_TOKEN` | The agent bearer token from Step 2 |

### Option A: `~/.claude/settings.json` (recommended)

Add both variables to the `env` object in `~/.claude/settings.json`. This
persists across all sessions and all projects automatically:

```json
{
  "env": {
    "CLAWVISOR_URL": "http://localhost:25297",
    "CLAWVISOR_AGENT_TOKEN": "<token from Step 2>"
  }
}
```

Merge with any existing keys in the file — do not overwrite other settings.

### Option B: Shell profile (all projects)

Add to the user's `~/.zshrc` or `~/.bashrc`:

```bash
export CLAWVISOR_URL=http://localhost:25297
export CLAWVISOR_AGENT_TOKEN=<token from Step 2>
```

---

## Step 5: Verify

Confirm Claude Code can reach Clawvisor by testing the agent token:

```bash
curl -sf -H "Authorization: Bearer $CLAWVISOR_AGENT_TOKEN" \
  "$CLAWVISOR_URL/api/skill/catalog" | head -20
```

This should return a JSON catalog of available services. If it returns a 401,
the token is invalid. If it fails to connect, `CLAWVISOR_URL` is wrong or the
server isn't running.

---

## Step 6: Summary

Present the user with:

```
Clawvisor + Claude Code Setup Complete
────────────────────────────────────────
Clawvisor:  <CLAWVISOR_URL>
Agent:      claude-code
Skill:      ~/.claude/skills/clawvisor/SKILL.md
Env:        ~/.claude/settings.json
```

Explain how it works:
- Claude Code auto-loads the skill when it's relevant to the conversation, or
  the user can invoke `/clawvisor` explicitly.
- Claude creates tasks, the user approves them in the dashboard, and Claude
  makes gateway requests under the approved scope.
- **Async handling**: Claude Code doesn't receive webhook callbacks. Instead,
  submit with `POST /api/gateway/request?wait=true` — if approval is needed,
  the request blocks until approved and returns the result in a single
  round-trip. Alternatively, poll `GET /api/gateway/request/{request_id}?wait=true`
  to check status, then call `POST /api/gateway/request/{request_id}/execute`.

Remind the user to:
- Connect services in the Clawvisor dashboard under the **Services** tab
  before asking Claude to use them
- Approve tasks in the dashboard (or via Telegram) when Claude requests them
- Optionally set restrictions in the dashboard to hard-block specific actions
