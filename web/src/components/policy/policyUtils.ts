import type { RuntimePolicyRule, RuntimeToolControl } from '../../api/client'

export function toolPolicyActionLabel(
  action: RuntimePolicyRule['action'] | RuntimeToolControl['action'],
): string {
  switch (action) {
    case 'allow':
      return 'always allow'
    case 'deny':
      return 'always deny'
    case 'unset':
      return 'unset'
    default:
      return 'review'
  }
}

const SHELL_LIKE_TOOL_NAMES = ['bash', 'shell', 'exec', 'exec_command', 'mcp__shell__exec', 'terminal']

export function isShellLikeToolName(name: string): boolean {
  return SHELL_LIKE_TOOL_NAMES.includes(name.trim().toLowerCase())
}
