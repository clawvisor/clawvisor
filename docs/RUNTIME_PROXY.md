# Runtime Proxy

> [!WARNING]
> The runtime proxy is in active development. Behavior, flags, and API surface may change in any release while it remains pre-1.0. Treat it as preview-quality and pin to a specific Clawvisor version in production.

The runtime proxy is a TLS-terminating egress proxy that runs inside the Clawvisor daemon. When enabled, an agent's outbound HTTPS traffic is routed through it so Clawvisor can observe model API calls, intercept tool-use, hold inline approvals, capture or substitute credentials, and attribute every request to a runtime session.

It is complementary to the gateway: gateway requests describe what the agent intends to do; the runtime proxy sees what it actually sends on the wire.

## Enable the proxy

The proxy is gated behind `runtime_proxy.enabled` and ships off by default.

In `config.yaml`:

```yaml
runtime_proxy:
  enabled: true
  listen_addr: 127.0.0.1:25290
  data_dir: ~/.clawvisor/runtime-proxy
  session_ttl_seconds: 3600
  listener_hostnames:
    - localhost
    - 127.0.0.1
```

Or via environment variables:

```bash
export CLAWVISOR_RUNTIME_PROXY_ENABLED=true
export CLAWVISOR_RUNTIME_PROXY_LISTEN_ADDR=127.0.0.1:25290
```

Restart the daemon to pick up the change:

```bash
clawvisor restart
```

On first start, the proxy generates a CA at `~/.clawvisor/runtime-proxy/ca.pem`. Agent traffic must trust this CA — the launcher commands below mount it for you.

### Multi-replica deployments

If you run more than one Clawvisor replica with the proxy enabled, configure Redis. The held-approval cache otherwise falls back to in-memory and approvals desync across replicas — one replica will hold a request while another has already approved it. The daemon logs a warning at startup when this configuration is detected.

```yaml
redis:
  url: redis://...
```

## Run an agent through the proxy

The simplest flow uses a registered agent:

```bash
# 1. Register an agent. This creates the agent on the daemon and stores its
#    token at ~/.clawvisor/agents.json so launchers can reference it by name.
clawvisor agent register my-agent

# 2. Run a command under a runtime session. Mints a session, injects proxy
#    env vars (HTTP_PROXY, HTTPS_PROXY, CLAWVISOR_RUNTIME_*, CA trust paths),
#    and execs the command.
clawvisor agent run --agent my-agent -- claude
```

The session runs until the wrapped process exits or the configured TTL elapses (default 1 hour). Pass `--ttl-seconds` to override.

If you'd rather inject the env into your current shell — useful for debugging or for launchers that don't tolerate a wrapping process — use `runtime-env`:

```bash
eval "$(clawvisor agent runtime-env --agent my-agent)"
claude  # now routes through the proxy
```

## Run a Dockerized agent

Container launchers use a long-lived agent token instead of a runtime session secret so containers can restart without re-minting credentials.

### One-shot `docker run`

```bash
clawvisor agent docker-run --agent my-agent -- \
  docker run --rm -it my-agent-image agent serve
```

This injects the env vars and mounts the proxy CA into the container at `/clawvisor/ca.pem`.

### `docker compose`

Generate a Compose override for a named service:

```bash
clawvisor agent docker-compose --agent my-agent --service my-svc > clawvisor.override.yml
docker compose -f docker-compose.yml -f clawvisor.override.yml up
```

The override is templated by default — it references `${CLAWVISOR_AGENT_TOKEN}` rather than baking the token in. Export the token before running compose:

```bash
export CLAWVISOR_AGENT_TOKEN=<token from `clawvisor agent register`>
```

### Print env for other launchers

To inspect or pipe the env into a different launcher:

```bash
clawvisor agent docker-env --agent my-agent --format env       # KEY=VALUE
clawvisor agent docker-env --agent my-agent --format export    # export KEY="VALUE"
clawvisor agent docker-env --agent my-agent --format docker-args  # -e KEY=VALUE -e ...
```

## Container isolation (advanced)

The default container flow trusts that the agent honors `HTTPS_PROXY`. For workloads that bypass env-var proxies (e.g. clients that pin DNS and call `connect()` directly), Clawvisor offers a kernel-enforced isolation mode that runs the user container in a netns sidecar with an iptables policy that rejects everything except the proxy and API endpoints.

This requires a separately-running `clawvisor proxy expose` instance the container's docker network can reach. `expose` publishes the local proxy and daemon API onto a network-routable bind address with a source-IP allowlist.

```bash
# On the host running the daemon (or a host that can reach it):
clawvisor proxy expose --detach \
  --bind 0.0.0.0 \
  --proxy-port 25291 \
  --api-port 18791
# Stop later with:
clawvisor proxy expose stop
```

By default the allowlist accepts loopback + RFC-1918. Use `--allow-cidr` to narrow or extend it (repeatable).

Then generate the isolated Compose override:

```bash
clawvisor agent docker-compose --agent my-agent --service my-svc \
  --isolation=container \
  --expose-url http://10.0.0.5:25291 \
  --expose-api-url http://10.0.0.5:18791 \
  > clawvisor.override.yml
```

The override emits a privileged netns-holder sidecar plus rewires the user service to share its netns via `network_mode: service:…`. Direct `connect()` to anything other than the two expose listeners returns `ECONNREFUSED` at the kernel.

If the user service in the base compose file declares `ports:`, those need to move to the holder. Use `--publish-port` (repeatable, Compose port syntax):

```bash
clawvisor agent docker-compose ... \
  --isolation=container \
  --publish-port 18789:18789 \
  --publish-port 0.0.0.0:18790:18790/tcp \
  --expose-url ... --expose-api-url ...
```

The user service automatically clears its inherited `ports:` list with `ports: !reset []`.

The same isolation mode is available for `docker run`:

```bash
clawvisor agent docker-run --agent my-agent --isolation=container -- \
  docker run --rm my-agent-image agent serve
```

## Starter profiles

The first time you launch a recognized agent (e.g. `claude`, `codex`), the launcher prompts to apply a starter set of allow rules for that agent's known control-plane traffic — model API calls, telemetry endpoints, OAuth refresh, and so on. This keeps the proxy from holding routine traffic for review while you build out your real policy.

Available profiles ship with Clawvisor:

| Profile ID | Display Name | Triggers on argv | Covers |
|---|---|---|---|
| `claude_code` | Claude Code | `claude` | `api.anthropic.com`, `platform.claude.com`, `mcp-proxy.anthropic.com`, plugin distribution, telemetry |
| `codex` | Codex | `codex` | `api.openai.com /v1/responses`, `/v1/chat/completions`, `chatgpt.com /backend-api/codex/responses` |

Press `Y` to apply, `n` to skip this launch, or `a` to remember the skip and never prompt for that agent + command pair again. You can manage these decisions later from the dashboard.

To apply a profile non-interactively:

```bash
clawvisor agent run --agent my-agent --runtime-profile claude_code -- claude
```

Pass `--runtime-profile none` to suppress the prompt entirely.

## Observe vs enforce

Each runtime session runs in either observe mode (default for new agents) or enforce mode. In observe mode, the proxy logs what it *would* have done — held a request, prompted inline, denied — but lets traffic pass. This is the recommended way to onboard a new agent: run it for a while, review the activity feed for unintended egress, promote rules into your policy, and then flip to enforce.

The launcher prints a notice on every observe-mode run so it's not silently lost.

To override per-launch:

```bash
clawvisor agent run --agent my-agent --observe -- claude        # force observe
clawvisor agent run --agent my-agent --observe=false -- claude  # force enforce
```

The persistent default for an agent lives at `agents.<agent>.runtime_settings.observation_mode` and is configurable from the dashboard.

## Where things live

| Path | Contents |
|---|---|
| `~/.clawvisor/runtime-proxy/ca.pem` | Runtime proxy CA. Mounted at `/clawvisor/ca.pem` inside containers by the launchers. |
| `~/.clawvisor/runtime-proxy/expose.pid` | Pidfile written by `clawvisor proxy expose --detach`. |
| `~/.clawvisor/agents.json` | Local agent registry populated by `clawvisor agent register`. |
| `runtime_proxy.timing_trace_dir` | Per-request latency traces when `runtime_proxy.timing_trace_enabled=true`. |
| `runtime_proxy.body_trace_dir` | Request/response body captures when `runtime_proxy.body_trace_enabled=true`. Disable in production. |

## Further reading

- [Runtime capability matrix](runtime-capability-matrix.md) — feature-by-feature coverage of what the proxy intercepts, where the code lives, and what's tested.
- The dashboard's **Runtime** page is the primary surface for sessions, events, leases, rules, and starter profiles. It's available whenever `runtime_proxy.enabled=true`.
