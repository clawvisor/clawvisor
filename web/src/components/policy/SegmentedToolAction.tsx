export default function SegmentedToolAction({
  value,
  disabled,
  onChange,
}: {
  value: 'unset' | 'allow' | 'review' | 'deny'
  disabled?: boolean
  onChange: (action: 'unset' | 'allow' | 'deny') => void
}) {
  const normalizedValue: 'unset' | 'allow' | 'deny' = value === 'allow' || value === 'deny' ? value : 'unset'
  const options: Array<{ value: 'unset' | 'allow' | 'deny'; label: string }> = [
    { value: 'unset', label: 'Unset' },
    { value: 'allow', label: 'Always allow' },
    { value: 'deny', label: 'Always deny' },
  ]
  return (
    <div className="policy-segmented" role="group" aria-label="Tool policy action">
      {options.map(option => {
        const active = normalizedValue === option.value
        const activeClass = active
          ? option.value === 'allow'
            ? 'policy-segmented-btn--active-allow'
            : option.value === 'deny'
              ? 'policy-segmented-btn--active-deny'
              : 'policy-segmented-btn--active-unset'
          : 'policy-segmented-btn--inactive'
        return (
          <button
            key={option.value}
            type="button"
            disabled={disabled || active}
            onClick={() => {
              if (active) return
              onChange(option.value)
            }}
            aria-pressed={active}
            className={`policy-segmented-btn ${activeClass}`}
          >
            {option.label}
          </button>
        )
      })}
    </div>
  )
}
