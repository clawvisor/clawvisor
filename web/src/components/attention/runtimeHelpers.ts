import type { ApprovalRecord } from '../../api/client'
import { serviceName, actionName } from '../../lib/services'

export function runtimeSummary(approval: ApprovalRecord): Record<string, any> {
  return approval.summary_json ?? {}
}

export function runtimePayload(approval: ApprovalRecord): Record<string, any> | null {
  return approval.payload_json ?? null
}

export function runtimeApprovalPrimary(
  payload: Record<string, any> | null,
  summary: Record<string, any>,
  fallback: string,
): string {
  if (payload?.tool_name) {
    return String(payload.tool_name)
  }
  if (payload?.host) {
    return `${String(payload.method ?? 'HTTP').toUpperCase()} ${payload.host}${payload.path ?? ''}`
  }
  if (summary.service && summary.action) {
    return `${serviceName(summary.service)} · ${actionName(summary.action)}`
  }
  return fallback
}

export function runtimeApprovalReason(
  payload: Record<string, any> | null,
  summary: Record<string, any>,
): string {
  return String(payload?.reason ?? summary.reason ?? summary.policy_reason ?? payload?.host ?? '')
}

export function runtimeApprovalDetail(payload: Record<string, any> | null): string {
  if (!payload) return ''
  const toolName = typeof payload.tool_name === 'string' ? payload.tool_name : ''
  const toolInput = payload.tool_input && typeof payload.tool_input === 'object' ? payload.tool_input : {}
  if (toolName) {
    const filePath =
      readString(toolInput.file_path) || readString(toolInput.path) || readString(toolInput.directory)
    if (filePath) return `${toolName} ${filePath}`
    const pattern = readString(toolInput.pattern)
    if (pattern) return `${toolName} ${pattern}`
    const command = readString(toolInput.command)
    if (command) return `${toolName} ${command}`
    return toolName
  }
  if (typeof payload.host === 'string') {
    return [payload.method, payload.host, payload.path].filter(Boolean).join(' ')
  }
  return ''
}

function readString(value: unknown): string {
  return typeof value === 'string' ? value : ''
}
