import { useState } from 'react'
import { Link } from 'react-router'
import { Activity, Plus } from 'lucide-react'
import { usePrefs, useSession } from '../app'
import { useApi } from '../hooks'
import { fmtDate, fmtMs, fmtNumber, fmtPct, fmtRelative } from '../format'
import { Button, Card, Empty, ErrorBanner, IncidentBadge, PageHeader, Segmented, Spinner, Stat, StatusBadge } from '../components/ui'
import { LatencyChart, UptimeBars } from '../components/charts'
import type { CertInfo, DailyBucket, Incident, LatencyPoint, MonitorStatus, NodeCell, NodeInfo, NodeLatency } from '../types'

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
          <NodeLatencyCard />
          <CertificatesCard />
        </div>
      )}
    </>
  )
}

function latencyClass(ms: number | null | undefined) {
  if (ms == null) return 'text-muted'
  if (ms >= 1000) return 'text-down'
  if (ms >= 400) return 'text-warn'
  return ''
}

function Cell({ c }: { c?: NodeCell }) {
  const { t, loc } = usePrefs()
  if (!c || c.probes === 0) return <span className="text-muted">—</span>
  return (
    <span className="inline-flex flex-col items-end">
      <span className={`tabular-nums ${latencyClass(c.avg_latency_ms)}`}>{c.avg_latency_ms == null ? '—' : fmtMs(loc, c.avg_latency_ms)}</span>
      {c.failures > 0 && <span className="text-[11px] text-down">{t('overview.failedProbes', { n: fmtNumber(loc, c.failures) })}</span>}
    </span>
  )
}

function NodeLatencyCard() {
  const { t } = usePrefs()
  const [win, setWin] = useState<'1h' | '24h' | '7d' | '30d'>('24h')
  const { data, error, reload } = useApi<NodeLatency>(`/overview/node-latency?window=${win}`, 60000)
  return (
    <Card title={t('overview.nodeLatency')} className="xl:col-span-3" bodyClass="p-0"
      actions={<Segmented label={t('overview.window')} value={win} onChange={setWin} options={(['1h', '24h', '7d', '30d'] as const).map((w) => ({ value: w, label: t(`overview.window.${w}`) }))} />}>
      {!data ? (error ? <div className="p-4"><ErrorBanner error={error} onRetry={reload} /></div> : <Spinner />) : data.nodes.length === 0 ? <Empty>{t('overview.noNodeData')}</Empty> : (
        <div className="overflow-x-auto" data-testid="node-latency">
          <table className="w-full min-w-[32rem] text-sm">
            <thead>
              <tr className="border-b border-line text-xs text-muted">
                <th className="px-4 py-2 text-start font-medium">{t('overview.monitor')}</th>
                {data.nodes.map((n) => <th key={n.id} className="px-4 py-2 text-end font-medium">{n.name}</th>)}
              </tr>
            </thead>
            <tbody>
              <tr className="border-b border-line bg-surface-2 font-medium" data-testid="node-latency-avg">
                <td className="px-4 py-2">{t('overview.allMonitors')}</td>
                {data.nodes.map((n) => <td key={n.id} className="px-4 py-2 text-end"><Cell c={n} /></td>)}
              </tr>
              {data.monitors.filter((m) => Object.keys(m.nodes).length > 0).map((m) => (
                <tr key={m.id} className="border-b border-line last:border-0">
                  <td className="px-4 py-2"><Link to={`/monitors/${m.id}`} className="hover:underline">{m.name}</Link></td>
                  {data.nodes.map((n) => <td key={n.id} className="px-4 py-2 text-end"><Cell c={m.nodes[n.id]} /></td>)}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </Card>
  )
}

function CertificatesCard() {
  const { t, loc } = usePrefs()
  const { data, error, reload } = useApi<{ certificates: CertInfo[] }>('/overview/certificates', 300000)
  const tone = (c: CertInfo) => c.days_left == null ? 'text-muted' : c.days_left < Math.max(c.fail_days, 7) ? 'text-down' : c.days_left < Math.max(c.warn_days, 30) ? 'text-warn' : 'text-up'
  return (
    <Card title={t('overview.certificates')} className="xl:col-span-3" bodyClass="p-0">
      {!data ? (error ? <div className="p-4"><ErrorBanner error={error} onRetry={reload} /></div> : <Spinner />) : data.certificates.length === 0 ? <Empty>{t('overview.noCertificates')}</Empty> : (
        <ul className="divide-y divide-line" data-testid="certificates">
          {data.certificates.map((c) => (
            <li key={c.id}>
              <Link to={`/monitors/${c.id}`} className="flex items-center justify-between gap-3 px-4 py-2.5 text-sm hover:bg-surface-2">
                <span className="min-w-0"><span className="block truncate font-medium">{c.name}</span><span dir="ltr" className="ltr block truncate text-xs text-muted">{c.target}</span></span>
                <span className="shrink-0 text-end">
                  <span className={`block font-medium tabular-nums ${tone(c)}`}>{c.days_left == null ? t('overview.certUnknown') : t('overview.certDays', { n: fmtNumber(loc, Math.floor(c.days_left)) })}</span>
                  {c.expires_at && <span className="block text-xs text-muted">{fmtDate(loc, c.expires_at)}</span>}
                </span>
              </Link>
            </li>
          ))}
        </ul>
      )}
    </Card>
  )
}
