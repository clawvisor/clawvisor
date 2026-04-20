# Standalone Network Proxy — Claude Code / Cursor / any agent

The Clawvisor Network Proxy can run on its own, without the OpenClaw
plugin. This is the right setup for:

- Claude Code running on your laptop
- Cursor / Windsurf / similar IDE-integrated agents
- Any coding agent that speaks `HTTPS_PROXY` and trusts a CA cert

You get:

- **Wire-level transcripts** of LLM calls (Anthropic + OpenAI today)
- **Vault credential injection** — agents never see raw API keys
- **Stage 3 policy enforcement** (when available) — block/deny rules applied at the proxy

You do **not** get (plugin-only features):

- In-container Telegram / webchat inbound channels
- Auto-approval context derived from OpenClaw session JSONL

## Setup

1. In the Clawvisor dashboard, go to **Agents → "Network Proxy without
   OpenClaw" → Set up → Generate install artifact**.

2. Save the one-time `cvisproxy_...` token. You'll re-enter it in the
   dashboard to rotate; otherwise you won't see it again.

3. Save `clawvisor-proxy.yml` and `install.sh` to a directory:

   ```sh
   mkdir ~/clawvisor-proxy && cd ~/clawvisor-proxy
   # paste the two files from the dashboard
   chmod +x install.sh
   ```

4. Run the installer — it brings up the proxy container, extracts the
   CA cert, and prints the env vars your agent needs:

   ```sh
   ./install.sh
   ```

5. In the dashboard, **Agents → Your Agents → Add Agent** to mint a
   `cvis_...` token. Copy it.

6. Set the env vars in the shell you'll run your coding agent from:

   ```sh
   export HTTP_PROXY="http://cvis_YOUR_AGENT_TOKEN@127.0.0.1:8880"
   export HTTPS_PROXY="$HTTP_PROXY"
   export NODE_EXTRA_CA_CERTS="$HOME/clawvisor-proxy/clawvisor-ca.pem"
   export SSL_CERT_FILE="$NODE_EXTRA_CA_CERTS"
   ```

7. Run your agent. Outbound traffic will flow through the proxy and
   show up in your Clawvisor dashboard as transcripts.

## Credential injection

Agents can't see raw API keys when you use the vault-backed flow:

1. Dashboard → **Vault Credentials → Move to vault**, paste your
   Anthropic / OpenAI / Google AI key. Choose which agents can use it.
2. Remove the key from any agent config (e.g. delete `ANTHROPIC_API_KEY`
   from your shell).
3. The proxy injects the header on every matching request. No more
   secrets on disk on the agent host.

## Rotating the proxy token

Click **Set up** again in the dashboard — that mints a new
`cvisproxy_...` and revokes the previous one. Replace the env var in
`clawvisor-proxy.yml` and `docker compose up -d` to pick up.

## Troubleshooting

- **Agent can't reach proxy**: confirm `curl http://127.0.0.1:9091/healthz`
  returns 200. If not, `docker logs clawvisor-proxy` will show the issue.
- **TLS errors in the agent**: your `NODE_EXTRA_CA_CERTS` or
  `SSL_CERT_FILE` path is wrong, or the agent's HTTP client doesn't
  respect those env vars. For Node, also try system-wide trust-store
  install (the install.sh output has OS-specific commands).
- **"agent not in ACL for credential"**: the credential you moved to
  vault is restricted to a different agent token. Edit the credential
  in the dashboard, or loosen the ACL to "any agent."
