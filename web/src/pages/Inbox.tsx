import AttentionInbox from '../components/attention/AttentionInbox'
import { useAttentionItems } from '../hooks/useAttentionItems'
import { useAttentionDeepLinks } from '../hooks/useAttentionDeepLinks'

export default function Inbox() {
  const { items, isLoading, count } = useAttentionItems()
  const { result, clear } = useAttentionDeepLinks()

  return (
    <div className="p-4 sm:p-8 space-y-6">
      <div className="flex items-baseline justify-between gap-3">
        <h1 className="text-2xl font-bold text-text-primary">Inbox</h1>
        {count > 0 && (
          <span className="text-sm text-text-tertiary">
            {count} item{count === 1 ? '' : 's'} need{count === 1 ? 's' : ''} your attention
          </span>
        )}
      </div>

      {result && (
        <div className="rounded-md border border-brand/30 bg-brand/10 px-5 py-3 flex items-center justify-between">
          <span className="text-brand text-sm">{result}</span>
          <button onClick={clear} className="text-brand text-xs hover:underline">Dismiss</button>
        </div>
      )}

      <AttentionInbox items={items} isLoading={isLoading} />
    </div>
  )
}
