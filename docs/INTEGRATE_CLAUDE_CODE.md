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
3. The Clawvisor skill installed at `.claude/skills/clawvisor/SKILL.md` in
   their project
4. Environment variables (`CLAWVISOR_URL`, `CLAWVISOR_AGENT_TOKEN`) configured
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

Otherwise, create one. Determine how Clawvisor is running:

**If running locally (native):**

```bash
cd "$CLAWVISOR_REPO" && ./bin/clawvisor agent create claude-code --replace --json
```

**If running in Docker:**

Create the agent inside the container:

```bash
docker exec clawvisor /clawvisor agent create claude-code --replace --json
```

Save the `token` value from the JSON output — it is shown only once.

---

## Step 3: Locate the Clawvisor skill source

The skill file is at `skills/clawvisor/SKILL.md` in the Clawvisor repository.
It needs to be copied to the user's project with the OpenClaw-specific YAML
frontmatter stripped.

First, check if the Clawvisor repository is accessible locally:

```bash
ls "$CLAWVISOR_REPO/skills/clawvisor/SKILL.md" 2>/dev/null
```

If not, search common locations:

```bash
ls -d ~/code/clawvisor/skills/clawvisor/SKILL.md 2>/dev/null \
  || ls -d ~/clawvisor/skills/clawvisor/SKILL.md 2>/dev/null \
  || ls -d ~/projects/clawvisor/skills/clawvisor/SKILL.md 2>/dev/null
```

Also check for alternate naming (`clawvisor-public`, `clawvisor-oss`).

If found locally, use that path. If not, you'll fetch it from GitHub in the
next step.

---

## Step 4: Install the skill

Determine the user's project root — this is where `.claude/` lives (or will
be created). If ambiguous, ask the user.

```bash
mkdir -p "$PROJECT_ROOT/.claude/skills/clawvisor"
```

**If the Clawvisor repo is available locally:**

Strip the YAML frontmatter (the `---` delimited block at the top, which
contains OpenClaw-specific metadata) and copy:

```bash
awk 'BEGIN{s=0} /^---$/{s++;next} s>=2' "$CLAWVISOR_REPO/skills/clawvisor/SKILL.md" \
  > "$PROJECT_ROOT/.claude/skills/clawvisor/SKILL.md"
```

**If the repo is not available locally:**

Fetch from GitHub and strip frontmatter:

```bash
curl -sL https://raw.githubusercontent.com/clawvisor/clawvisor/main/skills/clawvisor/SKILL.md \
  | awk 'BEGIN{s=0} /^---$/{s++;next} s>=2' \
  > "$PROJECT_ROOT/.claude/skills/clawvisor/SKILL.md"
```

Verify the skill was installed:

```bash
head -5 "$PROJECT_ROOT/.claude/skills/clawvisor/SKILL.md"
```

The first line should be `# Clawvisor Skill` (not `---`). If it starts with
`---`, the frontmatter was not stripped — re-run the sed command.

---

## Step 5: Set environment variables

Claude Code needs two environment variables:

| Variable | Value |
|---|---|
| `CLAWVISOR_URL` | The Clawvisor instance URL (e.g. `http://localhost:25297`) |
| `CLAWVISOR_AGENT_TOKEN` | The agent bearer token from Step 2 |

Ask the user how they'd like to configure them:

### Option A: Project-level `.claude/.env` (recommended)

Write the variables to `.claude/.env` in the project root. This keeps them
scoped to the project and out of the user's global shell.

First, strip any previous Clawvisor-related lines to make this idempotent:

```bash
grep -v '^CLAWVISOR_' "$PROJECT_ROOT/.claude/.env" > /tmp/claude-env.tmp 2>/dev/null || true
mv /tmp/claude-env.tmp "$PROJECT_ROOT/.claude/.env" 2>/dev/null || true
```

Then append:

```bash
cat >> "$PROJECT_ROOT/.claude/.env" <<EOF
CLAWVISOR_URL=$CLAWVISOR_URL
CLAWVISOR_AGENT_TOKEN=<token from Step 2>
EOF
```

Check if `.claude/.env` is in `.gitignore`. If not, warn the user:

```bash
grep -q '\.claude/\.env' "$PROJECT_ROOT/.gitignore" 2>/dev/null || echo "WARNING: .claude/.env is not in .gitignore — the agent token may be committed"
```

If not ignored, offer to add it:

```bash
echo '.claude/.env' >> "$PROJECT_ROOT/.gitignore"
```

### Option B: Shell profile (all projects)

Add to the user's `~/.zshrc` or `~/.bashrc`:

```bash
export CLAWVISOR_URL=http://localhost:25297
export CLAWVISOR_AGENT_TOKEN=<token from Step 2>
```

---

## Step 6: Verify

Confirm Claude Code can reach Clawvisor by testing the agent token:

```bash
curl -sf -H "Authorization: Bearer $CLAWVISOR_AGENT_TOKEN" \
  "$CLAWVISOR_URL/api/skill/catalog" | head -20
```

This should return a JSON catalog of available services. If it returns a 401,
the token is invalid. If it fails to connect, `CLAWVISOR_URL` is wrong or the
server isn't running.

---

## Step 7: Summary

Present the user with:

```
Clawvisor + Claude Code Setup Complete
────────────────────────────────────────
Clawvisor:  <CLAWVISOR_URL>
Agent:      claude-code
Skill:      <PROJECT_ROOT>/.claude/skills/clawvisor/SKILL.md
Env:        <where vars were written>
```

Explain how it works:
- Claude Code auto-loads the skill when it's relevant to the conversation, or
  the user can invoke `/clawvisor` explicitly.
- Claude creates tasks, the user approves them in the dashboard, and Claude
  makes gateway requests under the approved scope.
- **Async handling**: Claude Code doesn't receive webhook callbacks. Instead it
  uses long-polling — when a request returns `pending` (awaiting approval),
  Claude long-polls `GET /api/gateway/request/{request_id}/status?wait=true`
  until the status changes to `approved`, then calls
  `POST /api/gateway/request/{request_id}/execute` to get the result.

Remind the user to:
- Connect services in the Clawvisor dashboard under the **Services** tab
  before asking Claude to use them
- Approve tasks in the dashboard (or via Telegram) when Claude requests them
- Optionally set restrictions in the dashboard to hard-block specific actions
