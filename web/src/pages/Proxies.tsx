import { useState } from 'react'
import { Routes, Route, Link, useParams, useNavigate, useSearchParams } from 'react-router-dom'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { formatDistanceToNow } from 'date-fns'
import { api } from '../api/client'
import type { BridgeToken, ProxyEnableResponse } from '../api/client'
import PolicyEditor from '../components/PolicyEditor'

// Proxies — first-class IA for the Network Proxy. Replaces the
// proxy-as-section-on-Agents-page hairball. Two routes:
//   /dashboard/proxies              → list of all proxies (one per bridge)
//   /dashboard/proxies/:id          → detail: install / policy / violations / bans
//
// Deep-linkable: an agent can hand a user the URL and the user lands
// directly on the right proxy with the right install panel open.
export default function Proxies() {
  return (
    <Routes>
      <Route index element={<ProxiesList />} />
      <Route path=":id" element={<ProxyDetail />} />
    </Routes>
  )
}

// -- list view -----------------------------------------------------------

function ProxiesList() {
  const { data: bridges, isLoading } = useQuery({
    queryKey: ['bridges'],
    queryFn: () => api.plugin.listBridges(),
  })
  const active = bridges?.filter(b => !b.revoked_at) ?? []

  return (
    <div className="p-4 sm:p-8 space-y-6">
      <header>
        <h1 className="text-2xl font-bold text-text-primary">Proxies</h1>
        <p className="text-sm text-text-tertiary mt-1">
          The Clawvisor Network Proxy intercepts AI agent traffic to capture
          tamper-proof transcripts, inject vault credentials, and enforce
          policies. One proxy per bridge.
        </p>
      </header>

      {isLoading && <div className="text-sm text-text-tertiary">Loading proxies…</div>}

      {!isLoading && active.length === 0 && (
        <EmptyState />
      )}

      {active.length > 0 && (
        <div className="space-y-2">
          {active.map(b => (
            <ProxyListRow key={b.id} bridge={b} />
          ))}
        </div>
      )}

      <CreateProxyOnlyCard />
    </div>
  )
}

function ProxyListRow({ bridge }: { bridge: BridgeToken }) {
  const label = bridge.hostname || bridge.install_fingerprint || bridge.id
  const subtitle = bridge.is_proxy_only
    ? 'Standalone (no plugin)'
    : 'OpenClaw plugin'

  return (
    <Link
      to={`/dashboard/proxies/${bridge.id}`}
      className="block bg-surface-1 border border-border-default rounded-md px-5 py-4 hover:border-brand/40 transition-colors"
    >
      <div className="flex items-center justify-between gap-3">
        <div className="min-w-0">
          <div className="font-medium text-text-primary truncate flex items-center gap-2">
            {label}
            {bridge.proxy_enabled && (
              <span className="text-[10px] uppercase tracking-wider font-semibold px-1.5 py-0.5 rounded bg-success/10 text-success border border-success/20">
                Proxy on
              </span>
            )}
            {bridge.is_proxy_only && (
              <span className="text-[10px] uppercase tracking-wider font-semibold px-1.5 py-0.5 rounded bg-accent/10 text-accent border border-accent/20">
                Standalone
              </span>
            )}
          </div>
          <p className="text-xs text-text-tertiary mt-0.5">
            {subtitle} · Paired {formatDistanceToNow(new Date(bridge.created_at), { addSuffix: true })}
            {bridge.last_used_at && (
              <> · Last seen {formatDistanceToNow(new Date(bridge.last_used_at), { addSuffix: true })}</>
            )}
          </p>
        </div>
        <span className="text-text-tertiary text-sm">→</span>
      </div>
    </Link>
  )
}

function EmptyState() {
  return (
    <div className="text-sm text-text-tertiary text-center py-12 bg-surface-1 border border-border-default rounded-md space-y-2">
      <div className="text-text-secondary font-medium">No proxies yet.</div>
      <p>
        To get a proxy, either pair an OpenClaw plugin (creates a bridge automatically)
        or set up a standalone proxy below.
      </p>
    </div>
  )
}

// -- detail view ---------------------------------------------------------

function ProxyDetail() {
  const { id = '' } = useParams<{ id: string }>()
  const navigate = useNavigate()
  const qc = useQueryClient()
  const [searchParams] = useSearchParams()
  const setupHint = searchParams.get('setup') // e.g. ?setup=claude-code

  const { data: bridges, isLoading } = useQuery({
    queryKey: ['bridges'],
    queryFn: () => api.plugin.listBridges(),
  })
  const bridge = bridges?.find(b => b.id === id)

  const [installArtifact, setInstallArtifact] = useState<ProxyEnableResponse | null>(null)

  const enableProxyMut = useMutation({
    mutationFn: () => api.plugin.enableProxy(id),
    onSuccess: (artifact) => {
      setInstallArtifact(artifact)
      qc.invalidateQueries({ queryKey: ['bridges'] })
    },
  })
  const disableProxyMut = useMutation({
    mutationFn: () => api.plugin.disableProxy(id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['bridges'] }),
  })

  if (isLoading) return <div className="p-8 text-sm text-text-tertiary">Loading…</div>
  if (!bridge) {
    return (
      <div className="p-8 space-y-3">
        <div className="text-sm text-text-tertiary">Proxy not found.</div>
        <Link to="/dashboard/proxies" className="text-sm text-brand hover:underline">
          ← Back to proxies
        </Link>
      </div>
    )
  }

  const label = bridge.hostname || bridge.install_fingerprint || bridge.id

  return (
    <div className="p-4 sm:p-8 space-y-6 max-w-5xl">
      <header className="space-y-2">
        <button
          onClick={() => navigate('/dashboard/proxies')}
          className="text-xs text-text-tertiary hover:text-text-secondary"
        >
          ← All proxies
        </button>
        <h1 className="text-2xl font-bold text-text-primary flex items-center gap-3">
          {label}
          {bridge.proxy_enabled && (
            <span className="text-xs uppercase tracking-wider font-semibold px-2 py-0.5 rounded bg-success/10 text-success border border-success/20">
              Proxy on
            </span>
          )}
        </h1>
        <p className="text-xs text-text-tertiary font-mono">{bridge.id}</p>
      </header>

      {/* Status / enable strip */}
      <section className="bg-surface-1 border border-border-default rounded-md px-5 py-4">
        {bridge.proxy_enabled ? (
          <div className="flex items-center justify-between gap-3">
            <div>
              <div className="text-sm font-medium text-text-primary">Network Proxy is enabled</div>
              <div className="text-xs text-text-tertiary mt-0.5">
                Proxy intercepts and observes this bridge's outbound traffic.
              </div>
            </div>
            <div className="flex items-center gap-2">
              <button
                onClick={() => enableProxyMut.mutate()}
                disabled={enableProxyMut.isPending}
                className="text-xs px-3 py-1.5 rounded border border-border-default hover:bg-surface-2 disabled:opacity-50"
              >
                Rotate token / re-download
              </button>
              <button
                onClick={() => {
                  if (confirm('Disable the proxy for this bridge? Currently-running proxy containers will need to be torn down separately.')) {
                    disableProxyMut.mutate()
                  }
                }}
                disabled={disableProxyMut.isPending}
                className="text-xs px-3 py-1.5 rounded border border-border-default hover:bg-surface-2 disabled:opacity-50"
              >
                Disable
              </button>
            </div>
          </div>
        ) : (
          <div className="flex items-center justify-between gap-3">
            <div>
              <div className="text-sm font-medium text-text-primary">Proxy not enabled yet</div>
              <div className="text-xs text-text-tertiary mt-0.5">
                Click to mint a one-time proxy token and generate the install
                artifact for this bridge.
              </div>
            </div>
            <button
              onClick={() => enableProxyMut.mutate()}
              disabled={enableProxyMut.isPending}
              className="text-sm px-4 py-2 rounded bg-brand text-surface-0 hover:bg-brand-strong disabled:opacity-50"
            >
              {enableProxyMut.isPending ? 'Generating…' : 'Enable Proxy'}
            </button>
          </div>
        )}
        {installArtifact && (
          <div className="mt-4">
            <ProxyArtifactPanel artifact={installArtifact} setupHint={setupHint} onClose={() => setInstallArtifact(null)} />
          </div>
        )}
      </section>

      {/* Policy + violations + bans (only meaningful when proxy is enabled) */}
      {bridge.proxy_enabled && (
        <section className="bg-surface-1 border border-border-default rounded-md px-5 py-4">
          <h2 className="text-base font-semibold text-text-primary mb-1">Policy</h2>
          <p className="text-xs text-text-tertiary mb-2">
            YAML rules the proxy applies to every intercepted request.
          </p>
          <PolicyEditor bridgeId={id} />
        </section>
      )}
    </div>
  )
}

// ProxyArtifactPanel renders the one-time install artifact returned by
// EnableProxy. Differs from the legacy InstallArtifactViewer by adding
// the new daemon-driven path as the recommended option, with the
// docker-compose flow secondary.
function ProxyArtifactPanel({ artifact, setupHint, onClose }: {
  artifact: ProxyEnableResponse
  setupHint: string | null
  onClose: () => void
}) {
  const [tab, setTab] = useState<'daemon' | 'compose' | 'standalone-docker'>('daemon')
  const copy = (s: string) => { navigator.clipboard.writeText(s).catch(() => { /* noop */ }) }

  // Copy-pasteable one-liner for the daemon path. We don't have the
  // user's exact binary location yet (they'll build/download), so this
  // is a template they fill in.
  const daemonInstallSnippet = `# 1. Install clawvisor-local + the proxy binary, then:
clawvisor proxy install \\
  --binary $(which clawvisor-proxy) \\
  --proxy-token ${artifact.proxy_token || '<COPY-FROM-ABOVE>'} \\
  --bridge-id ${artifact.bridge_id}

# 2. Trust the proxy's CA cert (one-time):
clawvisor proxy trust-ca

# 3. Run any agent through it (scoped — only this command's traffic):
clawvisor proxy run --agent-token cvis_YOUR_AGENT_TOKEN -- claude
`

  return (
    <div className="border border-accent/30 rounded-md bg-surface-2 p-4 flex flex-col gap-3">
      <div className="flex items-start justify-between">
        <div>
          <div className="text-sm font-semibold text-text-primary">Install</div>
          <div className="text-xs text-text-tertiary mt-0.5">
            Generated {formatDistanceToNow(new Date(artifact.generated_at), { addSuffix: true })}.
            {setupHint && <> · Setup hint: <code>{setupHint}</code></>}
          </div>
        </div>
        <button onClick={onClose} className="text-xs text-text-tertiary hover:text-text-primary">✕</button>
      </div>

      {artifact.proxy_token && (
        <div className="rounded bg-warning/10 border border-warning/30 p-3 text-xs">
          <div className="font-semibold text-warning mb-1">Save this proxy token now — it won't be shown again</div>
          <code className="block bg-surface-1 p-2 rounded font-mono text-[11px] break-all">{artifact.proxy_token}</code>
          <button
            onClick={() => artifact.proxy_token && copy(artifact.proxy_token)}
            className="mt-2 text-[11px] px-2 py-1 rounded border border-border-default hover:bg-surface-2"
          >
            Copy token
          </button>
        </div>
      )}

      <div className="flex gap-1 border-b border-border-default text-xs">
        <TabButton active={tab === 'daemon'} onClick={() => setTab('daemon')}>
          macOS / Linux (recommended)
        </TabButton>
        <TabButton active={tab === 'standalone-docker'} onClick={() => setTab('standalone-docker')}>
          Docker (standalone)
        </TabButton>
        <TabButton active={tab === 'compose'} onClick={() => setTab('compose')}>
          Docker (compose override)
        </TabButton>
      </div>

      {tab === 'daemon' && (
        <div className="space-y-2">
          <p className="text-xs text-text-tertiary">
            For laptops + servers running coding agents (Claude Code, Cursor, OpenClaw)
            outside Docker. The proxy is supervised by clawvisor-local — auto-restart on
            crash, status in your menu bar.
          </p>
          <CodeBlock onCopy={() => copy(daemonInstallSnippet)}>{daemonInstallSnippet}</CodeBlock>
          <p className="text-[11px] text-text-tertiary">
            Don't have clawvisor-local? Install it first via the Settings → Install daemon flow.
          </p>
        </div>
      )}

      {tab === 'standalone-docker' && (
        <div className="space-y-2">
          <p className="text-xs text-text-tertiary">
            For self-hosters running their own agent containers. Two commands —
            no compose YAML required.
          </p>
          <CodeBlock onCopy={() => copy(buildStandaloneDockerSnippet(artifact))}>
            {buildStandaloneDockerSnippet(artifact)}
          </CodeBlock>
          <p className="text-[11px] text-text-tertiary">
            After this, point your agent container at <code>http://host.docker.internal:25298</code>
            with the agent's <code>cvis_…</code> token in <code>HTTP_PROXY</code>.
          </p>
        </div>
      )}

      {tab === 'compose' && (
        <div className="space-y-2">
          <p className="text-xs text-text-tertiary">
            Compose <em>override</em> for existing OpenClaw deployments. Save next to
            your existing <code>docker-compose.yml</code> and run together:
          </p>
          <CodeBlock onCopy={() => copy(`docker compose -f docker-compose.yml -f clawvisor-proxy.yml up -d`)}>
            {`# Save the YAML below as clawvisor-proxy.yml, then:
docker compose -f docker-compose.yml -f clawvisor-proxy.yml up -d`}
          </CodeBlock>
          <CodeBlock onCopy={() => copy(artifact.docker_compose_yaml)}>
            {artifact.docker_compose_yaml}
          </CodeBlock>
        </div>
      )}
    </div>
  )
}

function TabButton({ active, onClick, children }: {
  active: boolean
  onClick: () => void
  children: React.ReactNode
}) {
  return (
    <button
      onClick={onClick}
      className={`px-3 py-1.5 border-b-2 ${active ? 'border-accent text-accent' : 'border-transparent text-text-tertiary hover:text-text-primary'}`}
    >
      {children}
    </button>
  )
}

function CodeBlock({ children, onCopy }: { children: string; onCopy?: () => void }) {
  return (
    <div className="relative group bg-surface-0 border border-border-subtle rounded overflow-hidden">
      <pre className="px-3 py-2.5 text-xs font-mono text-text-primary overflow-x-auto whitespace-pre">
        {children}
      </pre>
      {onCopy && (
        <button
          onClick={onCopy}
          className="absolute top-2 right-2 text-xs px-2 py-1 rounded border border-border-subtle text-text-tertiary hover:text-text-primary hover:bg-surface-1 opacity-0 group-hover:opacity-100 transition-opacity"
        >
          Copy
        </button>
      )}
    </div>
  )
}

function buildStandaloneDockerSnippet(artifact: ProxyEnableResponse): string {
  return `# 1. Start the proxy container (binds to localhost:25298)
docker run -d --name clawvisor-proxy \\
  -p 127.0.0.1:25298:8880 \\
  -v clawvisor-proxy-data:/data \\
  -e CLAWVISOR_SERVER_URL=http://host.docker.internal:25297 \\
  -e CLAWVISOR_PROXY_TOKEN=${artifact.proxy_token || '<COPY-FROM-ABOVE>'} \\
  -e CLAWVISOR_BRIDGE_ID=${artifact.bridge_id} \\
  clawvisor/proxy:latest

# 2. (Optional) connect to your agent container's network so it can resolve "clawvisor-proxy"
docker network connect <your-network> clawvisor-proxy

# 3. Extract the CA cert your agent needs to trust:
docker cp clawvisor-proxy:/data/ca.pem ./clawvisor-ca.pem
`
}

// -- create-proxy-only card (moved from Agents page) --------------------

function CreateProxyOnlyCard() {
  const qc = useQueryClient()
  const navigate = useNavigate()
  const [hostname, setHostname] = useState('')
  const [expanded, setExpanded] = useState(false)

  const createMut = useMutation({
    mutationFn: () => api.plugin.createProxyOnlyBridge(hostname.trim() || undefined),
    onSuccess: (res) => {
      qc.invalidateQueries({ queryKey: ['bridges'] })
      // Drop the user straight into the new proxy's detail page with
      // the install artifact open.
      navigate(`/dashboard/proxies/${res.bridge_id}?setup=standalone`)
    },
  })

  return (
    <section className="bg-surface-1 border border-border-default rounded-md px-5 py-4">
      <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-3">
        <div className="min-w-0">
          <h2 className="text-base font-semibold text-text-primary">
            Add a standalone proxy
          </h2>
          <p className="text-sm text-text-tertiary mt-0.5">
            For Claude Code, Cursor, or any agent without OpenClaw. Mints a
            new bridge that's proxy-only — no plugin pairing needed.
          </p>
        </div>
        <button
          onClick={() => setExpanded(v => !v)}
          className="text-sm px-3 py-1.5 rounded border border-border-default hover:bg-surface-2"
        >
          {expanded ? 'Cancel' : 'Add proxy'}
        </button>
      </div>

      {expanded && (
        <div className="mt-4 pt-3 border-t border-border-default space-y-3">
          <div>
            <label className="block text-xs text-text-tertiary mb-1">
              Label (optional, e.g. "laptop", "ci")
            </label>
            <input
              value={hostname}
              onChange={e => setHostname(e.target.value)}
              placeholder="laptop"
              className="w-full text-sm rounded border border-border-default bg-surface-0 text-text-primary px-3 py-1.5"
            />
          </div>
          <button
            onClick={() => createMut.mutate()}
            disabled={createMut.isPending}
            className="px-4 py-1.5 text-sm rounded bg-brand text-surface-0 hover:bg-brand-strong disabled:opacity-50"
          >
            {createMut.isPending ? 'Creating…' : 'Create proxy + show install'}
          </button>
          {createMut.error && (
            <div className="text-xs text-danger">{(createMut.error as Error).message}</div>
          )}
        </div>
      )}
    </section>
  )
}
