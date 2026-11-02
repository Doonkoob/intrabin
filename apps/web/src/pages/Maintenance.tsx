import { useState } from 'react'
import { CalendarClock, Plus, Trash2 } from 'lucide-react'
import { ApiError, del, post } from '../api'
import { usePrefs, useSession } from '../app'
import { useApi } from '../hooks'
import { fmtDateTime, toLocalInput } from '../format'
import { Button, Card, Dialog, Empty, ErrorBanner, Field, Input, PageHeader, Select, Spinner, Table, td, th } from '../components/ui'
import type { MaintenanceWindow, Monitor } from '../types'

export function MaintenancePage() {
  const { t, loc } = usePrefs()
  const { can } = useSession()
  const { data, error, loading, reload } = useApi<{ maintenance_windows: MaintenanceWindow[] }>('/maintenance-windows', 30000)
  const monitors = useApi<{ monitors: Monitor[] }>('/monitors')
  const [open, setOpen] = useState(false)
  const now = new Date()
  const [form, setForm] = useState({ title: '', monitor_id: '', starts_at: toLocalInput(now), ends_at: toLocalInput(new Date(now.getTime() + 3600000)) })
  const [fields, setFields] = useState<Record<string, string>>({})
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState<string | null>(null)
  const manage = can('monitor.manage')
  const monName = (id?: string) => (id ? monitors.data?.monitors.find((m) => m.id === id)?.name ?? id : t('maint.allMonitors'))
  const save = async () => {
    setBusy(true); setFields({}); setErr(null)
    try {
      await post('/maintenance-windows', { ...form, starts_at: new Date(form.starts_at).toISOString(), ends_at: new Date(form.ends_at).toISOString() })
      setOpen(false); await reload()
    } catch (e) { const ae = e as ApiError; setFields(ae.fields); if (!Object.keys(ae.fields).length) setErr(ae.message) } finally { setBusy(false) }
  }
  const state = (w: MaintenanceWindow) => {
    const s = new Date(w.starts_at).getTime(), e = new Date(w.ends_at).getTime(), n = Date.now()
    if (n >= s && n < e) return <span className="rounded-full bg-info-soft px-2 py-0.5 text-xs text-info">{t('maint.active')}</span>
    if (n < s) return <span className="rounded-full bg-surface-2 px-2 py-0.5 text-xs text-muted">{t('maint.scheduled')}</span>
    return <span className="text-xs text-muted">{t('maint.ended')}</span>
  }
  return (
    <>
      <PageHeader title={t('maint.title')} subtitle={t('maint.help')} actions={manage && <Button variant="primary" onClick={() => setOpen(true)}><Plus className="size-4" />{t('maint.new')}</Button>} />
      {error && <ErrorBanner error={error} onRetry={reload} />}
      {err && !open && <ErrorBanner error={err} />}
      <Card bodyClass="p-0">
        {loading && !data ? <Spinner /> : !data?.maintenance_windows.length ? <Empty icon={<CalendarClock className="size-8" />}>{t('maint.empty')}</Empty> : (
          <Table>
            <thead><tr><th className={th}>{t('maint.titleField')}</th><th className={th}>{t('maint.monitor')}</th><th className={th}>{t('maint.starts')}</th><th className={th}>{t('maint.ends')}</th><th className={th}>{t('common.status')}</th>{manage && <th className={th}><span className="sr-only">{t('common.actions')}</span></th>}</tr></thead>
            <tbody>
              {data.maintenance_windows.map((w) => (
                <tr key={w.id}>
                  <td className={`${td} font-medium`}>{w.title}</td>
                  <td className={td}>{monName(w.monitor_id)}</td>
                  <td className={`${td} whitespace-nowrap`}>{fmtDateTime(loc, w.starts_at)}</td>
                  <td className={`${td} whitespace-nowrap`}>{fmtDateTime(loc, w.ends_at)}</td>
                  <td className={td}>{state(w)}</td>
                  {manage && <td className={`${td} text-end`}><Button size="sm" variant="ghost" aria-label={t('common.delete')} onClick={async () => { try { await del(`/maintenance-windows/${w.id}`); await reload() } catch (e) { setErr((e as Error).message) } }}><Trash2 className="size-4" /></Button></td>}
                </tr>
              ))}
            </tbody>
          </Table>
        )}
      </Card>
      <Dialog open={open} onClose={() => setOpen(false)} title={t('maint.new')}
        footer={<><Button onClick={() => setOpen(false)}>{t('common.cancel')}</Button><Button variant="primary" loading={busy} onClick={save}>{t('common.save')}</Button></>}>
        <div className="flex flex-col gap-4">
          {err && <ErrorBanner error={err} />}
          <Field label={t('maint.titleField')} htmlFor="mt" error={fields.title}><Input id="mt" value={form.title} onChange={(e) => setForm({ ...form, title: e.target.value })} invalid={!!fields.title} /></Field>
          <Field label={t('maint.monitor')} htmlFor="mm" error={fields.monitor_id}>
            <Select id="mm" value={form.monitor_id} onChange={(e) => setForm({ ...form, monitor_id: e.target.value })}>
              <option value="">{t('maint.allMonitors')}</option>
              {monitors.data?.monitors.map((m) => <option key={m.id} value={m.id}>{m.name}</option>)}
            </Select>
          </Field>
          <div className="grid gap-4 sm:grid-cols-2">
            <Field label={t('maint.starts')} htmlFor="ms"><Input id="ms" type="datetime-local" value={form.starts_at} onChange={(e) => setForm({ ...form, starts_at: e.target.value })} /></Field>
            <Field label={t('maint.ends')} htmlFor="me" error={fields.ends_at}><Input id="me" type="datetime-local" value={form.ends_at} onChange={(e) => setForm({ ...form, ends_at: e.target.value })} invalid={!!fields.ends_at} /></Field>
          </div>
        </div>
      </Dialog>
    </>
  )
}
