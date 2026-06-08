import { useMemo } from 'react'
import { useQuery } from '@tanstack/react-query'
import { format, subDays, startOfDay } from 'date-fns'
import { api, type ActivityBucket } from '../api/client'

const DEFAULT_DAYS = 14

function aggregateByDay(buckets: ActivityBucket[], days: number) {
  const counts = new Map<string, number>()
  for (const b of buckets) {
    const key = format(new Date(b.bucket), 'yyyy-MM-dd')
    counts.set(key, (counts.get(key) ?? 0) + b.count)
  }

  const today = startOfDay(new Date())
  const rows: { date: Date; count: number; label: string }[] = []
  for (let i = days - 1; i >= 0; i--) {
    const date = subDays(today, i)
    const key = format(date, 'yyyy-MM-dd')
    rows.push({
      date,
      count: counts.get(key) ?? 0,
      label: format(date, 'MMM d'),
    })
  }
  return rows
}

function intensityClass(count: number, max: number): string {
  if (count === 0) return 'bg-surface-2 border-border-default'
  const ratio = count / Math.max(max, 1)
  if (ratio >= 0.75) return 'bg-primary border-primary/60'
  if (ratio >= 0.5) return 'bg-primary/70 border-primary/50'
  if (ratio >= 0.25) return 'bg-primary/45 border-primary/35'
  return 'bg-primary/25 border-primary/25'
}

export default function ActivityHeatmap({ days = DEFAULT_DAYS }: { days?: number }) {
  const { data, isLoading } = useQuery({
    queryKey: ['audit-buckets', days],
    queryFn: () => api.audit.activityBuckets({ days, bucket_minutes: 1440 }),
    staleTime: 60_000,
    refetchInterval: 60_000,
  })

  const rows = useMemo(
    () => aggregateByDay(data?.buckets ?? [], days),
    [data?.buckets, days],
  )
  const max = useMemo(() => Math.max(1, ...rows.map(r => r.count)), [rows])
  const total = useMemo(() => rows.reduce((sum, r) => sum + r.count, 0), [rows])

  return (
    <section className="dev-panel p-4 space-y-3">
      <div>
        <h2 className="page-section-title mb-0">Activity heatmap</h2>
        <p className="text-xs text-text-tertiary mt-0.5">
          last {days} days · {total} event{total === 1 ? '' : 's'}
        </p>
      </div>

      {isLoading ? (
        <div className="ds-page-loading">loading heatmap…</div>
      ) : (
        <div className="grid gap-1.5" style={{ gridTemplateColumns: `repeat(${days}, minmax(0, 1fr))` }}>
          {rows.map(row => (
            <div key={row.label} className="flex flex-col items-center gap-1 min-w-0">
              <div
                title={`${row.label}: ${row.count} event${row.count === 1 ? '' : 's'}`}
                className={`w-full aspect-square max-h-10 rounded-md border transition-colors ${intensityClass(row.count, max)}`}
              />
              <span className="ds-data text-2xs truncate w-full text-center">
                {format(row.date, 'd')}
              </span>
            </div>
          ))}
        </div>
      )}
    </section>
  )
}
