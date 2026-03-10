import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api, type Task, type AuditEntry } from '../api/client'
import { format } from 'date-fns'
import { serviceName, actionName } from '../lib/services'
import CountdownTimer from './CountdownTimer'

// ── Status helpers ───────────────────────────────────────────────────────────

const STATUS_BADGE: Record<string, { bg: string; text: string; label: string }> = {
  pending_approval: { bg: 'bg-warning/15', text: 'text-warning', label: 'pending' },
  pending_scope_expansion: { bg: 'bg-warning/15', text: 'text-warning', label: 'scope expansion' },
  active: { bg: 'bg-success/15', text: 'text-success', label: 'active' },
  completed: { bg: 'bg-surface-2', text: 'text-text-tertiary', label: 'completed' },
  expired: { bg: 'bg-surface-2', text: 'text-text-tertiary', label: 'expired' },
  denied: { bg: 'bg-danger/15', text: 'text-danger', label: 'denied' },
  revoked: { bg: 'bg-surface-2', text: 'text-text-tertiary', label: 'revoked' },
}

const STATUS_DOT: Record<string, string> = {
  pending_approval: 'bg-warning',
  pending_scope_expansion: 'bg-warning',
  active: 'bg-success',
  completed: 'bg-text-tertiary',
  expired: 'bg-text-tertiary',
  denied: 'bg-danger',
  revoked: 'bg-text-tertiary',
}

const LEFT_BORDER: Record<string, string> = {
  pending_approval: 'border-l-warning',
  pending_scope_expansion: 'border-l-warning',
  active: 'border-l-success',
}

const OUTCOME_DOT: Record<string, string> = {
  executed: 'bg-success',
  blocked: 'bg-danger',
  restricted: 'bg-danger',
  pending: 'bg-warning',
  denied: 'bg-text-tertiary',
  error: 'bg-danger',
  timeout: 'bg-text-tertiary',
}

// ── Main TaskCard ────────────────────────────────────────────────────────────

export default function TaskCard({
  task,
  agentName,
}: {
  task: Task
  agentName: string
}) {
  const qc = useQueryClient()
  const [result, setResult] = useState<string | null>(null)
  const [scopesOpen, setScopesOpen] = useState(false)
  const [activityOpen, setActivityOpen] = useState(false)

  const invalidate = () => {
    qc.invalidateQueries({ queryKey: ['tasks'] })
    qc.invalidateQueries({ queryKey: ['overview'] })
  }

  const approveMut = useMutation({
    mutationFn: () => api.tasks.approve(task.id),
    onSuccess: () => { setResult('Approved'); invalidate() },
  })
  const denyMut = useMutation({
    mutationFn: () => api.tasks.deny(task.id),
    onSuccess: () => { setResult('Denied'); invalidate() },
  })
  const expandApproveMut = useMutation({
    mutationFn: () => api.tasks.expandApprove(task.id),
    onSuccess: () => { setResult('Expansion approved'); invalidate() },
  })
  const expandDenyMut = useMutation({
    mutationFn: () => api.tasks.expandDeny(task.id),
    onSuccess: () => { setResult('Expansion denied'); invalidate() },
  })
  const revokeMut = useMutation({
    mutationFn: () => api.tasks.revoke(task.id),
    onSuccess: () => { setResult('Revoked'); invalidate() },
  })

  const { data: auditData, isLoading: auditLoading } = useQuery({
    queryKey: ['audit', { task_id: task.id }],
    queryFn: () => api.audit.list({ task_id: task.id, limit: 50 }),
    enabled: activityOpen,
    refetchInterval: (query) =>
      activityOpen && task.request_count !== (query.state.data?.entries?.length ?? 0) ? 1_000 : false,
  })

  const isPending = approveMut.isPending || denyMut.isPending || expandApproveMut.isPending || expandDenyMut.isPending || revokeMut.isPending
  const needsApproval = task.status === 'pending_approval'
  const needsExpansion = task.status === 'pending_scope_expansion'
  const isActive = task.status === 'active'
  const isStanding = task.lifetime === 'standing'
  const isActionable = needsApproval || needsExpansion

  const autoActions = task.authorized_actions.filter(a => a.auto_execute)
  const manualActions = task.authorized_actions.filter(a => !a.auto_execute)
  const totalScopes = task.authorized_actions.length

  const auditEntries = auditData?.entries ?? []
  const badge = STATUS_BADGE[task.status] ?? { bg: 'bg-surface-2', text: 'text-text-tertiary', label: task.status }
  const dotColor = STATUS_DOT[task.status] ?? 'bg-text-tertiary'
  const leftBorder = LEFT_BORDER[task.status] ?? 'border-l-transparent'

  return (
    <div className={`bg-surface-1 border border-border-default rounded-md border-l-[3px] ${leftBorder} overflow-hidden`}>
      {/* Header */}
      <div className="px-5 pt-5 pb-4">
        <p className="text-lg font-semibold text-text-primary leading-snug">{task.purpose}</p>
        <div className="flex items-center gap-2 mt-2">
          <span className={`inline-flex items-center gap-1.5 text-xs font-mono font-medium px-2 py-0.5 rounded ${badge.bg} ${badge.text}`}>
            <span className={`w-1.5 h-1.5 rounded-full ${dotColor}`} />
            {badge.label}
          </span>
          <span className="text-xs font-mono text-text-secondary">{agentName}</span>
          <span className="text-xs text-text-tertiary">&middot;</span>
          <span className="text-xs font-mono text-text-tertiary">
            {isStanding ? 'ongoing' : 'session'}
            {isActive && !isStanding && task.expires_at && <> &middot; <CountdownTimer expiresAt={task.expires_at} /></>}
            {!isActive && task.expires_in_seconds > 0 && ` · ${Math.round(task.expires_in_seconds / 60)}m`}
          </span>
        </div>
      </div>

      {/* Result message */}
      {result && (
        <div className="px-5 pb-3">
          <div className="p-2 bg-surface-2 rounded text-sm text-text-tertiary">{result}</div>
        </div>
      )}

      {/* Scope expansion: collapsed approved scopes + new scope */}
      {needsExpansion && !result ? (
        <>
          <div className="px-5 pb-3 flex items-center justify-between">
            <button
              onClick={() => setScopesOpen(o => !o)}
              className="flex items-center gap-1.5 text-xs text-text-tertiary hover:text-text-secondary"
            >
              <svg className={`w-3 h-3 transition-transform ${scopesOpen ? 'rotate-90' : ''}`} fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24"><path d="M9 5l7 7-7 7"/></svg>
              <span className="font-medium">Approved scopes</span>
            </button>
            <span className="text-xs font-mono text-text-tertiary">{totalScopes} scopes &middot; {autoActions.length} auto &middot; {manualActions.length} approval</span>
          </div>

          {scopesOpen && (
            <div className="px-4 pb-2">
              <ScopeGroupTables autoActions={autoActions} manualActions={manualActions} />
            </div>
          )}

          {task.pending_action && (
            <div className="px-4 pb-3">
              <div className="bg-surface-0 border rounded overflow-hidden" style={{ borderColor: 'rgba(245, 158, 11, 0.3)' }}>
                <div className="px-3 py-1.5 border-b flex items-center gap-1.5" style={{ background: 'rgba(245, 158, 11, 0.04)', borderColor: 'rgba(245, 158, 11, 0.15)' }}>
                  <span className="w-1.5 h-1.5 rounded-full bg-warning" />
                  <span className="text-[10px] font-medium text-warning uppercase tracking-wider">New scope requested</span>
                </div>
                <table className="w-full text-sm">
                  <tbody>
                    <tr>
                      <td className="px-3 py-2 font-mono text-text-primary w-40">{serviceName(task.pending_action.service)} · {actionName(task.pending_action.action)}</td>
                      <td className="px-3 py-2 text-sm text-text-secondary">{task.pending_reason ?? ''}</td>
                    </tr>
                  </tbody>
                </table>
                {task.pending_action.auto_execute && (
                  <div className="px-3 py-1.5 border-t border-border-subtle flex items-center gap-1.5">
                    <span className="w-1.5 h-1.5 rounded-full bg-success" />
                    <span className="text-[10px] font-mono text-success">auto-execute</span>
                  </div>
                )}
              </div>
            </div>
          )}
        </>
      ) : isActionable && !result ? (
        /* New task: show grouped scope tables */
        <div className="px-4 space-y-2 pb-3">
          <ScopeGroupTables autoActions={autoActions} manualActions={manualActions} />
        </div>
      ) : (
        /* Active / completed / other: collapsible scopes */
        <div className="px-5 pb-3 flex items-center justify-between">
          <button
            onClick={() => setScopesOpen(o => !o)}
            className="flex items-center gap-1.5 text-xs text-text-tertiary hover:text-text-secondary"
          >
            <svg className={`w-3 h-3 transition-transform ${scopesOpen ? 'rotate-90' : ''}`} fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24"><path d="M9 5l7 7-7 7"/></svg>
            <span className="font-medium">Scopes</span>
          </button>
          <span className="text-xs font-mono text-text-tertiary">{totalScopes} scopes &middot; {autoActions.length} auto &middot; {manualActions.length} approval</span>
        </div>
      )}

      {/* Expanded scopes for non-actionable states */}
      {scopesOpen && !isActionable && (
        <div className="px-4 pb-3">
          <ScopeGroupTables autoActions={autoActions} manualActions={manualActions} />
        </div>
      )}

      {/* Action buttons */}
      {!result && needsApproval && (
        <div className="px-4 py-3 border-t border-border-subtle flex items-center justify-between">
          <span className="text-xs font-mono text-text-tertiary">{totalScopes} scopes &middot; {autoActions.length} auto &middot; {manualActions.length} approval</span>
          <div className="flex items-center gap-2">
            <button onClick={() => denyMut.mutate()} disabled={isPending}
              className="rounded px-4 py-1.5 text-sm font-medium bg-danger/10 text-danger border border-danger/20 hover:bg-danger/20 disabled:opacity-50">
              Deny
            </button>
            <button onClick={() => approveMut.mutate()} disabled={isPending}
              className="bg-brand text-surface-0 font-medium rounded px-5 py-1.5 text-sm hover:bg-brand-strong disabled:opacity-50">
              {approveMut.isPending ? 'Approving...' : 'Approve Task'}
            </button>
          </div>
        </div>
      )}

      {!result && needsExpansion && (
        <div className="px-4 py-3 border-t border-border-subtle flex items-center justify-end gap-2">
          <button onClick={() => expandDenyMut.mutate()} disabled={isPending}
            className="rounded px-4 py-1.5 text-sm font-medium bg-danger/10 text-danger border border-danger/20 hover:bg-danger/20 disabled:opacity-50">
            Deny
          </button>
          <button onClick={() => expandApproveMut.mutate()} disabled={isPending}
            className="bg-brand text-surface-0 font-medium rounded px-5 py-1.5 text-sm hover:bg-brand-strong disabled:opacity-50">
            {expandApproveMut.isPending ? 'Approving...' : 'Approve Scope'}
          </button>
        </div>
      )}

      {/* Activity section */}
      {(isActive || task.request_count > 0) && (
        <>
          <div className="px-5 py-2 border-t border-border-subtle flex items-center justify-between">
            <button
              onClick={() => setActivityOpen(o => !o)}
              className="flex items-center gap-1.5 text-xs text-text-secondary hover:text-text-primary"
            >
              <svg className={`w-3 h-3 transition-transform ${activityOpen ? 'rotate-90' : ''}`} fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24"><path d={activityOpen ? 'M19 9l-7 7-7-7' : 'M9 5l7 7-7 7'}/></svg>
              <span className="font-medium">Activity</span>
            </button>
            <span className="text-xs font-mono text-text-tertiary">
              {task.request_count} request{task.request_count !== 1 ? 's' : ''}
            </span>
          </div>

          {activityOpen && (
            <div className="divide-y divide-border-subtle text-sm">
              {auditLoading && <div className="px-4 py-2 text-xs text-text-tertiary">Loading...</div>}
              {!auditLoading && auditEntries.length === 0 && (
                <div className="px-4 py-2 text-xs text-text-tertiary">No actions recorded yet.</div>
              )}
              {auditEntries.map(e => (
                <ActivityRow key={e.id} entry={e} />
              ))}
            </div>
          )}
        </>
      )}

      {/* Revoke */}
      {!result && isActive && (
        <div className="px-4 py-2.5 border-t border-border-subtle flex items-center justify-end">
          <button
            onClick={() => revokeMut.mutate()}
            disabled={revokeMut.isPending}
            className="rounded px-3 py-1 text-xs font-medium text-text-secondary border border-border-subtle hover:bg-surface-2 hover:text-text-primary disabled:opacity-50"
          >
            {revokeMut.isPending ? 'Revoking...' : 'Revoke Task'}
          </button>
        </div>
      )}
    </div>
  )
}

// ── Scope group tables ───────────────────────────────────────────────────────

function ScopeGroupTables({ autoActions, manualActions }: {
  autoActions: { service: string; action: string; expected_use?: string }[]
  manualActions: { service: string; action: string; expected_use?: string }[]
}) {
  return (
    <>
      {autoActions.length > 0 && (
        <div className="bg-surface-0 border border-border-subtle rounded overflow-hidden">
          <div className="px-3 py-1.5 border-b border-border-subtle flex items-center gap-1.5" style={{ background: 'rgba(34, 197, 94, 0.04)' }}>
            <span className="w-1.5 h-1.5 rounded-full bg-success" />
            <span className="text-[10px] font-medium text-success uppercase tracking-wider">Auto-execute</span>
          </div>
          <table className="w-full text-sm">
            <tbody>
              {autoActions.map((a, i) => (
                <tr key={`${a.service}|${a.action}`} className={i < autoActions.length - 1 ? 'border-b border-border-subtle' : ''}>
                  <td className="px-3 py-2 font-mono text-text-primary w-40">{serviceName(a.service)} · {actionName(a.action)}</td>
                  <td className="px-3 py-2 text-sm text-text-secondary">{a.expected_use ?? ''}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {manualActions.length > 0 && (
        <div className="bg-surface-0 border rounded overflow-hidden mt-2" style={{ borderColor: 'rgba(245, 158, 11, 0.3)' }}>
          <div className="px-3 py-1.5 border-b flex items-center gap-1.5" style={{ background: 'rgba(245, 158, 11, 0.04)', borderColor: 'rgba(245, 158, 11, 0.15)' }}>
            <span className="w-1.5 h-1.5 rounded-full bg-warning" />
            <span className="text-[10px] font-medium text-warning uppercase tracking-wider">Requires approval</span>
          </div>
          <table className="w-full text-sm">
            <tbody>
              {manualActions.map((a, i) => (
                <tr key={`${a.service}|${a.action}`} className={i < manualActions.length - 1 ? 'border-b border-border-subtle' : ''}>
                  <td className="px-3 py-2 font-mono text-text-primary w-40">{serviceName(a.service)} · {actionName(a.action)}</td>
                  <td className="px-3 py-2 text-sm text-text-secondary">{a.expected_use ?? ''}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </>
  )
}

// ── Activity feed row ────────────────────────────────────────────────────────

function VerificationIcon({ result: r, type }: { result: string; type: 'param' | 'reason' }) {
  const isOk = r === 'ok'
  const isDanger = type === 'param' ? r === 'violation' : r === 'incoherent'
  const isWarning = type === 'reason' && r === 'insufficient'

  if (isOk) return (
    <span className="inline-flex items-center text-[10px] font-mono px-1 py-0.5 rounded bg-success/10 text-success">
      <svg className="w-2.5 h-2.5" fill="none" stroke="currentColor" strokeWidth="2.5" viewBox="0 0 24 24"><path d="M5 13l4 4L19 7"/></svg>
    </span>
  )
  if (isDanger) return (
    <span className="inline-flex items-center text-[10px] font-mono px-1 py-0.5 rounded bg-danger/12 text-danger">
      <svg className="w-2.5 h-2.5" fill="none" stroke="currentColor" strokeWidth="2.5" viewBox="0 0 24 24"><path d="M6 18L18 6M6 6l12 12"/></svg>
    </span>
  )
  if (isWarning) return (
    <span className="inline-flex items-center text-[10px] font-mono px-1 py-0.5 rounded bg-warning/15 text-warning">
      <svg className="w-2.5 h-2.5" fill="none" stroke="currentColor" strokeWidth="2.5" viewBox="0 0 24 24"><path d="M12 9v2m0 4h.01"/></svg>
    </span>
  )
  return null
}

function ParamsTable({ params }: { params: Record<string, unknown> }) {
  if (!params || Object.keys(params).length === 0) return null
  return (
    <div className="bg-surface-0 border border-border-subtle rounded overflow-hidden">
      <table className="w-full text-xs">
        <tbody>
          {Object.entries(params).map(([key, value], i, arr) => (
            <tr key={key} className={i < arr.length - 1 ? 'border-b border-border-subtle' : ''}>
              <td className="px-3 py-1.5 font-mono text-text-tertiary w-28 align-top">{key}</td>
              <td className="px-3 py-1.5 font-mono text-text-primary break-all">
                {typeof value === 'string' ? value : JSON.stringify(value)}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

function ActivityRow({ entry }: { entry: AuditEntry }) {
  const [expanded, setExpanded] = useState(false)
  const dotColor = OUTCOME_DOT[entry.outcome] ?? 'bg-text-tertiary'
  const hasProblem = entry.outcome === 'blocked' || entry.outcome === 'restricted' ||
    (entry.verification && (entry.verification.param_scope !== 'ok' || entry.verification.reason_coherence !== 'ok'))
  const rowBg = entry.outcome === 'blocked' || entry.outcome === 'restricted'
    ? 'rgba(239, 68, 68, 0.04)'
    : hasProblem ? 'rgba(245, 158, 11, 0.04)' : undefined

  return (
    <div style={rowBg ? { background: rowBg } : undefined}>
      <div
        className="px-4 py-2 flex items-center justify-between cursor-pointer"
        onClick={() => setExpanded(e => !e)}
      >
        <div className="flex items-center gap-2 min-w-0">
          <span className={`w-1.5 h-1.5 rounded-full shrink-0 ${dotColor}`} />
          <span className="font-mono text-text-primary text-xs">{serviceName(entry.service)} · {actionName(entry.action)}</span>
          <span className="text-text-tertiary text-xs">&middot;</span>
          <span className="text-text-secondary text-xs truncate" style={{ maxWidth: 260 }}>
            {entry.reason ?? entry.outcome}
          </span>
        </div>
        <div className="flex items-center gap-2 shrink-0">
          <span className="text-[10px] font-mono text-text-tertiary">
            {format(new Date(entry.timestamp), 'h:mm a')}
          </span>
          {entry.verification && (
            <>
              <VerificationIcon result={entry.verification.param_scope} type="param" />
              <VerificationIcon result={entry.verification.reason_coherence} type="reason" />
            </>
          )}
        </div>
      </div>

      {expanded && entry.verification && (
        <div className="px-4 pb-3 pt-1 space-y-2">
          <div className={`ml-3 pl-3 border-l-2 space-y-1.5 ${
            entry.outcome === 'blocked' || entry.outcome === 'restricted' ? 'border-danger'
            : entry.verification.reason_coherence !== 'ok' || entry.verification.param_scope !== 'ok' ? 'border-warning'
            : 'border-success'
          }`}>
            <div className="flex items-center gap-2">
              <span className={`text-[10px] font-mono font-medium ${
                entry.verification.param_scope === 'ok' ? 'text-success' : entry.verification.param_scope === 'violation' ? 'text-danger' : 'text-text-tertiary'
              }`}>params: {entry.verification.param_scope}</span>
              <span className={`text-[10px] font-mono font-medium ${
                entry.verification.reason_coherence === 'ok' ? 'text-success'
                : entry.verification.reason_coherence === 'incoherent' ? 'text-danger'
                : entry.verification.reason_coherence === 'insufficient' ? 'text-warning'
                : 'text-text-tertiary'
              }`}>reason: {entry.verification.reason_coherence}</span>
            </div>
            <p className="text-xs text-text-secondary">{entry.verification.explanation}</p>
            <div className="text-[10px] font-mono text-text-tertiary">{entry.verification.model} &middot; {entry.verification.latency_ms}ms{entry.duration_ms ? ` · executed in ${entry.duration_ms}ms` : ''}</div>
          </div>
          <ParamsTable params={entry.params_safe} />
        </div>
      )}

      {expanded && !entry.verification && (
        <div className="px-4 pb-3 pt-1 space-y-2">
          <div className="ml-3 pl-3 border-l-2 border-border-default space-y-1.5">
            {entry.error_msg && <p className="text-xs text-danger">{entry.error_msg}</p>}
            {entry.reason && <p className="text-xs text-text-secondary">{entry.reason}</p>}
            <div className="text-[10px] font-mono text-text-tertiary">{entry.duration_ms}ms</div>
          </div>
          <ParamsTable params={entry.params_safe} />
        </div>
      )}
    </div>
  )
}
