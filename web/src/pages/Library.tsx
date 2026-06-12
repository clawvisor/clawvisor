import { useMemo, useState, type ReactNode } from 'react'
import type { FeatureSet } from '../api/client'
import { useAuth } from '../hooks/useAuth'
import LibraryTaskDrawer from '../components/LibraryTaskDrawer'

type TaskCategory =
  | 'Get started'
  | 'Approve & decide'
  | 'Connect & wire'
  | 'Policy & control'
  | 'Review & audit'

interface LibraryTask {
  id: string
  title: string
  learn: string
  steps: string[]
  category: TaskCategory
  to: string
  cta: string
  icon: ReactNode
  prompt: string
  requires?: keyof FeatureSet
}

// Inline SVG icons. Keeping them inline avoids adding an icon-library
// dependency just for this page and matches the rest of the codebase.
const IconStroke = ({ children }: { children: ReactNode }) => (
  <svg className="w-5 h-5" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round" viewBox="0 0 24 24">
    {children}
  </svg>
)
const ICONS = {
  userPlus: <IconStroke><circle cx="9" cy="7" r="4"/><path d="M3 21v-2a4 4 0 0 1 4-4h4a4 4 0 0 1 4 4v2"/><path d="M19 8v6M22 11h-6"/></IconStroke>,
  userCheck: <IconStroke><circle cx="9" cy="7" r="4"/><path d="M3 21v-2a4 4 0 0 1 4-4h4a4 4 0 0 1 4 4v2"/><path d="m16 11 2 2 4-4"/></IconStroke>,
  plug: <IconStroke><path d="M9 22V12M15 22V12M3 12h18M5 12V8a4 4 0 0 1 4-4h6a4 4 0 0 1 4 4v4"/></IconStroke>,
  clipboard: <IconStroke><rect x="6" y="3" width="12" height="18" rx="2"/><path d="m9 12 2 2 4-4"/></IconStroke>,
  inbox: <IconStroke><polyline points="22 12 16 12 14 15 10 15 8 12 2 12"/><path d="M5.45 5.11 2 12v6a2 2 0 0 0 2 2h16a2 2 0 0 0 2-2v-6l-3.45-6.89A2 2 0 0 0 16.76 4H7.24a2 2 0 0 0-1.79 1.11z"/></IconStroke>,
  check: <IconStroke><circle cx="12" cy="12" r="10"/><path d="m9 12 2 2 4-4"/></IconStroke>,
  shield: <IconStroke><path d="M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10z"/><path d="m9 12 2 2 4-4"/></IconStroke>,
  arrows: <IconStroke><path d="M7 17l10-10M7 7h10v10"/></IconStroke>,
  server: <IconStroke><rect x="2" y="3" width="20" height="8" rx="2"/><rect x="2" y="13" width="20" height="8" rx="2"/><path d="M6 7h.01M6 17h.01"/></IconStroke>,
  external: <IconStroke><path d="M18 13v6a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h6"/><polyline points="15 3 21 3 21 9"/><line x1="10" y1="14" x2="21" y2="3"/></IconStroke>,
  lock: <IconStroke><rect x="3" y="11" width="18" height="11" rx="2"/><path d="M7 11V7a5 5 0 0 1 10 0v4"/></IconStroke>,
  stack: <IconStroke><polygon points="12 2 2 7 12 12 22 7 12 2"/><polyline points="2 17 12 22 22 17"/><polyline points="2 12 12 17 22 12"/></IconStroke>,
  bolt: <IconStroke><polygon points="13 2 3 14 12 14 11 22 21 10 12 10 13 2"/></IconStroke>,
  ban: <IconStroke><circle cx="12" cy="12" r="10"/><line x1="4.93" y1="4.93" x2="19.07" y2="19.07"/></IconStroke>,
  shieldLock: <IconStroke><path d="M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10z"/><rect x="9" y="11" width="6" height="5" rx="1"/><path d="M11 11V9a1 1 0 0 1 2 0v2"/></IconStroke>,
  report: <IconStroke><path d="M3 3v18h18"/><path d="m7 14 4-4 4 4 5-5"/></IconStroke>,
  history: <IconStroke><path d="M3 12a9 9 0 1 0 3-6.7"/><polyline points="3 4 3 10 9 10"/><path d="M12 7v5l4 2"/></IconStroke>,
  eye: <IconStroke><path d="M2 12s4-8 10-8 10 8 10 8-4 8-10 8-10-8-10-8z"/><circle cx="12" cy="12" r="3"/></IconStroke>,
  info: <IconStroke><circle cx="12" cy="12" r="10"/><path d="M12 16v-4M12 8h.01"/></IconStroke>,
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
    icon: ICONS.userPlus,
    prompt: "Install Clawvisor and register as my agent. Follow the setup instructions — I'll approve your connection request in the Agents page.",
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
    to: '/dashboard/inbox',
    cta: 'Open inbox',
    icon: ICONS.userCheck,
    prompt: 'Connect to Clawvisor and wait for me to approve your registration. Do not store API keys locally — route tool calls through Clawvisor.',
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
    icon: ICONS.plug,
    prompt: "Connect Gmail and GitHub through Clawvisor so you can help me with email and pull requests. I'll approve the connections in my Accounts page.",
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
    to: '/dashboard/inbox',
    cta: 'Open inbox',
    icon: ICONS.clipboard,
    prompt: "Read my latest Gmail messages and summarize anything urgent. Create a Clawvisor task for the scopes you need — I'll approve it in the Inbox.",
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
    to: '/dashboard/inbox',
    cta: 'Open inbox',
    icon: ICONS.inbox,
    prompt: "Check what's waiting in my Clawvisor inbox and tell me which approvals need my attention first.",
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
    to: '/dashboard/inbox',
    cta: 'Open inbox',
    icon: ICONS.check,
    prompt: "Draft a reply to my most recent important email and submit a Clawvisor task so I can review the Gmail scopes before you send anything.",
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
    to: '/dashboard/inbox',
    cta: 'Open inbox',
    icon: ICONS.shield,
    prompt: 'I need you to run a one-off GitHub action outside your current task. Request approval through Clawvisor before you call the API.',
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
    to: '/dashboard/inbox',
    cta: 'Open inbox',
    icon: ICONS.arrows,
    prompt: "You need a tool that isn't in your approved task yet. Request a scope expansion in Clawvisor and wait for my approval.",
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
    to: '/dashboard/inbox',
    cta: 'Open inbox',
    requires: 'runtime_activity',
    icon: ICONS.server,
    prompt: 'Your outbound network call was blocked by Clawvisor. Request a runtime retry approval and wait for me to allow it in Inbox.',
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
    to: '/dashboard/inbox',
    cta: 'Open inbox',
    icon: ICONS.external,
    prompt: 'When I tap approve in my notification, open Clawvisor and complete the pending approval for this request.',
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
    icon: ICONS.lock,
    prompt: 'Use Clawvisor placeholders for Gmail and GitHub — never ask me to paste raw API keys into this chat.',
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
    icon: ICONS.stack,
    prompt: 'Use my work Gmail alias for client email and my personal GitHub alias for side projects — both are connected in Clawvisor Accounts.',
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
    icon: ICONS.bolt,
    prompt: 'For this standing task, only auto-execute low-risk read actions. Ask me before any write or delete operations.',
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
    icon: ICONS.ban,
    prompt: 'Respect my Clawvisor policy blocks — if a tool or host is restricted, stop and tell me instead of working around it.',
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
    icon: ICONS.shieldLock,
    prompt: 'Route your outbound HTTP calls through the Clawvisor runtime proxy. If a call is blocked, request approval instead of bypassing the proxy.',
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
    icon: ICONS.report,
    prompt: 'After you finish, summarize what Clawvisor logged for this session — which tools ran, what was blocked, and what I approved.',
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
    icon: ICONS.history,
    prompt: 'List my recent Clawvisor tasks and tell me which are still active, pending approval, or completed.',
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
    icon: ICONS.eye,
    prompt: 'Tell me how many active Clawvisor tasks and live runtime sessions you currently have open.',
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
    to: '/dashboard/how-it-works',
    cta: 'Read tour',
    icon: ICONS.info,
    prompt: 'Explain how you use Clawvisor: register as an agent, request tasks for approval, call tools through the gateway, and never hold my credentials directly.',
  },
]

const CATEGORY_ORDER: TaskCategory[] = [
  'Get started',
  'Approve & decide',
  'Connect & wire',
  'Policy & control',
  'Review & audit',
]

type Filter = 'All' | TaskCategory

export default function Library() {
  const { features } = useAuth()
  const [filter, setFilter] = useState<Filter>('All')
  const [openTask, setOpenTask] = useState<LibraryTask | null>(null)

  const tasks = useMemo(
    () => TASKS.filter(t => !t.requires || !!features?.[t.requires]),
    [features],
  )

  const visible = filter === 'All' ? tasks : tasks.filter(t => t.category === filter)

  return (
    <div className="p-4 sm:p-8 space-y-6">
      <div>
        <h1 className="text-2xl font-bold text-text-primary">Library</h1>
        <p className="text-sm text-text-tertiary mt-1">
          Things you can do with Clawvisor — each card opens a short guide and a prompt you can copy to your agent.
        </p>
      </div>

      <FilterTabs
        filter={filter}
        onChange={setFilter}
        counts={{
          All: tasks.length,
          ...Object.fromEntries(
            CATEGORY_ORDER.map(cat => [cat, tasks.filter(t => t.category === cat).length]),
          ) as Record<TaskCategory, number>,
        }}
      />

      {/* min(320px, 100%) keeps the card track from forcing horizontal
          scroll on viewports narrower than 320px. */}
      <ul
        className="grid gap-3"
        style={{ gridTemplateColumns: 'repeat(auto-fill, minmax(min(320px, 100%), 1fr))' }}
      >
        {visible.map(task => (
          <LibraryCard key={task.id} task={task} onOpen={() => setOpenTask(task)} />
        ))}
      </ul>

      {openTask && (
        <LibraryTaskDrawer
          title={openTask.title}
          learn={openTask.learn}
          prompt={openTask.prompt}
          steps={openTask.steps}
          to={openTask.to}
          cta={openTask.cta}
          icon={openTask.icon}
          onClose={() => setOpenTask(null)}
        />
      )}
    </div>
  )
}

function FilterTabs({
  filter,
  onChange,
  counts,
}: {
  filter: Filter
  onChange: (f: Filter) => void
  counts: Record<Filter, number>
}) {
  const options: Filter[] = ['All', ...CATEGORY_ORDER]
  return (
    <div className="flex flex-wrap gap-1.5">
      {options.map(opt => {
        const active = filter === opt
        return (
          <button
            key={opt}
            type="button"
            onClick={() => onChange(opt)}
            className={`rounded-full px-3 py-1 text-xs font-medium transition ${
              active
                ? 'bg-brand text-surface-0'
                : 'bg-surface-1 text-text-secondary hover:bg-surface-2'
            }`}
          >
            {opt.toLowerCase()} ({counts[opt] ?? 0})
          </button>
        )
      })}
    </div>
  )
}

// The card title is the activation button so Enter/Space activate it via
// native semantics. Earlier designs put role="button" + onKeyDown on the
// outer container and called preventDefault on Enter/Space, which
// swallowed inner-button activation.
function LibraryCard({ task, onOpen }: { task: LibraryTask; onOpen: () => void }) {
  const [copied, setCopied] = useState(false)
  function copyPrompt(e: React.MouseEvent) {
    e.stopPropagation()
    navigator.clipboard.writeText(task.prompt).then(
      () => { setCopied(true); setTimeout(() => setCopied(false), 2000) },
      () => { /* clipboard rejection swallowed; users can still read the prompt in the drawer */ },
    )
  }
  return (
    <li className="flex flex-col rounded-md border border-border-default bg-surface-1 overflow-hidden hover:border-brand/30 transition-colors">
      <button
        type="button"
        onClick={onOpen}
        className="flex flex-1 items-start gap-3 px-4 pt-4 pb-3 text-left w-full"
      >
        <div className="shrink-0 text-brand">{task.icon}</div>
        <div className="min-w-0">
          <div className="text-sm font-semibold text-text-primary">{task.title}</div>
          <p className="text-xs text-text-tertiary mt-1 leading-relaxed line-clamp-3">{task.learn}</p>
        </div>
      </button>
      <div className="border-t border-border-subtle px-4 py-2.5 flex items-center justify-between gap-2 text-xs">
        <span className="text-text-tertiary">{task.category}</span>
        <button
          type="button"
          onClick={copyPrompt}
          className="text-brand hover:underline"
        >
          {copied ? 'Copied!' : 'Copy prompt'}
        </button>
      </div>
    </li>
  )
}
