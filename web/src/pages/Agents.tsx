import { useState } from 'react'
import { useSearchParams } from 'react-router-dom'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { api } from '../api/client'
import type { BridgeToken, ConnectionRequest, PluginPairRequest } from '../api/client'
import { useAuth } from '../hooks/useAuth'
import { formatDistanceToNow } from 'date-fns'
import CountdownTimer from '../components/CountdownTimer'
import VaultCredentials from '../components/VaultCredentials'

export default function Agents() {
  const { currentOrg } = useAuth()
  const orgId = currentOrg?.id
  const qc = useQueryClient()
  const [name, setName] = useState('')
  const [newToken, setNewToken] = useState<string | null>(null)
  const [formError, setFormError] = useState<string | null>(null)
  const [showCreateForm, setShowCreateForm] = useState(false)

  const { data: agents, isLoading } = useQuery({
    queryKey: ['agents', orgId ?? 'personal'],
    queryFn: () => orgId ? api.orgs.agents(orgId) : api.agents.list(),
  })

  const { data: connections } = useQuery({
    queryKey: ['connections'],
    queryFn: () => api.connections.list(),
    enabled: !orgId,
  })

  const { data: pluginPairs } = useQuery({
    queryKey: ['plugin-pairs'],
    queryFn: () => api.plugin.listPairs(),
    enabled: !orgId,
  })

  const { data: bridges } = useQuery({
    queryKey: ['bridges'],
    queryFn: () => api.plugin.listBridges(),
    enabled: !orgId,
  })

  const createMut = useMutation({
    mutationFn: () => orgId
      ? api.orgs.createAgent(orgId, name)
      : api.agents.create(name).then(agent => ({ agent, token: agent.token ?? '' })),
    onSuccess: (result) => {
      qc.invalidateQueries({ queryKey: ['agents'] })
      qc.invalidateQueries({ queryKey: ['welcome'] })
      setNewToken(result.token ?? null)
      setName('')
      setFormError(null)
      setShowCreateForm(false)
    },
    onError: (err: Error) => setFormError(err.message),
  })

  const deleteMut = useMutation({
    mutationFn: (id: string) => orgId ? api.orgs.deleteAgent(orgId, id) : api.agents.delete(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['agents'] })
      qc.invalidateQueries({ queryKey: ['tasks'] })
      qc.invalidateQueries({ queryKey: ['overview'] })
      qc.invalidateQueries({ queryKey: ['welcome'] })
    },
  })

  const pending = (!orgId ? connections : undefined) ?? []

  return (
    <div className="p-4 sm:p-8 space-y-8">
      <h1 className="text-2xl font-bold text-text-primary">Agents</h1>
      <p className="text-sm text-text-tertiary">
        An agent is any AI system (Claude, a custom bot, etc.) that you want to give controlled access to your services.
        Each agent gets a unique token — paste it into your agent's configuration to connect it to Clawvisor.
      </p>

      {/* Connect an Agent guide (personal context only) */}
      {!orgId && <ConnectAgentGuide />}

      {/* Pending connection requests (personal context only) */}
      {!orgId && pending.length > 0 && (
        <section>
          <div className="flex items-center gap-3 mb-3">
            <h2 className="text-lg font-semibold text-text-primary">Pending Connections</h2>
            <span className="bg-warning text-surface-0 text-xs font-bold rounded px-2.5 py-0.5 font-mono">
              {pending.length}
            </span>
          </div>
          <div className="space-y-3">
            {pending.map(cr => (
              <ConnectionCard key={cr.id} request={cr} />
            ))}
          </div>
        </section>
      )}

      {/* Pending plugin pair requests (personal context only) */}
      {!orgId && pluginPairs && pluginPairs.length > 0 && (
        <section>
          <div className="flex items-center gap-3 mb-3">
            <h2 className="text-lg font-semibold text-text-primary">Pending Plugin Pairings</h2>
            <span className="bg-warning text-surface-0 text-xs font-bold rounded px-2.5 py-0.5 font-mono">
              {pluginPairs.length}
            </span>
          </div>
          <div className="space-y-3">
            {pluginPairs.map(p => (
              <PluginPairCard key={p.id} request={p} />
            ))}
          </div>
        </section>
      )}

      {/* Active bridges (personal context only) */}
      {!orgId && bridges && bridges.length > 0 && (
        <section>
          <div className="flex items-center justify-between mb-3">
            <h2 className="text-lg font-semibold text-text-primary">OpenClaw Bridges</h2>
          </div>
          <p className="text-sm text-text-tertiary mb-3">
            One row per paired OpenClaw install. The auto-approval toggle lets Clawvisor infer
            approval from the conversations the plugin forwards — turn it off to require manual
            task approval for every agent call.
          </p>
          <div className="space-y-2">
            {bridges.filter(b => !b.revoked_at).map(b => (
              <BridgeRow key={b.id} bridge={b} />
            ))}
          </div>
        </section>
      )}

      {/* Vault credentials — shown only when at least one bridge has a proxy enabled,
          since credentials are only useful to the proxy. */}
      {!orgId && bridges && bridges.some(b => !b.revoked_at && b.proxy_enabled) && (
        <VaultCredentials agents={agents} />
      )}

      {/* New token display */}
      {newToken && (
        <div className="bg-success/10 border border-success/30 rounded-md p-4 space-y-2">
          <p className="text-sm font-medium text-success">Agent created — copy your token now, it won't be shown again.</p>
          <div className="flex items-center gap-2">
            <code className="flex-1 bg-surface-1 border border-success/30 rounded px-3 py-2 text-xs font-mono text-text-primary break-all">
              {newToken}
            </code>
            <button
              onClick={() => navigator.clipboard.writeText(newToken)}
              className="text-xs px-3 py-1.5 rounded border border-success/30 text-success hover:bg-success/10"
            >
              Copy
            </button>
          </div>
          <button onClick={() => setNewToken(null)} className="text-xs text-success hover:underline">
            Dismiss
          </button>
        </div>
      )}

      {/* Agent list */}
      <section>
        <div className="flex items-center justify-between mb-3">
          <h2 className="text-lg font-semibold text-text-primary">Your Agents</h2>
          <button
            onClick={() => { setShowCreateForm(!showCreateForm); setFormError(null) }}
            className="text-sm px-3 py-1.5 rounded bg-brand text-surface-0 hover:bg-brand-strong"
          >
            {showCreateForm ? 'Cancel' : 'Add Agent'}
          </button>
        </div>

        {/* Inline create form */}
        {showCreateForm && (
          <div className="bg-surface-1 border border-border-default rounded-md p-4 mb-3 space-y-3">
            {formError && <div className="text-xs text-danger">{formError}</div>}
            <div className="flex gap-3">
              <input
                value={name}
                onChange={e => setName(e.target.value)}
                onKeyDown={e => { if (e.key === 'Enter' && name.trim()) createMut.mutate() }}
                placeholder="Agent name"
                autoFocus
                className="flex-1 text-sm rounded border border-border-default bg-surface-0 text-text-primary px-3 py-1.5 focus:outline-none focus:ring-1 focus:ring-brand/30 focus:border-brand placeholder:text-text-tertiary"
              />
              <button
                onClick={() => createMut.mutate()}
                disabled={createMut.isPending || !name.trim()}
                className="px-4 py-1.5 text-sm rounded bg-brand text-surface-0 hover:bg-brand-strong disabled:opacity-50"
              >
                {createMut.isPending ? 'Creating…' : 'Create'}
              </button>
            </div>
          </div>
        )}

        {isLoading && <div className="text-sm text-text-tertiary">Loading…</div>}

        {!isLoading && (!agents || agents.length === 0) && !showCreateForm && (
          <div className="text-sm text-text-tertiary text-center py-8 bg-surface-1 border border-border-default rounded-md">
            No agents yet. Follow the setup guides above or click <strong>Add Agent</strong> to create one manually.
          </div>
        )}

        <div className="space-y-2">
          {agents?.map(agent => {
            const hasActiveTasks = agent.active_task_count > 0
            return (
              <div
                key={agent.id}
                className={`bg-surface-1 border rounded-md px-5 py-4 flex flex-col sm:flex-row sm:items-center justify-between gap-3 ${
                  hasActiveTasks
                    ? 'border-brand/40 border-l-[3px] border-l-brand'
                    : 'border-border-default'
                }`}
              >
                <div>
                  <div className="flex items-center gap-2">
                    <span className="font-medium text-text-primary">{agent.name}</span>
                    {hasActiveTasks && (
                      <span className="text-xs font-medium px-2 py-0.5 rounded-full bg-brand/10 text-brand">
                        {agent.active_task_count} active {agent.active_task_count === 1 ? 'task' : 'tasks'}
                      </span>
                    )}
                  </div>
                  <p className="text-xs text-text-tertiary mt-0.5">
                    Created {formatDistanceToNow(new Date(agent.created_at), { addSuffix: true })} · {agent.id}
                    {agent.last_task_at && (
                      <> · Last task {formatDistanceToNow(new Date(agent.last_task_at), { addSuffix: true })}</>
                    )}
                  </p>
                </div>
                <button
                  onClick={() => {
                    const taskWarning = hasActiveTasks
                      ? `\n\n${agent.active_task_count} active ${agent.active_task_count === 1 ? 'task' : 'tasks'} will be revoked.`
                      : ''
                    if (confirm(`Revoke agent "${agent.name}"? Running agents using this token will stop working.${taskWarning}`)) {
                      deleteMut.mutate(agent.id)
                    }
                  }}
                  disabled={deleteMut.isPending}
                  className="text-xs px-3 py-1.5 rounded bg-danger/10 text-danger border border-danger/20 hover:bg-danger/20"
                >
                  Revoke
                </button>
              </div>
            )
          })}
        </div>
      </section>

    </div>
  )
}

// ── Connect an Agent guide ───────────────────────────────────────────────────

type AgentTab = 'openclaw' | 'hermes' | 'claude-code' | 'claude-desktop' | 'other'

const AGENT_TABS: AgentTab[] = ['openclaw', 'hermes', 'claude-code', 'claude-desktop', 'other']

function ConnectAgentGuide() {
  const [searchParams, setSearchParams] = useSearchParams()
  const initialTab = (AGENT_TABS.includes(searchParams.get('agent') as AgentTab)
    ? (searchParams.get('agent') as AgentTab)
    : 'openclaw')
  const [tab, setTabState] = useState<AgentTab>(initialTab)
  const setTab = (next: AgentTab) => {
    setTabState(next)
    const params = new URLSearchParams(searchParams)
    params.set('agent', next)
    setSearchParams(params, { replace: true })
  }
  const [copied, setCopied] = useState(false)
  const { user } = useAuth()

  const { data: pairInfo } = useQuery({
    queryKey: ['pairInfo'],
    queryFn: () => api.devices.pairInfo(),
  })

  const isLocal = window.location.hostname === 'localhost' || window.location.hostname === '127.0.0.1'
  const hasRelay = !!(pairInfo?.daemon_id && pairInfo?.relay_host)

  // When accessed locally, agents should talk to the daemon directly rather
  // than routing through the relay. Use the relay URL only when the dashboard
  // itself is being accessed remotely (cloud-hosted).
  const clawvisorURL = isLocal
    ? window.location.origin
    : hasRelay
      ? `https://${pairInfo!.relay_host}/d/${pairInfo!.daemon_id}`
      : window.location.origin

  const userIdParam = user?.id ? `?user_id=${encodeURIComponent(user.id)}` : ''

  const baseSkillURL = hasRelay
    ? `https://${pairInfo!.relay_host}/d/${pairInfo!.daemon_id}`
    : window.location.origin
  const setupHermesURL = `${baseSkillURL}/skill/setup-hermes${userIdParam}`
  // OpenClaw setup URL is built from a minted pair_code inside OpenClawGuide —
  // not pre-computed here, because the code is single-use and we don't want
  // to mint one until the user actually opens the OpenClaw tab.
  // Legacy — still referenced by the "Other Agents" tab until we split it too.
  const setupURL = `${baseSkillURL}/skill/setup${userIdParam}`

  const copyText = (text: string) => {
    navigator.clipboard.writeText(text)
    setCopied(true)
    setTimeout(() => setCopied(false), 2000)
  }

  const tabs: { id: AgentTab; label: string }[] = [
    { id: 'openclaw', label: 'OpenClaw' },
    { id: 'hermes', label: 'Hermes' },
    { id: 'claude-code', label: 'Claude Code' },
    { id: 'claude-desktop', label: 'Claude Desktop' },
    { id: 'other', label: 'Other Agents' },
  ]

  return (
    <section className="bg-surface-1 border border-border-default rounded-md overflow-hidden">
      <div className="px-5 pt-5 pb-0">
        <h2 className="text-lg font-semibold text-text-primary">Connect an Agent</h2>
        <p className="text-sm text-text-tertiary mt-1">
          Follow the steps below to connect a coding agent to Clawvisor.
        </p>
      </div>

      {/* Tabs */}
      <div className="flex gap-0 px-5 mt-4 border-b border-border-subtle overflow-x-auto">
        {tabs.map(t => (
          <button
            key={t.id}
            onClick={() => { setTab(t.id); setCopied(false) }}
            className={`px-4 py-2.5 text-sm font-medium border-b-2 transition-colors ${
              tab === t.id
                ? 'border-brand text-brand'
                : 'border-transparent text-text-tertiary hover:text-text-secondary'
            }`}
          >
            {t.label}
          </button>
        ))}
      </div>

      <div className="p-5">
        {tab === 'openclaw' && <OpenClawGuide baseSkillURL={baseSkillURL} copied={copied} onCopy={copyText} />}
        {tab === 'hermes' && <HermesGuide setupURL={setupHermesURL} copied={copied} onCopy={copyText} />}
        {tab === 'claude-code' && <ClaudeCodeGuide clawvisorURL={clawvisorURL} userIdParam={userIdParam} onCopy={copyText} />}
        {tab === 'claude-desktop' && <ClaudeDesktopGuide isLocal={isLocal} onCopy={copyText} />}
        {tab === 'other' && <OtherAgentGuide setupURL={setupURL} clawvisorURL={clawvisorURL} copied={copied} onCopy={copyText} />}
      </div>
    </section>
  )
}

function StepNumber({ n }: { n: number }) {
  return (
    <span className="flex-shrink-0 w-6 h-6 rounded-full bg-brand/10 text-brand text-xs font-bold flex items-center justify-center">
      {n}
    </span>
  )
}

function CodeBlock({ children, onCopy }: { children: string; onCopy?: () => void }) {
  return (
    <div className="relative group bg-surface-0 border border-border-subtle rounded overflow-hidden">
      <pre className="px-3 py-2.5 text-xs font-mono text-text-primary overflow-x-auto whitespace-pre-wrap break-all">
        {children}
      </pre>
      {onCopy && (
        <>
          {/* Desktop: inline overlay */}
          <button
            onClick={onCopy}
            className="hidden sm:block absolute top-2 right-2 text-xs px-2 py-1 rounded border border-border-subtle text-text-tertiary hover:text-text-primary hover:bg-surface-1 opacity-0 group-hover:opacity-100 transition-opacity"
          >
            Copy
          </button>
          {/* Mobile: footer bar */}
          <div className="sm:hidden border-t border-border-subtle px-3 py-1.5 flex justify-end">
            <button
              onClick={onCopy}
              className="text-xs px-2.5 py-1 rounded border border-border-subtle text-text-tertiary hover:text-text-primary hover:bg-surface-1"
            >
              Copy
            </button>
          </div>
        </>
      )}
    </div>
  )
}

function ClaudeCodeGuide({ clawvisorURL, userIdParam, onCopy }: {
  clawvisorURL: string
  userIdParam: string
  onCopy: (text: string) => void
}) {
  const installCmd = `curl -sf "${clawvisorURL}/skill/clawvisor-setup.md${userIdParam}" \\\n  --create-dirs -o ~/.claude/commands/clawvisor-setup.md`

  return (
    <div className="space-y-5">
      <p className="text-sm text-text-secondary">
        Install a slash command, then run it in Claude Code. It handles agent registration,
        skill installation, environment setup, and a smoke test — all interactively.
      </p>

      <div className="flex items-start gap-3">
        <StepNumber n={1} />
        <div className="space-y-1.5 min-w-0 flex-1">
          <p className="text-sm font-medium text-text-primary">Install the setup command</p>
          <p className="text-xs text-text-tertiary">
            Run this in your terminal to install the{' '}
            <code className="font-mono text-text-secondary">/clawvisor-setup</code> slash command:
          </p>
          <CodeBlock onCopy={() => onCopy(installCmd)}>{installCmd}</CodeBlock>
        </div>
      </div>

      <div className="flex items-start gap-3">
        <StepNumber n={2} />
        <div className="space-y-1.5 min-w-0 flex-1">
          <p className="text-sm font-medium text-text-primary">Run /clawvisor-setup in Claude Code</p>
          <p className="text-xs text-text-tertiary">
            Open Claude Code and type{' '}
            <code className="font-mono text-text-secondary">/clawvisor-setup</code>.
            Claude will walk you through the setup — registering as an agent, configuring
            environment variables, and verifying the connection.
          </p>
        </div>
      </div>

      <div className="flex items-start gap-3">
        <StepNumber n={3} />
        <div className="space-y-1.5 min-w-0 flex-1">
          <p className="text-sm font-medium text-text-primary">Approve the connection</p>
          <p className="text-xs text-text-tertiary">
            During setup, Claude Code sends a connection request. Approve it in the{' '}
            <strong>Pending Connections</strong> section above. Once approved, Claude Code
            finishes setup automatically and runs a smoke test.
          </p>
        </div>
      </div>
    </div>
  )
}

function ClaudeDesktopGuide({ isLocal, onCopy }: { isLocal: boolean; onCopy: (text: string) => void }) {
  const pluginName = isLocal ? 'clawvisor-local@cowork-plugins' : 'clawvisor@cowork-plugins'
  const marketplaceCmd = 'claude plugin marketplace add clawvisor/cowork-plugins'
  const installCmd = `/plugin install ${pluginName}`

  return (
    <div className="space-y-5">
      <p className="text-sm text-text-secondary">
        {isLocal
          ? 'Connect Claude Desktop to your local Clawvisor instance via the Cowork plugin.'
          : 'Connect Claude Desktop to your Clawvisor cloud account via the Cowork plugin.'}
      </p>

      <div className="flex items-start gap-3">
        <StepNumber n={1} />
        <div className="space-y-1.5 min-w-0 flex-1">
          <p className="text-sm font-medium text-text-primary">Add the marketplace</p>
          <p className="text-xs text-text-tertiary">
            Run this in your terminal:
          </p>
          <CodeBlock onCopy={() => onCopy(marketplaceCmd)}>{marketplaceCmd}</CodeBlock>
        </div>
      </div>

      <div className="flex items-start gap-3">
        <StepNumber n={2} />
        <div className="space-y-1.5 min-w-0 flex-1">
          <p className="text-sm font-medium text-text-primary">Install the plugin</p>
          <p className="text-xs text-text-tertiary">
            From within Claude Desktop:
          </p>
          <CodeBlock onCopy={() => onCopy(installCmd)}>{installCmd}</CodeBlock>
        </div>
      </div>

      {!isLocal && (
        <div className="flex items-start gap-3">
          <StepNumber n={3} />
          <div className="space-y-1.5 min-w-0 flex-1">
            <p className="text-sm font-medium text-text-primary">Authenticate</p>
            <p className="text-xs text-text-tertiary">
              The first time Claude uses a Clawvisor tool, you'll be prompted to authenticate via OAuth.
              Follow the link in your terminal to sign in and connect Claude Desktop to your Clawvisor cloud account.
            </p>
          </div>
        </div>
      )}

      <div className="flex items-start gap-3">
        <StepNumber n={isLocal ? 3 : 4} />
        <div className="space-y-1.5 min-w-0 flex-1">
          <p className="text-sm font-medium text-text-primary">Start using it</p>
          <p className="text-xs text-text-tertiary">
            Ask Claude to do something that requires an external service — e.g. "check my Gmail" or
            "list my GitHub issues." Claude will create a task, ask you to approve, and execute through
            Clawvisor.{' '}
            {isLocal &&
              <>Open the dashboard with <code className="font-mono text-text-secondary">clawvisor tui</code> or visit <code className="font-mono text-text-secondary">http://localhost:25297</code> to manage services, approvals, and restrictions.</>
            }
          </p>
        </div>
      </div>
    </div>
  )
}

function OpenClawGuide({ baseSkillURL, copied, onCopy }: {
  baseSkillURL: string
  copied: boolean
  onCopy: (text: string) => void
}) {
  const mintMut = useMutation({
    mutationFn: () => api.plugin.mintPairCode(),
  })

  const setupURL = mintMut.data
    ? `${baseSkillURL}/skill/setup-openclaw?pair_code=${encodeURIComponent(mintMut.data.code)}`
    : null
  const prompt = setupURL
    ? `I'd like to set up the Clawvisor plugin in this OpenClaw install. Please follow the instructions at:\n${setupURL}`
    : null

  return (
    <div className="space-y-5">
      <p className="text-sm text-text-secondary">
        For OpenClaw installs with the Clawvisor plugin. The plugin pairs itself — in one dashboard
        approval, you mint a <strong>bridge token</strong> (held only by the plugin, never exposed
        to agents) plus one agent token per agent configured in OpenClaw.
      </p>

      <div className="space-y-4">
        <div className="flex items-start gap-3">
          <StepNumber n={1} />
          <div className="space-y-1.5 min-w-0 flex-1">
            <p className="text-sm font-medium text-text-primary">Generate a one-time setup link</p>
            {!prompt && (
              <>
                <button
                  onClick={() => mintMut.mutate()}
                  disabled={mintMut.isPending}
                  className="text-sm px-4 py-1.5 rounded bg-brand text-surface-0 hover:bg-brand-strong disabled:opacity-50"
                >
                  {mintMut.isPending ? 'Generating…' : 'Generate setup link'}
                </button>
                {mintMut.isError && (
                  <p className="text-xs text-danger">Failed to mint pair code: {(mintMut.error as Error).message}</p>
                )}
                <p className="text-xs text-text-tertiary">
                  Mints a one-time pair code bound to your account. The link embeds the code —
                  valid for 10 minutes, single use. Regenerate as needed.
                </p>
              </>
            )}
            {prompt && (
              <>
                <div className="relative group bg-surface-0 border border-brand/20 rounded overflow-hidden">
                  <pre className="px-3 py-2.5 text-xs font-mono text-text-primary overflow-x-auto whitespace-pre-wrap break-all">
                    {prompt}
                  </pre>
                  <button
                    onClick={() => onCopy(prompt)}
                    className="hidden sm:block absolute top-2 right-2 text-xs px-2 py-1 rounded border border-border-subtle text-text-tertiary hover:text-text-primary hover:bg-surface-1"
                  >
                    {copied ? 'Copied' : 'Copy'}
                  </button>
                  <div className="sm:hidden border-t border-brand/20 px-3 py-1.5 flex justify-end">
                    <button
                      onClick={() => onCopy(prompt)}
                      className="text-xs px-2.5 py-1 rounded border border-border-subtle text-text-tertiary hover:text-text-primary hover:bg-surface-1"
                    >
                      {copied ? 'Copied' : 'Copy'}
                    </button>
                  </div>
                </div>
                <p className="text-xs text-text-tertiary">
                  Paste this into an agent running under OpenClaw. The agent deposits the one-time
                  pair code into the Clawvisor plugin config. It does <em>not</em> call
                  <code className="px-1">/api/agents/connect</code> — the plugin handles pairing itself.
                  Code expires in ~10 min; if it does,
                  {' '}
                  <button onClick={() => mintMut.mutate()} className="text-brand hover:underline">
                    generate a new one
                  </button>
                  .
                </p>
              </>
            )}
          </div>
        </div>

        <div className="flex items-start gap-3">
          <StepNumber n={2} />
          <div className="space-y-1.5 min-w-0 flex-1">
            <p className="text-sm font-medium text-text-primary">Reload the Clawvisor plugin</p>
            <p className="text-xs text-text-tertiary">
              In OpenClaw, reload the Clawvisor plugin (or restart OpenClaw). The plugin redeems the
              code and sends a pair request.
            </p>
          </div>
        </div>

        <div className="flex items-start gap-3">
          <StepNumber n={3} />
          <div className="space-y-1.5 min-w-0 flex-1">
            <p className="text-sm font-medium text-text-primary">Approve the plugin pairing</p>
            <p className="text-xs text-text-tertiary">
              A <strong>Plugin pairing</strong> card will appear in the <strong>Pending Pairings</strong> section
              below. Review the install fingerprint, hostname, and agents list, then choose whether to let
              the plugin drive auto-approval from observed conversations — click <strong>Approve</strong> to
              mint tokens. The plugin receives them silently; agent tool calls start working immediately.
            </p>
          </div>
        </div>
      </div>
    </div>
  )
}

function HermesGuide({ setupURL, copied, onCopy }: {
  setupURL: string
  copied: boolean
  onCopy: (text: string) => void
}) {
  const prompt = `I'd like to set up Clawvisor as the trusted gateway for using data and services. Please follow the instructions at:\n${setupURL}`

  return (
    <div className="space-y-5">
      <p className="text-sm text-text-secondary">
        For Hermes agents (or any agent that talks to Clawvisor directly). Paste the setup prompt
        below — the agent self-registers and waits for your approval.
      </p>

      <div className="space-y-4">
        <div className="flex items-start gap-3">
          <StepNumber n={1} />
          <div className="space-y-1.5 min-w-0 flex-1">
            <p className="text-sm font-medium text-text-primary">Paste this into your agent</p>
            <div className="relative group bg-surface-0 border border-brand/20 rounded overflow-hidden">
              <pre className="px-3 py-2.5 text-xs font-mono text-text-primary overflow-x-auto whitespace-pre-wrap break-all">
                {prompt}
              </pre>
              <button
                onClick={() => onCopy(prompt)}
                className="hidden sm:block absolute top-2 right-2 text-xs px-2 py-1 rounded border border-border-subtle text-text-tertiary hover:text-text-primary hover:bg-surface-1"
              >
                {copied ? 'Copied' : 'Copy'}
              </button>
              <div className="sm:hidden border-t border-brand/20 px-3 py-1.5 flex justify-end">
                <button
                  onClick={() => onCopy(prompt)}
                  className="text-xs px-2.5 py-1 rounded border border-border-subtle text-text-tertiary hover:text-text-primary hover:bg-surface-1"
                >
                  {copied ? 'Copied' : 'Copy'}
                </button>
              </div>
            </div>
            <p className="text-xs text-text-tertiary">
              Your agent will follow the setup instructions — registering itself
              and installing the Clawvisor skill.
            </p>
          </div>
        </div>

        <div className="flex items-start gap-3">
          <StepNumber n={2} />
          <div className="space-y-1.5 min-w-0 flex-1">
            <p className="text-sm font-medium text-text-primary">Approve the connection</p>
            <p className="text-xs text-text-tertiary">
              A connection request will appear in the <strong>Pending Connections</strong> section above.
              Click <strong>Approve</strong> to grant the agent a token. It receives the token automatically
              and is ready to go.
            </p>
          </div>
        </div>
      </div>

      {/* Telegram tip */}
      <div className="bg-surface-0 border border-border-subtle rounded-md px-4 py-3">
        <p className="text-sm text-text-secondary">
          <strong>Using Telegram?</strong> If you talk to your agent via Telegram, you can set up a
          group chat with Clawvisor to get inline approval notifications and auto-approvals.{' '}
          <a href="/dashboard/settings" className="text-brand hover:underline">Set it up in Settings &rarr; Telegram</a>.
        </p>
      </div>
    </div>
  )
}

function OtherAgentGuide({ setupURL, clawvisorURL, copied, onCopy }: {
  setupURL: string
  clawvisorURL: string
  copied: boolean
  onCopy: (text: string) => void
}) {
  const prompt = `I'd like to set up Clawvisor as the trusted gateway for using data and services. Please follow the instructions at:\n${setupURL}`

  return (
    <div className="space-y-5">
      <p className="text-sm text-text-secondary">
        Any agent that can make HTTP requests can connect to Clawvisor. The fastest way is to paste the setup
        prompt below directly into your agent's chat — it will self-register and wait for your approval.
      </p>

      <div className="space-y-4">
        <div className="flex items-start gap-3">
          <StepNumber n={1} />
          <div className="space-y-1.5 min-w-0 flex-1">
            <p className="text-sm font-medium text-text-primary">Paste this into your agent</p>
            <div className="relative group bg-surface-0 border border-brand/20 rounded overflow-hidden">
              <pre className="px-3 py-2.5 text-xs font-mono text-text-primary overflow-x-auto whitespace-pre-wrap break-all">
                {prompt}
              </pre>
              <button
                onClick={() => onCopy(prompt)}
                className="hidden sm:block absolute top-2 right-2 text-xs px-2 py-1 rounded border border-border-subtle text-text-tertiary hover:text-text-primary hover:bg-surface-1"
              >
                {copied ? 'Copied' : 'Copy'}
              </button>
              <div className="sm:hidden border-t border-brand/20 px-3 py-1.5 flex justify-end">
                <button
                  onClick={() => onCopy(prompt)}
                  className="text-xs px-2.5 py-1 rounded border border-border-subtle text-text-tertiary hover:text-text-primary hover:bg-surface-1"
                >
                  {copied ? 'Copied' : 'Copy'}
                </button>
              </div>
            </div>
            <p className="text-xs text-text-tertiary">
              The agent will follow the setup instructions at that URL — it registers itself,
              sets up E2E encryption, and installs the Clawvisor skill.
            </p>
          </div>
        </div>

        <div className="flex items-start gap-3">
          <StepNumber n={2} />
          <div className="space-y-1.5 min-w-0 flex-1">
            <p className="text-sm font-medium text-text-primary">Approve the connection</p>
            <p className="text-xs text-text-tertiary">
              A connection request will appear in the <strong>Pending Connections</strong> section above.
              Click <strong>Approve</strong> to grant the agent a token. It receives the token automatically
              and is ready to go.
            </p>
          </div>
        </div>
      </div>

      {/* Manual path */}
      <details className="group">
        <summary className="text-sm font-medium text-text-secondary cursor-pointer hover:text-text-primary select-none">
          Manual setup (token + environment variables)
        </summary>
        <div className="mt-4 space-y-4 pl-0">
          <div className="flex items-start gap-3">
            <StepNumber n={1} />
            <div className="space-y-1.5 min-w-0 flex-1">
              <p className="text-sm font-medium text-text-primary">Create an agent token</p>
              <p className="text-xs text-text-tertiary">
                Use the <strong>Create Agent</strong> form above. Copy the token — it's shown only once.
              </p>
            </div>
          </div>

          <div className="flex items-start gap-3">
            <StepNumber n={2} />
            <div className="space-y-1.5 min-w-0 flex-1">
              <p className="text-sm font-medium text-text-primary">Configure environment variables</p>
              <p className="text-xs text-text-tertiary">
                Set these in your agent's environment (<code className="font-mono text-text-secondary">.env</code>, shell profile, container config, etc.):
              </p>
              <CodeBlock>{`CLAWVISOR_URL=${clawvisorURL}\nCLAWVISOR_AGENT_TOKEN=<your token>`}</CodeBlock>
            </div>
          </div>

          <div className="flex items-start gap-3">
            <StepNumber n={3} />
            <div className="space-y-1.5 min-w-0 flex-1">
              <p className="text-sm font-medium text-text-primary">Verify</p>
              <CodeBlock>{`curl -sf -H "Authorization: Bearer $CLAWVISOR_AGENT_TOKEN" \\\n  "$CLAWVISOR_URL/api/skill/catalog" | head -20`}</CodeBlock>
              <p className="text-xs text-text-tertiary">
                Should return a JSON catalog of available services. See{' '}
                <code className="font-mono text-text-secondary">{clawvisorURL}/skill/SKILL.md</code>{' '}
                for the full protocol reference.
              </p>
            </div>
          </div>
        </div>
      </details>
    </div>
  )
}

// ── Connection request card ──────────────────────────────────────────────────

function ConnectionCard({ request: cr }: { request: ConnectionRequest }) {
  const qc = useQueryClient()
  const [result, setResult] = useState<string | null>(null)

  const approveMut = useMutation({
    mutationFn: () => api.connections.approve(cr.id),
    onSuccess: () => {
      setResult('Approved')
      qc.invalidateQueries({ queryKey: ['connections'] })
      qc.invalidateQueries({ queryKey: ['agents'] })
      qc.invalidateQueries({ queryKey: ['overview'] })
      qc.invalidateQueries({ queryKey: ['welcome'] })
    },
    onError: (err: Error) => setResult(`Failed: ${err.message}`),
  })

  const denyMut = useMutation({
    mutationFn: () => api.connections.deny(cr.id),
    onSuccess: () => {
      setResult('Denied')
      qc.invalidateQueries({ queryKey: ['connections'] })
      qc.invalidateQueries({ queryKey: ['overview'] })
    },
    onError: (err: Error) => setResult(`Failed: ${err.message}`),
  })

  const isPending = approveMut.isPending || denyMut.isPending

  if (result) {
    return (
      <div className="border border-border-default rounded-md bg-surface-1 px-5 py-4">
        <div className="flex items-center justify-between">
          <span className="font-medium text-text-primary">{cr.name}</span>
          <span className={`text-xs font-medium px-2 py-0.5 rounded ${
            result === 'Approved' ? 'bg-success/10 text-success' :
            result === 'Denied' ? 'bg-danger/10 text-danger' :
            'bg-surface-2 text-text-tertiary'
          }`}>
            {result}
          </span>
        </div>
      </div>
    )
  }

  return (
    <div className="bg-surface-1 border border-border-default rounded-md border-l-[3px] border-l-brand overflow-hidden">
      <div className="px-5 pt-5 pb-4">
        <div className="flex items-center justify-between">
          <span className="font-mono text-lg font-semibold text-text-primary">{cr.name}</span>
          <CountdownTimer expiresAt={cr.expires_at} />
        </div>
        {cr.description && (
          <p className="text-sm text-text-secondary mt-1.5">{cr.description}</p>
        )}
        <div className="flex items-center gap-3 mt-2 text-xs text-text-tertiary">
          <span>IP: <code className="font-mono">{cr.ip_address}</code></span>
          <span>Requested {formatDistanceToNow(new Date(cr.created_at), { addSuffix: true })}</span>
        </div>
      </div>

      <div className="px-4 py-3 border-t border-border-subtle flex items-center justify-end gap-2">
        <button
          onClick={() => denyMut.mutate()}
          disabled={isPending}
          className="rounded px-4 py-1.5 text-sm font-medium bg-danger/10 text-danger border border-danger/20 hover:bg-danger/20 disabled:opacity-50"
        >
          Deny
        </button>
        <button
          onClick={() => approveMut.mutate()}
          disabled={isPending}
          className="bg-brand text-surface-0 font-medium rounded px-5 py-1.5 text-sm hover:bg-brand-strong disabled:opacity-50"
        >
          {approveMut.isPending ? 'Approving...' : 'Approve'}
        </button>
      </div>
    </div>
  )
}

// ── Plugin pairing approval card ─────────────────────────────────────────────

function PluginPairCard({ request: pr }: { request: PluginPairRequest }) {
  const qc = useQueryClient()
  const [autoApproval, setAutoApproval] = useState(false)
  const [result, setResult] = useState<string | null>(null)

  const isAgentAdd = !!pr.bridge_token_id

  const approveMut = useMutation({
    mutationFn: () => api.plugin.approvePair(pr.id, isAgentAdd ? false : autoApproval),
    onSuccess: () => {
      setResult('Approved')
      qc.invalidateQueries({ queryKey: ['plugin-pairs'] })
      qc.invalidateQueries({ queryKey: ['bridges'] })
      qc.invalidateQueries({ queryKey: ['agents'] })
      qc.invalidateQueries({ queryKey: ['overview'] })
    },
    onError: (err: Error) => setResult(`Failed: ${err.message}`),
  })

  const denyMut = useMutation({
    mutationFn: () => api.plugin.denyPair(pr.id),
    onSuccess: () => {
      setResult('Denied')
      qc.invalidateQueries({ queryKey: ['plugin-pairs'] })
    },
    onError: (err: Error) => setResult(`Failed: ${err.message}`),
  })

  const isPending = approveMut.isPending || denyMut.isPending

  if (result) {
    return (
      <div className="border border-border-default rounded-md bg-surface-1 px-5 py-4">
        <div className="flex items-center justify-between">
          <span className="font-medium text-text-primary">
            {isAgentAdd ? 'Agent add' : 'Plugin pairing'}: {pr.hostname || pr.install_fingerprint}
          </span>
          <span className={`text-xs font-medium px-2 py-0.5 rounded ${
            result === 'Approved' ? 'bg-success/10 text-success' :
            result === 'Denied' ? 'bg-danger/10 text-danger' :
            'bg-surface-2 text-text-tertiary'
          }`}>
            {result}
          </span>
        </div>
      </div>
    )
  }

  return (
    <div className="bg-surface-1 border border-border-default rounded-md border-l-[3px] border-l-brand overflow-hidden">
      <div className="px-5 pt-5 pb-4">
        <div className="flex items-center justify-between">
          <span className="font-medium text-text-primary">
            {isAgentAdd ? 'Agent add request' : 'Plugin pairing'}
          </span>
          <CountdownTimer expiresAt={pr.expires_at} />
        </div>
        <div className="mt-2 space-y-1 text-xs text-text-tertiary">
          {pr.hostname && <div>Host: <code className="font-mono">{pr.hostname}</code></div>}
          <div>Fingerprint: <code className="font-mono">{pr.install_fingerprint || '—'}</code></div>
          {pr.agent_ids.length > 0 && (
            <div>
              {isAgentAdd ? 'New agent: ' : 'Agents: '}
              <code className="font-mono">{pr.agent_ids.join(', ')}</code>
            </div>
          )}
          <div>Requested {formatDistanceToNow(new Date(pr.created_at), { addSuffix: true })}</div>
        </div>
        {!isAgentAdd && (
          <label className="flex items-start gap-2 mt-4 text-sm text-text-secondary cursor-pointer">
            <input
              type="checkbox"
              checked={autoApproval}
              onChange={e => setAutoApproval(e.target.checked)}
              className="mt-0.5"
            />
            <span>
              Allow this plugin to drive <strong>auto-approval</strong> from observed conversations.
              You can toggle this later without re-pairing.
            </span>
          </label>
        )}
      </div>

      <div className="px-4 py-3 border-t border-border-subtle flex items-center justify-end gap-2">
        <button
          onClick={() => denyMut.mutate()}
          disabled={isPending}
          className="rounded px-4 py-1.5 text-sm font-medium bg-danger/10 text-danger border border-danger/20 hover:bg-danger/20 disabled:opacity-50"
        >
          Deny
        </button>
        <button
          onClick={() => approveMut.mutate()}
          disabled={isPending}
          className="bg-brand text-surface-0 font-medium rounded px-5 py-1.5 text-sm hover:bg-brand-strong disabled:opacity-50"
        >
          {approveMut.isPending ? 'Approving...' : 'Approve'}
        </button>
      </div>
    </div>
  )
}

// ── Active bridge row (toggle auto-approval / revoke) ────────────────────────

function BridgeRow({ bridge }: { bridge: BridgeToken }) {
  const qc = useQueryClient()
  const [autoApproval, setAutoApproval] = useState(bridge.auto_approval_enabled)
  const [installArtifact, setInstallArtifact] = useState<import('../api/client').ProxyEnableResponse | null>(null)

  const patchMut = useMutation({
    mutationFn: (enabled: boolean) => api.plugin.patchBridge(bridge.id, { auto_approval_enabled: enabled }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['bridges'] })
    },
    onError: () => {
      // Revert optimistic state on failure.
      setAutoApproval(bridge.auto_approval_enabled)
    },
  })

  const revokeMut = useMutation({
    mutationFn: () => api.plugin.revokeBridge(bridge.id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['bridges'] })
    },
  })

  const enableProxyMut = useMutation({
    mutationFn: () => api.plugin.enableProxy(bridge.id),
    onSuccess: (artifact) => {
      setInstallArtifact(artifact)
      qc.invalidateQueries({ queryKey: ['bridges'] })
    },
  })

  const disableProxyMut = useMutation({
    mutationFn: () => api.plugin.disableProxy(bridge.id),
    onSuccess: () => {
      setInstallArtifact(null)
      qc.invalidateQueries({ queryKey: ['bridges'] })
    },
  })

  return (
    <div className="bg-surface-1 border border-border-default rounded-md px-5 py-4 flex flex-col gap-3">
      <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-3">
        <div className="min-w-0">
          <div className="font-medium text-text-primary truncate flex items-center gap-2">
            {bridge.hostname || bridge.install_fingerprint || bridge.id}
            {bridge.proxy_enabled && (
              <span className="text-[10px] uppercase tracking-wider font-semibold px-1.5 py-0.5 rounded bg-success/10 text-success border border-success/20">
                Proxy
              </span>
            )}
          </div>
          <p className="text-xs text-text-tertiary mt-0.5">
            Paired {formatDistanceToNow(new Date(bridge.created_at), { addSuffix: true })}
            {bridge.last_used_at && (
              <> · Last forward {formatDistanceToNow(new Date(bridge.last_used_at), { addSuffix: true })}</>
            )}
          </p>
        </div>
        <div className="flex items-center gap-3">
          <label className="flex items-center gap-2 text-sm text-text-secondary cursor-pointer">
            <input
              type="checkbox"
              checked={autoApproval}
              onChange={e => {
                setAutoApproval(e.target.checked)
                patchMut.mutate(e.target.checked)
              }}
              disabled={patchMut.isPending}
            />
            <span>Auto-approve</span>
          </label>
          <button
            onClick={() => {
              if (confirm(`Revoke bridge "${bridge.hostname || bridge.install_fingerprint}"? The plugin will stop forwarding messages and need to re-pair.`)) {
                revokeMut.mutate()
              }
            }}
            disabled={revokeMut.isPending}
            className="text-xs px-3 py-1.5 rounded bg-danger/10 text-danger border border-danger/20 hover:bg-danger/20 disabled:opacity-50"
          >
            Revoke
          </button>
        </div>
      </div>

      {/* Network Proxy section (Stage 1) — opt-in per bridge. */}
      <div className="pt-3 border-t border-border-default">
        {bridge.proxy_enabled ? (
          <div className="flex flex-col gap-2">
            <div className="flex items-center justify-between">
              <div>
                <div className="text-sm font-medium text-text-primary">Network Proxy enabled</div>
                <div className="text-xs text-text-tertiary mt-0.5">
                  Transcripts captured at the wire; plugin scavenger is dormant for this bridge.
                </div>
              </div>
              <button
                onClick={() => {
                  if (confirm('Disable the Network Proxy for this bridge? The plugin scavenger will resume and the proxy container (if still running) will need to be torn down manually.')) {
                    disableProxyMut.mutate()
                  }
                }}
                disabled={disableProxyMut.isPending}
                className="text-xs px-3 py-1.5 rounded border border-border-default hover:bg-surface-2 disabled:opacity-50"
              >
                Disable Proxy
              </button>
            </div>
            {installArtifact && (
              <InstallArtifactViewer artifact={installArtifact} onClose={() => setInstallArtifact(null)} />
            )}
          </div>
        ) : (
          <div className="flex items-center justify-between gap-3">
            <div>
              <div className="text-sm font-medium text-text-primary">Network Proxy (Beta)</div>
              <div className="text-xs text-text-tertiary mt-0.5">
                Opt into tamper-proof transcripts captured at the wire via the Clawvisor Proxy. Requires a Docker setup step. <a className="underline" href="https://github.com/clawvisor/clawvisor/blob/main/docs/design-proxy-stage1.md" target="_blank" rel="noopener noreferrer">Learn more</a>.
              </div>
            </div>
            <button
              onClick={() => enableProxyMut.mutate()}
              disabled={enableProxyMut.isPending}
              className="text-xs px-3 py-1.5 rounded bg-accent text-accent-foreground hover:bg-accent/90 disabled:opacity-50 whitespace-nowrap"
            >
              {enableProxyMut.isPending ? 'Enabling…' : 'Enable Network Proxy'}
            </button>
          </div>
        )}
      </div>
    </div>
  )
}

// InstallArtifactViewer renders the one-time install artifact returned
// by EnableProxy: the cvisproxy_ token (shown ONCE) plus the docker-
// compose / install-script templates with copy-to-clipboard buttons.
function InstallArtifactViewer({ artifact, onClose }: { artifact: import('../api/client').ProxyEnableResponse; onClose: () => void }) {
  const [tab, setTab] = useState<'compose' | 'script' | 'plugin'>('compose')
  const copy = (text: string) => {
    navigator.clipboard.writeText(text).catch(() => { /* clipboard denied; user can select + copy manually */ })
  }
  return (
    <div className="mt-2 border border-accent/30 rounded-md bg-surface-2 p-4 flex flex-col gap-3">
      <div className="flex items-start justify-between">
        <div>
          <div className="text-sm font-semibold text-text-primary">Proxy install artifact</div>
          <div className="text-xs text-text-tertiary mt-0.5">
            Generated {formatDistanceToNow(new Date(artifact.generated_at), { addSuffix: true })}. Apply this on the host running your OpenClaw container.
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
        {(['compose', 'script', 'plugin'] as const).map(t => (
          <button
            key={t}
            onClick={() => setTab(t)}
            className={`px-3 py-1.5 border-b-2 ${tab === t ? 'border-accent text-accent' : 'border-transparent text-text-tertiary hover:text-text-primary'}`}
          >
            {t === 'compose' ? 'clawvisor-proxy.yml (compose override)' : t === 'script' ? 'install.sh (native)' : 'plugin secrets'}
          </button>
        ))}
      </div>
      {tab === 'compose' && (
        <div className="text-[11px] text-text-tertiary px-1">
          Compose <em>override</em> — save next to your existing <code>docker-compose.yml</code> and run:
          <code className="block bg-surface-1 p-2 mt-1 rounded font-mono">docker compose -f docker-compose.yml -f clawvisor-proxy.yml up -d</code>
        </div>
      )}

      <pre className="bg-surface-1 p-3 rounded text-[11px] font-mono overflow-x-auto max-h-64 text-text-primary">
        {tab === 'compose'
          ? artifact.docker_compose_yaml
          : tab === 'script'
            ? artifact.install_script
            : artifact.plugin_secrets_json}
      </pre>
      <div className="flex gap-2">
        <button
          onClick={() => copy(
            tab === 'compose' ? artifact.docker_compose_yaml :
            tab === 'script' ? artifact.install_script :
            artifact.plugin_secrets_json
          )}
          className="text-xs px-3 py-1.5 rounded border border-border-default hover:bg-surface-2"
        >
          Copy to clipboard
        </button>
      </div>
    </div>
  )
}

