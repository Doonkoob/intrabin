import { useState } from 'react'
import { Link, useNavigate, useParams } from 'react-router'
import { ArrowLeft, Pause, Pencil, Play, ShieldCheck, Trash2 } from 'lucide-react'
import { del, post } from '../api'
import { usePrefs, useSession } from '../app'
import { useApi } from '../hooks'
import { fmtDate, fmtDateTime, fmtDuration, fmtMs, fmtNumber, fmtPct, fmtRelative } from '../format'
import { Button, Card, ConfirmDialog, Empty, ErrorBanner, IncidentBadge, PageHeader, Segmented, Spinner, StatusBadge, Table, td, th } from '../components/ui'
import { LatencyChart, Legend, RoundStrip, UptimeBars } from '../components/charts'
import type { CheckResult, DailyBucket, Incident, LatencyPoint, Monitor, RoundResult, UptimeStats } from '../types'
import type { Key } from '../i18n'

interface Stats {
  latency: LatencyPoint[]
  breakdown: { status_codes: Record<string, number>; error_categories: Record<string, number>; nodes: { node_id: string; total: number; failures: number; avg_latency_ms?: number; last_probe_at: string; last_success: boolean }[] }
  rounds: RoundResult[]
  cert_expires_at: string | null
  incidents: Incident[]
  node_names: Record<string, string>
}

type Range = '1h' | '24h' | '7d' | '30d'

function Bars({ data, label }: { data: Record<string, number>; label: (k: string) => string }) {
  const { loc, t } = usePrefs()
  const entries = Object.entries(data).sort((a, b) => b[1] - a[1])
  const total = entries.reduce((s, [, v]) => s + v, 0)
  if (!total) return <p className="text-sm text-muted">{t('common.noData')}</p>
  return (
    <ul className="flex flex-col gap-2">
      {entries.map(([k, v]) => (
        <li key={k} className="text-sm">
          <div className="flex justify-between gap-2"><span>{label(k)}</span><span className="tabular text-muted">{fmtNumber(loc, v)} · {fmtPct(loc, (v / total) * 100)}</span></div>
          <div className="mt-1 h-1.5 rounded bg-surface-2"><div className="h-1.5 rounded bg-accent" style={{ width: `${(v / total) * 100}%` }} /></div>
        </li>
      ))}
    </ul>
  )
}

export function MonitorDetailPage() {
  const { id } = useParams()
  const { t, loc } = usePrefs()
  const { can } = useSession()
  const navigate = useNavigate()
  const [range, setRange] = useState<Range>('24h')
  const [confirmDel, setConfirmDel] = useState(false)
  const [busy, setBusy] = useState(false)
  const [actionErr, setActionErr] = useState<Error | null>(null)
  const mon = useApi<{ monitor: Monitor }>(`/monitors/${id}`, 15000)
  const uptime = useApi<{ periods: Record<string, UptimeStats>; daily: DailyBucket[] }>(`/monitors/${id}/uptime`, 60000)
  const stats = useApi<Stats>(`/monitors/${id}/stats?range=${range}`, 15000)
  const results = useApi<{ results: CheckResult[] }>(`/monitors/${id}/results?limit=25`, 15000)

  if (mon.loading && !mon.data) return <Spinner />
  if (mon.error && !mon.data) return <ErrorBanner error={mon.error} onRetry={mon.reload} />
  const m = mon.data!.monitor
  const st = m.state
  const nodeName = (nid: string) => stats.data?.node_names[nid] ?? nid.slice(0, 8)

  const act = async (fn: () => Promise<unknown>) => {
    setBusy(true)
    setActionErr(null)
    try { await fn(); await mon.reload() } catch (e) { setActionErr(e as Error) } finally { setBusy(false) }
  }
  const certDays = stats.data?.cert_expires_at ? Math.floor((new Date(stats.data.cert_expires_at).getTime() - Date.now()) / 86400000) : null

  return (
    <>
      <Link to="/monitors" className="mb-3 inline-flex items-center gap-1 text-sm text-muted hover:text-fg"><ArrowLeft className="size-4 rtl:-scale-x-100" />{t('nav.monitors')}</Link>
      <PageHeader
        title={<span className="flex flex-wrap items-center gap-2">{m.name}<StatusBadge status={st?.status} /></span>}
        subtitle={<span className="flex flex-wrap items-center gap-x-3 gap-y-1"><span className="ltr font-mono text-xs">{m.target}</span>
          <span>{m.type.toUpperCase()} · {t('common.seconds', { n: fmtNumber(loc, m.interval_seconds) })}</span>
          {st && <span>{t('mdetail.since', { t: fmtRelative(loc, st.status_since) })}</span>}</span>}
        actions={can('monitor.manage') && <>
          {m.enabled
            ? <Button onClick={() => act(() => post(`/monitors/${id}/pause`))} loading={busy}><Pause className="size-4" />{t('mdetail.pause')}</Button>
            : <Button onClick={() => act(() => post(`/monitors/${id}/resume`))} loading={busy}><Play className="size-4" />{t('mdetail.resume')}</Button>}
          <Link to={`/monitors/${id}/edit`}><Button><Pencil className="size-4" />{t('common.edit')}</Button></Link>
          <Button variant="danger" onClick={() => setConfirmDel(true)}><Trash2 className="size-4" />{t('common.delete')}</Button>
        </>} />
      {actionErr && <div className="mb-4"><ErrorBanner error={actionErr} /></div>}
      {m.description && <p className="-mt-2 mb-4 text-sm text-muted">{m.description}</p>}

      <div className="grid grid-cols-2 gap-3 lg:grid-cols-4" data-testid="uptime-periods">
        {(['24h', '7d', '30d', '90d'] as const).map((p) => {
          const u = uptime.data?.periods[p]
          return (
            <div key={p} className="rounded-xl border border-line bg-surface p-4">
              <div className="text-xs font-medium text-muted">{t('mdetail.uptime')} · {p === '24h' ? t('range.24h') : p === '7d' ? t('range.7d') : p === '30d' ? t('range.30d') : t('mdetail.days90')}</div>
              <div className="mt-1 text-2xl font-semibold tabular">{fmtPct(loc, u?.availability)}</div>
              <div className="mt-1 text-xs text-muted" title={t('mdetail.coverageHelp')}>{t('mdetail.coverage')}: <span className="tabular">{fmtPct(loc, u?.coverage)}</span></div>
            </div>
          )
        })}
      </div>

      <div className="mt-4 grid gap-4 xl:grid-cols-3">
        <Card title={t('mdetail.days90')} className="xl:col-span-3">
          {uptime.data ? <UptimeBars days={uptime.data.daily} /> : <Spinner />}
          <p className="mt-3 text-xs text-muted">{t('mdetail.coverageHelp')}</p>
        </Card>
        <Card title={t('mdetail.responseTime')} className="xl:col-span-2"
          actions={<Segmented label={t('mdetail.responseTime')} value={range} onChange={setRange} options={(['1h', '24h', '7d', '30d'] as Range[]).map((r) => ({ value: r, label: t(`range.${r}` as Key) }))} />}>
          {stats.data ? <LatencyChart points={stats.data.latency} showMax /> : <Spinner />}
        </Card>
        <Card title={t('mdetail.rounds')}>
          {stats.data ? <><RoundStrip rounds={stats.data.rounds} /><div className="mt-3"><Legend /></div></> : <Spinner />}
          {st && (
            <dl className="mt-4 grid grid-cols-2 gap-y-1 text-sm">
              <dt className="text-muted">{t('mdetail.failures')}</dt><dd className="tabular">{fmtNumber(loc, st.consecutive_failed)} / {fmtNumber(loc, m.policy.failure_rounds)}</dd>
              <dt className="text-muted">{t('mform.recoveryRounds')}</dt><dd className="tabular">{fmtNumber(loc, st.consecutive_ok)} / {fmtNumber(loc, m.policy.recovery_rounds)}</dd>
            </dl>
          )}
        </Card>
        <Card title={t('mdetail.statusCodes')}>{stats.data && <Bars data={stats.data.breakdown.status_codes} label={(k) => (k === 'none' ? t('common.none') : `HTTP ${k}`)} />}</Card>
        <Card title={t('mdetail.errors')}>{stats.data && <Bars data={stats.data.breakdown.error_categories} label={(k) => t(`err.${k}` as Key)} />}</Card>
        <Card title={t('mdetail.cert')}>
          {stats.data?.cert_expires_at ? (
            <div className="flex items-start gap-2 text-sm">
              <ShieldCheck className={`size-5 shrink-0 ${certDays !== null && certDays < 14 ? 'text-warn' : 'text-up'}`} />
              <span>{t('mdetail.certExpires', { date: fmtDate(loc, stats.data.cert_expires_at), days: fmtNumber(loc, certDays ?? 0) })}</span>
            </div>
          ) : <p className="text-sm text-muted">{t('common.noData')}</p>}
        </Card>
        <Card title={t('mdetail.byNode')} className="xl:col-span-3" bodyClass="p-0">
          {stats.data && stats.data.breakdown.nodes.length > 0 ? (
            <Table>
              <thead><tr><th className={th}>{t('common.node')}</th><th className={th}>{t('mdetail.checks')}</th><th className={th}>{t('mdetail.failures')}</th><th className={th}>{t('mdetail.avg')}</th><th className={th}>{t('mdetail.lastProbe')}</th></tr></thead>
              <tbody>
                {stats.data.breakdown.nodes.map((n) => (
                  <tr key={n.node_id}>
                    <td className={td}><span className={`me-2 inline-block size-2 rounded-full ${n.last_success ? 'bg-up' : 'bg-down'}`} />{nodeName(n.node_id)}</td>
                    <td className={`${td} tabular`}>{fmtNumber(loc, n.total)}</td>
                    <td className={`${td} tabular`}>{fmtNumber(loc, n.failures)}</td>
                    <td className={`${td} tabular`}>{fmtMs(loc, n.avg_latency_ms)}</td>
                    <td className={`${td} text-muted`}>{fmtRelative(loc, n.last_probe_at)}</td>
                  </tr>
                ))}
              </tbody>
            </Table>
          ) : <Empty>{t('common.noData')}</Empty>}
        </Card>
        <Card title={t('mdetail.recentChecks')} className="xl:col-span-2" bodyClass="p-0">
          {results.data && results.data.results.length > 0 ? (
            <Table>
              <thead><tr><th className={th}>{t('common.time')}</th><th className={th}>{t('mdetail.result')}</th><th className={th}>{t('mdetail.code')}</th><th className={th}>{t('mdetail.latency')}</th><th className={`${th} hidden md:table-cell`}>{t('mdetail.message')}</th></tr></thead>
              <tbody data-testid="recent-checks">
                {results.data.results.map((r) => (
                  <tr key={r.id}>
                    <td className={`${td} whitespace-nowrap text-muted`}>{fmtDateTime(loc, r.probe_at)}</td>
                    <td className={td}>{r.success
                      ? <span className="text-up">{r.slow ? t('status.DEGRADED') : t('mdetail.success')}</span>
                      : <span className="text-down">{t(`err.${r.error_category || 'unknown'}` as Key)}</span>}</td>
                    <td className={`${td} tabular`}>{r.http_status ?? '—'}</td>
                    <td className={`${td} tabular`}>{fmtMs(loc, r.latency_ms)}</td>
                    <td className={`${td} hidden max-w-xs truncate text-xs text-muted md:table-cell`} title={r.error_message}><span className="ltr">{r.error_message}</span></td>
                  </tr>
                ))}
              </tbody>
            </Table>
          ) : <Empty>{t('common.noData')}</Empty>}
        </Card>
        <Card title={t('mdetail.incidents')} bodyClass="p-0">
          {stats.data && stats.data.incidents.length > 0 ? (
            <ul className="divide-y divide-line">
              {stats.data.incidents.map((i) => (
                <li key={i.id}>
                  <Link to={`/incidents/${i.id}`} className="block px-4 py-2.5 text-sm hover:bg-surface-2">
                    <div className="flex items-center justify-between gap-2"><IncidentBadge status={i.status} /><span className="text-xs text-muted">{fmtDateTime(loc, i.started_at)}</span></div>
                    <div className="mt-1 text-xs text-muted">{t('incidents.duration')}: {i.resolved_at ? fmtDuration(loc, (new Date(i.resolved_at).getTime() - new Date(i.started_at).getTime()) / 1000) : t('incidents.ongoing')}</div>
                  </Link>
                </li>
              ))}
            </ul>
          ) : <p className="p-4 text-sm text-muted">{t('overview.noIncidents')}</p>}
        </Card>
      </div>
      <ConfirmDialog open={confirmDel} onClose={() => setConfirmDel(false)} title={t('common.delete')} busy={busy}
        message={t('mdetail.deleteConfirm', { name: m.name })} confirmLabel={t('common.delete')}
        onConfirm={() => act(async () => { await del(`/monitors/${id}`); navigate('/monitors') })} />
    </>
  )
}
