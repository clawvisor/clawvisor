import { useState, useEffect, useMemo, type ReactNode } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Link } from 'react-router-dom'
import { api, type TaskSuggestion, type WelcomeData, type WelcomeService, type WelcomeAgent } from '../api/client'
import { ServiceIcon } from '../components/ServiceIcon'
import { useAuth } from '../hooks/useAuth'
import ConnectedAgentsStrip from '../components/ConnectedAgentsStrip'
import ConnectedServicesStrip from '../components/ConnectedServicesStrip'
import SetupChecklist from '../components/SetupChecklist'

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
    ? ['overview', 'suggestions', 'your-setup']
    : ['overview', 'connect-agent', 'connect-account', 'resolve-first-task'],
  [ready])
    
  const activeSection = useScrollSpy(sectionIds, isLoading)

  return (
    <div className="page-shell scroll-smooth">
      <div className="flex gap-6 lg:gap-8 items-start w-full">

        {/* ── Main content column ── */}
        <div className="flex-1 min-w-0 space-y-6">
          <div className="space-y-6">
            <Hero ready={ready} services={services} agents={agents} isLoading={isLoading} />
            <SetupChecklist />
          </div>

          {isLoading ? (
            <LoadingState />
          ) : ready ? (
            <>
              <SuggestionsSection data={data} isLoading={isLoading} isFetching={isFetching} onRefresh={() => refetch()} />
              <YourSetupSection services={services} agents={agents} />
            </>
          ) : null}
        </div>

        {/* ── "On this page" right sidebar ── */}
        <aside className="hidden lg:block w-fit shrink-0 sticky top-24 self-start max-h-[calc(100vh-8rem)] overflow-y-auto custom-scrollbar">
          <nav className="inline-flex flex-col items-start gap-1 dev-panel px-2 py-1.5 shadow-sm w-fit">
            <PageIndexLink 
              href="#overview" 
              label="Overview" 
              active={activeSection === 'overview'} 
              icon={<InfoIcon />} 
            />

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
                  href="#connect-agent" 
                  label="Connect an agent" 
                  active={activeSection === 'connect-agent'} 
                  icon={<BotIcon />} 
                />
                <PageIndexLink 
                  href="#connect-account" 
                  label="Connect an account" 
                  active={activeSection === 'connect-account'} 
                  icon={<PlugIcon />} 
                />
                <PageIndexLink 
                  href="#resolve-first-task" 
                  label="See Clawvisor in action" 
                  active={activeSection === 'resolve-first-task'} 
                  icon={<TaskIcon />} 
                />
              </>
            )}
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
  const [navigating, setNavigating] = useState(false)

  const handleClick = (e: React.MouseEvent<HTMLAnchorElement>) => {
    e.preventDefault()
    const id = href.startsWith('#') ? href.slice(1) : href
    const target = document.getElementById(id)

    setNavigating(true)
    target?.scrollIntoView({ behavior: 'smooth', block: 'start' })
    window.history.replaceState(null, '', href)

    window.setTimeout(() => setNavigating(false), 400)
  }

  const isHighlighted = active || navigating

  return (
    <a
      href={href}
      onClick={handleClick}
      className={`inline-flex items-center gap-2 w-fit whitespace-nowrap text-sm leading-snug py-1.5 px-2 rounded-md transition-colors ${
        navigating
          ? 'page-index-link--navigating'
          : active
            ? 'text-text-primary bg-surface-2'
            : 'text-text-tertiary hover:text-text-primary hover:bg-surface-2/50'
      }`}
    >
      <div className={`shrink-0 ${isHighlighted ? 'text-text-primary' : 'text-text-tertiary'}`}>
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
    <div className="flex items-center gap-4 rounded-lg border border-border-default bg-surface-1 px-4 py-3.5 animate-pulse">
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
      <h1 className="page-title flex items-center gap-3">
        Welcome, to Clawvisor!
        <span className="shrink-0 text-[1.875rem] leading-none" aria-hidden>👋</span>
      </h1>
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

// ── "Your setup" recap (ready state) ──────────────────────────────────────────

function YourSetupSection({ services, agents }: { services: WelcomeService[]; agents: WelcomeAgent[] }) {
  return (
    <section id="your-setup" className="scroll-mt-24">
      <h2 className="type-section mb-4">Your setup</h2>
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
          <h2 className="type-section">Things to try</h2>
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
    navigator.clipboard.writeText(suggestion.prompt).then(() => {
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
    <span className={`shrink-0 text-sm font-semibold uppercase tracking-wider px-1.5 py-0.5 rounded border ${styles}`}>
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
