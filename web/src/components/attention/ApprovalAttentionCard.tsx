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
  const [result, setResult] = useState<string | null>(null)
  const [inlineChatBound, setInlineChatBound] = useState(false)
  const [verifyOpen, setVerifyOpen] = useState(false)
  const a = item.approval!

  const approveMut = useMutation({
    mutationFn: () => api.approvals.approve(a.request_id, 'allow_once', a.task_id),
    onSuccess: (res) => {
      setResult(res.status === 'executed' ? 'Approved & executed' : `Outcome: ${res.status}`)
      invalidateAttention(qc)
    },
    onError: (err: Error) => {
      if (err instanceof APIError && err.code === 'INLINE_CHAT_BOUND') {
        setInlineChatBound(true)
      }
    },
  })

  const denyMut = useMutation({
    mutationFn: () => api.approvals.deny(a.request_id, a.task_id),
    onSuccess: () => {
      setResult('Denied')
      invalidateAttention(qc)
    },
    onError: (err: Error) => {
      if (err instanceof APIError && err.code === 'INLINE_CHAT_BOUND') {
        setInlineChatBound(true)
      }
    },
  })

  const isPending = approveMut.isPending || denyMut.isPending
  const params = a.params ?? {}
  const paramEntries = Object.entries(params)
  const hasIssue = a.verification ? hasVerificationIssue(a.verification) : false
  const showPanel = a.verification && (hasIssue || verifyOpen)

  if (result) {
    return (
      <div className="dev-panel p-5">
        <div className="dev-inset p-3 font-mono text-sm text-text-tertiary">{result}</div>
      </div>
    )
  }

  return (
    <div className="dev-card-accent-warning">
      <div className="px-5 pt-5 pb-4">
        <span className="font-mono text-lg font-semibold text-text-primary">{serviceName(a.service)} · {actionName(a.action)}</span>
        {a.reason && <p className="text-sm text-text-secondary mt-1.5">{a.reason}</p>}
        <div className="flex items-center gap-2 mt-2">
          <span className="dev-badge--warning">
            <span className="w-1.5 h-1.5 rounded-full bg-warning" />
            approval
          </span>
          {item.expires_at && <CountdownTimer expiresAt={item.expires_at} />}
        </div>
      </div>

      {inlineChatBound && <InlineChatBoundNotice />}

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

      {paramEntries.length > 0 && (
        <div className="px-5 pb-3">
          <div className="dev-inset overflow-hidden">
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

      <div className="px-4 py-3 border-t border-border-subtle flex items-center justify-end gap-2">
        <button
          onClick={() => denyMut.mutate()}
          disabled={isPending || inlineChatBound}
          title={inlineChatBound ? 'Reply approve/deny in the agent chat' : undefined}
          className="dev-btn-danger"
        >
          deny
        </button>
        <button
          onClick={() => approveMut.mutate()}
          disabled={isPending || inlineChatBound}
          title={inlineChatBound ? 'Reply approve/deny in the agent chat' : undefined}
          className="dev-btn-primary"
        >
          {approveMut.isPending ? 'Approving...' : 'Approve'}
        </button>
      </div>
    </div>
  )
}
