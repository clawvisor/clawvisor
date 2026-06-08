export type InstallerHelper = 'claude' | 'codex'

export const INSTALLER_HELPERS: Record<InstallerHelper, {
  shortName: string
  pillLabel: string
  skillPath: string
  invokeCommand: string
}> = {
  claude: {
    shortName: 'Claude Code',
    pillLabel: 'Claude Code skill',
    skillPath: '~/.claude/commands/clawvisor-install.md',
    invokeCommand: 'claude /clawvisor-install',
  },
  codex: {
    shortName: 'Codex',
    pillLabel: 'Codex skill',
    skillPath: '~/.codex/skills/clawvisor-install/SKILL.md',
    invokeCommand: "codex '$clawvisor-install'",
  },
}

export function buildHelperCommand(opts: {
  skillURL: string
  helper: InstallerHelper
}): string {
  const helperSpec = INSTALLER_HELPERS[opts.helper]
  return [
    `curl -sf "${opts.skillURL}" \\`,
    `  --create-dirs -o ${helperSpec.skillPath} \\`,
    `  && ${helperSpec.invokeCommand}`,
  ].join('\n')
}
