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
 *
 * Automatically reconnects with exponential backoff on connection loss.
 */
export function useEventStream() {
  const qc = useQueryClient()

  useEffect(() => {
    let es: EventSource | null = null
    let cancelled = false
    let retryDelay = 1000

    async function connect() {
      const ticket = await fetchTicket()
      if (cancelled || !ticket) return

      es = new EventSource(`/api/events?ticket=${encodeURIComponent(ticket)}`)

      es.onopen = () => {
        retryDelay = 1000 // reset backoff on successful connection
      }

      es.addEventListener('queue', () => {
        qc.invalidateQueries({ queryKey: ['overview'] })
        qc.invalidateQueries({ queryKey: ['queue'] })
        qc.invalidateQueries({ queryKey: ['connections'] })
        qc.invalidateQueries({ queryKey: ['welcome'] })
      })

      es.addEventListener('tasks', () => {
        qc.invalidateQueries({ queryKey: ['tasks'] })
        qc.invalidateQueries({ queryKey: ['overview'] })
      })

      es.addEventListener('devices', () => {
        qc.invalidateQueries({ queryKey: ['devices'] })
      })

      es.addEventListener('audit', (e) => {
        let id: string | undefined
        try {
          ({ id } = JSON.parse(e.data))
        } catch (err) {
          console.error('useEventStream: failed to parse audit event data', err)
          return
        }
        qc.invalidateQueries({ queryKey: ['audit'] })
        if (id) {
          qc.invalidateQueries({ queryKey: ['audit', { task_id: id }] })
        }
        qc.invalidateQueries({ queryKey: ['overview'] })
      })

      es.onerror = () => {
        es?.close()
        es = null
        if (cancelled) return
        setTimeout(() => {
          if (!cancelled) connect()
        }, retryDelay)
        retryDelay = Math.min(retryDelay * 2, 30_000) // cap at 30s
      }
    }

    connect()

    return () => {
      cancelled = true
      es?.close()
    }
  }, [qc])
}
