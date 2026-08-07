import type { ApprovalRecord, RuntimeEvent } from '../api/client'

export type RuntimeExplanation = {
  title: string
  summary: string
  reason?: string
  decision?: string
  toolName?: string
  agentName?: string
  target?: string
  scope?: string
  risk?: string
  identifiers: Array<{ label: string; value: string }>
  nextStep?: string
}

type ExplanationSource = ApprovalRecord | RuntimeEvent | null | undefined

const FALLBACK_SUMMARY = 'Clawvisor paused this request because it required review, but this event does not include a detailed policy reason.'
const SECRET_KEY_RE = /(token|secret|password|credential|authorization|api[_-]?key|bearer|vault|nonce)/i

export function parseExplanation(data: ExplanationSource): RuntimeExplanation {
  const explanation: RuntimeExplanation = {
    title: 'Needs Review',
    summary: FALLBACK_SUMMARY,
    identifiers: [],
  }

  if (!data) return explanation

  const payload = isApprovalRecord(data) ? safeObject(data.payload_json) : {}
  const summary = isApprovalRecord(data) ? safeObject(data.summary_json) : {}
  const metadata = isRuntimeEvent(data) ? safeObject(data.metadata_json) : {}

  const decision = firstString(
    read(data, 'decision'),
    read(data, 'status'),
    read(data, 'outcome'),
    read(payload, 'decision'),
    read(payload, 'status'),
    read(summary, 'decision'),
    read(summary, 'status'),
    read(metadata, 'decision'),
    read(metadata, 'outcome'),
  )
  explanation.decision = decision
  applyDecision(explanation, decision)

  const reason = firstString(
    read(data, 'reason'),
    read(payload, 'reason'),
    read(payload, 'policy_reason'),
    read(payload, 'decision_reason'),
    read(summary, 'reason'),
    read(summary, 'policy_reason'),
    read(summary, 'decision_reason'),
    read(metadata, 'reason'),
    read(metadata, 'policy_reason'),
    read(metadata, 'decision_reason'),
    read(metadata, 'original_reason'),
  )
  if (reason) {
    explanation.reason = reason
    explanation.summary = friendlyReason(reason)
  }

  explanation.toolName = firstString(
    read(data, 'tool_name'),
    read(payload, 'tool_name'),
    read(summary, 'tool_name'),
    read(metadata, 'tool_name'),
    read(data, 'action_kind') === 'tool_use' ? read(data, 'event_type') : undefined,
  )
  explanation.agentName = firstString(read(data, 'agent_name'), read(data, 'agent_id'))

  const target = buildTarget(safeObject(data), payload, metadata)
  explanation.target = target ?? firstString(read(data, 'target'), read(payload, 'target'), read(summary, 'target'), read(metadata, 'target'))

  const taskID = firstString(read(data, 'matched_task_id'), read(data, 'task_id'), read(payload, 'task_id'), read(summary, 'task_id'), read(metadata, 'task_id'))
  if (taskID) explanation.scope = `Task ${shortID(taskID)}`
  else if (reason && /no matching task|outside.*task|scope/i.test(reason)) explanation.scope = 'No matching approved task'

  explanation.risk = firstString(read(data, 'risk_level'), read(payload, 'risk_level'), read(summary, 'risk_level'), read(metadata, 'risk_level'))

  addIdentifier(explanation, 'Approval ID', firstString(read(data, 'approval_id'), isApprovalRecord(data) ? data.id : undefined))
  addIdentifier(explanation, 'Task ID', taskID)
  addIdentifier(explanation, 'Session ID', firstString(read(data, 'session_id'), read(payload, 'session_id'), read(metadata, 'session_id')))
  addIdentifier(explanation, 'Request ID', isApprovalRecord(data) ? data.request_id : undefined)
  addIdentifier(explanation, 'Tool Use ID', isRuntimeEvent(data) ? data.tool_use_id : firstString(read(payload, 'tool_use_id'), read(metadata, 'tool_use_id')))
  addIdentifier(explanation, 'Policy Rule', safeDetailString(metadata, 'policy_rule') ?? safeDetailString(payload, 'policy_rule') ?? safeDetailString(summary, 'policy_rule'))

  if (!explanation.nextStep) {
    explanation.nextStep = defaultNextStep(explanation)
  }

  return explanation
}

function applyDecision(explanation: RuntimeExplanation, rawDecision?: string) {
  const decision = rawDecision?.toLowerCase()
  if (!decision) return

  if (decision === 'block' || decision === 'blocked' || decision === 'deny' || decision === 'denied') {
    explanation.title = 'Action Blocked'
    explanation.summary = 'Clawvisor blocked this action before the agent could run it.'
    explanation.nextStep = 'Adjust the task scope or policy only if this action is expected for the work.'
    return
  }
  if (decision === 'restricted') {
    explanation.title = 'Action Restricted'
    explanation.summary = 'This action did not pass the configured runtime restrictions.'
    explanation.nextStep = 'Review the restriction before changing policy. Restrictions are intended to be hard stops.'
    return
  }
  if (decision === 'needs_approval' || decision === 'hold' || decision === 'held' || decision === 'pending' || decision === 'review' || decision === 'review_required') {
    explanation.title = 'Approval Required'
    explanation.summary = 'Clawvisor paused this action so a person can review it first.'
    explanation.nextStep = 'Approve once, deny, or create a task that covers this tool if it should be repeatable.'
  }
}

function friendlyReason(reason: string): string {
  const normalized = reason.toLowerCase()
  if (/no matching task|outside.*task|scope/.test(normalized)) {
    return 'The agent tried to use a tool outside an approved task.'
  }
  if (/credential|secret|vault|placeholder|autovault/.test(normalized)) {
    return 'The action touches credentials or vaulted secrets, so Clawvisor paused it for review.'
  }
  if (/private network|loopback|ssrf|self host|self-host/.test(normalized)) {
    return 'The request targets a protected or private network boundary.'
  }
  if (/policy|rule|deny|blocked/.test(normalized)) {
    return 'A runtime policy rule matched this action.'
  }
  if (/risk|high risk|critical/.test(normalized)) {
    return 'This action was flagged as risky enough to require manual review.'
  }
  return reason
}

function defaultNextStep(explanation: RuntimeExplanation): string | undefined {
  const title = explanation.title.toLowerCase()
  if (title.includes('blocked') || title.includes('restricted')) {
    return 'Only change policy if this action is expected and safe for the agent.'
  }
  if (title.includes('review') || title.includes('approval')) {
    return 'Approve once, deny, or create a task that covers this tool if it should be repeatable.'
  }
  return undefined
}

function buildTarget(...sources: Record<string, unknown>[]): string | undefined {
  const host = firstString(...sources.flatMap(source => [read(source, 'host'), read(source, 'target_host')]))
  const path = firstString(...sources.flatMap(source => [read(source, 'path'), read(source, 'target_path')]))
  const method = firstString(...sources.map(source => read(source, 'method')))
  if (!host && !path && !method) return undefined
  return truncateMiddle([method?.toUpperCase(), host && path ? `${host}${path}` : host || path].filter(Boolean).join(' '), 96)
}

function isApprovalRecord(value: ExplanationSource): value is ApprovalRecord {
  return !!value && 'kind' in value && 'surface' in value
}

function isRuntimeEvent(value: ExplanationSource): value is RuntimeEvent {
  return !!value && 'event_type' in value && 'timestamp' in value
}

function safeObject(value: unknown): Record<string, unknown> {
  return value && typeof value === 'object' && !Array.isArray(value) ? value as Record<string, unknown> : {}
}

function read(source: unknown, key: string): unknown {
  if (!source || typeof source !== 'object') return undefined
  return (source as Record<string, unknown>)[key]
}

function firstString(...values: unknown[]): string | undefined {
  for (const value of values) {
    if (typeof value === 'string' && value.trim() !== '') return value.trim()
  }
  return undefined
}

function safeDetailString(source: Record<string, unknown>, key: string): string | undefined {
  if (SECRET_KEY_RE.test(key)) return undefined
  const value = source[key]
  if (typeof value !== 'string' || value.trim() === '') return undefined
  if (looksSensitive(value)) return undefined
  return value.trim()
}

function looksSensitive(value: string): boolean {
  return /(bearer\s+|sk-[a-z0-9]|ghp_|github_pat_|xox[baprs]-|autovault_|cv-nonce|cvis_)/i.test(value)
}

function addIdentifier(explanation: RuntimeExplanation, label: string, value?: string) {
  if (!value || looksSensitive(value)) return
  explanation.identifiers.push({ label, value })
}

function shortID(value: string): string {
  if (value.length <= 12) return value
  return `${value.slice(0, 8)}...`
}

function truncateMiddle(value: string, maxLength: number): string {
  if (value.length <= maxLength) return value
  const keep = Math.floor((maxLength - 3) / 2)
  return `${value.slice(0, keep)}...${value.slice(-keep)}`
}
