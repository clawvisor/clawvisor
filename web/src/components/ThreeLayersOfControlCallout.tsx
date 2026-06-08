export default function ThreeLayersOfControlCallout() {
  return (
    <div className="flex items-start gap-3 rounded-lg border border-brand/30 bg-brand-muted p-4 text-sm text-text-secondary leading-relaxed">
      <ShieldCheckIcon className="w-5 h-5 shrink-0 text-brand mt-0.5" />
      <div>
        <span className="font-semibold text-brand">Three layers of control</span> check every request,
        in order: <span className="font-medium text-text-primary">restrictions</span> (hard blocks you
        configure), <span className="font-medium text-text-primary">task scopes</span> (what the agent
        declared and you approved), and{' '}
        <span className="font-medium text-text-primary">per-request approval</span> (anything outside the
        scope goes to your queue).
      </div>
    </div>
  )
}

function ShieldCheckIcon({ className }: { className?: string }) {
  return (
    <svg xmlns="http://www.w3.org/2000/svg" fill="none" viewBox="0 0 24 24" strokeWidth="1.5" stroke="currentColor" className={className}>
      <path strokeLinecap="round" strokeLinejoin="round" d="M12 9v3.75m0-10.036A11.959 11.959 0 0 1 3.598 6 11.99 11.99 0 0 0 3 9.75c0 5.592 3.824 10.29 9 11.622 5.176-1.332 9-6.03 9-11.622 0-1.31-.21-2.57-.598-3.75h-.152c-3.196 0-6.1-1.25-8.25-3.286Zm0 13.036h.008v.008H12v-.008Z" />
    </svg>
  )
}
