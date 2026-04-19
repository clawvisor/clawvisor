# Clawvisor Network Proxy — Setup Guide (Stage 1, Beta)

> **Beta feature.** Stage 1 is the first release of the Clawvisor Network Proxy. It captures LLM and channel traffic at the wire level for tamper-proof auto-approval transcripts. Observe-only at this stage; enforcement (block rules, ban mechanism, policy engine) lands in Stage 3. See [docs/design-proxy-stage1.md](./design-proxy-stage1.md) for the full design.

## What this gives you

The Network Proxy is an **optional enhancement** on top of your existing Clawvisor + OpenClaw integration. When enabled on a bridge, it:

- **Captures every outbound HTTP(S) request** your OpenClaw agent makes (Anthropic API calls, Telegram messages, …) via TLS MITM.
- **Parses LLM and channel traffic** into structured `TurnEvent`s and ships them to Clawvisor's transcript store.
- **Replaces the plugin's JSONL scavenger** as the authoritative transcript source. The scavenger is tampable by a compromised agent; the proxy isn't.
- **Cross-checks** proxy-observed vs plugin-observed transcripts. Disagreements surface as anomalies in the dashboard — a tamper signal.

Stage 1 is **observation only**. It does not inject credentials (Stage 2) or enforce policy (Stage 3).

## What you're committing to

Three things, in roughly ascending order of trust:

1. **Clawvisor sees your agent's decrypted LLM and channel traffic.** Anthropic prompts and responses, Telegram message contents — all pass through the proxy in plaintext. Stays local to your machine in self-hosted mode.
2. **You install a custom CA cert** in your OpenClaw container's trust store. The agent then trusts the proxy's TLS certificates. This is the mechanism that makes MITM work; it also means anyone with access to the proxy's CA private key can impersonate any site to the agent. The key stays in the proxy container.
3. **Two containers run side-by-side with your OpenClaw gateway.** One is the Clawvisor Proxy itself (sovereign — holds the CA key, observes traffic). The other is a bootstrap container that runs once to write plugin secrets to a shared volume.

For Posture A (default: proxy by convention) a malicious agent can still bypass by unsetting `HTTP_PROXY`. For Posture B (enforced) you apply iptables rules so the agent cannot reach the network except through the proxy. See [docs/design-proxy-stage1.md §2.2](./design-proxy-stage1.md) for the honest trust-boundary table.

## Prerequisites

- A paired OpenClaw bridge (you've already run `/skill/setup-openclaw`).
- Docker running on the host machine. The Clawvisor server can run locally or remotely — the proxy needs to reach it over HTTP.
- macOS or Linux. Windows is not supported at Stage 1.

## Step-by-step setup

### 1. Enable on the dashboard

Visit the Agents page of your Clawvisor dashboard and find the bridge you want to opt in. Under that bridge's row, click **Enable Network Proxy**.

The dashboard will display a one-time bundle:

- **A `cvisproxy_...` token** — shown ONCE. Copy it somewhere safe.
- **A `docker-compose.yml` template** (pre-filled with your tokens).
- **An `install.sh` script** for non-Docker native installs (less common).
- **A `plugin-secrets.json`** containing your bridge + agent tokens.

### 2. Apply the artifact

Drop the generated `docker-compose.yml` next to your existing OpenClaw compose file (or merge the `clawvisor-proxy` + `clawvisor-bootstrap` services into yours) and run:

```bash
docker compose up -d clawvisor-proxy clawvisor-bootstrap
docker compose restart openclaw-gateway     # or whatever you named your OpenClaw container
```

What happens:

- `clawvisor-proxy` starts, loads your `cvisproxy_` token, generates its own CA keypair in its data volume, and registers its signing key with the Clawvisor server.
- `clawvisor-bootstrap` writes `/etc/clawvisor/plugin/secrets.json` into the shared volume, then exits.
- OpenClaw restarts with `HTTP_PROXY=http://clawvisor-proxy:8880` and `NODE_EXTRA_CA_CERTS=/etc/clawvisor/ca.pem` in its environment.
- The plugin, on its next startup, reads `/etc/clawvisor/plugin/secrets.json`, calls `GET /api/plugin/bridges/self/config`, sees `scavenger_enabled=false`, and disables its JSONL scavenger.

### 3. Verify

Send a test message through your OpenClaw agent (Telegram, webchat, whatever). Then check the Agents page dashboard — the bridge row should now show a green **Proxy** badge, and transcripts for subsequent messages come from the proxy.

You can also query the server directly:

```bash
# Replace BRIDGE_ID and JWT with your values.
curl -s "http://localhost:25297/api/plugin/bridges/$BRIDGE_ID/buffer" \
  -H "Authorization: Bearer $JWT" | jq '.conversations'
```

Proxy-sourced entries have `"source": "proxy"` (compared to `"source": "plugin"` for Stage 0 scavenger data).

## Rollback

If something goes wrong, click **Disable Proxy** on the dashboard. This:

- Revokes the proxy instance server-side (subsequent `POST /api/proxy/turns` fails with `UNAUTHORIZED`).
- Flips `bridge.proxy_enabled = false`.
- The plugin, on next startup, re-enables its scavenger.

You're then responsible for stopping the `clawvisor-proxy` container and removing the `HTTP_PROXY` / `NODE_EXTRA_CA_CERTS` env vars from your OpenClaw container. The dashboard does NOT remotely tear down your local containers.

## Troubleshooting

- **"bridge token required for this endpoint" on plugin startup.** The plugin is hitting a proxy-scoped endpoint with its bridge token. Not a proxy issue — likely the plugin is on a stale version. Reinstall via `/skill/setup-openclaw`.

- **Proxy logs show `clawvisor: ingest failed: … 401`.** The `cvisproxy_` token in the proxy config is wrong or the server revoked the proxy instance. Re-run "Enable Network Proxy" on the dashboard to mint a fresh one, and update `/etc/clawvisor/proxy/config.yaml` on the host.

- **Proxy logs show `... UNKNOWN_TOKEN` rejections from OpenClaw traffic.** The `cvis_` agent tokens in the proxy's config aren't matching what OpenClaw is actually using in `Proxy-Authorization`. Check that the plugin has been restarted after the install artifact was applied (it rewrites the shared env).

- **No TurnEvents appearing in the buffer.** Check that OpenClaw is actually routing through the proxy: from inside the OpenClaw container, `curl -v https://api.anthropic.com/` should show the CONNECT going to `clawvisor-proxy:8880`, not to Anthropic directly.

- **`SIGNATURE_INVALID` warnings in server logs.** At Stage 1 these are audit-only — the event is still stored. They indicate the proxy's signature didn't verify against the registered public key. Most common cause: a stale signing key. Restart the proxy container to rotate.

## Known limitations

- **Posture A (default) doesn't prevent bypass.** A deliberately malicious agent can unset `HTTP_PROXY`. Posture B (iptables egress) is the answer; it requires root on the host and isn't pre-configured by the default template.

- **Webchat is invisible to the proxy.** OpenClaw's webchat is an in-container WebSocket. The MITM only sees external HTTPS. Webchat transcripts fall back to the plugin's `message_received` hook (tamperable as before).

- **LLM-stream transcripts may have duplicates.** Anthropic's Messages API is stateless; every request includes the full conversation history. Stage 1 emits each request's messages verbatim and relies on server-side dedup by `event_id`. Stage 2 adds proper sessionization.

- **HTTP/2 is not supported.** MITM is HTTP/1.1-only. Clients that don't gracefully downgrade won't work.

- **Windows not supported.** macOS + Linux only.

## Where to file issues

- GitHub: https://github.com/clawvisor/clawvisor/issues
- Label any Stage 1 proxy bugs with `area:proxy-stage1`.

See also:
- [docs/design-proxy-stage1.md](./design-proxy-stage1.md) — design rationale, trust model, open questions.
- [docs/proxy-api.md](./proxy-api.md) — wire contract between proxy and server.
- [docs/design-proxy-stage2.md](./design-proxy-stage2.md) — what's coming next (credential injection, hosted mode, sessionization).
