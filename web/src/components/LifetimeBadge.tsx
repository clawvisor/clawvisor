export default function LifetimeBadge({ lifetime }: { lifetime?: string }) {
  if (!lifetime || lifetime === 'session') return null
  return (
    <span
      className="inline-block px-2 py-0.5 rounded-full text-xs font-semibold bg-purple-100 text-purple-700"
      title="This task does not expire and remains active until revoked"
    >
      Ongoing
    </span>
  )
}
