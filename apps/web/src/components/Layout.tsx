import { useEffect, useState, type ReactNode } from 'react'
import { NavLink, Outlet, useLocation, useNavigate } from 'react-router'
import { Activity, AlertOctagon, Boxes, CalendarClock, FolderTree, LayoutDashboard, LogOut, Menu, Monitor, Moon, ScrollText, Settings, Sun, SunMoon, Users, X } from 'lucide-react'
import { usePrefs, useSession, type Theme } from '../app'
import { get, post } from '../api'
import type { Key } from '../i18n'

interface NavItem { to: string; label: Key; icon: ReactNode; perm?: string }

const nav: NavItem[] = [
  { to: '/', label: 'nav.overview', icon: <LayoutDashboard className="size-4" /> },
  { to: '/monitors', label: 'nav.monitors', icon: <Activity className="size-4" /> },
  { to: '/groups', label: 'nav.groups', icon: <FolderTree className="size-4" /> },
  { to: '/incidents', label: 'nav.incidents', icon: <AlertOctagon className="size-4" /> },
  { to: '/maintenance', label: 'nav.maintenance', icon: <CalendarClock className="size-4" /> },
  { to: '/cluster', label: 'nav.cluster', icon: <Boxes className="size-4" /> },
  { to: '/audit', label: 'nav.audit', icon: <ScrollText className="size-4" />, perm: 'audit.view' },
  { to: '/users', label: 'nav.users', icon: <Users className="size-4" />, perm: 'user.manage' },
  { to: '/settings', label: 'nav.settings', icon: <Settings className="size-4" /> },
]

export function Logo() {
  return (
    <span className="inline-flex size-7 items-center justify-center rounded-lg bg-accent text-accent-fg" aria-hidden>
      <Monitor className="size-4" />
    </span>
  )
}

export function PrefControls({ compact = false }: { compact?: boolean }) {
  const { lang, setLang, theme, setTheme, t } = usePrefs()
  const nextTheme: Record<Theme, Theme> = { system: 'light', light: 'dark', dark: 'system' }
  const icon = theme === 'light' ? <Sun className="size-4" /> : theme === 'dark' ? <Moon className="size-4" /> : <SunMoon className="size-4" />
  return (
    <div className="flex items-center gap-1">
      <button type="button" onClick={() => setLang(lang === 'en' ? 'fa' : 'en')} data-testid="lang-toggle"
        className="h-8 rounded-lg px-2 text-sm font-medium text-muted hover:bg-surface-2 hover:text-fg" aria-label={t('lang.label')}>
        {lang === 'en' ? 'فا' : 'EN'}
      </button>
      <button type="button" onClick={() => setTheme(nextTheme[theme])} data-testid="theme-toggle" data-theme-value={theme}
        className="inline-flex h-8 items-center gap-1.5 rounded-lg px-2 text-sm text-muted hover:bg-surface-2 hover:text-fg"
        aria-label={`${t('theme.label')}: ${t(`theme.${theme}` as Key)}`} title={`${t('theme.label')}: ${t(`theme.${theme}` as Key)}`}>
        {icon}{!compact && <span className="hidden sm:inline">{t(`theme.${theme}` as Key)}</span>}
      </button>
    </div>
  )
}

export function Layout() {
  const { t } = usePrefs()
  const { me, can, refresh } = useSession()
  const [open, setOpen] = useState(false)
  const loc = useLocation()
  const navigate = useNavigate()
  useEffect(() => setOpen(false), [loc.pathname])
  // Follow role changes (failover, quorum loss) so the UI switches between full and restricted views.
  const [role, setRole] = useState(me?.node.role)
  useEffect(() => {
    setRole(me?.node.role)
    const id = window.setInterval(async () => {
      try {
        const h = await get<{ role: typeof role }>('/health')
        setRole(h.role)
        if (h.role && me && h.role !== me.node.role) void refresh()
      } catch { /* shown by page-level errors */ }
    }, 5000)
    return () => window.clearInterval(id)
  }, [me, refresh])
  const logout = async () => {
    try { await post('/auth/logout') } finally { await refresh(); navigate('/login') }
  }
  const items = me?.node.can_manage ? nav.filter((n) => !n.perm || can(n.perm)) : []
  const sidebar = (
    <nav aria-label="Main" className="flex h-full flex-col gap-1 p-3">
      <div className="mb-3 flex items-center gap-2 px-2 py-1">
        <Logo />
        <span className="text-base font-semibold">{t('app.name')}</span>
      </div>
      {items.map((n) => (
        <NavLink key={n.to} to={n.to} end={n.to === '/'}
          className={({ isActive }) => `flex items-center gap-2.5 rounded-lg px-2.5 py-2 text-sm ${isActive ? 'bg-accent-soft font-medium text-accent' : 'text-muted hover:bg-surface-2 hover:text-fg'}`}>
          {n.icon}{t(n.label)}
        </NavLink>
      ))}
      <div className="mt-auto border-t border-line pt-3">
        <div className="px-2.5 pb-2 text-xs text-muted">
          <div className="truncate font-medium text-fg">{me?.user.display_name || me?.user.username}</div>
          <div>{t(`role.${me?.user.role}` as Key)} · {me?.node.node_name}</div>
        </div>
        <button type="button" onClick={logout} className="flex w-full items-center gap-2.5 rounded-lg px-2.5 py-2 text-sm text-muted hover:bg-surface-2 hover:text-fg">
          <LogOut className="size-4 rtl:-scale-x-100" />{t('user.logout')}
        </button>
      </div>
    </nav>
  )
  return (
    <div className="min-h-screen">
      <a href="#main" className="sr-only focus:not-sr-only focus:absolute focus:start-2 focus:top-2 focus:z-50 focus:rounded focus:bg-surface focus:p-2">{t('nav.skip')}</a>
      <aside className="fixed inset-y-0 start-0 z-30 hidden w-60 border-e border-line bg-surface lg:block">{sidebar}</aside>
      {open && (
        <div className="fixed inset-0 z-40 lg:hidden">
          <div className="absolute inset-0 bg-black/40" onClick={() => setOpen(false)} />
          <aside className="absolute inset-y-0 start-0 w-64 border-e border-line bg-surface">
            <button type="button" className="absolute end-2 top-3 rounded p-1 text-muted" onClick={() => setOpen(false)} aria-label={t('common.close')}><X className="size-4" /></button>
            {sidebar}
          </aside>
        </div>
      )}
      <div className="lg:ps-60">
        <header className="sticky top-0 z-20 flex h-14 items-center justify-between gap-2 border-b border-line bg-bg/85 px-4 backdrop-blur">
          <div className="flex items-center gap-2">
            <button type="button" className="rounded-lg p-1.5 text-muted hover:bg-surface-2 lg:hidden" onClick={() => setOpen(true)} aria-label={t('nav.menu')} data-testid="menu-button">
              <Menu className="size-5" />
            </button>
            <span className="inline-flex items-center gap-1.5 rounded-full border border-line bg-surface px-2 py-0.5 text-xs text-muted" data-testid="node-role">
              <span className={`size-1.5 rounded-full ${role === 'NO_QUORUM' ? 'bg-down' : 'bg-up'}`} aria-hidden />{role ? t(`roleName.${role}` as Key) : ''}
            </span>
          </div>
          <PrefControls />
        </header>
        {role === 'NO_QUORUM' && (
          <div role="alert" className="border-b border-down/30 bg-down/10 px-4 py-2 text-sm text-down" data-testid="no-quorum-banner">{t('banner.noQuorum')}</div>
        )}
        <main id="main" className="mx-auto max-w-7xl p-4 sm:p-6">
          <Outlet />
        </main>
      </div>
    </div>
  )
}
