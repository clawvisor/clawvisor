import { useQuery } from '@tanstack/react-query'
import { Link } from 'react-router-dom'
import { api, type AuditEntry } from '../api/client'
import { formatDistanceToNow } from 'date-fns'

interface Props {
  pendingCount: number
}

const OUTCOME_COLORS: Record<string, string> = {
  executed: 'bg-green-100 text-green-800',
  blocked: 'bg-red-100 text-red-800',
  pending: 'bg-yellow-100 text-yellow-800',
  denied: 'bg-gray-100 text-gray-700',
  timeout: 'bg-gray-100 text-gray-700',
  error: 'bg-red-100 text-red-800',
}

function OutcomeBadge({ outcome }: { outcome: string }) {
  return (
    <span className={`inline-block px-2 py-0.5 rounded-full text-xs font-medium ${OUTCOME_COLORS[outcome] ?? 'bg-gray-100 text-gray-700'}`}>
      {outcome}
    </span>
  )
}

function AuditRow({ entry }: { entry: AuditEntry }) {
  return (
    <tr className="border-t hover:bg-gray-50">
      <td className="px-4 py-2 text-sm text-gray-500">
        {formatDistanceToNow(new Date(entry.timestamp), { addSuffix: true })}
      </td>
      <td className="px-4 py-2 text-sm font-mono">{entry.service}</td>
      <td className="px-4 py-2 text-sm font-mono">{entry.action}</td>
      <td className="px-4 py-2"><OutcomeBadge outcome={entry.outcome} /></td>
    </tr>
  )
}

export default function Overview({ pendingCount }: Props) {
  const { data: services } = useQuery({
    queryKey: ['services'],
    queryFn: () => api.services.list(),
  })
  const { data: auditData } = useQuery({
    queryKey: ['audit', { limit: 10 }],
    queryFn: () => api.audit.list({ limit: 10 }),
  })

  const activatedCount = services?.services.filter(s => s.status === 'activated').length ?? 0
  const recentEntries = auditData?.entries ?? []

  return (
    <div className="p-8 space-y-8">
      <h1 className="text-2xl font-bold text-gray-900">Overview</h1>

      {/* Stat cards */}
      <div className="grid grid-cols-3 gap-4">
        <StatCard
          label="Activated Services"
          value={activatedCount}
          href="/dashboard/services"
          color="blue"
        />
        <StatCard
          label="Pending Approvals"
          value={pendingCount}
          href="/dashboard"
          color={pendingCount > 0 ? 'orange' : 'gray'}
        />
        <StatCard
          label="Audit Entries"
          value={auditData?.total ?? 0}
          href="/dashboard/audit"
          color="gray"
        />
      </div>

      {/* Pending approvals banner */}
      {pendingCount > 0 && (
        <div className="rounded-lg border border-orange-200 bg-orange-50 px-5 py-4 flex items-center justify-between">
          <span className="text-orange-800 font-medium">
            {pendingCount} approval{pendingCount > 1 ? 's' : ''} awaiting your decision
          </span>
          <span className="text-orange-600 text-sm">See the panel →</span>
        </div>
      )}

      {/* Recent audit activity */}
      <section>
        <div className="flex items-center justify-between mb-3">
          <h2 className="text-lg font-semibold text-gray-800">Recent Activity</h2>
          <Link to="/dashboard/audit" className="text-sm text-blue-600 hover:underline">
            View all
          </Link>
        </div>
        {recentEntries.length === 0 ? (
          <p className="text-sm text-gray-400">No activity yet. Set up an agent to get started.</p>
        ) : (
          <div className="bg-white border rounded-lg overflow-hidden">
            <table className="w-full">
              <thead className="bg-gray-50 text-xs text-gray-500 uppercase tracking-wide">
                <tr>
                  <th className="px-4 py-2 text-left">Time</th>
                  <th className="px-4 py-2 text-left">Service</th>
                  <th className="px-4 py-2 text-left">Action</th>
                  <th className="px-4 py-2 text-left">Outcome</th>
                </tr>
              </thead>
              <tbody>
                {recentEntries.map(e => <AuditRow key={e.id} entry={e} />)}
              </tbody>
            </table>
          </div>
        )}
      </section>
    </div>
  )
}

function StatCard({
  label, value, href, color,
}: {
  label: string
  value: number
  href: string
  color: 'blue' | 'orange' | 'gray'
}) {
  const ring: Record<string, string> = {
    blue: 'border-blue-200',
    orange: 'border-orange-200',
    gray: 'border-gray-200',
  }
  const text: Record<string, string> = {
    blue: 'text-blue-700',
    orange: 'text-orange-600',
    gray: 'text-gray-800',
  }
  return (
    <Link
      to={href}
      className={`bg-white border rounded-lg p-5 hover:shadow-sm transition-shadow ${ring[color]}`}
    >
      <div className={`text-3xl font-bold ${text[color]}`}>{value}</div>
      <div className="text-sm text-gray-500 mt-1">{label}</div>
    </Link>
  )
}
