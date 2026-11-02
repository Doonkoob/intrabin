import { useMemo, useState, type ReactNode } from 'react'
import { Link, useNavigate, useParams } from 'react-router'
import { ArrowDown, ArrowLeft, ArrowUp, CircleAlert, Play, Plus, Trash2, Workflow as WorkflowIcon } from 'lucide-react'
import { ApiError, del, post, put } from '../api'
import { usePrefs, useSession } from '../app'
import { useApi } from '../hooks'
import { fmtDateTime, fmtDuration } from '../format'
import { Button, Card, Checkbox, ConfirmDialog, Dialog, Empty, ErrorBanner, Field, IncidentBadge, Input, PageHeader, Segmented, Select, Spinner, Table, Textarea, td, th } from '../components/ui'
import type { ActionStatus, AutomationStatus, DNSProvider, IncidentStatus, Monitor, RunStatus, Workflow, WorkflowAction, WorkflowRun, WorkflowStep, WorkflowTrigger } from '../types'
import type { Key } from '../i18n'

const runStyle: Record<string, string> = {
  pending: 'bg-neutral-soft text-muted',
  running: 'bg-info-soft text-info',
  waiting: 'bg-info-soft text-info',
  succeeded: 'bg-up-soft text-up',
  reconciled_succeeded: 'bg-up-soft text-up',
  failed: 'bg-down-soft text-down',
  reconciled_failed: 'bg-down-soft text-down',
  cancelled: 'bg-neutral-soft text-muted',
  unknown_outcome: 'bg-warn-soft text-warn',
  dispatching: 'bg-info-soft text-info',
}

export function RunBadge({ status }: { status: RunStatus | ActionStatus }) {
  const { t } = usePrefs()
  return <span data-status={status} className={`inline-flex whitespace-nowrap rounded-full px-2 py-0.5 text-xs font-medium ${runStyle[status] ?? 'bg-neutral-soft text-muted'}`}>{t(`run.${status}` as Key)}</span>
}

function Ltr({ children }: { children: ReactNode }) {
  return <span dir="ltr" className="ltr inline-block font-mono text-xs">{children}</span>
}

const opSymbol: Record<string, string> = { gt: '>', ge: '≥', lt: '<', le: '≤', eq: '=', ne: '≠' }

function conditionText(t: (k: Key, v?: Record<string, string | number>) => string, loc: string, tr: WorkflowTrigger) {
  const value = tr.metric === 'status' ? t(`status.${tr.status_value ?? 'DOWN'}` as Key) : `${new Intl.NumberFormat(loc, { maximumFractionDigits: 2 }).format(tr.threshold ?? 0)} ${t(`wf.cond.unit.${tr.metric}` as Key)}`.trim()
  return `${t(`wf.cond.metric.${tr.metric}` as Key)} ${opSymbol[tr.op ?? 'gt']} ${value}`
}

function triggerLabel(t: (k: Key, v?: Record<string, string | number>) => string, loc: string, tr: WorkflowTrigger) {
  if (tr.type === 'condition') return t('wf.trigger.onCondition', { c: conditionText(t, loc, tr) })
  if (tr.type === 'schedule') return t('wf.trigger.everyN', { n: fmtDuration(loc, tr.interval_seconds ?? 0) })
  const names = (tr.on_statuses ?? ['CONFIRMED']).map((s) => t(`incident.${s}` as Key))
  if (tr.type === 'incident') return t('wf.trigger.onIncident', { s: new Intl.ListFormat(loc, { type: 'disjunction' }).format(names) })
  return t('wf.trigger.manual')
}

function RunnerCard({ st }: { st: AutomationStatus }) {
  const { t } = usePrefs()
  const r = st.runner
  const tone = !r.configured ? 'text-muted' : r.reachable ? 'text-up' : 'text-down'
  const label = !r.configured ? t('wf.runner.notConfigured') : r.reachable ? t('wf.runner.ok') : t('wf.runner.unreachable')
  return (
    <div className="mb-4 grid gap-3 sm:grid-cols-3" data-testid="automation-status">
      <div className="rounded-xl border border-line bg-surface p-4">
        <div className="text-xs font-medium text-muted">{t('wf.runner.title')}</div>
        <div className={`mt-1 text-sm font-semibold ${tone}`}>{label}</div>
        <div className="mt-1 text-xs text-muted">{r.reachable ? Object.keys(r.interpreters).join(', ') || '—' : t('wf.runner.help')}</div>
      </div>
      <div className="rounded-xl border border-line bg-surface p-4">
        <div className="text-xs font-medium text-muted">{t('wf.activeRuns')}</div>
        <div className="mt-1 text-2xl font-semibold tabular">{(st.runs.pending ?? 0) + (st.runs.running ?? 0) + (st.runs.waiting ?? 0)}</div>
      </div>
      <Link to="/automation?tab=runs&status=unknown_outcome" className="rounded-xl border border-line bg-surface p-4 hover:bg-surface-2">
        <div className="text-xs font-medium text-muted">{t('wf.needsReconcile')}</div>
        <div className={`mt-1 text-2xl font-semibold tabular ${(st.runs.unknown_outcome ?? 0) > 0 ? 'text-warn' : ''}`}>{st.runs.unknown_outcome ?? 0}</div>
      </Link>
    </div>
  )
}

export function AutomationPage() {
  const { t, loc } = usePrefs()
  const { can } = useSession()
  const params = new URLSearchParams(window.location.search)
  const [tab, setTab] = useState<'workflows' | 'runs'>(params.get('tab') === 'runs' ? 'runs' : 'workflows')
  const [runFilter, setRunFilter] = useState(params.get('status') ?? '')
  const status = useApi<AutomationStatus>('/automation/status', 10000)
  const wfs = useApi<{ workflows: Workflow[] }>(tab === 'workflows' ? '/workflows' : null, 15000)
  const runs = useApi<{ runs: WorkflowRun[] }>(tab === 'runs' ? `/workflow-runs?limit=200${runFilter ? '&status=' + runFilter : ''}` : null, 5000)
  return (
    <>
      <PageHeader title={t('wf.title')} subtitle={t('wf.subtitle')}
        actions={<>
          <Segmented label={t('wf.title')} value={tab} onChange={setTab} options={[{ value: 'workflows', label: t('wf.tab.workflows') }, { value: 'runs', label: t('wf.tab.runs') }]} />
          {can('automation.manage') && <Link to="/automation/new"><Button variant="primary"><Plus className="size-4" />{t('wf.new')}</Button></Link>}
        </>} />
      {status.data && <RunnerCard st={status.data} />}
      {tab === 'workflows' ? (
        <Card bodyClass="p-0">
          {wfs.error && <div className="p-4"><ErrorBanner error={wfs.error} onRetry={wfs.reload} /></div>}
          {wfs.loading && !wfs.data ? <Spinner /> : !wfs.data?.workflows.length ? <Empty icon={<WorkflowIcon className="size-8" />}>{t('wf.empty')}</Empty> : (
            <Table>
              <thead><tr><th className={th}>{t('common.name')}</th><th className={th}>{t('wf.trigger')}</th><th className={th}>{t('common.status')}</th><th className={`${th} hidden md:table-cell`}>{t('wf.steps')}</th><th className={`${th} hidden md:table-cell`}>{t('wf.updated')}</th></tr></thead>
              <tbody>
                {wfs.data.workflows.map((w) => (
                  <tr key={w.id} className="hover:bg-surface-2">
                    <td className={td}><Link to={`/automation/${w.id}`} className="font-medium hover:underline">{w.name}</Link></td>
                    <td className={`${td} text-muted`}>{triggerLabel(t, loc, w.trigger)}</td>
                    <td className={td}>{w.enabled ? <span className="text-up">{t('wf.enabled')}</span> : <span className="text-muted">{t('wf.disabled')}</span>}</td>
                    <td className={`${td} hidden tabular md:table-cell`}>{w.steps.length}</td>
                    <td className={`${td} hidden whitespace-nowrap text-muted md:table-cell`}>{fmtDateTime(loc, w.updated_at)}</td>
                  </tr>
                ))}
              </tbody>
            </Table>
          )}
        </Card>
      ) : (
        <Card bodyClass="p-0" title={t('wf.tab.runs')} actions={
          <Select aria-label={t('common.status')} value={runFilter} onChange={(e) => setRunFilter(e.target.value)} className="h-8 w-auto">
            <option value="">{t('common.all')}</option>
            {(['pending', 'running', 'waiting', 'unknown_outcome', 'succeeded', 'failed', 'cancelled'] as RunStatus[]).map((s) => <option key={s} value={s}>{t(`run.${s}` as Key)}</option>)}
          </Select>}>
          {runs.error && <div className="p-4"><ErrorBanner error={runs.error} onRetry={runs.reload} /></div>}
          {runs.loading && !runs.data ? <Spinner /> : <RunsTable runs={runs.data?.runs ?? []} showWorkflow />}
        </Card>
      )}
    </>
  )
}

export function RunsTable({ runs, showWorkflow }: { runs: WorkflowRun[]; showWorkflow?: boolean }) {
  const { t, loc } = usePrefs()
  if (!runs.length) return <Empty>{t('wf.noRuns')}</Empty>
  return (
    <Table>
      <thead><tr>{showWorkflow && <th className={th}>{t('wf.workflow')}</th>}<th className={th}>{t('common.status')}</th><th className={th}>{t('wf.trigger')}</th><th className={th}>{t('wf.started')}</th><th className={`${th} hidden md:table-cell`}>{t('wf.progress')}</th></tr></thead>
      <tbody>
        {runs.map((r) => (
          <tr key={r.id} className="hover:bg-surface-2" data-testid="run-row">
            {showWorkflow && <td className={td}><Link to={`/automation/runs/${r.id}`} className="font-medium hover:underline">{r.workflow_name}</Link></td>}
            <td className={td}><Link to={`/automation/runs/${r.id}`}><RunBadge status={r.status} /></Link></td>
            <td className={`${td} text-muted`}>{t(`wf.triggerType.${r.trigger_type}` as Key)}{r.created_by && r.trigger_type === 'manual' ? ` · ${r.created_by}` : ''}</td>
            <td className={`${td} whitespace-nowrap text-muted`}>{fmtDateTime(loc, r.created_at)}</td>
            <td className={`${td} hidden tabular md:table-cell`}>{Math.min(r.current_step, r.steps.length)}/{r.steps.length}</td>
          </tr>
        ))}
      </tbody>
    </Table>
  )
}

function StepSummary({ s }: { s: WorkflowStep }) {
  const { t } = usePrefs()
  switch (s.type) {
    case 'script': return <span>{s.language === 'python' ? 'Python' : 'Bash'} · {t('wf.timeoutN', { n: s.timeout_seconds ?? 60 })}</span>
    case 'http': return <span><Ltr>{s.method} {s.url}</Ltr></span>
    case 'wait': return <span>{t('wf.waitN', { n: s.seconds ?? 0 })}</span>
    case 'incident_status': return <span>{t('wf.setStatus')} <IncidentBadge status={s.status!} /></span>
  }
}

export function WorkflowDetailPage() {
  const { id } = useParams()
  const { t, loc } = usePrefs()
  const { can, me } = useSession()
  const navigate = useNavigate()
  const { data, error, loading, reload } = useApi<{ workflow: Workflow; runs: WorkflowRun[] }>(`/workflows/${id}`, 5000)
  const [confirm, setConfirm] = useState<'run' | 'delete' | null>(null)
  const [incidentId, setIncidentId] = useState('')
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState<string | null>(null)
  if (loading && !data) return <Spinner />
  if (error && !data) return <ErrorBanner error={error} onRetry={reload} />
  const w = data!.workflow
  const canRun = can('automation.manage') || (can('automation.run') && w.operator_can_run)
  const usesIncident = w.steps.some((s) => s.type === 'incident_status')
  const run = async () => {
    setBusy(true); setErr(null)
    try {
      const r = await post<{ run: WorkflowRun }>(`/workflows/${id}/run`, incidentId.trim() ? { context: { incident_id: incidentId.trim() } } : {})
      navigate(`/automation/runs/${r.run.id}`)
    } catch (e) { setErr((e as ApiError).message); setConfirm(null) } finally { setBusy(false) }
  }
  const remove = async () => {
    setBusy(true); setErr(null)
    try { await del(`/workflows/${id}`); navigate('/automation') } catch (e) { setErr((e as ApiError).message); setConfirm(null) } finally { setBusy(false) }
  }
  return (
    <>
      <Link to="/automation" className="mb-3 inline-flex items-center gap-1 text-sm text-muted hover:text-fg"><ArrowLeft className="size-4 rtl:-scale-x-100" />{t('wf.title')}</Link>
      <PageHeader title={<span className="flex flex-wrap items-center gap-2">{w.name}<span className={`rounded-full px-2 py-0.5 text-xs font-medium ${w.enabled ? 'bg-up-soft text-up' : 'bg-neutral-soft text-muted'}`}>{w.enabled ? t('wf.enabled') : t('wf.disabled')}</span></span>}
        subtitle={w.description || undefined}
        actions={<>
          {canRun && <Button variant="primary" onClick={() => setConfirm('run')}><Play className="size-4 rtl:-scale-x-100" />{t('wf.runNow')}</Button>}
          {can('automation.manage') && <Link to={`/automation/${id}/edit`}><Button>{t('common.edit')}</Button></Link>}
          {can('automation.manage') && <Button variant="danger" onClick={() => setConfirm('delete')}><Trash2 className="size-4" />{t('common.delete')}</Button>}
        </>} />
      {err && <div className="mb-4"><ErrorBanner error={err} /></div>}
      <div className="grid gap-4 lg:grid-cols-3">
        <Card title={t('common.details')}>
          <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-2 text-sm">
            <dt className="text-muted">{t('wf.trigger')}</dt><dd>{triggerLabel(t, loc, w.trigger)}</dd>
            {w.trigger.type === 'schedule' && <><dt className="text-muted">{t('wf.nextFire')}</dt><dd>{w.enabled ? fmtDateTime(loc, w.next_fire_at) : '—'}</dd></>}
            {w.trigger.type === 'condition' && <><dt className="text-muted">{t('wf.cond.repeat')}</dt><dd>{fmtDuration(loc, (w.trigger.repeat_minutes ?? 60) * 60)}</dd></>}
            {(w.trigger.type === 'incident' || w.trigger.type === 'condition') && <><dt className="text-muted">{t('wf.scope')}</dt><dd>{[...(w.trigger.tags ?? []).map((x) => '#' + x), ...(w.trigger.monitor_ids?.length ? [t('wf.nMonitors', { n: w.trigger.monitor_ids.length })] : [])].join(' ') || t('wf.allMonitors')}</dd></>}
            <dt className="text-muted">{t('wf.operatorRun')}</dt><dd>{w.operator_can_run ? t('common.yes') : t('common.no')}</dd>
            <dt className="text-muted">{t('wf.secrets')}</dt><dd>{w.secret_keys.length ? <Ltr>{w.secret_keys.join(', ')}</Ltr> : '—'}</dd>
            <dt className="text-muted">{t('wf.version')}</dt><dd className="tabular">{w.version}</dd>
            <dt className="text-muted">{t('wf.updated')}</dt><dd>{fmtDateTime(loc, w.updated_at)}</dd>
          </dl>
        </Card>
        <Card title={t('wf.steps')} className="lg:col-span-2">
          <ol className="flex flex-col gap-2">
            {w.steps.map((s, i) => (
              <li key={i} className="flex items-start gap-3 rounded-lg border border-line p-3 text-sm">
                <span className="mt-0.5 inline-flex size-6 shrink-0 items-center justify-center rounded-full bg-accent-soft text-xs font-semibold text-accent">{i + 1}</span>
                <div className="min-w-0">
                  <div className="font-medium">{s.name} <span className="text-xs font-normal text-muted">· {t(`wf.stepType.${s.type}` as Key)}</span></div>
                  <div className="mt-0.5 break-all text-muted"><StepSummary s={s} /></div>
                </div>
              </li>
            ))}
          </ol>
        </Card>
      </div>
      <Card title={t('wf.recentRuns')} className="mt-4" bodyClass="p-0"><RunsTable runs={data!.runs} /></Card>
      <Dialog open={confirm === 'run'} onClose={() => setConfirm(null)} title={t('wf.runConfirmTitle')}
        footer={<><Button onClick={() => setConfirm(null)}>{t('common.cancel')}</Button><Button variant="primary" loading={busy} onClick={run} data-testid="confirm-run">{t('wf.runNow')}</Button></>}>
        <p className="text-sm">{t('wf.runConfirm', { name: w.name, n: w.steps.length })}</p>
        {usesIncident && <div className="mt-3"><Field label={t('wf.incidentId')} optional htmlFor="inc-id" help={t('wf.incidentIdHelp')}><Input id="inc-id" dir="ltr" value={incidentId} onChange={(e) => setIncidentId(e.target.value)} /></Field></div>}
        <p className="mt-3 text-xs text-muted">{t('wf.runAs', { user: me?.user.username ?? '' })}</p>
      </Dialog>
      <ConfirmDialog open={confirm === 'delete'} onClose={() => setConfirm(null)} onConfirm={remove} busy={busy} title={t('wf.deleteTitle')} message={t('wf.deleteConfirm', { name: w.name })} confirmLabel={t('common.delete')} />
    </>
  )
}

export function RunDetailPage() {
  const { id } = useParams()
  const { t, loc } = usePrefs()
  const { can } = useSession()
  const { data, error, loading, reload } = useApi<{ run: WorkflowRun; actions: WorkflowAction[] }>(`/workflow-runs/${id}`, 2000)
  const [cancelOpen, setCancelOpen] = useState(false)
  const [rec, setRec] = useState<WorkflowAction | null>(null)
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState<string | null>(null)
  if (loading && !data) return <Spinner />
  if (error && !data) return <ErrorBanner error={error} onRetry={reload} />
  const r = data!.run
  const active = ['pending', 'running', 'waiting', 'unknown_outcome'].includes(r.status)
  const cancel = async () => {
    setBusy(true); setErr(null)
    try { await post(`/workflow-runs/${id}/cancel`); setCancelOpen(false); await reload() } catch (e) { setErr((e as ApiError).message); setCancelOpen(false) } finally { setBusy(false) }
  }
  return (
    <>
      <Link to={r.context?.dr_plan_id ? `/dr/${r.context.dr_plan_id}` : `/automation/${r.workflow_id}`} className="mb-3 inline-flex items-center gap-1 text-sm text-muted hover:text-fg"><ArrowLeft className="size-4 rtl:-scale-x-100" />{r.workflow_name ?? t('wf.workflow')}</Link>
      <PageHeader title={<span className="flex flex-wrap items-center gap-2">{t('wf.run')} <Ltr>{r.id.slice(0, 8)}</Ltr><RunBadge status={r.status} /></span>}
        subtitle={`${t(`wf.triggerType.${r.trigger_type}` as Key)} · ${fmtDateTime(loc, r.created_at)}`}
        actions={active && (can('automation.manage') || can('automation.run')) && <Button variant="danger" onClick={() => setCancelOpen(true)}>{t('wf.cancelRun')}</Button>} />
      {err && <div className="mb-4"><ErrorBanner error={err} /></div>}
      {r.status === 'unknown_outcome' && (
        <div role="alert" className="mb-4 flex items-start gap-3 rounded-lg border border-warn/40 bg-warn-soft p-3 text-sm text-warn" data-testid="unknown-outcome">
          <CircleAlert className="mt-0.5 size-4 shrink-0" aria-hidden />
          <div>{t('wf.unknownExplain')}</div>
        </div>
      )}
      {r.error && r.status !== 'unknown_outcome' && <div className="mb-4 rounded-lg border border-line bg-surface-2 p-3 text-sm">{r.error}</div>}
      <div className="grid gap-4 lg:grid-cols-3">
        <Card title={t('common.details')}>
          <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-2 text-sm">
            <dt className="text-muted">{t('wf.started')}</dt><dd>{fmtDateTime(loc, r.started_at)}</dd>
            <dt className="text-muted">{t('wf.finished')}</dt><dd>{fmtDateTime(loc, r.finished_at)}</dd>
            {r.next_at && <><dt className="text-muted">{t('wf.resumesAt')}</dt><dd>{fmtDateTime(loc, r.next_at)}</dd></>}
            <dt className="text-muted">{t('wf.version')}</dt><dd className="tabular">{r.workflow_version}</dd>
            {Object.entries(r.context).filter(([k]) => k !== 'workflow_name' && k !== 'trigger').map(([k, v]) => (
              <div key={k} className="contents"><dt className="text-muted"><Ltr>{k}</Ltr></dt><dd className="break-all">{k === 'incident_id' ? <Link className="hover:underline" to={`/incidents/${v}`}><Ltr>{v.slice(0, 8)}</Ltr></Link> : v}</dd></div>
            ))}
          </dl>
        </Card>
        <Card title={t('wf.steps')} className="lg:col-span-2">
          <ol className="flex flex-col gap-3" data-testid="run-steps">
            {r.steps.map((s, i) => {
              const acts = data!.actions.filter((a) => a.step_index === i)
              const state = i < r.current_step ? 'done' : i === r.current_step && active ? 'current' : 'todo'
              return (
                <li key={i} className={`rounded-lg border p-3 ${state === 'current' ? 'border-accent' : 'border-line'}`}>
                  <div className="flex flex-wrap items-center gap-2 text-sm">
                    <span className={`inline-flex size-6 items-center justify-center rounded-full text-xs font-semibold ${state === 'done' ? 'bg-up-soft text-up' : state === 'current' ? 'bg-accent text-accent-fg' : 'bg-surface-2 text-muted'}`}>{i + 1}</span>
                    <span className="font-medium">{s.name}</span>
                    <span className="text-xs text-muted">{t(`wf.stepType.${s.type}` as Key)}</span>
                    {s.type === 'wait' && state === 'current' && r.next_at && <span className="text-xs text-info">{t('wf.waitingUntil', { at: fmtDateTime(loc, r.next_at) })}</span>}
                  </div>
                  {acts.map((a) => (
                    <div key={a.id} className="mt-2 rounded-md bg-surface-2 p-2.5 text-xs" data-testid="action">
                      <div className="flex flex-wrap items-center gap-2">
                        <RunBadge status={a.status} />
                        <span className="text-muted">{t('wf.attempt', { n: a.attempt })}</span>
                        {a.exit_code !== undefined && a.exit_code !== null && <span className="text-muted">{s.type === 'http' ? 'HTTP' : t('wf.exitCode')} <span className="tabular">{a.exit_code}</span></span>}
                        <span className="text-muted" title={t('wf.fenceHelp')}>{t('wf.fence')} <Ltr>{a.fence_term}/{a.fence_leader.slice(0, 8)}</Ltr></span>
                        {a.reconciled_by && <span className="text-muted">{t('wf.reconciledBy', { who: a.reconciled_by })}</span>}
                        {a.status === 'unknown_outcome' && can('automation.manage') && <Button size="sm" variant="primary" className="ms-auto" onClick={() => setRec(a)} data-testid="reconcile">{t('wf.reconcile')}</Button>}
                      </div>
                      {a.error && <div className="mt-1.5 text-down">{a.error}</div>}
                      {a.output && <pre dir="ltr" className="mt-1.5 max-h-64 overflow-auto whitespace-pre-wrap break-all rounded bg-bg p-2 text-start font-mono text-[11px] leading-relaxed">{a.output}</pre>}
                    </div>
                  ))}
                </li>
              )
            })}
          </ol>
        </Card>
      </div>
      <ConfirmDialog open={cancelOpen} onClose={() => setCancelOpen(false)} onConfirm={cancel} busy={busy} title={t('wf.cancelRun')} message={t('wf.cancelConfirm')} confirmLabel={t('wf.cancelRun')} />
      {rec && <ReconcileDialog action={rec} onClose={() => setRec(null)} onDone={() => { setRec(null); void reload() }} />}
    </>
  )
}

function ReconcileDialog({ action, onClose, onDone }: { action: WorkflowAction; onClose: () => void; onDone: () => void }) {
  const { t } = usePrefs()
  const [result, setResult] = useState<'succeeded' | 'failed' | ''>('')
  const [note, setNote] = useState('')
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState<ApiError | null>(null)
  const submit = async () => {
    setBusy(true); setErr(null)
    try { await post(`/workflow-actions/${action.id}/reconcile`, { result, note }); onDone() } catch (e) { setErr(e as ApiError) } finally { setBusy(false) }
  }
  return (
    <Dialog open onClose={onClose} title={t('wf.reconcileTitle')}
      footer={<><Button onClick={onClose}>{t('common.cancel')}</Button><Button variant="primary" loading={busy} disabled={!result || !note.trim()} onClick={submit} data-testid="reconcile-submit">{t('wf.reconcile')}</Button></>}>
      <p className="text-sm">{t('wf.reconcileHelp')}</p>
      {err && <div className="mt-3"><ErrorBanner error={err.message} /></div>}
      <fieldset className="mt-4 flex flex-col gap-2 text-sm">
        <legend className="mb-1 font-medium">{t('wf.reconcileResult')}</legend>
        <label className="flex items-center gap-2"><input type="radio" name="res" checked={result === 'succeeded'} onChange={() => setResult('succeeded')} className="accent-[var(--accent)]" />{t('wf.reconcileSucceeded')}</label>
        <label className="flex items-center gap-2"><input type="radio" name="res" checked={result === 'failed'} onChange={() => setResult('failed')} className="accent-[var(--accent)]" />{t('wf.reconcileFailed')}</label>
      </fieldset>
      <div className="mt-4"><Field label={t('wf.reconcileNote')} htmlFor="rec-note" error={err?.fields.note}><Textarea id="rec-note" value={note} onChange={(e) => setNote(e.target.value)} rows={3} /></Field></div>
    </Dialog>
  )
}

// ---- editor ----

const allIncidentStatuses: IncidentStatus[] = ['SUSPECTED', 'CONFIRMED', 'ACKNOWLEDGED', 'RECOVERY_PENDING', 'RECOVERING', 'FAILED_OVER', 'RESOLVED', 'MANUAL_INTERVENTION']
const stepStatuses: IncidentStatus[] = ['ACKNOWLEDGED', 'RECOVERY_PENDING', 'RECOVERING', 'FAILED_OVER', 'RESOLVED', 'MANUAL_INTERVENTION']

function newStep(type: WorkflowStep['type'], n: number): WorkflowStep {
  switch (type) {
    case 'script': return { name: `Step ${n}`, type, language: 'bash', script: '#!/usr/bin/env bash\nset -euo pipefail\necho "Incident: ${INTRABIN_INCIDENT_ID:-none}"\n', timeout_seconds: 60, on_failure: 'stop', retries: 0, env: {} }
    case 'http': return { name: `Step ${n}`, type, method: 'POST', url: 'https://', headers: {}, body: '', expected_status: ['2xx'], timeout_seconds: 30, on_failure: 'stop', retries: 0 }
    case 'wait': return { name: `Step ${n}`, type, seconds: 60 }
    case 'incident_status': return { name: `Step ${n}`, type, status: 'RECOVERING', note: '', on_failure: 'stop' }
    case 'dns': return { name: `Step ${n}`, type, provider_id: '', zone_name: '', dns_action: 'upsert', record_type: 'A', record_name: '', content: '', ttl: 60, proxied: false, on_failure: 'stop', retries: 0 }
  }
}

type KV = { k: string; v: string }
const toKV = (m?: Record<string, string>): KV[] => Object.entries(m ?? {}).map(([k, v]) => ({ k, v }))
const fromKV = (l: KV[]) => Object.fromEntries(l.filter((x) => x.k.trim()).map((x) => [x.k.trim(), x.v]))

function KVEditor({ value, onChange, keyLabel, valueLabel, secret, placeholderStored }: { value: KV[]; onChange: (v: KV[]) => void; keyLabel: string; valueLabel: string; secret?: boolean; placeholderStored?: string[] }) {
  const { t } = usePrefs()
  return (
    <div className="flex flex-col gap-2">
      {value.map((row, i) => (
        <div key={i} className="flex gap-2">
          <Input aria-label={keyLabel} placeholder={keyLabel} dir="ltr" value={row.k} onChange={(e) => onChange(value.map((x, j) => j === i ? { ...x, k: e.target.value } : x))} className="font-mono" />
          <Input aria-label={valueLabel} dir="ltr" type={secret ? 'password' : 'text'} autoComplete="off"
            placeholder={secret && placeholderStored?.includes(row.k) ? t('wf.secretStored') : valueLabel}
            value={row.v} onChange={(e) => onChange(value.map((x, j) => j === i ? { ...x, v: e.target.value } : x))} />
          <Button variant="ghost" aria-label={t('common.delete')} onClick={() => onChange(value.filter((_, j) => j !== i))}><Trash2 className="size-4" /></Button>
        </div>
      ))}
      <div><Button size="sm" onClick={() => onChange([...value, { k: '', v: '' }])}><Plus className="size-4" />{t('wf.addRow')}</Button></div>
    </div>
  )
}

interface StepDraft extends WorkflowStep { _env: KV[]; _headers: KV[]; _expected: string }

function toDraft(s: WorkflowStep): StepDraft {
  return { ...s, _env: toKV(s.env), _headers: toKV(s.headers), _expected: (s.expected_status ?? ['2xx']).join(', ') }
}

function fromDraft(d: StepDraft): WorkflowStep {
  const { _env, _headers, _expected, ...s } = d
  if (s.type === 'script') s.env = fromKV(_env)
  if (s.type === 'http') {
    s.headers = fromKV(_headers)
    s.expected_status = _expected.split(',').map((x) => x.trim()).filter(Boolean)
  }
  return s
}

export function WorkflowFormPage() {
  const { id } = useParams()
  const editing = !!id
  const { t } = usePrefs()
  const navigate = useNavigate()
  const existing = useApi<{ workflow: Workflow }>(editing ? `/workflows/${id}` : null)
  const monitors = useApi<{ monitors: (Monitor & { state?: unknown })[] }>('/monitors')
  const dnsProviders = useApi<{ providers: DNSProvider[] }>('/dns/providers')
  if (editing && existing.loading && !existing.data) return <Spinner />
  if (editing && existing.error) return <ErrorBanner error={existing.error} onRetry={existing.reload} />
  return <WorkflowForm key={existing.data?.workflow.id ?? 'new'} initial={existing.data?.workflow} monitors={monitors.data?.monitors ?? []} dnsProviders={dnsProviders.data?.providers ?? []} onSaved={(w) => navigate(`/automation/${w.id}`)} onCancel={() => navigate(editing ? `/automation/${id}` : '/automation')} title={editing ? t('wf.edit') : t('wf.new')} />
}

function WorkflowForm({ initial, monitors, dnsProviders, onSaved, onCancel, title }: { initial?: Workflow; monitors: Monitor[]; dnsProviders: DNSProvider[]; onSaved: (w: Workflow) => void; onCancel: () => void; title: string }) {
  const { t, loc } = usePrefs()
  const [name, setName] = useState(initial?.name ?? '')
  const [description, setDescription] = useState(initial?.description ?? '')
  const [enabled, setEnabled] = useState(initial?.enabled ?? false)
  const [opRun, setOpRun] = useState(initial?.operator_can_run ?? false)
  const [trigger, setTrigger] = useState<WorkflowTrigger>(initial?.trigger ?? { type: 'manual' })
  const [tags, setTags] = useState((initial?.trigger.tags ?? []).join(', '))
  const [steps, setSteps] = useState<StepDraft[]>((initial?.steps ?? [newStep('script', 1)]).map(toDraft))
  const [secrets, setSecrets] = useState<KV[]>((initial?.secret_keys ?? []).map((k) => ({ k, v: '' })))
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState<ApiError | null>(null)
  const f = err?.fields ?? {}
  const stored = initial?.secret_keys ?? []
  const upd = (i: number, p: Partial<StepDraft>) => setSteps(steps.map((s, j) => j === i ? { ...s, ...p } : s))
  const move = (i: number, d: number) => { const n = [...steps]; const [x] = n.splice(i, 1); n.splice(i + d, 0, x); setSteps(n) }
  const autoWarn = enabled && trigger.type !== 'manual'
  const save = async () => {
    setBusy(true); setErr(null)
    const tr: WorkflowTrigger = trigger.type === 'incident' || trigger.type === 'condition' ? { ...trigger, tags: tags.split(',').map((x) => x.trim()).filter(Boolean) } : trigger
    const body = { name, description, enabled, operator_can_run: opRun, trigger: tr, steps: steps.map(fromDraft), secrets: fromKV(secrets) }
    try {
      const r = initial ? await put<{ workflow: Workflow }>(`/workflows/${initial.id}`, body) : await post<{ workflow: Workflow }>('/workflows', body)
      onSaved(r.workflow)
    } catch (e) { setErr(e as ApiError); window.scrollTo({ top: 0, behavior: 'smooth' }) } finally { setBusy(false) }
  }
  const monitorIds = trigger.monitor_ids ?? []
  const sortedMonitors = useMemo(() => [...monitors].sort((a, b) => a.name.localeCompare(b.name)), [monitors])
  return (
    <>
      <PageHeader title={title} />
      {err && <div className="mb-4"><ErrorBanner error={err.message} /></div>}
      <div className="flex flex-col gap-4">
        <Card title={t('common.details')}>
          <div className="grid gap-4 md:grid-cols-2">
            <Field label={t('common.name')} htmlFor="wf-name" error={f.name}><Input id="wf-name" value={name} onChange={(e) => setName(e.target.value)} invalid={!!f.name} /></Field>
            <Field label={t('common.description')} htmlFor="wf-desc" optional error={f.description}><Input id="wf-desc" value={description} onChange={(e) => setDescription(e.target.value)} /></Field>
          </div>
          <div className="mt-4 flex flex-col gap-2">
            <Checkbox id="wf-enabled" label={t('wf.enableAuto')} checked={enabled} onChange={setEnabled} />
            <Checkbox id="wf-oprun" label={t('wf.operatorCanRun')} checked={opRun} onChange={setOpRun} />
            {autoWarn && <p className="rounded-lg border border-warn/40 bg-warn-soft p-2.5 text-xs text-warn" role="note">{t('wf.autoWarning')}</p>}
          </div>
        </Card>

        <Card title={t('wf.trigger')}>
          <div className="grid gap-4 md:grid-cols-2">
            <Field label={t('wf.triggerTypeLabel')} htmlFor="wf-trigger" error={f['trigger.type']}>
              <Select id="wf-trigger" value={trigger.type} onChange={(e) => {
                const ty = e.target.value as WorkflowTrigger['type']
                setTrigger({ type: ty, interval_seconds: ty === 'schedule' ? 3600 : undefined, on_statuses: ty === 'incident' ? ['CONFIRMED'] : undefined,
                  ...(ty === 'condition' ? { metric: 'latency_ms', op: 'gt', threshold: 1000, repeat_minutes: 60 } : {}) })
              }}>
                <option value="manual">{t('wf.triggerType.manual')}</option>
                <option value="incident">{t('wf.triggerType.incident')}</option>
                <option value="condition">{t('wf.triggerType.condition')}</option>
                <option value="schedule">{t('wf.triggerType.schedule')}</option>
              </Select>
            </Field>
            {trigger.type === 'condition' && (
              <Field label={t('wf.cond.repeatMinutes')} htmlFor="wf-repeat" help={t('wf.cond.repeatHelp')} error={f['trigger.repeat_minutes']}>
                <Input id="wf-repeat" type="number" min={1} value={trigger.repeat_minutes ?? 60} onChange={(e) => setTrigger({ ...trigger, repeat_minutes: Number(e.target.value) })} />
              </Field>
            )}
            {trigger.type === 'schedule' && (
              <Field label={t('wf.intervalMinutes')} htmlFor="wf-interval" error={f['trigger.interval_seconds']}>
                <Input id="wf-interval" type="number" min={1} value={Math.round((trigger.interval_seconds ?? 3600) / 60)} onChange={(e) => setTrigger({ ...trigger, interval_seconds: Number(e.target.value) * 60 })} />
              </Field>
            )}
          </div>
          {trigger.type === 'condition' && (
            <div className="mt-4 grid gap-4 md:grid-cols-3" data-testid="condition-editor">
              <Field label={t('wf.cond.metric')} htmlFor="wf-metric" error={f['trigger.metric']}>
                <Select id="wf-metric" value={trigger.metric ?? 'latency_ms'} onChange={(e) => {
                  const metric = e.target.value as NonNullable<WorkflowTrigger['metric']>
                  const defaults: Record<string, Partial<WorkflowTrigger>> = { latency_ms: { op: 'gt', threshold: 1000 }, status_code: { op: 'ge', threshold: 500 }, uptime_24h: { op: 'lt', threshold: 99 }, cert_days: { op: 'lt', threshold: 14 }, status: { op: 'eq', status_value: 'DOWN' } }
                  setTrigger({ ...trigger, metric, ...defaults[metric] })
                }}>
                  {(['latency_ms', 'status_code', 'uptime_24h', 'cert_days', 'status'] as const).map((m) => <option key={m} value={m}>{t(`wf.cond.metric.${m}` as Key)}</option>)}
                </Select>
              </Field>
              <Field label={t('wf.cond.op')} htmlFor="wf-op" error={f['trigger.op']}>
                <Select id="wf-op" value={trigger.op ?? 'gt'} onChange={(e) => setTrigger({ ...trigger, op: e.target.value as WorkflowTrigger['op'] })}>
                  {(trigger.metric === 'status' ? ['eq', 'ne'] : ['gt', 'ge', 'lt', 'le', 'eq', 'ne']).map((o) => <option key={o} value={o}>{t(`wf.cond.op.${o}` as Key)}</option>)}
                </Select>
              </Field>
              {trigger.metric === 'status' ? (
                <Field label={t('wf.cond.value')} htmlFor="wf-sv" error={f['trigger.status_value']}>
                  <Select id="wf-sv" value={trigger.status_value ?? 'DOWN'} onChange={(e) => setTrigger({ ...trigger, status_value: e.target.value })}>
                    {['UP', 'DEGRADED', 'SUSPECTED_DOWN', 'DOWN', 'UNKNOWN', 'MAINTENANCE'].map((x) => <option key={x} value={x}>{t(`status.${x}` as Key)}</option>)}
                  </Select>
                </Field>
              ) : (
                <Field label={`${t('wf.cond.value')} (${t(`wf.cond.unit.${trigger.metric ?? 'latency_ms'}` as Key)})`} htmlFor="wf-thr" error={f['trigger.threshold']}>
                  <Input id="wf-thr" type="number" step="any" value={trigger.threshold ?? 0} onChange={(e) => setTrigger({ ...trigger, threshold: Number(e.target.value) })} />
                </Field>
              )}
              <p className="text-xs text-muted md:col-span-3" data-testid="condition-summary">{t('wf.cond.summary', { c: conditionText(t, loc, trigger) })}</p>
            </div>
          )}
          {(trigger.type === 'incident' || trigger.type === 'condition') && (
            <div className="mt-4 grid gap-4 md:grid-cols-2">
              {trigger.type === 'incident' ? <fieldset>
                <legend className="mb-2 text-sm font-medium">{t('wf.onStatuses')}</legend>
                <div className="grid grid-cols-2 gap-1.5">
                  {allIncidentStatuses.map((s) => (
                    <Checkbox key={s} label={t(`incident.${s}` as Key)} checked={(trigger.on_statuses ?? []).includes(s)}
                      onChange={(v) => setTrigger({ ...trigger, on_statuses: v ? [...(trigger.on_statuses ?? []), s] : (trigger.on_statuses ?? []).filter((x) => x !== s) })} />
                  ))}
                </div>
                {f['trigger.on_statuses'] && <p className="mt-1 text-xs text-down">{f['trigger.on_statuses']}</p>}
              </fieldset> : <p className="text-sm text-muted">{t('wf.cond.scopeHelp')}</p>}
              <div className="flex flex-col gap-3">
                <Field label={t('wf.tags')} htmlFor="wf-tags" optional help={t('wf.scopeHelp')}><Input id="wf-tags" value={tags} onChange={(e) => setTags(e.target.value)} placeholder="prod, api" /></Field>
                <fieldset>
                  <legend className="mb-1 text-sm font-medium">{t('wf.monitors')} <span className="text-xs font-normal text-muted">({t('common.optional')})</span></legend>
                  <div className="max-h-40 overflow-y-auto rounded-lg border border-line p-2">
                    {sortedMonitors.length === 0 ? <p className="text-xs text-muted">{t('wf.noMonitors')}</p> : sortedMonitors.map((m) => (
                      <div key={m.id}><Checkbox label={m.name} checked={monitorIds.includes(m.id)} onChange={(v) => setTrigger({ ...trigger, monitor_ids: v ? [...monitorIds, m.id] : monitorIds.filter((x) => x !== m.id) })} /></div>
                    ))}
                  </div>
                </fieldset>
              </div>
            </div>
          )}
        </Card>

        <Card title={t('wf.steps')} actions={
          <Select aria-label={t('wf.addStep')} value="" onChange={(e) => { if (e.target.value) setSteps([...steps, toDraft(newStep(e.target.value as WorkflowStep['type'], steps.length + 1))]) }} className="h-8 w-auto" data-testid="add-step">
            <option value="">{t('wf.addStep')}</option>
            {(['script', 'http', 'wait', 'incident_status', 'dns'] as const).map((x) => <option key={x} value={x}>{t(`wf.stepType.${x}` as Key)}</option>)}
          </Select>}>
          {f.steps && <p className="mb-3 text-xs text-down">{f.steps}</p>}
          <ol className="flex flex-col gap-4">
            {steps.map((s, i) => {
              const k = (n: string) => f[`steps.${i}.${n}`]
              const sid = `step-${i}`
              return (
                <li key={i} className="rounded-lg border border-line p-4" data-testid="step-editor">
                  <div className="mb-3 flex flex-wrap items-center gap-2">
                    <span className="inline-flex size-6 items-center justify-center rounded-full bg-accent-soft text-xs font-semibold text-accent">{i + 1}</span>
                    <span className="text-sm font-medium">{t(`wf.stepType.${s.type}` as Key)}</span>
                    <div className="ms-auto flex gap-1">
                      <Button size="sm" variant="ghost" aria-label={t('wf.moveUp')} disabled={i === 0} onClick={() => move(i, -1)}><ArrowUp className="size-4" /></Button>
                      <Button size="sm" variant="ghost" aria-label={t('wf.moveDown')} disabled={i === steps.length - 1} onClick={() => move(i, 1)}><ArrowDown className="size-4" /></Button>
                      <Button size="sm" variant="ghost" aria-label={t('common.delete')} disabled={steps.length === 1} onClick={() => setSteps(steps.filter((_, j) => j !== i))}><Trash2 className="size-4" /></Button>
                    </div>
                  </div>
                  <div className="grid gap-3 md:grid-cols-3">
                    <Field label={t('common.name')} htmlFor={`${sid}-name`} error={k('name')}><Input id={`${sid}-name`} value={s.name} onChange={(e) => upd(i, { name: e.target.value })} /></Field>
                    {s.type !== 'wait' && (
                      <Field label={t('wf.onFailure')} htmlFor={`${sid}-onf`}>
                        <Select id={`${sid}-onf`} value={s.on_failure ?? 'stop'} onChange={(e) => upd(i, { on_failure: e.target.value as 'stop' | 'continue' })}>
                          <option value="stop">{t('wf.onFailure.stop')}</option><option value="continue">{t('wf.onFailure.continue')}</option>
                        </Select>
                      </Field>
                    )}
                    {s.type === 'dns' && (
                      <Field label={t('wf.retries')} htmlFor={`${sid}-rt`} error={k('retries')}><Input id={`${sid}-rt`} type="number" min={0} max={5} value={s.retries ?? 0} onChange={(e) => upd(i, { retries: Number(e.target.value) })} /></Field>
                    )}
                    {(s.type === 'script' || s.type === 'http') && (
                      <div className="grid grid-cols-2 gap-3">
                        <Field label={t('wf.timeout')} htmlFor={`${sid}-to`} error={k('timeout_seconds')}><Input id={`${sid}-to`} type="number" min={1} value={s.timeout_seconds ?? ''} onChange={(e) => upd(i, { timeout_seconds: Number(e.target.value) })} /></Field>
                        <Field label={t('wf.retries')} htmlFor={`${sid}-rt`} error={k('retries')}><Input id={`${sid}-rt`} type="number" min={0} max={5} value={s.retries ?? 0} onChange={(e) => upd(i, { retries: Number(e.target.value) })} /></Field>
                      </div>
                    )}
                  </div>
                  {s.type === 'script' && (
                    <div className="mt-3 flex flex-col gap-3">
                      <Field label={t('wf.language')} htmlFor={`${sid}-lang`} error={k('language')}>
                        <Select id={`${sid}-lang`} value={s.language} onChange={(e) => upd(i, { language: e.target.value as 'bash' | 'python' })} className="md:w-48"><option value="bash">Bash</option><option value="python">Python 3</option></Select>
                      </Field>
                      <Field label={t('wf.script')} htmlFor={`${sid}-script`} error={k('script')} help={t('wf.scriptHelp')}>
                        <Textarea id={`${sid}-script`} dir="ltr" spellCheck={false} rows={8} className="text-start font-mono text-xs" value={s.script} onChange={(e) => upd(i, { script: e.target.value })} />
                      </Field>
                      <Field label={t('wf.env')} optional error={k('env')}><KVEditor value={s._env} onChange={(v) => upd(i, { _env: v })} keyLabel={t('wf.envName')} valueLabel={t('wf.value')} /></Field>
                    </div>
                  )}
                  {s.type === 'http' && (
                    <div className="mt-3 flex flex-col gap-3">
                      <div className="grid gap-3 md:grid-cols-[8rem_1fr]">
                        <Field label={t('wf.method')} htmlFor={`${sid}-m`} error={k('method')}>
                          <Select id={`${sid}-m`} value={s.method} onChange={(e) => upd(i, { method: e.target.value })}>{['POST', 'GET', 'PUT', 'PATCH', 'DELETE', 'HEAD'].map((m) => <option key={m}>{m}</option>)}</Select>
                        </Field>
                        <Field label="URL" htmlFor={`${sid}-url`} error={k('url')} help={t('wf.urlHelp')}><Input id={`${sid}-url`} dir="ltr" value={s.url} onChange={(e) => upd(i, { url: e.target.value })} invalid={!!k('url')} /></Field>
                      </div>
                      <Field label={t('wf.headers')} optional error={k('headers')}><KVEditor value={s._headers} onChange={(v) => upd(i, { _headers: v })} keyLabel={t('wf.headerName')} valueLabel={t('wf.value')} /></Field>
                      <Field label={t('wf.body')} htmlFor={`${sid}-body`} optional error={k('body')}><Textarea id={`${sid}-body`} dir="ltr" rows={3} className="text-start font-mono text-xs" value={s.body} onChange={(e) => upd(i, { body: e.target.value })} /></Field>
                      <div className="grid gap-3 md:grid-cols-2">
                        <Field label={t('wf.expectedStatus')} htmlFor={`${sid}-exp`} error={k('expected_status')}><Input id={`${sid}-exp`} dir="ltr" value={s._expected} onChange={(e) => upd(i, { _expected: e.target.value })} /></Field>
                        <div className="flex flex-col justify-end gap-1">
                          <Checkbox label={t('wf.allowPrivate')} checked={!!s.allow_private_network} onChange={(v) => upd(i, { allow_private_network: v })} />
                          <p className="text-xs text-muted">{t('wf.allowPrivateHelp')}</p>
                        </div>
                      </div>
                    </div>
                  )}
                  {s.type === 'wait' && (
                    <div className="mt-3 md:w-64"><Field label={t('wf.waitSeconds')} htmlFor={`${sid}-sec`} error={k('seconds')}><Input id={`${sid}-sec`} type="number" min={1} value={s.seconds ?? ''} onChange={(e) => upd(i, { seconds: Number(e.target.value) })} /></Field></div>
                  )}
                  {s.type === 'dns' && (
                    <div className="mt-3 flex flex-col gap-3">
                      {dnsProviders.length === 0 && <p className="rounded-lg border border-warn/40 bg-warn-soft p-2.5 text-xs text-warn">{t('wf.dns.noProviders')} <Link to="/dns" className="underline">{t('dns.title')}</Link></p>}
                      <div className="grid gap-3 md:grid-cols-3">
                        <Field label={t('wf.dns.integration')} htmlFor={`${sid}-dp`} error={k('provider_id')}>
                          <Select id={`${sid}-dp`} value={s.provider_id ?? ''} onChange={(e) => upd(i, { provider_id: e.target.value })}>
                            <option value="">{t('wf.dns.choose')}</option>
                            {dnsProviders.map((p) => <option key={p.id} value={p.id}>{p.name}</option>)}
                          </Select>
                        </Field>
                        <Field label={t('dns.zone')} htmlFor={`${sid}-zone`} error={k('zone_name')}><Input id={`${sid}-zone`} dir="ltr" placeholder="example.com" value={s.zone_name ?? ''} onChange={(e) => upd(i, { zone_name: e.target.value })} /></Field>
                        <Field label={t('wf.dns.action')} htmlFor={`${sid}-da`} error={k('dns_action')}>
                          <Select id={`${sid}-da`} value={s.dns_action} onChange={(e) => upd(i, { dns_action: e.target.value as 'upsert' | 'delete' })}>
                            <option value="upsert">{t('wf.dns.upsert')}</option><option value="delete">{t('wf.dns.delete')}</option>
                          </Select>
                        </Field>
                      </div>
                      <div className="grid gap-3 md:grid-cols-[8rem_1fr_1fr_7rem]">
                        <Field label={t('dns.recordType')} htmlFor={`${sid}-rtp`} error={k('record_type')}>
                          <Select id={`${sid}-rtp`} value={s.record_type} onChange={(e) => upd(i, { record_type: e.target.value })}>{['A', 'AAAA', 'CNAME', 'TXT'].map((x) => <option key={x}>{x}</option>)}</Select>
                        </Field>
                        <Field label={t('wf.dns.recordName')} htmlFor={`${sid}-rn`} error={k('record_name')}><Input id={`${sid}-rn`} dir="ltr" placeholder="app.example.com" value={s.record_name ?? ''} onChange={(e) => upd(i, { record_name: e.target.value })} /></Field>
                        {s.dns_action !== 'delete' && <>
                          <Field label={t('dns.content')} htmlFor={`${sid}-rc`} error={k('content')}><Input id={`${sid}-rc`} dir="ltr" placeholder="{{dr_ip}}" value={s.content ?? ''} onChange={(e) => upd(i, { content: e.target.value })} /></Field>
                          <Field label="TTL" htmlFor={`${sid}-ttl`} error={k('ttl')}><Input id={`${sid}-ttl`} type="number" min={1} value={s.ttl ?? 60} onChange={(e) => upd(i, { ttl: Number(e.target.value) })} /></Field>
                        </>}
                      </div>
                      {s.dns_action !== 'delete' && dnsProviders.find((p) => p.id === s.provider_id)?.type === 'cloudflare' && s.record_type !== 'TXT' && (
                        <Checkbox label={t('dns.proxied')} checked={!!s.proxied} onChange={(v) => upd(i, { proxied: v })} />
                      )}
                      <p className="text-xs text-muted">{t('wf.dns.help')}</p>
                    </div>
                  )}
                  {s.type === 'incident_status' && (
                    <div className="mt-3 grid gap-3 md:grid-cols-2">
                      <Field label={t('wf.newStatus')} htmlFor={`${sid}-st`} error={k('status')}>
                        <Select id={`${sid}-st`} value={s.status} onChange={(e) => upd(i, { status: e.target.value as IncidentStatus })}>{stepStatuses.map((x) => <option key={x} value={x}>{t(`incident.${x}` as Key)}</option>)}</Select>
                      </Field>
                      <Field label={t('wf.note')} htmlFor={`${sid}-note`} optional error={k('note')}><Input id={`${sid}-note`} value={s.note ?? ''} onChange={(e) => upd(i, { note: e.target.value })} /></Field>
                    </div>
                  )}
                </li>
              )
            })}
          </ol>
        </Card>

        <Card title={t('wf.secrets')}>
          <p className="mb-3 text-xs text-muted">{t('wf.secretsHelp')}</p>
          <KVEditor value={secrets} onChange={setSecrets} keyLabel={t('wf.envName')} valueLabel={t('wf.secretValue')} secret placeholderStored={stored} />
          {f.secrets && <p className="mt-2 text-xs text-down">{f.secrets}</p>}
        </Card>

        <div className="flex justify-end gap-2">
          <Button onClick={onCancel}>{t('common.cancel')}</Button>
          <Button variant="primary" loading={busy} onClick={save} data-testid="save-workflow">{t('common.save')}</Button>
        </div>
      </div>
    </>
  )
}
