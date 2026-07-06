import { useState, type ReactNode } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { formatDistanceToNow } from 'date-fns'
import { api } from '../api/client'
import { useAuth } from '../hooks/useAuth'

// Admin visibility (04b): a single instance-admin-only surface with a
// fleet-wide agents table, a cross-user approval queue (approve/deny), the
// instance cost rollup, and a cross-user audit view. Members never reach this
// page — the nav item is gated on role and every backing route is 403 for
// non-admins (and absent entirely in the cloud multi-org build).

function usd(micros: number): string {
  return `$${(micros / 1_000_000).toFixed(4)}`
}

function Section({ title, children }: { title: string; children: ReactNode }) {
  return (
    <section className="mb-8">
      <h2 className="text-sm font-semibold text-text-primary mb-2">{title}</h2>
      <div className="bg-surface-1 border border-border-default rounded-lg overflow-hidden">{children}</div>
    </section>
  )
}

function FleetAgents() {
  const { data, isLoading } = useQuery({ queryKey: ['admin', 'agents'], queryFn: () => api.admin.agents() })
  if (isLoading) return <div className="p-4 text-text-secondary text-sm">Loading…</div>
  const agents = data?.agents ?? []
  if (agents.length === 0) return <div className="p-4 text-text-secondary text-sm">No agents.</div>
  return (
    <table className="w-full text-sm">
      <thead className="text-text-secondary text-left">
        <tr>
          <th className="px-4 py-2">Agent</th>
          <th className="px-4 py-2">Owner</th>
          <th className="px-4 py-2">Active tasks</th>
        </tr>
      </thead>
      <tbody>
        {agents.map((a) => (
          <tr key={a.id} className="border-t border-border-default">
            <td className="px-4 py-2 text-text-primary">{a.name}</td>
            <td className="px-4 py-2 text-text-secondary">{a.owner_label}</td>
            <td className="px-4 py-2 text-text-secondary">{a.active_task_count}</td>
          </tr>
        ))}
      </tbody>
    </table>
  )
}

function ApprovalQueue() {
  const qc = useQueryClient()
  const { data, isLoading } = useQuery({ queryKey: ['admin', 'approvals'], queryFn: () => api.admin.approvals() })
  const resolve = useMutation({
    mutationFn: ({ id, decision }: { id: string; decision: 'approve' | 'deny' }) =>
      api.admin.resolveApproval(id, decision),
    onSettled: () => {
      qc.invalidateQueries({ queryKey: ['admin', 'approvals'] })
    },
  })
  if (isLoading) return <div className="p-4 text-text-secondary text-sm">Loading…</div>
  const entries = data?.entries ?? []
  if (entries.length === 0) return <div className="p-4 text-text-secondary text-sm">No pending holds.</div>
  return (
    <table className="w-full text-sm">
      <thead className="text-text-secondary text-left">
        <tr>
          <th className="px-4 py-2">Owner</th>
          <th className="px-4 py-2">Request</th>
          <th className="px-4 py-2">Raised</th>
          <th className="px-4 py-2 text-right">Resolve</th>
        </tr>
      </thead>
      <tbody>
        {entries.map((e) => (
          <tr key={e.id} className="border-t border-border-default">
            <td className="px-4 py-2 text-text-secondary">{e.owner_label}</td>
            <td className="px-4 py-2 font-mono text-xs text-text-secondary">{e.request_id}</td>
            <td className="px-4 py-2 text-text-secondary">
              {formatDistanceToNow(new Date(e.created_at), { addSuffix: true })}
            </td>
            <td className="px-4 py-2 text-right whitespace-nowrap">
              <button
                className="px-2 py-1 rounded bg-success text-surface-0 text-xs mr-2 disabled:opacity-50"
                disabled={resolve.isPending}
                onClick={() => resolve.mutate({ id: e.id, decision: 'approve' })}
              >
                Approve
              </button>
              <button
                className="px-2 py-1 rounded bg-danger text-surface-0 text-xs disabled:opacity-50"
                disabled={resolve.isPending}
                onClick={() => resolve.mutate({ id: e.id, decision: 'deny' })}
              >
                Deny
              </button>
            </td>
          </tr>
        ))}
      </tbody>
    </table>
  )
}

function CostRollup() {
  const [window, setWindow] = useState<'daily' | 'monthly'>('daily')
  const { data, isLoading } = useQuery({
    queryKey: ['admin', 'costs', window],
    queryFn: () => api.admin.costs(window),
  })
  return (
    <div className="p-4">
      <div className="flex items-center gap-2 mb-3">
        {(['daily', 'monthly'] as const).map((w) => (
          <button
            key={w}
            className={`px-2 py-1 rounded text-xs ${
              window === w ? 'bg-brand text-surface-0' : 'bg-surface-2 text-text-secondary'
            }`}
            onClick={() => setWindow(w)}
          >
            {w}
          </button>
        ))}
      </div>
      {isLoading || !data ? (
        <div className="text-text-secondary text-sm">Loading…</div>
      ) : (
        <>
          <div className="text-text-primary text-sm mb-3">
            Total: <span className="font-semibold">{usd(data.cost_micros)}</span> across {data.request_count} requests
          </div>
          <table className="w-full text-sm">
            <thead className="text-text-secondary text-left">
              <tr>
                <th className="px-2 py-1">User</th>
                <th className="px-2 py-1 text-right">Requests</th>
                <th className="px-2 py-1 text-right">Spend</th>
              </tr>
            </thead>
            <tbody>
              {data.by_user.map((u) => (
                <tr key={u.user_id} className="border-t border-border-default">
                  <td className="px-2 py-1 text-text-secondary">{u.owner_label}</td>
                  <td className="px-2 py-1 text-right text-text-secondary">{u.request_count}</td>
                  <td className="px-2 py-1 text-right text-text-primary">{usd(u.cost_micros)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </>
      )}
    </div>
  )
}

function CrossUserAudit() {
  const { data, isLoading } = useQuery({
    queryKey: ['admin', 'audit'],
    queryFn: () => api.admin.audit({ limit: 25 }),
  })
  if (isLoading) return <div className="p-4 text-text-secondary text-sm">Loading…</div>
  const entries = data?.entries ?? []
  if (entries.length === 0) return <div className="p-4 text-text-secondary text-sm">No audit events.</div>
  return (
    <table className="w-full text-sm">
      <thead className="text-text-secondary text-left">
        <tr>
          <th className="px-4 py-2">Actor</th>
          <th className="px-4 py-2">Event</th>
          <th className="px-4 py-2">Outcome</th>
          <th className="px-4 py-2">When</th>
        </tr>
      </thead>
      <tbody>
        {entries.map((e) => (
          <tr key={e.id} className="border-t border-border-default">
            <td className="px-4 py-2 text-text-secondary">{e.owner_label}</td>
            <td className="px-4 py-2 text-text-primary">{e.summary_text || `${e.service} ${e.action}`}</td>
            <td className="px-4 py-2 text-text-secondary">{e.outcome}</td>
            <td className="px-4 py-2 text-text-secondary">
              {formatDistanceToNow(new Date(e.timestamp), { addSuffix: true })}
            </td>
          </tr>
        ))}
      </tbody>
    </table>
  )
}

export default function Admin() {
  const { user } = useAuth()
  if (user?.role !== 'admin') {
    return <div className="p-6 text-text-secondary text-sm">Admin access required.</div>
  }
  return (
    <div className="p-6 max-w-5xl mx-auto">
      <h1 className="text-lg font-bold text-text-primary mb-1">Fleet</h1>
      <p className="text-text-secondary text-sm mb-6">
        Instance-wide view across every member. Terraform / automation resources are labeled as such.
      </p>
      <Section title="Approval queue">
        <ApprovalQueue />
      </Section>
      <Section title="Agents">
        <FleetAgents />
      </Section>
      <Section title="Cost">
        <CostRollup />
      </Section>
      <Section title="Audit">
        <CrossUserAudit />
      </Section>
    </div>
  )
}
