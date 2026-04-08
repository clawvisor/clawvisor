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
              {installing ? 'Installing...' : 'Save to local adapters'}
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

type InputMode = 'paste' | 'url'

export default function AdapterGen() {
  const qc = useQueryClient()
  const [inputMode, setInputMode] = useState<InputMode>('paste')
  const [source, setSource] = useState('')
  const [sourceUrl, setSourceUrl] = useState('')
  const [urlAuthHeader, setUrlAuthHeader] = useState('')
  const [serviceId, setServiceId] = useState('')
  const [authType, setAuthType] = useState('')
  const [result, setResult] = useState<AdapterGenResult | null>(null)
  const [error, setError] = useState<string | null>(null)

  const hasInput = inputMode === 'paste' ? source.trim() : sourceUrl.trim()

  const generateMut = useMutation({
    mutationFn: () => api.adapterGen.create({
      sourceType: 'openapi',
      source: inputMode === 'paste' ? source : undefined,
      sourceUrl: inputMode === 'url' ? sourceUrl : undefined,
      sourceHeaders: inputMode === 'url' && urlAuthHeader.trim()
        ? { Authorization: urlAuthHeader.trim() }
        : undefined,
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
    <div className="p-8 space-y-6 max-w-5xl">
      <div>
        <h1 className="text-2xl font-bold text-text-primary">Adapter Generator</h1>
        <p className="text-sm text-text-tertiary mt-1">
          Generate an adapter from an OpenAPI spec. Clawvisor independently classifies risk for each action.
        </p>
      </div>

      {/* Source input */}
      <div className="bg-surface-1 border border-border-default rounded-lg">
        <div className="px-5 py-4 border-b border-border-default">
          <h2 className="text-sm font-semibold text-text-primary">OpenAPI Specification</h2>
        </div>

        <div className="px-5 py-4 space-y-4">
          {/* Input mode toggle */}
          <div className="flex items-center gap-3">
            <button
              onClick={() => setInputMode('paste')}
              className={`text-xs font-medium px-2.5 py-1 rounded transition-colors ${
                inputMode === 'paste'
                  ? 'bg-surface-2 text-text-primary'
                  : 'text-text-tertiary hover:text-text-secondary'
              }`}
            >
              Paste spec
            </button>
            <button
              onClick={() => setInputMode('url')}
              className={`text-xs font-medium px-2.5 py-1 rounded transition-colors ${
                inputMode === 'url'
                  ? 'bg-surface-2 text-text-primary'
                  : 'text-text-tertiary hover:text-text-secondary'
              }`}
            >
              Fetch from URL
            </button>
          </div>

          {/* Source content: paste or URL */}
          {inputMode === 'paste' ? (
            <textarea
              value={source}
              onChange={e => setSource(e.target.value)}
              placeholder="Paste your OpenAPI spec here (JSON or YAML)..."
              className="w-full h-64 text-xs font-mono px-3 py-2 border border-border-default bg-surface-0 text-text-primary rounded-md focus:outline-none focus:ring-1 focus:ring-brand/30 focus:border-brand resize-y"
            />
          ) : (
            <div className="space-y-2">
              <input
                type="url"
                value={sourceUrl}
                onChange={e => setSourceUrl(e.target.value)}
                placeholder="https://api.example.com/openapi.json"
                className="w-full text-sm px-3 py-2.5 border border-border-default bg-surface-0 text-text-primary rounded-md focus:outline-none focus:ring-1 focus:ring-brand/30 focus:border-brand"
              />
              <input
                type="password"
                value={urlAuthHeader}
                onChange={e => setUrlAuthHeader(e.target.value)}
                placeholder="Authorization header (e.g. Bearer sk-...)"
                className="w-full text-xs px-3 py-2 border border-border-default bg-surface-0 text-text-primary rounded-md focus:outline-none focus:ring-1 focus:ring-brand/30 focus:border-brand"
              />
              <p className="text-xs text-text-tertiary">
                If the URL requires authentication, paste the Authorization header value above.
              </p>
            </div>
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
              {generateMut.isPending ? 'Generating...' : 'Generate Adapter'}
            </button>
            {generateMut.isPending && (
              <span className="text-xs text-text-tertiary">
                This may take 30-60 seconds (generates the definition and classifies risk independently)
              </span>
            )}
          </div>
        </div>
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
