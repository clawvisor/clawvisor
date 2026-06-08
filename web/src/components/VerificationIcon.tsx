export default function VerificationIcon({ result: r, type }: { result: string; type: 'param' | 'reason' }) {
  const isOk = r === 'ok'
  const isDanger = type === 'param' ? r === 'violation' : r === 'incoherent'
  const isWarning = type === 'reason' && r === 'insufficient'

  if (isOk) return (
    <span className="inline-flex items-center text-sm font-mono px-1 py-0.5 rounded bg-success/10 text-success">
      <svg className="w-2.5 h-2.5" fill="none" stroke="currentColor" strokeWidth="2.5" viewBox="0 0 24 24"><path d="M5 13l4 4L19 7"/></svg>
    </span>
  )
  if (isDanger) return (
    <span className="inline-flex items-center text-sm font-mono px-1 py-0.5 rounded bg-danger/12 text-danger">
      <svg className="w-2.5 h-2.5" fill="none" stroke="currentColor" strokeWidth="2.5" viewBox="0 0 24 24"><path d="M6 18L18 6M6 6l12 12"/></svg>
    </span>
  )
  if (isWarning) return (
    <span className="inline-flex items-center text-sm font-mono px-1 py-0.5 rounded bg-warning/15 text-warning">
      <svg className="w-2.5 h-2.5" fill="none" stroke="currentColor" strokeWidth="2.5" viewBox="0 0 24 24"><path d="M12 9v2m0 4h.01"/></svg>
    </span>
  )
  return null
}
