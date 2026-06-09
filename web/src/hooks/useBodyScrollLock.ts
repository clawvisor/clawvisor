import { useEffect } from 'react'

// Refcounted body-scroll lock so multiple drawers can be open at once
// without one closing and prematurely restoring scroll for the other.
// Each call increments the counter on mount and decrements on unmount;
// the original overflow value is captured once on the first lock and
// restored once on the final release.
let lockCount = 0
let savedOverflow: string | null = null

export function useBodyScrollLock(enabled = true) {
  useEffect(() => {
    if (!enabled) return
    if (lockCount === 0) {
      savedOverflow = document.body.style.overflow
      document.body.style.overflow = 'hidden'
    }
    lockCount++
    return () => {
      lockCount = Math.max(0, lockCount - 1)
      if (lockCount === 0) {
        document.body.style.overflow = savedOverflow ?? ''
        savedOverflow = null
      }
    }
  }, [enabled])
}

// Companion: close-on-Escape, shared by every drawer.
export function useEscapeKey(onClose: () => void, enabled = true) {
  useEffect(() => {
    if (!enabled) return
    const handler = (e: KeyboardEvent) => { if (e.key === 'Escape') onClose() }
    window.addEventListener('keydown', handler)
    return () => window.removeEventListener('keydown', handler)
  }, [onClose, enabled])
}
