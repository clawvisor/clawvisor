import { useState, useEffect } from 'react'
import { differenceInSeconds } from 'date-fns'

interface Props {
  expiresAt: string
  showLabel?: boolean
}

export default function CountdownTimer({ expiresAt, showLabel = false }: Props) {
  const [secs, setSecs] = useState(() =>
    Math.max(0, differenceInSeconds(new Date(expiresAt), new Date()))
  )

  useEffect(() => {
    if (secs <= 0) return
    const id = setInterval(() => {
      const remaining = Math.max(0, differenceInSeconds(new Date(expiresAt), new Date()))
      setSecs(remaining)
      if (remaining <= 0) clearInterval(id)
    }, 1000)
    return () => clearInterval(id)
  }, [expiresAt]) // eslint-disable-line react-hooks/exhaustive-deps

  if (secs <= 0) return <span className="text-xs text-danger font-medium">Expired</span>
  const mins = Math.floor(secs / 60)
  const s = secs % 60
  const urgent = secs < 60
  const time = `${mins}:${String(s).padStart(2, '0')}`

  return (
    <span className={`font-mono text-xs tabular-nums ${urgent ? 'text-danger' : 'text-text-tertiary'}`}>
      {showLabel ? `${time} remaining` : time}
    </span>
  )
}
