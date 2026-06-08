export default function QuestionToggleGroup({
  label,
  value,
  options,
  onChange,
}: {
  label: string
  value: string
  options: Array<[string, string]>
  onChange: (value: string) => void
}) {
  return (
    <div>
      {label && <p className="text-xs font-medium text-text-primary">{label}</p>}
      <div
        role="group"
        aria-label={label || undefined}
        className={`${label ? 'mt-1 ' : ''}inline-flex max-w-full flex-wrap rounded-md border border-border-default bg-surface-0 p-1`}
      >
        {options.map(([optionValue, optionLabel]) => (
          <button
            key={optionValue}
            type="button"
            onClick={() => onChange(optionValue)}
            aria-pressed={value === optionValue}
            className={`rounded px-3 py-1.5 text-sm font-medium leading-snug transition ${
              value === optionValue
                ? 'bg-surface-1 text-text-primary shadow-sm'
                : 'text-text-tertiary hover:text-text-primary'
            }`}
          >
            {optionLabel}
          </button>
        ))}
      </div>
    </div>
  )
}
