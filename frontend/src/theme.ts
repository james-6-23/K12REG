export type ThemeMode = 'dark' | 'light'

const KEY = 'k12reg-theme'

export function getStoredTheme(): ThemeMode {
  try {
    const v = localStorage.getItem(KEY)
    if (v === 'light' || v === 'dark') return v
  } catch {
    /* ignore */
  }
  if (typeof window !== 'undefined' && window.matchMedia?.('(prefers-color-scheme: light)').matches) {
    return 'light'
  }
  return 'dark'
}

export function applyTheme(mode: ThemeMode) {
  const root = document.documentElement
  root.classList.toggle('dark', mode === 'dark')
  root.classList.toggle('light', mode === 'light')
  root.style.colorScheme = mode
  try {
    localStorage.setItem(KEY, mode)
  } catch {
    /* ignore */
  }
}

export function toggleTheme(): ThemeMode {
  const next: ThemeMode = document.documentElement.classList.contains('light') ? 'dark' : 'light'
  applyTheme(next)
  return next
}

export function initTheme(): ThemeMode {
  const mode = getStoredTheme()
  applyTheme(mode)
  return mode
}
