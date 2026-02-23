import { useState } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { api, type PendingApproval } from '../api/client'
import { formatDistanceToNow, differenceInSeconds } from 'date-fns'

function CountdownTimer({ expiresAt }: { expiresAt: string }) {
  const secs = Math.max(0, differenceInSeconds(new Date(expiresAt), new Date()))
  const mins = Math.floor(secs / 60)
  const s = secs % 60
  const urgent = secs < 60

  return (
    <span className={`font-mono text-xs tabular-nums ${urgent ? 'text-red-600' : 'text-gray-400'}`}>
      {mins}:{String(s).padStart(2, '0')} remaining
    </span>
  )
}

function ApprovalCard({ approval }: { approval: PendingApproval }) {
  const qc = useQueryClient()
  const [result, setResult] = useState<string | null>(null)

  const approveMut = useMutation({
    mutationFn: () => api.approvals.approve(approval.request_id),
    onSuccess: (res) => {
      setResult(res.status === 'executed' ? 'Approved & executed' : `Outcome: ${res.status}`)
      qc.invalidateQueries({ queryKey: ['approvals'] })
    },
  })

  const denyMut = useMutation({
    mutationFn: () => api.approvals.deny(approval.request_id),
    onSuccess: () => {
      setResult('Denied')
      qc.invalidateQueries({ queryKey: ['approvals'] })
    },
  })

  const blob = approval.request_blob
  const isPending = approveMut.isPending || denyMut.isPending

  if (result) {
    return (
      <div className="p-3 bg-gray-50 rounded text-sm text-gray-500">{result}</div>
    )
  }

  return (
    <div className="border rounded-lg p-4 space-y-3 bg-white">
      <div className="flex items-start justify-between">
        <div>
          <span className="text-sm font-semibold text-gray-900">
            {blob.service} · {blob.action}
          </span>
          <CountdownTimer expiresAt={approval.expires_at} />
        </div>
        <span className="text-xs text-gray-400">
          {formatDistanceToNow(new Date(approval.created_at), { addSuffix: true })}
        </span>
      </div>

      {blob.reason && (
        <p className="text-xs text-gray-600 italic">"{blob.reason}"</p>
      )}

      {Object.keys(blob.params ?? {}).length > 0 && (
        <pre className="text-xs bg-gray-50 border rounded p-2 overflow-auto max-h-28 font-mono text-gray-700">
          {JSON.stringify(blob.params, null, 2)}
        </pre>
      )}

      <div className="flex gap-2 pt-1">
        <button
          onClick={() => approveMut.mutate()}
          disabled={isPending}
          className="flex-1 py-1.5 text-sm rounded bg-green-600 text-white hover:bg-green-700 disabled:opacity-50"
        >
          Approve
        </button>
        <button
          onClick={() => denyMut.mutate()}
          disabled={isPending}
          className="flex-1 py-1.5 text-sm rounded bg-red-100 text-red-700 hover:bg-red-200 disabled:opacity-50"
        >
          Deny
        </button>
      </div>
    </div>
  )
}

export default function ApprovalsPanel() {
  const [open, setOpen] = useState(true)

  const { data } = useQuery({
    queryKey: ['approvals'],
    queryFn: () => api.approvals.list(),
    refetchInterval: 10_000,
  })

  const entries = data?.entries ?? []
  if (entries.length === 0) return null

  return (
    <div className="fixed bottom-4 right-4 z-50 w-80 shadow-xl">
      {/* Header */}
      <button
        onClick={() => setOpen(o => !o)}
        className="w-full flex items-center justify-between bg-orange-500 text-white px-4 py-2.5 rounded-t-lg font-medium text-sm"
      >
        <span>⏳ {entries.length} Pending Approval{entries.length > 1 ? 's' : ''}</span>
        <span>{open ? '▼' : '▲'}</span>
      </button>

      {/* Panel body */}
      {open && (
        <div className="bg-white border border-t-0 rounded-b-lg max-h-[60vh] overflow-y-auto divide-y">
          {entries.map(a => (
            <div key={a.request_id} className="p-3">
              <ApprovalCard approval={a} />
            </div>
          ))}
        </div>
      )}
    </div>
  )
}
