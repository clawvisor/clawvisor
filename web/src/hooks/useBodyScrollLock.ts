import { useEffect } from 'react'

// Refcounted body-scroll lock so multiple drawers can be open at once
// without one closing and prematurely restoring scroll for the other.
// Each call increments the counter on mount and decrements on unmount;
// the original overflow value is captured once on the first lock and
// restored once on the final release.
let lockCount = 0
let savedOverflow: string | null = null
let observer: MutationObserver | null = null

function reconcileScrollLock() {
  const activeElements = document.querySelectorAll('[data-body-scroll-lock="true"]')
  if (activeElements.length === 0) {
    lockCount = 0
    if (document.body.style.overflow === 'hidden') {
      document.body.style.overflow = savedOverflow ?? ''
      savedOverflow = null
    }
  } else {
    lockCount = activeElements.length
    if (document.body.style.overflow !== 'hidden') {
      savedOverflow = document.body.style.overflow
      document.body.style.overflow = 'hidden'
    }
  }
}

function initMutationObserver() {
  if (typeof window === 'undefined' || observer) return
  observer = new MutationObserver(() => {
    reconcileScrollLock()
  })
  observer.observe(document.body, { childList: true, subtree: true })
}

export function useBodyScrollLock(enabled = true) {
  useEffect(() => {
    initMutationObserver()
    return () => {
      setTimeout(reconcileScrollLock, 0)
    }
  }, [])

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
      setTimeout(reconcileScrollLock, 0)
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
