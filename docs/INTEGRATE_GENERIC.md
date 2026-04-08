# Clawvisor — Generic Agent Integration Guide

You are connecting an HTTP agent to a running Clawvisor server. This guide
covers agent token creation, environment setup, and protocol reference.
Follow these instructions step by step.

**Prerequisite:** Clawvisor must be running. If it isn't, set it up first —
see [SETUP.md](SETUP.md).

---

## Goal State

When setup is complete, the user should have:

1. An agent token created in Clawvisor
2. `CLAWVISOR_URL` and `CLAWVISOR_AGENT_TOKEN` configured for the agent
3. The agent able to make gateway requests

---

## Step 1: Verify Clawvisor is running

```bash
curl -sf http://localhost:25297/ready 2>/dev/null && echo "RUNNING" || echo "NOT RUNNING"
```

If not running, the user needs to set it up first — see [SETUP.md](SETUP.md).

Store the URL as `$CLAWVISOR_URL` (default `http://localhost:25297`). If the
user has a remote instance, get the URL and verify with
`curl -sf $CLAWVISOR_URL/ready`.

---

## Step 2: Create an agent token

```bash
clawvisor agent create my-agent --replace --json
```

If running in Docker instead:

```bash
docker exec <APP_CONTAINER> /clawvisor agent create my-agent --replace --json
```

Alternatively, create an agent in the web UI under the **Agents** tab.

Save the `token` value from the output — it is shown only once.

### Optional: callback secret

If the agent supports receiving webhook callbacks, add `--with-callback-secret`
to the create command. This generates an HMAC signing secret for verifying
callback authenticity.

---

## Step 3: Configure the agent

The agent needs two environment variables:

| Variable | Value |
|---|---|
| `CLAWVISOR_URL` | The Clawvisor instance URL (e.g. `http://localhost:25297`) |
| `CLAWVISOR_AGENT_TOKEN` | The agent bearer token from Step 2 |

How to set these depends on the agent's runtime — `.env` file, shell export,
container environment, etc.

---

## Step 4: Verify

Confirm the agent can reach Clawvisor:

```bash
curl -sf -H "Authorization: Bearer $CLAWVISOR_AGENT_TOKEN" \
  "$CLAWVISOR_URL/api/skill/catalog" | head -20
```

This should return a JSON catalog of available services. A 401 response means
the token is invalid. A connection failure means `CLAWVISOR_URL` is wrong or
the server isn't running.

---

## Protocol reference

For the complete agent protocol — task creation, gateway requests, callbacks,
response statuses, and error handling — see
[`skills/clawvisor/SKILL.md`](../skills/clawvisor/SKILL.md).
