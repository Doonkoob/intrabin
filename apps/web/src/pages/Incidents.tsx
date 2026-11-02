import { useState } from 'react'
import { Link, useParams } from 'react-router'
import { AlertOctagon, ArrowLeft } from 'lucide-react'
import { ApiError, post } from '../api'
import { usePrefs, useSession } from '../app'
import { useApi } from '../hooks'
import { fmtDateTime, fmtDuration } from '../format'
import { Button, Card, Empty, ErrorBanner, IncidentBadge, PageHeader, Segmented, Spinner, Table, Textarea, td, th } from '../components/ui'
import type { Incident, IncidentEvent } from '../types'
import type { Key } from '../i18n'

function duration(loc: string, i: Incident, ongoing: string) {
  if (!i.resolved_at) return `${fmtDuration(loc, (Date.now() - new Date(i.started_at).getTime()) / 1000)} (${ongoing})`
  return fmtDuration(loc, (new Date(i.resolved_at).getTime() - new Date(i.started_at).getTime()) / 1000)
}

export function IncidentsPage() {
  const { t, loc } = usePrefs()
  const [filter, setFilter] = useState<'open' | 'all'>('open')
  const { data, error, loading, reload } = useApi<{ incidents: Incident[] }>(`/incidents?limit=200${filter === 'open' ? '&open=true' : ''}`, 15000)
  return (
    <>
      <PageHeader title={t('incidents.title')} actions={<Segmented label={t('incidents.title')} value={filter} onChange={setFilter} options={[{ value: 'open', label: t('incidents.open') }, { value: 'all', label: t('incidents.all') }]} />} />
      {error && <ErrorBanner error={error} onRetry={reload} />}
      <Card bodyClass="p-0">
        {loading && !data ? <Spinner /> : !data?.incidents.length ? <Empty icon={<AlertOctagon className="size-8" />}>{t('incidents.empty')}</Empty> : (
          <Table>
            <thead><tr><th className={th}>{t('incidents.monitor')}</th><th className={th}>{t('common.status')}</th><th className={th}>{t('incidents.started')}</th><th className={`${th} hidden sm:table-cell`}>{t('incidents.duration')}</th><th className={`${th} hidden md:table-cell`}>{t('mdetail.errors')}</th></tr></thead>
            <tbody>
              {data.incidents.map((i) => (
                <tr key={i.id} className="hover:bg-surface-2">
                  <td className={td}><Link to={`/incidents/${i.id}`} className="font-medium hover:underline">{i.monitor_name || i.title}</Link></td>
                  <td className={td}><IncidentBadge status={i.status} /></td>
                  <td className={`${td} whitespace-nowrap text-muted`}>{fmtDateTime(loc, i.started_at)}</td>
                  <td className={`${td} hidden tabular sm:table-cell`}>{duration(loc, i, t('incidents.ongoing'))}</td>
                  <td className={`${td} hidden text-muted md:table-cell`}>{i.last_error_category ? t(`err.${i.last_error_category}` as Key) : '—'}</td>
                </tr>
              ))}
            </tbody>
          </Table>
        )}
      </Card>
    </>
  )
}

export function IncidentDetailPage() {
  const { id } = useParams()
  const { t, loc } = usePrefs()
  const { can } = useSession()
  const { data, error, loading, reload } = useApi<{ incident: Incident; events: IncidentEvent[] }>(`/incidents/${id}`, 15000)
  const [note, setNote] = useState('')
  const [busy, setBusy] = useState<string | null>(null)
  const [err, setErr] = useState<string | null>(null)
  if (loading && !data) return <Spinner />
  if (error && !data) return <ErrorBanner error={error} onRetry={reload} />
  const i = data!.incident
  const act = async (action: 'acknowledge' | 'resolve' | 'notes') => {
    setBusy(action); setErr(null)
    try { await post(`/incidents/${id}/${action}`, { note }); setNote(''); await reload() } catch (e) { setErr((e as ApiError).message) } finally { setBusy(null) }
  }
  const open = i.status !== 'RESOLVED'
  const respond = can('incident.respond')
  return (
    <>
      <Link to="/incidents" className="mb-3 inline-flex items-center gap-1 text-sm text-muted hover:text-fg"><ArrowLeft className="size-4 rtl:-scale-x-100" />{t('nav.incidents')}</Link>
      <PageHeader title={<span className="flex flex-wrap items-center gap-2">{i.monitor_name || i.title}<IncidentBadge status={i.status} /></span>}
        subtitle={<Link to={`/monitors/${i.monitor_id}`} className="hover:underline">{t('incidents.monitor')}: {i.monitor_name}</Link>}
        actions={respond && open && <>
          {i.status !== 'ACKNOWLEDGED' && <Button onClick={() => act('acknowledge')} loading={busy === 'acknowledge'}>{t('incidents.acknowledge')}</Button>}
          <Button variant="primary" onClick={() => act('resolve')} loading={busy === 'resolve'}>{t('incidents.resolve')}</Button>
        </>} />
      {err && <div className="mb-4"><ErrorBanner error={err} /></div>}
      <div className="grid gap-4 lg:grid-cols-3">
        <Card title={t('common.details')}>
          <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-2 text-sm">
            <dt className="text-muted">{t('incidents.started')}</dt><dd>{fmtDateTime(loc, i.started_at)}</dd>
            <dt className="text-muted">{t('incidents.detected')}</dt><dd>{fmtDateTime(loc, i.detected_at)}</dd>
            <dt className="text-muted">{t('incidents.confirmed')}</dt><dd>{fmtDateTime(loc, i.confirmed_at)}</dd>
            {i.acknowledged_by && <><dt className="text-muted">{t('incidents.ackedBy')}</dt><dd>{i.acknowledged_by}</dd></>}
            <dt className="text-muted">{t('incidents.resolved')}</dt><dd>{fmtDateTime(loc, i.resolved_at)}</dd>
            <dt className="text-muted">{t('incidents.duration')}</dt><dd className="tabular">{duration(loc, i, t('incidents.ongoing'))}</dd>
            {i.resolution && <><dt className="text-muted">{t('incidents.resolution')}</dt><dd>{i.resolution}</dd></>}
            <dt className="text-muted">{t('mdetail.errors')}</dt><dd>{i.last_error_category ? t(`err.${i.last_error_category}` as Key) : '—'}</dd>
            <dt className="text-muted">{t('incidents.sources')}</dt><dd className="ltr break-all text-start text-xs">{i.confirmation_sources.join(', ') || '—'}</dd>
          </dl>
        </Card>
        <Card title={t('incidents.timeline')} className="lg:col-span-2">
          <ol className="relative ms-2 border-s border-line" data-testid="incident-timeline">
            {data!.events.map((e) => (
              <li key={e.id} className="mb-4 ms-4">
                <span className="absolute -start-1.5 mt-1.5 size-3 rounded-full border-2 border-surface bg-accent" aria-hidden />
                <div className="flex flex-wrap items-center gap-2 text-xs text-muted">
                  <time>{fmtDateTime(loc, e.at)}</time><span>·</span><span>{e.actor}</span>
                  {e.to_status && e.from_status !== e.to_status && <span className="inline-flex items-center gap-1">{e.from_status && <IncidentBadge status={e.from_status as Incident['status']} />}<span className="rtl:-scale-x-100">→</span><IncidentBadge status={e.to_status as Incident['status']} /></span>}
                </div>
                <p className="mt-1 text-sm">{e.message}</p>
              </li>
            ))}
          </ol>
          {respond && (
            <div className="mt-2 flex flex-col gap-2 border-t border-line pt-4">
              <label htmlFor="note" className="text-sm font-medium">{t('incidents.note')}</label>
              <Textarea id="note" value={note} onChange={(e) => setNote(e.target.value)} rows={2} />
              <div><Button onClick={() => act('notes')} disabled={!note.trim()} loading={busy === 'notes'}>{t('incidents.addNote')}</Button></div>
            </div>
          )}
        </Card>
      </div>
    </>
  )
}
