import { createContext, useContext, useEffect, useState, type ReactNode } from 'react'

import { getLocale, setLocale, type Locale } from '@/paraglide/runtime'

type LocaleContextValue = {
  locale: Locale
  setLocale: (locale: Locale) => void
}

const LocaleContext = createContext<LocaleContextValue | null>(null)

// Paraglide's default setLocale() reloads the document. The console owns its
// whole reactive shell, so it takes the documented reload:false escape hatch
// and re-renders in place, keeping terminal and form state (api.md §8).
//
// TanStack Router memoizes each Match, so a provider update alone does not
// reach a route: every route component calls useLocale() to subscribe, which
// re-renders it and its children.
export function LocaleProvider({ children }: { children: ReactNode }) {
  const [locale, setLocaleState] = useState<Locale>(getLocale)

  useEffect(() => {
    document.documentElement.lang = locale
  }, [locale])

  const change = (next: Locale) => {
    setLocale(next, { reload: false })
    setLocaleState(next)
  }

  return <LocaleContext.Provider value={{ locale, setLocale: change }}>{children}</LocaleContext.Provider>
}

export function useLocale(): LocaleContextValue {
  const value = useContext(LocaleContext)
  if (!value) throw new Error('useLocale must be used inside LocaleProvider')
  return value
}
