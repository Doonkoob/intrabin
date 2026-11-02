import { useState, type ReactNode } from 'react'
import { Link, useParams } from 'react-router'
import { ArrowLeft, Camera, CircleAlert, Globe, Pencil, Plus, RotateCcw, ShieldCheck, Trash2 } from 'lucide-react'
import { ApiError, del, post, put } from '../api'
import { usePrefs, useSession } from '../app'
import { useApi } from '../hooks'
import { fmtDateTime, fmtDuration } from '../format'
import { Button, Card, Checkbox, ConfirmDialog, Dialog, Empty, ErrorBanner, Field, Input, PageHeader, Segmented, Select, Spinner, Table, Textarea, td, th } from '../components/ui'
import type { DNSChange, DNSChangeRequest, DNSChangeStatus, DNSPlan, DNSProvider, DNSProviderType, DNSRecord, DNSRecordInput, DNSSnapshot, DNSZone } from '../types'
import type { Key } from '../i18n'

const changeStyle: Record<DNSChangeStatus, string> = {
  dispatching: 'bg-info-soft text-info',
  applied: 'bg-up-soft text-up',
  reconciled_applied: 'bg-up-soft text-up',
  failed: 'bg-down-soft text-down',
  reconciled_not_applied: 'bg-neutral-soft text-muted',
  unknown_outcome: 'bg-warn-soft text-warn',
}

export function ChangeBadge({ status }: { status: DNSChangeStatus }) {
  const { t } = usePrefs()
  return <span data-status={status} className={`inline-flex whitespace-nowrap rounded-full px-2 py-0.5 text-xs font-medium ${changeStyle[status]}`}>{t(`dns.status.${status}` as Key)}</span>
}

function Mono({ children }: { children: ReactNode }) {
  return <span dir="ltr" className="ltr inline-block max-w-full break-all font-mono text-xs">{children}</span>
}

const providerLabel = (t: (k: Key) => string, type: DNSProviderType) => t(`dns.type.${type}` as Key)

function ttlLabel(t: (k: Key) => string, loc: string, ttl: number) {
  return ttl === 1 ? t('dns.ttlAuto') : fmtDuration(loc, ttl)
}

// ---- main page ----

export function DNSPage() {
  const { t } = usePrefs()
  const { can } = useSession()
  const params = new URLSearchParams(window.location.search)
  const initial = params.get('tab')
  const [tab, setTab] = useState<'integrations' | 'changes' | 'snapshots'>(initial === 'changes' || initial === 'snapshots' ? initial : 'integrations')
  const [adding, setAdding] = useState(false)
  const providers = useApi<{ providers: DNSProvider[] }>('/dns/providers', 30000)
  const names = Object.fromEntries((providers.data?.providers ?? []).map((p) => [p.id, p.name]))
  return (
    <>
      <PageHeader title={t('dns.title')} subtitle={t('dns.subtitle')}
        actions={<>
          <Segmented label={t('dns.title')} value={tab} onChange={setTab} options={[
            { value: 'integrations', label: t('dns.tab.integrations') },
            { value: 'changes', label: t('dns.tab.changes') },
            { value: 'snapshots', label: t('dns.tab.snapshots') }]} />
          {can('dns.manage') && tab === 'integrations' && <Button variant="primary" onClick={() => setAdding(true)}><Plus className="size-4" />{t('dns.addIntegration')}</Button>}
        </>} />
      {tab === 'integrations' && <Integrations data={providers} />}
      {tab === 'changes' && <ChangeList providerNames={names} />}
      {tab === 'snapshots' && <SnapshotList providerNames={names} />}
      {adding && <ProviderDialog onClose={() => setAdding(false)} onSaved={() => { setAdding(false); providers.reload() }} />}
    </>
  )
}

function Integrations({ data }: { data: ReturnType<typeof useApi<{ providers: DNSProvider[] }>> }) {
  const { t, loc } = usePrefs()
  const { can } = useSession()
  const [editing, setEditing] = useState<DNSProvider | null>(null)
  const [deleting, setDeleting] = useState<DNSProvider | null>(null)
  const [busy, setBusy] = useState('')
  const [err, setErr] = useState<string | null>(null)
  if (data.loading && !data.data) return <Spinner />
  if (data.error) return <ErrorBanner error={data.error} onRetry={data.reload} />
  const list = data.data?.providers ?? []
  const verify = async (p: DNSProvider) => {
    setBusy(p.id); setErr(null)
    try { await post(`/dns/providers/${p.id}/verify`, {}); data.reload() } catch (e) { setErr((e as Error).message) } finally { setBusy('') }
  }
  const remove = async () => {
    if (!deleting) return
    setErr(null)
    try { await del(`/dns/providers/${deleting.id}`); setDeleting(null); data.reload() } catch (e) { setErr((e as Error).message); setDeleting(null) }
  }
  return (
    <>
      {err && <div className="mb-4"><ErrorBanner error={err} /></div>}
      <Card bodyClass="">
        {list.length === 0 ? (
          <Empty icon={<Globe className="size-8" />}>{t('dns.noIntegrations')}</Empty>
        ) : (
          <Table>
            <thead><tr><th className={th}>{t('common.name')}</th><th className={th}>{t('dns.provider')}</th><th className={th}>{t('dns.lastVerified')}</th><th className={th}><span className="sr-only">{t('common.actions')}</span></th></tr></thead>
            <tbody>
              {list.map((p) => (
                <tr key={p.id} className="hover:bg-surface-2" data-testid="dns-provider-row">
                  <td className={td}><Link to={`/dns/${p.id}`} className="font-medium hover:underline">{p.name}</Link></td>
                  <td className={`${td} text-muted`}>{providerLabel(t, p.type)}</td>
                  <td className={td}>
                    {p.last_error ? <span className="inline-flex items-center gap-1 text-down"><CircleAlert className="size-4" />{p.last_error}</span>
                      : <span className="text-muted">{p.last_verified_at ? fmtDateTime(loc, p.last_verified_at) : t('common.never')}</span>}
                  </td>
                  <td className={`${td} text-end`}>
                    <div className="flex justify-end gap-1">
                      <Link to={`/dns/${p.id}`}><Button size="sm">{t('dns.openZones')}</Button></Link>
                      {can('dns.manage') && <>
                        <Button size="sm" variant="ghost" loading={busy === p.id} onClick={() => verify(p)}><ShieldCheck className="size-4" />{t('dns.verify')}</Button>
                        <Button size="sm" variant="ghost" aria-label={t('common.edit')} onClick={() => setEditing(p)}><Pencil className="size-4" /></Button>
                        <Button size="sm" variant="ghost" aria-label={t('common.delete')} onClick={() => setDeleting(p)}><Trash2 className="size-4" /></Button>
                      </>}
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </Table>
        )}
      </Card>
      <p className="mt-3 text-xs text-muted">{t('dns.integrationsHelp')}</p>
      {editing && <ProviderDialog provider={editing} onClose={() => setEditing(null)} onSaved={() => { setEditing(null); data.reload() }} />}
      <ConfirmDialog open={!!deleting} onClose={() => setDeleting(null)} onConfirm={remove} title={t('dns.deleteIntegration')}
        message={t('dns.deleteIntegrationMsg', { name: deleting?.name ?? '' })} confirmLabel={t('common.delete')} />
    </>
  )
}

function ProviderDialog({ provider, onClose, onSaved }: { provider?: DNSProvider; onClose: () => void; onSaved: () => void }) {
  const { t } = usePrefs()
  const [name, setName] = useState(provider?.name ?? '')
  const [type, setType] = useState<DNSProviderType>(provider?.type ?? 'cloudflare')
  const [cred, setCred] = useState('')
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState<ApiError | null>(null)
  const f = err?.fields ?? {}
  const credField = type === 'cloudflare' ? 'api_token' : 'api_key'
  const save = async () => {
    setBusy(true); setErr(null)
    const body = { name, type, [credField]: cred }
    try {
      if (provider) await put(`/dns/providers/${provider.id}`, body)
      else await post('/dns/providers', body)
      onSaved()
    } catch (e) { setErr(e as ApiError) } finally { setBusy(false) }
  }
  return (
    <Dialog open onClose={onClose} title={provider ? t('dns.editIntegration') : t('dns.addIntegration')}
      footer={<><Button onClick={onClose}>{t('common.cancel')}</Button><Button variant="primary" loading={busy} onClick={save} data-testid="save-provider">{provider ? t('common.save') : t('dns.verifyAndSave')}</Button></>}>
      {err && <div className="mb-3"><ErrorBanner error={err.message} /></div>}
      <div className="flex flex-col gap-3">
        <Field label={t('common.name')} htmlFor="dp-name" error={f.name}><Input id="dp-name" value={name} onChange={(e) => setName(e.target.value)} /></Field>
        <Field label={t('dns.provider')} htmlFor="dp-type" error={f.type}>
          <Select id="dp-type" value={type} disabled={!!provider} onChange={(e) => setType(e.target.value as DNSProviderType)}>
            <option value="cloudflare">{t('dns.type.cloudflare')}</option>
            <option value="arvancloud">{t('dns.type.arvancloud')}</option>
          </Select>
        </Field>
        <Field label={type === 'cloudflare' ? t('dns.apiToken') : t('dns.apiKey')} htmlFor="dp-cred" error={f[credField]}
          help={provider ? t('dns.credKeep') : type === 'cloudflare' ? t('dns.cfTokenHelp') : t('dns.arvanKeyHelp')}>
          <Input id="dp-cred" type="password" dir="ltr" autoComplete="off" value={cred} onChange={(e) => setCred(e.target.value)}
            placeholder={provider ? t('wf.secretStored') : ''} />
        </Field>
      </div>
    </Dialog>
  )
}

// ---- zones and records ----

export function DNSProviderPage() {
  const { id = '' } = useParams()
  const { t, loc } = usePrefs()
  const { can } = useSession()
  const providers = useApi<{ providers: DNSProvider[] }>('/dns/providers')
  const zones = useApi<{ zones: DNSZone[] }>(can('dns.manage') ? `/dns/providers/${id}/zones` : null)
  const [zoneSel, setZoneSel] = useState('')
  const zone = zoneSel || zones.data?.zones[0]?.id || ''
  const records = useApi<{ zone: DNSZone; records: DNSRecord[] }>(zone ? `/dns/providers/${id}/zones/${encodeURIComponent(zone)}/records` : null)
  const [editing, setEditing] = useState<{ record?: DNSRecord; del?: boolean } | null>(null)
  const [snapBusy, setSnapBusy] = useState(false)
  const [msg, setMsg] = useState<string | null>(null)
  const [err, setErr] = useState<string | null>(null)
  const p = providers.data?.providers.find((x) => x.id === id)
  if (providers.loading && !providers.data) return <Spinner />
  if (!p) return <ErrorBanner error={t('dns.notFound')} />
  const zoneObj = zones.data?.zones.find((z) => z.id === zone)
  const snapshot = async () => {
    setSnapBusy(true); setErr(null); setMsg(null)
    try { await post(`/dns/providers/${id}/zones/${encodeURIComponent(zone)}/snapshots`, {}); setMsg(t('dns.snapshotTaken')) } catch (e) { setErr((e as Error).message) } finally { setSnapBusy(false) }
  }
  return (
    <>
      <Link to="/dns" className="mb-2 inline-flex items-center gap-1 text-sm text-muted hover:text-fg"><ArrowLeft className="size-4 rtl:rotate-180" />{t('dns.title')}</Link>
      <PageHeader title={p.name} subtitle={providerLabel(t, p.type)}
        actions={can('dns.manage') && zone ? <>
          <Button loading={snapBusy} onClick={snapshot}><Camera className="size-4" />{t('dns.takeSnapshot')}</Button>
          <Button variant="primary" onClick={() => setEditing({})} data-testid="add-record"><Plus className="size-4" />{t('dns.addRecord')}</Button>
        </> : undefined} />
      {!can('dns.manage') && <ErrorBanner error={t('dns.needManage')} />}
      {zones.error && <ErrorBanner error={zones.error} onRetry={zones.reload} />}
      {msg && <p className="mb-3 rounded-lg border border-up/40 bg-up-soft p-2.5 text-sm text-up" role="status">{msg}</p>}
      {err && <div className="mb-3"><ErrorBanner error={err} /></div>}
      {zones.data && (zones.data.zones.length === 0 ? <Empty icon={<Globe className="size-8" />}>{t('dns.noZones')}</Empty> : (
        <>
          <div className="mb-4 flex flex-wrap items-end gap-3">
            <Field label={t('dns.zone')} htmlFor="zone-sel">
              <Select id="zone-sel" value={zone} onChange={(e) => setZoneSel(e.target.value)} className="min-w-56">
                {zones.data.zones.map((z) => <option key={z.id} value={z.id}>{z.name}</option>)}
              </Select>
            </Field>
            <Link to={`/dns?tab=changes`} className="pb-2 text-sm text-muted hover:underline">{t('dns.viewHistory')}</Link>
          </div>
          <Card bodyClass="">
            {records.loading && !records.data ? <Spinner /> : records.error ? <ErrorBanner error={records.error} onRetry={records.reload} /> : (
              <Table>
                <thead><tr><th className={th}>{t('dns.recordType')}</th><th className={th}>{t('common.name')}</th><th className={th}>{t('dns.content')}</th><th className={`${th} hidden md:table-cell`}>TTL</th><th className={`${th} hidden md:table-cell`}>{p.type === 'cloudflare' ? t('dns.proxied') : t('dns.cloud')}</th><th className={th}><span className="sr-only">{t('common.actions')}</span></th></tr></thead>
                <tbody>
                  {(records.data?.records ?? []).map((r) => (
                    <tr key={r.id} className="hover:bg-surface-2" data-testid="dns-record-row">
                      <td className={`${td} font-mono text-xs`}>{r.type}</td>
                      <td className={td}><Mono>{r.name}</Mono></td>
                      <td className={td}><Mono>{r.content}</Mono></td>
                      <td className={`${td} hidden whitespace-nowrap text-muted md:table-cell`}>{ttlLabel(t, loc, r.ttl)}</td>
                      <td className={`${td} hidden text-muted md:table-cell`}>{r.proxied ? t('common.yes') : t('common.no')}</td>
                      <td className={`${td} text-end`}>
                        {r.editable && can('dns.manage') ? (
                          <div className="flex justify-end gap-1">
                            <Button size="sm" variant="ghost" aria-label={`${t('common.edit')} ${r.name}`} onClick={() => setEditing({ record: r })}><Pencil className="size-4" /></Button>
                            <Button size="sm" variant="ghost" aria-label={`${t('common.delete')} ${r.name}`} onClick={() => setEditing({ record: r, del: true })}><Trash2 className="size-4" /></Button>
                          </div>
                        ) : <span className="text-xs text-muted">{t('dns.readOnly')}</span>}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </Table>
            )}
          </Card>
        </>
      ))}
      {editing && zoneObj && <ChangeDialog provider={p} zone={zoneObj} record={editing.record} del={editing.del}
        onClose={() => setEditing(null)} onDone={() => { records.reload() }} />}
    </>
  )
}

function PlanView({ plan }: { plan: DNSPlan }) {
  const { t, loc } = usePrefs()
  const row = (label: string, r?: DNSRecord, tone = '') => r ? (
    <div className={`rounded-lg border border-line p-3 ${tone}`}>
      <div className="mb-1 text-xs font-medium text-muted">{label}</div>
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-sm">
        <span className="font-mono text-xs">{r.type}</span><Mono>{r.name}</Mono><span aria-hidden>→</span><Mono>{r.content}</Mono>
        <span className="text-xs text-muted">TTL {ttlLabel(t, loc, r.ttl)}{r.proxied ? ` · ${t('dns.proxied')}` : ''}</span>
      </div>
    </div>
  ) : null
  return (
    <div className="flex flex-col gap-2" data-testid="dns-plan">
      <p className="text-sm font-medium">{plan.no_change ? t('dns.plan.noChange') : t(`dns.plan.${plan.action}` as Key)}</p>
      {row(t('dns.before'), plan.before, plan.action === 'delete' ? 'bg-down-soft' : '')}
      {plan.action !== 'delete' && row(t('dns.after'), plan.after, 'bg-up-soft')}
      {(plan.warnings ?? []).map((w, i) => <p key={i} className="rounded-lg border border-warn/40 bg-warn-soft p-2.5 text-xs text-warn">{w}</p>)}
    </div>
  )
}

function ChangeDialog({ provider, zone, record, del: isDelete, onClose, onDone }: { provider: DNSProvider; zone: DNSZone; record?: DNSRecord; del?: boolean; onClose: () => void; onDone: () => void }) {
  const { t } = usePrefs()
  const [rec, setRec] = useState<DNSRecordInput>({ type: record?.type ?? 'A', name: record?.name ?? '', content: record?.content ?? '', ttl: record?.ttl ?? 1, proxied: record?.proxied ?? false })
  const [plan, setPlan] = useState<DNSPlan | null>(null)
  const [result, setResult] = useState<DNSChange | null>(null)
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState<ApiError | null>(null)
  const req: DNSChangeRequest = { provider_id: provider.id, zone: zone.id, action: isDelete ? 'delete' : 'upsert', record_id: record?.id, record: isDelete ? { ...rec, content: '' } : rec }
  const preview = async () => {
    setBusy(true); setErr(null)
    try { setPlan((await post<{ plan: DNSPlan }>('/dns/changes/plan', req)).plan) } catch (e) { setErr(e as ApiError) } finally { setBusy(false) }
  }
  const apply = async () => {
    setBusy(true); setErr(null)
    try { setResult((await post<{ change: DNSChange }>('/dns/changes', { change: req, plan })).change); onDone() } catch (e) {
      const ae = e as ApiError
      setErr(ae)
      if (ae.code === 'dns_conflict') setPlan(null)
    } finally { setBusy(false) }
  }
  const upd = (p: Partial<DNSRecordInput>) => { setRec({ ...rec, ...p }); setPlan(null) }
  const title = isDelete ? t('dns.deleteRecord') : record ? t('dns.editRecord') : t('dns.addRecord')
  const footer = result ? <Button variant="primary" onClick={onClose}>{t('common.close')}</Button> : <>
    <Button onClick={onClose}>{t('common.cancel')}</Button>
    {!plan ? <Button variant="primary" loading={busy} onClick={preview} data-testid="dns-preview">{t('dns.preview')}</Button>
      : <Button variant={isDelete ? 'danger' : 'primary'} loading={busy} disabled={plan.no_change} onClick={apply} data-testid="dns-apply">{t('dns.apply')}</Button>}
  </>
  return (
    <Dialog open onClose={onClose} title={title} wide footer={footer}>
      {err && <div className="mb-3"><ErrorBanner error={err.message} /></div>}
      {result ? <ChangeResult change={result} /> : (
        <div className="flex flex-col gap-4">
          {!isDelete && (
            <div className="grid gap-3 md:grid-cols-2">
              <Field label={t('dns.recordType')} htmlFor="rec-type" error={err?.fields['record.type']}>
                <Select id="rec-type" value={rec.type} disabled={!!record} onChange={(e) => upd({ type: e.target.value })}>{['A', 'AAAA', 'CNAME', 'TXT'].map((x) => <option key={x}>{x}</option>)}</Select>
              </Field>
              <Field label={t('common.name')} htmlFor="rec-name" help={t('dns.nameHelp', { zone: zone.name })}>
                <Input id="rec-name" dir="ltr" value={rec.name} disabled={!!record} onChange={(e) => upd({ name: e.target.value })} placeholder={`www.${zone.name}`} />
              </Field>
              <Field label={t('dns.content')} htmlFor="rec-content" help={t(`dns.contentHelp.${rec.type}` as Key)}>
                {rec.type === 'TXT' ? <Textarea id="rec-content" dir="ltr" rows={2} value={rec.content} onChange={(e) => upd({ content: e.target.value })} />
                  : <Input id="rec-content" dir="ltr" value={rec.content} onChange={(e) => upd({ content: e.target.value })} />}
              </Field>
              <Field label="TTL" htmlFor="rec-ttl" help={t('dns.ttlHelp')}>
                <Input id="rec-ttl" type="number" min={1} value={rec.ttl} onChange={(e) => upd({ ttl: Number(e.target.value) })} />
              </Field>
              {(rec.type === 'A' || rec.type === 'AAAA' || rec.type === 'CNAME') && (
                <div className="md:col-span-2"><Checkbox label={provider.type === 'cloudflare' ? t('dns.proxied') : t('dns.cloud')} checked={rec.proxied} onChange={(v) => upd({ proxied: v })} /></div>
              )}
            </div>
          )}
          {isDelete && record && !plan && <p className="text-sm">{t('dns.deleteConfirm', { name: record.name, type: record.type })}</p>}
          {plan && <PlanView plan={plan} />}
          <p className="text-xs text-muted">{t('dns.safetyNote')}</p>
        </div>
      )}
    </Dialog>
  )
}

function ChangeResult({ change }: { change: DNSChange }) {
  const { t } = usePrefs()
  return (
    <div className="flex flex-col gap-2 text-sm" data-testid="dns-result">
      <div className="flex items-center gap-2"><ChangeBadge status={change.status} />{change.verified && <span className="text-xs text-up">{t('dns.verified')}</span>}</div>
      {change.error && <p className="text-muted">{change.error}</p>}
      {change.status === 'unknown_outcome' && <p className="rounded-lg border border-warn/40 bg-warn-soft p-2.5 text-xs text-warn">{t('dns.unknownHelp')}</p>}
      <p className="text-xs text-muted">{t('dns.snapshotRef')} <Mono>{change.snapshot_id}</Mono></p>
    </div>
  )
}

// ---- change history ----

function sourceLabel(t: (k: Key) => string, s: string) {
  if (s.startsWith('workflow:')) return t('dns.source.workflow')
  if (s.startsWith('revert:')) return t('dns.source.revert')
  if (s.startsWith('restore:')) return t('dns.source.restore')
  return t('dns.source.manual')
}

function ChangeList({ providerNames }: { providerNames: Record<string, string> }) {
  const { t, loc } = usePrefs()
  const [status, setStatus] = useState('')
  const changes = useApi<{ changes: DNSChange[] }>(`/dns/changes?limit=200${status ? '&status=' + status : ''}`, 10000)
  const [open, setOpen] = useState<DNSChange | null>(null)
  return (
    <>
      <div className="mb-3 flex items-end gap-3">
        <Field label={t('common.status')} htmlFor="ch-status">
          <Select id="ch-status" value={status} onChange={(e) => setStatus(e.target.value)}>
            <option value="">{t('common.all')}</option>
            {(['applied', 'failed', 'unknown_outcome', 'reconciled_applied', 'reconciled_not_applied', 'dispatching'] as const).map((s) => <option key={s} value={s}>{t(`dns.status.${s}` as Key)}</option>)}
          </Select>
        </Field>
      </div>
      <Card bodyClass="">
        {changes.loading && !changes.data ? <Spinner /> : changes.error ? <ErrorBanner error={changes.error} onRetry={changes.reload} /> :
          (changes.data?.changes.length ?? 0) === 0 ? <Empty>{t('dns.noChanges')}</Empty> : (
            <Table>
              <thead><tr><th className={th}>{t('dns.when')}</th><th className={th}>{t('dns.record')}</th><th className={th}>{t('dns.change')}</th><th className={th}>{t('common.status')}</th><th className={`${th} hidden md:table-cell`}>{t('dns.source')}</th></tr></thead>
              <tbody>
                {changes.data!.changes.map((c) => (
                  <tr key={c.id} className="cursor-pointer hover:bg-surface-2" onClick={() => setOpen(c)} data-testid="dns-change-row">
                    <td className={`${td} whitespace-nowrap text-muted`}><button type="button" className="hover:underline" onClick={() => setOpen(c)}>{fmtDateTime(loc, c.created_at)}</button></td>
                    <td className={td}><span className="me-2 font-mono text-xs">{c.record_type}</span><Mono>{c.record_name}</Mono><div className="text-xs text-muted">{providerNames[c.provider_id] ?? ''} · {c.zone_name}</div></td>
                    <td className={td}><span className="text-xs text-muted">{t(`dns.action.${c.action}` as Key)}</span> <Mono>{c.before?.content ?? '∅'} → {c.action === 'delete' ? '∅' : c.after?.content}</Mono></td>
                    <td className={td}><ChangeBadge status={c.status} /></td>
                    <td className={`${td} hidden text-muted md:table-cell`}>{sourceLabel(t, c.source)} · {c.actor}</td>
                  </tr>
                ))}
              </tbody>
            </Table>
          )}
      </Card>
      {open && <ChangeDetail change={open} onClose={() => setOpen(null)} onChanged={() => { setOpen(null); changes.reload() }} />}
    </>
  )
}

function ChangeDetail({ change, onClose, onChanged }: { change: DNSChange; onClose: () => void; onChanged: () => void }) {
  const { t, loc } = usePrefs()
  const { can } = useSession()
  const [plan, setPlan] = useState<DNSPlan | null>(null)
  const [result, setResult] = useState<DNSChange | null>(null)
  const [reconcile, setReconcile] = useState(false)
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState<string | null>(null)
  const revertable = can('dns.manage') && (change.status === 'applied' || change.status === 'reconciled_applied')
  const planRevert = async () => {
    setBusy(true); setErr(null)
    try { setPlan((await post<{ plan: DNSPlan }>(`/dns/changes/${change.id}/revert`, { dry_run: true })).plan) } catch (e) { setErr((e as Error).message) } finally { setBusy(false) }
  }
  const doRevert = async () => {
    setBusy(true); setErr(null)
    try { setResult((await post<{ change: DNSChange }>(`/dns/changes/${change.id}/revert`, { plan })).change) } catch (e) { setErr((e as Error).message); setPlan(null) } finally { setBusy(false) }
  }
  const dl = (k: string, v: ReactNode) => <><dt className="text-muted">{k}</dt><dd className="min-w-0">{v}</dd></>
  return (
    <Dialog open onClose={result ? onChanged : onClose} title={t('dns.changeDetail')} wide
      footer={result ? <Button variant="primary" onClick={onChanged}>{t('common.close')}</Button> : <>
        <Button onClick={onClose}>{t('common.close')}</Button>
        {change.status === 'unknown_outcome' && can('dns.manage') && <Button onClick={() => setReconcile(true)} data-testid="dns-reconcile">{t('wf.reconcile')}</Button>}
        {revertable && (!plan ? <Button loading={busy} onClick={planRevert} data-testid="dns-revert-preview"><RotateCcw className="size-4" />{t('dns.revert')}</Button>
          : <Button variant="danger" loading={busy} disabled={plan.no_change} onClick={doRevert} data-testid="dns-revert-apply">{t('dns.applyRevert')}</Button>)}
      </>}>
      {err && <div className="mb-3"><ErrorBanner error={err} /></div>}
      {result ? <ChangeResult change={result} /> : <>
        <dl className="grid grid-cols-[8rem_1fr] gap-x-4 gap-y-2 text-sm">
          {dl(t('common.status'), <span className="inline-flex items-center gap-2"><ChangeBadge status={change.status} />{change.verified && <span className="text-xs text-up">{t('dns.verified')}</span>}</span>)}
          {dl(t('dns.record'), <><span className="me-2 font-mono text-xs">{change.record_type}</span><Mono>{change.record_name}</Mono></>)}
          {dl(t('dns.zone'), change.zone_name)}
          {dl(t('dns.before'), <Mono>{change.before ? `${change.before.content} (TTL ${ttlLabel(t, loc, change.before.ttl)})` : '∅'}</Mono>)}
          {dl(t('dns.after'), <Mono>{change.action === 'delete' || !change.after ? '∅' : `${change.after.content} (TTL ${ttlLabel(t, loc, change.after.ttl)})`}</Mono>)}
          {dl(t('dns.source'), `${sourceLabel(t, change.source)} · ${change.actor}`)}
          {dl(t('dns.when'), fmtDateTime(loc, change.created_at))}
          {change.fence_leader && dl(t('dns.fence'), <Mono>{change.fence_leader} · term {change.fence_term}</Mono>)}
          {dl(t('dns.snapshotRef'), <Mono>{change.snapshot_id || '—'}</Mono>)}
          {change.error && dl(t('dns.details'), <span className="text-muted">{change.error}</span>)}
        </dl>
        {change.status === 'unknown_outcome' && <p className="mt-3 rounded-lg border border-warn/40 bg-warn-soft p-2.5 text-xs text-warn">{t('dns.unknownHelp')}</p>}
        {plan && <div className="mt-4"><PlanView plan={plan} /></div>}
      </>}
      {reconcile && <DNSReconcileDialog change={change} onClose={() => setReconcile(false)} onDone={onChanged} />}
    </Dialog>
  )
}

function DNSReconcileDialog({ change, onClose, onDone }: { change: DNSChange; onClose: () => void; onDone: () => void }) {
  const { t } = usePrefs()
  const [result, setResult] = useState<'applied' | 'not_applied' | ''>('')
  const [note, setNote] = useState('')
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState<ApiError | null>(null)
  const submit = async () => {
    setBusy(true); setErr(null)
    try { await post(`/dns/changes/${change.id}/reconcile`, { result, note }); onDone() } catch (e) { setErr(e as ApiError) } finally { setBusy(false) }
  }
  return (
    <Dialog open onClose={onClose} title={t('wf.reconcileTitle')}
      footer={<><Button onClick={onClose}>{t('common.cancel')}</Button><Button variant="primary" loading={busy} disabled={!result || !note.trim()} onClick={submit}>{t('wf.reconcile')}</Button></>}>
      <p className="text-sm">{t('dns.reconcileHelp')}</p>
      {err && <div className="mt-3"><ErrorBanner error={err.message} /></div>}
      <fieldset className="mt-4 flex flex-col gap-2 text-sm">
        <legend className="mb-1 font-medium">{t('wf.reconcileResult')}</legend>
        <label className="flex items-center gap-2"><input type="radio" name="dres" checked={result === 'applied'} onChange={() => setResult('applied')} className="accent-[var(--accent)]" />{t('dns.reconcileApplied')}</label>
        <label className="flex items-center gap-2"><input type="radio" name="dres" checked={result === 'not_applied'} onChange={() => setResult('not_applied')} className="accent-[var(--accent)]" />{t('dns.reconcileNotApplied')}</label>
      </fieldset>
      <div className="mt-4"><Field label={t('wf.reconcileNote')} htmlFor="drec-note" error={err?.fields.note}><Textarea id="drec-note" value={note} onChange={(e) => setNote(e.target.value)} rows={3} /></Field></div>
    </Dialog>
  )
}

// ---- snapshots ----

function SnapshotList({ providerNames }: { providerNames: Record<string, string> }) {
  const { t, loc } = usePrefs()
  const snaps = useApi<{ snapshots: DNSSnapshot[] }>('/dns/snapshots?limit=200', 30000)
  const [open, setOpen] = useState<string | null>(null)
  return (
    <>
      <Card bodyClass="">
        {snaps.loading && !snaps.data ? <Spinner /> : snaps.error ? <ErrorBanner error={snaps.error} onRetry={snaps.reload} /> :
          (snaps.data?.snapshots.length ?? 0) === 0 ? <Empty icon={<Camera className="size-8" />}>{t('dns.noSnapshots')}</Empty> : (
            <Table>
              <thead><tr><th className={th}>{t('dns.when')}</th><th className={th}>{t('dns.zone')}</th><th className={th}>{t('dns.reason')}</th><th className={`${th} hidden md:table-cell`}>{t('dns.records')}</th><th className={th}><span className="sr-only">{t('common.actions')}</span></th></tr></thead>
              <tbody>
                {snaps.data!.snapshots.map((s) => (
                  <tr key={s.id} className="hover:bg-surface-2" data-testid="dns-snapshot-row">
                    <td className={`${td} whitespace-nowrap text-muted`}>{fmtDateTime(loc, s.created_at)}</td>
                    <td className={td}><Mono>{s.zone_name}</Mono><div className="text-xs text-muted">{providerNames[s.provider_id] ?? ''}</div></td>
                    <td className={`${td} text-muted`}>{s.reason} · {s.created_by}</td>
                    <td className={`${td} hidden tabular md:table-cell`}>{s.record_count}</td>
                    <td className={`${td} text-end`}><Button size="sm" onClick={() => setOpen(s.id)}>{t('dns.viewSnapshot')}</Button></td>
                  </tr>
                ))}
              </tbody>
            </Table>
          )}
      </Card>
      {open && <SnapshotDialog id={open} onClose={() => setOpen(null)} />}
    </>
  )
}

function SnapshotDialog({ id, onClose }: { id: string; onClose: () => void }) {
  const { t, loc } = usePrefs()
  const { can } = useSession()
  const snap = useApi<{ snapshot: DNSSnapshot }>(`/dns/snapshots/${id}`)
  const [includeDeletes, setIncludeDeletes] = useState(false)
  const [plans, setPlans] = useState<DNSPlan[] | null>(null)
  const [result, setResult] = useState<{ changes: DNSChange[]; planned: number; error?: string } | null>(null)
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState<string | null>(null)
  const preview = async () => {
    setBusy(true); setErr(null)
    try { setPlans((await post<{ plans: DNSPlan[] }>(`/dns/snapshots/${id}/restore`, { dry_run: true, include_deletes: includeDeletes })).plans) } catch (e) { setErr((e as Error).message) } finally { setBusy(false) }
  }
  const restore = async () => {
    setBusy(true); setErr(null)
    try { setResult(await post(`/dns/snapshots/${id}/restore`, { include_deletes: includeDeletes, plans })) } catch (e) { setErr((e as Error).message); setPlans(null) } finally { setBusy(false) }
  }
  const s = snap.data?.snapshot
  return (
    <Dialog open onClose={onClose} title={t('dns.snapshot')} wide
      footer={<>
        <Button onClick={onClose}>{t('common.close')}</Button>
        {can('dns.manage') && s && !result && (plans === null
          ? <Button loading={busy} onClick={preview} data-testid="restore-preview"><RotateCcw className="size-4" />{t('dns.restorePreview')}</Button>
          : <Button variant="danger" loading={busy} disabled={plans.length === 0} onClick={restore} data-testid="restore-apply">{t('dns.restoreApply', { n: plans.length })}</Button>)}
      </>}>
      {err && <div className="mb-3"><ErrorBanner error={err} /></div>}
      {!s ? <Spinner /> : result ? (
        <div className="flex flex-col gap-2 text-sm" data-testid="restore-result">
          <p>{t('dns.restoreDone', { n: result.changes.filter((c) => c.status === 'applied' || c.status === 'reconciled_applied').length, total: result.planned })}</p>
          {result.error && <ErrorBanner error={result.error} />}
          {result.changes.map((c) => <div key={c.id} className="flex items-center gap-2"><ChangeBadge status={c.status} /><span className="font-mono text-xs">{c.record_type}</span><Mono>{c.record_name}</Mono></div>)}
        </div>
      ) : plans !== null ? (
        <div className="flex flex-col gap-3">
          {plans.length === 0 ? <p className="text-sm">{t('dns.restoreNothing')}</p> : plans.map((p, i) => <PlanView key={i} plan={p} />)}
        </div>
      ) : (
        <>
          <p className="mb-3 text-sm text-muted">{s.zone_name} · {fmtDateTime(loc, s.created_at)} · {s.reason}</p>
          <div className="max-h-80 overflow-y-auto">
            <Table>
              <thead><tr><th className={th}>{t('dns.recordType')}</th><th className={th}>{t('common.name')}</th><th className={th}>{t('dns.content')}</th><th className={th}>TTL</th></tr></thead>
              <tbody>{(s.records ?? []).map((r) => (
                <tr key={r.id}><td className={`${td} font-mono text-xs`}>{r.type}</td><td className={td}><Mono>{r.name}</Mono></td><td className={td}><Mono>{r.content}</Mono></td><td className={`${td} text-muted`}>{ttlLabel(t, loc, r.ttl)}</td></tr>
              ))}</tbody>
            </Table>
          </div>
          {can('dns.manage') && <div className="mt-3"><Checkbox label={t('dns.includeDeletes')} checked={includeDeletes} onChange={setIncludeDeletes} /><p className="mt-1 text-xs text-muted">{t('dns.restoreHelp')}</p></div>}
        </>
      )}
    </Dialog>
  )
}
