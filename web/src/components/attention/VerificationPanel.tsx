import type { ReactNode } from 'react'
import type { VerificationVerdict } from '../../api/client'
import { isLocalHost } from '../../lib/env'

const VERIFY_COLORS = {
  clean: { bg: 'rgba(34, 197, 94, 0.04)', border: 'rgba(34, 197, 94, 0.15)', headerBorder: 'rgba(34, 197, 94, 0.10)', color: 'rgb(var(--color-success))' },
  warning: { bg: 'rgba(245, 158, 11, 0.05)', border: 'rgba(245, 158, 11, 0.2)', headerBorder: 'rgba(245, 158, 11, 0.12)', color: 'rgb(var(--color-warning))' },
  danger: { bg: 'rgba(239, 68, 68, 0.06)', border: 'rgba(239, 68, 68, 0.25)', headerBorder: 'rgba(239, 68, 68, 0.15)', color: 'rgb(var(--color-danger))' },
}

export function hasVerificationIssue(v: VerificationVerdict): boolean {
  return v.param_scope !== 'ok' || v.reason_coherence !== 'ok'
}

export default function VerificationPanel({ verification: v }: { verification: VerificationVerdict }) {
  const isDanger = v.param_scope === 'violation' || v.reason_coherence === 'incoherent'
  const isClean = v.param_scope === 'ok' && v.reason_coherence === 'ok'
  const colors = isClean ? VERIFY_COLORS.clean : isDanger ? VERIFY_COLORS.danger : VERIFY_COLORS.warning

  let headerIcon: ReactNode
  let headerLabel: string
  if (isClean) {
    headerIcon = <svg className="w-3 h-3" style={{ color: colors.color }} fill="none" stroke="currentColor" strokeWidth="2.5" viewBox="0 0 24 24"><path d="M5 13l4 4L19 7"/></svg>
    headerLabel = 'Verification Passed'
  } else if (isDanger) {
    headerIcon = <svg className="w-3 h-3" style={{ color: colors.color }} fill="none" stroke="currentColor" strokeWidth="2.5" viewBox="0 0 24 24"><path d="M12 9v2m0 4h.01m-6.938 4h13.856c1.54 0 2.502-1.667 1.732-3L13.732 4c-.77-1.333-2.694-1.333-3.464 0L3.34 16c-.77 1.333.192 3 1.732 3z"/></svg>
    headerLabel = 'Verification Warning'
  } else {
    headerIcon = <svg className="w-3 h-3" style={{ color: colors.color }} fill="none" stroke="currentColor" strokeWidth="2.5" viewBox="0 0 24 24"><path d="M12 9v2m0 4h.01m-6.938 4h13.856c1.54 0 2.502-1.667 1.732-3L13.732 4c-.77-1.333-2.694-1.333-3.464 0L3.34 16c-.77 1.333.192 3 1.732 3z"/></svg>
    headerLabel = 'Verification Notice'
  }

  return (
    <div className="px-5 pb-3">
      <div className="rounded overflow-hidden" style={{ background: colors.bg, border: `1px solid ${colors.border}` }}>
        <div className="px-3 py-1.5 flex items-center gap-1.5" style={{ borderBottom: `1px solid ${colors.headerBorder}` }}>
          {headerIcon}
          <span className="text-sm font-medium uppercase tracking-wider" style={{ color: colors.color }}>
            {headerLabel}
          </span>
        </div>
        <div className="px-3 py-2.5 space-y-1.5">
          <div className="flex items-center gap-3">
            <span className={`text-sm font-mono font-medium ${
              v.param_scope === 'ok' ? 'text-success' : v.param_scope === 'violation' ? 'text-danger' : 'text-text-tertiary'
            }`}>params: {v.param_scope}</span>
            <span className={`text-sm font-mono font-medium ${
              v.reason_coherence === 'ok' ? 'text-success'
              : v.reason_coherence === 'incoherent' ? 'text-danger'
              : v.reason_coherence === 'insufficient' ? 'text-warning'
              : 'text-text-tertiary'
            }`}>reason: {v.reason_coherence}</span>
          </div>
          {v.explanation && <p className="text-xs text-text-secondary">{v.explanation}</p>}
          <div className="text-sm font-mono text-text-tertiary">{isLocalHost ? `${v.model} · ` : ''}{v.latency_ms}ms</div>
        </div>
      </div>
    </div>
  )
}
