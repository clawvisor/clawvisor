import type { WalkthroughExample } from '../api/client'
import CopyablePromptField from './CopyablePromptField'

const DEFAULT_WALKTHROUGH: WalkthroughExample = {
  user_prompt: 'Triage my Gmail and add anything actionable to Linear.',
  agent_task:
    'read Gmail messages received in the last 72 hours, create items in Linear.',
  primary_name: 'Gmail',
  secondary_name: 'Linear',
}

export function walkthroughIntroText(example?: WalkthroughExample): string {
  const ex = example ?? DEFAULT_WALKTHROUGH
  return example
    ? `Using your connected ${ex.primary_name} and ${ex.secondary_name} as an example:`
    : `Here's an example using ${ex.primary_name} and ${ex.secondary_name}:`
}

export default function ClawvisorInActionWalkthrough({
  example,
  maxSteps,
  showIntro = true,
}: {
  example?: WalkthroughExample
  maxSteps?: number
  showIntro?: boolean
}) {
  const ex = example ?? DEFAULT_WALKTHROUGH

  const steps: { label: string; body: string; detail?: string; copyable?: boolean }[] = [
    {
      label: 'You ask',
      body: ex.user_prompt,
      copyable: true,
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

  const visibleSteps = maxSteps ? steps.slice(0, maxSteps) : steps

  return (
    <div className="space-y-5">
      {showIntro && (
        <p className="text-sm text-text-secondary">{walkthroughIntroText(example)}</p>
      )}

      <ol className="relative space-y-0">
        {visibleSteps.map((step, i) => {
          const isLast = i === visibleSteps.length - 1
          return (
            <li key={i} className="flex gap-4">
              <div className="flex flex-col items-center shrink-0">
                <div className="w-7 h-7 rounded-full bg-brand-muted text-brand text-xs font-bold flex items-center justify-center z-10 shrink-0">
                  {i + 1}
                </div>
                {!isLast && (
                  <div
                    aria-hidden
                    className="w-px flex-1 bg-border-subtle mt-1 mb-1 min-h-[24px]"
                  />
                )}
              </div>
              <div className={`flex-1 min-w-0 ${isLast ? 'pb-0' : 'pb-6'}`}>
                <p className="text-2xs font-semibold uppercase tracking-widest text-text-tertiary mb-1 mt-1">
                  {step.label}
                </p>
                {step.copyable ? (
                  <CopyablePromptField value={step.body} />
                ) : (
                  <p className="text-sm font-medium text-text-primary leading-relaxed">
                    {step.body}
                  </p>
                )}
                {step.detail && (
                  <p className="text-sm text-text-secondary mt-1 leading-relaxed">
                    {step.detail}
                  </p>
                )}
              </div>
            </li>
          )
        })}
      </ol>
    </div>
  )
}
