import { useEffect } from 'react'
import { useQuery } from '@tanstack/react-query'

type InstallSkillPreviewDrawerProps = {
  frameworkLabel: string
  skillURL: string
  onClose: () => void
}

function SkillPreviewBody({ url }: { url: string }) {
  const { data, isLoading, error } = useQuery({
    queryKey: ['installer-skill-preview', url],
    queryFn: async () => {
      const r = await fetch(url)
      if (!r.ok) throw new Error(`HTTP ${r.status}`)
      return r.text()
    },
    staleTime: 5 * 60 * 1000,
  })

  if (isLoading) return <p className="text-sm text-text-tertiary">Loading preview…</p>
  if (error) return <p className="text-sm text-danger">Couldn&apos;t load preview.</p>
  return (
    <pre className="text-xs font-mono whitespace-pre-wrap text-text-secondary leading-relaxed">
      {data}
    </pre>
  )
}

export default function InstallSkillPreviewDrawer({
  frameworkLabel,
  skillURL,
  onClose,
}: InstallSkillPreviewDrawerProps) {
  useEffect(() => {
    const prev = document.body.style.overflow
    document.body.style.overflow = 'hidden'
    const onKeyDown = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose()
    }
    window.addEventListener('keydown', onKeyDown)
    return () => {
      document.body.style.overflow = prev
      window.removeEventListener('keydown', onKeyDown)
    }
  }, [onClose])

  return (
    <div className="fixed inset-0 z-50 flex justify-end">
      <button
        type="button"
        className="absolute inset-0 bg-black/40"
        onClick={onClose}
        aria-label="Close skill preview"
      />
      <aside
        role="dialog"
        aria-labelledby="install-skill-preview-title"
        className="dev-setup-drawer relative flex h-full w-full max-w-xl flex-col border-l border-border-default bg-surface-1 shadow-xl"
      >
        <header className="flex shrink-0 items-start justify-between gap-3 border-b border-border-default px-5 py-4">
          <div className="min-w-0">
            <p className="ds-overline normal-case tracking-normal mb-1">Installer skill</p>
            <h2 id="install-skill-preview-title" className="text-lg font-semibold text-text-primary">
              clawvisor-install
            </h2>
            <p className="text-sm text-text-tertiary mt-1">
              What the helper agent will run to configure {frameworkLabel}.
            </p>
          </div>
          <button
            type="button"
            onClick={onClose}
            className="shrink-0 rounded-md border border-border-default px-2.5 py-1.5 text-sm text-text-secondary hover:bg-surface-2 hover:text-text-primary"
            aria-label="Close"
          >
            Close
          </button>
        </header>
        <div className="flex-1 overflow-y-auto px-5 py-5 custom-scrollbar">
          <SkillPreviewBody url={skillURL} />
        </div>
      </aside>
    </div>
  )
}
