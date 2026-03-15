# Clawvisor — OpenClaw Skill

Route tool requests through [Clawvisor](https://github.com/clawvisor/clawvisor)
for policy enforcement, credential vaulting, and human approval flows.

The agent never holds API keys. Every action is logged and auditable. The user
controls what is allowed via dashboard policies.

## Supported services

- **Google Gmail** — list, read, send, draft, delete
- **Google Calendar** — list, get, create, update, delete events
- **Google Drive** — list, get, create, update, delete files
- **Google Contacts** — list, get, create, update contacts
- **GitHub** — issues, pull requests, repositories

## Quick start

**1. Run Clawvisor**

```bash
# Local (SQLite, no Docker)
git clone https://github.com/clawvisor/clawvisor
cd clawvisor
JWT_SECRET=your-secret make run-sqlite
```

Or deploy to Cloud Run — see `deploy/` in the repository.

**2. Set up your account**

Open http://localhost:25297, register, then:
- **Services** → connect Google (covers Gmail, Calendar, Drive, Contacts) and/or GitHub
- **Agents** → create an agent, copy the token
- **Policies** → optionally add policies to control what the agent can do

**3. Install the skill**

```bash
clawhub install clawvisor
```

**4. Configure credentials**

```bash
openclaw credentials set CLAWVISOR_URL http://localhost:25297
openclaw credentials set CLAWVISOR_AGENT_TOKEN <token from dashboard>
```

**5. Use it**

Ask your agent to send an email, check your calendar, create a GitHub issue —
it routes everything through Clawvisor automatically.

---

## How it works

```
Agent → POST /api/gateway/request → Policy check → Vault inject → Adapter → Result
                                          ↓
                                   Approval queue (if policy requires)
                                          ↓
                                   Dashboard / Notification → Human approves/denies
                                          ↓
                                   Callback to agent session
```

Actions that require approval (`require_approval: true` in policy, or default
for new services) go to an approval queue. The user gets a Telegram message or
can approve from the dashboard. The result is delivered back to the agent via
`callback_url`.

## Environment variables

| Variable | Description |
|---|---|
| `CLAWVISOR_URL` | Base URL of your Clawvisor instance |
| `CLAWVISOR_AGENT_TOKEN` | Agent bearer token from the dashboard — treat as a high-privilege credential |
| `OPENCLAW_HOOKS_URL` | OpenClaw gateway URL for callbacks (default: `http://localhost:18789`) |

## Links

- [Repository](https://github.com/clawvisor/clawvisor)
- [Dashboard](http://localhost:25297) (local) / your Cloud Run URL
- [Phase docs](https://github.com/clawvisor/clawvisor/tree/main/docs)
