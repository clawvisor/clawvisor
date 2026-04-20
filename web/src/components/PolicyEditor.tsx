import { useEffect, useState } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { formatDistanceToNow } from 'date-fns'
import { api } from '../api/client'
import type { PolicyValidation, PolicyViolation, PolicyBan } from '../api/client'

export default function PolicyEditor({ bridgeId }: { bridgeId: string }) {
  const qc = useQueryClient()
  const [yaml, setYaml] = useState('')
  const [enabled, setEnabled] = useState(false)
  const [validation, setValidation] = useState<PolicyValidation | null>(null)
  const [dirty, setDirty] = useState(false)

  const policyQ = useQuery({
    queryKey: ['policy', bridgeId],
    queryFn: () => api.plugin.getPolicy(bridgeId),
  })

  useEffect(() => {
    if (policyQ.data && !dirty) {
      setYaml(policyQ.data.yaml)
      setEnabled(policyQ.data.enabled)
    }
  }, [policyQ.data, dirty])

  const validateMut = useMutation({
    mutationFn: (y: string) => api.plugin.validatePolicy(bridgeId, y),
    onSuccess: (v) => setValidation(v),
  })
  const generateMut = useMutation({
    mutationFn: () => api.plugin.generatePolicy(bridgeId),
    onSuccess: (res) => {
      setYaml(res.yaml)
      setDirty(true)
      setValidation(null)
    },
  })
  const saveMut = useMutation({
    mutationFn: () => api.plugin.upsertPolicy(bridgeId, yaml, enabled),
    onSuccess: () => {
      setDirty(false)
      qc.invalidateQueries({ queryKey: ['policy', bridgeId] })
      qc.invalidateQueries({ queryKey: ['policy', bridgeId, 'violations'] })
    },
  })

  const violationsQ = useQuery({
    queryKey: ['policy', bridgeId, 'violations'],
    queryFn: () => api.plugin.listViolations(bridgeId),
    refetchInterval: 30_000,
  })
  const bansQ = useQuery({
    queryKey: ['policy', bridgeId, 'bans'],
    queryFn: () => api.plugin.listBans(bridgeId),
    refetchInterval: 30_000,
  })
  const liftMut = useMutation({
    mutationFn: ({ agent, rule }: { agent: string; rule: string }) =>
      api.plugin.liftBan(bridgeId, agent, rule),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['policy', bridgeId, 'bans'] }),
  })

  return (
    <div className="pt-3 border-t border-border-default space-y-4">
      <div>
        <div className="flex items-center justify-between mb-2">
          <div>
            <div className="text-sm font-medium text-text-primary">Policy (YAML)</div>
            <div className="text-xs text-text-tertiary mt-0.5">
              Version {policyQ.data?.version ?? 0}{policyQ.data?.updated_at && (
                <> · updated {formatDistanceToNow(new Date(policyQ.data.updated_at), { addSuffix: true })}</>
              )}
            </div>
          </div>
          <label className="flex items-center gap-2 text-sm text-text-secondary cursor-pointer">
            <input
              type="checkbox"
              checked={enabled}
              onChange={e => {
                setEnabled(e.target.checked)
                setDirty(true)
              }}
            />
            <span>Enabled</span>
          </label>
        </div>
        <textarea
          value={yaml}
          onChange={e => {
            setYaml(e.target.value)
            setDirty(true)
            setValidation(null)
          }}
          rows={14}
          spellCheck={false}
          className="w-full text-xs font-mono rounded border border-border-default bg-surface-0 text-text-primary p-3 focus:outline-none focus:ring-1 focus:ring-brand/30 focus:border-brand"
        />
        <div className="flex items-center gap-2 mt-2 flex-wrap">
          <button
            onClick={() => validateMut.mutate(yaml)}
            disabled={validateMut.isPending || !yaml}
            className="text-xs px-3 py-1.5 rounded border border-border-default hover:bg-surface-2 disabled:opacity-50"
          >
            {validateMut.isPending ? 'Validating…' : 'Validate'}
          </button>
          <button
            onClick={() => saveMut.mutate()}
            disabled={saveMut.isPending || !yaml}
            className="text-xs px-3 py-1.5 rounded bg-brand text-surface-0 hover:bg-brand-strong disabled:opacity-50"
          >
            {saveMut.isPending ? 'Saving…' : dirty ? 'Save policy' : 'Saved'}
          </button>
          <button
            onClick={() => {
              if (!yaml || confirm('Replace the current YAML with a suggested starter based on observed traffic?')) {
                generateMut.mutate()
              }
            }}
            disabled={generateMut.isPending}
            className="text-xs px-3 py-1.5 rounded border border-border-default hover:bg-surface-2 disabled:opacity-50"
          >
            {generateMut.isPending ? 'Suggesting…' : 'Suggest from traffic'}
          </button>
          {saveMut.error && (
            <span className="text-xs text-danger">{(saveMut.error as Error).message}</span>
          )}
        </div>
        {validation && (
          <div className="mt-2 text-xs">
            {validation.ok ? (
              <div className="text-success">
                OK — {validation.rule_count ?? 0} rule(s); default {validation.default_action}
                {validation.ban_enabled ? '; ban enabled' : ''}
                {validation.judge_enabled ? '; judge enabled' : ''}
              </div>
            ) : (
              <div className="text-danger">{validation.error}</div>
            )}
            {validation.warnings?.map((w, i) => (
              <div key={i} className="text-warning mt-1">warning: {w}</div>
            ))}
          </div>
        )}
      </div>

      {bansQ.data && bansQ.data.bans.length > 0 && (
        <div>
          <div className="text-sm font-medium text-text-primary mb-1">Active bans</div>
          <div className="space-y-1">
            {bansQ.data.bans.map(b => (
              <BanRow
                key={b.id}
                ban={b}
                onLift={() => liftMut.mutate({ agent: b.agent_token_id, rule: b.rule_name })}
                lifting={liftMut.isPending}
              />
            ))}
          </div>
        </div>
      )}

      <div>
        <div className="text-sm font-medium text-text-primary mb-1">Recent violations (7d)</div>
        {violationsQ.data && violationsQ.data.violations.length > 0 ? (
          <div className="bg-surface-1 border border-border-default rounded overflow-hidden">
            <table className="w-full text-xs">
              <thead className="bg-surface-2 text-text-tertiary">
                <tr>
                  <th className="text-left px-3 py-1.5">When</th>
                  <th className="text-left px-3 py-1.5">Agent</th>
                  <th className="text-left px-3 py-1.5">Rule</th>
                  <th className="text-left px-3 py-1.5">Action</th>
                  <th className="text-left px-3 py-1.5">Destination</th>
                </tr>
              </thead>
              <tbody>
                {violationsQ.data.violations.map(v => <ViolationRow key={v.id} v={v} />)}
              </tbody>
            </table>
          </div>
        ) : (
          <div className="text-xs text-text-tertiary">No violations in the last 7 days.</div>
        )}
      </div>
    </div>
  )
}

function BanRow({ ban, onLift, lifting }: { ban: PolicyBan; onLift: () => void; lifting: boolean }) {
  return (
    <div className="flex items-center justify-between bg-warning/10 border border-warning/30 rounded px-3 py-2 text-xs">
      <div>
        <span className="font-mono">{ban.agent_token_id}</span>
        {ban.rule_name && <span className="text-text-tertiary"> · rule <code>{ban.rule_name}</code></span>}
        <span className="text-text-tertiary"> · {ban.violation_count} violations</span>
        <span className="text-text-tertiary"> · expires {formatDistanceToNow(new Date(ban.expires_at), { addSuffix: true })}</span>
      </div>
      <button
        onClick={onLift}
        disabled={lifting}
        className="text-xs px-2 py-1 rounded border border-warning/40 text-warning hover:bg-warning/20 disabled:opacity-50"
      >
        Lift
      </button>
    </div>
  )
}

function ViolationRow({ v }: { v: PolicyViolation }) {
  return (
    <tr className="border-t border-border-subtle">
      <td className="px-3 py-1.5 text-text-tertiary whitespace-nowrap">
        {formatDistanceToNow(new Date(v.ts), { addSuffix: true })}
      </td>
      <td className="px-3 py-1.5 font-mono text-[11px]">{v.agent_token_id || '—'}</td>
      <td className="px-3 py-1.5 font-mono text-[11px]">{v.rule_name}</td>
      <td className="px-3 py-1.5">
        <span className={`text-[10px] uppercase tracking-wider font-semibold px-1.5 py-0.5 rounded border ${
          v.action === 'block'
            ? 'bg-danger/10 text-danger border-danger/20'
            : 'bg-warning/10 text-warning border-warning/20'
        }`}>
          {v.action}
        </span>
      </td>
      <td className="px-3 py-1.5 text-text-tertiary font-mono text-[11px] truncate max-w-[260px]">
        {v.method} {v.destination_host}{v.destination_path}
      </td>
    </tr>
  )
}
