import { useEffect } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { getAccessToken } from '../api/client'

/**
 * Fetches a short-lived, single-use ticket for SSE authentication.
 * The ticket replaces sending the JWT directly as a query parameter.
 */
async function fetchTicket(): Promise<string | null> {
  const token = getAccessToken()
  if (!token) return null

  try {
    const res = await fetch('/api/events/ticket', {
      method: 'POST',
      headers: {
        'Authorization': `Bearer ${token}`,
        'Content-Type': 'application/json',
      },
    })
    if (!res.ok) return null
    const data = await res.json()
    return data.ticket ?? null
  } catch {
    return null
  }
}

/**
 * Connects to the SSE event stream and invalidates React Query caches
 * on server-pushed events. Uses a single-use ticket for authentication
 * instead of sending the JWT as a query parameter.
 */
export function useEventStream() {
  const qc = useQueryClient()

  useEffect(() => {
    let es: EventSource | null = null
    let cancelled = false

    async function connect() {
      const ticket = await fetchTicket()
      if (cancelled || !ticket) return

      es = new EventSource(`/api/events?ticket=${encodeURIComponent(ticket)}`)

      es.addEventListener('queue', () => {
        qc.invalidateQueries({ queryKey: ['overview'] })
        qc.invalidateQueries({ queryKey: ['queue'] })
      })

      es.addEventListener('tasks', () => {
        qc.invalidateQueries({ queryKey: ['tasks'] })
        qc.invalidateQueries({ queryKey: ['overview'] })
      })

      es.addEventListener('audit', (e) => {
        const { id } = JSON.parse(e.data)
        qc.invalidateQueries({ queryKey: ['audit'] })
        if (id) {
          qc.invalidateQueries({ queryKey: ['audit', { task_id: id }] })
        }
        qc.invalidateQueries({ queryKey: ['overview'] })
      })
    }

    connect()

    return () => {
      cancelled = true
      es?.close()
    }
  }, [qc])
}
