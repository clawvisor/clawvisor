import { useState } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { api, APIError, type QueueItem } from '../../api/client'
import { serviceName, actionName } from '../../lib/services'
import CountdownTimer from '../CountdownTimer'
import VerificationIcon from '../VerificationIcon'
import VerificationPanel, { hasVerificationIssue } from './VerificationPanel'
import InlineChatBoundNotice from './InlineChatBoundNotice'
import { invalidateAttention } from './invalidate'

export default function ApprovalAttentionCard({ item }: { item: QueueItem }) {
  const qc = useQueryClient()
  const [resolved, setResolved] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [inlineChat, setInlineChat] = useState(false)
  const [verifyOpen, setVerifyOpen] = useState(false)
  const a = item.approval!

  const approveMut = useMutation({
    mutationFn: () => api.approvals.approve(a.request_id, 'allow_once', a.task_id),
    onSuccess: (res) => {
      setResolved(res.status === 'executed' ? 'Approved & executed' : `Outcome: ${res.status}`)
      invalidateAttention(qc)
    },
    onError: (err: Error) => {
      if (err instanceof APIError && err.code === 'INLINE_CHAT_BOUND') {
        setInlineChat(true)
      } else {
        setError(err.message)
      }
    },
  })

  const denyMut = useMutation({
    mutationFn: () => api.approvals.deny(a.request_id, a.task_id),
    onSuccess: () => {
      setResolved('Denied')
      invalidateAttention(qc)
    },
    onError: (err: Error) => {
      if (err instanceof APIError && err.code === 'INLINE_CHAT_BOUND') {
        setInlineChat(true)
      } else {
        setError(err.message)
      }
    },
  })

  const isPending = approveMut.isPending || denyMut.isPending
  const params = a.params ?? {}
  const paramEntries = Object.entries(params)
  const hasIssue = a.verification ? hasVerificationIssue(a.verification) : false
  // Auto-expand when there's a problem
  const showPanel = a.verification && (hasIssue || verifyOpen)

  if (resolved) {
    return (
      <div className="border border-border-default rounded-md bg-surface-1 p-5">
        <div className="p-3 bg-surface-2 rounded text-sm text-text-tertiary">{resolved}</div>
      </div>
    )
  }

  return (
    <div className="bg-surface-1 border border-border-default rounded-md border-l-[3px] border-l-warning overflow-hidden">
      {/* Header */}
      <div className="px-5 pt-5 pb-4">
        <span className="font-mono text-lg font-semibold text-text-primary">{serviceName(a.service)} · {actionName(a.action)}</span>
        {a.reason && (
          <p className="text-sm text-text-secondary mt-1.5">{a.reason}</p>
        )}
        <div className="flex items-center gap-2 mt-2">
          <span className="inline-flex items-center gap-1.5 text-xs font-mono font-medium px-2 py-0.5 rounded bg-warning/15 text-warning">
            <span className="w-1.5 h-1.5 rounded-full bg-warning" />
            approval
          </span>
          {item.expires_at && <CountdownTimer expiresAt={item.expires_at} />}
        </div>
      </div>

      {/* Verification — auto-expanded for issues, collapsible toggle for clean */}
      {a.verification && !hasIssue && (
        <div className="px-5 pb-3">
          <button
            onClick={() => setVerifyOpen(o => !o)}
            className="flex items-center gap-1.5 text-xs text-text-tertiary hover:text-text-secondary"
          >
            <svg className={`w-3 h-3 transition-transform ${verifyOpen ? 'rotate-90' : ''}`} fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24"><path d="M9 5l7 7-7 7"/></svg>
            <span className="font-medium">Verification</span>
            <VerificationIcon result={a.verification.param_scope} type="param" />
            <VerificationIcon result={a.verification.reason_coherence} type="reason" />
          </button>
        </div>
      )}
      {showPanel && <VerificationPanel verification={a.verification!} />}

      {/* Parameters */}
      {paramEntries.length > 0 && (
        <div className="px-5 pb-3">
          <div className="bg-surface-0 border border-border-subtle rounded overflow-hidden">
            <table className="w-full text-xs">
              <tbody>
                {paramEntries.map(([key, value], i) => (
                  <tr key={key} className={i < paramEntries.length - 1 ? 'border-b border-border-subtle' : ''}>
                    <td className="px-3 py-1.5 font-mono text-text-tertiary w-28 align-top">{key}</td>
                    <td className="px-3 py-1.5 font-mono text-text-primary break-all">
                      {typeof value === 'string' ? value : JSON.stringify(value)}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      )}

      {inlineChat && (
        <div className="px-5 pb-3">
          <InlineChatBoundNotice />
        </div>
      )}
      {error && (
        <div className="px-5 pb-3">
          <div className="rounded border border-danger/30 bg-danger/10 px-3 py-2 text-xs text-danger flex items-center justify-between gap-3">
            <span>{error}</span>
            <button onClick={() => setError(null)} className="text-xs underline hover:opacity-80">Dismiss</button>
          </div>
        </div>
      )}

      {/* Actions — preserved on error so the user can retry. */}
      <div className="px-4 py-3 border-t border-border-subtle flex items-center justify-end gap-2">
        <button
          onClick={() => denyMut.mutate()}
          disabled={isPending || inlineChat}
          className="rounded px-4 py-1.5 text-sm font-medium bg-danger/10 text-danger border border-danger/20 hover:bg-danger/20 disabled:opacity-50 min-h-[44px] sm:min-h-0"
        >
          Deny
        </button>
        <button
          onClick={() => approveMut.mutate()}
          disabled={isPending || inlineChat}
          className="bg-brand text-surface-0 font-medium rounded px-5 py-1.5 text-sm hover:bg-brand-strong disabled:opacity-50 min-h-[44px] sm:min-h-0"
        >
          {approveMut.isPending ? 'Approving...' : 'Approve'}
        </button>
      </div>
    </div>
  )
}
