export default function InlineChatBoundNotice() {
  return (
    <div className="mx-4 mb-3 px-3 py-2 rounded-md bg-warning/10 border border-warning/30 text-xs text-text-secondary">
      <span className="font-medium text-text-primary">Reply in the agent chat</span>
      {' '}— this item is bound to an inline conversation. Approve or deny from the chat thread instead.
    </div>
  )
}
