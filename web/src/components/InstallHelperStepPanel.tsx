import { useState } from 'react'
import type { AuditEntry, RuntimeSession } from '../api/client'
import InstallSkillPreviewDrawer from './InstallSkillPreviewDrawer'
import QuestionToggleGroup from './QuestionToggleGroup'
import {
  INSTALLER_HELPERS,
  type InstallerHelper,
} from '../utils/installerHelpers'

type InstallHelperStepPanelProps = {
  helper: InstallerHelper
  onHelperChange: (helper: InstallerHelper) => void
  helperCommand: string
  skillPreviewUrl?: string
  frameworkLabel?: string
  onCopy: (text: string) => void
  copied?: boolean
  agentName?: string
  liveSession?: RuntimeSession
  startActivity?: AuditEntry
  watching?: boolean
  prototypeDetected?: boolean
}

function AgentStartStatus({
  liveSession,
  startActivity,
  waitingText,
  watching = false,
  prototypeDetected = false,
}: {
  liveSession?: RuntimeSession
  startActivity?: AuditEntry
  waitingText: string
  watching?: boolean
  prototypeDetected?: boolean
}) {
  const detected = !!liveSession || !!startActivity || prototypeDetected
  if (detected) {
    return (
      <div className="rounded-md border border-success/30 bg-success/10 px-3 py-3">
        <div className="flex items-start gap-2.5">
          <span className="mt-1 h-2.5 w-2.5 rounded-full bg-success shadow-[0_0_0_3px_rgba(34,197,94,0.16)]" />
          <div>
            <p className="text-sm font-medium text-success">
              {liveSession ? 'Live session detected' : 'Routed activity detected'}
            </p>
            <p className="text-xs text-text-secondary mt-1">
              Clawvisor is seeing traffic for this agent. Continue when you're ready to finish setup.
            </p>
          </div>
        </div>
      </div>
    )
  }
  if (watching) {
    return (
      <div className="rounded-md border border-brand/30 bg-brand/5 px-3 py-3">
        <div className="flex items-start gap-2.5">
          <span className="mt-1 h-2.5 w-2.5 rounded-full bg-brand animate-pulse shadow-[0_0_0_3px_rgba(59,130,246,0.16)]" />
          <div>
            <p className="text-sm font-medium text-text-primary">Waiting for Clawvisor traffic</p>
            <p className="text-xs text-text-tertiary mt-1">{waitingText}</p>
          </div>
        </div>
      </div>
    )
  }
  return (
    <div className="rounded-md border border-border-subtle bg-surface-0 px-3 py-3">
      <div className="flex items-start gap-2.5">
        <span className="mt-1 h-2.5 w-2.5 rounded-full bg-text-tertiary" />
        <div>
          <p className="text-sm font-medium text-text-primary">Waiting for Clawvisor traffic</p>
          <p className="text-xs text-text-tertiary mt-1">{waitingText}</p>
        </div>
      </div>
    </div>
  )
}

function CopyPromptBlock({
  text,
  copied,
  onCopy,
}: {
  text: string
  copied?: boolean
  onCopy: (text: string) => void
}) {
  const [localCopied, setLocalCopied] = useState(false)
  const showCopied = copied ?? localCopied

  const handleCopy = () => {
    onCopy(text)
    if (copied === undefined) {
      setLocalCopied(true)
      window.setTimeout(() => setLocalCopied(false), 2000)
    }
  }

  return (
    <div className="relative group bg-surface-0 border border-brand/20 rounded overflow-hidden">
      <pre className="px-3 py-2.5 sm:pr-16 text-xs font-mono text-text-primary overflow-x-auto whitespace-pre-wrap break-words">
        {text}
      </pre>
      <button
        type="button"
        onClick={handleCopy}
        className="hidden sm:block absolute top-2 right-2 text-xs px-2 py-1 rounded border border-border-subtle bg-surface-1 text-text-secondary hover:text-text-primary hover:bg-surface-2"
      >
        {showCopied ? 'Copied' : 'Copy'}
      </button>
      <div className="sm:hidden border-t border-brand/20 px-3 py-1.5 flex justify-end">
        <button
          type="button"
          onClick={handleCopy}
          className="text-xs px-2.5 py-1 rounded border border-border-subtle bg-surface-1 text-text-secondary hover:text-text-primary hover:bg-surface-2"
        >
          {showCopied ? 'Copied' : 'Copy'}
        </button>
      </div>
    </div>
  )
}

export default function InstallHelperStepPanel({
  helper,
  onHelperChange,
  helperCommand,
  skillPreviewUrl,
  frameworkLabel = 'this agent',
  onCopy,
  copied,
  agentName,
  liveSession,
  startActivity,
  watching,
  prototypeDetected,
}: InstallHelperStepPanelProps) {
  const [previewOpen, setPreviewOpen] = useState(false)

  return (
    <div className="space-y-4">
      <div>
        <p className="text-xs text-text-tertiary mb-1.5">Which helper agent is running this?</p>
        <QuestionToggleGroup
          label=""
          value={helper}
          onChange={value => onHelperChange(value as InstallerHelper)}
          options={(Object.keys(INSTALLER_HELPERS) as InstallerHelper[]).map(h => [h, INSTALLER_HELPERS[h].pillLabel])}
        />
      </div>
      <CopyPromptBlock text={helperCommand} copied={copied} onCopy={onCopy} />
      {skillPreviewUrl && (
        <button
          type="button"
          onClick={() => setPreviewOpen(true)}
          className="text-xs text-brand hover:underline"
        >
          Preview skill
        </button>
      )}
      {previewOpen && skillPreviewUrl && (
        <InstallSkillPreviewDrawer
          frameworkLabel={frameworkLabel}
          skillURL={skillPreviewUrl}
          onClose={() => setPreviewOpen(false)}
        />
      )}
      <AgentStartStatus
        liveSession={liveSession}
        startActivity={startActivity}
        watching={watching}
        prototypeDetected={prototypeDetected}
        waitingText={agentName
          ? `Watching ${agentName} for the helper's smoke test.`
          : 'Waiting for the helper to make its first Clawvisor-routed call.'}
      />
    </div>
  )
}
