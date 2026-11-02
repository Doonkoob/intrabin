import { useEffect, useState } from 'react'
import { Link, useNavigate, useParams } from 'react-router'
import { ArrowLeft, CircleAlert, CircleCheck, CircleX, LifeBuoy, Pencil, Plus, Trash2, Undo2, Zap } from 'lucide-react'
import { ApiError, del, post, put } from '../api'
import { usePrefs, useSession } from '../app'
import { useApi } from '../hooks'
import { fmtDateTime, fmtDuration } from '../format'
import { Button, Card, Checkbox, ConfirmDialog, Dialog, Empty, ErrorBanner, Field, Input, PageHeader, Select, Spinner, Table, Textarea, td, th } from '../components/ui'
import { RunsTable } from './Automation'
import type { DNSProvider, DRCheck, DRConfig, DRPlan, DRResult, DRState, Monitor, WorkflowRun } from '../types'
import type { Key } from '../i18n'

const stateStyle: Record<DRState, string> = {
  primary: 'bg-up-soft text-up',
  failing_over: 'bg-info-soft text-info',
  failed_over: 'bg-warn-soft text-warn',
  failing_back: 'bg-info-soft text-info',
  attention: 'bg-down-soft text-down',
}

export function DRStateBadge({ state }: { state: DRState }) {
  const { t } = usePrefs()
  return <span data-state={state} className={`inline-flex whitespace-nowrap rounded-full px-2 py-0.5 text-xs font-medium ${stateStyle[state]}`}>{t(`dr.state.${state}` as Key)}</span>
}

function Mono({ children }: { children: React.ReactNode }) {
  return <span dir="ltr" className="ltr inline-block break-all font-mono text-xs">{children}</span>
}

export function DRPage() {
  const { t, loc } = usePrefs()
  const { can } = useSession()
  const plans = useApi<{ plans: DRPlan[] }>('/dr-plans', 5000)
  return (
    <>
      <PageHeader title={t('dr.title')} subtitle={t('dr.subtitle')}
        actions={can('dr.manage') && <Link to="/dr/new"><Button variant="primary"><Plus className="size-4" />{t('dr.new')}</Button></Link>} />
      <Card bodyClass="">
        {plans.loading && !plans.data ? <Spinner /> : plans.error ? <ErrorBanner error={plans.error} onRetry={plans.reload} /> :
          (plans.data?.plans.length ?? 0) === 0 ? <Empty icon={<LifeBuoy className="size-8" />}>{t('dr.empty')}</Empty> : (
            <Table>
              <thead><tr><th className={th}>{t('common.name')}</th><th className={th}>{t('common.status')}</th><th className={th}>{t('dr.record')}</th><th className={`${th} hidden md:table-cell`}>{t('dr.mode')}</th><th className={`${th} hidden md:table-cell`}>{t('dr.lastChange')}</th></tr></thead>
              <tbody>
                {plans.data!.plans.map((p) => (
                  <tr key={p.id} className="hover:bg-surface-2" data-testid="dr-plan-row">
                    <td className={td}><Link to={`/dr/${p.id}`} className="font-medium hover:underline">{p.name}</Link>{p.waiting && <div className="mt-0.5 text-xs text-warn">{t('dr.autoWaiting')}</div>}</td>
                    <td className={td}><DRStateBadge state={p.state} /></td>
                    <td className={td}><Mono>{p.config.record_name}</Mono><div className="text-xs text-muted"><Mono>{p.state === 'failed_over' ? p.config.secondary_value : p.config.primary_value}</Mono></div></td>
                    <td className={`${td} hidden text-muted md:table-cell`}>{p.config.automatic ? t('dr.automatic') : t('dr.manual')}</td>
                    <td className={`${td} hidden whitespace-nowrap text-muted md:table-cell`}>{fmtDateTime(loc, p.last_transition_at)}</td>
                  </tr>
                ))}
              </tbody>
            </Table>
          )}
      </Card>
    </>
  )
}

function CheckList({ checks }: { checks: DRCheck[] }) {
  const { t } = usePrefs()
  return (
    <ul className="flex flex-col gap-2" data-testid="dr-checks">
      {checks.map((c) => (
        <li key={c.id} className="flex items-start gap-2 text-sm" data-check={c.id} data-ok={c.ok}>
          {c.ok ? <CircleCheck className="mt-0.5 size-4 shrink-0 text-up" aria-label={t('dr.checkOk')} />
            : c.blocking ? <CircleX className="mt-0.5 size-4 shrink-0 text-down" aria-label={t('dr.checkFailed')} />
              : <CircleAlert className="mt-0.5 size-4 shrink-0 text-warn" aria-label={t('dr.checkInfo')} />}
          <div className="min-w-0">
            <div className="font-medium">{t(`dr.check.${c.id}` as Key)}</div>
            <div className="text-xs text-muted">{c.detail}</div>
          </div>
        </li>
      ))}
    </ul>
  )
}

function SwitchDialog({ plan, direction, onClose, onDone }: { plan: DRPlan; direction: 'failover' | 'failback'; onClose: () => void; onDone: (r: DRResult) => void }) {
  const { t } = usePrefs()
  const [res, setRes] = useState<DRResult | null>(null)
  const [note, setNote] = useState('')
  const [force, setForce] = useState(false)
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState<ApiError | null>(null)
  useEffect(() => {
    let live = true
    post<{ result: DRResult }>(`/dr-plans/${plan.id}/preview`, { direction })
      .then((r) => { if (live) setRes(r.result) })
      .catch((e) => { if (live) setErr(e as ApiError) })
    return () => { live = false }
  }, [plan.id, direction])
  const failing = (res?.checks ?? []).filter((c) => !c.ok && c.blocking)
  const hard = failing.some((c) => !c.overridable)
  const needsForce = failing.length > 0
  const submit = async () => {
    setBusy(true); setErr(null)
    try {
      const r = await post<{ result: DRResult }>(`/dr-plans/${plan.id}/${direction}`, { note, force: needsForce && force })
      onDone(r.result)
    } catch (e) {
      const ae = e as ApiError
      setErr(ae)
    } finally { setBusy(false) }
  }
  const canGo = !!res && !hard && (!needsForce || (force && note.trim() !== ''))
  return (
    <Dialog open onClose={onClose} wide title={direction === 'failover' ? t('dr.failoverTitle', { name: plan.name }) : t('dr.failbackTitle', { name: plan.name })}
      footer={<><Button onClick={onClose}>{t('common.cancel')}</Button>
        <Button variant={direction === 'failover' ? 'danger' : 'primary'} loading={busy} disabled={!canGo} onClick={submit} data-testid="dr-confirm">
          {direction === 'failover' ? t('dr.failoverNow') : t('dr.failbackNow')}</Button></>}>
      {err && <div className="mb-3"><ErrorBanner error={err.message} /></div>}
      {!res ? (!err && <Spinner />) : (
        <div className="flex flex-col gap-4">
          <p className="text-sm">{direction === 'failover' ? t('dr.failoverExplain') : t('dr.failbackExplain')}</p>
          <CheckList checks={res.checks} />
          <div>
            <div className="mb-1 text-sm font-medium">{t('dr.stepsTitle')}</div>
            <ol className="list-decimal ps-5 text-sm text-muted">{res.steps.map((s, i) => <li key={i}>{s}</li>)}</ol>
          </div>
          {hard && <p className="rounded-lg border border-down/40 bg-down-soft p-2.5 text-xs text-down" role="alert">{t('dr.hardBlocked')}</p>}
          {needsForce && !hard && <Checkbox label={t('dr.force')} checked={force} onChange={setForce} />}
          <Field label={needsForce && force ? t('dr.noteRequired') : t('dr.note')} htmlFor="dr-note" optional={!(needsForce && force)} error={err?.fields.note}>
            <Textarea id="dr-note" rows={2} value={note} onChange={(e) => setNote(e.target.value)} />
          </Field>
        </div>
      )}
    </Dialog>
  )
}

function ResolveDialog({ plan, onClose, onDone }: { plan: DRPlan; onClose: () => void; onDone: () => void }) {
  const { t } = usePrefs()
  const [state, setState] = useState<'primary' | 'failed_over' | ''>('')
  const [note, setNote] = useState('')
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState<ApiError | null>(null)
  const submit = async () => {
    setBusy(true); setErr(null)
    try { await post(`/dr-plans/${plan.id}/resolve`, { state, note }); onDone() } catch (e) { setErr(e as ApiError) } finally { setBusy(false) }
  }
  return (
    <Dialog open onClose={onClose} title={t('dr.resolveTitle')}
      footer={<><Button onClick={onClose}>{t('common.cancel')}</Button><Button variant="primary" loading={busy} disabled={!state || !note.trim()} onClick={submit} data-testid="dr-resolve-submit">{t('dr.resolve')}</Button></>}>
      <p className="text-sm">{t('dr.resolveHelp')}</p>
      {err && <div className="mt-3"><ErrorBanner error={err.message} /></div>}
      <fieldset className="mt-4 flex flex-col gap-2 text-sm">
        <legend className="mb-1 font-medium">{t('dr.trafficNow')}</legend>
        <label className="flex items-center gap-2"><input type="radio" name="drs" checked={state === 'primary'} onChange={() => setState('primary')} className="accent-[var(--accent)]" />{t('dr.onPrimary')}</label>
        <label className="flex items-center gap-2"><input type="radio" name="drs" checked={state === 'failed_over'} onChange={() => setState('failed_over')} className="accent-[var(--accent)]" />{t('dr.onSecondary')}</label>
      </fieldset>
      <div className="mt-4"><Field label={t('wf.reconcileNote')} htmlFor="drs-note" error={err?.fields.note}><Textarea id="drs-note" rows={3} value={note} onChange={(e) => setNote(e.target.value)} /></Field></div>
    </Dialog>
  )
}

export function DRDetailPage() {
  const { id = '' } = useParams()
  const { t, loc } = usePrefs()
  const { can } = useSession()
  const navigate = useNavigate()
  const { data, error, loading, reload } = useApi<{ plan: DRPlan; runs: WorkflowRun[] }>(`/dr-plans/${id}`, 3000)
  const monitors = useApi<{ monitors: Monitor[] }>('/monitors')
  const providers = useApi<{ providers: DNSProvider[] }>('/dns/providers')
  const [sw, setSw] = useState<'failover' | 'failback' | null>(null)
  const [resolve, setResolve] = useState(false)
  const [deleting, setDeleting] = useState(false)
  const [err, setErr] = useState<string | null>(null)
  if (loading && !data) return <Spinner />
  if (error && !data) return <ErrorBanner error={error} onRetry={reload} />
  const p = data!.plan
  const c = p.config
  const monName = (mid: string) => monitors.data?.monitors.find((m) => m.id === mid)?.name ?? mid
  const busy = p.state === 'failing_over' || p.state === 'failing_back'
  const remove = async () => {
    try { await del(`/dr-plans/${id}`); navigate('/dr') } catch (e) { setErr((e as Error).message); setDeleting(false) }
  }
  const dl = (k: string, v: React.ReactNode) => <><dt className="text-muted">{k}</dt><dd className="min-w-0">{v}</dd></>
  return (
    <>
      <Link to="/dr" className="mb-3 inline-flex items-center gap-1 text-sm text-muted hover:text-fg"><ArrowLeft className="size-4 rtl:-scale-x-100" />{t('dr.title')}</Link>
      <PageHeader title={<span className="flex flex-wrap items-center gap-2">{p.name}<DRStateBadge state={p.state} /></span>} subtitle={p.description || undefined}
        actions={can('dr.manage') && <>
          {(p.state === 'primary' || p.state === 'attention') && <Button variant="danger" onClick={() => setSw('failover')} data-testid="dr-failover"><Zap className="size-4" />{t('dr.failover')}</Button>}
          {(p.state === 'failed_over' || p.state === 'attention') && <Button variant="primary" onClick={() => setSw('failback')} data-testid="dr-failback"><Undo2 className="size-4" />{t('dr.failback')}</Button>}
          {p.state === 'attention' && <Button onClick={() => setResolve(true)} data-testid="dr-resolve">{t('dr.resolve')}</Button>}
          {!busy && <Link to={`/dr/${id}/edit`}><Button variant="ghost" aria-label={t('common.edit')}><Pencil className="size-4" /></Button></Link>}
          {!busy && <Button variant="ghost" aria-label={t('common.delete')} onClick={() => setDeleting(true)}><Trash2 className="size-4" /></Button>}
        </>} />
      {err && <div className="mb-4"><ErrorBanner error={err} /></div>}
      {p.state === 'attention' && <div role="alert" className="mb-4 flex items-start gap-3 rounded-lg border border-down/40 bg-down-soft p-3 text-sm text-down"><CircleAlert className="mt-0.5 size-4 shrink-0" />{p.state_note}</div>}
      {p.waiting && <div role="status" className="mb-4 rounded-lg border border-warn/40 bg-warn-soft p-3 text-sm text-warn" data-testid="dr-waiting">{p.waiting}</div>}
      {busy && p.active_run_id && <div className="mb-4 rounded-lg border border-info/40 bg-info-soft p-3 text-sm text-info">{t(`dr.inProgress.${p.direction || 'failover'}` as Key)} <Link to={`/automation/runs/${p.active_run_id}`} className="underline">{t('dr.viewRun')}</Link></div>}
      <div className="grid gap-4 lg:grid-cols-2">
        <Card title={t('dr.traffic')}>
          <dl className="grid grid-cols-[9rem_1fr] gap-x-4 gap-y-2 text-sm">
            {dl(t('dr.record'), <Mono>{c.record_type} {c.record_name}</Mono>)}
            {dl(t('dr.primaryValue'), <span className="flex items-center gap-2"><Mono>{c.primary_value}</Mono>{p.state === 'primary' && <span className="text-xs text-up">{t('dr.active')}</span>}</span>)}
            {dl(t('dr.secondaryValue'), <span className="flex items-center gap-2"><Mono>{c.secondary_value}</Mono>{p.state === 'failed_over' && <span className="text-xs text-warn">{t('dr.active')}</span>}</span>)}
            {dl(t('dns.title'), `${providers.data?.providers.find((x) => x.id === c.provider_id)?.name ?? c.provider_id} · ${c.zone_name} · TTL ${c.ttl === 1 ? t('dns.ttlAuto') : fmtDuration(loc, c.ttl)}`)}
            {dl(t('dr.lastChange'), fmtDateTime(loc, p.last_transition_at))}
            {p.state_note && p.state !== 'attention' && dl(t('dr.lastNote'), <span className="text-muted">{p.state_note}</span>)}
          </dl>
        </Card>
        <Card title={t('dr.policy')}>
          <dl className="grid grid-cols-[9rem_1fr] gap-x-4 gap-y-2 text-sm">
            {dl(t('dr.mode'), c.automatic ? <span className="text-warn">{t('dr.automaticLong')}</span> : t('dr.manualLong'))}
            {dl(t('dr.watches'), c.monitor_ids.map(monName).join(', '))}
            {dl(t('dr.secondaryMonitor'), c.secondary_monitor_id ? monName(c.secondary_monitor_id) : t('common.none'))}
            {dl(t('dr.cooldown'), fmtDuration(loc, c.cooldown_seconds))}
            {dl(t('dr.extraSteps'), `${c.pre_steps.length} / ${c.post_steps.length}`)}
          </dl>
        </Card>
      </div>
      <Card title={t('dr.history')} className="mt-4" bodyClass=""><RunsTable runs={data!.runs} /></Card>
      {sw && <SwitchDialog plan={p} direction={sw} onClose={() => setSw(null)} onDone={(r) => { setSw(null); reload(); if (r.run) navigate(`/automation/runs/${r.run.id}`) }} />}
      {resolve && <ResolveDialog plan={p} onClose={() => setResolve(false)} onDone={() => { setResolve(false); reload() }} />}
      <ConfirmDialog open={deleting} onClose={() => setDeleting(false)} onConfirm={remove} title={t('dr.delete')} message={t('dr.deleteMsg', { name: p.name })} confirmLabel={t('common.delete')} />
    </>
  )
}

// ---- editor ----

export function DRFormPage() {
  const { id } = useParams()
  const editing = !!id
  const { t } = usePrefs()
  const navigate = useNavigate()
  const existing = useApi<{ plan: DRPlan }>(editing ? `/dr-plans/${id}` : null)
  const monitors = useApi<{ monitors: Monitor[] }>('/monitors')
  const providers = useApi<{ providers: DNSProvider[] }>('/dns/providers')
  if (editing && existing.loading && !existing.data) return <Spinner />
  if (editing && existing.error) return <ErrorBanner error={existing.error} onRetry={existing.reload} />
  if (!monitors.data || !providers.data) return <Spinner />
  return <DRForm key={existing.data?.plan.id ?? 'new'} initial={existing.data?.plan} monitors={monitors.data.monitors} providers={providers.data.providers}
    onSaved={(p) => navigate(`/dr/${p.id}`)} onCancel={() => navigate(editing ? `/dr/${id}` : '/dr')} title={editing ? t('dr.edit') : t('dr.new')} />
}

const emptyConfig: DRConfig = { monitor_ids: [], secondary_monitor_id: '', automatic: false, cooldown_seconds: 1800, provider_id: '', zone_name: '', record_type: 'A', record_name: '',
  primary_value: '', secondary_value: '', ttl: 60, proxied: false, pre_steps: [], post_steps: [] }

function DRForm({ initial, monitors, providers, onSaved, onCancel, title }: { initial?: DRPlan; monitors: Monitor[]; providers: DNSProvider[]; onSaved: (p: DRPlan) => void; onCancel: () => void; title: string }) {
  const { t } = usePrefs()
  const [name, setName] = useState(initial?.name ?? '')
  const [description, setDescription] = useState(initial?.description ?? '')
  const [c, setC] = useState<DRConfig>(initial?.config ?? emptyConfig)
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState<ApiError | null>(null)
  const f = err?.fields ?? {}
  const upd = (p: Partial<DRConfig>) => setC({ ...c, ...p })
  const sorted = [...monitors].sort((a, b) => a.name.localeCompare(b.name))
  const prov = providers.find((p) => p.id === c.provider_id)
  const save = async () => {
    setBusy(true); setErr(null)
    const body = { name, description, config: { ...c, secondary_monitor_id: c.secondary_monitor_id || '' } }
    try {
      const r = initial ? await put<{ plan: DRPlan }>(`/dr-plans/${initial.id}`, body) : await post<{ plan: DRPlan }>('/dr-plans', body)
      onSaved(r.plan)
    } catch (e) { setErr(e as ApiError); window.scrollTo({ top: 0, behavior: 'smooth' }) } finally { setBusy(false) }
  }
  return (
    <>
      <PageHeader title={title} />
      {err && <div className="mb-4"><ErrorBanner error={err.message} /></div>}
      <div className="flex flex-col gap-4">
        <Card title={t('common.details')}>
          <div className="grid gap-4 md:grid-cols-2">
            <Field label={t('common.name')} htmlFor="dr-name" error={f.name}><Input id="dr-name" value={name} onChange={(e) => setName(e.target.value)} /></Field>
            <Field label={t('common.description')} htmlFor="dr-desc" optional><Input id="dr-desc" value={description} onChange={(e) => setDescription(e.target.value)} /></Field>
          </div>
        </Card>
        <Card title={t('dr.health')}>
          <div className="grid gap-4 md:grid-cols-2">
            <fieldset>
              <legend className="mb-1 text-sm font-medium">{t('dr.watches')}</legend>
              <div className="max-h-48 overflow-y-auto rounded-lg border border-line p-2">
                {sorted.length === 0 ? <p className="text-xs text-muted">{t('wf.noMonitors')}</p> : sorted.map((m) => (
                  <div key={m.id}><Checkbox label={m.name} checked={c.monitor_ids.includes(m.id)} onChange={(v) => upd({ monitor_ids: v ? [...c.monitor_ids, m.id] : c.monitor_ids.filter((x) => x !== m.id) })} /></div>
                ))}
              </div>
              {f['config.monitor_ids'] && <p className="mt-1 text-xs text-down">{f['config.monitor_ids']}</p>}
              <p className="mt-1 text-xs text-muted">{t('dr.watchesHelp')}</p>
            </fieldset>
            <div className="flex flex-col gap-3">
              <Field label={t('dr.secondaryMonitor')} htmlFor="dr-sec" optional error={f['config.secondary_monitor_id']} help={t('dr.secondaryMonitorHelp')}>
                <Select id="dr-sec" value={c.secondary_monitor_id ?? ''} onChange={(e) => upd({ secondary_monitor_id: e.target.value })}>
                  <option value="">{t('common.none')}</option>
                  {sorted.map((m) => <option key={m.id} value={m.id}>{m.name}</option>)}
                </Select>
              </Field>
              <Field label={t('dr.cooldownMinutes')} htmlFor="dr-cool" error={f['config.cooldown_seconds']} help={t('dr.cooldownHelp')}>
                <Input id="dr-cool" type="number" min={1} value={Math.round(c.cooldown_seconds / 60)} onChange={(e) => upd({ cooldown_seconds: Number(e.target.value) * 60 })} />
              </Field>
            </div>
          </div>
          <div className="mt-4 flex flex-col gap-2">
            <Checkbox id="dr-auto" label={t('dr.automaticToggle')} checked={c.automatic} onChange={(v) => upd({ automatic: v })} />
            {c.automatic && <p className="rounded-lg border border-warn/40 bg-warn-soft p-2.5 text-xs text-warn" role="note">{t('dr.automaticWarning')}</p>}
          </div>
        </Card>
        <Card title={t('dr.dnsSwitch')}>
          {providers.length === 0 && <p className="mb-3 rounded-lg border border-warn/40 bg-warn-soft p-2.5 text-xs text-warn">{t('wf.dns.noProviders')} <Link to="/dns" className="underline">{t('dns.title')}</Link></p>}
          <div className="grid gap-4 md:grid-cols-3">
            <Field label={t('wf.dns.integration')} htmlFor="dr-prov" error={f['config.provider_id']}>
              <Select id="dr-prov" value={c.provider_id} onChange={(e) => upd({ provider_id: e.target.value })}>
                <option value="">{t('wf.dns.choose')}</option>
                {providers.map((p) => <option key={p.id} value={p.id}>{p.name}</option>)}
              </Select>
            </Field>
            <Field label={t('dns.zone')} htmlFor="dr-zone" error={f['config.zone_name']}><Input id="dr-zone" dir="ltr" placeholder="example.com" value={c.zone_name} onChange={(e) => upd({ zone_name: e.target.value })} /></Field>
            <Field label={t('dns.recordType')} htmlFor="dr-type" error={f['config.record_type']}>
              <Select id="dr-type" value={c.record_type} onChange={(e) => upd({ record_type: e.target.value })}>{['A', 'AAAA', 'CNAME'].map((x) => <option key={x}>{x}</option>)}</Select>
            </Field>
            <Field label={t('wf.dns.recordName')} htmlFor="dr-rec" error={f['config.record_name']}><Input id="dr-rec" dir="ltr" placeholder="app.example.com" value={c.record_name} onChange={(e) => upd({ record_name: e.target.value })} /></Field>
            <Field label={t('dr.primaryValue')} htmlFor="dr-pv" error={f['config.primary_value']}><Input id="dr-pv" dir="ltr" value={c.primary_value} onChange={(e) => upd({ primary_value: e.target.value })} /></Field>
            <Field label={t('dr.secondaryValue')} htmlFor="dr-sv" error={f['config.secondary_value']}><Input id="dr-sv" dir="ltr" value={c.secondary_value} onChange={(e) => upd({ secondary_value: e.target.value })} /></Field>
            <Field label="TTL" htmlFor="dr-ttl" help={t('dr.ttlHelp')}><Input id="dr-ttl" type="number" min={1} value={c.ttl} onChange={(e) => upd({ ttl: Number(e.target.value) })} /></Field>
            {prov?.type === 'cloudflare' && <div className="flex items-end pb-2"><Checkbox label={t('dns.proxied')} checked={c.proxied} onChange={(v) => upd({ proxied: v })} /></div>}
          </div>
          {(c.pre_steps.length > 0 || c.post_steps.length > 0) && <p className="mt-3 text-xs text-muted">{t('dr.extraStepsKept', { pre: c.pre_steps.length, post: c.post_steps.length })}</p>}
        </Card>
        <div className="flex justify-end gap-2">
          <Button onClick={onCancel}>{t('common.cancel')}</Button>
          <Button variant="primary" loading={busy} onClick={save} data-testid="save-dr">{t('common.save')}</Button>
        </div>
      </div>
    </>
  )
}
