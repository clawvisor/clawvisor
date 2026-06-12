export default function InlineChatBoundNotice() {
  return (
    <div className="rounded border border-warning/30 bg-warning/10 px-3 py-2 text-xs text-warning">
      Reply <span className="font-mono">approve</span> or <span className="font-mono">deny</span> in
      the agent chat — this approval is bound to the inline conversation.
    </div>
  )
}
