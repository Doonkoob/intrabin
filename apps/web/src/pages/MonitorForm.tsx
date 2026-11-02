import { useEffect, useMemo, useState, type FormEvent, type ReactNode } from 'react'
import { useNavigate, useParams } from 'react-router'
import { CheckCircle2, XCircle } from 'lucide-react'
import { ApiError, get, post, put } from '../api'
import { usePrefs } from '../app'
import { useApi } from '../hooks'
import { fmtDuration, fmtNumber } from '../format'
import { Button, Card, Checkbox, ErrorBanner, Field, Input, PageHeader, Select, Spinner, Textarea } from '../components/ui'
import type { Group, Monitor, Policy } from '../types'
import type { Key } from '../i18n'

interface FormState {
  name: string
  description: string
  type: Monitor['type']
  target: string
  group_id: string
  tags: string
  interval_seconds: number
  timeout_seconds: number
  retries: number
  enabled: boolean
  method: string
  headers: string
  body: string
  expected_status: string
  expected_body: string
  follow_redirects: boolean
  skip_tls_verify: boolean
  degraded_latency_ms: number
  cert_expiry_fail_days: number
  cert_expiry_warn_days: number
  record_type: string
  expected_value: string
  resolver: string
  check_path: string
  origin_address: string
  host_header: string
  tls_server_name: string
  policy: Policy
  basic_auth_user: string
  basic_auth_password: string
  clear_secrets: boolean
}

const defaults: FormState = {
  name: '', description: '', type: 'http', target: 'https://', group_id: '', tags: '', interval_seconds: 60, timeout_seconds: 10, retries: 0, enabled: true,
  method: 'GET', headers: '', body: '', expected_status: '', expected_body: '', follow_redirects: true, skip_tls_verify: false, degraded_latency_ms: 0,
  cert_expiry_fail_days: 0, cert_expiry_warn_days: 0, record_type: 'A', expected_value: '', resolver: '', check_path: 'public', origin_address: '', host_header: '', tls_server_name: '',
  policy: { confirmations_required: 1, min_locations: 1, min_valid_probes: 1, fail_mode: 'consecutive', failure_rounds: 3, window_seconds: 0, recovery_rounds: 2, cooldown_seconds: 300 },
  basic_auth_user: '', basic_auth_password: '', clear_secrets: false,
}

function fromMonitor(m: Monitor): FormState {
  const s = m.settings
  return {
    ...defaults,
    name: m.name, description: m.description, type: m.type, target: m.target, group_id: m.group_id ?? '', tags: m.tags.join(', '),
    interval_seconds: m.interval_seconds, timeout_seconds: m.timeout_seconds, retries: m.retries, enabled: m.enabled,
    method: s.method || 'GET', headers: Object.entries(s.headers ?? {}).map(([k, v]) => `${k}: ${v}`).join('\n'), body: s.body ?? '',
    expected_status: (s.expected_status ?? []).join(', '), expected_body: s.expected_body ?? '', follow_redirects: s.follow_redirects ?? true,
    skip_tls_verify: !!s.skip_tls_verify, degraded_latency_ms: s.degraded_latency_ms ?? 0, cert_expiry_fail_days: s.cert_expiry_fail_days ?? 0,
    cert_expiry_warn_days: s.cert_expiry_warn_days ?? 0, record_type: s.record_type || 'A', expected_value: s.expected_value ?? '', resolver: s.resolver ?? '',
    check_path: s.check_path || 'public', origin_address: s.origin_address ?? '', host_header: s.host_header ?? '', tls_server_name: s.tls_server_name ?? '',
    policy: { ...defaults.policy, ...m.policy },
  }
}

function toPayload(f: FormState) {
  const headers: Record<string, string> = {}
  for (const line of f.headers.split('\n')) {
    const i = line.indexOf(':')
    if (i > 0) headers[line.slice(0, i).trim()] = line.slice(i + 1).trim()
  }
  const settings: Record<string, unknown> = {
    check_path: f.check_path, skip_tls_verify: f.skip_tls_verify, degraded_latency_ms: Number(f.degraded_latency_ms) || 0,
    cert_expiry_fail_days: Number(f.cert_expiry_fail_days) || 0, cert_expiry_warn_days: Number(f.cert_expiry_warn_days) || 0,
    origin_address: f.origin_address.trim(), tls_server_name: f.tls_server_name.trim(),
  }
  if (f.type === 'http') {
    Object.assign(settings, {
      method: f.method, headers, body: f.body, expected_body: f.expected_body, follow_redirects: f.follow_redirects, host_header: f.host_header.trim(),
      expected_status: f.expected_status.split(',').map((s) => s.trim()).filter(Boolean),
    })
  }
  if (f.type === 'dns') Object.assign(settings, { record_type: f.record_type, expected_value: f.expected_value.trim(), resolver: f.resolver.trim() })
  const secrets = f.basic_auth_user || f.basic_auth_password ? { basic_auth_user: f.basic_auth_user, basic_auth_password: f.basic_auth_password } : undefined
  return {
    name: f.name, description: f.description, type: f.type, target: f.target.trim(), group_id: f.group_id, enabled: f.enabled,
    tags: f.tags.split(',').map((s) => s.trim()).filter(Boolean),
    interval_seconds: Number(f.interval_seconds), timeout_seconds: Number(f.timeout_seconds), retries: Number(f.retries),
    settings, policy: { ...f.policy, window_seconds: f.policy.fail_mode === 'window' ? Number(f.policy.window_seconds) : 0 },
    secrets, clear_secrets: f.clear_secrets,
  }
}

function Section({ title, help, children }: { title: string; help?: string; children: ReactNode }) {
  return (
    <Card title={title}>
      {help && <p className="mb-4 text-sm text-muted">{help}</p>}
      <div className="grid gap-4 sm:grid-cols-2">{children}</div>
    </Card>
  )
}

export function MonitorFormPage() {
  const { id } = useParams()
  const { t, loc } = usePrefs()
  const navigate = useNavigate()
  const [f, setF] = useState<FormState>(defaults)
  const [existing, setExisting] = useState<Monitor | null>(null)
  const [loading, setLoading] = useState(!!id)
  const [loadErr, setLoadErr] = useState<Error | null>(null)
  const [fields, setFields] = useState<Record<string, string>>({})
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const [testing, setTesting] = useState(false)
  const [test, setTest] = useState<{ success: boolean; latency_ms: number; http_status?: number; error_category: string; message: string } | null>(null)
  const groups = useApi<{ groups: Group[] }>('/monitor-groups')

  useEffect(() => {
    if (!id) return
    get<{ monitor: Monitor }>(`/monitors/${id}`).then((r) => { setExisting(r.monitor); setF(fromMonitor(r.monitor)) }).catch(setLoadErr).finally(() => setLoading(false))
  }, [id])

  const set = <K extends keyof FormState>(k: K, v: FormState[K]) => setF((p) => ({ ...p, [k]: v }))
  const setP = <K extends keyof Policy>(k: K, v: Policy[K]) => setF((p) => ({ ...p, policy: { ...p.policy, [k]: v } }))
  const num = (k: keyof FormState) => (e: React.ChangeEvent<HTMLInputElement>) => set(k, Number(e.target.value) as never)
  const str = (k: keyof FormState) => (e: React.ChangeEvent<HTMLInputElement | HTMLTextAreaElement | HTMLSelectElement>) => set(k, e.target.value as never)
  const err = (k: string) => fields[k]

  const summary = useMemo(() => {
    const p = f.policy
    if (p.fail_mode === 'window') {
      return t('mform.summaryWindow', { rounds: fmtNumber(loc, p.failure_rounds), window: fmtDuration(loc, Number(p.window_seconds) || 0), confirm: fmtNumber(loc, p.confirmations_required), locs: fmtNumber(loc, p.min_locations) })
    }
    return t('mform.summary', { rounds: fmtNumber(loc, p.failure_rounds), time: fmtDuration(loc, p.failure_rounds * f.interval_seconds), confirm: fmtNumber(loc, p.confirmations_required), locs: fmtNumber(loc, p.min_locations) })
  }, [f.policy, f.interval_seconds, loc, t])

  const submit = async (e: FormEvent) => {
    e.preventDefault()
    setBusy(true)
    setError(null)
    setFields({})
    try {
      const body = toPayload(f)
      const r = id ? await put<{ monitor: Monitor }>(`/monitors/${id}`, body) : await post<{ monitor: Monitor }>('/monitors', body)
      navigate(`/monitors/${r.monitor.id}`)
    } catch (ex) {
      const ae = ex as ApiError
      setFields(ae.fields)
      setError(ae.message)
      window.scrollTo({ top: 0, behavior: 'smooth' })
    } finally {
      setBusy(false)
    }
  }

  const runTest = async () => {
    setTesting(true)
    setTest(null)
    setFields({})
    try {
      setTest(await post('/monitors/test', toPayload(f)))
    } catch (ex) {
      const ae = ex as ApiError
      setFields(ae.fields)
      setError(ae.message)
    } finally {
      setTesting(false)
    }
  }

  if (loading) return <Spinner />
  if (loadErr) return <ErrorBanner error={loadErr} />
  const targetLabel: Record<Monitor['type'], Key> = { http: 'mform.url', tcp: 'mform.hostport', dns: 'mform.hostname', tls: 'mform.host' }
  const targetPh: Record<Monitor['type'], string> = { http: 'https://www.example.com/health', tcp: 'db.internal:5432', dns: 'www.example.com', tls: 'www.example.com:443' }

  return (
    <form onSubmit={submit} noValidate>
      <PageHeader title={id ? t('mform.editTitle') : t('mform.createTitle')} subtitle={existing?.name}
        actions={<>
          <Button onClick={() => navigate(-1)}>{t('common.cancel')}</Button>
          <Button onClick={runTest} loading={testing} data-testid="test-monitor">{testing ? t('mform.testing') : t('mform.test')}</Button>
          <Button type="submit" variant="primary" loading={busy} data-testid="save-monitor">{busy ? t('common.saving') : t('common.save')}</Button>
        </>} />
      <div className="flex flex-col gap-4">
        {error && <ErrorBanner error={error} />}
        {test && (
          <div role="status" data-testid="test-result" className={`flex items-start gap-2 rounded-lg border p-3 text-sm ${test.success ? 'border-up/30 bg-up-soft text-up' : 'border-down/30 bg-down-soft text-down'}`}>
            {test.success ? <CheckCircle2 className="size-4 shrink-0" /> : <XCircle className="size-4 shrink-0" />}
            <div>
              <div className="font-medium">{test.success ? t('mform.testOk', { ms: fmtNumber(loc, test.latency_ms) }) : `${t('mform.testFail')}: ${t(`err.${test.error_category}` as Key)}`}</div>
              {(test.http_status || test.message) && <div className="ltr mt-0.5 text-start text-xs opacity-90">{test.http_status ? `HTTP ${test.http_status} ` : ''}{test.message}</div>}
            </div>
          </div>
        )}
        <Section title={t('mform.general')}>
          <Field label={t('common.name')} htmlFor="name" error={err('name')}><Input id="name" value={f.name} onChange={str('name')} invalid={!!err('name')} required /></Field>
          <Field label={t('monitors.type')} htmlFor="type" error={err('type')}>
            <Select id="type" value={f.type} onChange={(e) => {
              const type = e.target.value as Monitor['type']
              setF((p) => ({ ...p, type, target: type === 'http' ? (p.target.startsWith('http') ? p.target : 'https://') : p.target.replace(/^https?:\/\//, '').replace(/\/.*$/, '') }))
            }}>
              {(['http', 'tcp', 'dns', 'tls'] as const).map((tp) => <option key={tp} value={tp}>{t(`mform.type.${tp}` as Key)}</option>)}
            </Select>
          </Field>
          <div className="sm:col-span-2"><Field label={t(targetLabel[f.type])} htmlFor="target" error={err('target')}>
            <Input id="target" className="ltr font-mono" placeholder={targetPh[f.type]} value={f.target} onChange={str('target')} invalid={!!err('target')} required />
          </Field></div>
          <div className="sm:col-span-2"><Field label={t('common.description')} htmlFor="description" optional error={err('description')}><Textarea id="description" value={f.description} onChange={str('description')} rows={2} /></Field></div>
          <Field label={t('monitors.group')} htmlFor="group" optional error={err('group_id')}>
            <Select id="group" value={f.group_id} onChange={str('group_id')}>
              <option value="">{t('common.none')}</option>
              {groups.data?.groups.map((g) => <option key={g.id} value={g.id}>{g.name}</option>)}
            </Select>
          </Field>
          <Field label={t('mform.tags')} htmlFor="tags" help={t('mform.tagsHelp')} optional error={err('tags')}><Input id="tags" value={f.tags} onChange={str('tags')} /></Field>
          <Field label={t('mform.interval')} htmlFor="interval" error={err('interval_seconds')}><Input id="interval" type="number" min={10} value={f.interval_seconds} onChange={num('interval_seconds')} invalid={!!err('interval_seconds')} /></Field>
          <Field label={t('mform.timeout')} htmlFor="timeout" error={err('timeout_seconds')}><Input id="timeout" type="number" min={1} max={60} value={f.timeout_seconds} onChange={num('timeout_seconds')} invalid={!!err('timeout_seconds')} /></Field>
          <Field label={t('mform.retries')} htmlFor="retries" error={err('retries')}><Input id="retries" type="number" min={0} max={5} value={f.retries} onChange={num('retries')} /></Field>
          <div className="flex items-end pb-2"><Checkbox label={t('mform.enabled')} checked={f.enabled} onChange={(v) => set('enabled', v)} /></div>
        </Section>

        {f.type === 'http' && (
          <Section title={t('mform.request')}>
            <Field label={t('mform.method')} htmlFor="method" error={err('settings.method')}>
              <Select id="method" value={f.method} onChange={str('method')}>{['GET', 'HEAD', 'POST', 'PUT', 'PATCH', 'DELETE', 'OPTIONS'].map((m) => <option key={m}>{m}</option>)}</Select>
            </Field>
            <Field label={t('mform.checkPath')} htmlFor="check_path">
              <Select id="check_path" value={f.check_path} onChange={str('check_path')}>{['public', 'origin', 'internal'].map((m) => <option key={m} value={m}>{t(`mform.checkPath.${m}` as Key)}</option>)}</Select>
            </Field>
            <div className="sm:col-span-2"><Field label={t('mform.headers')} htmlFor="headers" help={t('mform.headersHelp')} optional error={err('settings.headers')}>
              <Textarea id="headers" className="ltr font-mono text-xs" rows={3} placeholder="Accept: application/json" value={f.headers} onChange={str('headers')} />
            </Field></div>
            {['POST', 'PUT', 'PATCH'].includes(f.method) && (
              <div className="sm:col-span-2"><Field label={t('mform.body')} htmlFor="body" optional error={err('settings.body')}><Textarea id="body" className="ltr font-mono text-xs" rows={3} value={f.body} onChange={str('body')} /></Field></div>
            )}
          </Section>
        )}

        {f.type === 'dns' && (
          <Section title={t('mform.request')}>
            <Field label={t('mform.recordType')} htmlFor="record_type" error={err('settings.record_type')}>
              <Select id="record_type" value={f.record_type} onChange={str('record_type')}>{['A', 'AAAA', 'CNAME', 'MX', 'TXT', 'NS'].map((r) => <option key={r}>{r}</option>)}</Select>
            </Field>
            <Field label={t('mform.expectedValue')} htmlFor="expected_value" optional><Input id="expected_value" className="ltr" value={f.expected_value} onChange={str('expected_value')} /></Field>
            <Field label={t('mform.resolver')} htmlFor="resolver" optional error={err('settings.resolver')}><Input id="resolver" className="ltr" placeholder="1.1.1.1" value={f.resolver} onChange={str('resolver')} /></Field>
          </Section>
        )}

        {(f.type === 'http' || f.type === 'tls') && (
          <Section title={t('mform.expect')}>
            {f.type === 'http' && <>
              <Field label={t('mform.expectedStatus')} htmlFor="expected_status" help={t('mform.expectedStatusHelp')} optional error={err('settings.expected_status')}>
                <Input id="expected_status" className="ltr" placeholder="200-299" value={f.expected_status} onChange={str('expected_status')} invalid={!!err('settings.expected_status')} />
              </Field>
              <Field label={t('mform.expectedBody')} htmlFor="expected_body" optional><Input id="expected_body" value={f.expected_body} onChange={str('expected_body')} /></Field>
              <Field label={t('mform.degradedLatency')} htmlFor="degraded" optional><Input id="degraded" type="number" min={0} value={f.degraded_latency_ms} onChange={num('degraded_latency_ms')} /></Field>
            </>}
            <Field label={t('mform.certFailDays')} htmlFor="cert_fail" optional><Input id="cert_fail" type="number" min={0} value={f.cert_expiry_fail_days} onChange={num('cert_expiry_fail_days')} /></Field>
            {f.type === 'tls' && <Field label={t('mform.certWarnDays')} htmlFor="cert_warn" optional><Input id="cert_warn" type="number" min={0} value={f.cert_expiry_warn_days} onChange={num('cert_expiry_warn_days')} /></Field>}
            {f.type === 'http' && <div className="flex items-end pb-2"><Checkbox label={t('mform.followRedirects')} checked={f.follow_redirects} onChange={(v) => set('follow_redirects', v)} /></div>}
          </Section>
        )}

        <Section title={t('mform.policy')} help={t('mform.policyHelp')}>
          <Field label={t('mform.failMode')} htmlFor="fail_mode" error={err('policy.fail_mode')}>
            <Select id="fail_mode" value={f.policy.fail_mode} onChange={(e) => setP('fail_mode', e.target.value as Policy['fail_mode'])}>
              <option value="consecutive">{t('mform.failMode.consecutive')}</option>
              <option value="window">{t('mform.failMode.window')}</option>
            </Select>
          </Field>
          <Field label={t('mform.failureRounds')} htmlFor="failure_rounds" error={err('policy.failure_rounds')}><Input id="failure_rounds" type="number" min={1} value={f.policy.failure_rounds} onChange={(e) => setP('failure_rounds', Number(e.target.value))} /></Field>
          {f.policy.fail_mode === 'window' && (
            <Field label={t('mform.windowSeconds')} htmlFor="window_seconds" error={err('policy.window_seconds')}><Input id="window_seconds" type="number" min={1} value={f.policy.window_seconds} onChange={(e) => setP('window_seconds', Number(e.target.value))} invalid={!!err('policy.window_seconds')} /></Field>
          )}
          <Field label={t('mform.recoveryRounds')} htmlFor="recovery_rounds" error={err('policy.recovery_rounds')}><Input id="recovery_rounds" type="number" min={1} value={f.policy.recovery_rounds} onChange={(e) => setP('recovery_rounds', Number(e.target.value))} /></Field>
          <Field label={t('mform.confirmations')} htmlFor="confirmations" error={err('policy.confirmations_required')}><Input id="confirmations" type="number" min={1} value={f.policy.confirmations_required} onChange={(e) => setP('confirmations_required', Number(e.target.value))} /></Field>
          <Field label={t('mform.minLocations')} htmlFor="min_locations" error={err('policy.min_locations')}><Input id="min_locations" type="number" min={1} value={f.policy.min_locations} onChange={(e) => setP('min_locations', Number(e.target.value))} /></Field>
          <Field label={t('mform.minValid')} htmlFor="min_valid" error={err('policy.min_valid_probes')}><Input id="min_valid" type="number" min={1} value={f.policy.min_valid_probes} onChange={(e) => setP('min_valid_probes', Number(e.target.value))} /></Field>
          <Field label={t('mform.cooldown')} htmlFor="cooldown" error={err('policy.cooldown_seconds')}><Input id="cooldown" type="number" min={0} value={f.policy.cooldown_seconds} onChange={(e) => setP('cooldown_seconds', Number(e.target.value))} /></Field>
          <p className="rounded-lg bg-accent-soft p-3 text-sm text-fg sm:col-span-2" data-testid="policy-summary">{summary}</p>
        </Section>

        {(f.type === 'http' || f.type === 'tls') && (
          <Section title={t('mform.advanced')}>
            <Field label={t('mform.origin')} htmlFor="origin" help={t('mform.originHelp')} optional error={err('settings.origin_address')}>
              <Input id="origin" className="ltr" placeholder="203.0.113.10" value={f.origin_address} onChange={str('origin_address')} invalid={!!err('settings.origin_address')} />
            </Field>
            {f.type === 'http' && <Field label={t('mform.hostHeader')} htmlFor="host_header" optional><Input id="host_header" className="ltr" value={f.host_header} onChange={str('host_header')} /></Field>}
            <Field label={t('mform.sni')} htmlFor="sni" optional><Input id="sni" className="ltr" value={f.tls_server_name} onChange={str('tls_server_name')} /></Field>
            <div className="flex items-end pb-2"><Checkbox label={t('mform.skipTLS')} checked={f.skip_tls_verify} onChange={(v) => set('skip_tls_verify', v)} /></div>
          </Section>
        )}

        {f.type === 'http' && (
          <Section title={t('mform.secrets')} help={t('mform.secretsHelp')}>
            {existing && existing.secret_keys.length > 0 && (
              <p className="text-sm text-muted sm:col-span-2">{t('mform.storedSecrets', { keys: existing.secret_keys.join(', ') })}</p>
            )}
            <Field label={t('mform.basicUser')} htmlFor="bau" optional><Input id="bau" autoComplete="off" value={f.basic_auth_user} onChange={str('basic_auth_user')} /></Field>
            <Field label={t('mform.basicPass')} htmlFor="bap" optional><Input id="bap" type="password" autoComplete="new-password" value={f.basic_auth_password} onChange={str('basic_auth_password')} /></Field>
            {existing && existing.secret_keys.length > 0 && <Checkbox label={t('mform.clearSecrets')} checked={f.clear_secrets} onChange={(v) => set('clear_secrets', v)} />}
          </Section>
        )}
      </div>
    </form>
  )
}
