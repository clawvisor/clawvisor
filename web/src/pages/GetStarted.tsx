import { useState, useEffect, useMemo, type ReactNode } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Link } from 'react-router-dom'
import { api, type TaskSuggestion, type WelcomeData, type WelcomeService, type WelcomeAgent, type WalkthroughExample } from '../api/client'
import { ServiceIcon } from '../components/ServiceIcon'
import { useAuth } from '../hooks/useAuth'
import { copyText } from '../lib/clipboard'

// ── Main Component ────────────────────────────────────────────────────────────

export default function GetStarted() {
  const { data, isLoading, refetch, isFetching } = useQuery({
    queryKey: ['welcome'],
    queryFn: () => api.welcome.suggestions(),
    staleTime: 5 * 60_000,
    refetchOnWindowFocus: false,
  })

  const ready = !!data?.ready
  const services = data?.services ?? []
  const agents = data?.agents ?? []

  const sectionIds = useMemo(() => ready 
    ? ['overview', 'suggestions', 'your-setup', 'how-it-works']
    : ['overview', 'connect-service', 'connect-agent', 'how-it-works'], 
  [ready])
    
  const activeSection = useScrollSpy(sectionIds, isLoading)

  return (
    <div className="w-full max-w-6xl mx-auto px-4 sm:px-6 md:px-8 lg:px-10 py-10 scroll-smooth">
      <div className="flex gap-8 lg:gap-10 xl:gap-16 items-start">

        {/* ── Main content column ── */}
        <div className="flex-1 min-w-0 space-y-14">
          <Hero ready={ready} services={services} agents={agents} isLoading={isLoading} />

          {isLoading ? (
            <LoadingState />
          ) : ready ? (
            <>
              <SuggestionsSection data={data} isLoading={isLoading} isFetching={isFetching} onRefresh={() => refetch()} />
              <YourSetupSection services={services} agents={agents} />
              <ExampleWalkthrough example={data?.walkthrough} />
            </>
          ) : (
            <>
              <SetupSteps services={services} agents={agents} isLoading={isLoading} />
              <ExampleWalkthrough example={data?.walkthrough} />
            </>
          )}
        </div>

        {/* ── "On this page" right sidebar ── */}
        <aside className="hidden lg:block w-48 shrink-0 sticky top-24 self-start max-h-[calc(100vh-8rem)] overflow-y-auto custom-scrollbar">
          <p className="flex items-center gap-1.5 text-[11px] font-semibold uppercase tracking-widest text-text-tertiary mb-4">
            <svg xmlns="http://www.w3.org/2000/svg" fill="none" viewBox="0 0 24 24" strokeWidth="2" stroke="currentColor" className="w-3.5 h-3.5">
              <path strokeLinecap="round" strokeLinejoin="round" d="M3.75 6.75h16.5M3.75 12h16.5m-16.5 5.25H12" />
            </svg>
            On this page
          </p>
          <nav className="flex flex-col gap-0.5">
            <PageIndexLink 
              href="#overview" 
              label="Overview" 
              active={activeSection === 'overview'} 
              icon={<InfoIcon />} 
            />
            
            <div className="h-px bg-border-subtle my-2" />

            {ready ? (
              <>
                <PageIndexLink 
                  href="#suggestions" 
                  label="Suggestions" 
                  active={activeSection === 'suggestions'} 
                  icon={<SparklesIcon />} 
                />
                <PageIndexLink 
                  href="#your-setup" 
                  label="Your setup" 
                  active={activeSection === 'your-setup'} 
                  icon={<GridIcon />} 
                />
              </>
            ) : (
              <>
                <PageIndexLink 
                  href="#connect-service" 
                  label="Connect a service" 
                  active={activeSection === 'connect-service'} 
                  icon={<PlugIcon />} 
                />
                <PageIndexLink 
                  href="#connect-agent" 
                  label="Connect an agent" 
                  active={activeSection === 'connect-agent'} 
                  icon={<BotIcon />} 
                />
              </>
            )}

            <div className="h-px bg-border-subtle my-2" />
            
            <PageIndexLink 
              href="#how-it-works" 
              label="How a task works" 
              active={activeSection === 'how-it-works'} 
              icon={<TaskIcon />} 
            />
          </nav>
        </aside>

      </div>
    </div>
  )
}

function PageIndexLink({
  href,
  label,
  active,
  icon,
}: {
  href: string
  label: string
  active?: boolean
  icon?: ReactNode
}) {
  return (
    <a
      href={href}
      className={`flex items-center gap-2 text-[13px] leading-snug py-1.5 px-2 rounded-md transition-colors ${
        active
          ? 'text-brand font-medium bg-brand-muted'
          : 'text-text-secondary hover:text-text-primary hover:bg-surface-2'
      }`}
    >
      <div className={`shrink-0 ${active ? 'text-brand' : 'text-text-tertiary'}`}>
        {icon}
      </div>
      {label}
    </a>
  )
}

// ── Loading state ─────────────────────────────────────────────────────────────

function LoadingState() {
  return (
    <div className="space-y-10" aria-busy="true" aria-live="polite">
      <section>
        <div className="flex items-center gap-2 mb-4">
          <LoadingSpinner />
          <h2 className="text-sm font-semibold uppercase tracking-widest text-text-tertiary">Checking your setup…</h2>
        </div>
        <div className="space-y-2.5">
          <SkeletonServiceRow />
          <SkeletonServiceRow />
          <SkeletonServiceRow />
        </div>
      </section>
      <section>
        <div className="flex items-center gap-2 mb-4">
          <LoadingSpinner />
          <h2 className="text-sm font-semibold uppercase tracking-widest text-text-tertiary">Generating task ideas…</h2>
        </div>
        <SuggestionsLoading />
      </section>
    </div>
  )
}

function SkeletonServiceRow() {
  return (
    <div className="flex items-center gap-4 rounded-xl border border-border-subtle bg-surface-1 px-4 py-3.5 animate-pulse">
      <div className="w-9 h-9 rounded-lg bg-surface-2 shrink-0" />
      <div className="flex-1 space-y-2">
        <div className="h-3.5 bg-surface-2 rounded w-24" />
        <div className="h-3 bg-surface-2 rounded w-48" />
      </div>
    </div>
  )
}

function LoadingSpinner() {
  return (
    <svg
      className="w-4 h-4 animate-spin text-brand"
      fill="none"
      stroke="currentColor"
      strokeWidth="2"
      viewBox="0 0 24 24"
      aria-hidden
    >
      <circle cx="12" cy="12" r="10" opacity="0.25" />
      <path d="M22 12a10 10 0 01-10 10" />
    </svg>
  )
}

// ── Hero ──────────────────────────────────────────────────────────────────────

function Hero({
  ready,
  services,
  agents,
  isLoading,
}: {
  ready: boolean
  services: WelcomeService[]
  agents: WelcomeAgent[]
  isLoading: boolean
}) {
  return (
    <header id="overview" className="space-y-4">
      <p className="text-xs font-semibold uppercase tracking-widest text-brand">Quickstart</p>
      
      <h1 className="text-4xl sm:text-5xl font-bold text-text-primary tracking-tight leading-[1.15]">
        Your agents act.<br />You stay in control.
      </h1>
      
      <p className="text-base sm:text-lg text-text-secondary leading-relaxed max-w-2xl">
        Clawvisor sits between your AI agents and the APIs they use.
        Agents declare tasks — you approve the scope once.
        Credential injection, execution, and audit logging handled for you.
      </p>
      {isLoading ? (
        <p className="text-sm text-text-tertiary">Loading your setup…</p>
      ) : ready ? (
        <p className="text-sm text-text-tertiary">
          {services.length} service{services.length === 1 ? '' : 's'} connected
          {' · '}
          {agents.length} agent{agents.length === 1 ? '' : 's'} registered
        </p>
      ) : null}
    </header>
  )
}

// ── Setup steps (not-ready state) ─────────────────────────────────────────────

function SetupSteps({
  services,
  agents,
  isLoading,
}: {
  services: WelcomeService[]
  agents: WelcomeAgent[]
  isLoading: boolean
}) {
  const hasService = services.length > 0
  const hasAgent = agents.length > 0

  return (
    <section className="space-y-6">
      {!isLoading && (
       <h2 className="text-xl font-semibold text-text-primary tracking-tight">
          {hasService ? 'Continue your workspace setup' : 'Complete your workspace setup'}
        </h2>
      )}

      {/* ── Step 1: Connect a service ── */}
      <SetupStepCard id="connect-service" num={1} done={hasService} title="Connect a service" loading={isLoading}>
        <p className="text-sm text-text-secondary mb-5 leading-relaxed">
          Link an API like Gmail, GitHub, Slack, or Linear so your agents have something to act
          on. Credentials stay in Clawvisor's vault — agents never see them.
        </p>

        {hasService ? (
          <ConnectedServicesStrip services={services} />
        ) : (
          <>
            <div className="grid grid-cols-1 md:grid-cols-2 gap-3 mb-5">
              <ServiceRow
                id="google.gmail"
                label="Gmail"
                description="Read, send, and label emails on your behalf"
                icon={<img src="/logos/google-gmail.svg" alt="Gmail" className="w-5 h-5 object-contain" />}
              />
              <ServiceRow
                id="github"
                label="GitHub"
                description="Open issues, review PRs, push commits"
                icon={<img src="/logos/github.svg" alt="GitHub" className="w-5 h-5 object-contain dark:invert" />}
              />
              <ServiceRow
                id="slack"
                label="Slack"
                description="Post messages and read channel history"
                icon={<img src="/logos/slack.svg" alt="Slack" className="w-5 h-5 object-contain" />}
              />
              <ServiceRow
                id="google.calendar"
                label="Google Calendar"
                description="Create events, check availability, RSVP"
                icon={<img src="/logos/google-calendar.svg" alt="Google Calendar" className="w-5 h-5 object-contain" />}
              />
            </div>
            <Link 
              to="/dashboard/accounts" 
              className="inline-flex items-center gap-1.5 text-sm font-medium text-brand hover:text-brand-strong px-3.5 py-2 rounded-lg border border-brand/40 bg-brand-muted transition-colors"
              >
              <GridAppIcon className="w-4 h-4" />
              Browse all services
            </Link>
          </>
        )}
      </SetupStepCard>

      {/* ── Step 2: Connect an agent ── */}
      <SetupStepCard id="connect-agent" num={2} done={hasAgent} title="Connect an agent" loading={isLoading}>
        <p className="text-sm text-text-secondary mb-5 leading-relaxed">
          Pair an AI agent (Claude Code, Claude Desktop, OpenClaw, or any HTTP client) with
          Clawvisor so it can create tasks on your behalf.
        </p>
        {hasAgent ? (
          <ConnectedAgentsStrip agents={agents} />
        ) : (
          <div className="grid grid-cols-1 md:grid-cols-2 gap-3">
            <AgentRow 
              tab="claude-code"    
              label="Claude Code"    
              description="CLI-based coding agent" 
              icon={<img src="/logos/claude-color.svg" alt="Claude Code" className="w-5 h-5 object-contain" />}
            />
            <AgentRow 
              tab="claude-desktop" 
              label="Claude Desktop" 
              description="Desktop app agent" 
              icon={<img src="/logos/claude-color.svg" alt="Claude Desktop" className="w-5 h-5 object-contain" />}
            />
            <AgentRow 
              tab="openclaw"       
              label="OpenClaw"       
              description="Open-source client" 
              icon={<img src="/logos/openclaw.svg" alt="OpenClaw" className="w-5 h-5 object-contain" />}
            />
            <AgentRow 
              tab="other"          
              label="Other agents"   
              description="Any HTTP client" 
              icon={<OtherAgentIcon className="w-5 h-5 text-text-tertiary" />}
            />
          </div>
        )}
      </SetupStepCard>

    </section>
  )
}

function ServiceRow({
  id,
  label,
  description,
  icon,
}: {
  id: string
  label: string
  description: string
  icon: ReactNode
}) {
  return (
    <Link
      to={`/dashboard/accounts?search=${encodeURIComponent(id)}`}
      className="flex items-center gap-4 rounded-xl border border-border-subtle bg-surface-0 hover:bg-surface-1 hover:border-border-secondary px-4 py-3.5 transition-colors group"
    >
      <div className="w-9 h-9 shrink-0 rounded-lg flex items-center justify-center bg-surface-1 group-hover:bg-surface-2 transition-colors overflow-hidden">
        {icon}
      </div>
      <div className="flex-1 min-w-0">
        <p className="text-sm font-semibold text-text-primary">{label}</p>
        <p className="text-xs text-text-secondary mt-0.5 leading-relaxed">{description}</p>
      </div>
      <svg className="w-4 h-4 text-text-tertiary shrink-0 opacity-0 group-hover:opacity-100 transition-opacity" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24">
        <path strokeLinecap="round" strokeLinejoin="round" d="M9 5l7 7-7 7" />
      </svg>
    </Link>
  )
}

function AgentRow({
  tab,
  label,
  description,
  icon,
}: {
  tab: string
  label: string
  description: string
  icon?: ReactNode
}) {
  return (
    <Link
      to={`/dashboard/agents?agent=${encodeURIComponent(tab)}`}
      className="flex items-center gap-4 rounded-xl border border-border-subtle bg-surface-0 hover:bg-surface-1 hover:border-border-secondary px-4 py-3.5 transition-colors group"
    >
      {icon && (
        <div className="w-9 h-9 shrink-0 rounded-lg flex items-center justify-center bg-surface-1 group-hover:bg-surface-2 transition-colors overflow-hidden">
          {icon}
        </div>
      )}
      <div className="flex-1 min-w-0">
        <p className="text-sm font-semibold text-text-primary">{label}</p>
        <p className="text-xs text-text-secondary mt-0.5 leading-relaxed">{description}</p>
      </div>
      <svg className="w-4 h-4 text-text-tertiary shrink-0 opacity-0 group-hover:opacity-100 transition-opacity" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24">
        <path strokeLinecap="round" strokeLinejoin="round" d="M9 5l7 7-7 7" />
      </svg>
    </Link>
  )
}

// ── Components (used in connected strips) ─────────────────────────────────────

function ConnectedServicesStrip({ services }: { services: WelcomeService[] }) {
  return (
    <div className="flex flex-wrap gap-2">
      {services.map(s => (
        <div
          key={`${s.id}:${s.alias ?? ''}`}
          className="flex items-center gap-2 bg-surface-0 border border-border-subtle px-2.5 py-1.5 rounded-md"
        >
          {/* Automatically invert the logo in dark mode if it is GitHub */}
          <div className={s.id === 'github' ? 'dark:invert' : ''}>
            <ServiceIcon iconUrl={s.icon_url} iconSvg={s.icon_svg} serviceId={s.id} size={16} />
          </div>
          <span className="text-sm text-text-primary">{s.name}</span>
          {s.alias && <span className="text-xs text-text-tertiary">({s.alias})</span>}
        </div>
      ))}
      <Link
        to="/dashboard/accounts"
        className="text-sm text-brand hover:text-brand-strong font-medium px-2.5 py-1.5"
      >
        Connect another →
      </Link>
    </div>
  )
}

function ConnectedAgentsStrip({ agents }: { agents: WelcomeAgent[] }) {
  return (
    <div className="flex flex-wrap gap-2">
      {agents.map(a => (
        <div
          key={a.id}
          className="flex items-center gap-2 bg-surface-0 border border-border-subtle px-2.5 py-1.5 rounded-md"
        >
          <BotIcon />
          <span className="text-sm font-mono text-text-primary">{a.name}</span>
        </div>
      ))}
      <Link
        to="/dashboard/agents"
        className="text-sm text-brand hover:text-brand-strong font-medium px-2.5 py-1.5"
      >
        Connect another →
      </Link>
    </div>
  )
}

// ── SetupStepCard ─────────────────────────────────────────────────────────────

function SetupStepCard({
  id,
  num,
  done,
  title,
  loading,
  children,
}: {
  id?: string
  num: number
  done: boolean
  title: string
  loading: boolean
  children: ReactNode
}) {
  return (
    <div
      id={id}
      className={`rounded-xl border px-6 py-5 transition-colors scroll-mt-24 ${
        done ? 'border-success/30 bg-success/5' : 'border-border-subtle bg-surface-1'
      }`}
    >
      <div className="flex items-center gap-3 mb-4">
        <div
          className={`w-7 h-7 rounded-full shrink-0 flex items-center justify-center text-sm font-bold ${
            done ? 'bg-success text-surface-0' : 'bg-brand-muted text-brand'
          }`}
        >
          {done ? (
            <svg className="w-4 h-4" fill="none" stroke="currentColor" strokeWidth="3" viewBox="0 0 24 24">
              <path d="M5 13l4 4L19 7" />
            </svg>
          ) : (
            <span>{num}</span>
          )}
        </div>
        <h3 className="font-semibold text-base text-text-primary flex-1">{title}</h3>
        {loading ? (
          <span className="text-xs text-text-tertiary">checking…</span>
        ) : done ? (
          <span className="text-xs font-semibold text-success uppercase tracking-wider">Done</span>
        ) : null}
      </div>
      {children}
    </div>
  )
}

// ── "Your setup" recap (ready state) ──────────────────────────────────────────

function YourSetupSection({ services, agents }: { services: WelcomeService[]; agents: WelcomeAgent[] }) {
  return (
    <section id="your-setup" className="scroll-mt-24">
      <h2 className="text-xl font-semibold text-text-primary mb-4">Your setup</h2>
      <div className="grid gap-4 md:grid-cols-2">
        <div className="rounded-xl border border-border-subtle bg-surface-1 p-5">
          <div className="flex items-baseline justify-between mb-4">
            <h3 className="font-medium text-text-primary">Services</h3>
            <Link to="/dashboard/accounts" className="text-xs text-brand hover:text-brand-strong font-medium">
              Manage →
            </Link>
          </div>
          <ConnectedServicesStrip services={services} />
        </div>
        <div className="rounded-xl border border-border-subtle bg-surface-1 p-5">
          <div className="flex items-baseline justify-between mb-4">
            <h3 className="font-medium text-text-primary">Agents</h3>
            <Link to="/dashboard/agents" className="text-xs text-brand hover:text-brand-strong font-medium">
              Manage →
            </Link>
          </div>
          <ConnectedAgentsStrip agents={agents} />
        </div>
      </div>
    </section>
  )
}

// ── Suggestions ───────────────────────────────────────────────────────────────

function SuggestionsSection({
  data,
  isLoading,
  isFetching,
  onRefresh,
}: {
  data?: WelcomeData
  isLoading: boolean
  isFetching: boolean
  onRefresh: () => void
}) {
  const suggestions = data?.suggestions ?? []
  const llmStatus = data?.llm_status
  const services = data?.services ?? []

  const serviceById = new Map<string, WelcomeService>()
  for (const s of services) serviceById.set(s.id, s)

  return (
    <section id="suggestions" className="scroll-mt-24">
      <div className="flex items-baseline justify-between mb-5">
        <div>
          <h2 className="text-xl font-semibold text-text-primary">Things to try</h2>
          {data?.llm_used && (
            <p className="text-sm text-text-tertiary mt-0.5">
              Personalized for your setup · copy a prompt and paste it into your agent
            </p>
          )}
        </div>
        {data?.llm_used && (
          <button
            onClick={onRefresh}
            disabled={isFetching}
            className="text-xs font-medium text-text-secondary hover:text-text-primary disabled:opacity-50 transition-colors flex items-center gap-1.5"
          >
            <svg
              className={`w-3.5 h-3.5 ${isFetching ? 'animate-spin' : ''}`}
              fill="none"
              stroke="currentColor"
              strokeWidth="2"
              viewBox="0 0 24 24"
            >
              <path d="M23 4v6h-6M1 20v-6h6" />
              <path d="M3.51 9a9 9 0 0114.85-3.36L23 10M1 14l4.64 4.36A9 9 0 0020.49 15" />
            </svg>
            New ideas
          </button>
        )}
      </div>
      {isLoading ? (
        <SuggestionsLoading />
      ) : suggestions.length > 0 ? (
        <div
          className={`grid gap-4 md:grid-cols-2 transition-opacity ${isFetching ? 'opacity-50' : ''}`}
          aria-busy={isFetching}
        >
          {suggestions.map((s, i) => (
            <SuggestionCard key={i} suggestion={s} serviceById={serviceById} />
          ))}
        </div>
      ) : (
        <SuggestionsFallback status={llmStatus} />
      )}
    </section>
  )
}

function SuggestionCard({
  suggestion,
  serviceById,
}: {
  suggestion: TaskSuggestion
  serviceById: Map<string, WelcomeService>
}) {
  const [copied, setCopied] = useState(false)

  function copy() {
    copyText(suggestion.prompt).then(() => {
      setCopied(true)
      setTimeout(() => setCopied(false), 1500)
    })
  }

  return (
    <div className="group rounded-xl border border-border-subtle bg-surface-1 hover:border-border-secondary hover:bg-surface-0 transition-colors flex flex-col overflow-hidden">
      <div className="px-5 pt-5 pb-4 flex items-start justify-between gap-3">
        <h3 className="font-semibold text-text-primary leading-snug">{suggestion.title}</h3>
        {suggestion.risk && <RiskBadge level={suggestion.risk} />}
      </div>

      <div className="px-5 pb-4 flex-1">
        {suggestion.agent && (
          <p className="text-xs text-text-tertiary mb-1.5">
            Ask <span className="font-mono text-brand">{suggestion.agent}</span> to:
          </p>
        )}
        <div className="rounded-lg bg-surface-2 border border-border-subtle px-4 py-3">
          <p className="text-sm text-text-primary leading-relaxed italic whitespace-pre-wrap">
            {suggestion.prompt}
          </p>
        </div>
      </div>

      <div className="px-5 py-3 border-t border-border-subtle bg-surface-2 flex items-center justify-between gap-3">
        <div className="flex flex-wrap gap-1.5 min-w-0">
          {suggestion.services.map(id => {
            const svc = serviceById.get(id)
            return (
              <span
                key={id}
                className="inline-flex items-center gap-1 text-xs px-2 py-0.5 rounded-full bg-surface-0 border border-border-subtle text-text-tertiary"
                title={svc?.name ?? id}
              >
                {svc && (
                  <div className={id === 'github' ? 'dark:invert' : ''}>
                    <ServiceIcon iconUrl={svc.icon_url} iconSvg={svc.icon_svg} serviceId={id} size={11} />
                  </div>
                )}
                <span>{svc?.name ?? id}</span>
              </span>
            )
          })}
        </div>

        <button
          onClick={copy}
          className="shrink-0 inline-flex items-center gap-1.5 text-xs font-medium text-text-secondary hover:text-text-primary transition-colors px-2.5 py-1.5 rounded-lg hover:bg-surface-1 border border-transparent hover:border-border-subtle"
        >
          {copied ? (
            <>
              <svg className="w-3.5 h-3.5 text-success" fill="none" stroke="currentColor" strokeWidth="2.5" viewBox="0 0 24 24">
                <path d="M5 13l4 4L19 7" />
              </svg>
              Copied
            </>
          ) : (
            <>
              <svg className="w-3.5 h-3.5" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24">
                <rect x="9" y="9" width="13" height="13" rx="2" ry="2" />
                <path d="M5 15H4a2 2 0 01-2-2V4a2 2 0 012-2h9a2 2 0 012 2v1" />
              </svg>
              Copy prompt
            </>
          )}
        </button>
      </div>
    </div>
  )
}

function RiskBadge({ level }: { level: 'low' | 'medium' | 'high' }) {
  const styles = {
    low: 'bg-success/10 text-success border-success/30',
    medium: 'bg-warning/10 text-warning border-warning/30',
    high: 'bg-danger/10 text-danger border-danger/30',
  }[level]
  return (
    <span className={`shrink-0 text-[10px] font-semibold uppercase tracking-wider px-1.5 py-0.5 rounded border ${styles}`}>
      {level} risk
    </span>
  )
}

function SuggestionsLoading() {
  return (
    <div className="grid gap-4 md:grid-cols-2">
      {[0, 1, 2, 3].map(i => (
        <div key={i} className="rounded-xl border border-border-subtle bg-surface-1 overflow-hidden animate-pulse">
          <div className="px-5 pt-5 pb-4 space-y-2">
            <div className="h-4 bg-surface-2 rounded w-1/2" />
            <div className="h-3 bg-surface-2 rounded w-5/6" />
          </div>
          <div className="px-5 pb-4">
            <div className="rounded-lg bg-surface-2 border border-border-subtle px-4 py-3 space-y-2">
              <div className="h-3 bg-surface-3 rounded w-full" />
              <div className="h-3 bg-surface-3 rounded w-4/5" />
              <div className="h-3 bg-surface-3 rounded w-3/4" />
            </div>
          </div>
          <div className="px-5 py-3 border-t border-border-subtle bg-surface-2 flex justify-between items-center">
            <div className="flex gap-1.5">
              <div className="h-5 w-16 bg-surface-3 rounded-full" />
              <div className="h-5 w-16 bg-surface-3 rounded-full" />
            </div>
            <div className="h-6 w-24 bg-surface-3 rounded-lg" />
          </div>
        </div>
      ))}
    </div>
  )
}

function SuggestionsFallback({ status }: { status?: string }) {
  const { features } = useAuth()
  const multiTenant = !!features?.multi_tenant
  const message =
    status === 'unconfigured'
      ? multiTenant
        ? 'Personalized task suggestions are temporarily unavailable.'
        : "Personalized task suggestions need an LLM API key. Add one in Settings to see ideas tailored to what you've connected."
      : status === 'exhausted'
        ? multiTenant
          ? 'Personalized task suggestions are temporarily unavailable.'
          : 'The free LLM credit is exhausted. Add your own API key in Settings to keep seeing personalized suggestions.'
        : "Couldn't generate suggestions right now — try refreshing in a minute."
  return (
    <div className="rounded-xl border border-border-subtle bg-surface-1 px-5 py-5 text-sm text-text-secondary leading-relaxed">
      {message}
      {!multiTenant && (status === 'unconfigured' || status === 'exhausted') && (
        <>
          {' '}
          <Link to="/dashboard/settings" className="text-brand hover:text-brand-strong font-medium">
            Open Settings
          </Link>
        </>
      )}
    </div>
  )
}

// ── Example walkthrough ───────────────────────────────────────────────────────

const DEFAULT_WALKTHROUGH: WalkthroughExample = {
  user_prompt: 'Triage my Gmail and add anything actionable to Linear.',
  agent_task:
    'read Gmail messages received in the last 72 hours, create items in Linear.',
  primary_name: 'Gmail',
  secondary_name: 'Linear',
}

function ExampleWalkthrough({ example }: { example?: WalkthroughExample }) {
  const ex = example ?? DEFAULT_WALKTHROUGH
  const personalized = !!example

  const steps: { label: string; body: string; detail?: string }[] = [
    {
      label: 'You ask',
      body: `"${ex.user_prompt}"`,
    },
    {
      label: 'Agent declares a task',
      body: `The agent creates a Clawvisor task: ${ex.agent_task}`,
      detail: 'The agent never holds credentials. It just says what it needs to do.',
    },
    {
      label: 'You approve once',
      body: 'Clawvisor shows the scope + an LLM-powered risk assessment; you approve it in one click.',
      detail: 'High-risk or destructive actions can require per-request approval instead.',
    },
    {
      label: 'Clawvisor enforces it',
      body: 'Every gateway call is checked against restrictions, task scope, and approvals. Everything is audited.',
    },
  ]

  return (
    <section id="how-it-works" className="scroll-mt-24">
      <h2 className="text-xl font-semibold text-text-primary mb-1">
        See Clawvisor in action
      </h2>
      <p className="text-sm text-text-secondary mb-8">
        {personalized
          ? `Using your connected ${ex.primary_name} and ${ex.secondary_name} as an example:`
          : `Here's an example using ${ex.primary_name} and ${ex.secondary_name}:`}
      </p>

      <ol className="relative space-y-0">
        {steps.map((step, i) => {
          const isLast = i === steps.length - 1
          return (
            <li key={i} className="flex gap-5">
              <div className="flex flex-col items-center shrink-0">
                <div className="w-8 h-8 rounded-full bg-brand-muted text-brand text-sm font-bold flex items-center justify-center z-10 shrink-0">
                  {i + 1}
                </div>
                {!isLast && (
                  <div
                    aria-hidden
                    className="w-px flex-1 bg-border-subtle mt-1 mb-1 min-h-[32px]"
                  />
                )}
              </div>
              <div className={`flex-1 ${isLast ? 'pb-0' : 'pb-8'}`}>
                <p className="text-xs font-semibold uppercase tracking-widest text-text-tertiary mb-1 mt-1.5">
                  {step.label}
                </p>
                <p className="text-base font-medium text-text-primary leading-relaxed">
                  {step.body}
                </p>
                {step.detail && (
                  <p className="text-sm text-text-secondary mt-1.5 leading-relaxed">
                    {step.detail}
                  </p>
                )}
              </div>
            </li>
          )
        })}
      </ol>

      <div className="mt-8 flex items-start gap-3 rounded-xl border border-brand/30 bg-brand-muted p-4 text-sm text-text-secondary leading-relaxed shadow-sm">
        <ShieldCheckIcon className="w-5 h-5 shrink-0 text-brand mt-0.5" />
        <div>
          <span className="font-semibold text-brand">Three layers of control</span> check every request,
          in order: <span className="font-medium text-text-primary">restrictions</span> (hard blocks you
          configure), <span className="font-medium text-text-primary">task scopes</span> (what the agent
          declared and you approved), and{' '}
          <span className="font-medium text-text-primary">per-request approval</span> (anything outside the
          scope goes to your queue).
        </div>
      </div>
    </section>
  )
}

// ── Custom Hook for Scroll Spy ────────────────────────────────────────────────
function useScrollSpy(sectionIds: string[], isLoading: boolean) {
  const [activeId, setActiveId] = useState<string>(sectionIds[0] || '')

  useEffect(() => {
    if (isLoading) return;
    const scrollContainer = document.querySelector('main') || window;

    const handleScroll = () => {
      const elements = sectionIds.map(id => document.getElementById(id)).filter(Boolean)
      if (elements.length === 0) return;

      let currentActive: string = elements[0]?.id || '';

      const isWindow = scrollContainer === window;
      const scrollY = isWindow ? window.scrollY : (scrollContainer as HTMLElement).scrollTop;
      const containerHeight = isWindow ? window.innerHeight : (scrollContainer as HTMLElement).clientHeight;
      const scrollHeight = isWindow ? document.body.offsetHeight : (scrollContainer as HTMLElement).scrollHeight;

      const isAtBottom = containerHeight + Math.round(scrollY) >= scrollHeight - 50;
      if (isAtBottom) {
        setActiveId(sectionIds[sectionIds.length - 1]);
        return;
      }
      const detectionLine = window.innerHeight * 0.4;
      
      for (const el of elements) {
        if (!el) continue;
        const rect = el.getBoundingClientRect();
        if (rect.top <= detectionLine) {
          currentActive = el.id;
        }
      }

      setActiveId(currentActive);
    };

    const rafId = requestAnimationFrame(() => handleScroll());
    
    scrollContainer.addEventListener('scroll', handleScroll, { passive: true });
    window.addEventListener('resize', handleScroll, { passive: true });

    return () => {
      cancelAnimationFrame(rafId);
      scrollContainer.removeEventListener('scroll', handleScroll);
      window.removeEventListener('resize', handleScroll);
    };
  }, [sectionIds, isLoading]);

  return activeId || sectionIds[0];
}


// ── Shared Icons ─────────────────────────

export function InfoIcon() {
  return <svg className="w-4 h-4" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24"><circle cx="12" cy="12" r="10" /><path d="M12 16v-4M12 8h.01" /></svg>
}
export function SparklesIcon() {
  return <svg className="w-4 h-4" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24"><path d="M5 3v4M3 5h4M6 17v4M4 19h4M13 3l2.5 5.5L21 11l-5.5 2.5L13 19l-2.5-5.5L5 11l5.5-2.5z" /></svg>
}
export function GridIcon() {
  return <svg className="w-4 h-4" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24"><rect x="3" y="3" width="7" height="7" /><rect x="14" y="3" width="7" height="7" /><rect x="14" y="14" width="7" height="7" /><rect x="3" y="14" width="7" height="7" /></svg>
}
export function PlugIcon() {
  return <svg className="w-4 h-4" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24"><path d="M12 22v-5M9 8V2M15 8V2M19 13c0 2-2 4-7 4s-7-2-7-4V8h14v5z" /></svg>
}
export function BotIcon() {
  return <svg className="w-4 h-4" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24"><rect x="3" y="11" width="18" height="10" rx="2" /><circle cx="12" cy="5" r="2" /><path d="M12 7v4M8 16h.01M16 16h.01" /></svg>
}
export function TaskIcon() {
  return <svg className="w-4 h-4" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24"><path d="M9 5H7a2 2 0 00-2 2v12a2 2 0 002 2h10a2 2 0 002-2V7a2 2 0 00-2-2h-2M9 5a2 2 0 002 2h2a2 2 0 002-2M9 5a2 2 0 012-2h2a2 2 0 012 2m-6 9l2 2 4-4" /></svg>
}
export function GridAppIcon({ className }: { className?: string }) {
  return (
    <svg className={className} viewBox="0 0 24 24" fill="none" xmlns="http://www.w3.org/2000/svg">
     <path d="M17 14V20M14 17H20M15.6 10H18.4C18.9601 10 19.2401 10 19.454 9.89101C19.6422 9.79513 19.7951 9.64215 19.891 9.45399C20 9.24008 20 8.96005 20 8.4V5.6C20 5.03995 20 4.75992 19.891 4.54601C19.7951 4.35785 19.6422 4.20487 19.454 4.10899C19.2401 4 18.9601 4 18.4 4H15.6C15.0399 4 14.7599 4 14.546 4.10899C14.3578 4.20487 14.2049 4.35785 14.109 4.54601C14 4.75992 14 5.03995 14 5.6V8.4C14 8.96005 14 9.24008 14.109 9.45399C14.2049 9.64215 14.3578 9.79513 14.546 9.89101C14.7599 10 15.0399 10 15.6 10ZM5.6 10H8.4C8.96005 10 9.24008 10 9.45399 9.89101C9.64215 9.79513 9.79513 9.64215 9.89101 9.45399C10 9.24008 10 8.96005 10 8.4V5.6C10 5.03995 10 4.75992 9.89101 4.54601C9.79513 4.35785 9.64215 4.20487 9.45399 4.10899C9.24008 4 8.96005 4 8.4 4H5.6C5.03995 4 4.75992 4 4.54601 4.10899C4.35785 4.20487 4.20487 4.35785 4.10899 4.54601C4 4.75992 4 5.03995 4 5.6V8.4C4 8.96005 4 9.24008 4.10899 9.45399C4.20487 9.64215 4.35785 9.79513 4.54601 9.89101C4.75992 10 5.03995 10 5.6 10ZM5.6 20H8.4C8.96005 20 9.24008 20 9.45399 19.891C9.64215 19.7951 9.79513 19.6422 9.89101 19.454C10 19.2401 10 18.9601 10 18.4V15.6C10 15.0399 10 14.7599 9.89101 14.546C9.79513 14.3578 9.64215 14.2049 9.45399 14.109C9.24008 14 8.96005 14 8.4 14H5.6C5.03995 14 4.75992 14 4.54601 14.109C4.35785 14.2049 4.20487 14.3578 4.10899 14.546C4 14.7599 4 15.0399 4 15.6V18.4C4 18.9601 4 19.2401 4.10899 19.454C4.20487 19.6422 4.35785 19.7951 4.54601 19.891C4.75992 20 5.03995 20 5.6 20Z" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  )
}
export function OtherAgentIcon({ className }: { className?: string }) {
  return (
    <svg className={className} fill="none" xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24">
      <path d="M7 8h10M7 12h10M7 16h6" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round"/>
    </svg>
  )
}
export function ShieldCheckIcon({ className }: { className?: string }) {
  return (
    <svg xmlns="http://www.w3.org/2000/svg" fill="none" viewBox="0 0 24 24" strokeWidth="1.5" stroke="currentColor" className={className}>
      <path strokeLinecap="round" strokeLinejoin="round" d="M12 9v3.75m0-10.036A11.959 11.959 0 0 1 3.598 6 11.99 11.99 0 0 0 3 9.75c0 5.592 3.824 10.29 9 11.622 5.176-1.332 9-6.03 9-11.622 0-1.31-.21-2.57-.598-3.75h-.152c-3.196 0-6.1-1.25-8.25-3.286Zm0 13.036h.008v.008H12v-.008Z" />
    </svg>
  )
}
