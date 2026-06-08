import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { api, type TaskSuggestion, type WelcomeService } from '../api/client'
import ClawvisorInActionWalkthrough, { walkthroughIntroText } from './ClawvisorInActionWalkthrough'
import { ServiceIcon } from './ServiceIcon'

export default function AgentSetupTryItPanel({
  preview = false,
}: {
  preview?: boolean
}) {
  const { data } = useQuery({
    queryKey: ['welcome'],
    queryFn: () => api.welcome.suggestions(),
    staleTime: 5 * 60_000,
    refetchOnWindowFocus: false,
  })

  const suggestions = (data?.suggestions ?? []).slice(0, 2)
  const serviceById = new Map<string, WelcomeService>()
  for (const svc of data?.services ?? []) serviceById.set(svc.id, svc)

  return (
    <div className={`space-y-4 ${preview ? 'opacity-70' : ''}`}>
      <p className="text-sm text-text-secondary">{walkthroughIntroText(data?.walkthrough)}</p>

      <ClawvisorInActionWalkthrough
        example={data?.walkthrough}
        maxSteps={preview ? 1 : undefined}
        showIntro={false}
      />

      {!preview && suggestions.length > 0 && (
        <div className="space-y-3">
          <p className="text-xs uppercase tracking-wider text-text-tertiary">Suggested prompts</p>
          <div className="grid gap-3 sm:grid-cols-2">
            {suggestions.map((suggestion, i) => (
              <SuggestionPromptCard key={i} suggestion={suggestion} serviceById={serviceById} />
            ))}
          </div>
        </div>
      )}
    </div>
  )
}

function SuggestionPromptCard({
  suggestion,
  serviceById,
}: {
  suggestion: TaskSuggestion
  serviceById: Map<string, WelcomeService>
}) {
  const [copied, setCopied] = useState(false)

  function copy() {
    navigator.clipboard.writeText(suggestion.prompt).then(() => {
      setCopied(true)
      setTimeout(() => setCopied(false), 1500)
    })
  }

  return (
    <div className="rounded-lg border border-border-default bg-surface-1 flex flex-col overflow-hidden">
      <div className="px-4 pt-4 pb-3">
        <h4 className="text-sm font-semibold text-text-primary leading-snug">{suggestion.title}</h4>
        <p className="text-sm text-text-secondary mt-2 leading-relaxed italic line-clamp-4">
          {suggestion.prompt}
        </p>
      </div>
      <div className="px-4 py-3 border-t border-border-subtle flex items-center justify-between gap-3 mt-auto">
        <div className="flex flex-wrap gap-1.5 min-w-0">
          {suggestion.services.map(id => {
            const svc = serviceById.get(id)
            return (
              <span
                key={id}
                className="inline-flex items-center gap-1 text-xs px-2 py-0.5 rounded-full bg-surface-2 border border-border-subtle text-text-tertiary"
              >
                {svc && (
                  <div className={id === 'github' ? 'dark:invert' : ''}>
                    <ServiceIcon iconUrl={svc.icon_url} iconSvg={svc.icon_svg} serviceId={id} size={11} />
                  </div>
                )}
                <span>{svc?.name ?? id}</span>
              </span>
            )
          })}
        </div>
        <button
          type="button"
          onClick={copy}
          className="shrink-0 text-xs font-medium text-brand hover:underline"
        >
          {copied ? 'Copied' : 'Copy prompt'}
        </button>
      </div>
    </div>
  )
}
