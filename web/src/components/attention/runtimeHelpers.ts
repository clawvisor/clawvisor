import type { ApprovalRecord } from '../../api/client'
import { serviceName, actionName } from '../../lib/services'

export function runtimeSummary(approval: ApprovalRecord): Record<string, unknown> {
  return approval.summary_json ?? {}
}

export function runtimePayload(approval: ApprovalRecord): Record<string, unknown> | null {
  return approval.payload_json ?? null
}

export function runtimeApprovalPrimary(
  payload: Record<string, unknown> | null,
  summary: Record<string, unknown>,
  fallback: string,
): string {
  if (payload?.tool_name) return String(payload.tool_name)
  if (payload?.host) {
    return `${String(payload.method ?? 'HTTP').toUpperCase()} ${payload.host}${payload.path ?? ''}`
  }
  if (summary.service && summary.action) {
    return `${serviceName(String(summary.service))} · ${actionName(String(summary.action))}`
  }
  return fallback
}

export function runtimeApprovalReason(
  payload: Record<string, unknown> | null,
  summary: Record<string, unknown>,
): string {
  return String(payload?.reason ?? summary.reason ?? summary.policy_reason ?? payload?.host ?? '')
}

function readRuntimeApprovalString(value: unknown): string {
  return typeof value === 'string' ? value : ''
}

export function runtimeApprovalDetail(payload: Record<string, unknown> | null): string {
  if (!payload) return ''
  const toolName = typeof payload.tool_name === 'string' ? payload.tool_name : ''
  const toolInput = payload.tool_input && typeof payload.tool_input === 'object'
    ? (payload.tool_input as Record<string, unknown>)
    : {}
  if (toolName) {
    const filePath =
      readRuntimeApprovalString(toolInput.file_path)
      || readRuntimeApprovalString(toolInput.path)
      || readRuntimeApprovalString(toolInput.directory)
    if (filePath) return `${toolName} ${filePath}`
    const pattern = readRuntimeApprovalString(toolInput.pattern)
    if (pattern) return `${toolName} ${pattern}`
    const command = readRuntimeApprovalString(toolInput.command)
    if (command) return `${toolName} ${command}`
    return toolName
  }
  if (typeof payload.host === 'string') {
    return [payload.method, payload.host, payload.path].filter(Boolean).join(' ')
  }
  return ''
}
