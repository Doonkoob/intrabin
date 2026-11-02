import { useMemo, useState } from 'react'
import { Link, useNavigate } from 'react-router'
import { Plus, Search } from 'lucide-react'
import { usePrefs, useSession } from '../app'
import { useApi } from '../hooks'
import { fmtMs, fmtNumber, fmtPct } from '../format'
import { Button, Card, Empty, ErrorBanner, Input, PageHeader, Select, Spinner, StatusBadge, Table, td, th } from '../components/ui'
import type { Group, Monitor } from '../types'
import type { Key } from '../i18n'

const statuses = ['UP', 'DOWN', 'SUSPECTED_DOWN', 'DEGRADED', 'UNKNOWN', 'PAUSED', 'MAINTENANCE'] as const

export function MonitorsPage() {
  const { t, loc } = usePrefs()
  const { can } = useSession()
  const navigate = useNavigate()
  const [q, setQ] = useState('')
  const [status, setStatus] = useState('')
  const [group, setGroup] = useState('')
  const { data, error, loading, reload } = useApi<{ monitors: Monitor[] }>('/monitors', 15000)
  const groups = useApi<{ groups: Group[] }>('/monitor-groups')
  const list = useMemo(() => {
    const ql = q.toLowerCase()
    return (data?.monitors ?? []).filter((m) =>
      (!ql || `${m.name} ${m.target} ${m.tags.join(' ')}`.toLowerCase().includes(ql)) &&
      (!status || m.state?.status === status) && (!group || m.group_id === group))
  }, [data, q, status, group])
  return (
    <>
      <PageHeader title={t('monitors.title')} subtitle={data && `${fmtNumber(loc, data.monitors.length)}`}
        actions={can('monitor.manage') && <Link to="/monitors/new"><Button variant="primary"><Plus className="size-4" />{t('monitors.new')}</Button></Link>} />
      <div className="mb-3 flex flex-wrap gap-2">
        <div className="relative min-w-56 flex-1">
          <Search className="pointer-events-none absolute start-2.5 top-2.5 size-4 text-muted" aria-hidden />
          <Input className="ps-8" placeholder={t('common.search')} aria-label={t('common.search')} value={q} onChange={(e) => setQ(e.target.value)} />
        </div>
        <Select className="w-auto" aria-label={t('monitors.filterStatus')} value={status} onChange={(e) => setStatus(e.target.value)}>
          <option value="">{t('common.status')}: {t('common.all')}</option>
          {statuses.map((s) => <option key={s} value={s}>{t(`status.${s}` as Key)}</option>)}
        </Select>
        {(groups.data?.groups.length ?? 0) > 0 && (
          <Select className="w-auto" aria-label={t('monitors.filterGroup')} value={group} onChange={(e) => setGroup(e.target.value)}>
            <option value="">{t('monitors.group')}: {t('common.all')}</option>
            {groups.data!.groups.map((g) => <option key={g.id} value={g.id}>{g.name}</option>)}
          </Select>
        )}
      </div>
      {error && <div className="mb-3"><ErrorBanner error={error} onRetry={reload} /></div>}
      <Card bodyClass="p-0">
        {loading && !data ? <Spinner /> : list.length === 0 ? <Empty>{t('monitors.empty')}</Empty> : (
          <Table>
            <thead>
              <tr>
                <th className={th}>{t('common.name')}</th>
                <th className={th}>{t('common.status')}</th>
                <th className={`${th} hidden md:table-cell`}>{t('monitors.type')}</th>
                <th className={`${th} hidden sm:table-cell`}>{t('monitors.latency')}</th>
                <th className={th}>{t('monitors.uptime24')}</th>
                <th className={`${th} hidden lg:table-cell`}>{t('monitors.interval')}</th>
              </tr>
            </thead>
            <tbody>
              {list.map((m) => (
                <tr key={m.id} className="cursor-pointer hover:bg-surface-2" onClick={() => navigate(`/monitors/${m.id}`)}>
                  <td className={td}>
                    <Link to={`/monitors/${m.id}`} className="font-medium hover:underline" onClick={(e) => e.stopPropagation()}>{m.name}</Link>
                    <div className="ltr max-w-xs truncate text-start text-xs text-muted">{m.target}</div>
                    {(m.group_name || m.tags.length > 0) && (
                      <div className="mt-1 flex flex-wrap gap-1">
                        {m.group_name && <span className="rounded bg-accent-soft px-1.5 text-[11px] text-accent">{m.group_name}</span>}
                        {m.tags.map((tg) => <span key={tg} className="rounded bg-surface-2 px-1.5 text-[11px] text-muted">{tg}</span>)}
                      </div>
                    )}
                  </td>
                  <td className={td}><StatusBadge status={m.state?.status} /></td>
                  <td className={`${td} hidden uppercase text-muted md:table-cell`}>{m.type}</td>
                  <td className={`${td} hidden tabular sm:table-cell`}>{fmtMs(loc, m.state?.last_latency_ms)}</td>
                  <td className={`${td} tabular`}>{fmtPct(loc, m.uptime_24h)}</td>
                  <td className={`${td} hidden tabular text-muted lg:table-cell`}>{t('common.seconds', { n: fmtNumber(loc, m.interval_seconds) })}</td>
                </tr>
              ))}
            </tbody>
          </Table>
        )}
      </Card>
    </>
  )
}
