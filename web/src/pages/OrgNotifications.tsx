import { useEffect, useMemo, useRef, useState } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import {
  api,
  type NotifyChannel,
  type NotifyDelivery,
  type NotifySubscription,
} from '../api/client'
import { useAuth } from '../hooks/useAuth'

// ── Helpers ───────────────────────────────────────────────────────────────────

const EVENT_LABELS: Record<string, string> = {
  'violation.fired': 'Policy violation fired',
  'spend.cap_warning_80': 'Spend cap at 80%',
  'spend.cap_warning_100': 'Spend cap reached',
  'org.invite.created': 'New invite',
  'agent.created': 'New agent',
}

function humanizeEvent(eventType: string): string {
  if (EVENT_LABELS[eventType]) return EVENT_LABELS[eventType]
  return eventType.replace(/[._]/g, ' ').replace(/\b\w/g, (c) => c.toUpperCase())
}

function truncate(s: string, n = 50): string {
  if (!s) return ''
  return s.length > n ? s.slice(0, n - 1) + '…' : s
}

function formatDateTime(iso: string): string {
  if (!iso) return ''
  try { return new Date(iso).toLocaleString() } catch { return iso }
}

function isValidSlackWebhook(url: string): boolean {
  return /^https:\/\/hooks\.slack\.com\/services\/[A-Za-z0-9/_-]+$/.test(url)
}

function isValidEmail(addr: string): boolean {
  return /^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(addr)
}

type ChannelKind = 'slack_webhook' | 'email'
type DeliveryStatus = NotifyDelivery['status']

const INPUT_CLS =
  'w-full px-3 py-2 text-sm rounded-md border border-border-default bg-surface-0 text-text-primary focus:outline-none focus:ring-1 focus:ring-brand/30 focus:border-brand placeholder:text-text-tertiary'
const BTN_OUTLINE =
  'text-xs px-3 py-1.5 rounded border border-border-default hover:bg-surface-0 text-text-primary'
const BTN_PRIMARY =
  'text-xs px-3 py-1.5 rounded bg-brand text-surface-0 hover:bg-brand-strong disabled:opacity-50'

function StatusBadge({ status }: { status: DeliveryStatus }) {
  const cls: Record<DeliveryStatus, string> = {
    sent: 'bg-green-500/15 text-green-600 dark:text-green-400 border border-green-500/30',
    pending: 'bg-surface-0 text-text-secondary border border-border-default',
    failed: 'bg-yellow-500/15 text-yellow-700 dark:text-yellow-400 border border-yellow-500/30',
    dead: 'bg-danger/15 text-danger border border-danger/30',
    sampled_out: 'bg-blue-500/15 text-blue-600 dark:text-blue-400 border border-blue-500/30',
  }
  return (
    <span className={`text-xs px-1.5 py-0.5 rounded ${cls[status]}`}>
      {status.replace('_', ' ')}
    </span>
  )
}

type ColSpec = string | { label: string; align?: 'left' | 'right' }

function TableHead({ cols }: { cols: ColSpec[] }) {
  return (
    <thead className="bg-surface-0 text-xs text-text-secondary">
      <tr>
        {cols.map((c, i) => {
          const label = typeof c === 'string' ? c : c.label
          const align = typeof c === 'string' ? 'left' : c.align ?? 'left'
          return (
            <th key={i} className={`px-3 py-2 font-medium ${align === 'right' ? 'text-right' : 'text-left'}`}>
              {label}
            </th>
          )
        })}
      </tr>
    </thead>
  )
}

function EventCheckList({
  eventTypes, subs, setSubs,
}: {
  eventTypes: string[]
  subs: Set<string>
  setSubs: (next: Set<string>) => void
}) {
  return (
    <div className="space-y-1 max-h-64 overflow-y-auto border border-border-default rounded-md p-2">
      {eventTypes.length === 0 && (
        <p className="text-xs text-text-tertiary p-2">No event types available.</p>
      )}
      {eventTypes.map((et) => (
        <label key={et} className="flex items-center gap-2 p-2 rounded hover:bg-surface-0 cursor-pointer">
          <input
            type="checkbox"
            checked={subs.has(et)}
            onChange={(e) => {
              const copy = new Set(subs)
              if (e.target.checked) copy.add(et)
              else copy.delete(et)
              setSubs(copy)
            }}
          />
          <div>
            <div className="text-sm text-text-primary">{humanizeEvent(et)}</div>
            <div className="text-xs text-text-tertiary font-mono">{et}</div>
          </div>
        </label>
      ))}
    </div>
  )
}

// ── Main page ─────────────────────────────────────────────────────────────────

export default function OrgNotifications() {
  const { currentOrg } = useAuth()
  const orgId = currentOrg?.id ?? ''
  const queryClient = useQueryClient()

  const [showAdd, setShowAdd] = useState(false)
  const [editChannel, setEditChannel] = useState<NotifyChannel | null>(null)
  const [showDeliveries, setShowDeliveries] = useState(true)
  const [deliveryStatusFilter, setDeliveryStatusFilter] = useState<string>('')
  const [testedChannelId, setTestedChannelId] = useState<string | null>(null)
  const [rowErrors, setRowErrors] = useState<Record<string, string>>({})

  const channelsQ = useQuery({
    queryKey: ['notify', orgId, 'channels'],
    queryFn: () => api.orgs.notify.channels.list(orgId),
    enabled: !!orgId,
  })

  const deliveriesQ = useQuery({
    queryKey: ['notify', orgId, 'deliveries', deliveryStatusFilter],
    queryFn: () => api.orgs.notify.deliveries(orgId, deliveryStatusFilter || undefined),
    enabled: !!orgId,
  })

  // Channel "Last delivery" must reflect the truly latest delivery
  // regardless of the user-selected status filter. Using the filtered
  // list above would, for example, hide a fresh success behind a
  // "failed" filter and make the channel look unhealthier than it is.
  const allDeliveriesQ = useQuery({
    queryKey: ['notify', orgId, 'deliveries', 'all'],
    queryFn: () => api.orgs.notify.deliveries(orgId),
    enabled: !!orgId,
  })

  const eventTypesQ = useQuery({
    queryKey: ['notify', 'event_types'],
    queryFn: () => api.orgs.notify.eventTypes(),
  })

  const clearRowError = (channelId: string) =>
    setRowErrors((prev) => {
      if (!(channelId in prev)) return prev
      const next = { ...prev }
      delete next[channelId]
      return next
    })

  const deleteChannel = useMutation({
    mutationFn: (channelId: string) => api.orgs.notify.channels.delete(orgId, channelId),
    onSuccess: (_d, channelId) => {
      clearRowError(channelId)
      queryClient.invalidateQueries({ queryKey: ['notify', orgId, 'channels'] })
    },
    onError: (err, channelId) =>
      setRowErrors((prev) => ({
        ...prev,
        [channelId]: (err as Error)?.message ?? 'Failed to delete channel.',
      })),
  })

  const testChannel = useMutation({
    mutationFn: (channelId: string) => api.orgs.notify.channels.test(orgId, channelId),
    onSuccess: (_data, channelId) => {
      clearRowError(channelId)
      setTestedChannelId(channelId)
      setTimeout(() => setTestedChannelId((cur) => (cur === channelId ? null : cur)), 5000)
      queryClient.invalidateQueries({ queryKey: ['notify', orgId, 'deliveries'] })
    },
    onError: (err, channelId) =>
      setRowErrors((prev) => ({
        ...prev,
        [channelId]: (err as Error)?.message ?? 'Failed to send test.',
      })),
  })

  const lastDeliveryByChannel = useMemo(() => {
    const map = new Map<string, NotifyDelivery>()
    for (const d of allDeliveriesQ.data ?? []) {
      const existing = map.get(d.channel_id)
      if (!existing || new Date(d.created_at) > new Date(existing.created_at)) {
        map.set(d.channel_id, d)
      }
    }
    return map
  }, [allDeliveriesQ.data])

  const channelNameById = useMemo(() => {
    const map = new Map<string, string>()
    for (const c of channelsQ.data ?? []) map.set(c.id, c.name)
    return map
  }, [channelsQ.data])

  if (!currentOrg) {
    return <p className="text-sm text-text-secondary">Select an organization to manage notifications.</p>
  }

  const channels = channelsQ.data ?? []
  const deliveries = deliveriesQ.data ?? []
  const eventTypes = eventTypesQ.data ?? []

  return (
    <div className="p-4 sm:p-8 space-y-10">
      <div>
        <h1 className="text-2xl font-bold text-text-primary">Notifications</h1>
        <p className="text-sm text-text-secondary mt-2">
          Receive governance events in Slack or email.
        </p>
      </div>

      {/* Channels */}
      <section className="space-y-3">
        <div className="flex items-center justify-between">
          <h3 className="text-sm font-medium text-text-primary">Channels</h3>
          <button onClick={() => setShowAdd(true)} className={BTN_PRIMARY}>Add channel</button>
        </div>

        {channels.length === 0 ? (
          <div className="bg-surface-1 rounded-md border border-border-default p-6 text-center">
            <p className="text-sm text-text-primary font-medium">No channels yet</p>
            <p className="text-xs text-text-secondary mt-1">
              Add a Slack webhook or email address to start receiving governance events.
            </p>
            <button onClick={() => setShowAdd(true)} className={`mt-3 ${BTN_PRIMARY}`}>Add channel</button>
            <p className="text-xs text-text-secondary mt-3">
              Setting up notifications?{' '}
              <a
                href="/dashboard/org/governance"
                className="underline text-brand hover:text-brand-strong"
              >
                See your governance configuration
              </a>
              .
            </p>
          </div>
        ) : (
          <div className="bg-surface-1 rounded-md border border-border-default overflow-hidden">
            <div className="overflow-x-auto">
              <table className="w-full text-sm">
                <TableHead cols={['Name', 'Kind', 'Target', 'Last delivery', { label: 'Actions', align: 'right' }]} />
                <tbody>
                  {channels.flatMap((c) => {
                    const last = lastDeliveryByChannel.get(c.id)
                    const rows = [
                      <tr key={c.id} className="border-t border-border-default">
                        <td className="px-3 py-2 text-text-primary min-w-0">{c.name}</td>
                        <td className="px-3 py-2 text-text-secondary">{c.kind}</td>
                        <td className="px-3 py-2 text-text-secondary font-mono text-xs min-w-0">
                          {truncate(c.target_url, 40)}
                        </td>
                        <td className="px-3 py-2">
                          {last ? (
                            <div className="flex items-center gap-2">
                              <StatusBadge status={last.status} />
                              <span className="text-xs text-text-tertiary">{formatDateTime(last.created_at)}</span>
                            </div>
                          ) : (
                            <span className="text-xs text-text-tertiary">—</span>
                          )}
                        </td>
                        <td className="px-3 py-2 text-right">
                          <div className="inline-flex items-center gap-2">
                            {testedChannelId === c.id ? (
                              <span className="text-xs text-text-secondary">
                                Queued — check delivery log in ~5s.
                              </span>
                            ) : (
                              <button
                                onClick={() => testChannel.mutate(c.id)}
                                disabled={testChannel.isPending}
                                className={BTN_OUTLINE}
                              >Test</button>
                            )}
                            <button onClick={() => setEditChannel(c)} className={BTN_OUTLINE}>Edit</button>
                            <button
                              onClick={() => {
                                if (confirm(`Delete channel "${c.name}"?`)) deleteChannel.mutate(c.id)
                              }}
                              className="text-xs px-2 py-1 rounded border border-danger/40 text-danger hover:bg-danger/10"
                            >Delete</button>
                          </div>
                        </td>
                      </tr>,
                    ]
                    if (rowErrors[c.id]) {
                      rows.push(
                        <tr key={`${c.id}-err`}>
                          <td colSpan={5} className="px-3 pb-2">
                            <div className="text-xs text-danger">{rowErrors[c.id]}</div>
                          </td>
                        </tr>,
                      )
                    }
                    return rows
                  })}
                </tbody>
              </table>
            </div>
          </div>
        )}
      </section>

      {/* Delivery log */}
      <section className="space-y-3">
        <div className="flex items-center justify-between">
          <button
            onClick={() => setShowDeliveries((v) => !v)}
            className="text-sm font-medium text-text-primary hover:text-brand"
          >
            {showDeliveries ? '▾' : '▸'} Recent deliveries
          </button>
          {showDeliveries && (
            <select
              value={deliveryStatusFilter}
              onChange={(e) => setDeliveryStatusFilter(e.target.value)}
              className="text-xs px-2 py-1 rounded border border-border-default bg-surface-0 text-text-primary"
            >
              <option value="">All statuses</option>
              <option value="pending">Pending</option>
              <option value="sent">Sent</option>
              <option value="failed">Failed</option>
              <option value="dead">Dead</option>
              <option value="sampled_out">Sampled out</option>
            </select>
          )}
        </div>

        {showDeliveries && (
          <div className="bg-surface-1 rounded-md border border-border-default overflow-hidden">
            {deliveries.length === 0 ? (
              <p className="text-sm text-text-secondary p-4">No deliveries yet.</p>
            ) : (
              <div className="overflow-x-auto">
                <table className="w-full text-sm">
                  <TableHead cols={['Channel', 'Event', 'Status', 'Attempts', 'Created']} />
                  <tbody>
                    {deliveries.map((d) => (
                      <tr key={d.id} className="border-t border-border-default">
                        <td className="px-3 py-2 text-text-primary min-w-0">
                          {channelNameById.get(d.channel_id) ?? (
                            <span className="text-text-tertiary font-mono text-xs">{truncate(d.channel_id, 12)}</span>
                          )}
                        </td>
                        <td className="px-3 py-2 text-text-secondary min-w-0">{humanizeEvent(d.event_type)}</td>
                        <td className="px-3 py-2"><StatusBadge status={d.status} /></td>
                        <td className="px-3 py-2 text-text-secondary">{d.attempt_count}</td>
                        <td className="px-3 py-2 text-text-tertiary text-xs">{formatDateTime(d.created_at)}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </div>
        )}
      </section>

      {showAdd && (
        <ChannelWizardModal
          orgId={orgId}
          eventTypes={eventTypes}
          onClose={() => setShowAdd(false)}
          onSaved={() => {
            queryClient.invalidateQueries({ queryKey: ['notify', orgId, 'channels'] })
            setShowAdd(false)
          }}
        />
      )}

      {editChannel && (
        <EditChannelModal
          orgId={orgId}
          channel={editChannel}
          eventTypes={eventTypes}
          onClose={() => setEditChannel(null)}
          onSaved={() => {
            queryClient.invalidateQueries({ queryKey: ['notify', orgId, 'channels'] })
            setEditChannel(null)
          }}
        />
      )}
    </div>
  )
}

// ── Add wizard modal ──────────────────────────────────────────────────────────

interface WizardProps {
  orgId: string
  eventTypes: string[]
  onClose: () => void
  onSaved: () => void
}

function ChannelWizardModal({ orgId, eventTypes, onClose, onSaved }: WizardProps) {
  const [step, setStep] = useState<1 | 2 | 3 | 4>(1)
  const [kind, setKind] = useState<ChannelKind>('slack_webhook')
  const [name, setName] = useState('')
  const [target, setTarget] = useState('')
  const [subs, setSubs] = useState<Set<string>>(new Set())
  const [minInterval, setMinInterval] = useState(60)
  const [sampleRate, setSampleRate] = useState(1.0)
  const [error, setError] = useState<string | null>(null)

  const create = useMutation({
    mutationFn: () =>
      api.orgs.notify.channels.create(orgId, {
        kind, name, target_url: target,
        min_interval_seconds: minInterval, sample_rate: sampleRate,
        subscriptions: Array.from(subs),
      } as Partial<NotifyChannel> & { subscriptions?: string[] }),
    onSuccess: () => onSaved(),
    onError: (e: unknown) => setError(e instanceof Error ? e.message : 'Failed to create channel'),
  })

  function validateStep2(): string | null {
    if (!name.trim()) return 'Name is required.'
    if (!target.trim()) return 'Target is required.'
    if (kind === 'slack_webhook' && !isValidSlackWebhook(target))
      return 'Slack webhook URL must look like https://hooks.slack.com/services/...'
    if (kind === 'email' && !isValidEmail(target)) return 'Enter a valid email address.'
    return null
  }

  function next() {
    setError(null)
    if (step === 2) {
      const err = validateStep2()
      if (err) { setError(err); return }
    }
    setStep((s) => (s < 4 ? ((s + 1) as 1 | 2 | 3 | 4) : s))
  }

  function back() {
    setError(null)
    setStep((s) => (s > 1 ? ((s - 1) as 1 | 2 | 3 | 4) : s))
  }

  function submit() {
    setError(null)
    const err = validateStep2()
    if (err) { setError(err); setStep(2); return }
    create.mutate()
  }

  return (
    <ModalShell title="Add notification channel" onClose={onClose}>
      <div className="px-6 py-4 space-y-4">
        <StepIndicator step={step} />

        {step === 1 && (
          <div className="space-y-2">
            <p className="text-sm text-text-secondary">Choose a channel kind.</p>
            <KindRadio
              value="slack_webhook" current={kind} setKind={setKind}
              title="Slack webhook"
              desc="Post events to a Slack channel via an incoming webhook URL."
            />
            <KindRadio
              value="email" current={kind} setKind={setKind}
              title="Email" desc="Send events to a single email address."
            />
          </div>
        )}

        {step === 2 && (
          <div className="space-y-3">
            <div>
              <label className="block text-xs text-text-secondary mb-1">Name</label>
              <input
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder="e.g. #governance-alerts"
                className={INPUT_CLS}
              />
            </div>
            <div>
              <label className="block text-xs text-text-secondary mb-1">
                {kind === 'slack_webhook' ? 'Slack webhook URL' : 'Email address'}
              </label>
              <input
                value={target}
                onChange={(e) => setTarget(e.target.value)}
                placeholder={kind === 'slack_webhook'
                  ? 'https://hooks.slack.com/services/T000/B000/XXX'
                  : 'alerts@example.com'}
                className={`${INPUT_CLS} font-mono`}
              />
              <p className="text-xs text-text-tertiary mt-1">
                {kind === 'slack_webhook'
                  ? 'Format: https://hooks.slack.com/services/...'
                  : 'A single recipient email address.'}
              </p>
            </div>
          </div>
        )}

        {step === 3 && (
          <div className="space-y-2">
            <p className="text-sm text-text-secondary">Pick the events to deliver.</p>
            <EventCheckList eventTypes={eventTypes} subs={subs} setSubs={setSubs} />
          </div>
        )}

        {step === 4 && (
          <div className="space-y-3">
            <p className="text-xs text-text-secondary">
              Suppress duplicate events within a window; sample to throttle high-volume sources.
            </p>
            <div>
              <label className="block text-xs text-text-secondary mb-1">Min interval (seconds)</label>
              <input
                type="number" min={0} value={minInterval}
                onChange={(e) => setMinInterval(Number(e.target.value))}
                className={INPUT_CLS}
              />
            </div>
            <div>
              <label className="block text-xs text-text-secondary mb-1">Sample rate (0.0 – 1.0)</label>
              <input
                type="number" min={0} max={1} step={0.1} value={sampleRate}
                onChange={(e) => setSampleRate(Number(e.target.value))}
                className={INPUT_CLS}
              />
            </div>
          </div>
        )}

        {error && <p className="text-xs text-danger">{error}</p>}
      </div>

      <div className="flex items-center justify-between px-6 py-3 border-t border-border-default">
        <button onClick={onClose} className={BTN_OUTLINE}>Cancel</button>
        <div className="flex items-center gap-2">
          {step > 1 && <button onClick={back} className={BTN_OUTLINE}>Back</button>}
          {step < 4 ? (
            <button onClick={next} className={BTN_PRIMARY}>Next</button>
          ) : (
            <button onClick={submit} disabled={create.isPending} className={BTN_PRIMARY}>
              {create.isPending ? 'Creating…' : 'Create channel'}
            </button>
          )}
        </div>
      </div>
    </ModalShell>
  )
}

function KindRadio({
  value, current, setKind, title, desc,
}: {
  value: ChannelKind; current: ChannelKind
  setKind: (k: ChannelKind) => void
  title: string; desc: string
}) {
  return (
    <label className="flex items-start gap-3 p-3 rounded-md border border-border-default cursor-pointer hover:bg-surface-0">
      <input
        type="radio" name="kind" className="mt-1"
        checked={current === value} onChange={() => setKind(value)}
      />
      <div>
        <div className="text-sm font-medium text-text-primary">{title}</div>
        <div className="text-xs text-text-secondary">{desc}</div>
      </div>
    </label>
  )
}

// ── Edit modal ────────────────────────────────────────────────────────────────

interface EditProps {
  orgId: string
  channel: NotifyChannel
  eventTypes: string[]
  onClose: () => void
  onSaved: () => void
}

function EditChannelModal({ orgId, channel, eventTypes, onClose, onSaved }: EditProps) {
  const [name, setName] = useState(channel.name)
  const [target, setTarget] = useState(channel.target_url)
  const [minInterval, setMinInterval] = useState(channel.min_interval_seconds)
  const [sampleRate, setSampleRate] = useState(channel.sample_rate)
  const [error, setError] = useState<string | null>(null)
  const [subs, setSubs] = useState<Set<string> | null>(null)

  // Track whether the user has touched the subscription checkboxes
  // locally. Once they have, we keep their edits intact; until then,
  // we always mirror the latest server data — including the post-mount
  // background refetch that React Query does on cached payloads. The
  // earlier "initialize once when subs is null" pattern silently locked
  // in stale cached subscriptions if React Query served from cache and
  // the in-flight refetch arrived after the initialization fired.
  const dirtyRef = useRef(false)
  const setSubsLocal = (next: Set<string>) => {
    dirtyRef.current = true
    setSubs(next)
  }

  const subsQ = useQuery({
    queryKey: ['notify', orgId, 'subscriptions', channel.id],
    queryFn: () => api.orgs.notify.subscriptions.list(orgId, channel.id),
    // Make sure reopening the modal triggers a background refetch even
    // if cached data is present — pairs with the dirtyRef gate so fresh
    // server state replaces stale-from-cache on initial render.
    staleTime: 0,
    refetchOnMount: 'always',
  })

  useEffect(() => {
    if (dirtyRef.current || !subsQ.data) return
    const next = new Set<string>()
    for (const s of subsQ.data) if (s.enabled) next.add(s.event_type)
    setSubs(next)
  }, [subsQ.data])

  const update = useMutation({
    mutationFn: async () => {
      await api.orgs.notify.channels.update(orgId, channel.id, {
        name, target_url: target,
        min_interval_seconds: minInterval, sample_rate: sampleRate,
      })
      if (subs) {
        const payload: NotifySubscription[] = eventTypes.map((et) => ({
          channel_id: channel.id, event_type: et,
          enabled: subs.has(et), created_at: '',
        }))
        await api.orgs.notify.subscriptions.update(orgId, channel.id, payload)
      }
    },
    onSuccess: () => onSaved(),
    onError: (e: unknown) => setError(e instanceof Error ? e.message : 'Failed to update channel'),
  })

  function validate(): string | null {
    if (!name.trim()) return 'Name is required.'
    if (!target.trim()) return 'Target is required.'
    if (channel.kind === 'slack_webhook' && !isValidSlackWebhook(target))
      return 'Slack webhook URL must look like https://hooks.slack.com/services/...'
    if (channel.kind === 'email' && !isValidEmail(target)) return 'Enter a valid email address.'
    return null
  }

  function submit() {
    const err = validate()
    if (err) { setError(err); return }
    setError(null)
    update.mutate()
  }

  return (
    <ModalShell title={`Edit channel — ${channel.name}`} onClose={onClose}>
      <div className="px-6 py-4 space-y-3">
        <div className="text-xs text-text-secondary">
          Kind: <span className="font-mono text-text-primary">{channel.kind}</span>
        </div>
        <div>
          <label className="block text-xs text-text-secondary mb-1">Name</label>
          <input value={name} onChange={(e) => setName(e.target.value)} className={INPUT_CLS} />
        </div>
        <div>
          <label className="block text-xs text-text-secondary mb-1">
            {channel.kind === 'slack_webhook' ? 'Slack webhook URL' : 'Email address'}
          </label>
          <input
            value={target} onChange={(e) => setTarget(e.target.value)}
            className={`${INPUT_CLS} font-mono`}
          />
        </div>
        <div className="grid grid-cols-2 gap-3">
          <div>
            <label className="block text-xs text-text-secondary mb-1">Min interval (s)</label>
            <input
              type="number" min={0} value={minInterval}
              onChange={(e) => setMinInterval(Number(e.target.value))}
              className={INPUT_CLS}
            />
          </div>
          <div>
            <label className="block text-xs text-text-secondary mb-1">Sample rate</label>
            <input
              type="number" min={0} max={1} step={0.1} value={sampleRate}
              onChange={(e) => setSampleRate(Number(e.target.value))}
              className={INPUT_CLS}
            />
          </div>
        </div>
        <div>
          <p className="text-xs text-text-secondary mb-1">Event subscriptions</p>
          <EventCheckList
            eventTypes={eventTypes}
            subs={subs ?? new Set()}
            setSubs={setSubsLocal}
          />
        </div>
        {error && <p className="text-xs text-danger">{error}</p>}
      </div>
      <div className="flex items-center justify-end gap-2 px-6 py-3 border-t border-border-default">
        <button onClick={onClose} className={BTN_OUTLINE}>Cancel</button>
        <button onClick={submit} disabled={update.isPending} className={BTN_PRIMARY}>
          {update.isPending ? 'Saving…' : 'Save'}
        </button>
      </div>
    </ModalShell>
  )
}

// ── Shared modal shell ────────────────────────────────────────────────────────

function ModalShell({
  title, onClose, children,
}: {
  title: string
  onClose: () => void
  children: React.ReactNode
}) {
  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center">
      <div className="absolute inset-0 bg-black/60" onClick={onClose} />
      <div className="relative bg-surface-1 border border-border-default rounded-lg w-full max-w-xl mx-4 max-h-[85vh] flex flex-col shadow-xl">
        <div className="flex items-center justify-between px-6 py-4 border-b border-border-default">
          <h2 className="text-base font-semibold text-text-primary">{title}</h2>
          <button
            onClick={onClose}
            className="text-text-tertiary hover:text-text-primary text-xl leading-none"
          >&times;</button>
        </div>
        <div className="flex-1 overflow-y-auto">{children}</div>
      </div>
    </div>
  )
}

function StepIndicator({ step }: { step: 1 | 2 | 3 | 4 }) {
  const labels = ['Kind', 'Details', 'Events', 'Advanced']
  return (
    <div className="flex items-center gap-2 text-xs">
      {labels.map((lbl, i) => {
        const n = (i + 1) as 1 | 2 | 3 | 4
        const active = n === step
        const done = n < step
        const dotCls = active
          ? 'bg-brand text-surface-0 border-brand'
          : done
          ? 'bg-surface-0 text-text-primary border-border-default'
          : 'bg-surface-0 text-text-tertiary border-border-default'
        return (
          <div key={lbl} className="flex items-center gap-2">
            <span className={`inline-flex items-center justify-center w-5 h-5 rounded-full border ${dotCls}`}>{n}</span>
            <span className={active ? 'text-text-primary font-medium' : 'text-text-tertiary'}>{lbl}</span>
            {i < labels.length - 1 && <span className="text-text-tertiary">›</span>}
          </div>
        )
      })}
    </div>
  )
}
