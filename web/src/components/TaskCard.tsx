import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api, type Task, type PlannedCall, type AuditEntry, type RiskAssessment, type ApprovalRationale } from '../api/client'
import { format } from 'date-fns'
import { serviceName, actionName } from '../lib/services'
import CountdownTimer from './CountdownTimer'
import RiskBadge from './RiskBadge'
import VerificationIcon from './VerificationIcon'

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
  onRevoke,
}: {
  task: Task
  agentName: string
  onRevoke?: (taskId: string) => Promise<unknown>
}) {
  const qc = useQueryClient()
  const [result, setResult] = useState<string | null>(null)
  const [scopesOpen, setScopesOpen] = useState(false)
  const [activityOpen, setActivityOpen] = useState(false)
  const [riskOpen, setRiskOpen] = useState(false)
  const [confirmApprove, setConfirmApprove] = useState(false)

  const invalidate = () => {
    qc.invalidateQueries({ queryKey: ['tasks'] })
    qc.invalidateQueries({ queryKey: ['overview'] })
    qc.invalidateQueries({ queryKey: ['queue'] })
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
    mutationFn: () => onRevoke ? onRevoke(task.id) : api.tasks.revoke(task.id),
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
  const riskLevel = task.risk_level ?? ''
  const riskDetails = task.risk_details
  const hasRisk = riskLevel !== '' && riskLevel !== 'unknown'
  const isHighRisk = riskLevel === 'high' || riskLevel === 'critical'
  const riskPanelExpanded = !isActive && riskLevel !== 'low' && riskLevel !== ''
  // Critical pending tasks shift the left border to danger red.
  const leftBorder = (isActionable && riskLevel === 'critical')
    ? 'border-l-danger'
    : (LEFT_BORDER[task.status] ?? 'border-l-transparent')

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
          {riskLevel && <RiskBadge level={riskLevel} />}
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

      {/* Risk assessment panel (auto-expanded for high/critical pending, collapsible otherwise) */}
      {riskDetails && hasRisk && (
        riskPanelExpanded ? (
          <RiskPanel risk={riskDetails} level={riskLevel} />
        ) : (
          <>
            <div className="px-5 pb-3">
              <button
                onClick={() => setRiskOpen(o => !o)}
                className="flex items-center gap-1.5 text-xs text-text-tertiary hover:text-text-secondary"
              >
                <svg className={`w-3 h-3 transition-transform ${riskOpen ? 'rotate-90' : ''}`} fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24"><path d="M9 5l7 7-7 7"/></svg>
                <span className="font-medium">Risk assessment</span>
              </button>
            </div>
            {riskOpen && <RiskPanel risk={riskDetails} level={riskLevel} />}
          </>
        )
      )}

      {/* Auto-approval rationale */}
      {task.approval_source === 'telegram_group' && task.approval_rationale && (
        <AutoApprovalPanel rationale={task.approval_rationale} />
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
              <div className="bg-surface-0 border rounded overflow-hidden" style={{ borderColor: 'var(--color-warning-border-light)' }}>
                <div className="px-3 py-1.5 border-b flex items-center gap-1.5" style={{ background: 'var(--color-warning-tint)', borderColor: 'var(--color-warning-border-subtle)' }}>
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
        /* New task: show grouped scope tables + planned calls */
        <div className="px-4 space-y-2 pb-3">
          <ScopeGroupTables autoActions={autoActions} manualActions={manualActions} />
          {task.planned_calls && task.planned_calls.length > 0 && (
            <PlannedCallsTable calls={task.planned_calls} />
          )}
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
        <div className="px-4 pb-3 space-y-2">
          <ScopeGroupTables autoActions={autoActions} manualActions={manualActions} />
          {task.planned_calls && task.planned_calls.length > 0 && (
            <PlannedCallsTable calls={task.planned_calls} />
          )}
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
            {isHighRisk && !confirmApprove ? (
              <button onClick={() => setConfirmApprove(true)} disabled={isPending}
                className="bg-brand text-surface-0 font-medium rounded px-5 py-1.5 text-sm hover:bg-brand-strong disabled:opacity-50">
                Approve Task
              </button>
            ) : (
              <button onClick={() => approveMut.mutate()} disabled={isPending}
                className={`font-medium rounded px-5 py-1.5 text-sm disabled:opacity-50 ${
                  confirmApprove
                    ? 'bg-danger text-surface-0 hover:bg-danger/80'
                    : 'bg-brand text-surface-0 hover:bg-brand-strong'
                }`}>
                {approveMut.isPending ? 'Approving...' : confirmApprove ? 'Confirm Approve' : 'Approve Task'}
              </button>
            )}
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
          <div className="px-3 py-1.5 border-b border-border-subtle flex items-center gap-1.5" style={{ background: 'var(--color-success-tint)' }}>
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
        <div className="bg-surface-0 border rounded overflow-hidden mt-2" style={{ borderColor: 'var(--color-warning-border-light)' }}>
          <div className="px-3 py-1.5 border-b flex items-center gap-1.5" style={{ background: 'var(--color-warning-tint)', borderColor: 'var(--color-warning-border-subtle)' }}>
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

// ── Planned calls table ──────────────────────────────────────────────────────

function PlannedCallsTable({ calls }: { calls: PlannedCall[] }) {
  return (
    <div className="bg-surface-0 border border-border-subtle rounded overflow-hidden mt-2">
      <div className="px-3 py-1.5 border-b border-border-subtle flex items-center gap-1.5" style={{ background: 'var(--color-brand-tint, rgba(59,130,246,0.05))' }}>
        <span className="w-1.5 h-1.5 rounded-full bg-brand" />
        <span className="text-[10px] font-medium text-brand uppercase tracking-wider">Planned calls</span>
      </div>
      <table className="w-full text-sm">
        <tbody>
          {calls.map((pc, i) => (
            <tr key={`${pc.service}|${pc.action}|${i}`} className={i < calls.length - 1 ? 'border-b border-border-subtle' : ''}>
              <td className="px-3 py-2 font-mono text-text-primary w-40">{serviceName(pc.service)} · {actionName(pc.action)}</td>
              <td className="px-3 py-2 text-sm text-text-secondary">
                {pc.reason}
                {pc.params && Object.keys(pc.params).length > 0 && (
                  <span className="ml-2 text-xs font-mono text-text-tertiary">
                    ({Object.entries(pc.params).map(([k, v]) =>
                      v === '$chain' ? `${k}=\u27e8from prior call\u27e9` : `${k}=${JSON.stringify(v)}`
                    ).join(', ')})
                  </span>
                )}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

// ── Auto-approval rationale panel ─────────────────────────────────────────────

function AutoApprovalPanel({ rationale }: { rationale: ApprovalRationale }) {
  return (
    <div className="px-4 pb-3">
      <div className="rounded overflow-hidden" style={{ background: 'rgba(96, 165, 250, 0.04)', border: '1px solid rgba(96, 165, 250, 0.15)' }}>
        <div className="px-3 py-1.5 flex items-center gap-1.5" style={{ borderBottom: '1px solid rgba(96, 165, 250, 0.10)' }}>
          <svg className="w-3 h-3 text-blue-400" fill="none" stroke="currentColor" strokeWidth="2.5" viewBox="0 0 24 24"><path d="M5 13l4 4L19 7"/></svg>
          <span className="text-[10px] font-medium uppercase tracking-wider text-blue-400">Auto-Approved via Group Chat</span>
        </div>
        <div className="px-3 py-2.5 space-y-1.5">
          <p className="text-sm text-text-secondary">{rationale.explanation}</p>
          <div className="text-[10px] font-mono text-text-tertiary pt-0.5">
            {rationale.confidence} confidence &middot; {rationale.model} &middot; {rationale.latency_ms}ms
          </div>
        </div>
      </div>
    </div>
  )
}

// ── Risk assessment panel ─────────────────────────────────────────────────────

const RISK_PANEL_COLORS: Record<string, {
  bg: string; border: string; headerBorder: string; color: string; conflictBorder: string
}> = {
  low:      { bg: 'rgba(34, 197, 94, 0.04)', border: 'rgba(34, 197, 94, 0.15)', headerBorder: 'rgba(34, 197, 94, 0.10)', color: 'rgb(var(--color-success))', conflictBorder: 'rgba(34, 197, 94, 0.1)' },
  medium:   { bg: 'rgba(245, 158, 11, 0.05)', border: 'rgba(245, 158, 11, 0.2)', headerBorder: 'rgba(245, 158, 11, 0.12)', color: 'rgb(var(--color-warning))', conflictBorder: 'rgba(245, 158, 11, 0.1)' },
  high:     { bg: 'rgba(249, 115, 22, 0.05)', border: 'rgba(249, 115, 22, 0.2)', headerBorder: 'rgba(249, 115, 22, 0.12)', color: 'rgb(var(--color-risk-orange))', conflictBorder: 'rgba(249, 115, 22, 0.1)' },
  critical: { bg: 'rgba(239, 68, 68, 0.06)', border: 'rgba(239, 68, 68, 0.25)', headerBorder: 'rgba(239, 68, 68, 0.15)', color: 'rgb(var(--color-danger))', conflictBorder: 'rgba(239, 68, 68, 0.1)' },
}

function RiskPanel({ risk, level }: { risk: RiskAssessment; level: string }) {
  const colors = RISK_PANEL_COLORS[level] ?? RISK_PANEL_COLORS.medium
  const hasConflicts = risk.conflicts && risk.conflicts.length > 0
  const hasFactors = risk.factors && risk.factors.length > 0
  const headerLabel = level === 'critical' ? 'Risk Assessment \u2014 Critical' : 'Risk Assessment'

  return (
    <div className="px-4 pb-3">
      <div className="rounded overflow-hidden" style={{ background: colors.bg, border: `1px solid ${colors.border}` }}>
        <div className="px-3 py-1.5 flex items-center gap-1.5" style={{ borderBottom: `1px solid ${colors.headerBorder}` }}>
          {level === 'low'
            ? <svg className="w-3 h-3" style={{ color: colors.color }} fill="none" stroke="currentColor" strokeWidth="2.5" viewBox="0 0 24 24"><path d="M5 13l4 4L19 7"/></svg>
            : <svg className="w-3 h-3" style={{ color: colors.color }} fill="none" stroke="currentColor" strokeWidth="2.5" viewBox="0 0 24 24"><path d="M12 9v2m0 4h.01m-6.938 4h13.856c1.54 0 2.502-1.667 1.732-3L13.732 4c-.77-1.333-2.694-1.333-3.464 0L3.34 16c-.77 1.333.192 3 1.732 3z"/></svg>
          }
          <span className="text-[10px] font-medium uppercase tracking-wider" style={{ color: colors.color }}>{headerLabel}</span>
        </div>
        <div className="px-3 py-2.5 space-y-2">
          <p className="text-sm text-text-secondary">{risk.explanation}</p>

          {/* Conflicts (shown before factors for critical, after for others) */}
          {hasConflicts && level === 'critical' && (
            <div className="space-y-1.5">
              {risk.conflicts.map((c, i) => (
                <div key={i} className="flex items-start gap-2">
                  <svg className="w-3 h-3 shrink-0 mt-0.5" style={{ color: colors.color }} fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24"><path d="M6 18L18 6M6 6l12 12"/></svg>
                  <span className="text-xs text-text-secondary">{c.description}</span>
                </div>
              ))}
            </div>
          )}

          {/* Factors */}
          {hasFactors && (
            <div className="space-y-1" style={hasConflicts && level === 'critical' ? { borderTop: `1px solid ${colors.conflictBorder}`, paddingTop: '0.25rem' } : undefined}>
              {risk.factors.map((f, i) => (
                <div key={i} className="flex items-start gap-2">
                  <span className="text-text-tertiary mt-0.5 text-xs">&bull;</span>
                  <span className="text-xs text-text-secondary">{f}</span>
                </div>
              ))}
            </div>
          )}

          {/* Conflicts (after factors for non-critical) */}
          {hasConflicts && level !== 'critical' && (
            <div className="mt-1 pt-2 space-y-1.5" style={{ borderTop: `1px solid ${colors.conflictBorder}` }}>
              {risk.conflicts.map((c, i) => (
                <div key={i} className="flex items-start gap-2">
                  <svg className="w-3 h-3 shrink-0 mt-0.5" style={{ color: colors.color }} fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24"><path d="M6 18L18 6M6 6l12 12"/></svg>
                  <span className="text-xs text-text-secondary">{c.description}</span>
                </div>
              ))}
            </div>
          )}

          {/* Model & latency metadata */}
          <div className="text-[10px] font-mono text-text-tertiary pt-1">{risk.model} &middot; {risk.latency_ms}ms</div>
        </div>
      </div>
    </div>
  )
}

// ── Activity feed row ────────────────────────────────────────────────────────

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
    ? 'var(--color-danger-tint)'
    : hasProblem ? 'var(--color-warning-tint)' : undefined

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
          <span
            className="text-text-secondary text-xs truncate"
            style={{ maxWidth: 480 }}
            title={entry.reason ?? entry.outcome}
          >
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
