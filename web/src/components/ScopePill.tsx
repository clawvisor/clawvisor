import { useEffect, useLayoutEffect, useRef, useState } from 'react'

type Verify = 'strict' | 'lenient' | 'off'

export type ScopePillValue = { auto: boolean; verification: Verify }

export default function ScopePill({
  value,
  expanded,
  onExpand,
  onCollapse,
  onChange,
  disabled = false,
}: {
  value: ScopePillValue
  expanded: boolean
  onExpand: () => void
  onCollapse: () => void
  onChange: (next: ScopePillValue) => void
  disabled?: boolean
}) {
  const ref = useRef<HTMLDivElement>(null)
  const compactRef = useRef<HTMLSpanElement>(null)
  const expandedRef = useRef<HTMLSpanElement>(null)
  const [widths, setWidths] = useState<{ wc: number; we: number } | null>(null)

  const autoLabel = value.auto ? 'auto' : 'approve'
  const label = `${autoLabel} · ${value.verification}`
  const dangerous = value.auto && value.verification === 'off'

  useLayoutEffect(() => {
    if (!compactRef.current || !expandedRef.current) return
    const wc = Math.ceil(compactRef.current.getBoundingClientRect().width)
    const we = Math.ceil(expandedRef.current.getBoundingClientRect().width)
    setWidths({ wc, we })
  }, [label])

  useEffect(() => {
    if (!expanded) return
    const onDocClick = (e: MouseEvent) => {
      if (!ref.current) return
      if (!ref.current.contains(e.target as Node)) onCollapse()
    }
    document.addEventListener('mousedown', onDocClick)
    return () => document.removeEventListener('mousedown', onDocClick)
  }, [expanded, onCollapse])

  const targetWidth = widths
    ? (expanded ? widths.we : widths.wc)
    : undefined

  const handleCompactClick = () => {
    if (disabled) return
    if (!expanded) onExpand()
  }

  const pillClass = [
    'scope-pill',
    expanded ? 'scope-pill--expanded' : '',
    dangerous && !expanded ? 'scope-pill--danger' : '',
    disabled ? 'scope-pill--disabled' : '',
  ].filter(Boolean).join(' ')

  return (
    <div
      ref={ref}
      className={pillClass}
      style={targetWidth !== undefined ? { width: targetWidth } : undefined}
      onClick={handleCompactClick}
    >
      <span ref={compactRef} className="scope-pill__compact">
        <span className="scope-pill__dot" />
        <span>{label}</span>
      </span>
      <span ref={expandedRef} className="scope-pill__expanded-inner">
        <span className="scope-pill__seg">
          <button
            type="button"
            onClick={(e) => { e.stopPropagation(); onChange({ ...value, auto: true }) }}
            className={value.auto ? 'is-active' : ''}
          >auto</button>
          <button
            type="button"
            onClick={(e) => { e.stopPropagation(); onChange({ ...value, auto: false }) }}
            className={!value.auto ? 'is-active' : ''}
          >approve</button>
        </span>
        <span className="scope-pill__divider" />
        <span className="scope-pill__seg">
          {(['strict', 'lenient', 'off'] as const).map(mode => (
            <button
              type="button"
              key={mode}
              onClick={(e) => { e.stopPropagation(); onChange({ ...value, verification: mode }) }}
              className={value.verification === mode ? 'is-active' : ''}
            >{mode}</button>
          ))}
        </span>
        <button
          type="button"
          className="scope-pill__close"
          onClick={(e) => { e.stopPropagation(); onCollapse() }}
          aria-label="Close"
        >
          <svg width="10" height="10" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.5">
            <path d="M6 18L18 6M6 6l12 12" />
          </svg>
        </button>
      </span>
    </div>
  )
}
