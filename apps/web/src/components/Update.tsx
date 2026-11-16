import { useEffect, useRef, useState } from 'react'
import { ArrowUpCircle, CheckCircle2, ExternalLink, Loader2, RefreshCw, X } from 'lucide-react'
import { ApiError, get, post } from '../api'
import { usePrefs, useSession } from '../app'
import { useApi } from '../hooks'
import { fmtDateTime, fmtRelative } from '../format'
import { Button, Card, Dialog, ErrorBanner, Spinner } from './ui'
import type { UpdateState } from '../types'
import type { Key } from '../i18n'

type Phase = 'confirm' | 'running' | 'done' | 'failed'

function inProgress(s: UpdateState) {
  return s.pending || s.status?.state === 'running'
}

/** Confirms an update, hands it to the server's update agent and follows it until the new version answers. */
export function UpdateDialog({ open, onClose, state, onChanged }: { open: boolean; onClose: () => void; state: UpdateState; onChanged: () => void }) {
  const { t } = usePrefs()
  const target = (inProgress(state) && state.status?.target) || state.latest?.version || ''
  const [phase, setPhase] = useState<Phase>('confirm')
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState<string | null>(null)
  const [progress, setProgress] = useState<'queued' | 'installing' | 'restarting'>('queued')
  const [fail, setFail] = useState<{ message: string; log?: string } | null>(null)
  const [slow, setSlow] = useState(false)
  // The status of an earlier attempt must not be mistaken for the outcome of this one.
  const prevStarted = useRef<string | undefined>(undefined)
  const requestedAt = useRef(0)

  useEffect(() => {
    if (!open) return
    setErr(null)
    setFail(null)
    setSlow(false)
    if (inProgress(state)) {
      prevStarted.current = state.pending ? state.status?.started_at : undefined
      requestedAt.current = Date.now()
      setPhase('running')
    } else {
      setPhase('confirm')
    }
  }, [open]) // only when the dialog opens: later state changes are followed by polling

  useEffect(() => {
    if (!open || phase !== 'running') return
    let alive = true
    const tick = async () => {
      try {
        const h = await get<{ version: string }>('/health')
        if (!alive) return
        if (h.version === target) {
          setPhase('done')
          onChanged()
          window.setTimeout(() => window.location.reload(), 3000)
          return
        }
      } catch {
        if (alive) setProgress('restarting')
        return
      }
      try {
        const s = (await get<{ update: UpdateState }>('/system/update')).update
        if (!alive) return
        const st = s.status
        if (!s.pending && st && st.target === target && st.state === 'failed' && st.started_at !== prevStarted.current) {
          setFail({ message: st.message, log: s.log })
          setPhase('failed')
          onChanged()
          return
        }
        setProgress(s.pending ? 'queued' : 'installing')
        setSlow(s.pending && Date.now() - requestedAt.current > 90_000)
      } catch {
        if (alive) setProgress('restarting')
      }
    }
    void tick()
    const id = window.setInterval(() => void tick(), 3000)
    return () => {
      alive = false
      window.clearInterval(id)
    }
  }, [open, phase, target, onChanged])

  const apply = async () => {
    setBusy(true)
    setErr(null)
    try {
      prevStarted.current = state.status?.started_at
      requestedAt.current = Date.now()
      await post('/system/update/apply', { version: target })
      setProgress('queued')
      setPhase('running')
      onChanged()
    } catch (e) {
      setErr((e as ApiError).message)
    } finally {
      setBusy(false)
    }
  }

  const footer = phase === 'confirm'
    ? <><Button onClick={onClose}>{t('common.cancel')}</Button>{state.agent.available && <Button variant="primary" loading={busy} onClick={apply} data-testid="update-confirm">{t('update.confirm')}</Button>}</>
    : phase === 'done' ? <Button variant="primary" onClick={() => window.location.reload()}>{t('update.reload')}</Button>
      : <Button onClick={onClose}>{t('common.close')}</Button>

  return (
    <Dialog open={open} onClose={onClose} title={t('update.title', { version: target })} wide footer={footer}>
      {phase === 'confirm' && (
        <div className="flex flex-col gap-3 text-sm" data-testid="update-dialog">
          {err && <ErrorBanner error={err} />}
          <p>{t('update.confirmBody', { current: state.current, latest: target })}</p>
          <ul className="list-disc space-y-1 ps-5 text-muted">
            {(['backup', 'data', 'restart', 'cluster'] as const).map((k) => <li key={k}>{t(`update.point.${k}` as Key)}</li>)}
          </ul>
          {state.latest?.notes && (
            <div>
              <div className="mb-1 flex items-center justify-between gap-2">
                <span className="font-medium">{t('update.notes')}</span>
                {state.latest.url && <a className="inline-flex items-center gap-1 text-accent hover:underline" href={state.latest.url} target="_blank" rel="noreferrer">GitHub<ExternalLink className="size-3.5" /></a>}
              </div>
              <pre className="ltr max-h-48 overflow-auto whitespace-pre-wrap rounded-lg border border-line bg-surface-2 p-3 text-xs">{state.latest.notes}</pre>
            </div>
          )}
          {!state.agent.available && (
            <div className="rounded-lg border border-warn/40 bg-warn/10 p-3" data-testid="update-manual">
              <p>{t('update.noAgent')}</p>
              <code className="ltr mt-2 block rounded bg-surface-2 px-2 py-1.5 font-mono text-xs">sudo intrabin update --to {target}</code>
              <p className="mt-2 text-xs text-muted">{t('update.noAgentHelp')}</p>
            </div>
          )}
        </div>
      )}
      {phase === 'running' && (
        <div className="flex flex-col gap-3 text-sm" role="status" data-testid="update-progress">
          <div className="flex items-center gap-2 font-medium">
            <Loader2 className="size-4 animate-spin text-accent" aria-hidden />
            {progress === 'queued' ? t('update.queued') : progress === 'installing' ? t('update.installing', { version: target }) : t('update.restarting')}
          </div>
          {slow && <p className="rounded-lg border border-warn/40 bg-warn/10 p-3 text-xs">{t('update.slowAgent')} <code className="ltr font-mono">systemctl status intrabin-update.path</code></p>}
          <p className="text-xs text-muted">{t('update.keepOpen')}</p>
        </div>
      )}
      {phase === 'done' && (
        <div className="flex items-center gap-2 text-sm font-medium text-up" role="status"><CheckCircle2 className="size-4" aria-hidden />{t('update.done', { version: target })}</div>
      )}
      {phase === 'failed' && fail && (
        <div className="flex flex-col gap-3 text-sm" role="alert">
          <ErrorBanner error={`${t('update.failed')}: ${fail.message}`} />
          {fail.log && <pre className="ltr max-h-64 overflow-auto whitespace-pre-wrap rounded-lg border border-line bg-surface-2 p-3 text-xs">{fail.log}</pre>}
        </div>
      )}
    </Dialog>
  )
}

/** Top-of-page notice about a new release, for users who manage settings. Hidden for 30 days when closed. */
export function UpdateBanner() {
  const { t } = usePrefs()
  const { can } = useSession()
  const u = useApi<{ update: UpdateState }>(can('settings.manage') ? '/system/update' : null, 10 * 60 * 1000)
  const [open, setOpen] = useState(false)
  const [hidden, setHidden] = useState(false)
  const st = u.data?.update
  if (!st) return null
  const dismiss = async () => {
    setHidden(true)
    try { await post('/system/update/dismiss') } catch { /* shown again on the next load */ }
  }
  return (
    <>
      {st.show_banner && st.latest && !hidden && (
        <div role="status" data-testid="update-banner" className="flex flex-wrap items-center gap-x-3 gap-y-1 border-b border-accent/30 bg-accent-soft px-4 py-2 text-sm">
          <ArrowUpCircle className="size-4 shrink-0 text-accent" aria-hidden />
          <span>{t('update.banner', { latest: st.latest.version, current: st.current })}</span>
          {st.latest.url && <a className="inline-flex items-center gap-1 text-accent hover:underline" href={st.latest.url} target="_blank" rel="noreferrer">{t('update.notes')}<ExternalLink className="size-3.5" /></a>}
          <div className="ms-auto flex items-center gap-1">
            <Button size="sm" variant="primary" onClick={() => setOpen(true)} data-testid="update-open">{t('update.updateNow')}</Button>
            <Button size="sm" variant="ghost" onClick={dismiss} aria-label={t('update.dismiss')} title={t('update.dismiss')} data-testid="update-dismiss"><X className="size-4" /></Button>
          </div>
        </div>
      )}
      <UpdateDialog open={open} onClose={() => setOpen(false)} state={st} onChanged={u.reload} />
    </>
  )
}

/** Settings card: installed and latest version, manual check, update. */
export function UpdatesCard() {
  const { t, loc } = usePrefs()
  const u = useApi<{ update: UpdateState }>('/system/update', 60000)
  const [checking, setChecking] = useState(false)
  const [err, setErr] = useState<string | null>(null)
  const [open, setOpen] = useState(false)
  const st = u.data?.update
  const check = async () => {
    setChecking(true)
    setErr(null)
    try { await post('/system/update/check'); await u.reload() } catch (e) { setErr((e as ApiError).message) } finally { setChecking(false) }
  }
  return (
    <Card title={t('update.card')} actions={st?.enabled && <Button size="sm" loading={checking} onClick={check} data-testid="update-check"><RefreshCw className="size-3.5" />{t('update.checkNow')}</Button>}>
      {!st ? (u.error ? <ErrorBanner error={u.error} onRetry={u.reload} /> : <Spinner />) : (
        <div className="flex flex-col gap-3 text-sm" data-testid="updates-card">
          {err && <ErrorBanner error={err} />}
          <dl className="grid grid-cols-2 gap-y-2">
            <dt className="text-muted">{t('update.current')}</dt><dd className="ltr text-start font-mono text-xs">{st.current}</dd>
            <dt className="text-muted">{t('update.latest')}</dt>
            <dd className="ltr text-start">{st.latest
              ? <a className="inline-flex items-center gap-1 font-mono text-xs text-accent hover:underline" href={st.latest.url} target="_blank" rel="noreferrer">{st.latest.version}<ExternalLink className="size-3" /></a>
              : '—'}</dd>
            <dt className="text-muted">{t('update.checked')}</dt>
            <dd title={st.checked_at ? fmtDateTime(loc, st.checked_at) : undefined}>{st.checked_at ? fmtRelative(loc, st.checked_at) : t('update.never')}</dd>
            {st.status && <>
              <dt className="text-muted">{t('update.lastRun')}</dt>
              <dd><span className={st.status.state === 'failed' ? 'text-down' : st.status.state === 'succeeded' ? 'text-up' : ''}>{st.status.target} · {t(`update.state.${st.status.state}` as Key)}</span>
                {st.status.finished_at && <span className="block text-xs text-muted">{fmtDateTime(loc, st.status.finished_at)}</span>}</dd>
            </>}
          </dl>
          {!st.enabled && <p className="text-muted">{t('update.disabled')}</p>}
          {st.check_error && <p className="text-warn">{t('update.checkError', { error: st.check_error })}</p>}
          {st.enabled && !st.check_error && st.latest && !st.available && <p className="text-up">{t('update.upToDate')}</p>}
          {st.dismissed_until && st.available && <p className="text-xs text-muted">{t('update.dismissed', { date: fmtDateTime(loc, st.dismissed_until) })}</p>}
          <p className="text-xs text-muted">{st.agent.available ? t('update.agentReady') : t('update.agentMissing')}</p>
          {(st.available || inProgress(st)) && st.latest && (
            <Button variant="primary" className="self-start" onClick={() => setOpen(true)} data-testid="update-open-settings">
              {inProgress(st) ? <Loader2 className="size-4 animate-spin" aria-hidden /> : <ArrowUpCircle className="size-4" aria-hidden />}
              {t('update.updateTo', { version: (inProgress(st) && st.status?.target) || st.latest.version })}
            </Button>
          )}
          <UpdateDialog open={open} onClose={() => setOpen(false)} state={st} onChanged={u.reload} />
        </div>
      )}
    </Card>
  )
}
