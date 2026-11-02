import { createContext, useCallback, useContext, useEffect, useMemo, useState, type ReactNode } from 'react'
import { get, setCsrf, setUnauthorizedHandler } from './api'
import { translate, locale, type Key, type Lang } from './i18n'
import type { Me } from './types'

export type Theme = 'light' | 'dark' | 'system'

interface Prefs {
  lang: Lang
  setLang: (l: Lang) => void
  theme: Theme
  setTheme: (t: Theme) => void
  t: (k: Key, vars?: Record<string, string | number>) => string
  loc: string
}

const PrefsCtx = createContext<Prefs | null>(null)

function readStored<T extends string>(k: string, allowed: readonly T[], def: T): T {
  try {
    const v = localStorage.getItem(k) as T | null
    return v && allowed.includes(v) ? v : def
  } catch {
    return def
  }
}

function store(k: string, v: string) {
  try {
    localStorage.setItem(k, v)
  } catch {
    /* storage unavailable: preference applies to this tab only */
  }
}

export function PrefsProvider({ children }: { children: ReactNode }) {
  const [lang, setLangState] = useState<Lang>(() => readStored('intrabin.lang', ['en', 'fa'] as const, navigator.language?.startsWith('fa') ? 'fa' : 'en'))
  const [theme, setThemeState] = useState<Theme>(() => readStored('intrabin.theme', ['light', 'dark', 'system'] as const, 'system'))

  useEffect(() => {
    document.documentElement.lang = lang
    document.documentElement.dir = lang === 'fa' ? 'rtl' : 'ltr'
  }, [lang])

  useEffect(() => {
    const mq = window.matchMedia('(prefers-color-scheme: dark)')
    const apply = () => {
      const dark = theme === 'dark' || (theme === 'system' && mq.matches)
      document.documentElement.classList.toggle('dark', dark)
      document.documentElement.dataset.theme = dark ? 'dark' : 'light'
    }
    apply()
    mq.addEventListener('change', apply)
    return () => mq.removeEventListener('change', apply)
  }, [theme])

  const value = useMemo<Prefs>(() => ({
    lang,
    setLang: (l) => { store('intrabin.lang', l); setLangState(l) },
    theme,
    setTheme: (t) => { store('intrabin.theme', t); setThemeState(t) },
    t: (k, vars) => translate(lang, k, vars),
    loc: locale(lang),
  }), [lang, theme])
  return <PrefsCtx.Provider value={value}>{children}</PrefsCtx.Provider>
}

export function usePrefs() {
  const v = useContext(PrefsCtx)
  if (!v) throw new Error('PrefsProvider missing')
  return v
}

export const useT = () => usePrefs().t

interface Session {
  me: Me | null
  loading: boolean
  refresh: () => Promise<void>
  can: (perm: string) => boolean
}

const SessionCtx = createContext<Session | null>(null)

export function SessionProvider({ children }: { children: ReactNode }) {
  const [me, setMe] = useState<Me | null>(null)
  const [loading, setLoading] = useState(true)
  const refresh = useCallback(async () => {
    try {
      const m = await get<Me>('/auth/me')
      setCsrf(m.csrf_token)
      setMe(m)
    } catch {
      setCsrf('')
      setMe(null)
    } finally {
      setLoading(false)
    }
  }, [])
  useEffect(() => {
    setUnauthorizedHandler(() => { setCsrf(''); setMe(null) })
    void refresh()
  }, [refresh])
  const can = useCallback((p: string) => !!me?.permissions.includes(p), [me])
  return <SessionCtx.Provider value={{ me, loading, refresh, can }}>{children}</SessionCtx.Provider>
}

export function useSession() {
  const v = useContext(SessionCtx)
  if (!v) throw new Error('SessionProvider missing')
  return v
}
