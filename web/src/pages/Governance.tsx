import { useEffect, useMemo, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import {
  api,
  type GovAuditSettings,
  type GovByModelEntry,
  type GovContentPolicy,
  type GovLeaderboardEntry,
  type GovPromptOverride,
  type GovSpendCap,
  type GovUsageSummary,
  type GovViolation,
} from '../api/client'
import { useAuth } from '../hooks/useAuth'

type Section =
  | 'overview'
  | 'models'
  | 'spending'
  | 'content'
  | 'tasks'
  | 'prompts'
  | 'violations'
  | 'audit'

const SECTIONS: { id: Section; label: string }[] = [
  { id: 'overview', label: 'Overview' },
  { id: 'models', label: 'Models' },
  { id: 'spending', label: 'Spending' },
  { id: 'content', label: 'Content' },
  { id: 'tasks', label: 'Tasks' },
  { id: 'prompts', label: 'Prompts' },
  { id: 'violations', label: 'Violations' },
  { id: 'audit', label: 'Audit Settings' },
]

// ── helpers ─────────────────────────────────────────────────────────

function microsToUsd(micros: number): string {
  return `$${(micros / 1_000_000).toFixed(2)}`
}

function usdToMicros(usd: string): number {
  const n = parseFloat(usd)
  if (Number.isNaN(n) || n < 0) return 0
  return Math.round(n * 1_000_000)
}

function truncateId(id: string | undefined, head = 6, tail = 4): string {
  if (!id) return ''
  if (id.length <= head + tail + 1) return id
  return `${id.slice(0, head)}…${id.slice(-tail)}`
}

function formatTimestamp(ts: string): string {
  try {
    const d = new Date(ts)
    return d.toLocaleString()
  } catch {
    return ts
  }
}

function isoSinceHoursAgo(hours: number): string {
  return new Date(Date.now() - hours * 3600 * 1000).toISOString()
}

// Date-only filter inputs (yyyy-mm-dd) parse to UTC midnight when fed
// to new Date(...). For "since" that's fine; for "until" we want the
// full day included, so pin to 23:59:59.999 in the user's local zone.
function endOfDayIso(dateOnly: string): string {
  const [y, m, d] = dateOnly.split('-').map(Number)
  if (!y || !m || !d) return new Date(dateOnly).toISOString()
  const dt = new Date(y, m - 1, d, 23, 59, 59, 999)
  return dt.toISOString()
}

function isErrorMessage(err: unknown): string {
  if (err && typeof err === 'object' && 'message' in err && typeof (err as { message: unknown }).message === 'string') {
    return (err as { message: string }).message
  }
  return 'Something went wrong.'
}

// ── shared UI ───────────────────────────────────────────────────────

function ErrorBanner({ error }: { error: unknown }) {
  if (!error) return null
  return (
    <div className="rounded-md border border-danger/40 bg-danger/10 px-3 py-2 text-sm text-danger">
      {isErrorMessage(error)}
    </div>
  )
}

function EmptyState({ children }: { children: React.ReactNode }) {
  return <p className="text-sm text-text-secondary italic">{children}</p>
}

function Card({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <div className="rounded-md border border-border-default bg-surface-1 p-4">
      <h3 className="text-sm font-semibold text-text-primary mb-3">{title}</h3>
      {children}
    </div>
  )
}

function PrimaryButton({
  children,
  onClick,
  disabled,
  type = 'button',
}: {
  children: React.ReactNode
  onClick?: () => void
  disabled?: boolean
  type?: 'button' | 'submit'
}) {
  return (
    <button
      type={type}
      onClick={onClick}
      disabled={disabled}
      className="px-3 py-1.5 text-sm font-medium rounded-md bg-brand text-surface-0 hover:bg-brand-strong disabled:opacity-50"
    >
      {children}
    </button>
  )
}

function SecondaryButton({
  children,
  onClick,
  disabled,
}: {
  children: React.ReactNode
  onClick?: () => void
  disabled?: boolean
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      disabled={disabled}
      className="px-3 py-1.5 text-sm rounded-md border border-border-default bg-surface-0 text-text-primary hover:bg-surface-1 disabled:opacity-50"
    >
      {children}
    </button>
  )
}

function DangerButton({
  children,
  onClick,
  disabled,
}: {
  children: React.ReactNode
  onClick?: () => void
  disabled?: boolean
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      disabled={disabled}
      className="px-3 py-1.5 text-sm rounded-md border border-danger/40 text-danger hover:bg-danger/10 disabled:opacity-50"
    >
      {children}
    </button>
  )
}

function ProgressBar({ pct }: { pct: number }) {
  const clamped = Math.max(0, Math.min(100, pct))
  const color = clamped >= 100 ? 'bg-danger' : clamped >= 80 ? 'bg-warning' : 'bg-brand'
  return (
    <div className="h-2 w-full rounded-full bg-surface-0 overflow-hidden border border-border-default">
      <div className={`h-full ${color}`} style={{ width: `${clamped}%` }} />
    </div>
  )
}

// ── Overview ────────────────────────────────────────────────────────

function OverviewSection({ orgId }: { orgId: string }) {
  const startOfMonthIso = useMemo(() => {
    const d = new Date()
    d.setDate(1)
    d.setHours(0, 0, 0, 0)
    return d.toISOString()
  }, [])

  const summary = useQuery({
    queryKey: ['gov', orgId, 'usage', 'summary', 'today'],
    queryFn: () => api.orgs.governance.usage.summary(orgId),
    enabled: !!orgId,
  })

  // Separate MTD query so the monthly spend-cap progress reflects
  // month-to-date spend, not just today's. Without this the monthly cap
  // can look safely under-cap when MTD has already blown past the limit.
  const summaryMtd = useQuery({
    queryKey: ['gov', orgId, 'usage', 'summary', 'mtd', startOfMonthIso],
    queryFn: () => api.orgs.governance.usage.summary(orgId, startOfMonthIso),
    enabled: !!orgId,
  })

  const spendCaps = useQuery({
    queryKey: ['gov', orgId, 'spendCaps'],
    queryFn: () => api.orgs.governance.spendCaps.list(orgId),
    enabled: !!orgId,
  })

  const modelPolicy = useQuery({
    queryKey: ['gov', orgId, 'modelPolicy'],
    queryFn: () => api.orgs.governance.modelPolicy.get(orgId),
    enabled: !!orgId,
  })

  const contentPolicies = useQuery({
    queryKey: ['gov', orgId, 'contentPolicies'],
    queryFn: () => api.orgs.governance.contentPolicies.list(orgId),
    enabled: !!orgId,
  })

  const taskPolicy = useQuery({
    queryKey: ['gov', orgId, 'taskPolicy'],
    queryFn: () => api.orgs.governance.taskPolicy.get(orgId),
    enabled: !!orgId,
  })

  const prompts = useQuery({
    queryKey: ['gov', orgId, 'prompts'],
    queryFn: () => api.orgs.governance.prompts.list(orgId),
    enabled: !!orgId,
  })

  const auditSettings = useQuery({
    queryKey: ['gov', orgId, 'auditSettings'],
    queryFn: () => api.orgs.governance.auditSettings.get(orgId),
    enabled: !!orgId,
  })

  const since24h = useMemo(() => isoSinceHoursAgo(24), [])
  const violations = useQuery({
    queryKey: ['gov', orgId, 'violations', '24h'],
    queryFn: () => api.orgs.governance.violations.list(orgId, { limit: 10, since: since24h }),
    enabled: !!orgId,
  })

  const startOfTodayIso = useMemo(() => {
    const d = new Date()
    d.setHours(0, 0, 0, 0)
    return d.toISOString()
  }, [])
  const blockedToday = useQuery({
    queryKey: ['gov', orgId, 'violations', 'blocked-today'],
    queryFn: () =>
      api.orgs.governance.violations.list(orgId, {
        action_taken: 'blocked',
        since: startOfTodayIso,
        limit: 250,
      }),
    enabled: !!orgId,
  })

  // Notification status — is any channel subscribed to violation.fired?
  // Uses a single backend summary endpoint instead of an N+1 client
  // fan-out across all channels.
  const notifySummary = useQuery({
    queryKey: ['gov-notify-status', orgId, 'violation.fired'],
    queryFn: () => api.orgs.notify.subscriptionSummary(orgId, 'violation.fired'),
    enabled: !!orgId,
    staleTime: 60_000,
  })

  const hasViolationFiredSub = useMemo(() => {
    if (!notifySummary.data) return null
    return notifySummary.data.subscribed
  }, [notifySummary.data])

  const usage: GovUsageSummary | undefined = summary.data
  const todayCostMicros = usage?.cost_micros ?? 0
  const mtdCostMicros = summaryMtd.data?.cost_micros ?? 0

  // Hierarchy banner counts
  const enforcingPolicies = useMemo(() => {
    let n = 0
    if (modelPolicy.data) n += 1
    n += (contentPolicies.data ?? []).filter((p) => p.action === 'block' && p.enabled).length
    n += (spendCaps.data ?? []).filter((c) => c.enforcement === 'hard').length
    return n
  }, [modelPolicy.data, contentPolicies.data, spendCaps.data])

  const blockedTodayCount = blockedToday.data?.length ?? 0
  const monthlyCap = spendCaps.data?.find((c) => c.window_kind === 'monthly')
  const monthlyPct =
    monthlyCap && monthlyCap.cap_micros > 0
      ? Math.round((mtdCostMicros / monthlyCap.cap_micros) * 100)
      : null
  const immutableOn = auditSettings.data?.immutable ?? false

  return (
    <div className="space-y-6">
      {hasViolationFiredSub === false && (
        <div className="rounded-md border border-warning/40 bg-warning/10 px-3 py-2 text-sm text-text-primary">
          You won&apos;t be notified when violations fire.{' '}
          <a
            href="/dashboard/org/notifications"
            className="underline text-brand hover:text-brand-strong"
          >
            Configure a channel
          </a>
          .
        </div>
      )}

      <div className="rounded-md border border-border-default bg-surface-1 px-3 py-2 text-sm text-text-secondary">
        <span className="text-text-primary font-medium">{enforcingPolicies}</span>{' '}
        {enforcingPolicies === 1 ? 'policy' : 'policies'} enforcing
        {' · '}
        <span className="text-text-primary font-medium">{blockedTodayCount}</span> blocked today
        {' · '}
        {monthlyPct !== null ? (
          <>
            <span className="text-text-primary font-medium">{monthlyPct}%</span>/cap monthly
          </>
        ) : (
          <>no monthly cap</>
        )}
        {' · '}
        immutable{' '}
        <span className={immutableOn ? 'text-success font-medium' : 'text-text-secondary'}>
          {immutableOn ? 'on' : 'off'}
        </span>
      </div>

      <div className="grid grid-cols-1 md:grid-cols-3 gap-4">
        <Card title="Today's spend vs cap">
          {summary.isLoading ? (
            <p className="text-sm text-text-secondary">Loading…</p>
          ) : (
            <>
              <div className="text-2xl font-semibold text-text-primary">
                {microsToUsd(todayCostMicros)}
              </div>
              <div className="text-xs text-text-secondary mt-1">
                {usage?.requests ?? 0} requests today
              </div>
              <div className="mt-4 space-y-3">
                {spendCaps.data?.length ? (
                  spendCaps.data.map((cap) => {
                    // Daily cap is measured against today's spend, monthly
                    // cap against month-to-date — using one value for both
                    // would silently mask MTD overruns.
                    const spent =
                      cap.window_kind === 'monthly' ? mtdCostMicros : todayCostMicros
                    const pct = cap.cap_micros > 0 ? (spent / cap.cap_micros) * 100 : 0
                    return (
                      <div key={cap.id}>
                        <div className="flex items-center justify-between text-xs text-text-secondary mb-1">
                          <span className="capitalize">{cap.window_kind} cap ({cap.enforcement})</span>
                          <span>{microsToUsd(spent)} / {microsToUsd(cap.cap_micros)}</span>
                        </div>
                        <ProgressBar pct={pct} />
                      </div>
                    )
                  })
                ) : (
                  <EmptyState>No spend caps configured.</EmptyState>
                )}
              </div>
            </>
          )}
        </Card>

        <Card title="Active policies">
          <ul className="space-y-2 text-sm">
            <li className="flex justify-between">
              <span className="text-text-secondary">Model policy</span>
              <span className="text-text-primary font-medium">
                {modelPolicy.data ? 'Enabled' : '—'}
              </span>
            </li>
            <li className="flex justify-between">
              <span className="text-text-secondary">Content policies</span>
              <span className="text-text-primary font-medium">
                {contentPolicies.data?.length ?? 0}
              </span>
            </li>
            <li className="flex justify-between">
              <span className="text-text-secondary">Task policy</span>
              <span className="text-text-primary font-medium">
                {taskPolicy.data ? 'Enabled' : '—'}
              </span>
            </li>
            <li className="flex justify-between">
              <span className="text-text-secondary">Prompt overrides</span>
              <span className="text-text-primary font-medium">
                {prompts.data?.length ?? 0}
              </span>
            </li>
          </ul>
        </Card>

        <Card title="Recent violations (24h)">
          {violations.isLoading ? (
            <p className="text-sm text-text-secondary">Loading…</p>
          ) : violations.data?.length ? (
            <ul className="space-y-2 text-xs">
              {violations.data.map((v) => (
                <li key={v.id} className="border-b border-border-default pb-2 last:border-0 last:pb-0">
                  <div className="flex items-center justify-between">
                    <span className="font-mono text-text-secondary">{formatTimestamp(v.created_at)}</span>
                    <span className="text-text-primary">{v.action_taken}</span>
                  </div>
                  <div className="text-text-secondary mt-0.5">{v.policy_kind}</div>
                </li>
              ))}
            </ul>
          ) : (
            <EmptyState>No violations in the last 24 hours.</EmptyState>
          )}
        </Card>
      </div>
    </div>
  )
}

// ── Models ──────────────────────────────────────────────────────────

function ModelsSection({ orgId }: { orgId: string }) {
  const qc = useQueryClient()
  const policy = useQuery({
    queryKey: ['gov', orgId, 'modelPolicy'],
    queryFn: () => api.orgs.governance.modelPolicy.get(orgId),
    enabled: !!orgId,
  })

  const [mode, setMode] = useState<'allow' | 'deny'>('allow')
  const [modelsText, setModelsText] = useState('')
  const [loaded, setLoaded] = useState(false)

  useEffect(() => {
    if (!loaded && policy.data) {
      setMode((policy.data.mode === 'deny' ? 'deny' : 'allow'))
      setModelsText((policy.data.models ?? []).join('\n'))
      setLoaded(true)
    } else if (!loaded && policy.isFetched && !policy.data) {
      setLoaded(true)
    }
  }, [policy.data, policy.isFetched, loaded])

  const save = useMutation({
    mutationFn: () => {
      const models = modelsText
        .split('\n')
        .map((m) => m.trim())
        .filter((m) => m.length > 0)
      return api.orgs.governance.modelPolicy.put(orgId, mode, models)
    },
    onSuccess: () => qc.invalidateQueries({ queryKey: ['gov', orgId, 'modelPolicy'] }),
  })

  const clear = useMutation({
    mutationFn: () => api.orgs.governance.modelPolicy.delete(orgId),
    onSuccess: () => {
      setMode('allow')
      setModelsText('')
      qc.invalidateQueries({ queryKey: ['gov', orgId, 'modelPolicy'] })
    },
  })

  return (
    <div className="space-y-4 max-w-2xl">
      <div>
        <h2 className="text-lg font-semibold text-text-primary">Model policy</h2>
        <p className="text-sm text-text-secondary mt-1">
          Restrict which models agents in this org can use. Lines should match the
          full model id (e.g. <code className="font-mono bg-surface-0 px-1 rounded text-xs">anthropic/claude-3-5-sonnet-20241022</code>).
        </p>
      </div>

      <div className="rounded-md border border-border-default bg-surface-1 p-4 space-y-4">
        <div>
          <label className="block text-xs font-medium text-text-secondary mb-1">Mode</label>
          <div className="flex gap-2">
            {(['allow', 'deny'] as const).map((m) => (
              <button
                key={m}
                type="button"
                onClick={() => setMode(m)}
                className={`px-3 py-1.5 text-sm rounded-md border ${
                  mode === m
                    ? 'bg-brand text-surface-0 border-brand'
                    : 'border-border-default bg-surface-0 text-text-primary hover:bg-surface-1'
                }`}
              >
                {m === 'allow' ? 'Allow list' : 'Deny list'}
              </button>
            ))}
          </div>
          <p className="text-xs text-text-secondary mt-1">
            {mode === 'allow'
              ? 'Only the listed models are usable.'
              : 'All models except the listed ones are usable.'}
          </p>
        </div>

        <div>
          <label className="block text-xs font-medium text-text-secondary mb-1">Models (one per line)</label>
          <textarea
            value={modelsText}
            onChange={(e) => setModelsText(e.target.value)}
            rows={8}
            placeholder="anthropic/claude-3-5-sonnet-20241022"
            className="w-full px-3 py-2 text-sm font-mono rounded-md border border-border-default bg-surface-0 text-text-primary"
          />
        </div>

        <ErrorBanner error={save.error ?? clear.error} />

        <div className="flex items-center gap-2">
          <PrimaryButton onClick={() => save.mutate()} disabled={save.isPending}>
            {save.isPending ? 'Saving…' : 'Save policy'}
          </PrimaryButton>
          <DangerButton onClick={() => clear.mutate()} disabled={clear.isPending || !policy.data}>
            {clear.isPending ? 'Clearing…' : 'Clear policy'}
          </DangerButton>
        </div>
      </div>
    </div>
  )
}

// ── Spending ────────────────────────────────────────────────────────

function SpendCapRow({
  orgId,
  windowKind,
  current,
}: {
  orgId: string
  windowKind: 'daily' | 'monthly'
  current?: GovSpendCap
}) {
  const qc = useQueryClient()
  const [editing, setEditing] = useState(false)
  const [usd, setUsd] = useState(current ? (current.cap_micros / 1_000_000).toFixed(2) : '')
  const [enforcement, setEnforcement] = useState<'soft' | 'hard'>(current?.enforcement ?? 'soft')

  useEffect(() => {
    if (current) {
      setUsd((current.cap_micros / 1_000_000).toFixed(2))
      setEnforcement(current.enforcement)
    } else {
      // When the cap is removed (or the parent re-mounts on org switch
      // without a current cap), clear the edit state so a fresh "Set
      // cap" doesn't silently prefill the previously-saved values.
      setUsd('')
      setEnforcement('soft')
    }
  }, [current])

  const save = useMutation({
    mutationFn: () =>
      api.orgs.governance.spendCaps.put(orgId, windowKind, usdToMicros(usd), enforcement),
    onSuccess: () => {
      setEditing(false)
      qc.invalidateQueries({ queryKey: ['gov', orgId, 'spendCaps'] })
    },
  })

  const remove = useMutation({
    mutationFn: () => api.orgs.governance.spendCaps.delete(orgId, windowKind),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['gov', orgId, 'spendCaps'] }),
  })

  return (
    <div className="rounded-md border border-border-default bg-surface-1 p-4">
      <div className="flex items-center justify-between mb-2">
        <h3 className="text-sm font-semibold text-text-primary capitalize">{windowKind} cap</h3>
        {!editing && (
          <div className="flex gap-2">
            <SecondaryButton onClick={() => setEditing(true)}>
              {current ? 'Edit' : 'Set cap'}
            </SecondaryButton>
            {current && (
              <DangerButton onClick={() => remove.mutate()} disabled={remove.isPending}>
                Remove
              </DangerButton>
            )}
          </div>
        )}
      </div>

      {!editing && (
        current ? (
          <div className="text-sm text-text-secondary">
            <span className="text-text-primary font-medium">{microsToUsd(current.cap_micros)}</span>{' '}
            ({current.enforcement === 'hard' ? 'hard block' : 'soft warn'})
          </div>
        ) : (
          <EmptyState>No {windowKind} cap configured.</EmptyState>
        )
      )}
      {!editing && remove.isError && (
        <div className="mt-2 text-xs text-danger">{isErrorMessage(remove.error)}</div>
      )}

      {editing && (
        <div className="space-y-3">
          <div className="flex gap-3 items-end">
            <div className="flex-1">
              <label className="block text-xs font-medium text-text-secondary mb-1">Cap (USD)</label>
              <input
                type="number"
                min="0"
                step="0.01"
                value={usd}
                onChange={(e) => setUsd(e.target.value)}
                className="w-full px-3 py-2 text-sm rounded-md border border-border-default bg-surface-0 text-text-primary"
              />
            </div>
            <div>
              <label className="block text-xs font-medium text-text-secondary mb-1">Enforcement</label>
              <select
                value={enforcement}
                onChange={(e) => setEnforcement(e.target.value as 'soft' | 'hard')}
                className="px-3 py-2 text-sm rounded-md border border-border-default bg-surface-0 text-text-primary"
              >
                <option value="soft">Soft (warn)</option>
                <option value="hard">Hard (block)</option>
              </select>
            </div>
          </div>
          <ErrorBanner error={save.error ?? remove.error} />
          <div className="flex gap-2">
            <PrimaryButton onClick={() => save.mutate()} disabled={save.isPending}>
              {save.isPending ? 'Saving…' : 'Save cap'}
            </PrimaryButton>
            <SecondaryButton onClick={() => setEditing(false)} disabled={save.isPending}>
              Cancel
            </SecondaryButton>
          </div>
        </div>
      )}
    </div>
  )
}

function SpendingSection({ orgId }: { orgId: string }) {
  const spendCaps = useQuery({
    queryKey: ['gov', orgId, 'spendCaps'],
    queryFn: () => api.orgs.governance.spendCaps.list(orgId),
    enabled: !!orgId,
  })

  const byModel = useQuery({
    queryKey: ['gov', orgId, 'usage', 'byModel'],
    queryFn: () => api.orgs.governance.usage.byModel(orgId),
    enabled: !!orgId,
  })

  const [leaderKind, setLeaderKind] = useState<'tasks' | 'agents' | 'users'>('tasks')

  const leaderboard = useQuery({
    queryKey: ['gov', orgId, 'leaderboard', leaderKind],
    queryFn: () => api.orgs.governance.usage.leaderboard(orgId, leaderKind),
    enabled: !!orgId,
  })

  const daily = spendCaps.data?.find((c) => c.window_kind === 'daily')
  const monthly = spendCaps.data?.find((c) => c.window_kind === 'monthly')

  return (
    <div className="space-y-6 max-w-4xl">
      <div>
        <h2 className="text-lg font-semibold text-text-primary">Spending</h2>
        <p className="text-sm text-text-secondary mt-1">
          Set caps on org spend and review where spend is going.
        </p>
      </div>

      <div className="grid grid-cols-1 md:grid-cols-2 gap-4">
        <SpendCapRow orgId={orgId} windowKind="daily" current={daily} />
        <SpendCapRow orgId={orgId} windowKind="monthly" current={monthly} />
      </div>

      <div>
        <h3 className="text-sm font-semibold text-text-primary mb-2">By model</h3>
        <div className="rounded-md border border-border-default bg-surface-1 overflow-hidden">
          {byModel.isLoading ? (
            <p className="p-4 text-sm text-text-secondary">Loading…</p>
          ) : byModel.data?.entries.length ? (
            <table className="w-full text-sm">
              <thead className="bg-surface-0 text-text-secondary text-xs">
                <tr>
                  <th className="text-left px-3 py-2 font-medium">Model</th>
                  <th className="text-right px-3 py-2 font-medium">Requests</th>
                  <th className="text-right px-3 py-2 font-medium">Cost</th>
                </tr>
              </thead>
              <tbody>
                {byModel.data.entries.map((row: GovByModelEntry) => (
                  <tr key={row.model} className="border-t border-border-default">
                    <td className="px-3 py-2 font-mono text-xs text-text-primary">{row.model}</td>
                    <td className="px-3 py-2 text-right text-text-primary">{row.requests}</td>
                    <td className="px-3 py-2 text-right text-text-primary">{microsToUsd(row.cost_micros)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          ) : (
            <div className="p-4">
              <EmptyState>No model usage yet.</EmptyState>
            </div>
          )}
        </div>
      </div>

      <div>
        <div className="flex items-center gap-2 mb-2">
          <h3 className="text-sm font-semibold text-text-primary">Leaderboard</h3>
          <div className="flex gap-1">
            {(['tasks', 'agents', 'users'] as const).map((k) => (
              <button
                key={k}
                type="button"
                onClick={() => setLeaderKind(k)}
                className={`text-xs px-2 py-1 rounded ${
                  leaderKind === k
                    ? 'bg-brand text-surface-0'
                    : 'bg-surface-0 text-text-secondary border border-border-default hover:bg-surface-1'
                }`}
              >
                {k}
              </button>
            ))}
          </div>
        </div>
        <div className="rounded-md border border-border-default bg-surface-1 overflow-hidden">
          {leaderboard.isLoading ? (
            <p className="p-4 text-sm text-text-secondary">Loading…</p>
          ) : leaderboard.data?.entries.length ? (
            <table className="w-full text-sm">
              <thead className="bg-surface-0 text-text-secondary text-xs">
                <tr>
                  <th className="text-left px-3 py-2 font-medium">Label</th>
                  <th className="text-right px-3 py-2 font-medium">Requests</th>
                  <th className="text-right px-3 py-2 font-medium">Cost</th>
                </tr>
              </thead>
              <tbody>
                {leaderboard.data.entries.map((row: GovLeaderboardEntry) => (
                  <tr key={row.id} className="border-t border-border-default">
                    <td className="px-3 py-2 text-text-primary">
                      <span className="font-mono text-xs text-text-secondary mr-2">{truncateId(row.id)}</span>
                      {row.label}
                    </td>
                    <td className="px-3 py-2 text-right text-text-primary">{row.requests}</td>
                    <td className="px-3 py-2 text-right text-text-primary">{microsToUsd(row.cost_micros)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          ) : (
            <div className="p-4">
              <EmptyState>No {leaderKind} activity yet.</EmptyState>
            </div>
          )}
        </div>
      </div>
    </div>
  )
}

// ── Content ─────────────────────────────────────────────────────────

interface ContentDraft {
  name: string
  pattern: string
  pattern_kind: 'regex' | 'keyword'
  action: 'block' | 'flag'
  block_message: string
  enabled: boolean
}

function emptyContentDraft(): ContentDraft {
  return {
    name: '',
    pattern: '',
    pattern_kind: 'keyword',
    action: 'flag',
    block_message: '',
    enabled: true,
  }
}

function ContentPolicyForm({
  orgId,
  existing,
  onDone,
}: {
  orgId: string
  existing?: GovContentPolicy
  onDone?: () => void
}) {
  const qc = useQueryClient()
  const [draft, setDraft] = useState<ContentDraft>(() =>
    existing
      ? {
          name: existing.name,
          pattern: existing.pattern,
          pattern_kind: existing.pattern_kind,
          action: existing.action,
          block_message: existing.block_message ?? '',
          enabled: existing.enabled,
        }
      : emptyContentDraft(),
  )
  const [sample, setSample] = useState('')

  const mut = useMutation({
    mutationFn: () => {
      const body: Partial<GovContentPolicy> = {
        name: draft.name.trim(),
        pattern: draft.pattern,
        pattern_kind: draft.pattern_kind,
        action: draft.action,
        enabled: draft.enabled,
      }
      if (draft.action === 'block' && draft.block_message.trim()) {
        body.block_message = draft.block_message.trim().slice(0, 500)
      }
      if (existing) {
        return api.orgs.governance.contentPolicies.update(orgId, existing.id, body)
      }
      return api.orgs.governance.contentPolicies.create(orgId, body)
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['gov', orgId, 'contentPolicies'] })
      if (!existing) setDraft(emptyContentDraft())
      onDone?.()
    },
  })

  const regexResult = useMemo(() => {
    if (draft.pattern_kind !== 'regex' || !draft.pattern || !sample) return null
    try {
      const re = new RegExp(draft.pattern)
      return { ok: true, matches: re.test(sample) }
    } catch (err) {
      return { ok: false, error: isErrorMessage(err) }
    }
  }, [draft.pattern, draft.pattern_kind, sample])

  return (
    <div className="rounded-md border border-border-default bg-surface-1 p-4 space-y-3">
      <h3 className="text-sm font-semibold text-text-primary">
        {existing ? 'Edit policy' : 'Add content policy'}
      </h3>
      <div className="grid grid-cols-1 md:grid-cols-2 gap-3">
        <div>
          <label className="block text-xs font-medium text-text-secondary mb-1">Name</label>
          <input
            value={draft.name}
            onChange={(e) => setDraft({ ...draft, name: e.target.value })}
            className="w-full px-3 py-2 text-sm rounded-md border border-border-default bg-surface-0 text-text-primary"
          />
        </div>
        <div>
          <label className="block text-xs font-medium text-text-secondary mb-1">Pattern kind</label>
          <select
            value={draft.pattern_kind}
            onChange={(e) =>
              setDraft({ ...draft, pattern_kind: e.target.value as 'regex' | 'keyword' })
            }
            className="w-full px-3 py-2 text-sm rounded-md border border-border-default bg-surface-0 text-text-primary"
          >
            <option value="keyword">Keyword</option>
            <option value="regex">Regex</option>
          </select>
        </div>
      </div>
      <div>
        <label className="block text-xs font-medium text-text-secondary mb-1">Pattern</label>
        <input
          value={draft.pattern}
          onChange={(e) => setDraft({ ...draft, pattern: e.target.value })}
          className="w-full px-3 py-2 text-sm font-mono rounded-md border border-border-default bg-surface-0 text-text-primary"
        />
      </div>
      <div className="grid grid-cols-1 md:grid-cols-2 gap-3">
        <div>
          <label className="block text-xs font-medium text-text-secondary mb-1">Action</label>
          <select
            value={draft.action}
            onChange={(e) => setDraft({ ...draft, action: e.target.value as 'block' | 'flag' })}
            className="w-full px-3 py-2 text-sm rounded-md border border-border-default bg-surface-0 text-text-primary"
          >
            <option value="flag">Flag</option>
            <option value="block">Block</option>
          </select>
        </div>
        <div className="flex items-end">
          <label className="inline-flex items-center gap-2 text-sm text-text-primary">
            <input
              type="checkbox"
              checked={draft.enabled}
              onChange={(e) => setDraft({ ...draft, enabled: e.target.checked })}
            />
            Enabled
          </label>
        </div>
      </div>
      {draft.action === 'block' && (
        <div>
          <label className="block text-xs font-medium text-text-secondary mb-1">
            Block message (max 500 chars)
          </label>
          <textarea
            value={draft.block_message}
            onChange={(e) => setDraft({ ...draft, block_message: e.target.value.slice(0, 500) })}
            rows={2}
            className="w-full px-3 py-2 text-sm rounded-md border border-border-default bg-surface-0 text-text-primary"
          />
          <div className="text-xs text-text-secondary mt-1">
            {draft.block_message.length} / 500
          </div>
        </div>
      )}
      {draft.pattern_kind === 'regex' && (
        <div>
          <label className="block text-xs font-medium text-text-secondary mb-1">
            Test against sample input
          </label>
          <textarea
            value={sample}
            onChange={(e) => setSample(e.target.value)}
            rows={2}
            placeholder="Paste sample text to test the regex…"
            className="w-full px-3 py-2 text-sm rounded-md border border-border-default bg-surface-0 text-text-primary"
          />
          {regexResult && (
            <div
              className={`mt-1 text-xs ${
                !regexResult.ok
                  ? 'text-danger'
                  : regexResult.matches
                  ? 'text-success'
                  : 'text-text-secondary'
              }`}
            >
              {regexResult.ok
                ? regexResult.matches
                  ? 'Pattern matches sample.'
                  : 'No match.'
                : `Invalid regex: ${regexResult.error}`}
            </div>
          )}
        </div>
      )}
      <ErrorBanner error={mut.error} />
      <div className="flex gap-2">
        <PrimaryButton onClick={() => mut.mutate()} disabled={mut.isPending || !draft.name || !draft.pattern}>
          {mut.isPending ? 'Saving…' : existing ? 'Save changes' : 'Add policy'}
        </PrimaryButton>
        {existing && onDone && (
          <SecondaryButton onClick={onDone}>Cancel</SecondaryButton>
        )}
      </div>
    </div>
  )
}

function ContentSection({ orgId }: { orgId: string }) {
  const qc = useQueryClient()
  const policies = useQuery({
    queryKey: ['gov', orgId, 'contentPolicies'],
    queryFn: () => api.orgs.governance.contentPolicies.list(orgId),
    enabled: !!orgId,
  })

  const [editingId, setEditingId] = useState<string | null>(null)
  const [rowErrors, setRowErrors] = useState<Record<string, string>>({})

  const toggle = useMutation({
    mutationFn: ({ id, enabled }: { id: string; enabled: boolean }) =>
      api.orgs.governance.contentPolicies.update(orgId, id, { enabled }),
    onSuccess: (_d, vars) => {
      setRowErrors((prev) => {
        const next = { ...prev }
        delete next[vars.id]
        return next
      })
      qc.invalidateQueries({ queryKey: ['gov', orgId, 'contentPolicies'] })
    },
    onError: (err, vars) =>
      setRowErrors((prev) => ({ ...prev, [vars.id]: isErrorMessage(err) })),
  })

  const remove = useMutation({
    mutationFn: (id: string) => api.orgs.governance.contentPolicies.delete(orgId, id),
    onSuccess: (_d, id) => {
      setRowErrors((prev) => {
        const next = { ...prev }
        delete next[id]
        return next
      })
      qc.invalidateQueries({ queryKey: ['gov', orgId, 'contentPolicies'] })
    },
    onError: (err, id) =>
      setRowErrors((prev) => ({ ...prev, [id]: isErrorMessage(err) })),
  })

  return (
    <div className="space-y-6 max-w-4xl">
      <div>
        <h2 className="text-lg font-semibold text-text-primary">Content policies</h2>
        <p className="text-sm text-text-secondary mt-1">
          Match patterns on agent inputs and outputs to block or flag sensitive content.
        </p>
      </div>

      <div className="rounded-md border border-border-default bg-surface-1 overflow-hidden">
        {policies.isLoading ? (
          <p className="p-4 text-sm text-text-secondary">Loading…</p>
        ) : policies.data?.length ? (
          <div className="overflow-x-auto">
            <table className="w-full text-sm">
              <thead className="bg-surface-0 text-text-secondary text-xs">
                <tr>
                  <th className="text-left px-3 py-2 font-medium">Name</th>
                  <th className="text-left px-3 py-2 font-medium">Pattern</th>
                  <th className="text-left px-3 py-2 font-medium">Kind</th>
                  <th className="text-left px-3 py-2 font-medium">Action</th>
                  <th className="text-left px-3 py-2 font-medium">Enabled</th>
                  <th className="text-right px-3 py-2 font-medium">Actions</th>
                </tr>
              </thead>
              <tbody>
                {policies.data.flatMap((p) => {
                  const rows = [
                    <tr key={p.id} className="border-t border-border-default">
                      <td className="px-3 py-2 text-text-primary min-w-0">{p.name}</td>
                      <td className="px-3 py-2 font-mono text-xs text-text-primary truncate max-w-[20rem] min-w-0">
                        {p.pattern}
                      </td>
                      <td className="px-3 py-2 text-text-secondary">{p.pattern_kind}</td>
                      <td className="px-3 py-2">
                        <span
                          className={`text-xs px-2 py-0.5 rounded ${
                            p.action === 'block'
                              ? 'bg-danger/15 text-danger'
                              : 'bg-warning/15 text-warning'
                          }`}
                        >
                          {p.action}
                        </span>
                      </td>
                      <td className="px-3 py-2">
                        <input
                          type="checkbox"
                          checked={p.enabled}
                          disabled={toggle.isPending}
                          onChange={(e) => toggle.mutate({ id: p.id, enabled: e.target.checked })}
                        />
                      </td>
                      <td className="px-3 py-2">
                        <div className="flex gap-2 justify-end">
                          <SecondaryButton onClick={() => setEditingId(p.id)}>Edit</SecondaryButton>
                          <DangerButton onClick={() => remove.mutate(p.id)} disabled={remove.isPending}>
                            Delete
                          </DangerButton>
                        </div>
                      </td>
                    </tr>,
                  ]
                  if (rowErrors[p.id]) {
                    rows.push(
                      <tr key={`${p.id}-err`}>
                        <td colSpan={6} className="px-3 pb-2">
                          <div className="text-xs text-danger">{rowErrors[p.id]}</div>
                        </td>
                      </tr>,
                    )
                  }
                  return rows
                })}
              </tbody>
            </table>
          </div>
        ) : (
          <div className="p-4">
            <EmptyState>
              No content policies yet. Create one to start filtering requests.
            </EmptyState>
          </div>
        )}
      </div>

      {editingId &&
        policies.data?.find((p) => p.id === editingId) && (
          <ContentPolicyForm
            orgId={orgId}
            existing={policies.data.find((p) => p.id === editingId)!}
            onDone={() => setEditingId(null)}
          />
        )}

      {!editingId && <ContentPolicyForm orgId={orgId} />}
    </div>
  )
}

// ── Tasks ───────────────────────────────────────────────────────────

function TasksSection({ orgId }: { orgId: string }) {
  const qc = useQueryClient()
  const policy = useQuery({
    queryKey: ['gov', orgId, 'taskPolicy'],
    queryFn: () => api.orgs.governance.taskPolicy.get(orgId),
    enabled: !!orgId,
  })

  const [guidance, setGuidance] = useState('')
  const [loaded, setLoaded] = useState(false)

  useEffect(() => {
    if (!loaded && policy.isFetched) {
      setGuidance(policy.data?.guidance ?? '')
      setLoaded(true)
    }
  }, [policy.data, policy.isFetched, loaded])

  const save = useMutation({
    mutationFn: () => api.orgs.governance.taskPolicy.put(orgId, guidance),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['gov', orgId, 'taskPolicy'] }),
  })

  const clear = useMutation({
    mutationFn: () => api.orgs.governance.taskPolicy.delete(orgId),
    onSuccess: () => {
      setGuidance('')
      qc.invalidateQueries({ queryKey: ['gov', orgId, 'taskPolicy'] })
    },
  })

  return (
    <div className="space-y-4 max-w-2xl">
      <div>
        <h2 className="text-lg font-semibold text-text-primary">Task guidance</h2>
        <p className="text-sm text-text-secondary mt-1">
          Org-wide guidance prepended to every task. Use it to remind agents of policy,
          tone, or required steps.
        </p>
      </div>
      <div className="rounded-md border border-border-default bg-surface-1 p-4 space-y-3">
        <textarea
          value={guidance}
          onChange={(e) => setGuidance(e.target.value)}
          rows={10}
          placeholder="Always confirm before sending external emails. Avoid making purchases over $50 without explicit approval…"
          className="w-full px-3 py-2 text-sm rounded-md border border-border-default bg-surface-0 text-text-primary"
        />
        <ErrorBanner error={save.error ?? clear.error} />
        <div className="flex gap-2">
          <PrimaryButton onClick={() => save.mutate()} disabled={save.isPending}>
            {save.isPending ? 'Saving…' : 'Save guidance'}
          </PrimaryButton>
          <DangerButton onClick={() => clear.mutate()} disabled={clear.isPending || !policy.data}>
            {clear.isPending ? 'Clearing…' : 'Clear guidance'}
          </DangerButton>
        </div>
      </div>
    </div>
  )
}

// ── Prompts ─────────────────────────────────────────────────────────

const PROMPT_KINDS: { value: 'intent_verify' | 'risk_assess'; label: string; description: string }[] = [
  {
    value: 'intent_verify',
    label: 'Intent verification',
    description: 'Prompt used to check that agent actions match the user’s stated intent.',
  },
  {
    value: 'risk_assess',
    label: 'Risk assessment',
    description: 'Prompt used to score the risk of a planned action before execution.',
  },
]

function PromptOverridePanel({
  orgId,
  kind,
  label,
  description,
  existing,
}: {
  orgId: string
  kind: 'intent_verify' | 'risk_assess'
  label: string
  description: string
  existing?: GovPromptOverride
}) {
  const qc = useQueryClient()
  const [open, setOpen] = useState(false)
  const [draft, setDraft] = useState(existing?.prompt ?? '')

  useEffect(() => {
    setDraft(existing?.prompt ?? '')
  }, [existing?.prompt])

  const save = useMutation({
    mutationFn: () => api.orgs.governance.prompts.put(orgId, kind, draft),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['gov', orgId, 'prompts'] }),
  })

  const remove = useMutation({
    mutationFn: () => api.orgs.governance.prompts.delete(orgId, kind),
    onSuccess: () => {
      setDraft('')
      qc.invalidateQueries({ queryKey: ['gov', orgId, 'prompts'] })
    },
  })

  return (
    <div className="rounded-md border border-border-default bg-surface-1">
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        className="w-full flex items-center justify-between px-4 py-3 text-left"
      >
        <div>
          <div className="text-sm font-semibold text-text-primary">{label}</div>
          <div className="text-xs text-text-secondary mt-0.5">{description}</div>
        </div>
        <span
          className={`text-xs px-2 py-0.5 rounded ${
            existing ? 'bg-brand/15 text-brand' : 'bg-surface-0 text-text-secondary border border-border-default'
          }`}
        >
          {existing ? 'Override' : 'Default'}
        </span>
      </button>
      {open && (
        <div className="border-t border-border-default p-4 space-y-3">
          <textarea
            value={draft}
            onChange={(e) => setDraft(e.target.value)}
            rows={10}
            placeholder="Leave the default in place, or paste a replacement prompt here…"
            className="w-full px-3 py-2 text-sm font-mono rounded-md border border-border-default bg-surface-0 text-text-primary"
          />
          <ErrorBanner error={save.error ?? remove.error} />
          <div className="flex gap-2">
            <PrimaryButton onClick={() => save.mutate()} disabled={save.isPending || !draft.trim()}>
              {save.isPending ? 'Saving…' : 'Save override'}
            </PrimaryButton>
            <DangerButton onClick={() => remove.mutate()} disabled={remove.isPending || !existing}>
              {remove.isPending ? 'Removing…' : 'Revert to default'}
            </DangerButton>
          </div>
        </div>
      )}
    </div>
  )
}

function PromptsSection({ orgId }: { orgId: string }) {
  const prompts = useQuery({
    queryKey: ['gov', orgId, 'prompts'],
    queryFn: () => api.orgs.governance.prompts.list(orgId),
    enabled: !!orgId,
  })

  return (
    <div className="space-y-4 max-w-3xl">
      <div>
        <h2 className="text-lg font-semibold text-text-primary">Prompt overrides</h2>
        <p className="text-sm text-text-secondary mt-1">
          Replace the built-in governance prompts with org-specific versions. Changes
          take effect for new agent runs.
        </p>
      </div>
      <div className="space-y-3">
        {PROMPT_KINDS.map((p) => (
          <PromptOverridePanel
            key={p.value}
            orgId={orgId}
            kind={p.value}
            label={p.label}
            description={p.description}
            existing={prompts.data?.find((row) => row.kind === p.value)}
          />
        ))}
      </div>
    </div>
  )
}

// ── Violations ──────────────────────────────────────────────────────

function ViolationsSection({ orgId }: { orgId: string }) {
  const [policyKind, setPolicyKind] = useState('')
  const [actionTaken, setActionTaken] = useState('')
  const [since, setSince] = useState('')
  const [until, setUntil] = useState('')
  const [limit, setLimit] = useState(50)

  const filter = useMemo(
    () => ({
      policy_kind: policyKind || undefined,
      action_taken: actionTaken || undefined,
      since: since ? new Date(since).toISOString() : undefined,
      // Until is a date-only input — pin it to end-of-day so a user
      // selecting "until 2026-06-29" actually includes that day's
      // violations instead of cutting off at 00:00:00.
      until: until ? endOfDayIso(until) : undefined,
      limit,
    }),
    [policyKind, actionTaken, since, until, limit],
  )

  const violations = useQuery({
    queryKey: ['gov', orgId, 'violations', filter],
    queryFn: () => api.orgs.governance.violations.list(orgId, filter),
    enabled: !!orgId,
  })

  const [exportError, setExportError] = useState<string | null>(null)
  const [exporting, setExporting] = useState<'csv' | 'json' | null>(null)

  // Authenticated download: a bare <a href> would hit /api/... without
  // the in-memory bearer token, 401-ing for users without a session
  // cookie. Use the API client's download() helper to attach the token
  // and stream the response as a blob.
  const onExport = async (format: 'csv' | 'json') => {
    setExporting(format)
    setExportError(null)
    try {
      const { blob, filename } = await api.orgs.governance.violations.download(orgId, format)
      const url = URL.createObjectURL(blob)
      const a = document.createElement('a')
      a.href = url
      a.download = filename ?? `violations.${format}`
      document.body.appendChild(a)
      a.click()
      a.remove()
      window.setTimeout(() => URL.revokeObjectURL(url), 0)
    } catch (err) {
      setExportError(err instanceof Error ? err.message : 'Failed to download export.')
    } finally {
      setExporting(null)
    }
  }

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between flex-wrap gap-2">
        <h2 className="text-lg font-semibold text-text-primary">Violations</h2>
        <div className="flex gap-2">
          <button
            type="button"
            onClick={() => onExport('csv')}
            disabled={exporting !== null}
            className="px-3 py-1.5 text-sm rounded-md border border-border-default bg-surface-0 text-text-primary hover:bg-surface-1 disabled:opacity-50"
          >
            {exporting === 'csv' ? 'Downloading…' : 'Download CSV'}
          </button>
          <button
            type="button"
            onClick={() => onExport('json')}
            disabled={exporting !== null}
            className="px-3 py-1.5 text-sm rounded-md border border-border-default bg-surface-0 text-text-primary hover:bg-surface-1 disabled:opacity-50"
          >
            {exporting === 'json' ? 'Downloading…' : 'Download JSON'}
          </button>
        </div>
      </div>
      {exportError && (
        <div className="rounded-md border border-danger/40 bg-danger/10 px-3 py-2 text-sm text-danger">
          {exportError}
        </div>
      )}

      <div className="rounded-md border border-border-default bg-surface-1 p-3 grid grid-cols-2 md:grid-cols-5 gap-3">
        <div>
          <label className="block text-xs font-medium text-text-secondary mb-1">Policy kind</label>
          <select
            value={policyKind}
            onChange={(e) => setPolicyKind(e.target.value)}
            className="w-full px-2 py-1.5 text-sm rounded-md border border-border-default bg-surface-0 text-text-primary"
          >
            <option value="">All</option>
            <option value="model_policy">model_policy</option>
            <option value="spend_cap">spend_cap</option>
            <option value="content_policy">content_policy</option>
            <option value="task_policy">task_policy</option>
            <option value="restriction">restriction</option>
          </select>
        </div>
        <div>
          <label className="block text-xs font-medium text-text-secondary mb-1">Action</label>
          <select
            value={actionTaken}
            onChange={(e) => setActionTaken(e.target.value)}
            className="w-full px-2 py-1.5 text-sm rounded-md border border-border-default bg-surface-0 text-text-primary"
          >
            <option value="">All</option>
            <option value="blocked">blocked</option>
            <option value="flagged">flagged</option>
            <option value="warned">warned</option>
          </select>
        </div>
        <div>
          <label className="block text-xs font-medium text-text-secondary mb-1">Since</label>
          <input
            type="date"
            value={since}
            onChange={(e) => setSince(e.target.value)}
            className="w-full px-2 py-1.5 text-sm rounded-md border border-border-default bg-surface-0 text-text-primary"
          />
        </div>
        <div>
          <label className="block text-xs font-medium text-text-secondary mb-1">Until</label>
          <input
            type="date"
            value={until}
            onChange={(e) => setUntil(e.target.value)}
            className="w-full px-2 py-1.5 text-sm rounded-md border border-border-default bg-surface-0 text-text-primary"
          />
        </div>
        <div>
          <label className="block text-xs font-medium text-text-secondary mb-1">Limit</label>
          <select
            value={limit}
            onChange={(e) => setLimit(parseInt(e.target.value, 10))}
            className="w-full px-2 py-1.5 text-sm rounded-md border border-border-default bg-surface-0 text-text-primary"
          >
            <option value={25}>25</option>
            <option value={50}>50</option>
            <option value={100}>100</option>
            <option value={250}>250</option>
          </select>
        </div>
      </div>

      <div className="rounded-md border border-border-default bg-surface-1 overflow-hidden">
        {violations.isLoading ? (
          <p className="p-4 text-sm text-text-secondary">Loading…</p>
        ) : violations.data?.length ? (
          <div className="overflow-x-auto">
            <table className="w-full text-sm">
              <thead className="bg-surface-0 text-text-secondary text-xs">
                <tr>
                  <th className="text-left px-3 py-2 font-medium">Timestamp</th>
                  <th className="text-left px-3 py-2 font-medium">Policy</th>
                  <th className="text-left px-3 py-2 font-medium">Action</th>
                  <th className="text-left px-3 py-2 font-medium">Agent</th>
                  <th className="text-left px-3 py-2 font-medium">User</th>
                  <th className="text-left px-3 py-2 font-medium">Detail</th>
                </tr>
              </thead>
              <tbody>
                {violations.data.map((v: GovViolation) => (
                  <tr key={v.id} className="border-t border-border-default align-top">
                    <td className="px-3 py-2 font-mono text-xs text-text-secondary whitespace-nowrap">
                      {formatTimestamp(v.created_at)}
                    </td>
                    <td className="px-3 py-2 text-text-primary">{v.policy_kind}</td>
                    <td className="px-3 py-2">
                      <span
                        className={`text-xs px-2 py-0.5 rounded ${
                          v.action_taken === 'blocked'
                            ? 'bg-danger/15 text-danger'
                            : v.action_taken === 'flagged'
                            ? 'bg-warning/15 text-warning'
                            : 'bg-surface-0 text-text-secondary border border-border-default'
                        }`}
                      >
                        {v.action_taken}
                      </span>
                    </td>
                    <td className="px-3 py-2 font-mono text-xs text-text-secondary">
                      {truncateId(v.agent_id)}
                    </td>
                    <td className="px-3 py-2 font-mono text-xs text-text-secondary">
                      {truncateId(v.user_id)}
                    </td>
                    <td className="px-3 py-2 text-text-primary text-xs max-w-md truncate min-w-0" title={v.detail}>
                      {v.detail}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        ) : (
          <div className="p-4">
            <EmptyState>No violations match the current filters.</EmptyState>
          </div>
        )}
      </div>
    </div>
  )
}

// ── Audit Settings ──────────────────────────────────────────────────

// normalizeConfirm collapses non-breaking spaces, en-dashes, em-dashes,
// and runs of internal whitespace down to a single ASCII space, then
// trims. Used by the immutability typed-name confirmation so users
// aren't locked out by invisible character differences.
function normalizeConfirm(s: string): string {
  return s
    .normalize('NFKC')
    .replace(/[–—]/g, '-') // en-dash, em-dash → hyphen
    .replace(/[ \s]+/g, ' ')    // NBSP + any whitespace run → single space
    .trim()
}

function AuditSettingsSection({ orgId }: { orgId: string }) {
  const qc = useQueryClient()
  const { currentOrg } = useAuth()
  const orgName = currentOrg?.name ?? ''
  const settings = useQuery({
    queryKey: ['gov', orgId, 'auditSettings'],
    queryFn: () => api.orgs.governance.auditSettings.get(orgId),
    enabled: !!orgId,
  })

  const [retention, setRetention] = useState<number>(0)
  const [immutable, setImmutable] = useState(false)
  const [loaded, setLoaded] = useState(false)
  const [confirmOpen, setConfirmOpen] = useState(false)
  const [confirmName, setConfirmName] = useState('')

  useEffect(() => {
    if (!loaded && settings.data) {
      setRetention(settings.data.retention_days)
      setImmutable(settings.data.immutable)
      setLoaded(true)
    }
  }, [settings.data, loaded])

  const alreadyImmutable = settings.data?.immutable ?? false

  const save = useMutation({
    mutationFn: (turnImmutableOn: boolean) => {
      const body: Partial<GovAuditSettings> = {
        retention_days: retention,
      }
      // Only allow turning immutable ON; once set, the API blocks changes anyway.
      if (!alreadyImmutable && turnImmutableOn) {
        body.immutable = true
      }
      return api.orgs.governance.auditSettings.put(orgId, body)
    },
    onSuccess: () => {
      setConfirmOpen(false)
      setConfirmName('')
      qc.invalidateQueries({ queryKey: ['gov', orgId, 'auditSettings'] })
    },
  })

  const onSaveClick = () => {
    // Immutable toggle requires a typed-name confirmation; everything else
    // saves directly.
    if (!alreadyImmutable && immutable) {
      setConfirmName('')
      setConfirmOpen(true)
      return
    }
    save.mutate(false)
  }

  // Normalize before compare so non-breaking spaces, em-dashes, or
  // multiple internal whitespace don't lock the admin out of confirm.
  // The displayed org name passes through normalizeConfirm too so the
  // user sees exactly what they're expected to type.
  const confirmDisabled =
    save.isPending ||
    normalizeConfirm(confirmName) !== normalizeConfirm(orgName) ||
    normalizeConfirm(orgName) === ''

  return (
    <div className="space-y-4 max-w-2xl">
      <div>
        <h2 className="text-lg font-semibold text-text-primary">Audit settings</h2>
        <p className="text-sm text-text-secondary mt-1">
          Control how long violations and audit entries are retained, and whether
          they can ever be deleted.
        </p>
      </div>
      <div className="rounded-md border border-border-default bg-surface-1 p-4 space-y-4">
        <div>
          <label className="block text-xs font-medium text-text-secondary mb-1">
            Retention (days)
          </label>
          <input
            type="number"
            min={0}
            max={3650}
            value={retention}
            onChange={(e) =>
              setRetention(Math.max(0, Math.min(3650, parseInt(e.target.value || '0', 10) || 0)))
            }
            className="w-32 px-3 py-2 text-sm rounded-md border border-border-default bg-surface-0 text-text-primary"
          />
          <p className="text-xs text-text-secondary mt-1">
            0 means use the system default. Max 3650 (10 years).
          </p>
        </div>
        <div>
          <label className="inline-flex items-start gap-2 text-sm text-text-primary">
            <input
              type="checkbox"
              checked={immutable}
              disabled={alreadyImmutable}
              onChange={(e) => setImmutable(e.target.checked)}
              className="mt-0.5"
            />
            <span>
              <span className="font-medium">Make violations immutable</span>
              <span className="block text-xs text-text-secondary mt-0.5">
                {alreadyImmutable
                  ? 'Immutability is permanently on for this org.'
                  : 'This cannot be undone. The retention sweeper will skip this org’s violations entirely once immutable is on.'}
              </span>
            </span>
          </label>
        </div>
        <ErrorBanner error={save.error} />
        <div>
          <PrimaryButton
            onClick={onSaveClick}
            disabled={save.isPending || !settings.data}
          >
            {save.isPending ? 'Saving…' : 'Save settings'}
          </PrimaryButton>
        </div>
      </div>

      {confirmOpen && (
        <div className="fixed inset-0 z-50 flex items-center justify-center">
          <div
            className="absolute inset-0 bg-black/60"
            onClick={() => {
              if (!save.isPending) {
                setConfirmOpen(false)
                setConfirmName('')
              }
            }}
          />
          <div className="relative bg-surface-1 border border-border-default rounded-lg w-full max-w-md mx-4 shadow-xl">
            <div className="px-6 py-4 border-b border-border-default">
              <h2 className="text-base font-semibold text-text-primary">
                Make violations permanently immutable
              </h2>
            </div>
            <div className="px-6 py-4 space-y-3">
              <p className="text-sm text-text-primary">
                Turning this on for <span className="font-semibold">{orgName}</span> has the
                following consequences:
              </p>
              <ul className="list-disc pl-5 text-sm text-text-secondary space-y-1">
                <li>Retention sweeper will skip violation deletion for this org forever.</li>
                <li>No admin can re-enable deletion through the UI or API.</li>
                <li>
                  Required for SOC2 evidence collection; treat as a one-way legal decision.
                </li>
              </ul>
              <div>
                <label
                  htmlFor="confirm-immutable-name"
                  className="block text-xs font-medium text-text-secondary mb-1"
                >
                  Type the org name <span className="font-mono text-text-primary">{orgName}</span> to confirm
                </label>
                <input
                  id="confirm-immutable-name"
                  value={confirmName}
                  onChange={(e) => setConfirmName(e.target.value)}
                  autoFocus
                  className="w-full px-3 py-2 text-sm rounded-md border border-border-default bg-surface-0 text-text-primary"
                />
              </div>
              <ErrorBanner error={save.error} />
            </div>
            <div className="flex items-center justify-end gap-2 px-6 py-3 border-t border-border-default">
              <SecondaryButton
                onClick={() => {
                  setConfirmOpen(false)
                  setConfirmName('')
                  // Revert local checkbox so user has to opt-in again.
                  setImmutable(false)
                }}
                disabled={save.isPending}
              >
                Cancel
              </SecondaryButton>
              <button
                type="button"
                onClick={() => save.mutate(true)}
                disabled={confirmDisabled}
                className="px-3 py-1.5 text-sm font-medium rounded-md bg-danger text-surface-0 hover:bg-danger/90 disabled:opacity-50"
              >
                {save.isPending ? 'Saving…' : 'Confirm — make permanent'}
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  )
}

// ── Page shell ──────────────────────────────────────────────────────

export default function Governance() {
  const { currentOrg } = useAuth()
  const [section, setSection] = useState<Section>('overview')

  if (!currentOrg) {
    return (
      <p className="text-sm text-text-secondary">
        Select an organization to view governance.
      </p>
    )
  }

  const orgId = currentOrg.id

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-xl font-semibold text-text-primary">
          Governance &mdash; {currentOrg.name}
        </h1>
        <p className="text-sm text-text-secondary mt-1">
          Policies, spend caps, content filters, and audit settings for your org.
        </p>
      </div>

      <div className="grid grid-cols-1 md:grid-cols-[12rem_minmax(0,1fr)] gap-6">
        <nav className="md:sticky md:top-4 self-start">
          <ul className="space-y-0.5">
            {SECTIONS.map((s) => (
              <li key={s.id}>
                <button
                  type="button"
                  onClick={() => setSection(s.id)}
                  className={`w-full text-left px-3 py-2 text-sm rounded-md ${
                    section === s.id
                      ? 'bg-brand-muted text-brand'
                      : 'text-text-secondary hover:bg-surface-1 hover:text-text-primary'
                  }`}
                >
                  {s.label}
                </button>
              </li>
            ))}
          </ul>
        </nav>

        <div className="min-w-0">
          {/*
            key={orgId} on each section forces a remount when the
            selected org changes. Several sections seed their form
            state from the loaded policy via a one-shot useState +
            useEffect pattern — without remounting, switching orgs
            keeps the previous org's form values in state and a Save
            click would write them into the wrong org.
          */}
          {section === 'overview' && <OverviewSection key={orgId} orgId={orgId} />}
          {section === 'models' && <ModelsSection key={orgId} orgId={orgId} />}
          {section === 'spending' && <SpendingSection key={orgId} orgId={orgId} />}
          {section === 'content' && <ContentSection key={orgId} orgId={orgId} />}
          {section === 'tasks' && <TasksSection key={orgId} orgId={orgId} />}
          {section === 'prompts' && <PromptsSection key={orgId} orgId={orgId} />}
          {section === 'violations' && <ViolationsSection key={orgId} orgId={orgId} />}
          {section === 'audit' && <AuditSettingsSection key={orgId} orgId={orgId} />}
        </div>
      </div>
    </div>
  )
}
