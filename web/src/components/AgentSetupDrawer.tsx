import { useEffect, type ReactNode } from 'react'
import { AGENT_META, type AgentTab } from '../constants/agentTabs'

type AgentSetupDrawerProps = {
  agent: AgentTab
  onClose: () => void
  children: ReactNode
}

export default function AgentSetupDrawer({ agent, onClose, children }: AgentSetupDrawerProps) {
  useEffect(() => {
    const prev = document.body.style.overflow
    document.body.style.overflow = 'hidden'
    const onKeyDown = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose()
    }
    window.addEventListener('keydown', onKeyDown)
    return () => {
      document.body.style.overflow = prev
      window.removeEventListener('keydown', onKeyDown)
    }
  }, [onClose])

  const meta = AGENT_META[agent]

  return (
    <div className="fixed inset-0 z-50 flex justify-end">
      <button
        type="button"
        className="absolute inset-0 bg-black/40"
        onClick={onClose}
        aria-label="Close setup guide"
      />
      <aside
        role="dialog"
        aria-labelledby="agent-setup-drawer-title"
        className="dev-setup-drawer relative flex h-full w-full max-w-xl flex-col border-l border-border-default bg-surface-1 shadow-xl"
      >
        <header className="flex shrink-0 items-start justify-between gap-3 border-b border-border-default px-5 py-4">
          <div className="min-w-0">
            <p className="ds-overline normal-case tracking-normal mb-1">Connect an agent</p>
            <h2 id="agent-setup-drawer-title" className="text-lg font-semibold text-text-primary">
              Connect your {meta.label} agent
            </h2>
            <p className="text-sm text-text-tertiary mt-1">{meta.tagline}</p>
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
        <div className="flex-1 overflow-y-auto px-5 py-4 custom-scrollbar">
          {children}
        </div>
      </aside>
    </div>
  )
}
