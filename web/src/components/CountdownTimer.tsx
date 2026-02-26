import { differenceInSeconds } from 'date-fns'

interface Props {
  expiresAt: string
  showLabel?: boolean
}

export default function CountdownTimer({ expiresAt, showLabel = false }: Props) {
  const secs = Math.max(0, differenceInSeconds(new Date(expiresAt), new Date()))
  if (secs <= 0) return <span className="text-xs text-red-500 font-medium">Expired</span>
  const mins = Math.floor(secs / 60)
  const s = secs % 60
  const urgent = secs < 60
  const time = `${mins}:${String(s).padStart(2, '0')}`

  return (
    <span className={`font-mono text-xs tabular-nums ${urgent ? 'text-red-600' : 'text-gray-400'}`}>
      {showLabel ? `${time} remaining` : time}
    </span>
  )
}
