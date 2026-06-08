import type { RuntimeToolControl } from '../api/client'
import { isShellLikeToolName } from '../components/policy/policyUtils'

export type ToolControlDefault = {
  tool_name: string
  global: {
    action: 'unset' | 'allow' | 'review' | 'deny'
    readOnlyCommandsAllowed?: boolean
    sensitiveFileGuardEnabled?: boolean
  }
  agent: {
    action: 'unset' | 'allow' | 'review' | 'deny'
    readOnlyCommandsAllowed?: boolean
    sensitiveFileGuardEnabled?: boolean
  }
}

/** Canonical default tool policies shown before harness discovery. */
export const TOOL_CONTROL_DEFAULTS: ToolControlDefault[] = [
  {
    tool_name: 'exec',
    global: {
      action: 'allow',
      readOnlyCommandsAllowed: true,
      sensitiveFileGuardEnabled: true,
    },
    agent: {
      action: 'unset',
      readOnlyCommandsAllowed: true,
      sensitiveFileGuardEnabled: true,
    },
  },
  {
    tool_name: 'read',
    global: {
      action: 'allow',
      sensitiveFileGuardEnabled: true,
    },
    agent: {
      action: 'unset',
      sensitiveFileGuardEnabled: true,
    },
  },
]

const DEFAULT_TOOL_NAMES = new Set(TOOL_CONTROL_DEFAULTS.map(d => d.tool_name.toLowerCase()))

export function isDefaultToolControlName(name: string): boolean {
  return DEFAULT_TOOL_NAMES.has(name.trim().toLowerCase())
}

function sensitiveGuardApplies(toolName: string): boolean {
  const lower = toolName.trim().toLowerCase()
  if (isShellLikeToolName(toolName)) return true
  return ['read', 'read_file', 'glob', 'grep', 'ls', 'mcp__filesystem__read_file'].includes(lower)
}

function buildSyntheticControl(agentId: string, def: ToolControlDefault): RuntimeToolControl {
  const guardApplies = sensitiveGuardApplies(def.tool_name)
  return {
    agent_id: agentId,
    tool_name: def.tool_name,
    action: def.agent.action,
    source: 'default',
    scope: 'unset',
    global_action: def.global.action,
    agent_action: def.agent.action,
    read_only_commands_allowed: def.agent.readOnlyCommandsAllowed ?? isShellLikeToolName(def.tool_name),
    global_read_only_commands_allowed: def.global.readOnlyCommandsAllowed,
    agent_read_only_commands_allowed: def.agent.readOnlyCommandsAllowed,
    sensitive_file_guard_applies: guardApplies,
    sensitive_file_guard_enabled: def.agent.sensitiveFileGuardEnabled ?? guardApplies,
    global_sensitive_file_guard_enabled: def.global.sensitiveFileGuardEnabled,
    agent_sensitive_file_guard_enabled: def.agent.sensitiveFileGuardEnabled,
    advanced_rule_count: 0,
    advanced_rules: [],
  }
}

/** Overlay API controls onto defaults; discovered tools append when not in defaults. */
export function mergeToolControlsWithDefaults(
  agentId: string,
  controls: RuntimeToolControl[],
): RuntimeToolControl[] {
  const byName = new Map<string, RuntimeToolControl>()

  for (const def of TOOL_CONTROL_DEFAULTS) {
    byName.set(def.tool_name.toLowerCase(), buildSyntheticControl(agentId, def))
  }

  for (const control of controls) {
    const key = control.tool_name.trim().toLowerCase()
    const existing = byName.get(key)
    if (existing) {
      byName.set(key, {
        ...existing,
        ...control,
        tool_name: control.tool_name,
        sensitive_file_guard_applies: control.sensitive_file_guard_applies ?? existing.sensitive_file_guard_applies,
      })
    } else {
      byName.set(key, control)
    }
  }

  return [...byName.values()].sort((a, b) => a.tool_name.localeCompare(b.tool_name))
}

/** Tools that belong in the Global Tool Policies section. */
export function filterGlobalToolControls(controls: RuntimeToolControl[]): RuntimeToolControl[] {
  return controls.filter(control =>
    isDefaultToolControlName(control.tool_name)
    || isShellLikeToolName(control.tool_name)
    || !!control.sensitive_file_guard_applies
    || !!control.global_rule_id
    || (control.advanced_rules ?? []).some(rule => !rule.agent_id),
  )
}
