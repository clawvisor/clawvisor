import { useCallback, useMemo, useState } from 'react'
import { Link, useSearchParams } from 'react-router-dom'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api, type ActivityMute, type Agent, type AuditEntry } from '../api/client'
import { useAuth } from '../hooks/useAuth'
import { formatDistanceToNow, format } from 'date-fns'
import { actionName, formatServiceAction, serviceName } from '../lib/services'
import type { RuleDraft } from './Runtime'

function escapeCsvField(value: string): string {
  if (value.includes(',') || value.includes('"') || value.includes('\n')) {
    return `"${value.replace(/"/g, '""')}"`
  }
  return value
}

function entriesToCsv(entries: AuditEntry[]): string {
  const headers = ['Timestamp', 'Service', 'Action', 'Decision', 'Outcome', 'Duration (ms)', 'Reason', 'Data Origin', 'Error', 'Policy', 'Safety Flagged', 'Safety Reason', 'Request ID']
  const rows = entries.map(e => [
    e.timestamp,
    serviceName(e.service),
    actionName(e.action),
    e.decision,
    e.outcome,
    String(e.duration_ms),
    e.reason ?? '',
    e.data_origin ?? '',
    e.error_msg ?? '',
    e.policy_id ?? '',
    e.safety_flagged ? 'yes' : 'no',
    e.safety_reason ?? '',
    e.request_id,
  ].map(escapeCsvField))
  return [headers.join(','), ...rows.map(r => r.join(','))].join('\n')
}

function downloadCsv(csv: string, filename: string) {
  const blob = new Blob([csv], { type: 'text/csv;charset=utf-8;' })
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = filename
  a.click()
  URL.revokeObjectURL(url)
}

const OUTCOMES = ['', 'executed', 'blocked', 'restricted', 'pending', 'denied', 'error', 'timeout']
const ACTIVITY_TYPES = [
  { value: '', label: 'All activity types' },
  { value: 'runtime_egress', label: 'Runtime egress' },
  { value: 'runtime_tool_use', label: 'Runtime tool use' },
  { value: 'runtime', label: 'Other runtime activity' },
  { value: 'service', label: 'Service activity' },
] as const

const OUTCOME_STYLE: Record<string, string> = {
  executed: 'bg-success/15 text-success',
  blocked: 'bg-danger/15 text-danger',
  restricted: 'bg-warning/15 text-warning',
  pending: 'bg-warning/15 text-warning',
  denied: 'bg-surface-2 text-text-tertiary',
  error: 'bg-danger/15 text-danger',
  timeout: 'bg-surface-2 text-text-tertiary',
}

type ActivityTypeFilter = '' | 'runtime_egress' | 'runtime_tool_use' | 'runtime' | 'service'
type DisplayMode = Exclude<ActivityTypeFilter, ''> | 'default'
type RuleAction = 'allow' | 'review' | 'deny'
type EgressMatchMode = 'exact' | 'host'
type ToolMatchMode = 'tool' | 'target'
type EgressMethodMode = 'this' | 'any'
type MuteMatchMode = 'host' | 'path'

function OutcomeBadge({ outcome }: { outcome: string }) {
  return (
    <span className={`inline-block px-2 py-0.5 rounded text-xs font-medium ${OUTCOME_STYLE[outcome] ?? 'bg-surface-2 text-text-tertiary'}`}>
      {outcome}
    </span>
  )
}

function compactID(value?: string) {
  if (!value) return null
  return value.length > 12 ? `${value.slice(0, 8)}…${value.slice(-4)}` : value
}

function readString(value: unknown): string {
  return typeof value === 'string' ? value : ''
}

function normalizeHttpVerb(value: string): string {
  const trimmed = value.trim()
  if (!trimmed) return ''
  const upper = trimmed.toUpperCase()
  const known = new Set(['GET', 'POST', 'PUT', 'PATCH', 'DELETE', 'HEAD', 'OPTIONS'])
  return known.has(upper) ? upper : ''
}

function runtimeEgressDetails(entry: AuditEntry): { method: string; host: string; path: string; summary: string } {
  const params = entry.params_safe ?? {}
  const method = normalizeHttpVerb(readString(params.method)) || normalizeHttpVerb(entry.action)
  const host = readString(params.host)
  const path = readString(params.path)
  const summary = host ? `${[method, `${host}${path}`].filter(Boolean).join(' ')}` : 'Runtime egress'
  return { method, host, path, summary }
}

function runtimeToolDetails(entry: AuditEntry): { toolName: string; target: string; summary: string } {
  const params = entry.params_safe ?? {}
  const toolName = readString(params.tool_name) || actionName(entry.action)
  const toolInput = params.tool_input && typeof params.tool_input === 'object' ? params.tool_input as Record<string, unknown> : {}
  const target = readString(toolInput.file_path)
    || readString(toolInput.path)
    || readString(toolInput.directory)
    || readString(toolInput.pattern)
    || readString(toolInput.command)
  return {
    toolName,
    target,
    summary: target ? `${toolName} ${target}` : toolName,
  }
}

function activitySummary(entry: AuditEntry): string {
  if (entry.summary_text) return entry.summary_text
  if (entry.service === 'runtime.egress') return runtimeEgressDetails(entry).summary
  if (entry.service === 'runtime.tool_use') return runtimeToolDetails(entry).summary
  return `${serviceName(entry.service)} ${actionName(entry.action)}`.trim()
}

function activityLabel(entry: AuditEntry): string {
  if (entry.service === 'runtime.egress') return 'Runtime egress'
  if (entry.service === 'runtime.tool_use') return 'Runtime tool use'
  if (entry.service.startsWith('runtime.')) return `${serviceName(entry.service)} event`
  return formatServiceAction(entry.service, entry.action)
}

function activityType(entry: AuditEntry): ActivityTypeFilter {
  if (entry.service === 'runtime.egress') return 'runtime_egress'
  if (entry.service === 'runtime.tool_use') return 'runtime_tool_use'
  if (entry.service.startsWith('runtime.')) return 'runtime'
  return 'service'
}

function matchesActivityType(entry: AuditEntry, filter: ActivityTypeFilter): boolean {
  if (!filter) return true
  return activityType(entry) === filter
}

function displayMode(filter: ActivityTypeFilter): DisplayMode {
  return filter || 'default'
}

function escapeRegex(value: string): string {
  return value.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
}

function Segments<T extends string>({
  value,
  onChange,
  options,
}: {
  value: T
  onChange: (value: T) => void
  options: Array<{ value: T; label: string }>
}) {
  return (
    <div className="flex flex-wrap gap-2" role="radiogroup">
      {options.map(option => {
        const active = value === option.value
        return (
          <button
            key={option.value}
            type="button"
            role="radio"
            aria-checked={active}
            onClick={() => onChange(option.value)}
            className={`rounded-full border px-3 py-1.5 text-sm transition-colors ${
              active
                ? 'border-brand bg-brand/10 text-brand'
                : 'border-border-default bg-surface-0 text-text-secondary hover:bg-surface-2'
            }`}
          >
            {option.label}
          </button>
        )
      })}
    </div>
  )
}

function CreateRuleModal({
  entry,
  agentName,
  onCancel,
  onCreateRuntimeRule,
  onCreateRestriction,
  runtimeBusy,
  restrictionBusy,
}: {
  entry: AuditEntry
  agentName?: string
  onCancel: () => void
  onCreateRuntimeRule: (rule: RuleDraft) => void
  onCreateRestriction: (args: { service: string; action: string; reason: string }) => void
  runtimeBusy: boolean
  restrictionBusy: boolean
}) {
  const entryType = activityType(entry)
  const isRuntimeEgress = entryType === 'runtime_egress'
  const isRuntimeTool = entryType === 'runtime_tool_use'
  const isRuntimeRule = isRuntimeEgress || isRuntimeTool
  const egress = runtimeEgressDetails(entry)
  const tool = runtimeToolDetails(entry)
  const [action, setAction] = useState<RuleAction>('allow')
  const [scope, setScope] = useState<'global' | 'agent'>(entry.agent_id ? 'agent' : 'global')
  const [egressMatch, setEgressMatch] = useState<EgressMatchMode>('exact')
  const [egressMethod, setEgressMethod] = useState<EgressMethodMode>('this')
  const [toolMatch, setToolMatch] = useState<ToolMatchMode>(tool.target ? 'target' : 'tool')
  const [reason, setReason] = useState(entry.reason ?? '')

  const saveRuntimeRule = () => {
    if (isRuntimeEgress) {
      onCreateRuntimeRule({
        scope,
        agent_id: scope === 'agent' ? entry.agent_id : undefined,
        kind: 'egress',
        action,
        host: egress.host,
        method: egressMethod === 'this' ? egress.method : '',
        path: egressMatch === 'exact' ? egress.path : '',
        reason,
        enabled: true,
        source: 'user',
      })
      return
    }
    if (isRuntimeTool) {
      onCreateRuntimeRule({
        scope,
        agent_id: scope === 'agent' ? entry.agent_id : undefined,
        kind: 'tool',
        action,
        tool_name: tool.toolName,
        input_regex: toolMatch === 'target' && tool.target ? escapeRegex(tool.target) : '',
        reason,
        enabled: true,
        source: 'user',
      })
    }
  }

  const saveRestriction = () => {
    onCreateRestriction({
      service: entry.service,
      action: entry.action,
      reason,
    })
  }

  const preview = isRuntimeEgress
    ? `${action} ${egressMethod === 'this' && egress.method ? `${egress.method} ` : ''}${egress.host}${egressMatch === 'exact' ? egress.path : ''}`.trim()
    : isRuntimeTool
      ? `${action} ${tool.toolName}${toolMatch === 'target' && tool.target ? ` matching ${tool.target}` : ''}`.trim()
      : `Block ${entry.service} ${entry.action}`.trim()
  const busy = isRuntimeRule ? runtimeBusy : restrictionBusy

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-surface-0/70 p-4">
      <div className="w-full max-w-3xl rounded-lg border border-border-default bg-surface-1 p-5 shadow-xl space-y-5">
        <div className="flex items-start justify-between gap-3">
          <div>
            <h2 className="text-lg font-semibold text-text-primary">Create policy rule</h2>
            <p className="text-sm text-text-tertiary mt-1">
              Start from the observed activity, then choose the narrowest rule shape that removes the noise.
            </p>
          </div>
          <button onClick={onCancel} className="rounded border border-border-default px-3 py-2 text-sm text-text-secondary hover:bg-surface-2">
            Cancel
          </button>
        </div>

        <div className="grid gap-3 md:grid-cols-2">
          <div className="rounded border border-border-subtle bg-surface-0 p-3">
            <div className="text-xs uppercase tracking-wider text-text-tertiary">Observed activity</div>
            <div className="mt-1 text-sm font-medium text-text-primary">{activitySummary(entry)}</div>
            <div className="mt-1 text-xs text-text-tertiary">{formatServiceAction(entry.service, entry.action)}</div>
          </div>
          <div className="rounded border border-border-subtle bg-surface-0 p-3">
            <div className="text-xs uppercase tracking-wider text-text-tertiary">Agent scope</div>
            <div className="mt-1 text-sm font-medium text-text-primary">{entry.agent_id ? (agentName ?? compactID(entry.agent_id) ?? 'Agent') : 'Global / unscoped activity'}</div>
            {entry.agent_id && <div className="mt-1 text-xs text-text-tertiary">{compactID(entry.agent_id)}</div>}
          </div>
        </div>

        {isRuntimeRule && (
          <>
            <div className="space-y-2">
              <label className="text-sm font-medium text-text-primary">Rule action</label>
              <Segments<RuleAction>
                value={action}
                onChange={setAction}
                options={[
                  { value: 'allow', label: 'Allow' },
                  { value: 'review', label: 'Review' },
                  { value: 'deny', label: 'Deny' },
                ]}
              />
            </div>

            {entry.agent_id && (
              <div className="space-y-2">
                <label className="text-sm font-medium text-text-primary">Scope</label>
                <Segments<'global' | 'agent'>
                  value={scope}
                  onChange={setScope}
                  options={[
                    { value: 'agent', label: `This agent (${agentName ?? 'selected agent'})` },
                    { value: 'global', label: 'All agents' },
                  ]}
                />
              </div>
            )}

            {isRuntimeEgress && (
              <>
                <div className="space-y-2">
                  <label className="text-sm font-medium text-text-primary">Verb match</label>
                  <Segments<EgressMethodMode>
                    value={egressMethod}
                    onChange={setEgressMethod}
                    options={[
                      { value: 'this', label: egress.method || 'This verb' },
                      { value: 'any', label: 'All verbs' },
                    ]}
                  />
                </div>
                <div className="space-y-2">
                  <label className="text-sm font-medium text-text-primary">Path match</label>
                  <Segments<EgressMatchMode>
                    value={egressMatch}
                    onChange={setEgressMatch}
                    options={[
                      { value: 'exact', label: 'This endpoint' },
                      { value: 'host', label: 'This host' },
                    ]}
                  />
                </div>
              </>
            )}

            {isRuntimeTool && (
              <div className="space-y-2">
                <label className="text-sm font-medium text-text-primary">Match shape</label>
                <Segments<ToolMatchMode>
                  value={toolMatch}
                  onChange={setToolMatch}
                  options={tool.target
                    ? [
                        { value: 'target', label: 'This tool + target' },
                        { value: 'tool', label: 'This tool' },
                      ]
                    : [
                        { value: 'tool', label: 'This tool' },
                      ]}
                />
              </div>
            )}
          </>
        )}

        {!isRuntimeRule && (
          <div className="rounded border border-border-subtle bg-surface-0 p-3 text-sm text-text-secondary">
            This activity currently creates a service restriction entry. Runtime allow / review / deny actions are only available for runtime egress and tool-use rows.
          </div>
        )}

        <textarea
          value={reason}
          onChange={e => setReason(e.target.value)}
          placeholder="Short reason / note"
          className="min-h-[100px] w-full rounded border border-border-default bg-surface-0 px-3 py-2 text-sm text-text-primary"
        />
        <div className="rounded border border-border-subtle bg-surface-0 p-3">
          <div className="text-xs uppercase tracking-wider text-text-tertiary">Preview</div>
          <div className="mt-1 text-sm font-medium text-text-primary">{preview}</div>
        </div>
        <div className="flex justify-end gap-2">
          <button onClick={onCancel} className="rounded border border-border-default px-3 py-2 text-sm text-text-secondary hover:bg-surface-2">
            Cancel
          </button>
          <button
            onClick={isRuntimeRule ? saveRuntimeRule : saveRestriction}
            disabled={busy}
            className="rounded bg-brand px-4 py-2 text-sm font-medium text-surface-0 hover:bg-brand-strong disabled:opacity-50"
          >
            {busy ? 'Creating…' : 'Create rule'}
          </button>
        </div>
      </div>
    </div>
  )
}

function CreateMuteModal({
  entry,
  onCancel,
  onCreate,
  busy,
}: {
  entry: AuditEntry
  onCancel: () => void
  onCreate: (args: { host: string; pathPrefix?: string }) => void
  busy: boolean
}) {
  const egress = runtimeEgressDetails(entry)
  const [mode, setMode] = useState<MuteMatchMode>(egress.path ? 'path' : 'host')

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-surface-0/70 p-4">
      <div className="w-full max-w-2xl rounded-lg border border-border-default bg-surface-1 p-5 shadow-xl space-y-5">
        <div className="flex items-start justify-between gap-3">
          <div>
            <h2 className="text-lg font-semibold text-text-primary">Mute activity noise</h2>
            <p className="text-sm text-text-tertiary mt-1">
              Hide matching runtime egress rows from the Activity feed before they are returned by the backend.
            </p>
          </div>
          <button onClick={onCancel} className="rounded border border-border-default px-3 py-2 text-sm text-text-secondary hover:bg-surface-2">
            Cancel
          </button>
        </div>

        <div className="rounded border border-border-subtle bg-surface-0 p-3">
          <div className="text-xs uppercase tracking-wider text-text-tertiary">Observed request</div>
          <div className="mt-1 text-sm font-medium text-text-primary">{egress.summary}</div>
        </div>

        <div className="space-y-2">
          <label className="text-sm font-medium text-text-primary">Mute scope</label>
          <Segments<MuteMatchMode>
            value={mode}
            onChange={setMode}
            options={egress.path
              ? [
                  { value: 'path', label: 'This path prefix' },
                  { value: 'host', label: 'This host' },
                ]
              : [
                  { value: 'host', label: 'This host' },
                ]}
          />
        </div>

        <div className="rounded border border-border-subtle bg-surface-0 p-3">
          <div className="text-xs uppercase tracking-wider text-text-tertiary">Preview</div>
          <div className="mt-1 text-sm font-medium text-text-primary">
            {mode === 'path' && egress.path
              ? `Mute ${egress.host}${egress.path}*`
              : `Mute ${egress.host}`}
          </div>
        </div>

        <div className="flex justify-end gap-2">
          <button onClick={onCancel} className="rounded border border-border-default px-3 py-2 text-sm text-text-secondary hover:bg-surface-2">
            Cancel
          </button>
          <button
            onClick={() => onCreate({ host: egress.host, pathPrefix: mode === 'path' ? egress.path : '' })}
            disabled={busy || !egress.host}
            className="rounded bg-brand px-4 py-2 text-sm font-medium text-surface-0 hover:bg-brand-strong disabled:opacity-50"
          >
            {busy ? 'Saving…' : 'Mute'}
          </button>
        </div>
      </div>
    </div>
  )
}

function AuditRow({
  entry,
  mode,
  agentName,
  onCreateRule,
  onMute,
}: {
  entry: AuditEntry
  mode: DisplayMode
  agentName?: string
  onCreateRule: (entry: AuditEntry) => void
  onMute: (entry: AuditEntry) => void
}) {
  const [expanded, setExpanded] = useState(false)
  const summary = activitySummary(entry)
  const serviceActionLabel = activityLabel(entry)
  const showAgentColumn = mode !== 'default'
  const isRuntimeEgressMode = mode === 'runtime_egress'
  const isRuntimeToolMode = mode === 'runtime_tool_use'
  const egress = runtimeEgressDetails(entry)
  const tool = runtimeToolDetails(entry)
  const primaryLabel = isRuntimeEgressMode
    ? egress.summary
    : isRuntimeToolMode
      ? tool.summary
      : summary

  return (
    <>
      <tr
        className="border-t border-border-default hover:bg-surface-2 cursor-pointer"
        onClick={() => setExpanded(e => !e)}
      >
        <td className="px-4 py-2 text-xs text-text-tertiary whitespace-nowrap" title={format(new Date(entry.timestamp), 'PPpp')}>
          {formatDistanceToNow(new Date(entry.timestamp), { addSuffix: true })}
        </td>
        <td className="px-4 py-2 text-sm">
          <div className="text-text-primary font-medium">{primaryLabel}</div>
          <div className="text-xs text-text-tertiary mt-0.5">{serviceActionLabel}</div>
        </td>
        {showAgentColumn && (
          <td className="px-4 py-2 text-sm">
            {entry.agent_id ? (
              <div>
                <div className="text-text-primary font-medium">{agentName ?? compactID(entry.agent_id) ?? 'Agent'}</div>
                <div className="text-xs text-text-tertiary mt-0.5">{compactID(entry.agent_id)}</div>
              </div>
            ) : (
              <span className="text-xs text-text-tertiary">No agent</span>
            )}
          </td>
        )}
        <td className="px-4 py-2">
          <span className={`text-xs px-1.5 py-0.5 rounded ${entry.decision === 'block' ? 'bg-danger/10 text-danger' : entry.decision === 'approve' ? 'bg-warning/10 text-warning' : entry.decision === 'verify' ? 'bg-brand/10 text-brand' : 'bg-success/10 text-success'}`}>
            {entry.decision}
          </span>
        </td>
        <td className="px-4 py-2"><OutcomeBadge outcome={entry.outcome} /></td>
        <td className="px-4 py-2 text-xs text-text-tertiary">{entry.duration_ms}ms</td>
        <td className="px-4 py-2">
          <div className="flex flex-wrap gap-2">
            {entry.service === 'runtime.egress' && (
              <button
                onClick={(event) => {
                  event.stopPropagation()
                  onMute(entry)
                }}
                className="rounded border border-border-default px-3 py-1.5 text-xs text-text-secondary hover:bg-surface-0"
              >
                Mute
              </button>
            )}
            <button
              onClick={(event) => {
                event.stopPropagation()
                onCreateRule(entry)
              }}
              className="rounded border border-border-default px-3 py-1.5 text-xs text-text-secondary hover:bg-surface-0"
            >
              Create rule
            </button>
          </div>
        </td>
        <td className="px-4 py-2 text-xs text-text-secondary">{expanded ? '▲' : '▼'}</td>
      </tr>
      {expanded && (
        <tr className="border-t border-border-default bg-surface-2">
          <td colSpan={showAgentColumn ? 8 : 7} className="px-4 py-3">
            <div className="grid grid-cols-1 sm:grid-cols-2 gap-4 text-xs">
              <div>
                <div className="text-text-primary font-medium mb-1">{summary}</div>
                <div className="text-text-tertiary text-[11px] mb-2">{serviceActionLabel}</div>
                <pre className="bg-surface-1 border border-border-default rounded p-2 overflow-auto max-h-48 text-text-secondary">
                  {JSON.stringify(entry.params_safe, null, 2)}
                </pre>
              </div>
              <div className="space-y-2">
                {entry.agent_id && (
                  <div className="bg-surface-1 border border-border-default rounded p-2">
                    <div className="text-text-primary font-medium mb-1">Agent</div>
                    <Link
                      to={`/dashboard/agents/${entry.agent_id}`}
                      className="text-brand hover:underline"
                      onClick={event => event.stopPropagation()}
                    >
                      {agentName ?? compactID(entry.agent_id) ?? entry.agent_id}
                    </Link>
                  </div>
                )}
                {entry.reason && (
                  <div className="bg-brand/10 rounded p-2">
                    <div className="text-brand font-medium mb-0.5">Agent's reason</div>
                    <div className="text-text-secondary">{entry.reason}</div>
                  </div>
                )}
                {entry.data_origin && (
                  <div><span className="text-text-tertiary">Data origin:</span> {entry.data_origin}</div>
                )}
                {entry.error_msg && (
                  <div><span className="text-text-tertiary">Error:</span> <span className="text-danger">{entry.error_msg}</span></div>
                )}
                {entry.policy_id && (
                  <div><span className="text-text-tertiary">Policy:</span> {entry.policy_id}</div>
                )}
                {entry.rule_id && (
                  <div><span className="text-text-tertiary">Rule:</span> {entry.rule_id}</div>
                )}
                {entry.safety_flagged && (
                  <div className="text-warning">Safety flagged{entry.safety_reason ? `: ${entry.safety_reason}` : ''}</div>
                )}
                {entry.verification && (
                  <div className="bg-brand/10 rounded p-2 space-y-1">
                    <div className="text-brand font-medium mb-0.5">Intent verification</div>
                    <div className="flex gap-2 flex-wrap">
                      <span className={`inline-block px-1.5 py-0.5 rounded text-xs font-medium ${
                        entry.verification.param_scope === 'ok' ? 'bg-success/15 text-success' :
                        entry.verification.param_scope === 'violation' ? 'bg-danger/15 text-danger' :
                        'bg-surface-2 text-text-tertiary'
                      }`}>
                        params: {entry.verification.param_scope}
                      </span>
                      <span className={`inline-block px-1.5 py-0.5 rounded text-xs font-medium ${
                        entry.verification.reason_coherence === 'ok' ? 'bg-success/15 text-success' :
                        entry.verification.reason_coherence === 'incoherent' ? 'bg-danger/15 text-danger' :
                        'bg-warning/15 text-warning'
                      }`}>
                        reason: {entry.verification.reason_coherence}
                      </span>
                      {entry.verification.cached && (
                        <span className="inline-block px-1.5 py-0.5 rounded text-xs bg-surface-2 text-text-tertiary">cached</span>
                      )}
                    </div>
                    <div className="text-text-secondary">{entry.verification.explanation}</div>
                    <div className="text-text-tertiary text-[10px]">{entry.verification.model} &middot; {entry.verification.latency_ms}ms</div>
                  </div>
                )}
                {(entry.session_id || entry.approval_id || entry.lease_id || entry.matched_task_id || entry.lease_task_id) && (
                  <div className="bg-surface-1 border border-border-default rounded p-2 space-y-1">
                    <div className="text-text-primary font-medium">Runtime context</div>
                    {entry.session_id && <div><span className="text-text-tertiary">Session:</span> <code className="font-mono">{compactID(entry.session_id)}</code></div>}
                    {entry.approval_id && <div><span className="text-text-tertiary">Approval:</span> <code className="font-mono">{compactID(entry.approval_id)}</code></div>}
                    {entry.lease_id && <div><span className="text-text-tertiary">Lease:</span> <code className="font-mono">{compactID(entry.lease_id)}</code></div>}
                    {entry.matched_task_id && <div><span className="text-text-tertiary">Matched task:</span> <code className="font-mono">{compactID(entry.matched_task_id)}</code></div>}
                    {entry.lease_task_id && <div><span className="text-text-tertiary">Lease task:</span> <code className="font-mono">{compactID(entry.lease_task_id)}</code></div>}
                    {entry.resolution_confidence && <div><span className="text-text-tertiary">Confidence:</span> {entry.resolution_confidence}</div>}
                    {entry.intent_verdict && <div><span className="text-text-tertiary">Intent verdict:</span> {entry.intent_verdict}</div>}
                  </div>
                )}
                {(entry.would_block || entry.would_review || entry.would_prompt_inline) && (
                  <div className="bg-warning/10 rounded p-2 space-y-1">
                    <div className="text-warning font-medium">Observation mode</div>
                    <div className="flex gap-2 flex-wrap">
                      {entry.would_block && <span className="inline-block px-1.5 py-0.5 rounded text-xs font-medium bg-danger/15 text-danger">would block</span>}
                      {entry.would_review && <span className="inline-block px-1.5 py-0.5 rounded text-xs font-medium bg-warning/15 text-warning">would review</span>}
                      {entry.would_prompt_inline && <span className="inline-block px-1.5 py-0.5 rounded text-xs font-medium bg-brand/15 text-brand">would prompt inline</span>}
                    </div>
                    <div className="text-text-tertiary">
                      {entry.used_active_task_context && 'Used active task context. '}
                      {entry.used_lease_bias && 'Used lease bias. '}
                      {entry.used_conv_judge_resolution && 'Used conversation judge resolution.'}
                    </div>
                  </div>
                )}
                <div className="text-text-tertiary font-mono">{entry.request_id}</div>
              </div>
            </div>
          </td>
        </tr>
      )}
    </>
  )
}

const PAGE_SIZE = 50

export default function Activity() {
  const qc = useQueryClient()
  const { currentOrg } = useAuth()
  const orgId = currentOrg?.id
  const [searchParams, setSearchParams] = useSearchParams()
  const [outcomeFilter, setOutcomeFilter] = useState('')
  const [serviceFilter, setServiceFilter] = useState('')
  const [activityTypeFilter, setActivityTypeFilter] = useState<ActivityTypeFilter>('')
  const [offset, setOffset] = useState(0)
  const [ruleModalEntry, setRuleModalEntry] = useState<AuditEntry | null>(null)
  const [muteModalEntry, setMuteModalEntry] = useState<AuditEntry | null>(null)
  const agentFilter = searchParams.get('agent_id') ?? ''

  const filter = {
    outcome: outcomeFilter || undefined,
    service: serviceFilter || undefined,
    agent_id: agentFilter || undefined,
    limit: PAGE_SIZE,
    offset,
  }

  const { data, isLoading, refetch } = useQuery({
    queryKey: ['audit', orgId ?? 'personal', { outcome: outcomeFilter, service: serviceFilter, agent_id: agentFilter, offset }],
    queryFn: () => orgId
      ? api.orgs.audit(orgId, filter)
      : api.audit.list(filter),
    refetchInterval: 30_000,
  })

  const { data: agents = [] } = useQuery({
    queryKey: ['activity-agents', orgId ?? 'personal'],
    queryFn: () => orgId ? api.orgs.agents(orgId) : api.agents.list(),
    staleTime: 60_000,
  })
  const { data: mutedHosts } = useQuery({
    queryKey: ['activity-mutes'],
    queryFn: () => api.audit.listMutes(),
    staleTime: 30_000,
  })

  const agentMap = useMemo(() => new Map(agents.map((agent: Agent) => [agent.id, agent.name])), [agents])
  const mode = displayMode(activityTypeFilter)
  const rawEntries = data?.entries ?? []
  const entries = useMemo(
    () => rawEntries.filter(entry => matchesActivityType(entry, activityTypeFilter)),
    [rawEntries, activityTypeFilter],
  )
  const total = data?.total ?? 0
  const [exporting, setExporting] = useState(false)

  const createRuleMut = useMutation({
    mutationFn: (rule: RuleDraft) => api.runtime.createRule(rule),
    onSuccess: () => {
      setRuleModalEntry(null)
      qc.invalidateQueries({ queryKey: ['runtime-rules'] })
      qc.invalidateQueries({ queryKey: ['audit'] })
    },
  })

  const createRestrictionMut = useMutation<void, Error, { service: string; action: string; reason: string }>({
    mutationFn: async ({ service, action, reason }) => {
      await (
      orgId
        ? api.orgs.restrictions.create(orgId, service, action, reason)
        : api.restrictions.create(service, action, reason)
      )
    },
    onSuccess: () => {
      setRuleModalEntry(null)
      qc.invalidateQueries({ queryKey: ['restrictions'] })
      qc.invalidateQueries({ queryKey: ['audit'] })
    },
  })
  const createMuteMut = useMutation({
    mutationFn: ({ host, pathPrefix }: { host: string; pathPrefix?: string }) => api.audit.createMute(host, pathPrefix),
    onSuccess: () => {
      setMuteModalEntry(null)
      qc.invalidateQueries({ queryKey: ['audit'] })
      qc.invalidateQueries({ queryKey: ['activity-mutes'] })
    },
  })
  const deleteMuteMut = useMutation({
    mutationFn: (id: string) => api.audit.deleteMute(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['audit'] })
      qc.invalidateQueries({ queryKey: ['activity-mutes'] })
    },
  })

  const handleOpenCreateRule = useCallback((entry: AuditEntry) => {
    setRuleModalEntry(entry)
  }, [])

  const handleExport = useCallback(async () => {
    setExporting(true)
    try {
      const allEntries: AuditEntry[] = []
      const batchSize = 200
      let batchOffset = 0
      while (true) {
        const batchFilter = {
          outcome: outcomeFilter || undefined,
          service: serviceFilter || undefined,
          limit: batchSize,
          offset: batchOffset,
        }
        const batch = orgId
          ? await api.orgs.audit(orgId, batchFilter)
          : await api.audit.list(batchFilter)
        allEntries.push(...batch.entries)
        if (allEntries.length >= batch.total || batch.entries.length < batchSize) break
        batchOffset += batchSize
      }
      const filteredExportEntries = allEntries.filter(entry => matchesActivityType(entry, activityTypeFilter))
      const csv = entriesToCsv(filteredExportEntries)
      const dateStr = format(new Date(), 'yyyy-MM-dd')
      downloadCsv(csv, `activity-log-${dateStr}.csv`)
    } finally {
      setExporting(false)
    }
  }, [activityTypeFilter, orgId, outcomeFilter, serviceFilter])

  const showAgentColumn = mode !== 'default'
  const summaryHeading = mode === 'runtime_egress'
    ? 'Runtime egress'
    : mode === 'runtime_tool_use'
      ? 'Runtime tool use'
      : 'Summary'

  return (
    <div className="p-4 sm:p-8 space-y-4">
      <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-2">
        <h1 className="text-2xl font-bold text-text-primary">{orgId ? `${currentOrg!.name} Activity` : 'Activity'}</h1>
        <div className="flex items-center gap-3">
          <button
            onClick={handleExport}
            disabled={exporting || entries.length === 0}
            className="text-sm text-brand hover:underline disabled:opacity-40 disabled:no-underline"
          >
            {exporting ? 'Exporting…' : 'Export CSV'}
          </button>
          <button
            onClick={() => refetch()}
            className="text-sm text-brand hover:underline"
          >
            Refresh
          </button>
        </div>
      </div>

      {ruleModalEntry && (
        <CreateRuleModal
          entry={ruleModalEntry}
          agentName={ruleModalEntry.agent_id ? agentMap.get(ruleModalEntry.agent_id) : undefined}
          onCancel={() => setRuleModalEntry(null)}
          onCreateRuntimeRule={draft => createRuleMut.mutate(draft)}
          onCreateRestriction={args => createRestrictionMut.mutate(args)}
          runtimeBusy={createRuleMut.isPending}
          restrictionBusy={createRestrictionMut.isPending}
        />
      )}

      {muteModalEntry && (
        <CreateMuteModal
          entry={muteModalEntry}
          onCancel={() => setMuteModalEntry(null)}
          onCreate={({ host, pathPrefix }) => createMuteMut.mutate({ host, pathPrefix })}
          busy={createMuteMut.isPending}
        />
      )}

      <div className="flex gap-3 flex-wrap items-center">
        {agentFilter && (
          <div className="rounded-full border border-brand/20 bg-brand/5 px-3 py-1.5 text-xs text-brand">
            Agent filter active
            <button
              onClick={() => {
                const next = new URLSearchParams(searchParams)
                next.delete('agent_id')
                setSearchParams(next, { replace: true })
              }}
              className="ml-2 underline"
            >
              Clear
            </button>
          </div>
        )}
        <select
          value={activityTypeFilter}
          onChange={e => { setActivityTypeFilter(e.target.value as ActivityTypeFilter); setOffset(0) }}
          className="text-sm rounded border border-border-default bg-surface-0 text-text-primary px-2 py-1.5 focus:outline-none focus:ring-1 focus:ring-brand/30 focus:border-brand"
        >
          {ACTIVITY_TYPES.map(option => (
            <option key={option.value} value={option.value}>{option.label}</option>
          ))}
        </select>
        <select
          value={outcomeFilter}
          onChange={e => { setOutcomeFilter(e.target.value); setOffset(0) }}
          className="text-sm rounded border border-border-default bg-surface-0 text-text-primary px-2 py-1.5 focus:outline-none focus:ring-1 focus:ring-brand/30 focus:border-brand"
        >
          <option value="">All outcomes</option>
          {OUTCOMES.filter(Boolean).map(o => (
            <option key={o} value={o}>{o}</option>
          ))}
        </select>
        <input
          value={serviceFilter}
          onChange={e => { setServiceFilter(e.target.value); setOffset(0) }}
          placeholder="Filter by service…"
          className="text-sm rounded border border-border-default bg-surface-0 text-text-primary px-2 py-1.5 focus:outline-none focus:ring-1 focus:ring-brand/30 focus:border-brand placeholder:text-text-tertiary"
        />
      </div>

      {(mutedHosts?.entries?.length ?? 0) > 0 && (
        <div className="rounded-md border border-border-default bg-surface-1 p-4 space-y-3">
          <div>
            <h2 className="text-sm font-semibold text-text-primary">Muted activity hosts</h2>
            <p className="text-xs text-text-tertiary mt-1">
              Matching runtime egress rows are filtered out by the backend before they are returned to this page.
            </p>
          </div>
          <div className="flex flex-wrap gap-2">
            {(mutedHosts?.entries ?? []).map((mute: ActivityMute) => (
              <div key={mute.id} className="inline-flex items-center gap-2 rounded-full border border-border-default bg-surface-0 px-3 py-1.5 text-xs text-text-secondary">
                <span>{mute.host}{mute.path_prefix ? `${mute.path_prefix}*` : ''}</span>
                <button
                  onClick={() => deleteMuteMut.mutate(mute.id)}
                  disabled={deleteMuteMut.isPending}
                  className="text-text-tertiary hover:text-danger"
                >
                  Remove
                </button>
              </div>
            ))}
          </div>
        </div>
      )}

      {isLoading && <div className="text-sm text-text-tertiary">Loading…</div>}

      {!isLoading && entries.length === 0 && (
        <div className="text-sm text-text-tertiary py-8 text-center">
          {outcomeFilter || serviceFilter || activityTypeFilter
            ? 'No entries match your filters.'
            : "No activity yet. Your agent's requests will be logged here."}
        </div>
      )}

      {entries.length > 0 && (
        <div className="bg-surface-1 border border-border-default rounded-md overflow-x-auto">
          <table className="w-full min-w-[720px]">
            <thead className="bg-surface-2 text-xs text-text-tertiary font-medium">
              <tr>
                <th className="px-4 py-2 text-left">Time</th>
                <th className="px-4 py-2 text-left">{summaryHeading}</th>
                {showAgentColumn && <th className="px-4 py-2 text-left">Agent</th>}
                <th className="px-4 py-2 text-left">Authorization</th>
                <th className="px-4 py-2 text-left">Outcome</th>
                <th className="px-4 py-2 text-left">Duration</th>
                <th className="px-4 py-2 text-left">Rule</th>
                <th className="px-4 py-2"></th>
              </tr>
            </thead>
            <tbody>
              {entries.map(entry => (
                <AuditRow
                  key={entry.id}
                  entry={entry}
                  mode={mode}
                  agentName={entry.agent_id ? agentMap.get(entry.agent_id) : undefined}
                  onCreateRule={handleOpenCreateRule}
                  onMute={setMuteModalEntry}
                />
              ))}
            </tbody>
          </table>
        </div>
      )}

      {total > PAGE_SIZE && (
        <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-2 text-sm text-text-tertiary">
          <span>Showing {offset + 1}–{Math.min(offset + PAGE_SIZE, total)} of {total}</span>
          <div className="flex gap-2">
            <button
              disabled={offset === 0}
              onClick={() => setOffset(Math.max(0, offset - PAGE_SIZE))}
              className="px-3 py-1 rounded border border-border-strong disabled:opacity-40 hover:bg-surface-2"
            >
              Previous
            </button>
            <button
              disabled={offset + PAGE_SIZE >= total}
              onClick={() => setOffset(offset + PAGE_SIZE)}
              className="px-3 py-1 rounded border border-border-strong disabled:opacity-40 hover:bg-surface-2"
            >
              Next
            </button>
          </div>
        </div>
      )}
    </div>
  )
}
