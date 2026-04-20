import { useMemo, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api, type Task, type TaskAction, type AuditEntry, type RiskAssessment, type ApprovalRationale, type ScopeOverride } from '../api/client'
import { format } from 'date-fns'
import { serviceName, actionName } from '../lib/services'
import CountdownTimer from './CountdownTimer'
import VerificationIcon from './VerificationIcon'
import ScopePill, { type ScopePillValue } from './ScopePill'

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
  const [expanded, setExpanded] = useState(false)
  const [activeTab, setActiveTab] = useState<'activity' | 'scopes'>(
    task.request_count === 0 ? 'scopes' : 'activity'
  )
  const [scopesOpenInExpansion, setScopesOpenInExpansion] = useState(false)
  const [confirmApprove, setConfirmApprove] = useState(false)
  const [openPillKey, setOpenPillKey] = useState<string | null>(null)
  const [scopeOverrides, setScopeOverrides] = useState<Record<string, ScopeOverride>>({})

  const invalidate = () => {
    qc.invalidateQueries({ queryKey: ['tasks'] })
    qc.invalidateQueries({ queryKey: ['overview'] })
    qc.invalidateQueries({ queryKey: ['queue'] })
  }

  const overrideList = Object.values(scopeOverrides)

  const approveMut = useMutation({
    mutationFn: () => api.tasks.approve(task.id, { scopes: overrideList }),
    onSuccess: () => { setResult('Approved'); invalidate() },
  })
  const updateScopeMut = useMutation({
    mutationFn: (scopes: ScopeOverride[]) => api.tasks.updateScope(task.id, scopes),
    onSuccess: () => { invalidate() },
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

  const isPending = approveMut.isPending || denyMut.isPending || expandApproveMut.isPending || expandDenyMut.isPending || revokeMut.isPending
  const needsApproval = task.status === 'pending_approval'
  const needsExpansion = task.status === 'pending_scope_expansion'
  const isActive = task.status === 'active'
  const isStanding = task.lifetime === 'standing'
  const isActionable = needsApproval || needsExpansion
  const activityVisible = !isActionable && expanded && activeTab === 'activity'

  const { data: auditData, isLoading: auditLoading } = useQuery({
    queryKey: ['audit', { task_id: task.id }],
    queryFn: () => api.audit.list({ task_id: task.id, limit: 50 }),
    enabled: activityVisible,
    refetchInterval: (query) =>
      activityVisible && task.request_count !== (query.state.data?.entries?.length ?? 0) ? 1_000 : false,
  })

  const effectiveValue = (a: TaskAction): ScopePillValue => {
    const key = `${a.service}|${a.action}`
    const o = scopeOverrides[key]
    return {
      auto: o?.auto_execute ?? a.auto_execute,
      verification: (o?.verification ?? a.verification ?? 'strict') as 'strict' | 'lenient' | 'off',
    }
  }

  const effectiveAuto = (a: TaskAction) => effectiveValue(a).auto
  const autoActions = task.authorized_actions.filter(effectiveAuto)
  const manualActions = task.authorized_actions.filter(a => !effectiveAuto(a))

  const groupedByService = useMemo(() => {
    const groups = new Map<string, { service: string; actions: TaskAction[] }>()
    for (const a of task.authorized_actions) {
      const g = groups.get(a.service) ?? { service: a.service, actions: [] }
      g.actions.push(a)
      groups.set(a.service, g)
    }
    return [...groups.values()]
  }, [task.authorized_actions])

  const handleScopeChange = (a: TaskAction, next: ScopePillValue) => {
    const key = `${a.service}|${a.action}`
    const patch: ScopeOverride = { service: a.service, action: a.action }
    if (next.auto !== a.auto_execute) patch.auto_execute = next.auto
    if (next.verification !== (a.verification ?? 'strict')) patch.verification = next.verification
    const nextMap = { ...scopeOverrides }
    if (patch.auto_execute === undefined && patch.verification === undefined) {
      delete nextMap[key]
    } else {
      nextMap[key] = patch
    }
    setScopeOverrides(nextMap)
    if (isActive) {
      updateScopeMut.mutate([{
        service: a.service,
        action: a.action,
        verification: next.verification,
        auto_execute: next.auto,
      }])
    }
  }

  const auditEntries = auditData?.entries ?? []
  const badge = STATUS_BADGE[task.status] ?? { bg: 'bg-surface-2', text: 'text-text-tertiary', label: task.status }
  const dotColor = STATUS_DOT[task.status] ?? 'bg-text-tertiary'
  const riskLevel = task.risk_level ?? ''
  const riskDetails = task.risk_details
  const hasRisk = riskLevel !== '' && riskLevel !== 'unknown'
  const isHighRisk = riskLevel === 'high' || riskLevel === 'critical'
  const leftBorder = (isActionable && riskLevel === 'critical')
    ? 'border-l-danger'
    : (LEFT_BORDER[task.status] ?? 'border-l-transparent')

  const showRisk = riskDetails && hasRisk
  const showRationale = task.approval_source === 'telegram_group' && task.approval_rationale

  return (
    <div className={`bg-surface-1 border border-border-default rounded-md border-l-[3px] ${leftBorder} overflow-hidden`}>

      {/* ── Compact row for non-actionable cards ── */}
      {!isActionable && (
        <div
          className="px-5 py-3.5 flex items-center gap-3 cursor-pointer hover:bg-white/[0.015] select-none"
          onClick={() => setExpanded(e => !e)}
        >
          <span className={`w-2 h-2 rounded-full shrink-0 ${dotColor}`} />
          <span className={`text-text-primary text-sm flex-1 ${expanded ? '' : 'truncate'}`}>{task.purpose}</span>
          <span className="font-mono text-[11px] text-text-tertiary shrink-0">{agentName}</span>
          <span className="text-text-tertiary text-[11px]">&middot;</span>
          <span className="font-mono text-[11px] text-text-tertiary shrink-0">
            {isActive
              ? (isStanding
                  ? 'ongoing'
                  : (task.expires_at ? <CountdownTimer expiresAt={task.expires_at} /> : 'session'))
              : badge.label}
          </span>
          <svg className={`w-3.5 h-3.5 text-text-tertiary transition-transform shrink-0 ${expanded ? 'rotate-90' : ''}`} fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24"><path d="M9 5l7 7-7 7"/></svg>
        </div>
      )}

      {/* ── Full header for actionable cards ── */}
      {isActionable && (
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
              {!isStanding && task.expires_in_seconds > 0 && ` · ${Math.round(task.expires_in_seconds / 60)}m`}
            </span>
          </div>
        </div>
      )}

      {/* ── Result banner (actionable cards only — non-actionable hides body when collapsed) ── */}
      {result && isActionable && (
        <div className="px-5 pb-3">
          <div className="p-2 bg-surface-2 rounded text-sm text-text-tertiary">{result}</div>
        </div>
      )}

      {/* ── Pending approval body ── */}
      {needsApproval && !result && (
        <>
          {showRisk && <RiskPanel risk={riskDetails} level={riskLevel} />}
          {showRationale && <AutoApprovalPanel rationale={task.approval_rationale!} />}
          <div className="px-4 pb-3">
            <GroupedScopes
              groups={groupedByService}
              effectiveValue={effectiveValue}
              openPillKey={openPillKey}
              setOpenPillKey={setOpenPillKey}
              onChange={handleScopeChange}
              disabled={isPending}
            />
          </div>
          <div className="px-4 py-3 border-t border-border-subtle flex items-center justify-end gap-2">
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
        </>
      )}

      {/* ── Pending scope expansion body ── */}
      {needsExpansion && !result && (
        <>
          {showRisk && <RiskPanel risk={riskDetails} level={riskLevel} />}
          <div className="px-5 pb-3">
            <button
              onClick={() => setScopesOpenInExpansion(o => !o)}
              className="flex items-center gap-1.5 text-xs text-text-tertiary hover:text-text-secondary"
            >
              <svg className={`w-3 h-3 transition-transform ${scopesOpenInExpansion ? 'rotate-90' : ''}`} fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24"><path d="M9 5l7 7-7 7"/></svg>
              <span className="font-medium">Approved scopes</span>
            </button>
          </div>
          {scopesOpenInExpansion && (
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
        </>
      )}

      {/* ── Non-actionable expanded body (animated open via grid-rows) ── */}
      {!isActionable && (
      <div className={`grid transition-[grid-template-rows] duration-300 ease-out ${expanded ? 'grid-rows-[1fr]' : 'grid-rows-[0fr]'}`}>
      <div className="overflow-hidden min-h-0">
        <>
          {result && (
            <div className="px-5 pb-3">
              <div className="p-2 bg-surface-2 rounded text-sm text-text-tertiary">{result}</div>
            </div>
          )}

          <div className="border-t border-border-subtle flex items-stretch px-2">
            <button
              onClick={() => setActiveTab('activity')}
              className={`px-3 py-2.5 text-[12.5px] border-b-2 -mb-px transition-colors ${
                activeTab === 'activity'
                  ? 'text-text-primary border-brand'
                  : 'text-text-tertiary border-transparent hover:text-text-secondary'
              }`}
            >
              Activity
            </button>
            <button
              onClick={() => setActiveTab('scopes')}
              className={`px-3 py-2.5 text-[12.5px] border-b-2 -mb-px transition-colors ${
                activeTab === 'scopes'
                  ? 'text-text-primary border-brand'
                  : 'text-text-tertiary border-transparent hover:text-text-secondary'
              }`}
            >
              Scopes
            </button>
          </div>

          {activeTab === 'activity' && (
            <div className="divide-y divide-border-subtle text-sm">
              {auditLoading && <div className="px-4 py-2 text-xs text-text-tertiary">Loading...</div>}
              {!auditLoading && auditEntries.length === 0 && (
                <div className="px-4 py-2 text-xs text-text-tertiary">No actions recorded yet.</div>
              )}
              {auditEntries.map(e => <ActivityRow key={e.id} entry={e} />)}
            </div>
          )}

          {activeTab === 'scopes' && (
            <div className="pt-3">
              {showRisk && <RiskPanel risk={riskDetails} level={riskLevel} />}
              {showRationale && <AutoApprovalPanel rationale={task.approval_rationale!} />}
              <div className="px-4 pb-3">
                <GroupedScopes
                  groups={groupedByService}
                  effectiveValue={effectiveValue}
                  openPillKey={openPillKey}
                  setOpenPillKey={setOpenPillKey}
                  onChange={handleScopeChange}
                  disabled={!isActive || isPending}
                />
              </div>
            </div>
          )}

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
        </>
      </div>
      </div>
      )}
    </div>
  )
}

// ── Grouped scopes with inline ScopePill ─────────────────────────────────────

function GroupedScopes({
  groups,
  effectiveValue,
  openPillKey,
  setOpenPillKey,
  onChange,
  disabled,
}: {
  groups: { service: string; actions: TaskAction[] }[]
  effectiveValue: (a: TaskAction) => ScopePillValue
  openPillKey: string | null
  setOpenPillKey: (k: string | null) => void
  onChange: (a: TaskAction, v: ScopePillValue) => void
  disabled: boolean
}) {
  return (
    <div className="space-y-4">
      {groups.map(g => (
        <div key={g.service}>
          <div className="pb-2 flex items-baseline gap-2">
            <span className="text-[15px] font-medium tracking-tight text-text-primary">{serviceName(g.service)}</span>
          </div>
          <div className="border-y border-border-subtle">
            {g.actions.map((a, i) => {
              const key = `${a.service}|${a.action}`
              return (
                <div
                  key={key}
                  className={`py-2 pl-4 pr-1 flex items-center justify-between gap-3 ${i > 0 ? 'border-t border-border-subtle' : ''}`}
                >
                  <div className="min-w-0">
                    <div className="font-mono text-[12px] text-text-primary">{actionName(a.action, a.service)}</div>
                    {a.expected_use && (
                      <div className="text-[12px] text-text-secondary mt-0.5">{a.expected_use}</div>
                    )}
                  </div>
                  <ScopePill
                    value={effectiveValue(a)}
                    expanded={openPillKey === key}
                    onExpand={() => setOpenPillKey(key)}
                    onCollapse={() => setOpenPillKey(null)}
                    onChange={(v) => onChange(a, v)}
                    disabled={disabled}
                  />
                </div>
              )
            })}
          </div>
        </div>
      ))}
    </div>
  )
}

// ── Scope group tables (used only for needsExpansion approved-scopes view) ───

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

  return (
    <div className="px-4 pb-3">
      <div className="rounded overflow-hidden" style={{ background: colors.bg, border: `1px solid ${colors.border}` }}>
        <div className="px-3 py-1.5 flex items-center gap-1.5" style={{ borderBottom: `1px solid ${colors.headerBorder}` }}>
          {level === 'low'
            ? <svg className="w-3 h-3" style={{ color: colors.color }} fill="none" stroke="currentColor" strokeWidth="2.5" viewBox="0 0 24 24"><path d="M5 13l4 4L19 7"/></svg>
            : <svg className="w-3 h-3" style={{ color: colors.color }} fill="none" stroke="currentColor" strokeWidth="2.5" viewBox="0 0 24 24"><path d="M12 9v2m0 4h.01m-6.938 4h13.856c1.54 0 2.502-1.667 1.732-3L13.732 4c-.77-1.333-2.694-1.333-3.464 0L3.34 16c-.77 1.333.192 3 1.732 3z"/></svg>
          }
          <span className="text-[10px] font-medium uppercase tracking-wider" style={{ color: colors.color }}>Risk assessment &middot; {level}</span>
        </div>
        <div className="px-3 py-2.5 space-y-2">
          <p className="text-sm text-text-secondary">{risk.explanation}</p>

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

      {expanded && (
        <div className="px-4 pb-3 pt-1 space-y-2">
          {entry.verification && (
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
          )}
          {entry.error_msg && (
            <div className="ml-3 pl-3 border-l-2 border-danger space-y-1">
              <div className="text-[10px] font-mono font-medium text-danger">error</div>
              <pre className="text-xs text-danger whitespace-pre-wrap break-words font-mono max-h-48 overflow-auto">{entry.error_msg}</pre>
            </div>
          )}
          {!entry.verification && !entry.error_msg && (
            <div className="ml-3 pl-3 border-l-2 border-border-default space-y-1.5">
              {entry.reason && <p className="text-xs text-text-secondary">{entry.reason}</p>}
              <div className="text-[10px] font-mono text-text-tertiary">{entry.duration_ms}ms</div>
            </div>
          )}
          <ParamsTable params={entry.params_safe} />
        </div>
      )}
    </div>
  )
}
