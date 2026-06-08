import { useState } from 'react'

export default function CopyablePromptField({ value }: { value: string }) {
  const [copied, setCopied] = useState(false)

  function copy() {
    navigator.clipboard.writeText(value).then(() => {
      setCopied(true)
      setTimeout(() => setCopied(false), 1500)
    })
  }

  return (
    <div className="flex items-stretch rounded-md border border-border-default bg-surface-1 overflow-hidden">
      <input
        readOnly
        value={value}
        aria-label="Example prompt"
        className="ds-input flex-1 min-w-0 !h-auto !py-2 !border-0 !rounded-none !bg-transparent focus:!ring-0 focus:!border-transparent"
        onFocus={e => e.currentTarget.select()}
        onClick={e => e.currentTarget.select()}
      />
      <button
        type="button"
        onClick={copy}
        title={copied ? 'Copied' : 'Copy prompt'}
        aria-label={copied ? 'Copied' : 'Copy prompt'}
        className="shrink-0 inline-flex items-center gap-1.5 px-3 border-l border-border-default text-xs font-medium text-text-secondary hover:text-text-primary hover:bg-surface-2 transition-colors"
      >
        {copied ? (
          <>
            <svg className="w-3.5 h-3.5 text-success" fill="none" stroke="currentColor" strokeWidth="2.5" viewBox="0 0 24 24" aria-hidden>
              <path d="M5 13l4 4L19 7" />
            </svg>
            Copied
          </>
        ) : (
          <>
            <svg className="w-3.5 h-3.5" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24" aria-hidden>
              <rect x="9" y="9" width="13" height="13" rx="2" ry="2" />
              <path d="M5 15H4a2 2 0 01-2-2V4a2 2 0 012-2h9a2 2 0 012 2v1" />
            </svg>
            Copy
          </>
        )}
      </button>
    </div>
  )
}
