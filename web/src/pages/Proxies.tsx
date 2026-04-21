import { useState, useEffect } from 'react'
import { useAuth } from '../hooks/useAuth'
import { Routes, Route, Link, useParams, useNavigate, useSearchParams, useLocation } from 'react-router-dom'
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
  const location = useLocation()
  const setupHint = searchParams.get('setup') // e.g. ?setup=claude-code

  const { data: bridges, isLoading } = useQuery({
    queryKey: ['bridges'],
    queryFn: () => api.plugin.listBridges(),
  })
  const bridge = bridges?.find(b => b.id === id)

  // Initial install-artifact state: prefer the artifact handed to us
  // via navigation state (from "Add proxy" flow — preserves the
  // one-time token without a rotate). Falls back to null; the
  // install-artifact-without-token path below provides a viewer-only
  // fallback for users who arrive here later without a fresh token.
  const navArtifact = (location.state as { artifact?: ProxyEnableResponse } | null)?.artifact ?? null
  const [installArtifact, setInstallArtifact] = useState<ProxyEnableResponse | null>(navArtifact)

  // For proxies that are already enabled but where we don't have a
  // fresh token in state (user navigated here from elsewhere, or
  // refreshed the page), fetch the token-less install artifact so we
  // can still surface install instructions. The proxy_token field on
  // the response will be empty in that case — the UI handles it.
  useEffect(() => {
    if (installArtifact || !bridge?.proxy_enabled) return
    let cancelled = false
    api.plugin.installArtifact(id)
      .then(art => { if (!cancelled) setInstallArtifact(art) })
      .catch(() => { /* not fatal — user can click Rotate to get a fresh one */ })
    return () => { cancelled = true }
  }, [bridge?.proxy_enabled, id, installArtifact])

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
// EnableProxy. The default tab adapts to the user's persona: cloud
// users with laptop-style Clawvisor get the daemon path; self-hosters
// (multi_tenant=false on this server) get Docker by default since
// they're already managing infrastructure via containers.
function ProxyArtifactPanel({ artifact, setupHint, onClose }: {
  artifact: ProxyEnableResponse
  setupHint: string | null
  onClose: () => void
}) {
  const { features } = useAuth()
  // multi_tenant=true → cloud Clawvisor → user is probably on a laptop
  //                     using clawvisor-local. Default: daemon path.
  // multi_tenant=false → self-hosted server → Docker is more natural.
  const isSelfHosted = features?.multi_tenant === false
  const [tab, setTab] = useState<'daemon' | 'compose' | 'standalone-docker'>(
    isSelfHosted ? 'standalone-docker' : 'daemon'
  )
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

      {/* What is this — concise, factual, sets expectations before
          the user picks a tab. Short enough to read; honest about the
          trust + scope model. */}
      <ProxyExplainer />

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

      <div className="flex flex-wrap gap-1 border-b border-border-default text-xs">
        <TabButton active={tab === 'daemon'} onClick={() => setTab('daemon')}>
          Personal Mac / Linux laptop{!isSelfHosted && <span className="text-text-tertiary"> · suggested</span>}
        </TabButton>
        <TabButton active={tab === 'standalone-docker'} onClick={() => setTab('standalone-docker')}>
          Docker server{isSelfHosted && <span className="text-text-tertiary"> · suggested</span>}
        </TabButton>
        <TabButton active={tab === 'compose'} onClick={() => setTab('compose')}>
          Existing OpenClaw compose
        </TabButton>
      </div>

      {tab === 'daemon' && (
        <div className="space-y-2">
          <div className="text-xs text-text-secondary space-y-1.5">
            <p>
              <strong>Pick this if:</strong> you run AI tools directly on your
              laptop — like Claude Code in your terminal, Cursor as an app, or
              OpenClaw running on the same machine you're reading this from.
            </p>
            <p>
              <strong>What you get:</strong> a menu-bar icon showing the proxy's
              status, automatic restart if it crashes, and one command to wrap any
              agent (<code>clawvisor proxy run -- claude</code>) so only that
              specific agent goes through Clawvisor.
            </p>
          </div>

          <DaemonPrerequisites />

          <DaemonOneClickEnable artifact={artifact} />

          <CodeBlock onCopy={() => copy(daemonInstallSnippet)}>{daemonInstallSnippet}</CodeBlock>

          {artifact.proxy_token && (
            <div className="mt-3 pt-3 border-t border-border-subtle">
              <div className="text-xs font-medium text-text-primary mb-1">
                Or have an agent walk the user through it
              </div>
              <p className="text-[11px] text-text-tertiary mb-2">
                Paste the URL below into your agent — it'll fetch the markdown, ask
                permission for each step, and run the install commands. Works for
                any agent that respects skills (Claude Code, Cursor, OpenClaw).
              </p>
              <CodeBlock
                onCopy={() => copy(buildSkillURL(artifact))}
              >
                {`# In your agent:\nFetch ${buildSkillURL(artifact)} and follow the steps.`}
              </CodeBlock>
            </div>
          )}
        </div>
      )}

      {tab === 'standalone-docker' && (
        <div className="space-y-2">
          <div className="text-xs text-text-secondary space-y-1.5">
            <p>
              <strong>Pick this if:</strong> you run your AI agents inside Docker
              containers (your own server, a dev environment, etc.) and you want
              to manage Clawvisor the same way you manage everything else.
            </p>
            <p>
              <strong>What you get:</strong> a single Clawvisor container running
              alongside your existing stack. Two <code>docker run</code> commands;
              no YAML files to write or merge.
            </p>
          </div>
          <CodeBlock onCopy={() => copy(buildStandaloneDockerSnippet(artifact))}>
            {buildStandaloneDockerSnippet(artifact)}
          </CodeBlock>
        </div>
      )}

      {tab === 'compose' && (
        <div className="space-y-2">
          <div className="text-xs text-text-secondary space-y-1.5">
            <p>
              <strong>Pick this if:</strong> you already have an OpenClaw setup running
              from a <code>docker-compose.yml</code> file and you want Clawvisor to start
              and stop with everything else.
            </p>
            <p>
              <strong>What you get:</strong> a small extra YAML file that adds the
              Clawvisor proxy to your existing setup. Run <code>docker compose up</code>
              with both files together and it just works.
            </p>
          </div>
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

// ProxyExplainer is the "what is this" intro card on the install
// panel. Plain English — no security jargon, no acronyms. Designed
// for users who want their AI agent to be safer but don't know what
// TLS or "MITM" means. The advanced details live in the docs, not
// here.
function ProxyExplainer() {
  const [open, setOpen] = useState(true)
  return (
    <div className="rounded bg-surface-1 border border-border-default p-3 text-xs space-y-2">
      <button
        onClick={() => setOpen(v => !v)}
        className="flex items-center gap-1.5 text-text-primary font-medium hover:text-brand transition-colors"
      >
        <span>{open ? '▾' : '▸'}</span>
        <span>What this does, in plain English</span>
      </button>
      {open && (
        <div className="text-text-secondary space-y-3 pl-4 text-[12px] leading-relaxed">
          <p>
            This installs a small program on your computer that sits between your AI
            agent and the internet. It lets Clawvisor see what your agent is doing
            so you can review it, block bad actions, and keep your API keys safe.
          </p>

          <div>
            <div className="font-medium text-text-primary mb-1">What it watches</div>
            <p>
              <strong>Only the AI agents you point at it.</strong> Not your web browser,
              not your email, not Slack, not anything else on your machine.
            </p>
          </div>

          <div>
            <div className="font-medium text-text-primary mb-1">What it can do for you</div>
            <ul className="list-disc pl-5 space-y-0.5">
              <li>Show you a record of every conversation your agent had.</li>
              <li>Plug your API keys into your agent's requests <em>without</em> the agent ever seeing them.</li>
              <li>Block your agent from doing things you don't want (like deleting a repository).</li>
              <li>Ask a second AI to double-check risky actions before they happen.</li>
            </ul>
          </div>

          <div>
            <div className="font-medium text-text-primary mb-1">What stays private</div>
            <ul className="list-disc pl-5 space-y-0.5">
              <li>Your normal browsing, downloads, and other apps. Untouched.</li>
              <li>If you're self-hosting, none of your data leaves your machine.</li>
              <li>You can remove it any time — uninstall is one command, no leftovers.</li>
            </ul>
          </div>

          <div>
            <div className="font-medium text-text-primary mb-1">The one trade-off</div>
            <p>
              For this to work, we add Clawvisor to your computer's list of trusted
              services (the same list that already includes things like your bank's
              certificate). This is how developer tools like 1Password CLI and
              Charles Proxy work too. It only stays as long as you keep the program
              installed.
            </p>
          </div>
        </div>
      )}
    </div>
  )
}

// DaemonPrerequisites surfaces the two install steps a fresh user
// needs before the rest of the panel works: (1) install clawvisor-local
// itself, (2) install the proxy binary. Hidden when both are detected;
// otherwise renders a clearly-labeled prerequisites card with copy-
// pasteable commands so users don't have to dig into Settings.
function DaemonPrerequisites() {
  const [daemonStatus, setDaemonStatus] = useState<'probing' | 'present' | 'absent'>('probing')
  const [binaryPresent, setBinaryPresent] = useState<'unknown' | 'present' | 'absent'>('unknown')

  useEffect(() => {
    let cancelled = false
    fetch(`${DAEMON_BASE}/api/proxy/status`, { credentials: 'include' })
      .then(r => r.ok ? r.json() : Promise.reject())
      .then(s => {
        if (cancelled) return
        setDaemonStatus('present')
        // If the daemon already has a binary_path recorded, it's been
        // installed before. Otherwise the user still needs the proxy.
        setBinaryPresent(s?.binary_path ? 'present' : 'absent')
      })
      .catch(() => { if (!cancelled) setDaemonStatus('absent') })
    return () => { cancelled = true }
  }, [])

  // Both prereqs satisfied → don't clutter the panel.
  if (daemonStatus === 'present' && binaryPresent === 'present') return null
  if (daemonStatus === 'probing') return null

  const copy = (s: string) => { navigator.clipboard.writeText(s).catch(() => { /* noop */ }) }
  const daemonInstall = 'curl -fsSL https://raw.githubusercontent.com/clawvisor/clawvisor/main/scripts/install-local.sh | sh'
  const proxyBuildFromSource = `# Build the proxy binary from source (until we publish a release):
git clone https://github.com/clawvisor/clawvisor-proxy.git
cd clawvisor-proxy && make build
# Binary lands at ./dist/kumo — pass that path to 'clawvisor proxy install --binary <path>'`

  return (
    <div className="bg-warning/5 border border-warning/30 rounded-md p-3 space-y-3">
      <div className="text-xs font-medium text-text-primary">Prerequisites</div>

      {daemonStatus === 'absent' && (
        <div className="space-y-1">
          <div className="text-[11px] text-warning">⚠ clawvisor-local daemon not detected on this machine.</div>
          <p className="text-[11px] text-text-tertiary">
            Install the local daemon. It supervises the proxy, surfaces it in your menu bar,
            and pairs with the cloud over a tunnel.
          </p>
          <div className="relative group">
            <pre className="bg-surface-0 border border-border-subtle rounded p-2 text-[11px] font-mono overflow-x-auto">{daemonInstall}</pre>
            <button
              onClick={() => copy(daemonInstall)}
              className="absolute top-1 right-1 text-[10px] px-2 py-0.5 rounded border border-border-subtle text-text-tertiary hover:bg-surface-1 opacity-0 group-hover:opacity-100"
            >
              Copy
            </button>
          </div>
          <p className="text-[11px] text-text-tertiary">
            After install, complete pairing in <Link to="/dashboard/settings" className="underline">Settings → Local Daemon</Link>, then come back here.
          </p>
        </div>
      )}

      {binaryPresent === 'absent' && (
        <div className="space-y-1">
          <div className="text-[11px] text-warning">⚠ Proxy binary not yet on this machine.</div>
          <p className="text-[11px] text-text-tertiary">
            We don't publish a pre-built proxy binary yet — for beta, build it from source. A future
            release will let you run <code>clawvisor proxy update-binary</code> to fetch automatically.
          </p>
          <div className="relative group">
            <pre className="bg-surface-0 border border-border-subtle rounded p-2 text-[11px] font-mono overflow-x-auto whitespace-pre">{proxyBuildFromSource}</pre>
            <button
              onClick={() => copy(proxyBuildFromSource)}
              className="absolute top-1 right-1 text-[10px] px-2 py-0.5 rounded border border-border-subtle text-text-tertiary hover:bg-surface-1 opacity-0 group-hover:opacity-100"
            >
              Copy
            </button>
          </div>
        </div>
      )}
    </div>
  )
}

// DaemonOneClickEnable probes for clawvisor-local on the user's
// machine. If reachable, it offers a one-click install path that
// POSTs the freshly-minted proxy_token + bridge_id to the daemon's
// /api/proxy/configure endpoint. CORS is gated by the daemon's
// AllowedCloudOrigins config — typically includes app.clawvisor.com
// + localhost dev origins, so the dashboard can call across.
//
// The user still needs to know the local proxy binary's path, which
// we ask them for since the daemon enforces a binary_path check
// (avoiding "where's the binary on your machine?" being a server-
// side guess).
const DAEMON_BASE = 'http://127.0.0.1:25299'

function DaemonOneClickEnable({ artifact }: { artifact: ProxyEnableResponse }) {
  const [reachable, setReachable] = useState<'probing' | 'yes' | 'no'>('probing')
  const [binaryPath, setBinaryPath] = useState('')
  const [submitting, setSubmitting] = useState(false)
  const [result, setResult] = useState<{ ok: boolean; msg: string } | null>(null)

  useEffect(() => {
    let cancelled = false
    fetch(`${DAEMON_BASE}/api/proxy/status`, { credentials: 'include' })
      .then(r => r.ok ? r.json() : Promise.reject(r.status))
      .then(s => {
        if (cancelled) return
        setReachable('yes')
        // If the daemon already remembers a binary path, prefill it so
        // re-config / token-rotation is one click.
        if (s?.binary_path) setBinaryPath(s.binary_path)
      })
      .catch(() => { if (!cancelled) setReachable('no') })
    return () => { cancelled = true }
  }, [])

  if (reachable === 'probing') {
    return (
      <div className="text-[11px] text-text-tertiary italic">Looking for a local clawvisor daemon…</div>
    )
  }
  if (reachable === 'no') {
    return null // silently fall back to the copy-paste snippet below
  }

  const handleEnable = async () => {
    if (!binaryPath.trim()) {
      setResult({ ok: false, msg: 'Enter the proxy binary path first.' })
      return
    }
    setSubmitting(true)
    setResult(null)
    try {
      const r = await fetch(`${DAEMON_BASE}/api/proxy/configure`, {
        method: 'POST',
        credentials: 'include',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          binary_path: binaryPath.trim(),
          server_url: window.location.origin,
          proxy_token: artifact.proxy_token,
          bridge_id: artifact.bridge_id,
          listen_host: '127.0.0.1',
          listen_port: 25298,
          mode: 'observe',
          auto_enable: true,
        }),
      })
      const body = await r.json()
      if (!r.ok) {
        setResult({ ok: false, msg: body?.error || `daemon returned ${r.status}` })
      } else if (body?.enable_error) {
        setResult({ ok: false, msg: 'Configured, but enable failed: ' + body.enable_error })
      } else if (body?.restart_error) {
        setResult({ ok: false, msg: 'Configured, but restart failed: ' + body.restart_error })
      } else {
        setResult({ ok: true, msg: 'Proxy is configured and running. Run "clawvisor proxy trust-ca" in a terminal next.' })
      }
    } catch (err) {
      setResult({ ok: false, msg: 'Network error talking to daemon: ' + (err as Error).message })
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <div className="bg-success/5 border border-success/30 rounded-md p-3 space-y-2 mb-2">
      <div className="flex items-center gap-2">
        <span className="text-success text-xs">●</span>
        <div className="text-xs font-medium text-text-primary">Local daemon detected</div>
      </div>
      <p className="text-[11px] text-text-tertiary">
        clawvisor-local is running on this machine. We can configure it directly — no terminal command needed.
      </p>
      <div>
        <label className="block text-[11px] text-text-tertiary mb-1">Proxy binary path on this machine</label>
        <input
          value={binaryPath}
          onChange={e => setBinaryPath(e.target.value)}
          placeholder="/usr/local/bin/clawvisor-proxy"
          className="w-full text-xs font-mono rounded border border-border-default bg-surface-0 text-text-primary px-2 py-1"
        />
      </div>
      <button
        onClick={handleEnable}
        disabled={submitting || !binaryPath.trim()}
        className="text-xs px-3 py-1.5 rounded bg-success text-surface-0 hover:bg-success/90 disabled:opacity-50"
      >
        {submitting ? 'Configuring…' : 'Enable on this device'}
      </button>
      {result && (
        <div className={`text-[11px] ${result.ok ? 'text-success' : 'text-danger'}`}>
          {result.msg}
        </div>
      )}
    </div>
  )
}

// buildSkillURL renders a URL the agent can fetch to get
// /skill/setup-clawvisor-proxy with the bridge + proxy token already
// pre-filled. Uses the dashboard's own origin since that's where the
// Clawvisor server lives.
function buildSkillURL(artifact: ProxyEnableResponse): string {
  const origin = window.location.origin
  const params = new URLSearchParams()
  params.set('bridge_id', artifact.bridge_id)
  if (artifact.proxy_token) params.set('proxy_token', artifact.proxy_token)
  return `${origin}/skill/setup-clawvisor-proxy?${params.toString()}`
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
      // the install artifact open. Pass the artifact via navigation
      // state so the detail page surfaces install instructions
      // immediately — without forcing the user to click "Rotate token"
      // (which would burn the one-time proxy_token they just got).
      navigate(`/dashboard/proxies/${res.bridge_id}?setup=standalone`, {
        state: { artifact: res },
      })
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
