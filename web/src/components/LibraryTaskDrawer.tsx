import { useState, type ReactNode } from 'react'
import { Link } from 'react-router-dom'
import { useBodyScrollLock, useEscapeKey } from '../hooks/useBodyScrollLock'

interface Props {
  title: string
  learn: string
  prompt: string
  steps: string[]
  to: string
  cta: string
  icon?: ReactNode
  onClose: () => void
}

export default function LibraryTaskDrawer({
  title,
  learn,
  prompt,
  steps,
  to,
  cta,
  icon,
  onClose,
}: Props) {
  useBodyScrollLock()
  useEscapeKey(onClose)
  const [copied, setCopied] = useState(false)

  function copyPrompt() {
    navigator.clipboard.writeText(prompt).then(
      () => { setCopied(true); setTimeout(() => setCopied(false), 2000) },
      () => { /* clipboard rejection — silently swallow, the button remains visible */ },
    )
  }

  return (
    <div className="fixed inset-0 z-50 flex justify-end">
      <button
        type="button"
        className="absolute inset-0 bg-black/40"
        onClick={onClose}
        aria-label="Close"
      />
      <aside
        role="dialog"
        aria-labelledby="library-task-drawer-title"
        className="relative flex h-full w-full max-w-md flex-col border-l border-border-default bg-surface-1 shadow-xl"
      >
        <header className="flex shrink-0 items-start justify-between gap-3 border-b border-border-default px-5 py-4">
          <div className="flex items-start gap-3 min-w-0">
            {icon && <div className="shrink-0 text-brand">{icon}</div>}
            <div className="min-w-0">
              <p className="text-[10px] uppercase tracking-wider text-text-tertiary mb-1">Achievement guide</p>
              <h2 id="library-task-drawer-title" className="text-lg font-semibold text-text-primary">
                {title}
              </h2>
            </div>
          </div>
          <button
            type="button"
            onClick={onClose}
            className="shrink-0 rounded-md border border-border-default px-2.5 py-1.5 text-sm text-text-secondary hover:bg-surface-2 hover:text-text-primary"
            aria-label="Close"
          >
            Close
          </button>
        </header>
        <div className="flex-1 overflow-y-auto px-5 py-5 space-y-5">
          <p className="text-sm text-text-secondary leading-relaxed">{learn}</p>
          <div>
            <p className="text-xs uppercase tracking-wider text-text-tertiary mb-2">Example prompt</p>
            <div className="rounded border border-border-default bg-surface-0 p-3 text-sm text-text-primary leading-relaxed">
              {prompt}
            </div>
            <button
              type="button"
              onClick={copyPrompt}
              className="mt-2 text-xs text-brand hover:underline"
            >
              {copied ? 'Copied!' : 'Copy prompt'}
            </button>
          </div>
          <div>
            <p className="text-xs uppercase tracking-wider text-text-tertiary mb-3">How to do it</p>
            <ol className="pl-5 text-sm text-text-primary space-y-2.5 list-decimal">
              {steps.map((step, i) => (
                <li key={i} className="leading-relaxed">{step}</li>
              ))}
            </ol>
          </div>
          <Link
            to={to}
            onClick={onClose}
            className="inline-flex w-fit items-center gap-2 rounded bg-brand px-4 py-2 text-sm font-medium text-surface-0 hover:bg-brand-strong"
          >
            {cta} →
          </Link>
        </div>
      </aside>
    </div>
  )
}
