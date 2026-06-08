export default function Toggle({
  checked,
  disabled,
  loading,
  onChange,
}: {
  checked: boolean
  disabled?: boolean
  loading?: boolean
  onChange: (checked: boolean) => void
}) {
  return (
    <button
      role="switch"
      aria-checked={checked}
      disabled={disabled || loading}
      onClick={() => onChange(!checked)}
      className={`relative inline-flex h-5 w-9 shrink-0 rounded-full transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-offset-2 ${
        disabled ? 'opacity-40 cursor-not-allowed' : 'cursor-pointer'
      } ${loading ? 'opacity-60' : ''} ${checked ? 'bg-danger' : 'bg-border-strong'}`}
    >
      <span
        className={`pointer-events-none inline-block h-4 w-4 rounded-full bg-white shadow transform transition-transform mt-0.5 ${
          checked ? 'translate-x-[18px] ml-0' : 'translate-x-0.5'
        }`}
      />
    </button>
  )
}
