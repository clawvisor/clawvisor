import { useState, type ReactNode } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Link } from 'react-router-dom'
import { api, type TaskSuggestion, type WelcomeData, type WelcomeService, type WelcomeAgent, type WalkthroughExample } from '../api/client'
import { ServiceIcon } from '../components/ServiceIcon'

// GetStarted — the "What is Clawvisor?" page. It's context-aware: users with
// nothing connected are steered through setup, while users who are already
// set up lead with personalized LLM-generated task ideas.
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

  return (
    <div className="p-4 sm:p-8 space-y-10 max-w-5xl">
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
  )
}

function LoadingState() {
  return (
    <div className="space-y-10" aria-busy="true" aria-live="polite">
      <section>
        <div className="flex items-center gap-2 mb-3">
          <LoadingSpinner />
          <h2 className="text-xl font-semibold text-text-primary">Checking your setup&hellip;</h2>
        </div>
        <div className="grid gap-3 sm:grid-cols-2">
          <SkeletonCard label="Services" />
          <SkeletonCard label="Agents" />
        </div>
      </section>
      <section>
        <div className="flex items-center gap-2 mb-3">
          <LoadingSpinner />
          <h2 className="text-xl font-semibold text-text-primary">Generating task ideas&hellip;</h2>
        </div>
        <SuggestionsLoading />
      </section>
    </div>
  )
}

function SkeletonCard({ label }: { label: string }) {
  return (
    <div className="rounded-lg border border-border-subtle bg-surface-1 p-4 animate-pulse">
      <div className="text-xs font-medium uppercase tracking-wider text-text-tertiary mb-3">{label}</div>
      <div className="flex flex-wrap gap-2">
        <div className="h-6 w-24 bg-surface-2 rounded-md" />
        <div className="h-6 w-28 bg-surface-2 rounded-md" />
        <div className="h-6 w-20 bg-surface-2 rounded-md" />
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
    <header className="space-y-3">
      <h1 className="text-3xl sm:text-4xl font-bold text-text-primary tracking-tight">
        Your agents act. You stay in control.
      </h1>
      <p className="text-lg text-text-secondary max-w-3xl leading-relaxed">
        Clawvisor is the gatekeeper between your AI agents and the APIs they act on. Agents never
        hold credentials — they declare <strong className="text-text-primary">tasks</strong>, you
        approve the scope once, and Clawvisor handles credential injection, execution, and audit
        logging for every request.
      </p>
      {isLoading ? (
        <p className="text-sm text-text-tertiary">Loading your setup&hellip;</p>
      ) : ready ? (
        <p className="text-sm text-text-tertiary">
          {services.length} service{services.length === 1 ? '' : 's'} connected · {agents.length}{' '}
          agent{agents.length === 1 ? '' : 's'} registered
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
    <section>
      <h2 className="text-xl font-semibold text-text-primary mb-1">Let's get you set up</h2>
      <p className="text-sm text-text-secondary mb-4">
        Two quick steps and your agents will be able to act on your behalf — safely.
      </p>

      <div className="space-y-3">
        <SetupStepCard num={1} done={hasService} title="Connect a service" loading={isLoading}>
          <p className="text-sm text-text-secondary mb-3">
            Link an API like Gmail, GitHub, Slack, or Linear so your agents have something to act
            on. Credentials stay in Clawvisor's vault — agents never see them.
          </p>
          {hasService ? (
            <ConnectedServicesStrip services={services} />
          ) : (
            <div className="flex flex-wrap gap-2">
              <PopularService id="google.gmail" label="Gmail" />
              <PopularService id="github" label="GitHub" />
              <PopularService id="linear" label="Linear" />
              <PopularService id="slack" label="Slack" />
              <PopularService id="google.calendar" label="Google Calendar" />
              <Link
                to="/dashboard/accounts"
                className="inline-flex items-center gap-1 text-sm font-medium text-brand hover:text-brand-strong px-3 py-1.5 rounded-md border border-brand/40 bg-brand-muted transition-colors"
              >
                Browse all services
                <svg className="w-3.5 h-3.5" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24">
                  <path d="M9 5l7 7-7 7" />
                </svg>
              </Link>
            </div>
          )}
        </SetupStepCard>

        <SetupStepCard num={2} done={hasAgent} title="Connect an agent" loading={isLoading}>
          <p className="text-sm text-text-secondary mb-3">
            Pair an AI agent (Claude Code, Claude Desktop, OpenClaw, or any HTTP client) with
            Clawvisor so it can create tasks on your behalf.
          </p>
          {hasAgent ? (
            <ConnectedAgentsStrip agents={agents} />
          ) : (
            <div className="flex flex-wrap gap-2">
              <PopularAgent tab="claude-code" label="Claude Code" />
              <PopularAgent tab="claude-desktop" label="Claude Desktop" />
              <PopularAgent tab="openclaw" label="OpenClaw" />
              <PopularAgent tab="other" label="Other agents" />
            </div>
          )}
        </SetupStepCard>
      </div>
    </section>
  )
}

function SetupStepCard({
  num,
  done,
  title,
  loading,
  children,
}: {
  num: number
  done: boolean
  title: string
  loading: boolean
  children: ReactNode
}) {
  return (
    <div
      className={`rounded-lg border px-5 py-4 ${
        done ? 'border-success/30 bg-success/5' : 'border-border-subtle bg-surface-1'
      }`}
    >
      <div className="flex items-center gap-3 mb-3">
        <div
          className={`w-7 h-7 rounded-full shrink-0 flex items-center justify-center ${
            done ? 'bg-success text-surface-0' : 'bg-brand-muted text-brand'
          }`}
        >
          {done ? (
            <svg className="w-4 h-4" fill="none" stroke="currentColor" strokeWidth="3" viewBox="0 0 24 24">
              <path d="M5 13l4 4L19 7" />
            </svg>
          ) : (
            <span className="text-sm font-bold">{num}</span>
          )}
        </div>
        <h3 className="font-semibold text-text-primary text-base flex-1">{title}</h3>
        {loading ? (
          <span className="text-xs text-text-tertiary">checking…</span>
        ) : done ? (
          <span className="text-xs font-medium text-success uppercase tracking-wider">Done</span>
        ) : null}
      </div>
      {children}
    </div>
  )
}

function PopularService({ id, label }: { id: string; label: string }) {
  return (
    <Link
      to={`/dashboard/accounts?search=${encodeURIComponent(id)}`}
      className="inline-flex items-center gap-1.5 text-sm text-text-primary bg-surface-2 hover:bg-surface-3 px-3 py-1.5 rounded-md border border-border-subtle transition-colors"
    >
      {label}
    </Link>
  )
}

function PopularAgent({ tab, label }: { tab: string; label: string }) {
  return (
    <Link
      to={`/dashboard/agents?agent=${encodeURIComponent(tab)}`}
      className="inline-flex items-center gap-1.5 text-sm text-text-primary bg-surface-2 hover:bg-surface-3 px-3 py-1.5 rounded-md border border-border-subtle transition-colors"
    >
      {label}
    </Link>
  )
}

function ConnectedServicesStrip({ services }: { services: WelcomeService[] }) {
  return (
    <div className="flex flex-wrap gap-2">
      {services.map(s => (
        <div
          key={`${s.id}:${s.alias ?? ''}`}
          className="flex items-center gap-2 bg-surface-0 border border-border-subtle px-2.5 py-1.5 rounded-md"
        >
          <ServiceIcon iconUrl={s.icon_url} iconSvg={s.icon_svg} serviceId={s.id} size={16} />
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
          <svg className="w-4 h-4 text-text-tertiary" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24">
            <rect x="3" y="11" width="18" height="10" rx="2" ry="2" />
            <circle cx="12" cy="5" r="2" />
            <path d="M12 7v4M8 16h.01M16 16h.01" />
          </svg>
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

// ── "Your setup" recap (ready state) ──────────────────────────────────────────

function YourSetupSection({ services, agents }: { services: WelcomeService[]; agents: WelcomeAgent[] }) {
  return (
    <section>
      <h2 className="text-xl font-semibold text-text-primary mb-3">Your setup</h2>
      <div className="grid gap-4 md:grid-cols-2">
        <div className="rounded-lg border border-border-subtle bg-surface-1 p-4">
          <div className="flex items-baseline justify-between mb-3">
            <h3 className="font-medium text-text-primary">Services</h3>
            <Link to="/dashboard/accounts" className="text-xs text-brand hover:text-brand-strong font-medium">
              Manage →
            </Link>
          </div>
          <ConnectedServicesStrip services={services} />
        </div>
        <div className="rounded-lg border border-border-subtle bg-surface-1 p-4">
          <div className="flex items-baseline justify-between mb-3">
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
    <section>
      <div className="flex items-baseline justify-between mb-4">
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
          className={`grid gap-3 md:grid-cols-2 transition-opacity ${isFetching ? 'opacity-50' : ''}`}
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
    <div className="rounded-lg border border-border-subtle bg-surface-1 p-4 flex flex-col gap-3">
      <div className="flex items-start justify-between gap-3">
        <h3 className="font-semibold text-text-primary leading-snug">{suggestion.title}</h3>
        {suggestion.risk && <RiskBadge level={suggestion.risk} />}
      </div>

      <div className="space-y-1.5">
        {suggestion.agent && (
          <div className="text-xs text-text-tertiary">
            Ask <span className="font-mono text-brand">{suggestion.agent}</span> to:
          </div>
        )}
        <blockquote className="text-sm text-text-primary leading-relaxed whitespace-pre-wrap border-l-2 border-brand/40 pl-3 italic">
          {suggestion.prompt}
        </blockquote>
      </div>

      <div className="flex items-end justify-between gap-3 mt-auto pt-1">
        <div className="flex flex-wrap gap-1.5">
          {suggestion.services.map(id => {
            const svc = serviceById.get(id)
            return (
              <span
                key={id}
                className="inline-flex items-center gap-1 text-xs px-2 py-0.5 rounded bg-surface-2 text-text-tertiary"
                title={svc?.name ?? id}
              >
                {svc && <ServiceIcon iconUrl={svc.icon_url} iconSvg={svc.icon_svg} serviceId={id} size={12} />}
                <span>{svc?.name ?? id}</span>
              </span>
            )
          })}
        </div>

        <button
          onClick={copy}
          className="shrink-0 inline-flex items-center gap-1 text-xs font-medium text-text-secondary hover:text-text-primary transition-colors"
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
    <div className="grid gap-3 md:grid-cols-2">
      {[0, 1, 2, 3].map(i => (
        <div key={i} className="rounded-lg border border-border-subtle bg-surface-1 p-4 animate-pulse">
          <div className="h-4 bg-surface-2 rounded w-1/2 mb-3" />
          <div className="h-3 bg-surface-2 rounded w-full mb-1.5" />
          <div className="h-3 bg-surface-2 rounded w-5/6 mb-1.5" />
          <div className="h-3 bg-surface-2 rounded w-3/4" />
        </div>
      ))}
    </div>
  )
}

function SuggestionsFallback({ status }: { status?: string }) {
  const message =
    status === 'unconfigured'
      ? "Personalized task suggestions need an LLM API key. Add one in Settings to see ideas tailored to what you've connected."
      : status === 'exhausted'
        ? 'The free LLM credit is exhausted. Add your own API key in Settings to keep seeing personalized suggestions.'
        : "Couldn't generate suggestions right now — try refreshing in a minute."
  return (
    <div className="rounded-lg border border-border-subtle bg-surface-1 px-4 py-5 text-sm text-text-secondary">
      {message}
      {(status === 'unconfigured' || status === 'exhausted') && (
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
      label: 'You approve the scope once',
      body: 'Clawvisor shows the scope + an LLM-powered risk assessment; you approve it in one click.',
      detail: 'High-risk or destructive actions can require per-request approval instead.',
    },
    {
      label: 'Clawvisor enforces it on every request',
      body: 'Every gateway call is checked against restrictions, task scope, and approvals. Everything is audited.',
    },
  ]

  return (
    <section>
      <h2 className="text-xl font-semibold text-text-primary mb-1">
        Here&rsquo;s what a task looks like
      </h2>
      <p className="text-sm text-text-secondary mb-4">
        {personalized
          ? `Using your connected ${ex.primary_name} and ${ex.secondary_name} as an example:`
          : `Here\u2019s an example using ${ex.primary_name} and ${ex.secondary_name}:`}
      </p>
      <ol className="space-y-0 relative">
        {steps.map((step, i) => (
          <li key={i} className="flex gap-4 pb-5 last:pb-0 relative">
            {/* Connector line */}
            {i < steps.length - 1 && (
              <span
                aria-hidden
                className="absolute left-[15px] top-8 bottom-0 w-px bg-border-subtle"
              />
            )}
            <div className="w-8 h-8 shrink-0 rounded-full bg-brand-muted text-brand text-xs font-bold flex items-center justify-center relative z-10">
              {i + 1}
            </div>
            <div className="flex-1 pt-1">
              <div className="text-xs font-medium uppercase tracking-wider text-text-tertiary">
                {step.label}
              </div>
              <div className="text-text-primary mt-0.5 leading-relaxed">{step.body}</div>
              {step.detail && (
                <div className="text-sm text-text-secondary mt-1">{step.detail}</div>
              )}
            </div>
          </li>
        ))}
      </ol>
      <div className="mt-4 rounded-md border border-border-subtle bg-surface-1 p-3 text-sm text-text-secondary">
        <strong className="text-text-primary">Three layers of control</strong> check every request,
        in order: <span className="text-text-primary">restrictions</span> (hard blocks you
        configure), <span className="text-text-primary">task scopes</span> (what the agent
        declared and you approved), and{' '}
        <span className="text-text-primary">per-request approval</span> (anything outside the
        scope goes to your queue).
      </div>
    </section>
  )
}
