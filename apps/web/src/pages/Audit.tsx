import { useEffect, useState } from 'react'
import { Search } from 'lucide-react'
import { get } from '../api'
import { usePrefs } from '../app'
import { fmtDateTime } from '../format'
import { Button, Card, Empty, ErrorBanner, Input, PageHeader, Select, Spinner, Table, td, th } from '../components/ui'
import type { AuditEntry } from '../types'

const prefixes = ['auth.', 'user.', 'monitor.', 'monitor_group.', 'incident.', 'maintenance.', 'settings.', 'setup.']

export function AuditPage() {
  const { t, loc } = usePrefs()
  const [q, setQ] = useState('')
  const [action, setAction] = useState('')
  const [entries, setEntries] = useState<AuditEntry[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<Error | null>(null)
  const [more, setMore] = useState(false)
  const load = async (before?: string) => {
    setLoading(true)
    try {
      const p = new URLSearchParams({ limit: '100' })
      if (q) p.set('q', q)
      if (action) p.set('action', action)
      if (before) p.set('before', before)
      const r = await get<{ entries: AuditEntry[] }>(`/audit-logs?${p}`)
      setEntries((prev) => (before ? [...prev, ...r.entries] : r.entries))
      setMore(r.entries.length === 100)
      setError(null)
    } catch (e) { setError(e as Error) } finally { setLoading(false) }
  }
  useEffect(() => {
    const id = setTimeout(() => void load(), 250)
    return () => clearTimeout(id)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [q, action])
  return (
    <>
      <PageHeader title={t('audit.title')} />
      <div className="mb-3 flex flex-wrap gap-2">
        <div className="relative min-w-56 flex-1">
          <Search className="pointer-events-none absolute start-2.5 top-2.5 size-4 text-muted" aria-hidden />
          <Input className="ps-8" placeholder={t('common.search')} aria-label={t('common.search')} value={q} onChange={(e) => setQ(e.target.value)} />
        </div>
        <Select className="w-auto" aria-label={t('audit.action')} value={action} onChange={(e) => setAction(e.target.value)}>
          <option value="">{t('audit.action')}: {t('common.all')}</option>
          {prefixes.map((p) => <option key={p} value={p}>{p.replace('.', '')}</option>)}
        </Select>
      </div>
      {error && <ErrorBanner error={error} onRetry={() => load()} />}
      <Card bodyClass="p-0">
        {loading && !entries.length ? <Spinner /> : !entries.length ? <Empty>{t('audit.empty')}</Empty> : (
          <Table>
            <thead><tr><th className={th}>{t('common.time')}</th><th className={th}>{t('audit.actor')}</th><th className={th}>{t('audit.action')}</th><th className={`${th} hidden md:table-cell`}>{t('audit.target')}</th><th className={th}>{t('audit.outcome')}</th><th className={`${th} hidden lg:table-cell`}>{t('audit.ip')}</th></tr></thead>
            <tbody>
              {entries.map((e) => (
                <tr key={e.id}>
                  <td className={`${td} whitespace-nowrap text-muted`}>{fmtDateTime(loc, e.at)}</td>
                  <td className={td}>{e.actor_name || '—'}</td>
                  <td className={`${td} ltr text-start font-mono text-xs`}>{e.action}</td>
                  <td className={`${td} ltr hidden max-w-[16rem] truncate text-start text-xs text-muted md:table-cell`} title={JSON.stringify(e.details ?? {})}>{e.target_type}{e.target_id ? `:${e.target_id.slice(0, 8)}` : ''} {e.details && Object.keys(e.details).length ? JSON.stringify(e.details) : ''}</td>
                  <td className={td}><span className={e.outcome === 'success' ? 'text-up' : 'text-down'}>{e.outcome}</span></td>
                  <td className={`${td} ltr hidden text-start text-xs text-muted lg:table-cell`}>{e.ip}</td>
                </tr>
              ))}
            </tbody>
          </Table>
        )}
      </Card>
      {more && <div className="mt-3 flex justify-center"><Button loading={loading} onClick={() => load(entries[entries.length - 1].at)}>{t('audit.loadMore')}</Button></div>}
    </>
  )
}
