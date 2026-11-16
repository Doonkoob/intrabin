import { useEffect, useRef, useState, type ReactNode } from 'react'
import { NavLink, Outlet, useLocation, useNavigate } from 'react-router'
import { Activity, AlertOctagon, Boxes, CalendarClock, FolderTree, Globe, LayoutDashboard, LifeBuoy, LogOut, Menu, ChevronRight, Moon, ScrollText, Settings, Sun, SunMoon, Users, Workflow, X } from 'lucide-react'
import { usePrefs, useSession, type Theme } from '../app'
import { get, post } from '../api'
import type { Key } from '../i18n'
import { UpdateBanner } from './Update'

interface NavItem { to: string; label: Key; icon: ReactNode; perm?: string; group: 'observe' | 'operate' | 'workspace' }

const nav: NavItem[] = [
  { to: '/', group: 'observe', label: 'nav.overview', icon: <LayoutDashboard className="size-4" /> },
  { to: '/monitors', group: 'observe', label: 'nav.monitors', icon: <Activity className="size-4" /> },
  { to: '/groups', group: 'observe', label: 'nav.groups', icon: <FolderTree className="size-4" /> },
  { to: '/incidents', group: 'observe', label: 'nav.incidents', icon: <AlertOctagon className="size-4" /> },
  { to: '/maintenance', group: 'observe', label: 'nav.maintenance', icon: <CalendarClock className="size-4" /> },
  { to: '/automation', group: 'operate', label: 'nav.automation', icon: <Workflow className="size-4" /> },
  { to: '/dns', group: 'operate', label: 'nav.dns', icon: <Globe className="size-4" /> },
  { to: '/dr', group: 'operate', label: 'nav.dr', icon: <LifeBuoy className="size-4" /> },
  { to: '/cluster', group: 'operate', label: 'nav.cluster', icon: <Boxes className="size-4" /> },
  { to: '/audit', group: 'workspace', label: 'nav.audit', icon: <ScrollText className="size-4" />, perm: 'audit.view' },
  { to: '/users', group: 'workspace', label: 'nav.users', icon: <Users className="size-4" />, perm: 'user.manage' },
  { to: '/settings', group: 'workspace', label: 'nav.settings', icon: <Settings className="size-4" /> },
]

export function Logo() {
  return (
    <span className="inline-flex size-9 shrink-0 items-center justify-center rounded-xl bg-accent text-accent-fg shadow-sm" aria-hidden>
      <Activity className="size-5" />
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
        aria-label={`${t('theme.label')}: ${t(`theme.${theme}` as Key)}. ${t('theme.switchTo', { theme: t(`theme.${nextTheme[theme]}` as Key) })}`} title={t('theme.switchTo', { theme: t(`theme.${nextTheme[theme]}` as Key) })}>
        <span key={theme} className="theme-icon" aria-hidden>{icon}</span>{!compact && <span className="hidden sm:inline">{t(`theme.${theme}` as Key)}</span>}
      </button>
    </div>
  )
}

export function Layout() {
  const { t } = usePrefs()
  const { me, can, refresh } = useSession()
  const [open, setOpen] = useState(false)
  const mobileMenu = useRef<HTMLDialogElement>(null)
  useEffect(() => {
    const dialog = mobileMenu.current
    if (open && !dialog?.open) dialog?.showModal()
    if (!open && dialog?.open) dialog.close()
  }, [open])
  useEffect(() => {
    const desktop = window.matchMedia('(min-width: 1024px)')
    const closeOnDesktop = () => { if (desktop.matches) setOpen(false) }
    desktop.addEventListener('change', closeOnDesktop)
    return () => desktop.removeEventListener('change', closeOnDesktop)
  }, [])
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
    <nav aria-label={t('nav.menu')} className="flex h-full flex-col overflow-y-auto p-4">
      <div className="mb-5 flex items-center gap-3 px-2 py-3">
        <Logo />
        <span className="sidebar-brand text-xl font-semibold text-white">{t('app.name')}</span>
      </div>
      {(['observe', 'operate', 'workspace'] as const).map((group) => {
        const links = items.filter((n) => n.group === group)
        if (!links.length) return null
        return <div key={group} className="mb-5">
          <p className="sidebar-group mb-2 px-3">{t(`nav.section.${group}`)}</p>
          <div className="space-y-1">{links.map((n) => <NavLink key={n.to} to={n.to} end={n.to === '/'}
            className={({ isActive }) => `sidebar-link flex items-center gap-3 rounded-lg px-3 py-2.5 text-[13px] font-medium ${isActive ? 'active' : ''}`}>
            {n.icon}<span>{t(n.label)}</span>
          </NavLink>)}</div>
        </div>
      })}
      <div className="mt-auto pt-4">
        <div className="sidebar-account flex items-center gap-3 rounded-xl p-3">
          <span aria-hidden className="flex size-9 shrink-0 items-center justify-center rounded-full bg-[#2d4b59] text-sm font-semibold text-[#b7e9de]">{(me?.user.display_name || me?.user.username || 'I').slice(0, 1).toUpperCase()}</span>
          <div className="min-w-0 flex-1"><div className="truncate text-sm font-medium text-white">{me?.user.display_name || me?.user.username}</div><div className="mt-0.5 truncate text-[11px] text-[#a9bdca]">{t(`role.${me?.user.role}` as Key)} · {me?.node.node_name}</div></div>
        </div>
        <button type="button" onClick={logout} className="sidebar-link mt-2 flex w-full items-center gap-3 rounded-lg px-3 py-2.5 text-xs">
          <LogOut className="size-4 rtl:-scale-x-100" />{t('user.logout')}
        </button>
      </div>
    </nav>
  )
  return (
    <div className="min-h-screen">
      <a href="#main" className="sr-only focus:not-sr-only focus:absolute focus:start-2 focus:top-2 focus:z-50 focus:rounded focus:bg-surface focus:p-2">{t('nav.skip')}</a>
      <aside className="fixed inset-y-0 start-0 z-30 hidden app-sidebar w-60 border-e lg:block">{sidebar}</aside>
      <dialog ref={mobileMenu} aria-label={t('nav.menu')} onClose={() => setOpen(false)} onCancel={() => setOpen(false)}
        onClick={(e) => { if (e.target === e.currentTarget) setOpen(false) }}
        onKeyDown={(e) => {
          if (e.key !== 'Tab' || e.ctrlKey || e.altKey || e.metaKey) return
          const controls = e.currentTarget.querySelectorAll<HTMLElement>('a[href], button:not([disabled])')
          const first = controls[0], last = controls[controls.length - 1]
          if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last?.focus() }
          if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first?.focus() }
        }}
        className="fixed inset-0 m-0 h-dvh max-h-none w-full max-w-none border-0 bg-transparent p-0">
          <aside className="absolute inset-y-0 start-0 app-sidebar w-64 border-e">
            <button type="button" className="absolute end-2 top-3 rounded p-1 text-[#a9bdca]" onClick={() => setOpen(false)} aria-label={t('common.close')}><X className="size-4" /></button>
            {sidebar}
          </aside>
      </dialog>
      <div className="lg:ps-60">
        <header className="sticky top-0 z-20 flex h-16 items-center justify-between gap-3 border-b border-line bg-surface/90 px-4 backdrop-blur-md sm:px-7">
          <div className="flex items-center gap-2">
            <button type="button" className="rounded-lg p-1.5 text-muted hover:bg-surface-2 lg:hidden" onClick={() => setOpen(true)} aria-label={t('nav.menu')} data-testid="menu-button">
              <Menu className="size-5" />
            </button>
            <div className="hidden items-center gap-2 text-xs sm:flex"><span className="text-muted">{t('nav.workspace')}</span><ChevronRight className="size-3 text-muted rtl:rotate-180" aria-hidden /><span className="font-medium">{t((nav.find((n) => n.to === '/' ? loc.pathname === '/' : loc.pathname.startsWith(n.to)) ?? nav[0]).label)}</span><span className="mx-2 h-4 border-e border-line" aria-hidden /></div>
            <span className="inline-flex items-center gap-1.5 rounded-full border border-line bg-surface px-2 py-0.5 text-xs text-muted" data-testid="node-role">
              <span className={`size-1.5 rounded-full ${role === 'NO_QUORUM' ? 'bg-down' : 'bg-up'}`} aria-hidden />{role ? t(`roleName.${role}` as Key) : ''}
            </span>
          </div>
          <PrefControls />
        </header>
        {role === 'NO_QUORUM' && (
          <div role="alert" className="border-b border-down/30 bg-down/10 px-4 py-2 text-sm text-down" data-testid="no-quorum-banner">{t('banner.noQuorum')}</div>
        )}
        <UpdateBanner />
        <main id="main" className="mx-auto max-w-[1440px] p-4 sm:p-7 lg:p-8">
          <Outlet />
        </main>
      </div>
    </div>
  )
}
