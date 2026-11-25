// Per-viewer view choices kept across reloads (#516), like the theme (agd-theme) and the scope (agd-scope).
// A blocked or private-mode store, or a value this version does not know, falls back to the default.
// start, when set, wins over the stored value for this visit (a link that needs one view); a click still stores.
import { useCallback, useState } from 'react'

export function useStoredChoice<T extends string>(key: string, options: readonly T[], fallback: T, start?: T): [T, (v: T) => void] {
  const [value, setValue] = useState<T>(() => {
    if (start) return start
    try {
      const v = localStorage.getItem(key)
      return options.includes(v as T) ? (v as T) : fallback
    } catch { return fallback }
  })
  const set = useCallback((v: T) => {
    setValue(v)
    try { localStorage.setItem(key, v) } catch { /* storage blocked */ }
  }, [key])
  return [value, set]
}
