import { useState } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { api, type AdapterGenResult, type AdapterGenActionPreview } from '../api/client'

const riskColors: Record<string, string> = {
  low: 'bg-green-500/10 text-green-600 border-green-500/20',
  medium: 'bg-yellow-500/10 text-yellow-600 border-yellow-500/20',
  high: 'bg-red-500/10 text-red-600 border-red-500/20',
}

const methodColors: Record<string, string> = {
  GET: 'text-blue-500',
  POST: 'text-green-500',
  PUT: 'text-yellow-500',
  PATCH: 'text-yellow-500',
  DELETE: 'text-red-500',
}

function RiskBadge({ category, sensitivity }: { category: string; sensitivity: string }) {
  return (
    <span className={`px-1.5 py-0.5 text-[10px] font-medium rounded border ${riskColors[sensitivity] ?? riskColors.high}`}>
      {category}/{sensitivity}
    </span>
  )
}

function ActionRow({ action }: { action: AdapterGenActionPreview }) {
  const [expanded, setExpanded] = useState(false)
  const requiredParams = action.params?.filter(p => p.required) ?? []
  const optionalParams = action.params?.filter(p => !p.required) ?? []

  return (
    <div className="border-b border-border-subtle last:border-b-0">
      <button
        onClick={() => setExpanded(e => !e)}
        className="w-full px-4 py-3 flex items-center gap-3 text-left hover:bg-surface-0/50 transition-colors"
      >
        {action.method && (
          <span className={`text-[10px] font-bold font-mono w-11 shrink-0 ${methodColors[action.method] ?? 'text-text-tertiary'}`}>
            {action.method}
          </span>
        )}
        <div className="min-w-0 flex-1">
          <div className="flex items-center gap-2">
            <span className="text-xs font-medium text-text-primary">{action.display_name || action.name}</span>
            <span className="text-[10px] font-mono text-text-tertiary">{action.name}</span>
          </div>
          {action.path && (
            <p className="text-[10px] font-mono text-text-tertiary mt-0.5 truncate">{action.path}</p>
          )}
        </div>
        <RiskBadge category={action.category} sensitivity={action.sensitivity} />
        <svg className={`w-3 h-3 text-text-tertiary shrink-0 transition-transform ${expanded ? 'rotate-180' : ''}`} fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24">
          <path d="M19 9l-7 7-7-7" />
        </svg>
      </button>
      {expanded && action.params && action.params.length > 0 && (
        <div className="px-4 pb-3 pl-[4.25rem]">
          <table className="w-full text-xs">
            <thead>
              <tr className="text-text-tertiary text-left">
                <th className="font-medium pb-1 pr-4">Parameter</th>
                <th className="font-medium pb-1 pr-4">Type</th>
                <th className="font-medium pb-1">Required</th>
              </tr>
            </thead>
            <tbody>
              {requiredParams.map(p => (
                <tr key={p.name}>
                  <td className="py-0.5 pr-4 font-mono text-text-primary">{p.name}</td>
                  <td className="py-0.5 pr-4 text-text-secondary">{p.type}</td>
                  <td className="py-0.5 text-text-secondary">Yes</td>
                </tr>
              ))}
              {optionalParams.map(p => (
                <tr key={p.name}>
                  <td className="py-0.5 pr-4 font-mono text-text-tertiary">{p.name}</td>
                  <td className="py-0.5 pr-4 text-text-tertiary">{p.type}</td>
                  <td className="py-0.5 text-text-tertiary">No</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  )
}

function ResultPreview({
  result,
  onInstall,
  onRemove,
  installing,
  removing,
}: {
  result: AdapterGenResult
  onInstall: () => void
  onRemove: () => void
  installing: boolean
  removing: boolean
}) {
  const [showYaml, setShowYaml] = useState(false)

  return (
    <div className="bg-surface-1 border border-border-default rounded-lg">
      {/* Header */}
      <div className="px-5 py-4 border-b border-border-default flex items-center justify-between">
        <div>
          <h2 className="text-base font-semibold text-text-primary">
            {result.display_name || result.service_id}
          </h2>
          <div className="flex items-center gap-3 mt-1">
            <span className="text-xs font-mono text-text-tertiary">{result.service_id}</span>
            <span className="text-xs text-text-tertiary">{result.base_url}</span>
            <span className="text-[10px] px-1.5 py-0.5 rounded bg-surface-2 text-text-secondary font-medium">
              {result.auth_type}
            </span>
            {result.installed && (
              <span className="text-[10px] px-1.5 py-0.5 rounded bg-green-500/10 text-green-600 font-medium">
                Installed
              </span>
            )}
          </div>
          {result.description && (
            <p className="text-xs text-text-tertiary mt-1.5">{result.description}</p>
          )}
        </div>
        <div className="flex items-center gap-2 shrink-0">
          {!result.installed ? (
            <button
              onClick={onInstall}
              disabled={installing}
              className="px-3 py-1.5 rounded text-xs font-medium bg-brand text-surface-0 hover:bg-brand-strong transition-colors disabled:opacity-50"
            >
              {installing ? 'Installing...' : 'Save to local integrations'}
            </button>
          ) : (
            <button
              onClick={onRemove}
              disabled={removing}
              className="px-2.5 py-1 rounded text-xs text-danger border border-danger/20 hover:bg-danger/10 transition-colors disabled:opacity-50"
            >
              {removing ? 'Removing...' : 'Remove'}
            </button>
          )}
        </div>
      </div>

      {/* Warnings */}
      {result.warnings && result.warnings.length > 0 && (
        <div className="px-5 py-3 border-b border-border-default bg-warning/5">
          {result.warnings.map((w, i) => (
            <p key={i} className="text-xs text-warning">{w}</p>
          ))}
        </div>
      )}

      {/* Actions */}
      <div className="border-b border-border-default">
        <div className="px-5 py-3 border-b border-border-subtle">
          <h3 className="text-xs font-semibold text-text-secondary">
            {result.actions.length} Action{result.actions.length !== 1 ? 's' : ''}
          </h3>
        </div>
        {result.actions.map(action => (
          <ActionRow key={action.name} action={action} />
        ))}
      </div>

      {/* YAML toggle */}
      <div className="px-5 py-3">
        <button
          onClick={() => setShowYaml(v => !v)}
          className="text-xs text-text-tertiary hover:text-text-secondary transition-colors flex items-center gap-1"
        >
          <svg className={`w-3 h-3 transition-transform ${showYaml ? 'rotate-90' : ''}`} fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24">
            <path d="M9 5l7 7-7 7" />
          </svg>
          {showYaml ? 'Hide' : 'Show'} raw YAML
        </button>
        {showYaml && (
          <pre className="mt-2 text-xs font-mono bg-surface-0 border border-border-default rounded-md p-3 overflow-x-auto max-h-96 overflow-y-auto text-text-primary whitespace-pre">
            {result.yaml}
          </pre>
        )}
      </div>
    </div>
  )
}

const authTypes = [
  { value: '', label: 'Auto-detect' },
  { value: 'api_key', label: 'API Key' },
  { value: 'oauth2', label: 'OAuth 2.0' },
  { value: 'basic', label: 'Basic Auth' },
  { value: 'none', label: 'None' },
] as const

const installCmd = [
  'curl -fsSL \\',
  '  https://raw.githubusercontent.com/clawvisor/clawvisor/main/skills/clawvisor-generate-integration.md \\',
  '  -o ~/.claude/commands/clawvisor-generate-integration.md \\',
  '  --create-dirs',
].join('\n')

function CopyButton({ text }: { text: string }) {
  const [copied, setCopied] = useState(false)
  return (
    <button
      onClick={() => {
        navigator.clipboard.writeText(text)
        setCopied(true)
        setTimeout(() => setCopied(false), 2000)
      }}
      className="text-[10px] px-2 py-0.5 rounded border border-border-default text-text-tertiary hover:text-text-primary hover:bg-surface-2 transition-colors shrink-0"
    >
      {copied ? 'Copied' : 'Copy'}
    </button>
  )
}

function DocsTab() {
  return (
    <div className="px-5 py-4 space-y-4">
      <p className="text-sm text-text-secondary">
        Use the <code className="px-1.5 py-0.5 rounded bg-surface-2 text-text-primary font-mono text-xs">/clawvisor-generate-integration</code> skill
        in Claude Code to generate integrations from any API documentation.
      </p>

      <div className="space-y-2">
        <p className="text-xs font-medium text-text-secondary">Install the skill:</p>
        <div className="relative bg-surface-0 border border-border-default rounded-md p-3 pr-16">
          <pre className="font-mono text-xs text-text-secondary leading-relaxed whitespace-pre">{installCmd}</pre>
          <div className="absolute top-2 right-2">
            <CopyButton text={installCmd} />
          </div>
        </div>
      </div>

      <div className="space-y-2">
        <p className="text-xs font-medium text-text-secondary">Then use it in any Claude Code session:</p>
        <div className="bg-surface-0 border border-border-default rounded-md p-3 font-mono text-xs text-text-secondary leading-relaxed">
          <div>/clawvisor-generate-integration Notion</div>
          <div>/clawvisor-generate-integration Todoist</div>
          <div>/clawvisor-generate-integration "HubSpot CRM"</div>
        </div>
      </div>

      <p className="text-xs text-text-tertiary">
        Claude will research the API docs, generate the integration YAML with risk classification, and save it to{' '}
        <code className="px-1 py-0.5 rounded bg-surface-2 font-mono">~/.clawvisor/adapters/</code>.
        The integration will appear on the Services page immediately.
      </p>
    </div>
  )
}

type Tab = 'openapi' | 'docs'

function looksLikeUrl(s: string): boolean {
  const trimmed = s.trim()
  return trimmed.startsWith('http://') || trimmed.startsWith('https://')
}

export default function AdapterGen() {
  const qc = useQueryClient()
  const [tab, setTab] = useState<Tab>('openapi')
  const [source, setSource] = useState('')
  const [serviceId, setServiceId] = useState('')
  const [authType, setAuthType] = useState('')
  const [result, setResult] = useState<AdapterGenResult | null>(null)
  const [error, setError] = useState<string | null>(null)

  const hasInput = source.trim().length > 0
  const isUrl = looksLikeUrl(source)

  const generateMut = useMutation({
    mutationFn: () => api.adapterGen.create({
      sourceType: 'openapi',
      source: isUrl ? undefined : source,
      sourceUrl: isUrl ? source.trim() : undefined,
      serviceId: serviceId || undefined,
      authType: authType || undefined,
    }),
    onSuccess: (data) => {
      setResult(data)
      setError(null)
      qc.invalidateQueries({ queryKey: ['services'] })
    },
    onError: (err: Error) => {
      setError(err.message)
      setResult(null)
    },
  })

  const installMut = useMutation({
    mutationFn: (yaml: string) => api.adapterGen.install(yaml),
    onSuccess: (data) => {
      setResult(data)
      setError(null)
      qc.invalidateQueries({ queryKey: ['services'] })
    },
    onError: (err: Error) => setError(err.message),
  })

  const removeMut = useMutation({
    mutationFn: (id: string) => api.adapterGen.remove(id),
    onSuccess: () => {
      setResult(null)
      qc.invalidateQueries({ queryKey: ['services'] })
    },
    onError: (err: Error) => setError(err.message),
  })

  return (
    <div className="p-4 sm:p-8 space-y-6 max-w-5xl">
      <div>
        <h1 className="text-2xl font-bold text-text-primary">Generate Integration</h1>
        <p className="text-sm text-text-tertiary mt-1">
          Generate a Clawvisor integration from an API source. Risk is independently classified for each action.
        </p>
      </div>

      {/* Tabs */}
      <div className="bg-surface-1 border border-border-default rounded-lg">
        <div className="flex border-b border-border-default">
          <button
            onClick={() => setTab('openapi')}
            className={`px-5 py-3 text-sm font-medium border-b-2 transition-colors ${
              tab === 'openapi'
                ? 'border-brand text-text-primary'
                : 'border-transparent text-text-tertiary hover:text-text-secondary'
            }`}
          >
            OpenAPI Spec
          </button>
          <button
            onClick={() => setTab('docs')}
            className={`px-5 py-3 text-sm font-medium border-b-2 transition-colors ${
              tab === 'docs'
                ? 'border-brand text-text-primary'
                : 'border-transparent text-text-tertiary hover:text-text-secondary'
            }`}
          >
            API Documentation
          </button>
        </div>

        {/* OpenAPI Spec tab */}
        {tab === 'openapi' && (
          <div className="px-5 py-4 space-y-4">
            <textarea
              value={source}
              onChange={e => setSource(e.target.value)}
              placeholder={"Paste an OpenAPI spec or a URL to one\ne.g. https://developer.spotify.com/reference/web-api/open-api-schema.yaml"}
              className="w-full h-64 text-xs font-mono px-3 py-2 border border-border-default bg-surface-0 text-text-primary rounded-md focus:outline-none focus:ring-1 focus:ring-brand/30 focus:border-brand resize-y placeholder:text-text-tertiary"
            />
            {isUrl && (
              <p className="text-xs text-text-tertiary -mt-2">
                URL detected — the spec will be fetched from this address.
              </p>
            )}

            {/* Optional overrides */}
            <div className="flex gap-4">
              <div className="flex-1">
                <label className="block text-xs font-medium text-text-secondary mb-1">
                  Service ID <span className="text-text-tertiary">(optional)</span>
                </label>
                <input
                  type="text"
                  value={serviceId}
                  onChange={e => setServiceId(e.target.value)}
                  placeholder="e.g. jira, pagerduty"
                  className="w-full text-xs px-2 py-1.5 border border-border-default bg-surface-0 text-text-primary rounded focus:outline-none focus:ring-1 focus:ring-brand/30 focus:border-brand"
                />
              </div>
              <div className="flex-1">
                <label className="block text-xs font-medium text-text-secondary mb-1">
                  Auth Type <span className="text-text-tertiary">(optional)</span>
                </label>
                <select
                  value={authType}
                  onChange={e => setAuthType(e.target.value)}
                  className="w-full text-xs px-2 py-1.5 border border-border-default bg-surface-0 text-text-primary rounded focus:outline-none focus:ring-1 focus:ring-brand/30 focus:border-brand"
                >
                  {authTypes.map(at => (
                    <option key={at.value} value={at.value}>{at.label}</option>
                  ))}
                </select>
              </div>
            </div>

            {/* Generate button */}
            <div className="flex items-center gap-3">
              <button
                onClick={() => generateMut.mutate()}
                disabled={!hasInput || generateMut.isPending}
                className="px-4 py-2 rounded-md bg-brand text-surface-0 text-sm font-medium hover:bg-brand-strong disabled:opacity-50 disabled:cursor-not-allowed transition-colors"
              >
                {generateMut.isPending ? 'Generating...' : 'Generate Integration'}
              </button>
              {generateMut.isPending && (
                <span className="text-xs text-text-tertiary">
                  This may take 30-60 seconds (generates the definition and classifies risk independently)
                </span>
              )}
            </div>
          </div>
        )}

        {/* API Documentation tab */}
        {tab === 'docs' && (
          <DocsTab />
        )}
      </div>

      {/* Error */}
      {error && (
        <div className="px-4 py-3 rounded-md bg-danger/10 border border-danger/30 text-sm text-danger">
          {error}
        </div>
      )}

      {/* Result preview */}
      {result && <ResultPreview
        result={result}
        onInstall={() => installMut.mutate(result.yaml)}
        onRemove={() => removeMut.mutate(result.service_id)}
        installing={installMut.isPending}
        removing={removeMut.isPending}
      />}
    </div>
  )
}
