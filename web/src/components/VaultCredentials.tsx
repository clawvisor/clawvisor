import { useMemo, useState } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { formatDistanceToNow } from 'date-fns'
import { api } from '../api/client'
import type { Agent, InjectableCredential, CredentialUsageRecord } from '../api/client'

type Preset = { ref: string; label: string; help: string }

const PRESETS: Preset[] = [
  { ref: 'vault:anthropic', label: 'Anthropic API', help: 'Injected as x-api-key on api.anthropic.com/v1/*' },
  { ref: 'vault:openai', label: 'OpenAI API', help: 'Injected as Authorization: Bearer on api.openai.com/v1/*' },
  { ref: 'vault:google-ai', label: 'Google AI (Gemini)', help: 'Injected as ?key= on generativelanguage.googleapis.com' },
]

export default function VaultCredentials({ agents }: { agents: Agent[] | undefined }) {
  const qc = useQueryClient()
  const [showAdd, setShowAdd] = useState(false)
  const [showUsage, setShowUsage] = useState(false)
  const [expandedId, setExpandedId] = useState<string | null>(null)

  const { data } = useQuery({
    queryKey: ['vault', 'credentials'],
    queryFn: () => api.vault.credentials.list(),
  })
  const creds = data?.credentials ?? []
  const active = creds.filter(c => !c.revoked_at)

  const revokeMut = useMutation({
    mutationFn: (ref: string) => api.vault.credentials.revoke(ref),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['vault', 'credentials'] }),
  })

  return (
    <section>
      <div className="flex items-center justify-between mb-3">
        <div>
          <h2 className="text-lg font-semibold text-text-primary">Vault Credentials</h2>
          <p className="text-sm text-text-tertiary mt-1">
            Move an API key into the Clawvisor vault so the Network Proxy can inject it at
            request time — agents never see the raw credential. Each credential can be restricted
            to a subset of your agents.
          </p>
        </div>
        <button
          onClick={() => setShowAdd(v => !v)}
          className="text-sm px-3 py-1.5 rounded bg-brand text-surface-0 hover:bg-brand-strong"
        >
          {showAdd ? 'Cancel' : 'Move to vault'}
        </button>
      </div>

      {showAdd && (
        <AddCredentialForm
          agents={agents ?? []}
          existingRefs={active.map(c => c.credential_ref)}
          onCancel={() => setShowAdd(false)}
          onSaved={() => {
            setShowAdd(false)
            qc.invalidateQueries({ queryKey: ['vault', 'credentials'] })
          }}
        />
      )}

      {active.length === 0 && !showAdd && (
        <div className="text-sm text-text-tertiary text-center py-8 bg-surface-1 border border-border-default rounded-md">
          No credentials in the vault yet. Click <strong>Move to vault</strong> to add one.
        </div>
      )}

      <div className="space-y-2 mt-3">
        {active.map(c => (
          <CredentialRow
            key={c.id}
            credential={c}
            agents={agents ?? []}
            expanded={expandedId === c.id}
            onToggleExpand={() => setExpandedId(expandedId === c.id ? null : c.id)}
            onRevoke={() => {
              if (confirm(`Revoke credential "${c.credential_ref}"? The proxy will stop injecting it on the next lookup.`)) {
                revokeMut.mutate(c.credential_ref)
              }
            }}
            onSaved={() => qc.invalidateQueries({ queryKey: ['vault', 'credentials'] })}
          />
        ))}
      </div>

      <div className="mt-4 flex items-center gap-3">
        <button
          onClick={() => setShowUsage(v => !v)}
          className="text-xs px-3 py-1.5 rounded border border-border-default hover:bg-surface-2"
        >
          {showUsage ? 'Hide recent usage' : 'Show recent usage (7d)'}
        </button>
      </div>
      {showUsage && <UsageLog />}
    </section>
  )
}

function AddCredentialForm({
  agents,
  existingRefs,
  onCancel,
  onSaved,
}: {
  agents: Agent[]
  existingRefs: string[]
  onCancel: () => void
  onSaved: () => void
}) {
  const [mode, setMode] = useState<'preset' | 'custom'>('preset')
  const [ref, setRef] = useState<string>(PRESETS.find(p => !existingRefs.includes(p.ref))?.ref ?? PRESETS[0].ref)
  const [customRef, setCustomRef] = useState('')
  const [secret, setSecret] = useState('')
  const [scopeMode, setScopeMode] = useState<'all' | 'restricted'>('all')
  const [selectedAgents, setSelectedAgents] = useState<string[]>([])
  const [error, setError] = useState<string | null>(null)

  const helpText = useMemo(() => {
    if (mode !== 'preset') return 'Custom credential_ref — any injection rule you define can target this.'
    return PRESETS.find(p => p.ref === ref)?.help ?? ''
  }, [mode, ref])

  const mutation = useMutation({
    mutationFn: () => api.vault.credentials.upsert({
      credential_ref: mode === 'preset' ? ref : customRef.trim(),
      credential: secret,
      usable_by_agents: scopeMode === 'all' ? [] : selectedAgents,
    }),
    onSuccess: () => {
      setSecret('')
      onSaved()
    },
    onError: (err: Error) => setError(err.message),
  })

  const submitDisabled = !secret || (mode === 'custom' && !customRef.trim()) || mutation.isPending

  return (
    <div className="bg-surface-1 border border-border-default rounded-md p-4 mb-3 space-y-3">
      {error && <div className="text-xs text-danger">{error}</div>}

      <div className="flex items-center gap-2 text-sm">
        <button
          onClick={() => setMode('preset')}
          className={`px-2.5 py-1 rounded border ${mode === 'preset' ? 'border-brand text-brand bg-brand/10' : 'border-border-default text-text-tertiary'}`}
        >
          Built-in provider
        </button>
        <button
          onClick={() => setMode('custom')}
          className={`px-2.5 py-1 rounded border ${mode === 'custom' ? 'border-brand text-brand bg-brand/10' : 'border-border-default text-text-tertiary'}`}
        >
          Custom credential_ref
        </button>
      </div>

      {mode === 'preset' ? (
        <div>
          <label className="block text-xs text-text-tertiary mb-1">Provider</label>
          <select
            value={ref}
            onChange={e => setRef(e.target.value)}
            className="w-full text-sm rounded border border-border-default bg-surface-0 text-text-primary px-3 py-1.5"
          >
            {PRESETS.map(p => (
              <option key={p.ref} value={p.ref}>
                {p.label} ({p.ref}){existingRefs.includes(p.ref) ? ' — will overwrite' : ''}
              </option>
            ))}
          </select>
          <p className="text-xs text-text-tertiary mt-1">{helpText}</p>
        </div>
      ) : (
        <div>
          <label className="block text-xs text-text-tertiary mb-1">credential_ref</label>
          <input
            value={customRef}
            onChange={e => setCustomRef(e.target.value)}
            placeholder="vault:my-service"
            className="w-full text-sm rounded border border-border-default bg-surface-0 text-text-primary px-3 py-1.5 font-mono"
          />
          <p className="text-xs text-text-tertiary mt-1">{helpText}</p>
        </div>
      )}

      <div>
        <label className="block text-xs text-text-tertiary mb-1">Secret value</label>
        <input
          type="password"
          value={secret}
          onChange={e => setSecret(e.target.value)}
          placeholder="sk-..."
          autoComplete="new-password"
          className="w-full text-sm rounded border border-border-default bg-surface-0 text-text-primary px-3 py-1.5 font-mono"
        />
        <p className="text-xs text-text-tertiary mt-1">
          Stored encrypted in the Clawvisor vault. Never shown again after save.
        </p>
      </div>

      <div>
        <label className="block text-xs text-text-tertiary mb-1">Who can use this credential?</label>
        <div className="flex items-center gap-3 text-sm">
          <label className="flex items-center gap-1.5">
            <input type="radio" checked={scopeMode === 'all'} onChange={() => setScopeMode('all')} />
            <span>Any of my agents</span>
          </label>
          <label className="flex items-center gap-1.5">
            <input type="radio" checked={scopeMode === 'restricted'} onChange={() => setScopeMode('restricted')} />
            <span>Only specific agents</span>
          </label>
        </div>
        {scopeMode === 'restricted' && (
          <AgentMultiSelect
            agents={agents}
            selected={selectedAgents}
            onChange={setSelectedAgents}
          />
        )}
      </div>

      <div className="flex items-center gap-2 pt-1">
        <button
          onClick={() => mutation.mutate()}
          disabled={submitDisabled}
          className="px-4 py-1.5 text-sm rounded bg-brand text-surface-0 hover:bg-brand-strong disabled:opacity-50"
        >
          {mutation.isPending ? 'Saving…' : 'Save to vault'}
        </button>
        <button
          onClick={onCancel}
          className="px-3 py-1.5 text-sm rounded border border-border-default hover:bg-surface-2"
        >
          Cancel
        </button>
      </div>
    </div>
  )
}

function CredentialRow({
  credential,
  agents,
  expanded,
  onToggleExpand,
  onRevoke,
  onSaved,
}: {
  credential: InjectableCredential
  agents: Agent[]
  expanded: boolean
  onToggleExpand: () => void
  onRevoke: () => void
  onSaved: () => void
}) {
  const preset = PRESETS.find(p => p.ref === credential.credential_ref)
  const scopeLabel = credential.usable_by_agents.length === 0
    ? 'any agent'
    : `${credential.usable_by_agents.length} agent${credential.usable_by_agents.length === 1 ? '' : 's'}`

  return (
    <div className="bg-surface-1 border border-border-default rounded-md px-5 py-4">
      <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-3">
        <div className="min-w-0">
          <div className="font-medium text-text-primary flex items-center gap-2">
            <code className="text-xs font-mono bg-surface-2 px-1.5 py-0.5 rounded">{credential.credential_ref}</code>
            {preset && <span className="text-xs text-text-tertiary">{preset.label}</span>}
          </div>
          <p className="text-xs text-text-tertiary mt-0.5">
            Scope: {scopeLabel} · Added {formatDistanceToNow(new Date(credential.created_at), { addSuffix: true })}
            {credential.rotated_at && (
              <> · Rotated {formatDistanceToNow(new Date(credential.rotated_at), { addSuffix: true })}</>
            )}
          </p>
        </div>
        <div className="flex items-center gap-2">
          <button
            onClick={onToggleExpand}
            className="text-xs px-3 py-1.5 rounded border border-border-default hover:bg-surface-2"
          >
            {expanded ? 'Close' : 'Edit'}
          </button>
          <button
            onClick={onRevoke}
            className="text-xs px-3 py-1.5 rounded bg-danger/10 text-danger border border-danger/20 hover:bg-danger/20"
          >
            Revoke
          </button>
        </div>
      </div>

      {expanded && (
        <EditCredential
          credential={credential}
          agents={agents}
          onSaved={onSaved}
          onClose={onToggleExpand}
        />
      )}
    </div>
  )
}

function EditCredential({
  credential,
  agents,
  onSaved,
  onClose,
}: {
  credential: InjectableCredential
  agents: Agent[]
  onSaved: () => void
  onClose: () => void
}) {
  const [rotateSecret, setRotateSecret] = useState('')
  const [scopeMode, setScopeMode] = useState<'all' | 'restricted'>(
    credential.usable_by_agents.length === 0 ? 'all' : 'restricted'
  )
  const [selectedAgents, setSelectedAgents] = useState<string[]>(credential.usable_by_agents)
  const [error, setError] = useState<string | null>(null)

  const aclOnlyMut = useMutation({
    // Updating ACL requires re-POST with the secret; we don't have the plaintext,
    // so ACL-only updates need a user-supplied rotation. To support ACL-only edits,
    // the server would need a PATCH endpoint; for now we tell users to rotate.
    mutationFn: () => {
      if (!rotateSecret) {
        throw new Error('Enter the current or new secret to save ACL changes.')
      }
      return api.vault.credentials.upsert({
        credential_ref: credential.credential_ref,
        credential: rotateSecret,
        usable_by_agents: scopeMode === 'all' ? [] : selectedAgents,
      })
    },
    onSuccess: () => {
      setRotateSecret('')
      onSaved()
      onClose()
    },
    onError: (err: Error) => setError(err.message),
  })

  return (
    <div className="mt-4 pt-3 border-t border-border-default space-y-3">
      {error && <div className="text-xs text-danger">{error}</div>}

      <div>
        <label className="block text-xs text-text-tertiary mb-1">Rotate secret</label>
        <input
          type="password"
          value={rotateSecret}
          onChange={e => setRotateSecret(e.target.value)}
          placeholder="Paste new secret (or current secret to change ACL only)"
          autoComplete="new-password"
          className="w-full text-sm rounded border border-border-default bg-surface-0 text-text-primary px-3 py-1.5 font-mono"
        />
      </div>

      <div>
        <label className="block text-xs text-text-tertiary mb-1">ACL</label>
        <div className="flex items-center gap-3 text-sm">
          <label className="flex items-center gap-1.5">
            <input type="radio" checked={scopeMode === 'all'} onChange={() => setScopeMode('all')} />
            <span>Any of my agents</span>
          </label>
          <label className="flex items-center gap-1.5">
            <input type="radio" checked={scopeMode === 'restricted'} onChange={() => setScopeMode('restricted')} />
            <span>Only specific agents</span>
          </label>
        </div>
        {scopeMode === 'restricted' && (
          <AgentMultiSelect agents={agents} selected={selectedAgents} onChange={setSelectedAgents} />
        )}
      </div>

      <div className="flex items-center gap-2">
        <button
          onClick={() => aclOnlyMut.mutate()}
          disabled={aclOnlyMut.isPending || !rotateSecret}
          className="px-4 py-1.5 text-sm rounded bg-brand text-surface-0 hover:bg-brand-strong disabled:opacity-50"
        >
          {aclOnlyMut.isPending ? 'Saving…' : 'Save'}
        </button>
        <button
          onClick={onClose}
          className="px-3 py-1.5 text-sm rounded border border-border-default hover:bg-surface-2"
        >
          Cancel
        </button>
      </div>
    </div>
  )
}

function AgentMultiSelect({
  agents,
  selected,
  onChange,
}: {
  agents: Agent[]
  selected: string[]
  onChange: (next: string[]) => void
}) {
  if (agents.length === 0) {
    return (
      <div className="mt-2 text-xs text-text-tertiary italic">
        You don't have any agents yet. Create one first to restrict access.
      </div>
    )
  }
  const toggle = (id: string) => {
    onChange(selected.includes(id) ? selected.filter(x => x !== id) : [...selected, id])
  }
  return (
    <div className="mt-2 flex flex-wrap gap-2">
      {agents.map(a => {
        const on = selected.includes(a.id)
        return (
          <button
            key={a.id}
            type="button"
            onClick={() => toggle(a.id)}
            className={`text-xs px-2.5 py-1 rounded border ${
              on ? 'border-brand bg-brand/10 text-brand' : 'border-border-default text-text-tertiary hover:border-text-tertiary'
            }`}
          >
            {a.name}
          </button>
        )
      })}
    </div>
  )
}

function UsageLog() {
  const { data, isLoading } = useQuery({
    queryKey: ['vault', 'credentials', 'usage'],
    queryFn: () => api.vault.credentials.usage(),
  })
  if (isLoading) return <div className="mt-3 text-sm text-text-tertiary">Loading usage…</div>
  const records = data?.records ?? []
  if (records.length === 0) {
    return <div className="mt-3 text-sm text-text-tertiary">No credential lookups in the past 7 days.</div>
  }
  return (
    <div className="mt-3 bg-surface-1 border border-border-default rounded-md overflow-hidden">
      <table className="w-full text-xs">
        <thead className="bg-surface-2 text-text-tertiary">
          <tr>
            <th className="text-left px-3 py-2">When</th>
            <th className="text-left px-3 py-2">Agent</th>
            <th className="text-left px-3 py-2">Credential</th>
            <th className="text-left px-3 py-2">Destination</th>
            <th className="text-left px-3 py-2">Decision</th>
          </tr>
        </thead>
        <tbody>
          {records.map(r => (
            <tr key={r.id} className="border-t border-border-subtle">
              <td className="px-3 py-1.5 text-text-tertiary whitespace-nowrap">
                {formatDistanceToNow(new Date(r.ts), { addSuffix: true })}
              </td>
              <td className="px-3 py-1.5 font-mono text-[11px]">{r.agent_token_id || '—'}</td>
              <td className="px-3 py-1.5 font-mono text-[11px]">{r.credential_ref}</td>
              <td className="px-3 py-1.5 text-text-tertiary font-mono text-[11px] truncate max-w-[240px]">
                {r.destination_host}{r.destination_path}
              </td>
              <td className="px-3 py-1.5">
                <DecisionBadge decision={r.decision} />
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

function DecisionBadge({ decision }: { decision: CredentialUsageRecord['decision'] }) {
  const style =
    decision === 'granted' ? 'bg-success/10 text-success border-success/20' :
    decision === 'denied_acl' ? 'bg-warning/10 text-warning border-warning/20' :
    decision === 'denied_revoked' ? 'bg-danger/10 text-danger border-danger/20' :
    'bg-surface-2 text-text-tertiary border-border-default'
  return (
    <span className={`text-[10px] uppercase tracking-wider font-semibold px-1.5 py-0.5 rounded border ${style}`}>
      {decision.replace('_', ' ')}
    </span>
  )
}
