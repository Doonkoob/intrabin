import { useState, type FormEvent } from 'react'
import { AlertTriangle, Boxes, Check, Copy, ExternalLink, KeyRound, LogIn, Network, Server, ShieldCheck, X } from 'lucide-react'
import { ApiError, del, post } from '../api'
import { usePrefs, useSession } from '../app'
import { useApi } from '../hooks'
import { fmtBytes, fmtDateTime, fmtDuration, fmtNumber, fmtRelative } from '../format'
import { Button, Card, ConfirmDialog, Dialog, Empty, ErrorBanner, Field, Input, PageHeader, Select, Spinner, Table, Textarea, td, th } from '../components/ui'
import type { ClusterMember, ClusterStatus, EnrollmentToken, NodeInfo } from '../types'
import type { Key } from '../i18n'

function NodeCard({ node }: { node: NodeInfo }) {
  const { t, loc } = usePrefs()
  return (
    <dl className="grid grid-cols-1 gap-x-6 gap-y-2 text-sm sm:grid-cols-[auto_1fr]">
      <dt className="text-muted">{t('cluster.nodeName')}</dt><dd className="font-medium">{node.node_name}</dd>
      <dt className="text-muted">{t('cluster.nodeId')}</dt><dd className="break-all font-mono text-xs"><span className="ltr inline-block">{node.node_id}</span></dd>
      <dt className="text-muted">{t('cluster.role')}</dt><dd><RoleBadge role={node.role} /></dd>
      <dt className="text-muted">{t('cluster.version')}</dt><dd><span className="ltr inline-block">{node.version} ({node.commit.slice(0, 7)})</span></dd>
      <dt className="text-muted">{t('cluster.uptime')}</dt><dd>{fmtDuration(loc, node.uptime_seconds ?? 0)}</dd>
      <dt className="text-muted">{t('cluster.assigned')}</dt><dd className="tabular">{fmtNumber(loc, node.assigned_monitors ?? 0)}</dd>
      <dt className="text-muted">{t('cluster.db')}</dt><dd className="tabular">{fmtBytes(loc, node.database_bytes)}</dd>
    </dl>
  )
}

const roleTone: Record<string, string> = {
  LEADER: 'bg-accent-soft text-accent',
  FOLLOWER: 'bg-surface-2 text-fg',
  STANDALONE: 'bg-surface-2 text-fg',
  NO_QUORUM: 'bg-down/15 text-down',
}

export function RoleBadge({ role }: { role: string }) {
  const { t } = usePrefs()
  return <span className={`inline-flex rounded-full px-2 py-0.5 text-xs font-medium ${roleTone[role] ?? 'bg-surface-2'}`} data-testid="role-badge">{t(`roleName.${role}` as Key)}</span>
}

function Reach({ ok }: { ok: boolean }) {
  const { t } = usePrefs()
  return ok
    ? <span className="inline-flex items-center gap-1 text-up"><Check className="size-3.5" aria-hidden />{t('cluster.reachable')}</span>
    : <span className="inline-flex items-center gap-1 text-down"><X className="size-3.5" aria-hidden />{t('cluster.unreachable')}</span>
}

/** Cluster summary + members (+ admin actions when `admin`). Used on leader and follower views. */
function ClusterView({ c, admin, reload }: { c: ClusterStatus; admin: boolean; reload: () => void }) {
  const { t, loc } = usePrefs()
  const [pending, setPending] = useState<{ m: ClusterMember; action: 'promote' | 'demote' | 'remove' } | null>(null)
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState<string | null>(null)
  const leader = c.members.find((m) => m.leader)
  const run = async () => {
    if (!pending) return
    setBusy(true)
    setErr(null)
    try {
      if (pending.action === 'remove') await del(`/cluster/nodes/${pending.m.id}`)
      else await post(`/cluster/nodes/${pending.m.id}/${pending.action}`)
      setPending(null)
      reload()
    } catch (e) {
      setErr((e as ApiError).message)
      setPending(null)
    } finally {
      setBusy(false)
    }
  }
  const names = Object.fromEntries(c.members.map((m) => [m.id, m.name]))
  return (
    <>
      {c.warnings.length > 0 && (
        <div className="flex flex-col gap-2">
          {c.warnings.map((w) => (
            <div key={w} role="status" className="flex items-start gap-2 rounded-lg border border-warn/40 bg-warn/10 p-3 text-sm">
              <AlertTriangle className="mt-0.5 size-4 shrink-0 text-warn" aria-hidden />{t(`cluster.warn.${w}` as Key)}
            </div>
          ))}
        </div>
      )}
      {err && <ErrorBanner error={err} />}
      <div className="grid gap-4 lg:grid-cols-3">
        <Card title={<span className="inline-flex items-center gap-2"><Boxes className="size-4" />{t('cluster.summary')}</span>} className="lg:col-span-2">
          <dl className="grid grid-cols-1 gap-x-6 gap-y-2 text-sm sm:grid-cols-[auto_1fr]">
            <dt className="text-muted">{t('cluster.thisNode')}</dt><dd><RoleBadge role={c.role} /></dd>
            <dt className="text-muted">{t('cluster.leader')}</dt>
            <dd>{leader ? leader.name : <span className="text-down">{t('cluster.noLeader')}</span>}
              {c.leader_api_url && !leader?.self && <a className="ms-2 inline-flex items-center gap-1 text-accent hover:underline" href={c.leader_api_url}>{t('cluster.openLeader')}<ExternalLink className="size-3.5" /></a>}
            </dd>
            <dt className="text-muted">{t('cluster.clusterId')}</dt><dd className="break-all font-mono text-xs"><span className="ltr inline-block">{c.cluster_id}</span></dd>
            <dt className="text-muted">{t('cluster.term')}</dt><dd className="tabular">{c.term}</dd>
            <dt className="text-muted">{t('cluster.applied')}</dt><dd className="tabular">{fmtNumber(loc, c.applied_index ?? 0)}</dd>
            <dt className="text-muted">{t('cluster.advertise')}</dt><dd className="font-mono text-xs"><span className="ltr inline-block">{c.advertise}</span></dd>
          </dl>
        </Card>
        <Card title={<span className="inline-flex items-center gap-2"><ShieldCheck className="size-4" />{t('cluster.gate')}</span>}>
          {c.gate && (
            <div className="flex flex-col gap-2 text-sm" data-testid="gate">
              <div className={`text-base font-semibold ${c.gate.open ? 'text-up' : 'text-down'}`}>{c.gate.open ? t('cluster.gateOpen') : t('cluster.gateClosed')}</div>
              <p className="text-muted">{t('cluster.gateDetail', { reachable: fmtNumber(loc, c.gate.reachable), voters: fmtNumber(loc, c.gate.voters), required: fmtNumber(loc, c.gate.required) })}</p>
              <p className="text-xs text-muted">{t('cluster.gateHelp')}</p>
            </div>
          )}
        </Card>
      </div>
      <Card title={t('cluster.members')} bodyClass="p-0">
        <Table>
          <thead><tr>
            <th className={th}>{t('cluster.nodeName')}</th><th className={th}>{t('cluster.suffrage')}</th><th className={th}>{t('cluster.connectivity')}</th>
            <th className={th}>{t('cluster.lastSeen')}</th><th className={th}>{t('cluster.applied')}</th><th className={th}>{t('cluster.outbox')}</th>
            <th className={th}>{t('cluster.address')}</th>{admin && <th className={th}><span className="sr-only">{t('common.actions')}</span></th>}
          </tr></thead>
          <tbody>
            {c.members.map((m) => (
              <tr key={m.id} data-testid={`member-${m.name}`}>
                <td className={td}>
                  <div className="font-medium">{m.name}{m.self && <span className="ms-2 text-xs text-muted">({t('cluster.thisNodeShort')})</span>}</div>
                  <div className="text-xs text-muted">{m.leader ? t('roleName.LEADER') : m.raft_state ? t(`raft.${m.raft_state}` as Key) : '—'}{m.location && ` · ${m.location}`}</div>
                </td>
                <td className={td}>{m.suffrage === 'voter' ? t('cluster.voter') : t('cluster.nonvoter')}</td>
                <td className={td}><Reach ok={m.reachable} /></td>
                <td className={`${td} tabular`}>{m.self ? '—' : m.last_seen_ms == null ? t('cluster.never') : fmtDuration(loc, Math.round(m.last_seen_ms / 1000))}</td>
                <td className={`${td} tabular`}>{fmtNumber(loc, m.applied_index)}</td>
                <td className={`${td} tabular`}>{fmtNumber(loc, m.outbox)}</td>
                <td className={`${td} ltr text-start font-mono text-xs`}>{m.address}</td>
                {admin && (
                  <td className={`${td} text-end`}>
                    {!m.self && (
                      <div className="flex justify-end gap-1">
                        {m.suffrage === 'nonvoter'
                          ? <Button size="sm" onClick={() => setPending({ m, action: 'promote' })}>{t('cluster.promote')}</Button>
                          : <Button size="sm" variant="ghost" onClick={() => setPending({ m, action: 'demote' })}>{t('cluster.demote')}</Button>}
                        <Button size="sm" variant="ghost" className="text-down" onClick={() => setPending({ m, action: 'remove' })}>{t('common.remove')}</Button>
                      </div>
                    )}
                  </td>
                )}
              </tr>
            ))}
          </tbody>
        </Table>
      </Card>
      {c.mesh && c.members.length > 1 && (
        <Card title={<span className="inline-flex items-center gap-2"><Network className="size-4" />{t('cluster.mesh')}</span>}>
          <p className="mb-3 text-xs text-muted">{t('cluster.meshHelp')}</p>
          <div className="overflow-x-auto">
            <table className="text-sm" data-testid="mesh">
              <thead><tr><th className="p-2" />{c.members.map((m) => <th key={m.id} scope="col" className="p-2 text-xs font-medium text-muted">{m.name}</th>)}</tr></thead>
              <tbody>
                {c.members.map((from) => (
                  <tr key={from.id}>
                    <th scope="row" className="p-2 text-start text-xs font-medium text-muted">{from.name}</th>
                    {c.members.map((to) => {
                      if (from.id === to.id) return <td key={to.id} className="p-2 text-center text-muted">·</td>
                      const row = c.mesh?.[from.id]
                      const ok = row ? !!row[to.id] : undefined
                      const label = ok === undefined ? t('cluster.unknown') : ok ? t('cluster.reachable') : t('cluster.unreachable')
                      return (
                        <td key={to.id} className="p-2 text-center" title={`${names[from.id]} ↔ ${names[to.id]}: ${label}`}>
                          {ok === undefined ? <span className="text-muted">?</span> : ok ? <Check className="mx-auto size-4 text-up" aria-label={label} /> : <X className="mx-auto size-4 text-down" aria-label={label} />}
                        </td>
                      )
                    })}
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </Card>
      )}
      <ConfirmDialog open={!!pending} onClose={() => setPending(null)} onConfirm={run} busy={busy}
        title={pending ? t(`cluster.confirm.${pending.action}.title` as Key, { name: pending.m.name }) : ''}
        message={pending ? t(`cluster.confirm.${pending.action}.body` as Key, { name: pending.m.name }) : ''}
        confirmLabel={pending ? t(`cluster.${pending.action}` as Key) : ''} />
    </>
  )
}

function tokenState(tk: EnrollmentToken): 'used' | 'revoked' | 'expired' | 'active' {
  if (tk.used_at) return 'used'
  if (tk.revoked_at) return 'revoked'
  if (new Date(tk.expires_at).getTime() < Date.now()) return 'expired'
  return 'active'
}

function TokensCard() {
  const { t, loc } = usePrefs()
  const tokens = useApi<{ tokens: EnrollmentToken[] }>('/cluster/tokens', 15000)
  const [open, setOpen] = useState(false)
  const [form, setForm] = useState({ description: '', ttl_minutes: '30' })
  const [created, setCreated] = useState<string | null>(null)
  const [copied, setCopied] = useState(false)
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState<string | null>(null)
  const [revoke, setRevoke] = useState<EnrollmentToken | null>(null)
  const create = async (e: FormEvent) => {
    e.preventDefault()
    setBusy(true)
    setErr(null)
    try {
      const r = await post<{ token: string }>('/cluster/tokens', { description: form.description, ttl_minutes: Number(form.ttl_minutes) })
      setCreated(r.token)
      setOpen(false)
      setForm({ description: '', ttl_minutes: '30' })
      tokens.reload()
    } catch (e2) {
      setErr((e2 as ApiError).message)
    } finally {
      setBusy(false)
    }
  }
  const copy = async () => {
    if (!created) return
    try { await navigator.clipboard.writeText(created); setCopied(true) } catch { setCopied(false) }
  }
  const doRevoke = async () => {
    if (!revoke) return
    setBusy(true)
    try { await del(`/cluster/tokens/${revoke.id}`); setRevoke(null); tokens.reload() } catch (e2) { setErr((e2 as ApiError).message); setRevoke(null) } finally { setBusy(false) }
  }
  return (
    <Card title={<span className="inline-flex items-center gap-2"><KeyRound className="size-4" />{t('cluster.tokens')}</span>}
      actions={<Button size="sm" variant="primary" onClick={() => setOpen(true)}>{t('cluster.newToken')}</Button>} bodyClass="p-0">
      {err && <div className="p-3"><ErrorBanner error={err} /></div>}
      <p className="px-4 pt-3 text-xs text-muted">{t('cluster.tokensHelp')}</p>
      {tokens.data && tokens.data.tokens.length === 0 ? <Empty>{t('cluster.noTokens')}</Empty> : (
        <Table>
          <thead><tr><th className={th}>{t('field.description')}</th><th className={th}>{t('common.status')}</th><th className={th}>{t('cluster.expires')}</th><th className={th}>{t('cluster.createdBy')}</th><th className={th}><span className="sr-only">{t('common.actions')}</span></th></tr></thead>
          <tbody>
            {(tokens.data?.tokens ?? []).map((tk) => {
              const s = tokenState(tk)
              return (
                <tr key={tk.id}>
                  <td className={td}>{tk.description || <span className="text-muted">—</span>}</td>
                  <td className={td}><span className={s === 'active' ? 'text-up' : 'text-muted'}>{t(`cluster.token.${s}` as Key)}</span></td>
                  <td className={td} title={fmtDateTime(loc, tk.expires_at)}>{fmtRelative(loc, tk.expires_at)}</td>
                  <td className={td}>{tk.created_by}</td>
                  <td className={`${td} text-end`}>{s === 'active' && <Button size="sm" variant="ghost" className="text-down" onClick={() => setRevoke(tk)}>{t('cluster.revoke')}</Button>}</td>
                </tr>
              )
            })}
          </tbody>
        </Table>
      )}
      <Dialog open={open} onClose={() => setOpen(false)} title={t('cluster.newToken')}
        footer={<><Button onClick={() => setOpen(false)}>{t('common.cancel')}</Button><Button variant="primary" type="submit" form="token-form" loading={busy}>{t('cluster.createToken')}</Button></>}>
        <form id="token-form" onSubmit={create} className="flex flex-col gap-4">
          <Field label={t('field.description')} htmlFor="tk-desc" optional><Input id="tk-desc" value={form.description} onChange={(e) => setForm({ ...form, description: e.target.value })} maxLength={200} /></Field>
          <Field label={t('cluster.ttl')} htmlFor="tk-ttl">
            <Select id="tk-ttl" value={form.ttl_minutes} onChange={(e) => setForm({ ...form, ttl_minutes: e.target.value })}>
              {[15, 30, 60, 240, 1440].map((m) => <option key={m} value={m}>{fmtDuration(loc, m * 60)}</option>)}
            </Select>
          </Field>
        </form>
      </Dialog>
      <Dialog open={!!created} onClose={() => { setCreated(null); setCopied(false) }} title={t('cluster.tokenCreated')} wide
        footer={<Button variant="primary" onClick={() => { setCreated(null); setCopied(false) }}>{t('common.done')}</Button>}>
        <p className="mb-3 text-sm text-muted">{t('cluster.tokenOnce')}</p>
        <Textarea readOnly value={created ?? ''} className="ltr h-28 font-mono text-xs" data-testid="join-token" onFocus={(e) => e.currentTarget.select()} />
        <Button className="mt-3" onClick={copy}>{copied ? <Check className="size-4" /> : <Copy className="size-4" />}{copied ? t('common.copied') : t('common.copy')}</Button>
      </Dialog>
      <ConfirmDialog open={!!revoke} onClose={() => setRevoke(null)} onConfirm={doRevoke} busy={busy} title={t('cluster.revokeTitle')} message={t('cluster.revokeBody')} confirmLabel={t('cluster.revoke')} />
    </Card>
  )
}

function StandaloneActions({ c, reload }: { c: ClusterStatus; reload: () => void }) {
  const { t } = usePrefs()
  const { me, refresh } = useSession()
  const [adv, setAdv] = useState(c.defaults.advertise ?? '')
  const [busy, setBusy] = useState<'init' | 'join' | null>(null)
  const [err, setErr] = useState<string | null>(null)
  const [fields, setFields] = useState<Record<string, string>>({})
  const [join, setJoin] = useState({ token: '', advertise: c.defaults.advertise ?? '', data_action: 'export' })
  const [confirmJoin, setConfirmJoin] = useState(false)
  const [confirmInit, setConfirmInit] = useState(false)
  const [joined, setJoined] = useState<string | null>(null)
  const init = async () => {
    setBusy('init')
    setErr(null)
    try {
      await post('/cluster/init', { advertise: adv })
      setConfirmInit(false)
      await refresh()
      reload()
    } catch (e) {
      setErr((e as ApiError).message)
      setConfirmInit(false)
    } finally {
      setBusy(null)
    }
  }
  const doJoin = async () => {
    setBusy('join')
    setErr(null)
    setFields({})
    try {
      const r = await post<{ backup_path?: string }>('/cluster/join', join)
      setConfirmJoin(false)
      setJoined(r.backup_path ?? '')
    } catch (e) {
      const ae = e as ApiError
      setFields(ae.fields)
      if (!Object.keys(ae.fields).length) setErr(ae.message)
      setConfirmJoin(false)
    } finally {
      setBusy(null)
    }
  }
  if (joined !== null) {
    return (
      <Card title={t('cluster.joinedTitle')}>
        <p className="text-sm">{t('cluster.joinedBody')}</p>
        {joined && <p className="mt-2 text-sm text-muted">{t('cluster.backupWritten')} <code className="ltr break-all font-mono text-xs">{joined}</code></p>}
        <Button className="mt-4" variant="primary" onClick={() => window.location.assign('/login')}><LogIn className="size-4" />{t('cluster.signInAgain')}</Button>
      </Card>
    )
  }
  const owner = me?.user.role === 'owner'
  return (
    <>
      {err && <ErrorBanner error={err} />}
      <div className="grid gap-4 lg:grid-cols-2">
        <Card title={t('cluster.createTitle')}>
          <p className="mb-4 text-sm text-muted">{t('cluster.createHelp')}</p>
          <form onSubmit={(e) => { e.preventDefault(); setConfirmInit(true) }} className="flex flex-col gap-4">
            <Field label={t('cluster.advertise')} htmlFor="adv" help={t('cluster.advertiseHelp')}>
              <Input id="adv" className="ltr font-mono" value={adv} onChange={(e) => setAdv(e.target.value)} placeholder="10.0.0.11:7443" required />
            </Field>
            <Button type="submit" variant="primary" className="self-start" loading={busy === 'init'}>{t('cluster.createSubmit')}</Button>
          </form>
        </Card>
        <Card title={t('cluster.joinTitle')}>
          <p className="mb-4 text-sm text-muted">{t('cluster.joinHelp')}</p>
          {!owner ? <p className="text-sm text-muted">{t('cluster.ownerOnly')}</p> : (
            <form onSubmit={(e) => { e.preventDefault(); setConfirmJoin(true) }} className="flex flex-col gap-4">
              <Field label={t('cluster.joinToken')} htmlFor="jt" error={fields.token}>
                <Textarea id="jt" className="ltr h-24 font-mono text-xs" value={join.token} onChange={(e) => setJoin({ ...join, token: e.target.value })} required />
              </Field>
              <Field label={t('cluster.advertise')} htmlFor="jadv" help={t('cluster.advertiseHelp')}>
                <Input id="jadv" className="ltr font-mono" value={join.advertise} onChange={(e) => setJoin({ ...join, advertise: e.target.value })} placeholder="10.0.0.12:7443" required />
              </Field>
              <fieldset className="flex flex-col gap-2">
                <legend className="mb-1 text-sm font-medium">{t('cluster.localData')}</legend>
                {(['export', 'discard'] as const).map((a) => (
                  <label key={a} className="flex items-start gap-2 text-sm">
                    <input type="radio" name="data_action" value={a} checked={join.data_action === a} onChange={() => setJoin({ ...join, data_action: a })} className="mt-1 accent-[var(--color-accent)]" />
                    <span><span className="font-medium">{t(`cluster.data.${a}` as Key)}</span><span className="block text-xs text-muted">{t(`cluster.data.${a}Help` as Key)}</span></span>
                  </label>
                ))}
              </fieldset>
              <Button type="submit" variant="primary" className="self-start" loading={busy === 'join'}>{t('cluster.joinSubmit')}</Button>
            </form>
          )}
        </Card>
      </div>
      <ConfirmDialog open={confirmInit} onClose={() => setConfirmInit(false)} onConfirm={init} busy={busy === 'init'} title={t('cluster.createTitle')} message={t('cluster.createConfirm')} confirmLabel={t('cluster.createSubmit')} />
      <ConfirmDialog open={confirmJoin} onClose={() => setConfirmJoin(false)} onConfirm={doJoin} busy={busy === 'join'} title={t('cluster.joinTitle')}
        message={join.data_action === 'discard' ? t('cluster.joinConfirmDiscard') : t('cluster.joinConfirmExport')} confirmLabel={t('cluster.joinSubmit')} />
    </>
  )
}

export function ClusterPage() {
  const { t } = usePrefs()
  const { can } = useSession()
  const cluster = useApi<{ cluster: ClusterStatus }>('/cluster', 5000)
  const node = useApi<{ node: NodeInfo }>('/node', 15000)
  if ((cluster.loading && !cluster.data) || (node.loading && !node.data)) return <Spinner />
  if (cluster.error || node.error) return <ErrorBanner error={(cluster.error ?? node.error)!} onRetry={() => { cluster.reload(); node.reload() }} />
  const c = cluster.data!.cluster
  const admin = can('cluster.manage')
  return (
    <div className="flex flex-col gap-4">
      <PageHeader title={t('cluster.title')} subtitle={c.enabled ? t('cluster.subtitleClustered') : t('cluster.standaloneHelp')} />
      {c.enabled ? <ClusterView c={c} admin={admin} reload={cluster.reload} /> : admin ? <StandaloneActions c={c} reload={cluster.reload} /> : null}
      {c.enabled && admin && <TokensCard />}
      <Card title={<span className="inline-flex items-center gap-2"><Server className="size-4" />{t('overview.node')}</span>}><NodeCard node={node.data!.node} /></Card>
    </div>
  )
}

/** Restricted UI shown on nodes that are cluster members but not the active controller. */
export function FollowerStatusPage() {
  const { t } = usePrefs()
  const node = useApi<{ node: NodeInfo }>('/node', 10000)
  const cluster = useApi<{ cluster: ClusterStatus }>('/cluster', 5000)
  const c = cluster.data?.cluster
  const noQuorum = node.data?.node.role === 'NO_QUORUM'
  return (
    <div className="flex flex-col gap-4">
      <PageHeader title={noQuorum ? t('follower.noQuorumTitle') : t('follower.title')} subtitle={noQuorum ? t('follower.noQuorumHelp') : t('follower.help')}
        actions={c?.leader_api_url && !noQuorum ? <a className="inline-flex h-9 items-center gap-1.5 rounded-lg bg-accent px-3.5 text-sm font-medium text-accent-fg hover:opacity-90" href={c.leader_api_url}>{t('cluster.openLeader')}<ExternalLink className="size-4" /></a> : undefined} />
      {(node.error || cluster.error) && <ErrorBanner error={(node.error ?? cluster.error)!} onRetry={() => { node.reload(); cluster.reload() }} />}
      {c?.enabled && <ClusterView c={c} admin={false} reload={cluster.reload} />}
      {node.data ? <Card title={<span className="inline-flex items-center gap-2"><Server className="size-4" />{t('overview.node')}</span>}><NodeCard node={node.data.node} /></Card> : <Spinner />}
    </div>
  )
}
