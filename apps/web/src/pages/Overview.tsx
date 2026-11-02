import { Link } from 'react-router'
import { Activity, Plus } from 'lucide-react'
import { usePrefs, useSession } from '../app'
import { useApi } from '../hooks'
import { fmtMs, fmtNumber, fmtPct, fmtRelative } from '../format'
import { Button, Card, Empty, ErrorBanner, IncidentBadge, PageHeader, Spinner, Stat, StatusBadge } from '../components/ui'
import { LatencyChart, UptimeBars } from '../components/charts'
import type { DailyBucket, Incident, LatencyPoint, MonitorStatus, NodeInfo } from '../types'

interface Overview {
  monitors: { total: number; by_status: Record<string, number>; list: { id: string; name: string; status: MonitorStatus; uptime_24h: number | null; latency_ms?: number }[] }
  incidents: { open: number; open_list: Incident[]; recent: Incident[] }
  uptime_24h_avg: number | null
  latency_24h: LatencyPoint[]
  health_30d: DailyBucket[]
  node: NodeInfo
  cluster: { mode: string; nodes_total: number; nodes_online: number; leader: string; quorum: string }
}

export function OverviewPage() {
  const { t, loc } = usePrefs()
  const { can } = useSession()
  const { data, error, loading, reload } = useApi<Overview>('/overview', 15000)
  if (loading && !data) return <Spinner />
  if (error && !data) return <ErrorBanner error={error} onRetry={reload} />
  if (!data) return null
  const s = data.monitors.by_status
  const down = (s.DOWN ?? 0)
  return (
    <>
      <PageHeader title={t('overview.title')} actions={can('monitor.manage') && <Link to="/monitors/new"><Button variant="primary"><Plus className="size-4" />{t('monitors.new')}</Button></Link>} />
      {error && <div className="mb-4"><ErrorBanner error={error} onRetry={reload} /></div>}
      <div className="grid grid-cols-2 gap-3 md:grid-cols-3 xl:grid-cols-6">
        <Stat label={t('overview.totalMonitors')} value={fmtNumber(loc, data.monitors.total)} />
        <Stat label={t('overview.up')} value={fmtNumber(loc, s.UP ?? 0)} tone="up" />
        <Stat label={t('overview.down')} value={fmtNumber(loc, down)} tone={down ? 'down' : undefined} sub={s.SUSPECTED_DOWN ? `${t('status.SUSPECTED_DOWN')}: ${fmtNumber(loc, s.SUSPECTED_DOWN)}` : undefined} />
        <Stat label={t('overview.degraded')} value={fmtNumber(loc, s.DEGRADED ?? 0)} tone={s.DEGRADED ? 'warn' : undefined} />
        <Stat label={t('overview.activeIncidents')} value={fmtNumber(loc, data.incidents.open)} tone={data.incidents.open ? 'down' : undefined} />
        <Stat label={t('overview.avgUptime')} value={fmtPct(loc, data.uptime_24h_avg)} />
      </div>
      {data.monitors.total === 0 ? (
        <Card className="mt-4">
          <Empty icon={<Activity className="size-8" />} action={can('monitor.manage') && <Link to="/monitors/new"><Button variant="primary">{t('overview.createFirst')}</Button></Link>}>
            {t('overview.empty')}
          </Empty>
        </Card>
      ) : (
        <div className="mt-4 grid gap-4 xl:grid-cols-3">
          <Card title={t('overview.responseTime')} className="xl:col-span-2"><LatencyChart points={data.latency_24h} /></Card>
          <Card title={t('overview.node')}>
            <dl className="grid grid-cols-2 gap-y-2 text-sm">
              <dt className="text-muted">{t('cluster.mode')}</dt><dd>{t('cluster.standalone')}</dd>
              <dt className="text-muted">{t('overview.leader')}</dt><dd className="truncate">{data.cluster.leader}</dd>
              <dt className="text-muted">{t('overview.nodes')}</dt><dd className="tabular">{fmtNumber(loc, data.cluster.nodes_online)}/{fmtNumber(loc, data.cluster.nodes_total)}</dd>
              <dt className="text-muted">{t('overview.quorum')}</dt><dd>{t('overview.quorumNA')}</dd>
              <dt className="text-muted">{t('cluster.version')}</dt><dd className="ltr text-start">{data.node.version}</dd>
            </dl>
          </Card>
          <Card title={t('overview.health30')} className="xl:col-span-2"><UptimeBars days={data.health_30d} /></Card>
          <Card title={t('overview.recentIncidents')} bodyClass="p-0">
            {data.incidents.recent.length === 0 ? <p className="p-4 text-sm text-muted">{t('overview.noIncidents')}</p> : (
              <ul className="divide-y divide-line">
                {data.incidents.recent.map((i) => (
                  <li key={i.id}>
                    <Link to={`/incidents/${i.id}`} className="flex items-center justify-between gap-2 px-4 py-2.5 text-sm hover:bg-surface-2">
                      <span className="min-w-0 truncate">{i.monitor_name || i.title}</span>
                      <span className="flex shrink-0 items-center gap-2"><span className="text-xs text-muted">{fmtRelative(loc, i.started_at)}</span><IncidentBadge status={i.status} /></span>
                    </Link>
                  </li>
                ))}
              </ul>
            )}
          </Card>
          <Card title={t('overview.monitorStatus')} className="xl:col-span-3" bodyClass="p-0">
            <ul className="grid divide-y divide-line sm:grid-cols-2 sm:divide-y-0 lg:grid-cols-3">
              {data.monitors.list.map((m) => (
                <li key={m.id} className="sm:border-b sm:border-line">
                  <Link to={`/monitors/${m.id}`} className="flex items-center justify-between gap-2 px-4 py-2.5 text-sm hover:bg-surface-2">
                    <span className="min-w-0 truncate font-medium">{m.name}</span>
                    <span className="flex shrink-0 items-center gap-3 text-xs text-muted tabular">
                      <span>{fmtMs(loc, m.latency_ms)}</span><span>{fmtPct(loc, m.uptime_24h)}</span><StatusBadge status={m.status} />
                    </span>
                  </Link>
                </li>
              ))}
            </ul>
          </Card>
        </div>
      )}
    </>
  )
}
