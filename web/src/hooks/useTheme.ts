import { useState, useEffect, useCallback } from 'react'

type Theme = 'light' | 'dark' | 'system'
type Resolved = 'light' | 'dark'

const STORAGE_KEY = 'clawvisor_theme'

function getSystemTheme(): Resolved {
  return window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light'
}

function resolve(theme: Theme): Resolved {
  return theme === 'system' ? getSystemTheme() : theme
}

function apply(resolved: Resolved) {
  const root = document.documentElement
  root.classList.toggle('dark', resolved === 'dark')
  root.setAttribute('data-mode', resolved)
}

export function useTheme() {
  const [theme, setThemeState] = useState<Theme>(() => {
    const stored = localStorage.getItem(STORAGE_KEY)
    if (stored === 'light' || stored === 'dark') return stored
    return 'system'
  })

  const resolvedTheme = resolve(theme)

  const setTheme = useCallback((t: Theme) => {
    setThemeState(t)
    if (t === 'system') {
      localStorage.removeItem(STORAGE_KEY)
    } else {
      localStorage.setItem(STORAGE_KEY, t)
    }
    apply(resolve(t))
  }, [])

  // Apply on mount
  useEffect(() => {
    apply(resolvedTheme)
  }, [resolvedTheme])

  // Listen for system preference changes when in system mode
  useEffect(() => {
    if (theme !== 'system') return
    const mq = window.matchMedia('(prefers-color-scheme: dark)')
    const handler = () => apply(getSystemTheme())
    mq.addEventListener('change', handler)
    return () => mq.removeEventListener('change', handler)
  }, [theme])

  return { theme, resolvedTheme, setTheme } as const
}
