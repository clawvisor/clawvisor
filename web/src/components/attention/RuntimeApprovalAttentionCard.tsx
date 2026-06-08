import { useState } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { api, type ApprovalRecord } from '../../api/client'
import CountdownTimer from '../CountdownTimer'
import {
  runtimeApprovalDetail,
  runtimeApprovalPrimary,
  runtimeApprovalReason,
  runtimePayload,
  runtimeSummary,
} from './runtimeHelpers'
import { invalidateAttention } from './invalidate'

export default function RuntimeApprovalAttentionCard({ approval }: { approval: ApprovalRecord }) {
  const qc = useQueryClient()
  const [result, setResult] = useState<string | null>(null)

  const summary = runtimeSummary(approval)
  const payload = runtimePayload(approval)
  const primary = runtimeApprovalPrimary(payload, summary, approval.kind)
  const reason = runtimeApprovalReason(payload, summary)
  const detail = runtimeApprovalDetail(payload)
  const allowLabel = approval.resolution_transport === 'release_held_tool_use' ? 'Release Tool Call' : 'Allow Once'

  const resolveMut = useMutation({
    mutationFn: (resolution: 'allow_once' | 'deny') => api.runtime.resolveApproval(approval.id, resolution),
    onSuccess: (_res, resolution) => {
      setResult(resolution === 'deny' ? 'Denied' : 'Allowed once')
      invalidateAttention(qc)
    },
    onError: (err: Error) => setResult(`Failed: ${err.message}`),
  })

  if (result) {
    return (
      <div className="dev-panel p-5">
        <div className="dev-inset p-3 font-mono text-sm text-text-tertiary">{result}</div>
      </div>
    )
  }

  return (
    <div className="dev-card-accent-brand">
      <div className="px-5 pt-5 pb-4">
        <span className="font-mono text-lg font-semibold text-text-primary break-all">{primary}</span>
        {reason && <p className="text-sm text-text-secondary mt-1.5">{reason}</p>}
        <div className="flex flex-wrap items-center gap-2 mt-2">
          <span className="dev-badge--brand">
            <span className="w-1.5 h-1.5 rounded-full bg-brand" />
            {approval.resolution_transport === 'release_held_tool_use' ? 'inline runtime approval' : 'runtime retry approval'}
          </span>
          {approval.session_id && (
            <span className="text-xs text-text-tertiary">session <code className="font-mono">{approval.session_id.slice(0, 8)}</code></span>
          )}
          {approval.expires_at && <CountdownTimer expiresAt={approval.expires_at} />}
        </div>
        {payload && (
          <div className="mt-3 dev-inset p-3 space-y-1">
            {detail && <div className="text-sm font-mono text-text-tertiary break-all">{detail}</div>}
            {'host' in payload && payload.host != null && (
              <div className="text-sm font-mono text-text-tertiary">host: {String(payload.host)}</div>
            )}
            {'path' in payload && payload.path != null && (
              <div className="text-sm font-mono text-text-tertiary">path: {String(payload.path)}</div>
            )}
            {typeof payload.query === 'object' && payload.query !== null && Object.keys(payload.query as object).length > 0 && (
              <div className="text-sm font-mono text-text-tertiary break-all">query: {JSON.stringify(payload.query)}</div>
            )}
          </div>
        )}
      </div>
      <div className="px-4 py-3 border-t border-border-subtle flex items-center justify-end gap-2">
        <button
          onClick={() => resolveMut.mutate('deny')}
          disabled={resolveMut.isPending}
          className="dev-btn-danger"
        >
          deny
        </button>
        <button
          onClick={() => resolveMut.mutate('allow_once')}
          disabled={resolveMut.isPending}
          className="dev-btn-primary"
        >
          {resolveMut.isPending ? 'Updating...' : allowLabel}
        </button>
      </div>
    </div>
  )
}
