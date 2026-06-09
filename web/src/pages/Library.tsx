import { useMemo, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Link } from 'react-router-dom'
import type { TablerIcon } from '@tabler/icons-react'
import {
  IconArrowsDiagonal,
  IconBan,
  IconBolt,
  IconCircleCheck,
  IconClipboardCheck,
  IconExternalLink,
  IconEye,
  IconHistory,
  IconInbox,
  IconInfoCircle,
  IconLock,
  IconPlugConnected,
  IconReportAnalytics,
  IconServer2,
  IconShieldCheck,
  IconShieldLock,
  IconStack2,
  IconUserCheck,
  IconUserPlus,
} from '@tabler/icons-react'
import { api, type FeatureSet } from '../api/client'
import { useAuth } from '../hooks/useAuth'
import { useAttentionItems } from '../hooks/useAttentionItems'
import { useSetupProgress } from '../hooks/useSetupProgress'
import LibraryTaskDrawer from '../components/LibraryTaskDrawer'
import './library.css'

type TaskCategory = 'Get started' | 'Approve & decide' | 'Connect & wire' | 'Policy & control' | 'Review & audit'

type LibraryTask = {
  id: string
  title: string
  learn: string
  steps: string[]
  category: TaskCategory
  to: string
  cta: string
  Icon: TablerIcon
  /** Agent prompt shown on the card with copy + hover tooltip */
  prompt: string
  /** Hide when this feature flag is required but off */
  requires?: keyof FeatureSet
}

const TASKS: LibraryTask[] = [
  {
    id: 'connect-agent',
    title: 'Connect an AI agent',
    learn: 'Register an agent so Clawvisor can sit between it and the tools it calls.',
    steps: [
      'Open Agents and create or pair a new agent.',
      'Copy the connection token or config into your agent harness.',
      'Wait for the agent to knock — approve the connection in Inbox.',
    ],
    category: 'Get started',
    to: '/dashboard/agents',
    cta: 'Open agents',
    Icon: IconUserPlus,
    prompt:
      'Install Clawvisor and register as my agent. Follow the setup instructions — I\'ll approve your connection request in the Agents page.',
  },
  {
    id: 'approve-connection',
    title: 'Approve an agent connection',
    learn: 'Let a new agent register with your Clawvisor instance without handing it credentials.',
    steps: [
      'When an agent connects, a connection request appears in Inbox.',
      'Review the agent name, IP, and description.',
      'Approve to register it, or deny to block the knock.',
    ],
    category: 'Get started',
    to: '/dashboard/activity',
    cta: 'Open activity',
    Icon: IconUserCheck,
    prompt:
      'Connect to Clawvisor and wait for me to approve your registration. Do not store API keys locally — route tool calls through Clawvisor.',
  },
  {
    id: 'connect-account',
    title: 'Connect a service account',
    learn: 'Give agents managed access to Gmail, GitHub, Slack, and more — secrets stay in Clawvisor.',
    steps: [
      'Open Accounts and pick a service to activate.',
      'Complete OAuth or paste credentials into the vault.',
      'Agents receive placeholders; Clawvisor swaps real secrets at execution time.',
    ],
    category: 'Get started',
    to: '/dashboard/accounts',
    cta: 'Open accounts',
    Icon: IconPlugConnected,
    prompt:
      'Connect Gmail and GitHub through Clawvisor so you can help me with email and pull requests. I\'ll approve the connections in my Accounts page.',
  },
  {
    id: 'first-task',
    title: 'Approve your first task',
    learn: 'Tasks are scoped units of agent work. You approve once; Clawvisor enforces the boundary.',
    steps: [
      'Ask your agent to do something that needs tools (e.g. read email, open a PR).',
      'The agent creates a task describing purpose and required scopes.',
      'Open Inbox, review the task, and approve or deny.',
    ],
    category: 'Get started',
    to: '/dashboard/activity',
    cta: 'Open activity',
    Icon: IconClipboardCheck,
    prompt:
      'Read my latest Gmail messages and summarize anything urgent. Create a Clawvisor task for the scopes you need — I\'ll approve it in Activity.',
  },
  {
    id: 'triage-inbox',
    title: 'Triage your attention queue',
    learn: 'Inbox is the single place for every decision: tasks, approvals, connections, and runtime items.',
    steps: [
      'Open Inbox when the badge shows pending items.',
      'Work top to bottom — each card is one decision.',
      'Approve, deny, or follow the inline-chat notice when an item is chat-bound.',
    ],
    category: 'Approve & decide',
    to: '/dashboard/activity',
    cta: 'Open activity',
    Icon: IconInbox,
    prompt:
      'Check what\'s waiting in my Clawvisor inbox and tell me which approvals need my attention first.',
  },
  {
    id: 'approve-task',
    title: 'Approve or deny a pending task',
    learn: 'You set the scope before work begins. Denied tasks stop the agent cold.',
    steps: [
      'Find the task in Inbox (status: pending approval).',
      'Read purpose, planned tools, and any risk or verification flags.',
      'Approve to start the session, or deny to block it.',
    ],
    category: 'Approve & decide',
    to: '/dashboard/activity',
    cta: 'Open activity',
    Icon: IconCircleCheck,
    prompt:
      'Draft a reply to my most recent important email and submit a Clawvisor task so I can review the Gmail scopes before you send anything.',
  },
  {
    id: 'standalone-approval',
    title: 'Approve a one-off tool call',
    learn: 'Some actions happen outside an active task. Clawvisor pauses and asks before they run.',
    steps: [
      'Standalone approvals appear in Inbox with service and action labels.',
      'Check verification results if the call was risk-scored.',
      'Allow once to let it through, or deny to block.',
    ],
    category: 'Approve & decide',
    to: '/dashboard/activity',
    cta: 'Open activity',
    Icon: IconShieldCheck,
    prompt:
      'I need you to run a one-off GitHub action outside your current task. Request approval through Clawvisor before you call the API.',
  },
  {
    id: 'scope-expansion',
    title: 'Approve scope expansion',
    learn: 'When an agent needs a tool outside the approved task, it must ask you to widen scope.',
    steps: [
      'Scope expansion requests show in Inbox on an active task.',
      'Review which service/action the agent wants to add.',
      'Approve to extend the task, or deny to keep the original boundary.',
    ],
    category: 'Approve & decide',
    to: '/dashboard/activity',
    cta: 'Open activity',
    Icon: IconArrowsDiagonal,
    prompt:
      'You need a tool that isn\'t in your approved task yet. Request a scope expansion in Clawvisor and wait for my approval.',
  },
  {
    id: 'runtime-approval',
    title: 'Handle a runtime retry approval',
    learn: 'The local proxy can pause outbound calls and ask before retrying blocked network access.',
    steps: [
      'Runtime approvals appear in Inbox with host/path details.',
      'Decide whether the retry is in scope for the active session.',
      'Allow once or deny — the proxy enforces your choice immediately.',
    ],
    category: 'Approve & decide',
    to: '/dashboard/activity',
    cta: 'Open activity',
    requires: 'runtime_activity',
    Icon: IconServer2,
    prompt:
      'Your outbound network call was blocked by Clawvisor. Request a runtime retry approval and wait for me to allow it in Activity.',
  },
  {
    id: 'deep-link',
    title: 'Approve from a notification link',
    learn: 'Telegram and other channels can deep-link straight into Clawvisor with the right context.',
    steps: [
      'Tap approve/deny in your notification — it opens Clawvisor with action params.',
      'You land in Inbox or Tasks with the item ready to resolve.',
      'If buttons are disabled, reply in the agent chat (inline-chat-bound).',
    ],
    category: 'Approve & decide',
    to: '/dashboard/activity',
    cta: 'Open activity',
    Icon: IconExternalLink,
    prompt:
      'When I tap approve in my notification, open Clawvisor and complete the pending approval for this request.',
  },
  {
    id: 'vault-secrets',
    title: 'Store credentials without exposing them',
    learn: 'Agents work with placeholders. Clawvisor injects real secrets only for approved calls.',
    steps: [
      'Connect a service under Accounts — credentials land in the vault.',
      'Agents reference the service alias in tool calls, never the raw key.',
      'Revoke or rotate credentials in Accounts without reconfiguring the agent.',
    ],
    category: 'Connect & wire',
    to: '/dashboard/accounts',
    cta: 'Open accounts',
    Icon: IconLock,
    prompt:
      'Use Clawvisor placeholders for Gmail and GitHub — never ask me to paste raw API keys into this chat.',
  },
  {
    id: 'service-aliases',
    title: 'Use multiple accounts for one service',
    learn: 'Aliases let one agent pick which connected account to use (work Gmail vs personal).',
    steps: [
      'Activate the same service type twice under different names.',
      'Agents request scopes like gmail:work or github:personal.',
      'Clawvisor routes each call to the right vault entry.',
    ],
    category: 'Connect & wire',
    to: '/dashboard/accounts',
    cta: 'Open accounts',
    Icon: IconStack2,
    prompt:
      'Use my work Gmail alias for client email and my personal GitHub alias for side projects — both are connected in Clawvisor Accounts.',
  },
  {
    id: 'auto-execute',
    title: 'Auto-approve safe tool calls',
    learn: 'Per scope, you can let low-risk actions run inside a task without stopping you each time.',
    steps: [
      'Open Policy or edit scopes on an active task card.',
      'Toggle auto-execute for trusted service/action pairs.',
      'Keep strict verification on sensitive operations.',
    ],
    category: 'Policy & control',
    to: '/dashboard/policy',
    cta: 'Open policy',
    Icon: IconBolt,
    prompt:
      'For this standing task, only auto-execute low-risk read actions. Ask me before any write or delete operations.',
  },
  {
    id: 'restrictions',
    title: 'Block tools and hosts',
    learn: 'Restrictions fail closed — blocked actions never slip through silently.',
    steps: [
      'Open Policy and add service or host restrictions.',
      'Choose block vs observe-only per rule.',
      'Blocked attempts show in Activity with a clear outcome.',
    ],
    category: 'Policy & control',
    to: '/dashboard/policy',
    cta: 'Open policy',
    Icon: IconBan,
    prompt:
      'Respect my Clawvisor policy blocks — if a tool or host is restricted, stop and tell me instead of working around it.',
  },
  {
    id: 'runtime-proxy',
    title: 'Enforce network policy locally',
    learn: 'The runtime proxy observes or blocks agent egress and can require inline approval.',
    steps: [
      'Check runtime status on Home or under Policy.',
      'Enable the proxy and set observation vs enforce defaults.',
      'Runtime approvals for retries land in Inbox.',
    ],
    category: 'Policy & control',
    to: '/dashboard/policy',
    cta: 'Open policy',
    requires: 'runtime_activity',
    Icon: IconShieldLock,
    prompt:
      'Route your outbound HTTP calls through the Clawvisor runtime proxy. If a call is blocked, request approval instead of bypassing the proxy.',
  },
  {
    id: 'audit-trail',
    title: 'See what agents actually did',
    learn: 'Every tool call is attributed to a task, agent, and outcome — executed, blocked, or pending.',
    steps: [
      'Open Activity for the full audit log.',
      'Filter by agent, service, or outcome.',
      'Drill into a task from Task history for scope and action detail.',
    ],
    category: 'Review & audit',
    to: '/dashboard/activity',
    cta: 'Open activity',
    Icon: IconReportAnalytics,
    prompt:
      'After you finish, summarize what Clawvisor logged for this session — which tools ran, what was blocked, and what I approved.',
  },
  {
    id: 'task-history',
    title: 'Review past and active tasks',
    learn: 'Task history shows every session — pending, active, completed, denied — with full context.',
    steps: [
      'Open Task history for a paginated list of all tasks.',
      'Expand a task card to see scopes, audit entries, and costs.',
      'Actionable items still belong in Inbox, not here.',
    ],
    category: 'Review & audit',
    to: '/dashboard/tasks',
    cta: 'Open task history',
    Icon: IconHistory,
    prompt:
      'List my recent Clawvisor tasks and tell me which are still active, pending approval, or completed.',
  },
  {
    id: 'live-sessions',
    title: 'Monitor active agent sessions',
    learn: 'See which agents are mid-task and how many runtime sessions are open right now.',
    steps: [
      'Check Home for active tasks and runtime session count.',
      'Open Agents for per-agent session detail.',
      'Revoke a task from its card if you need to stop work immediately.',
    ],
    category: 'Review & audit',
    to: '/dashboard/home',
    cta: 'Open home',
    requires: 'agent_live_sessions',
    Icon: IconEye,
    prompt:
      'Tell me how many active Clawvisor tasks and live runtime sessions you currently have open.',
  },
  {
    id: 'how-proxy-works',
    title: 'Understand the control plane',
    learn: 'Clawvisor sits between your agent and its tools — approving tasks, swapping secrets, logging everything.',
    steps: [
      'Read the six-step tour: proxy, tasks, approvals, checks, secrets, observability.',
      'Use this mental model when configuring policy and reading Inbox cards.',
    ],
    category: 'Get started',
    to: '/dashboard/library/how-it-works',
    cta: 'Read tour',
    Icon: IconInfoCircle,
    prompt:
      'Explain how you use Clawvisor: register as an agent, request tasks for approval, call tools through the gateway, and never hold my credentials directly.',
  },
]

const CATEGORY_ORDER: TaskCategory[] = [
  'Get started',
  'Approve & decide',
  'Connect & wire',
  'Policy & control',
  'Review & audit',
]

const CATEGORY_CLASS: Record<TaskCategory, string> = {
  'Get started': 'lib-cat-start',
  'Approve & decide': 'lib-cat-approve',
  'Connect & wire': 'lib-cat-connect',
  'Policy & control': 'lib-cat-policy',
  'Review & audit': 'lib-cat-review',
}

function TaskIcon({ Icon }: { Icon: TablerIcon }) {
  return (
    <div className="lib-icon shrink-0">
      <Icon size={18} stroke={1.75} aria-hidden />
    </div>
  )
}

type ExplorationCapability = {
  id: string
  label: string
  explored: boolean
}

function categoryTabLabel(cat: 'All' | TaskCategory, taskList: LibraryTask[]) {
  const count = cat === 'All'
    ? taskList.length
    : taskList.filter(t => t.category === cat).length
  const label = cat === 'All' ? 'all' : cat.toLowerCase()
  return `${label} (${count})`
}

function explorationRatingDetail(score: number) {
  if (score >= 1) return 'every core capability explored'
  if (score >= 0.8) return 'one area left to try'
  if (score >= 0.6) return 'solid coverage across the control plane'
  if (score >= 0.4) return 'discovering what clawvisor can do'
  if (score >= 0.2) return 'early steps — keep going'
  return 'start with the recommended task below'
}

function ExplorationRating({
  loading,
  capabilities,
  recommendedTask,
}: {
  loading: boolean
  capabilities: ExplorationCapability[]
  recommendedTask?: LibraryTask | null
}) {
  const exploredCount = capabilities.filter(c => c.explored).length
  const score = capabilities.length === 0 ? 0 : exploredCount / capabilities.length
  const ratingDetail = explorationRatingDetail(score)

  return (
    <section className="dev-panel px-5 py-6 space-y-4">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="min-w-0">
          <h2 className="page-section-title mb-0 normal-case">Achievements Unlocked</h2>
          <p className="text-sm text-text-tertiary mt-1">
            {loading
              ? 'measuring your coverage…'
              : `${exploredCount} of ${capabilities.length} areas tried · ${ratingDetail}`}
          </p>
        </div>
        <div className="text-right shrink-0">
          <div className="font-sans text-2xl font-semibold text-text-primary tabular-nums leading-none">
            {loading ? '—' : `${Math.round(score * 100)}%`}
          </div>
        </div>
      </div>
      <div className="h-1.5 bg-surface-2 rounded-full overflow-hidden">
        <div
          className="h-full bg-brand transition-all duration-300"
          style={{ width: loading ? '0%' : `${Math.round(score * 100)}%` }}
        />
      </div>
      {!loading && recommendedTask && (
        <Link
          to={recommendedTask.to}
          className="flex items-center gap-3 no-underline text-inherit hover:opacity-90 transition-opacity"
        >
          <TaskIcon Icon={recommendedTask.Icon} />
          <div className="min-w-0 flex-1">
            <div className="dev-pick-title">{recommendedTask.title}</div>
            <p className="dev-pick-desc line-clamp-1 mt-0.5">{recommendedTask.learn}</p>
          </div>
          <span className="dev-text-link shrink-0 hidden sm:inline">{recommendedTask.cta} →</span>
        </Link>
      )}
    </section>
  )
}

export default function Library() {
  const { features, currentOrg } = useAuth()
  const orgId = currentOrg?.id
  const {
    steps,
    incompleteRequired,
    isComplete,
    isLoading: setupLoading,
  } = useSetupProgress()
  const { attentionCount } = useAttentionItems()
  const [category, setCategory] = useState<'All' | TaskCategory>('All')

  const { data: activityBuckets, isLoading: activityLoading } = useQuery({
    queryKey: ['audit-buckets', 30],
    queryFn: () => api.audit.activityBuckets({ days: 30, bucket_minutes: 1440 }),
    staleTime: 60_000,
  })

  const { data: hasPolicyRules, isLoading: restrictionsLoading } = useQuery({
    queryKey: ['library-policy-explored', orgId ?? 'personal'],
    queryFn: async () => {
      const list = orgId
        ? await api.orgs.restrictions.list(orgId)
        : await api.restrictions.list()
      return list.length > 0
    },
    staleTime: 60_000,
  })

  const activityLast30d = useMemo(
    () => (activityBuckets?.buckets ?? []).reduce((sum, b) => sum + b.count, 0),
    [activityBuckets],
  )

  const stepComplete = (id: string) => steps.find(s => s.id === id)?.complete ?? false

  const explorationCapabilities = useMemo<ExplorationCapability[]>(() => [
    { id: 'agents', label: 'agents', explored: stepComplete('agent') },
    { id: 'accounts', label: 'accounts', explored: stepComplete('account') },
    { id: 'approvals', label: 'approvals', explored: stepComplete('approval') },
    { id: 'activity', label: 'activity log', explored: activityLast30d > 0 },
    { id: 'policy', label: 'policy', explored: !!hasPolicyRules },
  ], [activityLast30d, hasPolicyRules, steps])

  const explorationLoading = setupLoading || activityLoading || restrictionsLoading

  const tasks = useMemo(() => {
    return TASKS.filter(t => {
      if (!t.requires) return true
      return !!features?.[t.requires]
    })
  }, [features])

  const categories = ['All', ...CATEGORY_ORDER.filter(c => tasks.some(t => t.category === c))]

  const nextTaskId = useMemo(() => {
    if (isComplete) return null
    const step = incompleteRequired[0]?.id
    if (step === 'agent') return 'connect-agent'
    if (step === 'account') return 'connect-account'
    if (step === 'approval') return 'first-task'
    if (step === 'security') return 'connect-agent'
    return 'connect-agent'
  }, [incompleteRequired, isComplete])

  const recommendedTask = useMemo(() => {
    if (nextTaskId) {
      return tasks.find(t => t.id === nextTaskId) ?? tasks[0] ?? null
    }
    if (attentionCount > 0) {
      return tasks.find(t => t.id === 'triage-inbox') ?? null
    }
    return tasks.find(t => t.id === 'how-proxy-works') ?? tasks[0] ?? null
  }, [attentionCount, nextTaskId, tasks])

  const visible = useMemo(() => {
    const filtered = tasks.filter(t => category === 'All' || t.category === category)
    if (!recommendedTask || setupLoading) return filtered
    if (!filtered.some(t => t.id === recommendedTask.id)) return filtered
    return [recommendedTask, ...filtered.filter(t => t.id !== recommendedTask.id)]
  }, [tasks, category, recommendedTask, setupLoading])

  return (
    <div className="lib-page">
      <header className="lib-hero">
        <h1 className="page-title">Achievements</h1>
        <p className="page-desc">
          Task-focused guides for operating agents through Clawvisor — connect agents, approve work,
          wire services, set policy, and audit what happened. Pick a task below to learn how and where to do it.
        </p>
      </header>

      <ExplorationRating
        loading={explorationLoading}
        capabilities={explorationCapabilities}
        recommendedTask={recommendedTask}
      />

      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="flex flex-wrap gap-1.5">
          {categories.map(cat => (
            <button
              key={cat}
              type="button"
              className={
                cat === category
                  ? 'dev-btn-ghost text-text-primary border-brand/25 bg-[var(--color-info-bg)]'
                  : 'dev-btn-ghost text-text-tertiary'
              }
              onClick={() => setCategory(cat as typeof category)}
            >
              {categoryTabLabel(cat as 'All' | TaskCategory, tasks)}
            </button>
          ))}
        </div>
        <span className="ds-data text-text-tertiary shrink-0 tabular-nums">
          {visible.length} shown
        </span>
      </div>

      <section className="lib-grid">
        {visible.map(task => (
          <TaskCard
            key={task.id}
            task={task}
            recommended={!setupLoading && task.id === recommendedTask?.id}
          />
        ))}
      </section>

      {visible.length === 0 && (
        <div className="lib-error">No tasks in this category.</div>
      )}
    </div>
  )
}

function LibCardCopyPromptButton({ prompt }: { prompt: string }) {
  const [copied, setCopied] = useState(false)

  function copy(e: React.MouseEvent) {
    e.preventDefault()
    e.stopPropagation()
    navigator.clipboard.writeText(prompt).then(() => {
      setCopied(true)
      setTimeout(() => setCopied(false), 1500)
    })
  }

  return (
    <button
      type="button"
      onClick={copy}
      title={prompt}
      aria-label={copied ? 'Copied prompt' : 'Copy prompt'}
      className="dev-btn-ghost"
    >
      {copied ? 'Copied' : 'Copy prompt'}
    </button>
  )
}

function TaskCard({
  task,
  recommended,
}: {
  task: LibraryTask
  recommended?: boolean
}) {
  const [drawerOpen, setDrawerOpen] = useState(false)
  const className = `lib-card ${CATEGORY_CLASS[task.category]}${recommended ? ' lib-card-highlight' : ''}`

  function openDrawer() {
    setDrawerOpen(true)
  }

  return (
    <>
      <div
        role="button"
        tabIndex={0}
        className={className}
        onClick={openDrawer}
        onKeyDown={e => {
          if (e.key === 'Enter' || e.key === ' ') {
            e.preventDefault()
            openDrawer()
          }
        }}
      >
        <div className="lib-card-content">
          <div className="lib-card-head">
            <TaskIcon Icon={task.Icon} />
            <div className="lib-card-title min-w-0">{task.title}</div>
          </div>
          <p className="lib-description">{task.learn}</p>
          <div className="lib-card-actions" onClick={e => e.stopPropagation()}>
            <button type="button" className="dev-btn-ghost" onClick={openDrawer}>
              Learn more
            </button>
            <LibCardCopyPromptButton prompt={task.prompt} />
          </div>
        </div>
      </div>
      {drawerOpen && (
        <LibraryTaskDrawer
          title={task.title}
          learn={task.learn}
          prompt={task.prompt}
          steps={task.steps}
          to={task.to}
          cta={task.cta}
          icon={<TaskIcon Icon={task.Icon} />}
          onClose={() => setDrawerOpen(false)}
        />
      )}
    </>
  )
}
