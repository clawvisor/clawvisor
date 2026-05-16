# Runtime Proxy

> [!WARNING]
> The runtime proxy is in active development. Behavior, flags, and API surface may change in any release while it remains pre-1.0. Treat it as preview-quality and pin to a specific Clawvisor version in production.

> [!NOTE]
> The public command-line launcher now defaults to proxy-lite. Use
> `clawvisor agent run --agent <name> -- claude` or the provider-specific
> `clawvisor agent claude` / `clawvisor agent codex` wrappers from
> [LITE_PROXY.md](LITE_PROXY.md). The full CONNECT/TLS runtime-proxy launcher
> commands are not registered on the public CLI.

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

On first start, the proxy generates a CA at `~/.clawvisor/runtime-proxy/ca.pem`.

### Multi-replica deployments

If you run more than one Clawvisor replica with the proxy enabled, configure Redis. The held-approval cache otherwise falls back to in-memory and approvals desync across replicas — one replica will hold a request while another has already approved it. The daemon logs a warning at startup when this configuration is detected.

```yaml
redis:
  url: redis://...
```

## Historical full-proxy launcher

The full runtime-proxy launcher used to expose `agent run` and `runtime-env`.
Those command names are no longer available for the full proxy on the public
CLI; `agent run` now means proxy-lite.

```bash
clawvisor agent register my-agent
clawvisor agent run --agent my-agent -- claude
```

See [LITE_PROXY.md](LITE_PROXY.md) for the supported command-line flow.

## Public CLI status

The full CONNECT/TLS proxy still exists in the daemon and lower-level runtime
code, but its command-line launchers are not part of the public CLI. This means
the following historical commands are intentionally unavailable:

```bash
clawvisor agent runtime-env
clawvisor agent docker-env
clawvisor agent docker-run
clawvisor agent docker-compose
clawvisor proxy expose
```

For command-line agents, use proxy-lite instead:

```bash
clawvisor agent register my-agent
clawvisor agent run --agent my-agent -- claude --print "ping"
clawvisor agent codex --agent my-agent -- exec "say hi"
```

## Observe vs enforce

Each runtime session runs in either observe mode or enforce mode. In observe mode, the proxy logs what it *would* have done — held a request, prompted inline, denied — but lets traffic pass.

New agents default to **enforce**. To make new agents start in observe instead — useful if you'd rather onboard each agent by running it for a while, reviewing the activity feed for unintended egress, promoting rules into your policy, and then flipping to enforce — set the daemon-wide default in `config.yaml`:

```yaml
runtime_policy:
  observation_mode_default: true
```

(Or `CLAWVISOR_RUNTIME_POLICY_OBSERVATION_DEFAULT=true`.) This only affects the initial runtime settings created for new agents; existing agents keep whatever they have.

The persistent default for a specific agent is its `runtime_mode` field (`observe` or `enforce`) on the agent's runtime settings record, configurable from the dashboard or via `PUT /api/agents/{id}/runtime-settings`.

## Where things live

| Path | Contents |
|---|---|
| `~/.clawvisor/runtime-proxy/ca.pem` | Runtime proxy CA generated when the full proxy is enabled. |
| `~/.clawvisor/agents.json` | Local agent registry populated by `clawvisor agent register`. |
| `runtime_proxy.timing_trace_dir` | Per-request latency traces when `runtime_proxy.timing_trace_enabled=true`. |
| `runtime_proxy.body_trace_dir` | Request/response body captures when `runtime_proxy.body_trace_enabled=true`. Disable in production. |

## Further reading

- [Runtime capability matrix](runtime-capability-matrix.md) — feature-by-feature coverage of what the proxy intercepts, where the code lives, and what's tested.
- The dashboard's **Runtime** page is the primary surface for sessions, events, leases, rules, and starter profiles. It's available whenever `runtime_proxy.enabled=true`.
