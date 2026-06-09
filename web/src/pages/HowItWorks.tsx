import { useCallback, useEffect, useMemo, useState } from 'react'
import { PageHeader } from '../components/layout/PageLayout'

type Step = 1 | 2 | 3 | 4 | 5 | 6

const STEP_ORDER: Step[] = [1, 2, 3, 4, 5, 6]

const HASH_TO_STEP: Record<string, Step> = {
  '#step-1': 1,
  '#step-2': 2,
  '#step-3': 3,
  '#step-4': 4,
  '#step-5': 5,
  '#step-6': 6,
}

const STEP_TO_HASH: Record<Step, string> = {
  1: '#step-1',
  2: '#step-2',
  3: '#step-3',
  4: '#step-4',
  5: '#step-5',
  6: '#step-6',
}

type Section = {
  pill: string
  headline: string
  paragraphs: string[]
  visual?: 'spine' | 'taskCompose' | 'taskApproval' | 'inlinePrompt' | 'credentialSwap' | 'observations'
}

const SECTIONS: Record<Step, Section> = {
  1: {
    pill: 'Proxy',
    headline: 'Your agent talks to your LLM through Clawvisor',
    paragraphs: [
      'Your agent talks to the model through Clawvisor instead of talking to it directly. That position lets Clawvisor hold your API key and see every action the model emits, before the agent runs it.',
      'You authenticate once. From then on, your agent uses a short-lived token that Clawvisor swaps for the real key at the last hop. Your agent never has to handle your real LLM credentials.',
    ],
    visual: 'spine',
  },
  2: {
    pill: 'Tasks',
    headline: 'Tasks are the core unit of work in Clawvisor',
    paragraphs: [
      'A task is a piece of work you have approved your agent to do — something like "build a user dashboard and open a PR." A single task usually covers many tool calls in service of one goal: read existing code, edit files, run tests, push a branch, open a PR.',
      'Inside an approved task, your agent runs without interrupting you. Outside one, it stops and asks. Everything else Clawvisor does — checking actions, injecting credentials, recording activity — is built around this idea.',
    ],
    visual: 'taskCompose',
  },
  3: {
    pill: 'Approve',
    headline: 'When you ask your agent to do work, it creates a task and you approve it',
    paragraphs: [
      'Before starting, your agent describes what it wants to do and which tools it needs. You approve it once. That approval sets the scope for everything that follows.',
      'The approval can come from wherever your agent is already talking to you: a chat, the dashboard, your phone. The point is that it happens before the work begins, not after.',
    ],
    visual: 'taskApproval',
  },
  4: {
    pill: 'Check',
    headline: 'All tools are checked for alignment with that task',
    paragraphs: [
      'When the agent acts, Clawvisor checks each tool call against the approved task. Calls inside the task run on their own. Anything outside it pauses and asks you, right in the chat.',
      'Unrecognized actions fail closed: they are never let through silently.',
    ],
    visual: 'inlinePrompt',
  },
  5: {
    pill: 'Secrets',
    headline: 'Your agent never has to see raw secrets',
    paragraphs: [
      'Your real keys never reach the agent. The agent works with placeholders — stand-ins that mean nothing on their own. Clawvisor swaps in the real credential only when an approved task calls for it.',
      'The model and the harness may see the placeholder. The real secret only goes where it is needed.',
    ],
    visual: 'credentialSwap',
  },
  6: {
    pill: 'Observe',
    headline: 'You get attribution and observability on your agent',
    paragraphs: [
      'Every action your agent takes is recorded and tied to the task that approved it. You can look back at any run and see what the agent did, under whose approval, and what it cost.',
      'Because every action is attributed to a task, you can reconstruct any chain after the fact: not just what happened, but which approval authorized it.',
    ],
    visual: 'observations',
  },
}

function stepFromHash(hash: string): Step {
  return HASH_TO_STEP[hash] ?? 1
}

export default function HowItWorks() {
  const [step, setStep] = useState<Step>(() =>
    typeof window === 'undefined' ? 1 : stepFromHash(window.location.hash),
  )

  useEffect(() => {
    const onHash = () => setStep(stepFromHash(window.location.hash))
    window.addEventListener('hashchange', onHash)
    return () => window.removeEventListener('hashchange', onHash)
  }, [])

  const goToStep = useCallback((next: Step) => {
    const hash = STEP_TO_HASH[next]
    if (window.location.hash !== hash) {
      window.history.replaceState(null, '', hash)
    }
    setStep(next)
  }, [])

  const stepIndex = STEP_ORDER.indexOf(step)
  const prev = stepIndex > 0 ? STEP_ORDER[stepIndex - 1] : null
  const next = stepIndex < STEP_ORDER.length - 1 ? STEP_ORDER[stepIndex + 1] : null

  const section = SECTIONS[step]
  const liveText = useMemo(() => `Step ${step}. ${section.headline}.`, [step, section.headline])

  return (
    <div className="page-shell max-w-3xl mx-auto">
      <PageHeader
        title="How it works"
        meta="A short tour of how Clawvisor sits between your agent and the services it uses."
      />

      <StepPills active={step} onChange={goToStep} />

      <div aria-live="polite" aria-atomic="true" className="sr-only">
        {liveText}
      </div>

      <section className="space-y-4">
        <h2 className="text-xl font-semibold text-text-primary">{section.headline}</h2>
        {section.paragraphs.map((p, i) => (
          <p key={i} className="text-text-secondary leading-relaxed">
            {p}
          </p>
        ))}
      </section>

      {section.visual && (
        <div className="pt-2">
          <Visual kind={section.visual} />
        </div>
      )}

      <PrevNextControls
        prev={prev}
        next={next}
        onChange={goToStep}
        atEndLabel="Back to start"
        atEndTarget={1}
      />
    </div>
  )
}

// ── Step pills ───────────────────────────────────────────────────────────────

function StepPills({ active, onChange }: { active: Step; onChange: (s: Step) => void }) {
  return (
    <nav aria-label="Walkthrough steps" className="flex flex-wrap gap-2">
      {STEP_ORDER.map((s) => {
        const isActive = s === active
        return (
          <button
            key={s}
            type="button"
            aria-current={isActive ? 'step' : undefined}
            onClick={() => onChange(s)}
            className={`px-3 py-1.5 rounded-full text-sm border transition-colors focus:outline-none focus-visible:ring-2 focus-visible:ring-brand ${
              isActive
                ? 'bg-brand text-white border-brand'
                : 'bg-surface-1 text-text-secondary border-border-default hover:border-border-strong hover:text-text-primary'
            }`}
          >
            <span className={`mr-1.5 ${isActive ? 'text-white/80' : 'text-text-tertiary'}`}>{s}.</span>
            {SECTIONS[s].pill}
          </button>
        )
      })}
    </nav>
  )
}

// ── Visuals ──────────────────────────────────────────────────────────────────

function Visual({ kind }: { kind: NonNullable<Section['visual']> }) {
  switch (kind) {
    case 'spine':
      return <SpineVisual />
    case 'taskCompose':
      return <TaskComposeVisual />
    case 'taskApproval':
      return <TaskApprovalVisual />
    case 'inlinePrompt':
      return <InlinePromptVisual />
    case 'credentialSwap':
      return <CredentialSwapVisual />
    case 'observations':
      return <ObservationsVisual />
  }
}

// Round-trip spine: Agent ↔ Clawvisor ↔ LLM with pulsing dots.
function SpineVisual() {
  return (
    <div aria-hidden="true" className="bg-surface-1 border border-border-default rounded-md p-4">
      <style>{`
        @keyframes hiw-pulse-fwd {
          0% { transform: translateX(0); opacity: 0; }
          15% { opacity: 1; }
          85% { opacity: 1; }
          100% { transform: translateX(134px); opacity: 0; }
        }
        @keyframes hiw-pulse-back {
          0% { transform: translateX(0); opacity: 0; }
          15% { opacity: 1; }
          85% { opacity: 1; }
          100% { transform: translateX(-134px); opacity: 0; }
        }
        .hiw-dot-fwd { animation: hiw-pulse-fwd 1.6s linear infinite; }
        .hiw-dot-back { animation: hiw-pulse-back 1.6s linear infinite; }
        @media (prefers-reduced-motion: reduce) {
          .hiw-dot-fwd, .hiw-dot-back { animation: none; opacity: 0; }
        }
      `}</style>
      <svg viewBox="0 0 1000 280" className="block w-full h-auto" role="img">
        <defs>
          <marker id="hiw-arrow" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="6" markerHeight="6" orient="auto-start-reverse">
            <path d="M0,0 L10,5 L0,10 z" fill="currentColor" />
          </marker>
        </defs>

        {/* Inbound leg arrows */}
        <g className="text-text-tertiary">
          <line x1="268" y1="130" x2="402" y2="130" stroke="currentColor" strokeWidth="2" markerEnd="url(#hiw-arrow)" />
          <line x1="402" y1="150" x2="268" y2="150" stroke="currentColor" strokeWidth="2" markerEnd="url(#hiw-arrow)" />
        </g>

        {/* Outbound leg arrows */}
        <g className="text-text-tertiary">
          <line x1="598" y1="130" x2="732" y2="130" stroke="currentColor" strokeWidth="2" markerEnd="url(#hiw-arrow)" />
          <line x1="732" y1="150" x2="598" y2="150" stroke="currentColor" strokeWidth="2" markerEnd="url(#hiw-arrow)" />
        </g>

        {/* Pulsing dots */}
        <g>
          <circle cx={268} cy={130} r={4} className="fill-brand-strong hiw-dot-fwd" />
          <circle cx={268} cy={130} r={4} className="fill-brand-strong hiw-dot-fwd" style={{ animationDelay: '-0.8s' }} />
          <circle cx={598} cy={130} r={4} className="fill-brand-strong hiw-dot-fwd" />
          <circle cx={598} cy={130} r={4} className="fill-brand-strong hiw-dot-fwd" style={{ animationDelay: '-0.8s' }} />
          <circle cx={732} cy={150} r={4} className="fill-brand-strong hiw-dot-back" />
          <circle cx={732} cy={150} r={4} className="fill-brand-strong hiw-dot-back" style={{ animationDelay: '-0.8s' }} />
          <circle cx={402} cy={150} r={4} className="fill-brand-strong hiw-dot-back" />
          <circle cx={402} cy={150} r={4} className="fill-brand-strong hiw-dot-back" style={{ animationDelay: '-0.8s' }} />
        </g>

        <Node x={80} y={90} w={180} h={100} label="Agent" subtitle="your AI agent" tone="neutral" />
        <Node x={410} y={90} w={180} h={100} label="Clawvisor" subtitle="the trust layer" tone="accent" />
        <Node x={740} y={90} w={180} h={100} label="LLM" subtitle="the model" tone="neutral" />
      </svg>
    </div>
  )
}

function Node({
  x,
  y,
  w,
  h,
  label,
  subtitle,
  tone,
}: {
  x: number
  y: number
  w: number
  h: number
  label: string
  subtitle: string
  tone: 'neutral' | 'accent'
}) {
  const fill = tone === 'accent' ? 'fill-brand-muted' : 'fill-surface-0'
  const stroke = tone === 'accent' ? 'stroke-brand' : 'stroke-border-default'
  const textFill = tone === 'accent' ? 'fill-brand-strong' : 'fill-text-primary'

  return (
    <g>
      <rect x={x} y={y} width={w} height={h} rx="10" className={`${fill} ${stroke}`} strokeWidth="1.5" />
      <text x={x + w / 2} y={y + h / 2 - 4} textAnchor="middle" className={textFill} fontSize="16" fontWeight="600">
        {label}
      </text>
      <text x={x + w / 2} y={y + h / 2 + 16} textAnchor="middle" className="fill-text-tertiary" fontSize="11">
        {subtitle}
      </text>
    </g>
  )
}

// Step 2: same tool-call dots, scattered without tasks vs. enclosed
// by braces with a task. No surrounding boxes — visuals stand alone.
function TaskComposeVisual() {
  const scattered: { x: number; y: number }[] = [
    { x: 22, y: 24 }, { x: 62, y: 14 }, { x: 104, y: 30 }, { x: 148, y: 18 },
    { x: 180, y: 46 }, { x: 32, y: 58 }, { x: 80, y: 68 }, { x: 122, y: 54 },
    { x: 164, y: 78 }, { x: 44, y: 98 }, { x: 98, y: 110 }, { x: 168, y: 118 },
  ]
  // 4 columns × 3 rows = 12 dots, centered horizontally and vertically.
  const cols = [70, 90, 110, 130]
  const rows = [40, 70, 100]
  const grouped: { x: number; y: number }[] = []
  for (const y of rows) for (const x of cols) grouped.push({ x, y })

  return (
    <div aria-hidden="true" className="grid sm:grid-cols-2 gap-8 max-w-2xl mx-auto">
      <div className="flex flex-col items-center">
        <div className="text-sm uppercase tracking-wide text-text-tertiary mb-3">
          Without tasks
        </div>
        <svg viewBox="0 0 200 140" className="w-full max-w-[260px] h-auto" role="img">
          {scattered.map((p, i) => (
            <circle key={i} cx={p.x} cy={p.y} r={4} className="fill-brand-strong" />
          ))}
        </svg>
      </div>
      <div className="flex flex-col items-center">
        <div className="text-sm uppercase tracking-wide text-text-tertiary mb-3">
          With tasks
        </div>
        <svg viewBox="0 0 200 140" className="w-full max-w-[260px] h-auto" role="img">
          <text
            x="38"
            y="58"
            textAnchor="middle"
            dominantBaseline="central"
            className="fill-text-tertiary"
            fontSize="110"
            fontWeight="150"
            fontFamily="ui-sans-serif, system-ui, sans-serif"
          >
            {'{'}
          </text>
          <text
            x="162"
            y="58"
            textAnchor="middle"
            dominantBaseline="central"
            className="fill-text-tertiary"
            fontSize="110"
            fontWeight="150"
            fontFamily="ui-sans-serif, system-ui, sans-serif"
          >
            {'}'}
          </text>
          {grouped.map((p, i) => (
            <circle key={i} cx={p.x} cy={p.y} r={4} className="fill-brand-strong" />
          ))}
        </svg>
      </div>
    </div>
  )
}

// Step 3: a condensed version of a real Clawvisor task card in
// pending_approval state, with the same shape as the Tasks-tab card.
function TaskApprovalVisual() {
  const tools: { service: string; action: string; use: string }[] = [
    { service: 'file', action: 'read', use: 'Read existing dashboard pages and routes' },
    { service: 'file', action: 'write', use: 'Create the new component and wire it up' },
    { service: 'bash', action: 'run', use: 'Run the type check, tests, and dev server' },
    { service: 'github', action: 'create_pr', use: 'Open the PR with a description' },
  ]
  return (
    <div className="bg-surface-1 border border-border-default rounded-md border-l-[3px] border-l-warning max-w-xl">
      <div className="px-5 pt-5 pb-4">
        <p className="text-lg font-semibold text-text-primary leading-snug">
          build a user dashboard and open a PR
        </p>
        <div className="flex flex-wrap items-center gap-2 mt-2">
          <span className="inline-flex items-center gap-1.5 text-xs font-mono font-medium px-2 py-0.5 rounded bg-warning/10 text-warning">
            <span className="w-1.5 h-1.5 rounded-full bg-warning" />
            awaiting approval
          </span>
          <span className="text-xs font-mono text-text-secondary">claude-agent</span>
          <span className="text-xs text-text-tertiary">·</span>
          <span className="text-xs text-text-tertiary">session lifetime</span>
        </div>
      </div>

      <div className="border-t border-border-subtle px-5 py-3">
        <div className="text-sm uppercase tracking-wide text-text-tertiary mb-2">
          Tools requested
        </div>
        <div>
          {tools.map((t, i) => (
            <div
              key={`${t.service}.${t.action}`}
              className={`py-2 flex items-start justify-between gap-3 ${
                i > 0 ? 'border-t border-border-subtle' : ''
              }`}
            >
              <div className="min-w-0">
                <div className="font-mono text-[12px] text-text-primary">
                  {t.service}.{t.action}
                </div>
                <div className="text-[12px] text-text-secondary mt-0.5">{t.use}</div>
              </div>
              <span className="text-sm font-mono px-1.5 py-px rounded-full border border-border-subtle text-text-tertiary shrink-0 whitespace-nowrap">
                approve · strict
              </span>
            </div>
          ))}
        </div>
      </div>

      <div className="border-t border-border-subtle px-5 py-3 flex gap-2 justify-end">
        <button
          type="button"
          disabled
          className="px-3 py-1.5 text-sm rounded border border-border-default text-text-secondary cursor-default"
        >
          Deny
        </button>
        <button
          type="button"
          disabled
          className="px-3 py-1.5 text-sm rounded bg-brand text-white cursor-default"
        >
          Approve
        </button>
      </div>
    </div>
  )
}

// Inline approval prompt card. Body text mirrors what the lite proxy
// renders in `internal/runtime/llmproxy/postprocess.go:approvalPrompt`.
// Backticked spans are styled rather than shown literally.
function InlinePromptVisual() {
  const body = `Clawvisor paused this tool call for approval.

Tool: \`bash.run\`
Reason: no matching task scope
Input: gh pr merge --auto 1234

Reply \`yes\` or \`y\` to run this tool call, \`no\` or \`n\` to block it, or \`task\` to instruct the agent to include this in a task definition for approval.`

  const parts = body.split(/(`[^`]+`)/g)

  return (
    <div
      role="region"
      aria-label="Inline approval prompt"
      className="rounded-md border border-warning/40 bg-warning/5 p-4 sm:p-5 space-y-3"
      style={{ borderLeftWidth: '3px' }}
    >
      <p className="sr-only">Inline approval prompt:</p>
      <div className="flex items-center gap-2 text-xs font-medium text-warning uppercase tracking-wide">
        <svg className="w-3.5 h-3.5" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24" aria-hidden="true">
          <circle cx="12" cy="12" r="10" />
          <line x1="12" y1="8" x2="12" y2="12" />
          <line x1="12" y1="16" x2="12.01" y2="16" />
        </svg>
        Tool uses not covered by task scope
      </div>
      <pre className="whitespace-pre-wrap font-mono text-sm text-text-primary leading-relaxed">
        {parts.map((part, i) =>
          part.startsWith('`') && part.endsWith('`') ? (
            <span key={i} className="text-brand-strong font-semibold">
              {part.slice(1, -1)}
            </span>
          ) : (
            <span key={i}>{part}</span>
          ),
        )}
      </pre>
    </div>
  )
}

// Before/after credential swap: what the agent sees vs. what GitHub sees.
// Clicking the masked secret reveals the immortal `hunter2`.
function CredentialSwapVisual() {
  const [revealed, setRevealed] = useState(false)
  return (
    <div className="grid sm:grid-cols-2 gap-3">
      <div className="bg-surface-1 border border-border-default rounded-md p-4">
        <div className="text-sm uppercase tracking-wide text-text-tertiary mb-2">What the agent sees</div>
        <code className="block font-mono text-sm text-text-primary break-all">
          autovault_password_k3sJ2nQ8vT4mB7xW
        </code>
        <p className="text-xs text-text-tertiary mt-3">
          A placeholder. Useless on its own.
        </p>
      </div>
      <div className="bg-surface-1 border border-brand/40 rounded-md p-4">
        <div className="text-sm uppercase tracking-wide text-brand-strong mb-2">What the service sees</div>
        <code className="block font-mono text-sm text-text-primary break-all">
          <button
            type="button"
            onClick={() => setRevealed((v) => !v)}
            aria-label={revealed ? 'Hide secret' : 'Reveal secret'}
            className="font-mono cursor-pointer hover:text-text-secondary focus:outline-none focus-visible:ring-1 focus-visible:ring-brand rounded"
          >
            {revealed ? (
              <span className="text-text-primary">hunter2</span>
            ) : (
              <span className="text-text-tertiary">●●●●●●●●●●●●●●●●</span>
            )}
          </button>
        </code>
        <p className="text-xs text-text-tertiary mt-3">
          Real password, swapped in by Clawvisor at the last hop.
        </p>
      </div>
    </div>
  )
}

// Observations panel with a few example rows tied to the running example.
function ObservationsVisual() {
  const rows = [
    { time: '11:14', tool: 'github.create_pr', scope: 'build a user dashboard and open a PR', cost: '$0.04' },
    { time: '11:09', tool: 'bash.run', scope: 'build a user dashboard and open a PR', cost: '$0.01' },
    { time: '11:03', tool: 'file.write', scope: 'build a user dashboard and open a PR', cost: '$0.03' },
  ]
  return (
    <div className="bg-surface-1 border border-border-default rounded-md overflow-hidden">
      <div className="px-4 py-2 border-b border-border-default text-sm uppercase tracking-wide text-text-tertiary">
        Observations
      </div>
      <table className="w-full text-sm">
        <thead>
          <tr className="text-text-tertiary text-xs">
            <th className="text-left font-normal px-4 py-2">Time</th>
            <th className="text-left font-normal px-4 py-2">Tool</th>
            <th className="text-left font-normal px-4 py-2">Under task</th>
            <th className="text-right font-normal px-4 py-2">Cost</th>
          </tr>
        </thead>
        <tbody className="divide-y divide-border-default">
          {rows.map((row, i) => (
            <tr key={i}>
              <td className="px-4 py-2 text-text-tertiary font-mono text-xs">{row.time}</td>
              <td className="px-4 py-2 font-mono text-text-primary">{row.tool}</td>
              <td className="px-4 py-2 text-text-secondary">{row.scope}</td>
              <td className="px-4 py-2 text-right font-mono text-text-secondary">{row.cost}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

// ── Prev / Next ──────────────────────────────────────────────────────────────

function PrevNextControls({
  prev,
  next,
  onChange,
  atEndLabel,
  atEndTarget,
}: {
  prev: Step | null
  next: Step | null
  onChange: (s: Step) => void
  atEndLabel: string
  atEndTarget: Step
}) {
  return (
    <div className="flex items-center justify-between pt-4 border-t border-border-default">
      <button
        type="button"
        onClick={() => prev !== null && onChange(prev)}
        disabled={prev === null}
        className="px-3 py-1.5 text-sm rounded border border-border-default text-text-secondary hover:text-text-primary hover:border-border-strong disabled:opacity-40 disabled:cursor-not-allowed focus:outline-none focus-visible:ring-2 focus-visible:ring-brand"
      >
        ← Prev
      </button>
      {next === null ? (
        <button
          type="button"
          onClick={() => onChange(atEndTarget)}
          className="text-sm text-text-tertiary hover:text-text-secondary underline-offset-2 hover:underline focus:outline-none focus-visible:ring-2 focus-visible:ring-brand rounded"
        >
          {atEndLabel}
        </button>
      ) : (
        <button
          type="button"
          onClick={() => onChange(next)}
          className="px-3 py-1.5 text-sm rounded bg-brand text-white hover:bg-brand-strong focus:outline-none focus-visible:ring-2 focus-visible:ring-brand"
        >
          Next →
        </button>
      )}
    </div>
  )
}

