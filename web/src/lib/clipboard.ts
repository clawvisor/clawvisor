// `navigator.clipboard.writeText` rejects when the page isn't in a secure
// context, when the user-agent denies permission, or when called from an
// iframe without the right policy. Bare callers leak unhandled rejections
// into the console; even `.then(...)` chains miss the failure path.
//
// `copyText` is the one safe call site: it swallows rejections so the
// caller doesn't have to thread `.catch` everywhere, and reports success
// so a UI can flip a "Copied!" affordance only when the write actually
// landed.
export function copyText(text: string): Promise<boolean> {
  if (typeof navigator === 'undefined' || !navigator.clipboard?.writeText) {
    return Promise.resolve(false)
  }
  return navigator.clipboard.writeText(text).then(
    () => true,
    () => false,
  )
}
